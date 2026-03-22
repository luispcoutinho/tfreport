package writer

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"github.com/luispcoutinho/tfreport/terraformstate"
	"github.com/olekukonko/tablewriter"
)

// TableWriter writes resource changes in a table format.
type TableWriter struct {
	mdEnabled     bool
	details       bool
	changes       map[string]terraformstate.ResourceChanges
	outputChanges map[string][]string
	plannedValues terraformstate.PlannedValuesMap
}

var tableOrder = []string{"import", "add", "update", "recreate", "delete", "moved"}

func (t TableWriter) Write(writer io.Writer) error {
	if t.details {
		return t.writeDetails(writer)
	}
	return t.writeStandard(writer)
}

// writeStandard is the original tablewriter-based rendering (no -details).
func (t TableWriter) writeStandard(writer io.Writer) error {
	tableString := make([][]string, 0, 4)

	for _, change := range tableOrder {
		changedResources := t.changes[change]
		resourceCount := len(changedResources)
		for _, changedResource := range changedResources {
			changeLabel := fmt.Sprintf("%s (%d)", change, resourceCount)
			if change == "moved" {
				if t.mdEnabled {
					tableString = append(tableString, []string{changeLabel, fmt.Sprintf("`%s` to `%s`", changedResource.PreviousAddress, changedResource.Address)})
				} else {
					tableString = append(tableString, []string{changeLabel, fmt.Sprintf("%s to %s", changedResource.PreviousAddress, changedResource.Address)})
				}
			} else {
				if t.mdEnabled {
					tableString = append(tableString, []string{changeLabel, fmt.Sprintf("`%s`", changedResource.Address)})
				} else {
					tableString = append(tableString, []string{changeLabel, changedResource.Address})
				}
			}
		}
	}

	table := tablewriter.NewWriter(writer)
	table.SetHeader([]string{"Change", "Resource"})
	table.SetAutoMergeCells(true)
	table.SetAutoWrapText(false)
	table.SetRowLine(true)
	table.AppendBulk(tableString)

	if t.mdEnabled {
		table.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
		table.SetCenterSeparator("|")
	}

	table.Render()

	if hasOutputChanges(t.outputChanges) {
		tableString = make([][]string, 0, 4)
		for _, change := range tableOrder {
			changedOutputs := t.outputChanges[change]
			outputCount := len(changedOutputs)
			for _, changedOutput := range changedOutputs {
				if t.mdEnabled {
					tableString = append(tableString, []string{fmt.Sprintf("%s (%d)", change, outputCount), fmt.Sprintf("`%s`", changedOutput)})
				} else {
					tableString = append(tableString, []string{fmt.Sprintf("%s (%d)", change, outputCount), changedOutput})
				}
			}
		}
		table = tablewriter.NewWriter(writer)
		table.SetHeader([]string{"Change", "Output"})
		table.SetAutoMergeCells(true)
		table.SetAutoWrapText(false)
		table.SetRowLine(true)
		table.AppendBulk(tableString)

		if t.mdEnabled {
			_, _ = fmt.Fprint(writer, tablewriter.NEWLINE)
			table.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
			table.SetCenterSeparator("|")
		}

		table.Render()
	}

	return nil
}

// resourceBlock holds the rendered lines for a single resource entry.
type resourceBlock struct {
	change      string   // change type label (first resource in group) or "" (subsequent)
	changeColor string   // ANSI color for this group
	lines       []string // [0]=address, [1:]=attribute diff lines (plain key: value strings)
	lastInGroup bool
}

