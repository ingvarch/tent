package app

import (
	"fmt"
	"io"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/engine"
)

// DeletePlan is what a delete of a cluster removes, in the order it goes: the nodes, the infrastructure, such as the
// network, firewalls and SSH keys, and the state in the state store.
type DeletePlan struct {
	Nodes []NodeChange // the node deletes, by name
	Infra *engine.Plan // the infrastructure's deletes; nil stands for none
	State []string     // the paths of the state, in the order of deletion
	// CloudUnknown reports that the cluster has no cluster.yaml, so tent cannot tell its cloud: the plan deletes the
	// state alone.
	CloudUnknown bool
	// Unsupported is the provider that the cluster's cluster.yaml names when tent cannot manage clusters on it yet:
	// tent made no cloud objects there, so the plan deletes the state alone. It is empty otherwise.
	Unsupported v1alpha1.Provider
	// Applied reports that DeleteCluster made every change of the plan.
	Applied bool
}

// HasChanges reports whether the plan deletes a node, an infrastructure object or a path of the state.
func (p DeletePlan) HasChanges() bool {
	return len(p.Nodes) > 0 || p.infraChanges() || len(p.State) > 0
}

// infraChanges reports whether the plan deletes infrastructure objects.
func (p DeletePlan) infraChanges() bool { return p.Infra != nil && p.Infra.HasChanges() }

// WriteText writes the plan for people, in the order of deletion: a line for each node, the lines of the
// infrastructure's deletes as the engine writes them, and a line for each path of the state, such as
// "- state prod/cluster.yaml"; then a blank line and a line of counts for each part that deletes anything. A plan
// that deletes nothing is the line "No changes.".
func (p DeletePlan) WriteText(w io.Writer) error {
	var lines, counts strings.Builder
	if len(p.Nodes) > 0 {
		for _, c := range p.Nodes {
			lines.WriteString(c.line() + "\n")
		}
		fmt.Fprintf(&counts, "Nodes: %d to delete.\n", len(p.Nodes))
	}
	if p.infraChanges() {
		if err := writeInfra(&lines, &counts, p.Infra); err != nil {
			return err
		}
	}
	if len(p.State) > 0 {
		for _, path := range p.State {
			lines.WriteString("- state " + path + "\n")
		}
		counts.WriteString(stateCount(len(p.State)))
	}
	text := "No changes.\n"
	if counts.Len() > 0 {
		text = lines.String() + "\n" + counts.String()
	}
	return writePlan(w, text)
}

// stateCount returns the line that counts the paths of the state to delete, such as "State: 5 objects to delete.".
func stateCount(n int) string {
	if n == 1 {
		return "State: 1 object to delete.\n"
	}
	return fmt.Sprintf("State: %d objects to delete.\n", n)
}

// WriteApplied writes what applying the plan deleted, on one line, such as "Deleted: 6 nodes, 3 infrastructure
// objects, 4 state objects.", each part only when it deleted anything. A plan that deletes nothing is the line
// "No changes.".
func (p DeletePlan) WriteApplied(w io.Writer) error {
	var infra int
	if p.Infra != nil {
		infra = len(p.Infra.Changes())
	}
	var counts []string
	for _, part := range []struct {
		n    int
		noun string
	}{{len(p.Nodes), "node"}, {infra, "infrastructure object"}, {len(p.State), "state object"}} {
		switch {
		case part.n == 1:
			counts = append(counts, "1 "+part.noun)
		case part.n > 1:
			counts = append(counts, fmt.Sprintf("%d %ss", part.n, part.noun))
		}
	}
	if len(counts) == 0 {
		return writeSummary(w, nil)
	}
	return writeSummary(w, []string{"Deleted: " + strings.Join(counts, ", ") + "."})
}

// MarshalJSON encodes the plan as {"applied": true, "nodes": [...], "infrastructure": <the engine's plan>,
// "state": [...], "cloudUnknown": true, "unsupportedProvider": "hetzner"}, each part in the order of deletion. Nodes
// and state are [] when empty, infrastructure is null when the plan has no infrastructure plan, and applied,
// cloudUnknown and unsupportedProvider are left out when they are false or empty. It leaves HTML characters such as <
// and & as they are, so the caller's encoder decides whether to escape them.
func (p DeletePlan) MarshalJSON() ([]byte, error) {
	return marshalPlan(struct {
		Applied        bool              `json:"applied,omitempty"`
		Nodes          []NodeChange      `json:"nodes"`
		Infrastructure *engine.Plan      `json:"infrastructure"`
		State          []string          `json:"state"`
		CloudUnknown   bool              `json:"cloudUnknown,omitempty"`
		Unsupported    v1alpha1.Provider `json:"unsupportedProvider,omitempty"`
	}{p.Applied, orEmpty(p.Nodes), p.Infra, orEmpty(p.State), p.CloudUnknown, p.Unsupported})
}
