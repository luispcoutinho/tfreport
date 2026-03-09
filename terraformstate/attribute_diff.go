package terraformstate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"
)

// BlockElementDiff describes how a single element of a block array changed.
// Status is one of: "added", "removed", "unchanged", "changed".
// Name is the value of the "name" field (or "" if none).
// Lines holds the pretty-printed field lines for the element:
//   - added/removed: all fields (same as prettyBlock output for one element)
//   - changed: only the changed fields, each rendered as one of:
//     "  - key: before"  /  "  + key: after"  (scalar change)
//     "  key:"  followed by nested BlockDiffs (block change)
//   - unchanged: Lines is nil (element is skipped in output)
type BlockElementDiff struct {
	Status     string            // "added" | "removed" | "changed" | "unchanged"
	Name       string            // identity key value (name or index)
	Lines      []string          // flat display lines for this element
	FieldDiffs []FieldDiff       // per-field diffs for "changed" elements
}

// FieldDiff represents a single scalar field change within a changed block element.
type FieldDiff struct {
	Key    string
	Before string
	After  string
	// SubDiffs is non-nil when this field is itself a block array that changed.
	SubDiffs []BlockElementDiff
}

// AttributeDiff represents a single attribute change for a resource.
type AttributeDiff struct {
	Key    string
	Before string
	After  string
	// Lines, when non-nil, replaces After/Before with a pretty-printed
	// block representation. Each entry is one display line, e.g.:
	//   "  [0] name: \"bp-web-ppr\""
	//   "      ip_addresses: [\"10.253.25.12\"]"
	// Used by the table/tree writers for block-valued attributes on create/delete.
	Lines []string
	// BlockDiffs, when non-nil, is used for update rendering of block arrays.
	// Each entry describes one element's status (added/removed/changed/unchanged).
	// Unchanged elements are present but produce no output.
	BlockDiffs []BlockElementDiff
}

// GetAttributeDiffs returns the list of changed attributes for a resource change.
// Unknown (computed) values are rendered as "(known after apply)".
// Sensitive values are rendered as "(sensitive)", unless the planned_values
// entry for the same address has the real value with no leaf-level sensitivity,
// in which case the real value is shown instead.
// Null values are rendered as "(null)".
func GetAttributeDiffs(rc *tfjson.ResourceChange, pv PlannedValuesMap) []AttributeDiff {
	if rc.Change == nil {
		return nil
	}

	actions := rc.Change.Actions

	// For pure creates, show all non-null after-values
	if actions.Create() && !actions.Delete() {
		return diffForCreate(rc, pv)
	}

	// For pure deletes, show all non-null before-values
	if actions.Delete() && !actions.Create() {
		return diffForDelete(rc, pv)
	}

	// For updates and recreates, show before → after for changed attrs
	if actions.Update() || actions.DestroyBeforeCreate() || actions.CreateBeforeDestroy() {
		return diffForUpdate(rc, pv)
	}

	return nil
}