// terminalWidth returns the current terminal column width.
// It tries ioctl TIOCGWINSZ on stdout, then $COLUMNS, then defaults to 120.
func terminalWidth() int {
	// $COLUMNS takes priority — allows CI pipelines to control output width explicitly.
	if cols := os.Getenv("COLUMNS"); cols != "" {
		var n int
		if _, err := fmt.Sscanf(cols, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }
	ws := &winsize{}
	ret, _, _ := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(1),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if ret == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	return 120
}

// wrapValue splits a value string into lines of at most maxWidth runes.
// Subsequent lines are indented with indent to align with the value start.
// It tries to break at JSON structural characters (},] ,) before hard-wrapping.
func wrapValue(value, indent string, maxWidth int) []string {
	if maxWidth <= 0 || utf8.RuneCountInString(value) <= maxWidth {
		return []string{value}
	}
	var lines []string
	runes := []rune(value)
	for len(runes) > 0 {
		width := maxWidth
		if len(lines) > 0 {
			width = maxWidth // continuation lines same width (indent already applied)
		}
		if width >= len(runes) {
			lines = append(lines, string(runes))
			break
		}
		// Find a good break point: scan backward from width for , } ]
		breakAt := width
		for i := width - 1; i > width/2; i-- {
			ch := runes[i]
			if ch == ',' || ch == '}' || ch == ']' || ch == '{' || ch == '[' {
				breakAt = i + 1
				break
			}
		}
		lines = append(lines, string(runes[:breakAt]))
		runes = []rune(indent + string(runes[breakAt:]))
	}
	return lines
}

// writeDetails renders the details table with full ANSI styling:
//   - Header row/border: bold, no color
//   - All borders/pipes/separators from first === onward: group color
//   - Change label: color + bold
//   - Resource address: color + bold
//   - Attribute keys: bold only
//   - Attribute values: plain
//   - Heavy === between change groups, light --- between resources in group
func (t TableWriter) writeDetails(writer io.Writer) error {
	blocks := make([]resourceBlock, 0)

	for _, change := range tableOrder {
		changedResources := t.changes[change]
		if len(changedResources) == 0 {
			continue
		}

		color := terraformstate.ChangeColor(change)
		isCreate := change == "add" || change == "import"
		isDelete := change == "delete"

		for i, rc := range changedResources {
			var addr string
			if change == "moved" {
				addr = fmt.Sprintf("%s to %s", rc.PreviousAddress, rc.Address)
			} else {
				addr = rc.Address
			}

			// lines[0] = address (plain — styled at render time)
			// lines[1:] = "  key: value" or "  key: before -> after" (plain)
			lines := []string{addr}

			diffs := terraformstate.GetAttributeDiffs(rc, t.plannedValues)
			for _, d := range diffs {
				// Blank separator before every top-level attribute.
				lines = append(lines, "")
				switch {
				case isCreate:
					if d.Lines != nil {
						lines = append(lines, fmt.Sprintf("  %s:", d.Key))
						lines = append(lines, d.Lines...)
					} else {
						lines = append(lines, fmt.Sprintf("  %s: %s", d.Key, d.After))
					}
				case isDelete:
					lines = append(lines, fmt.Sprintf("  %s: %s", d.Key, d.Before))
				default:
					// Update: use BlockDiffs for block arrays, scalar diff otherwise.
					if d.BlockDiffs != nil {
						lines = append(lines, fmt.Sprintf("  %s:", d.Key))
						lines = append(lines, renderBlockDiffs(d.BlockDiffs, "  ")...)
					} else {
						lines = append(lines, fmt.Sprintf("  %s: %s -> %s", d.Key, d.Before, d.After))
					}
				}
			}

			label := ""
			if i == 0 {
				label = change
			}

			blocks = append(blocks, resourceBlock{
				change:      label,
				changeColor: color,
				lines:       lines,
				lastInGroup: i == len(changedResources)-1,
			})
		}
	}

	if len(blocks) == 0 {
		return nil
	}

	// Determine terminal width and col2 wrap budget.
	// Layout: | col1 | col2 |  → 2 pipes + 4 spaces padding + 1 trailing pipe = 7 chars overhead.
	termW := terminalWidth()
	
	// First pass: compute col1W from change labels only.
	col1W := utf8.RuneCountInString("CHANGE")
	for _, b := range blocks {
		if w := utf8.RuneCountInString(b.change); w > col1W {
			col1W = w
		}
	}
	
	// col2 budget: terminal width minus borders/padding minus col1.
	// | SPACE col1 SPACE | SPACE col2 SPACE |
	// = 3 pipes + 2*(1 space each side) = 3 + 4 = 7 chars overhead.
	col2Budget := termW - col1W - 7
	if col2Budget < 40 {
		col2Budget = 40 // minimum usable width
	}
	
	// Second pass: wrap long array values element-by-element.
	// Rules:
	//   - Only lines whose value starts with "[" are candidates.
	//   - Plain quoted strings are never broken mid-value.
	//   - Elements are packed onto lines greedily; a new line is started
	//     only when the next element would exceed col2Budget.
	//   - When a single element is longer than the budget (e.g. an Azure
	//     resource ID), it gets its own line — the table grows to fit it,
	//     but no mid-string break ever occurs.
	for bi := range blocks {
		newLines := []string{blocks[bi].lines[0]} // address line — never wrapped
		for _, line := range blocks[bi].lines[1:] {
			// Blank separator lines pass through unchanged.
			if line == "" {
				newLines = append(newLines, line)
				continue
			}
			// Block header lines ("  key:") have no value — pass through.
			if strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.Contains(line, ": ") {
				newLines = append(newLines, line)
				continue
			}
			// Find "key: value" split point.
			colonIdx := strings.Index(line, ": ")
			if colonIdx < 0 {
				newLines = append(newLines, line)
				continue
			}
			value := line[colonIdx+2:]
			// Only process array values.
			if len(value) == 0 || value[0] != '[' {
				newLines = append(newLines, line)
				continue
			}
			// Line already fits — no wrapping needed.
			if utf8.RuneCountInString(line) <= col2Budget {
				newLines = append(newLines, line)
				continue
			}
			// prefix = everything up to and including ": ".
			// contIndent aligns to one char after the opening "[".
			prefix := line[:colonIdx+2]
			prefixW := utf8.RuneCountInString(prefix)
			contIndent := strings.Repeat(" ", prefixW+1) // +1 for the "["
			// Split the array into individual element strings.
			elems := splitArrayElements(value)
			// Pack elements greedily onto lines.
			current := "["
			firstLine := true
			for i, elem := range elems {
				isLast := i == len(elems)-1
				sep := ","
				if isLast {
					sep = "]"
				}
				candidate := current + elem + sep
				var fullW int
				if firstLine {
					fullW = prefixW + utf8.RuneCountInString(candidate)
				} else {
					fullW = utf8.RuneCountInString(contIndent) + utf8.RuneCountInString(candidate)
				}
				if fullW <= col2Budget || current == "[" {
					// Fits, or we haven't placed any element yet (must place at least one).
					current = candidate
				} else {
					// Flush current line.
					if firstLine {
						newLines = append(newLines, prefix+current)
						firstLine = false
					} else {
						newLines = append(newLines, contIndent+current)
					}
					current = elem + sep
				}
			}
			// Flush the last line.
			if firstLine {
				newLines = append(newLines, prefix+current)
			} else {
				newLines = append(newLines, contIndent+current)
			}
		}
		blocks[bi].lines = newLines
	}

	// Third pass: measure actual col2W after wrapping.
	col2W := utf8.RuneCountInString("RESOURCE")
	for _, b := range blocks {
		for _, line := range b.lines {
			if w := utf8.RuneCountInString(line); w > col2W {
				col2W = w
			}
		}
	}

	p := func(s string) { fmt.Fprintln(writer, s) }

	// hLine builds a full-width horizontal rule.
	// fill is "-" or "=". color optionally tints the fill segments and the pipes.
	hLine := func(fill, color string) string {
		pipe := "+"
		seg1 := strings.Repeat(fill, col1W+2)
		seg2 := strings.Repeat(fill, col2W+2)
		if color != "" {
			pipe = color + "+" + terraformstate.ColorReset
			seg1 = color + seg1 + terraformstate.ColorReset
			seg2 = color + seg2 + terraformstate.ColorReset
		}
		return pipe + seg1 + pipe + seg2 + pipe
	}

	// dataRow builds a data row with colored pipes.
	// c1: left cell content (plain), c2: right cell content (plain)
	// c1Styled: pre-styled version of c1 (with ANSI), c2Styled same for c2.
	// pipeColor: color for | characters; "" = default.
	dataRow := func(c1Plain, c1Styled, c2Plain, c2Styled, pipeColor string) string {
		pad1 := col1W - utf8.RuneCountInString(c1Plain)
		pad2 := col2W - utf8.RuneCountInString(c2Plain)
		pipe := "|"
		if pipeColor != "" {
			pipe = pipeColor + "|" + terraformstate.ColorReset
		}
		return fmt.Sprintf("%s %s%s %s %s%s %s",
			pipe,
			c1Styled, strings.Repeat(" ", pad1),
			pipe,
			c2Styled, strings.Repeat(" ", pad2),
			pipe,
		)
	}

	// styleAttrLine applies bold/color to the key portion of an attribute line.
	// Handles these formats:
	//   "  key: value"        → bold key
	//   "  key:"              → bold key (block header)
	//   "  [+] key: value"    → green [+], rest plain
	//   "  [-] key: value"    → red [-], rest plain
	//   "  [~] key: value"    → yellow [~], rest plain
	//   "    - key: before"   → red - prefix
	//   "    + key: after"    → green + prefix
	//   "  [0] ..."           → pass-through (prettyBlock sub-line)
	//   "      ..."           → pass-through (continuation)
	styleAttrLine := func(line string) string {
		trimmed := strings.TrimPrefix(line, "  ")
		// Block diff status lines: [+], [-], [~]
		if strings.HasPrefix(trimmed, "[+]") {
			rest := trimmed[3:]
			return "  " + colorBold("[+]", terraformstate.ColorGreen) + rest
		}
		if strings.HasPrefix(trimmed, "[-]") {
			rest := trimmed[3:]
			return "  " + colorBold("[-]", terraformstate.ColorRed) + rest
		}
		if strings.HasPrefix(trimmed, "[~]") {
			rest := trimmed[3:]
			return "  " + colorBold("[~]", terraformstate.ColorYellow) + rest
		}
		// Field-level diff lines inside a changed element: "    - key:" / "    + key:"
		// These are indented 4 spaces from the block indent, then "- " or "+ ".
		contTrimmed := strings.TrimPrefix(line, "      ") // 6 spaces (indent + contIndent)
		if strings.HasPrefix(contTrimmed, "- ") {
			rest := contTrimmed[2:]
			colonIdx := strings.Index(rest, ": ")
			if colonIdx >= 0 {
				key := rest[:colonIdx]
				val := rest[colonIdx:]
				return "      " + terraformstate.ColorRed + "- " + bold(key) + val + terraformstate.ColorReset
			}
			return "      " + terraformstate.ColorRed + "- " + rest + terraformstate.ColorReset
		}
		if strings.HasPrefix(contTrimmed, "+ ") {
			rest := contTrimmed[2:]
			colonIdx := strings.Index(rest, ": ")
			if colonIdx >= 0 {
				key := rest[:colonIdx]
				val := rest[colonIdx:]
				return "      " + terraformstate.ColorGreen + "+ " + bold(key) + val + terraformstate.ColorReset
			}
			return "      " + terraformstate.ColorGreen + "+ " + rest + terraformstate.ColorReset
		}
		// Block sub-lines: start with '[' (index like [0]) or ' ' (continuation)
		if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == ' ') {
			return line
		}
		// Block header: "  key:" with no ": " inside
		colonOnly := strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.Contains(line, ": ")
		if colonOnly {
			key := strings.TrimSuffix(strings.TrimSpace(line), ":")
			return "  " + bold(key) + ":"
		}
		// Normal: "  key: value"
		colonIdx := strings.Index(trimmed, ": ")
		if colonIdx < 0 {
			return line
		}
		key := trimmed[:colonIdx]
		rest := trimmed[colonIdx:] // ": value"
		return "  " + bold(key) + rest
	}

	// ── Header (bold, no color) ──────────────────────────────────────────────
	p(hLine("-", ""))
	p(dataRow("CHANGE", bold("CHANGE"), "RESOURCE", bold("RESOURCE"), ""))
	// First heavy separator — no color yet (transition into first group color below)
	p(hLine("=", ""))

	currentColor := ""

	for bi, b := range blocks {
		// On the first row of each group, update the active color
		if b.change != "" {
			currentColor = b.changeColor
		}

		// ── Address row ──────────────────────────────────────────────────────
		c1Plain := b.change
		c1Styled := colorBold(b.change, currentColor) // colored+bold, or "" if blank
		c2Plain := b.lines[0]
		c2Styled := colorBold(b.lines[0], currentColor) // resource address colored+bold
		p(dataRow(c1Plain, c1Styled, c2Plain, c2Styled, currentColor))

		// ── Attribute lines ──────────────────────────────────────────────────
		for _, dl := range b.lines[1:] {
			if dl == "" {
				p(dataRow("", "", "", "", currentColor))
				continue
			}
			styledDl := styleAttrLine(dl)
			p(dataRow("", "", dl, styledDl, currentColor))
		}

		// ── Separator after this block ───────────────────────────────────────
		isLast := bi == len(blocks)-1
		if isLast {
			p(hLine("-", currentColor))
		} else if b.lastInGroup {
			// Heavy separator; tint with the *next* group's color
			nextColor := blocks[bi+1].changeColor
			p(hLine("=", nextColor))
			currentColor = nextColor
		} else {
			// Light separator within the same group
			p(hLine("-", currentColor))
		}
	}

	return nil
}

