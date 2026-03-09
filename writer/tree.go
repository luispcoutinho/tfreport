package writer

import (
	"fmt"
	"io"

	"github.com/luispcoutinho/tfreport/terraformstate"
	"github.com/luispcoutinho/tfreport/tree"
)

// TreeWriter writes resource changes in a tree format.
type TreeWriter struct {
	changes       terraformstate.ResourceChanges
	drawable      bool
	details       bool
	plannedValues terraformstate.PlannedValuesMap
}

func (t TreeWriter) Write(writer io.Writer) error {
	trees := tree.CreateTree(t.changes)

	if t.drawable {
		drawableTree := trees.DrawableTree()
		_, err := fmt.Fprint(writer, drawableTree.String())
		return err
	}

	for _, tr := range trees {
		err := printTree(writer, tr, "", t.details, t.plannedValues)
		if err != nil {
			return fmt.Errorf("error writing data to %s: %s", writer, err.Error())
		}
	}
	return nil
}

// NewTreeWriter returns a new TreeWriter.
func NewTreeWriter(changes terraformstate.ResourceChanges, drawable bool, details bool, pv terraformstate.PlannedValuesMap) Writer {
	return TreeWriter{changes: changes, drawable: drawable, details: details, plannedValues: pv}
}

// renderBlockDiffLines converts BlockElementDiffs into plain text lines for tree output.
// It delegates to the table writer's renderBlockDiffs which returns plain lines.
func renderBlockDiffLines(diffs []terraformstate.BlockElementDiff, prefix string) []string {
	raw := renderBlockDiffs(diffs, "  ")
	result := make([]string, 0, len(raw))
	for _, l := range raw {
		result = append(result, prefix+l)
	}
	return result
}

func printTree(writer io.Writer, t *tree.Tree, prefixSpace string, details bool, pv terraformstate.PlannedValuesMap) error {
	var err error
	prefixSymbol := fmt.Sprintf("%s|---", prefixSpace)
	if t.Value != nil {
		colorPrefix, suffix := terraformstate.GetColorPrefixAndSuffixText(t.Value)
		if details {
			// Resource address: color + bold in details mode
			_, err = fmt.Fprintf(writer, "%s%s\n",
				prefixSymbol,
				colorBold(t.Name+suffix, colorPrefix),
			)
		} else {
			_, err = fmt.Fprintf(writer, "%s%s%s%s%s\n",
				prefixSymbol, colorPrefix, t.Name, suffix, terraformstate.ColorReset)
		}
		if err != nil {
			return fmt.Errorf("error writing data to %s: %s", writer, err.Error())
		}

		if details {
			diffs := terraformstate.GetAttributeDiffs(t.Value, pv)
			detailPrefix := fmt.Sprintf("%s|\t  ", prefixSpace)
			isCreate := t.Value.Change.Actions.Create() && !t.Value.Change.Actions.Delete()
			isDelete := t.Value.Change.Actions.Delete() && !t.Value.Change.Actions.Create()
			for _, d := range diffs {
				bKey := bold(d.Key)
				switch {
				case isCreate:
					if d.Lines != nil {
						_, err = fmt.Fprintf(writer, "%s%s:\n", detailPrefix, bKey)
						for _, l := range d.Lines {
							_, err = fmt.Fprintf(writer, "%s%s\n", detailPrefix, l)
						}
					} else {
						_, err = fmt.Fprintf(writer, "%s%s: %s\n", detailPrefix, bKey, d.After)
					}
				case isDelete:
					_, err = fmt.Fprintf(writer, "%s%s: %s\n", detailPrefix, bKey, d.Before)
				default:
					if d.BlockDiffs != nil {
						_, err = fmt.Fprintf(writer, "%s%s:\n", detailPrefix, bKey)
						for _, l := range renderBlockDiffLines(d.BlockDiffs, detailPrefix) {
							_, err = fmt.Fprintf(writer, "%s\n", l)
						}
					} else {
						_, err = fmt.Fprintf(writer, "%s%s: %s -> %s\n", detailPrefix, bKey, d.Before, d.After)
					}
				}
				if err != nil {
					return fmt.Errorf("error writing data to %s: %s", writer, err.Error())
				}
			}
		}
	} else {
		_, err = fmt.Fprintf(writer, "%s%s\n", prefixSymbol, t.Name)
		if err != nil {
			return fmt.Errorf("error writing data to %s: %s", writer, err.Error())
		}
	}

	for _, c := range t.Children {
		separator := "|"
		err = printTree(writer, c, fmt.Sprintf("%s%s\t", prefixSpace, separator), details, pv)
		if err != nil {
			return fmt.Errorf("error writing data to %s: %s", writer, err.Error())
		}
	}
	return nil
}
