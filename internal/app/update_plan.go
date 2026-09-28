package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ingvarch/tent/internal/engine"
)

// UpdatePlan is what an update of a cluster changes: its infrastructure, its nodes, and its completed spec in the
// state store.
type UpdatePlan struct {
	Infra *engine.Plan // the infrastructure's changes; nil stands for none
	Nodes []NodeChange // the node changes in the order they run
	// Completed reports that the completed spec, the specs with every default as last applied, will be written:
	// the stored one is missing or differs.
	Completed bool
	// Applied reports that Update made every change of the plan; a plan without changes that Update was asked to
	// apply counts as applied too.
	Applied bool
}

// HasChanges reports whether the plan changes the infrastructure, the nodes or the completed spec.
func (p UpdatePlan) HasChanges() bool { return p.infraChanges() || len(p.Nodes) > 0 || p.Completed }

// infraChanges reports whether the plan changes the infrastructure.
func (p UpdatePlan) infraChanges() bool { return p.Infra != nil && p.Infra.HasChanges() }

// completedLine is the line of a text plan that writes the completed spec.
const completedLine = "State: cluster.completed.yaml will be written.\n"

// WriteText writes the plan for people: the lines of the infrastructure's changes as the engine writes them, a line
// for each node change, then a blank line and a line of counts for each of the two parts that changes, and last the
// line "State: cluster.completed.yaml will be written." when the plan writes the completed spec. A plan that changes
// only the completed spec is that line alone, and a plan without changes is the line "No changes.". Operation ids do
// not show.
func (p UpdatePlan) WriteText(w io.Writer) error {
	var lines, counts strings.Builder
	if p.infraChanges() {
		if err := writeInfra(&lines, &counts, p.Infra); err != nil {
			return err
		}
	}
	if len(p.Nodes) > 0 {
		for _, c := range p.Nodes {
			lines.WriteString(c.line() + "\n")
		}
		counts.WriteString(nodeCounts(p.Nodes))
	}
	var text string
	if counts.Len() > 0 {
		text = lines.String() + "\n" + counts.String()
	}
	if p.Completed {
		text += completedLine
	}
	if text == "" {
		text = "No changes.\n"
	}
	return writePlan(w, text)
}

// writePlan writes the text of a plan to w.
func writePlan(w io.Writer, text string) error {
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf("writing the plan: %w", err)
	}
	return nil
}

// writeInfra writes the lines of the infrastructure plan p's changes to lines, as the engine writes them, and the line
// that counts them to counts.
func writeInfra(lines, counts *strings.Builder, p *engine.Plan) error {
	if err := p.WriteChanges(lines); err != nil {
		return err
	}
	counts.WriteString(p.Summary().String() + "\n")
	return nil
}

// line returns the change's line in a text plan, such as "+ node prod-servers-0 (server, vc2-2c-4gb, ams)".
func (c NodeChange) line() string {
	switch c.Action {
	case NodeCreate:
		return fmt.Sprintf("+ node %s (%s, %s, %s)", c.Name, c.Role, c.MachineType, c.Zone)
	case NodeWait:
		return fmt.Sprintf("~ node %s (ID %s, wait until it is ready)", c.Name, c.ID)
	case NodeDelete:
		if c.Reason == "" {
			return fmt.Sprintf("- node %s (ID %s)", c.Name, c.ID)
		}
		return fmt.Sprintf("- node %s (ID %s, %s)", c.Name, c.ID, c.Reason)
	default:
		return fmt.Sprintf("%s node %s", c.Action, c.Name)
	}
}

// nodeCounts returns the line that counts node changes by action.
func nodeCounts(changes []NodeChange) string {
	n := countNodes(changes)
	return fmt.Sprintf("Nodes: %d to create, %d to wait for, %d to delete.\n", n[NodeCreate], n[NodeWait], n[NodeDelete])
}

// countNodes counts node changes by action.
func countNodes(changes []NodeChange) map[NodeAction]int {
	n := make(map[NodeAction]int, len(nodeActionNames))
	for _, c := range changes {
		n[c.Action]++
	}
	return n
}

// WriteApplied writes what applying the plan did, on one line in the past tense: the infrastructure's changes, such
// as "Applied: 3 created, 0 updated, 0 replaced, 0 deleted.", the node changes, such as "Nodes: 6 created, 0 waited
// for, 0 deleted.", and "Wrote cluster.completed.yaml.", each only when that part changed. A plan without changes is
// the line "No changes.".
func (p UpdatePlan) WriteApplied(w io.Writer) error {
	var parts []string
	if p.infraChanges() {
		n := p.Infra.Summary()
		parts = append(parts, fmt.Sprintf("Applied: %d created, %d updated, %d replaced, %d deleted.",
			n.Create, n.Update, n.Replace, n.Delete))
	}
	if len(p.Nodes) > 0 {
		n := countNodes(p.Nodes)
		parts = append(parts, fmt.Sprintf("Nodes: %d created, %d waited for, %d deleted.",
			n[NodeCreate], n[NodeWait], n[NodeDelete]))
	}
	if p.Completed {
		parts = append(parts, "Wrote cluster.completed.yaml.")
	}
	return writeSummary(w, parts)
}

// writeSummary writes the parts of a summary of an applied plan to w, on one line, or "No changes." for none.
func writeSummary(w io.Writer, parts []string) error {
	if len(parts) == 0 {
		return writePlan(w, "No changes.\n")
	}
	return writePlan(w, strings.Join(parts, " ")+"\n")
}

// MarshalJSON encodes the plan as {"applied": true, "infrastructure": <the engine's plan>, "nodes": [...],
// "completedSpec": true}, the node changes in the order they run. Nodes is [] when nothing changes, infrastructure is
// null when the plan has no infrastructure plan, and applied and completedSpec are left out when they are false. It
// leaves HTML characters such as < and & as they are, so the caller's encoder decides whether to escape them.
func (p UpdatePlan) MarshalJSON() ([]byte, error) {
	return marshalPlan(struct {
		Applied        bool         `json:"applied,omitempty"`
		Infrastructure *engine.Plan `json:"infrastructure"`
		Nodes          []NodeChange `json:"nodes"`
		CompletedSpec  bool         `json:"completedSpec,omitempty"`
	}{p.Applied, p.Infra, orEmpty(p.Nodes), p.Completed})
}

// marshalPlan encodes the plan v as JSON on one line, and leaves HTML characters such as < and & as they are.
func marshalPlan(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding the plan: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// orEmpty returns s, or an empty slice when s is nil, so that JSON shows [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