// NewTableWriter returns a new TableWriter.
// splitArrayElements splits a JSON array string into its top-level element
// strings, preserving exact encoding. It respects nesting (sub-arrays, objects)
// and quoted strings, so commas inside strings or nested structures are not
// treated as element separators.
//
// Input:  `["a","b","c"]`  or  `[1,2,["x","y"]]`
// Output: `["a"`, `"b"`, `"c"]`  (note: brackets are NOT part of elements)
//         Actually returns: `"a"`, `"b"`, `"c"` (inner content only)
// The caller is responsible for re-adding `[`, `,`, `]` when reassembling.
func splitArrayElements(s string) []string {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return []string{s}
	}
	inner := s[1 : len(s)-1]
	var elements []string
	depth := 0
	inString := false
	escaped := false
	start := 0
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"'  {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '[' || ch == '{' {
			depth++
		} else if ch == ']' || ch == '}' {
			depth--
		} else if ch == ',' && depth == 0 {
			elements = append(elements, inner[start:i])
			start = i + 1
		}
	}
	elements = append(elements, inner[start:])
	return elements
}

func NewTableWriter(changes map[string]terraformstate.ResourceChanges, outputChanges map[string][]string, mdEnabled bool, details bool, pv terraformstate.PlannedValuesMap) Writer {
	return TableWriter{
		changes:       changes,
		mdEnabled:     mdEnabled,
		details:       details,
		outputChanges: outputChanges,
		plannedValues: pv,
	}
}

