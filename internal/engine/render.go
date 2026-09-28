package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Summary counts the changes of a plan by action.
type Summary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Replace int `json:"replace"`
	Delete  int `json:"delete"`
}

// String returns the counts as the last line of a text plan shows them, such as
// "Plan: 2 to create, 1 to update, 1 to replace, 2 to delete.".
func (s Summary) String() string {
	return fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to delete.", s.Create, s.Update,
		s.Replace, s.Delete)
}

// Summary returns the counts of the plan's changes by action.
func (p *Plan) Summary() Summary { return summarize(p.Changes()) }

// summarize counts changes by action.
func summarize(changes []PlannedChange) Summary {
	var s Summary
	for _, c := range changes {
		switch c.Action {
		case Create:
			s.Create++
		case Update:
			s.Update++
		case Replace:
			s.Replace++
		case Delete:
			s.Delete++
		}
	}
	return s
}

// MarshalJSON encodes the plan as {"changes": [...], "summary": {...}}, the changes in apply order. It leaves HTML
// characters such as < and & as they are, so the caller's encoder decides whether to escape them.
func (p *Plan) MarshalJSON() ([]byte, error) {
	changes := p.Changes()
	if changes == nil {
		changes = []PlannedChange{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Changes []PlannedChange `json:"changes"`
		Summary Summary         `json:"summary"`
	}{changes, summarize(changes)})
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// symbols mark the actions in a text plan.
var symbols = map[Action]string{Create: "+", Update: "~", Replace: "-/+", Delete: "-"}

// WriteText writes the plan for people: the lines of WriteChanges, then a blank line and the line of Summary. A plan
// without changes is the line "No changes.".
func (p *Plan) WriteText(w io.Writer) error {
	changes := p.Changes()
	if len(changes) == 0 {
		return writeText(w, "No changes.\n")
	}
	var b strings.Builder
	for _, c := range changes {
		writeChange(&b, c)
	}
	b.WriteString("\n" + summarize(changes).String() + "\n")
	return writeText(w, b.String())
}

// WriteChanges writes a line for each change in apply order, with its field diffs indented below it. A plan without
// changes writes nothing.
func (p *Plan) WriteChanges(w io.Writer) error {
	var b strings.Builder
	for _, c := range p.Changes() {
		writeChange(&b, c)
	}
	return writeText(w, b.String())
}

// writeText writes the text of a plan to w.
func writeText(w io.Writer, text string) error {
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}

// writeChange writes the line of c, such as "- vultr.VPC/prod (ID vpc-2, duplicate)", and a line for each field
// diff.
func writeChange(b *strings.Builder, c PlannedChange) {
	b.WriteString(symbols[c.Action] + " " + c.String()) // c.String is its key's
	var notes []string
	if c.Action == Delete {
		notes = append(notes, "ID "+c.ID)
	}
	if c.Reason != "" {
		notes = append(notes, c.Reason)
	}
	if len(notes) > 0 {
		b.WriteString(" (" + strings.Join(notes, ", ") + ")")
	}
	b.WriteString("\n")
	for _, d := range c.Diff {
		b.WriteString("    " + diffLine(d) + "\n")
	}
}

// diffLine returns d as "+ field: new" for an added value, "- field: old" for a removed one, "~ field: old -> new"
// for a changed one, and "~ field" when the diff has neither value.
func diffLine(d FieldDiff) string {
	switch {
	case d.Old == "" && d.New == "":
		return "~ " + d.Field
	case d.Old == "":
		return "+ " + d.Field + ": " + d.New
	case d.New == "":
		return "- " + d.Field + ": " + d.Old
	default:
		return "~ " + d.Field + ": " + d.Old + " -> " + d.New
	}
}
