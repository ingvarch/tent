package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// summary counts the changes of a plan by action, for the plan's JSON.
type summary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Replace int `json:"replace"`
	Delete  int `json:"delete"`
}

// summarize counts changes by action.
func summarize(changes []PlannedChange) summary {
	var s summary
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
		Summary summary         `json:"summary"`
	}{changes, summarize(changes)})
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// symbols mark the actions in a text plan.
var symbols = map[Action]string{Create: "+", Update: "~", Replace: "-/+", Delete: "-"}

// WriteText writes the plan for people: a line for each change in apply order, with its field diffs indented below
// it, then a blank line and the counts of the changes. A plan without changes is the line "No changes.".
func (p *Plan) WriteText(w io.Writer) error {
	var b strings.Builder
	if changes := p.Changes(); len(changes) == 0 {
		b.WriteString("No changes.\n")
	} else {
		for _, c := range changes {
			writeChange(&b, c)
		}
		s := summarize(changes)
		fmt.Fprintf(&b, "\nPlan: %d to create, %d to update, %d to replace, %d to delete.\n",
			s.Create, s.Update, s.Replace, s.Delete)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
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