func diffForCreate(rc *tfjson.ResourceChange, pv PlannedValuesMap) []AttributeDiff {
	after := toMap(rc.Change.After)
	afterUnknown := toMap(rc.Change.AfterUnknown)
	afterSensitive := toMap(rc.Change.AfterSensitive)
	if after == nil && afterUnknown == nil {
		return nil
	}

	// Resolve planned values for this resource, if available.
	// planned_values contains the real attribute values without block-level
	// sensitive redaction. We use it to recover values that resource_changes
	// marks as (sensitive) because the Azure provider (and others) mark whole
	// nested blocks as sensitive even when the content is not a secret.
	var pvValues map[string]interface{}
	var pvSensitive map[string]interface{}
	if sr, ok := pv[rc.Address]; ok && sr != nil {
		pvValues = sr.AttributeValues
		pvSensitive = parseSensitiveValues(sr.SensitiveValues)
	}

	// For creates, only show attributes that:
	//   - have a concrete known value (skip "(known after apply)" — not actionable at plan time)
	//   - are not null/empty/false (skip provider defaults and unset optionals)
	//   - are sensitive (always shown — tells the reviewer something is configured there)
	keys := mergedKeys(after, afterUnknown)
	diffs := make([]AttributeDiff, 0, len(keys))
	for _, k := range keys {
		afterStr := formatValue(after[k], afterUnknown[k], afterSensitive[k])

		// If resource_changes marks this as (sensitive), try to resolve the real
		// value from planned_values where leaf-level sensitivity is tracked.
		// In planned_values.sensitive_values:
		//   - false          → not sensitive (leaf scalar)
		//   - true           → sensitive (leaf scalar)
		//   - {}  / [{}]    → container with no sensitive leaves → safe to show
		//   - {"k":true}/[{"k":true}] → container with at least one sensitive leaf
		if afterStr == "(sensitive)" && pvValues != nil {
			pvVal := pvValues[k]
			pvSens := pvSensitive[k]
			if !hasAnySensitiveLeaf(pvSens) {
				resolved := formatValue(pvVal, afterUnknown[k], nil)
				if !isEmptyValue(resolved) {
					afterStr = resolved
				}
			}
		}

		// Skip unset / empty / default values
		if isEmptyValue(afterStr) {
			continue
		}
		// Skip computed values that are only known post-apply
		if afterStr == "(known after apply)" {
			continue
		}
		// Try to render block arrays as pretty-printed sub-lines.
		// Use pvValues if available (already resolved above), else after[k].
		valSrc := after[k]
		if pvValues != nil {
			if pv := pvValues[k]; pv != nil {
				valSrc = pv
			}
		}
		blockLines := prettyBlock("  ", valSrc, afterUnknown[k], afterSensitive[k])

		// Before is always omitted for creates (no "(none) ->" prefix)
		diffs = append(diffs, AttributeDiff{Key: k, Before: "", After: afterStr, Lines: blockLines})
	}
	return diffs
}

// hasAnySensitiveLeaf returns true if the sensitive_values marker contains any
// true leaf. In planned_values.sensitive_values:
//   - false / nil       → not sensitive
//   - true              → sensitive leaf
//   - {}  / [{}]        → container, no sensitive leaves → false
//   - {"k":true}        → container with sensitive leaf → true
//   - [{"k":true}]      → array of containers, at least one sensitive leaf → true
func hasAnySensitiveLeaf(v interface{}) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case map[string]interface{}:
		for _, child := range val {
			if hasAnySensitiveLeaf(child) {
				return true
			}
		}
		return false
	case []interface{}:
		for _, elem := range val {
			if hasAnySensitiveLeaf(elem) {
				return true
			}
		}
		return false
	}
	return false
}