// renderBlockDiffs converts []BlockElementDiff into display lines at the given indent level.
// Each added element is prefixed [+], removed with [-], changed elements show
// only their differing fields with -/+ per field. Unchanged elements are skipped.
// indent is the base indentation (e.g. "  " for top-level block attrs).
func renderBlockDiffs(diffs []terraformstate.BlockElementDiff, indent string) []string {
	// bd.Lines are generated by prettyBlockElement with base indent "  ".
	// Re-apply them at the caller's indent level.
	reindentLine := func(l string) string {
		stripped := strings.TrimPrefix(l, "  ")
		return indent + stripped
	}

	var lines []string
	for _, d := range diffs {
		switch d.Status {
		case "unchanged":
			continue
		case "added":
			if len(d.Lines) == 0 {
				lines = append(lines, fmt.Sprintf("%s[+] name: %q", indent, d.Name))
				continue
			}
			for i, l := range d.Lines {
				rl := reindentLine(l)
				if i == 0 {
					lines = append(lines, rewriteIndexPrefix(rl, indent, "[+]"))
				} else {
					lines = append(lines, rl)
				}
			}
		case "removed":
			if len(d.Lines) == 0 {
				lines = append(lines, fmt.Sprintf("%s[-] name: %q", indent, d.Name))
				continue
			}
			for i, l := range d.Lines {
				rl := reindentLine(l)
				if i == 0 {
					lines = append(lines, rewriteIndexPrefix(rl, indent, "[-]"))
				} else {
					lines = append(lines, rl)
				}
			}
		case "changed":
			lines = append(lines, fmt.Sprintf("%s[~] name: %q", indent, d.Name))
			contIndent := indent + "    "
			for _, fd := range d.FieldDiffs {
				if fd.SubDiffs != nil {
					lines = append(lines, fmt.Sprintf("%s    %s:", contIndent, fd.Key))
					subLines := renderBlockDiffs(fd.SubDiffs, contIndent+"    ")
					lines = append(lines, subLines...)
				} else {
					lines = append(lines, fmt.Sprintf("%s  - %s: %s", contIndent, fd.Key, fd.Before))
					lines = append(lines, fmt.Sprintf("%s  + %s: %s", contIndent, fd.Key, fd.After))
				}
			}
		}
	}
	return lines
}

// rewriteIndexPrefix replaces the leading "[N]" index in a prettyBlock line
// with the given symbol (e.g. "[+]", "[-]").
// e.g. "  [0] name: ..." → "  [+] name: ..."
func rewriteIndexPrefix(line, indent, symbol string) string {
	// prettyBlock produces lines like: indent + "[N] " + rest
	// Find the "]" and replace from indent to "] " with symbol + " ".
	trimmed := strings.TrimPrefix(line, indent)
	closeBracket := strings.Index(trimmed, "]")
	if closeBracket < 0 {
		return line
	}
	rest := trimmed[closeBracket+1:] // " key: value"
	return indent + symbol + rest
}