// parseSensitiveValues unmarshals the SensitiveValues json.RawMessage into a
// map[string]interface{} for easy lookup. Returns nil on error or empty input.
func parseSensitiveValues(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func diffForDelete(rc *tfjson.ResourceChange, _ PlannedValuesMap) []AttributeDiff {
	before := toMap(rc.Change.Before)
	beforeSensitive := toMap(rc.Change.BeforeSensitive)
	if before == nil {
		return nil
	}

	// For deletes, only show the identifying attribute: prefer "id", fallback to "name"
	for _, key := range []string{"id", "name"} {
		if v, ok := before[key]; ok {
			str := formatValue(v, nil, beforeSensitive[key])
			if str != "(null)" {
				return []AttributeDiff{{Key: key, Before: str, After: ""}}
			}
		}
	}
	return nil
}

func diffForUpdate(rc *tfjson.ResourceChange, pv PlannedValuesMap) []AttributeDiff {
	before := toMap(rc.Change.Before)
	after := toMap(rc.Change.After)
	afterUnknown := toMap(rc.Change.AfterUnknown)
	beforeSensitive := toMap(rc.Change.BeforeSensitive)
	afterSensitive := toMap(rc.Change.AfterSensitive)

	// Resolve planned_values for this resource, if available.
	// planned_values tracks sensitivity at leaf level, so we can recover
	// values that resource_changes marks as (sensitive) due to block-level
	// sensitivity markers (common with Azure provider nested blocks).
	var pvValues map[string]interface{}
	var pvSensitive map[string]interface{}
	if sr, ok := pv[rc.Address]; ok && sr != nil {
		pvValues = sr.AttributeValues
		pvSensitive = parseSensitiveValues(sr.SensitiveValues)
	}

	keys := mergedKeys(before, after, afterUnknown)
	diffs := make([]AttributeDiff, 0)

	for _, k := range keys {
		beforeStr := formatValue(before[k], nil, beforeSensitive[k])
		afterStr := formatValue(after[k], afterUnknown[k], afterSensitive[k])

		// Attempt to resolve (sensitive) values from planned_values.
		// planned_values only has after-state, so we can only resolve afterStr.
		// beforeStr stays as "(sensitive)" — we don't have the previous state values.
		if afterStr == "(sensitive)" && pvValues != nil {
			pvVal := pvValues[k]
			pvSens := pvSensitive[k]
			resolved := formatValue(pvVal, afterUnknown[k], pvSens)
			if resolved != "(sensitive)" {
				afterStr = resolved
			}
		}

		// Skip unchanged
		if beforeStr == afterStr {
			continue
		}
		// Skip null -> null
		if beforeStr == "(null)" && afterStr == "(null)" {
			continue
		}
		// Skip empty/zero -> (null): internal resets like false->(null), 0->(null), []->(null)
		if isEmptyValue(beforeStr) && afterStr == "(null)" {
			continue
		}
		// For block arrays in updates, compute a semantic set-diff so that only
		// added/removed/changed elements are shown. Unchanged elements are suppressed.
		var blockDiffs []BlockElementDiff
		beforeArr, beforeIsArr := toBlockArray(before[k])
		afterArr, afterIsArr := toBlockArray(after[k])
		if beforeIsArr && afterIsArr {
			blockDiffs = diffBlockArray(
				beforeArr, afterArr,
				toInterfaceSlice(beforeSensitive[k]),
				toInterfaceSlice(afterSensitive[k]),
				toInterfaceSlice(afterUnknown[k]),
			)
		}

		diffs = append(diffs, AttributeDiff{
			Key:        k,
			Before:     beforeStr,
			After:      afterStr,
			BlockDiffs: blockDiffs,
		})
	}
	return diffs
}

// isEmptyValue returns true for values that represent an unset/zero state.
func isEmptyValue(s string) bool {
	switch s {
	case "(null)", "false", "0", "[]", "{}", "\"\"":
		return true
	}
	return false
}

// formatValue converts a raw JSON interface value into a human-readable string,
// applying special labels for unknown and sensitive markers.
//
// Both unknown and sensitive use Terraform's marker structure:
//   - scalar true  → this specific value is unknown/sensitive
//   - container    → some nested field is unknown/sensitive, but the container
//                    itself is present and should be displayed
//
// Examples from azurerm provider:
//   after_sensitive["http_listener"] = [{"host_names":[false]}]
//     → container marker: show the http_listener array, it's not redacted
//   after_unknown["http_listener"]   = [{"id":true,"frontend_ip_configuration_id":true}]
//     → container marker: show the http_listener array, only sub-fields are unknown
//   after_unknown["id"]              = true
//     → scalar true: this specific field is unknown, show "(known after apply)"
//   after_sensitive["password"]      = true
//     → scalar true: this specific field is sensitive, show "(sensitive)"
func formatValue(val interface{}, unknown interface{}, sensitive interface{}) string {
	// Sensitive: only suppress when the marker is scalar true.
	// hasAnySensitiveLeaf(true) = true; hasAnySensitiveLeaf([{}]) = false.
	if hasAnySensitiveLeaf(sensitive) {
		return "(sensitive)"
	}
	// Unknown: only suppress when the marker is scalar true (not a container).
	// A container unknown marker means sub-fields are unknown, not the whole value.
	if unknown == true {
		return "(known after apply)"
	}
	if val == nil {
		return "(null)"
	}
	switch v := val.(type) {
	case string:
		return fmt.Sprintf("%q", v)
	case bool:
		return fmt.Sprintf("%v", v)
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%g", v)
	case map[string]interface{}:
		if len(v) == 0 {
			return "{}"
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	case []interface{}:
		if len(v) == 0 {
			return "[]"
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// prettyBlock renders a Terraform block attribute ([]interface{} of map objects)
// as indented key/value lines. Returns nil if the value is not a block array
// or has no displayable content after filtering empty values.
//
// Output format (indent = leading spaces, e.g. "  "):
//
//	indent[0] name:     "bp-web-ppr"
//	indentXXX ip_addresses: ["10.253.25.12"]
//
// where XXX aligns all keys in the block to the same column.
// Empty/null/false/"" values inside each object are filtered out (same
// rules as isEmptyValue at the top level).
func prettyBlock(indent string, val interface{}, unknown interface{}, sensitive interface{}) []string {
	arr, ok := val.([]interface{})
	if !ok || len(arr) == 0 {
		return nil
	}
	// Only pretty-print if every element is a map (a Terraform block list).
	for _, elem := range arr {
		if _, isMap := elem.(map[string]interface{}); !isMap {
			return nil
		}
	}

	// unknownArr / sensitiveArr: per-element markers for sub-fields.
	unknownArr, _ := unknown.([]interface{})
	sensitiveArr, _ := sensitive.([]interface{})

	var lines []string
	// indexWidth: number of digits in the largest index (for alignment).
	indexWidth := len(fmt.Sprintf("%d", len(arr)-1))

	for i, elem := range arr {
		obj := elem.(map[string]interface{})

		var elemUnknown map[string]interface{}
		var elemSensitive map[string]interface{}
		if i < len(unknownArr) {
			elemUnknown, _ = unknownArr[i].(map[string]interface{})
		}
		if i < len(sensitiveArr) {
			elemSensitive, _ = sensitiveArr[i].(map[string]interface{})
		}

		// Prefix computation must come before pairs collection (recursive calls need contPfx).
		idxStr := fmt.Sprintf("[%*d]", indexWidth, i)
		firstPfx := indent + idxStr + " "
		contPfx := indent + strings.Repeat(" ", len(idxStr)) + " "

		// Collect displayable key/value pairs (filter empty/null/defaults).
		// For nested block arrays, recurse instead of flat JSON.
		type kvEntry struct {
			k     string
			v     string    // flat value (non-nil when not a block)
			lines []string  // sub-lines (non-nil when nested block)
		}
		var pairs []kvEntry
		for _, k := range sortedKeys(obj) {
			fieldVal := obj[k]
			fieldUnknown := elemUnknown[k]
			fieldSensitive := elemSensitive[k]

			// Try recursive block expansion first.
			subLines := prettyBlock(contPfx+"    ", fieldVal, fieldUnknown, fieldSensitive)
			if subLines != nil {
				pairs = append(pairs, kvEntry{k: k, lines: subLines})
				continue
			}

			vStr := formatValue(fieldVal, fieldUnknown, fieldSensitive)
			if isEmptyValue(vStr) || vStr == "(known after apply)" {
				continue
			}
			pairs = append(pairs, kvEntry{k: k, v: vStr})
		}
		if len(pairs) == 0 {
			continue
		}

		// Align keys in this block element (flat pairs only; nested blocks get their own lines).
		maxKeyLen := 0
		for _, p := range pairs {
			if len(p.k) > maxKeyLen {
				maxKeyLen = len(p.k)
			}
		}

		for j, p := range pairs {
			padding := strings.Repeat(" ", maxKeyLen-len(p.k))
			keyPart := p.k + ":" + padding
			pfx := contPfx
			if j == 0 {
				pfx = firstPfx
			}
			if p.lines != nil {
				// Nested block: emit "pfx key:" header then indented sub-lines.
				lines = append(lines, pfx+keyPart)
				lines = append(lines, p.lines...)
			} else {
				lines = append(lines, pfx+keyPart+" "+p.v)
			}
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

// isTruthy returns true if the value is boolean true, a non-empty map, or a non-empty slice.
func isTruthy(v interface{}) bool {
	if v == nil {
		return false
	}
	switch vt := v.(type) {
	case bool:
		return vt
	case map[string]interface{}:
		return len(vt) > 0
	case []interface{}:
		return len(vt) > 0
	}
	return false
}

// toMap safely type-asserts an interface{} to map[string]interface{}.
func toMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mergedKeys(maps ...map[string]interface{}) []string {
	seen := make(map[string]struct{})
	for _, m := range maps {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── Block diff engine ─────────────────────────────────────────────────────────

// toBlockArray checks whether v is a []interface{} where every element is a
// map[string]interface{} (a Terraform block list). Returns the slice and true
// on success, nil and false otherwise.
func toBlockArray(v interface{}) ([]map[string]interface{}, bool) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]map[string]interface{}, 0, len(arr))
	for _, elem := range arr {
		m, isMap := elem.(map[string]interface{})
		if !isMap {
			return nil, false
		}
		result = append(result, m)
	}
	return result, true
}

// toMap2 is like toMap but accepts interface{} (json.RawMessage decoded).
func toMap2(v interface{}) map[string]interface{} {
	return toMap(v)
}

// toInterfaceSlice casts interface{} to []interface{}, returning nil if not a slice.
func toInterfaceSlice(v interface{}) []interface{} {
	if v == nil {
		return nil
	}
	s, ok := v.([]interface{})
	if !ok {
		return nil
	}
	return s
}

// isIDField returns true for fields that are Azure/provider-computed identifiers
// that should be excluded from semantic diff comparisons (id, *_id fields).
func isIDField(k string) bool {
	return k == "id" || strings.HasSuffix(k, "_id")
}

// blockElementName returns the natural identity key of a block element.
// Prefers "name", then falls back to a string index.
func blockElementName(obj map[string]interface{}, idx int) string {
	if n, ok := obj["name"]; ok {
		if s, ok := n.(string); ok && s != "" {
			return s
		}
	}
	return fmt.Sprintf("__idx_%d", idx)
}

// diffBlockArray computes a semantic set-diff between two block arrays, matching
// elements by their "name" field (or positional index as fallback).
//
// For each pair of matched elements it inspects every non-id field:
//   - If all fields are equal → status "unchanged" (omitted from output)
//   - If any scalar field differs → status "changed", FieldDiffs populated
//   - If an element exists only in after → status "added", Lines populated
//   - If an element exists only in before → status "removed", Lines populated
//
// beforeSens/afterSens/afterUnk are the per-array sensitivity/unknown markers
// from resource_changes, used when rendering field values.
func diffBlockArray(
	before, after []map[string]interface{},
	beforeSens, afterSens, afterUnk []interface{},
) []BlockElementDiff {
	// Index before elements by name.
	type indexedElem struct {
		obj  map[string]interface{}
		idx  int
		sens interface{}
	}
	beforeByName := make(map[string]indexedElem, len(before))
	for i, obj := range before {
		name := blockElementName(obj, i)
		var s interface{}
		if i < len(beforeSens) {
			s = beforeSens[i]
		}
		beforeByName[name] = indexedElem{obj: obj, idx: i, sens: s}
	}

	var result []BlockElementDiff

	// Walk after elements in order.
	for i, aObj := range after {
		name := blockElementName(aObj, i)
		var aSens interface{}
		var aUnk interface{}
		if i < len(afterSens) {
			aSens = afterSens[i]
		}
		if i < len(afterUnk) {
			aUnk = afterUnk[i]
		}

		be, exists := beforeByName[name]
		if !exists {
			// New element: render all fields as "added".
			elemLines := prettyBlockElement("  ", i, aObj, aUnk, aSens)
			result = append(result, BlockElementDiff{
				Status: "added",
				Name:   name,
				Lines:  elemLines,
			})
			continue
		}
		// Matched: compare field by field (skip id fields).
		bObj := be.obj
		bSens := be.sens
		allKeys := mergedKeys(bObj, aObj)
		var fieldDiffs []FieldDiff
		for _, k := range allKeys {
			if isIDField(k) {
				continue
			}
			bv := bObj[k]
			av := aObj[k]

			// Check if this field is itself a nested block array.
			bArr, bIsArr := toBlockArray(bv)
			aArr, aIsArr := toBlockArray(av)
			if bIsArr && aIsArr {
				bsArr := toInterfaceSlice(toMap(bSens)[k])
				asArr := toInterfaceSlice(toMap(aSens)[k])
				auArr := toInterfaceSlice(toMap(aUnk)[k])
				subDiffs := diffBlockArray(bArr, aArr, bsArr, asArr, auArr)
				// Check if any sub-element actually changed.
				hasChange := false
				for _, sd := range subDiffs {
					if sd.Status != "unchanged" {
						hasChange = true
						break
					}
				}
				if hasChange {
					fieldDiffs = append(fieldDiffs, FieldDiff{
						Key:      k,
						SubDiffs: subDiffs,
					})
				}
				continue
			}

			bSensField := toMap(bSens)[k]
			aSensField := toMap(aSens)[k]
			aUnkField := toMap(aUnk)[k]
			bStr := formatValue(bv, nil, bSensField)
			aStr := formatValue(av, aUnkField, aSensField)
			if bStr == aStr {
				continue
			}
			// Ignore empty→null and null→null noise.
			if bStr == "(null)" && aStr == "(null)" {
				continue
			}
			if isEmptyValue(bStr) && aStr == "(null)" {
				continue
			}
			if bStr == "(null)" && isEmptyValue(aStr) {
				continue
			}
			fieldDiffs = append(fieldDiffs, FieldDiff{
				Key:    k,
				Before: bStr,
				After:  aStr,
			})
		}

		if len(fieldDiffs) == 0 {
			result = append(result, BlockElementDiff{Status: "unchanged", Name: name})
		} else {
			result = append(result, BlockElementDiff{
				Status:     "changed",
				Name:       name,
				FieldDiffs: fieldDiffs,
			})
		}
		delete(beforeByName, name)
	}

	// Remaining before elements were removed.
	// Emit them in original index order.
	type removedEntry struct {
		name string
		ie   indexedElem
	}
	var removed []removedEntry
	for name, ie := range beforeByName {
		removed = append(removed, removedEntry{name: name, ie: ie})
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].ie.idx < removed[j].ie.idx })
	for _, r := range removed {
		elemLines := prettyBlockElement("  ", r.ie.idx, r.ie.obj, nil, r.ie.sens)
		result = append(result, BlockElementDiff{
			Status: "removed",
			Name:   r.name,
			Lines:  elemLines,
		})
	}

	return result
}

// prettyBlockElement renders a single block element (one map) as indented lines,
// using the same key-alignment logic as prettyBlock but for a single entry.
// idx is used only for the index prefix "[0]".
func prettyBlockElement(indent string, idx int, obj map[string]interface{}, unknown interface{}, sensitive interface{}) []string {
	elemUnknown, _ := unknown.(map[string]interface{})
	elemSensitive, _ := sensitive.(map[string]interface{})

	indexWidth := 1 // single element; width can be 1
	idxStr := fmt.Sprintf("[%*d]", indexWidth, idx)
	firstPfx := indent + idxStr + " "
	contPfx := indent + strings.Repeat(" ", len(idxStr)) + " "

	type kvEntry struct {
		k     string
		v     string
		lines []string
	}
	var pairs []kvEntry
	for _, k := range sortedKeys(obj) {
		if isIDField(k) {
			continue
		}
		fieldVal := obj[k]
		fieldUnknown := elemUnknown[k]
		fieldSensitive := elemSensitive[k]

		subLines := prettyBlock(contPfx+"    ", fieldVal, fieldUnknown, fieldSensitive)
		if subLines != nil {
			pairs = append(pairs, kvEntry{k: k, lines: subLines})
			continue
		}
		vStr := formatValue(fieldVal, fieldUnknown, fieldSensitive)
		if isEmptyValue(vStr) || vStr == "(known after apply)" {
			continue
		}
		pairs = append(pairs, kvEntry{k: k, v: vStr})
	}
	if len(pairs) == 0 {
		return nil
	}

	maxKeyLen := 0
	for _, p := range pairs {
		if len(p.k) > maxKeyLen {
			maxKeyLen = len(p.k)
		}
	}

	var lines []string
	for j, p := range pairs {
		padding := strings.Repeat(" ", maxKeyLen-len(p.k))
		keyPart := p.k + ":" + padding
		pfx := contPfx
		if j == 0 {
			pfx = firstPfx
		}
		if p.lines != nil {
			lines = append(lines, pfx+keyPart)
			lines = append(lines, p.lines...)
		} else {
			lines = append(lines, pfx+keyPart+" "+p.v)
		}
	}
	return lines
}
