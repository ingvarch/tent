package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/english"
)

// UpdatePlan is what an update of a cluster changes: its infrastructure, its nodes, Nomad, and its secrets, completed
// spec and bootstrap mark in the state store.
type UpdatePlan struct {
	Infra *engine.Plan // the infrastructure's changes; nil stands for none
	Nodes []NodeChange // the node changes in the order they run
	Nomad *NomadStep   // what update does with Nomad; nil stands for nothing
	// Secrets are the paths of the secrets that the store lacks and the update writes, such as pki/private/ca.key,
	// relative to the cluster and in the order they are written.
	Secrets []string
	// Completed reports that the completed spec, the specs with every default as last applied, will be written:
	// the stored one is missing or differs.
	Completed bool
	// Outdated are the machines that stay and that a rolling update replaces, by node group and name; they are no
	// change of the update. It is empty when none is outdated, and when the release files that tell the node groups'
	// spec hashes could not be read.
	Outdated []OutdatedNode
	// Applied reports that Update made every change of the plan; a plan without changes that Update was asked to
	// apply counts as applied too.
	Applied bool
}

// NomadStep is what an update does with Nomad once the servers run: bootstrap the ACL system, and wait until the
// servers are healthy and vote.
type NomadStep struct {
	Bootstrap bool `json:"bootstrap"` // bootstrap the ACL system and write its mark to the state store
	Servers   int  `json:"servers"`   // how many servers must be healthy and vote
}

// HasChanges reports whether the plan changes the infrastructure, the nodes, Nomad, the secrets or the completed spec.
func (p UpdatePlan) HasChanges() bool {
	return p.infraChanges() || len(p.Nodes) > 0 || p.Nomad != nil || len(p.stateWrites()) > 0
}

// infraChanges reports whether the plan changes the infrastructure.
func (p UpdatePlan) infraChanges() bool { return p.Infra != nil && p.Infra.HasChanges() }

// stateWrites returns the paths, relative to the cluster, of the objects that the plan writes to the state store, in
// the order they are written: the secrets, the completed spec, then the mark of the Nomad bootstrap.
func (p UpdatePlan) stateWrites() []string {
	writes := slices.Clone(p.Secrets)
	if p.Completed {
		writes = append(writes, "cluster.completed.yaml")
	}
	if p.Nomad != nil && p.Nomad.Bootstrap {
		writes = append(writes, "nomad/bootstrapped")
	}
	return writes
}

// WriteText writes the plan for people: the lines of the infrastructure's changes as the engine writes them, a line
// for each node change, then a blank line and a line of counts for each of the two parts that changes, a line for the
// Nomad step, such as "Nomad: bootstrap the ACL system and wait for 3 healthy servers.", and last a line that names
// the objects it writes to the state store in the order they are written, such as "State: secrets/gossip.key and
// cluster.completed.yaml will be written.". A plan that changes only the state store is that line alone, and a plan
// without changes is the line "No changes.". When machines are outdated, a last line names them, such as "Outdated:
// prod-workers-0 and prod-workers-1; tent rolling-update cluster replaces them.". Operation ids and the secrets'
// contents do not show.
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
	if p.Nomad != nil {
		text += p.Nomad.planLine() + "\n"
	}
	if writes := p.stateWrites(); len(writes) > 0 {
		text += "State: " + english.And(writes) + " will be written.\n"
	}
	if text == "" {
		text = "No changes.\n"
	}
	if len(p.Outdated) > 0 {
		text += outdatedPlanLine(p.Outdated)
	}
	return writeText(w, "the plan", text)
}

// WriteOutdated writes the line that names the outdated machines, and nothing when there are none.
func (p UpdatePlan) WriteOutdated(w io.Writer) error {
	if len(p.Outdated) == 0 {
		return nil
	}
	return writeText(w, "the outdated machines", outdatedPlanLine(p.Outdated))
}

// planLine returns the step's line in a text plan, without its newline.
func (n NomadStep) planLine() string {
	wait := fmt.Sprintf("wait for %d healthy %s", n.Servers, serverNoun(n.Servers))
	if n.Bootstrap {
		return "Nomad: bootstrap the ACL system and " + wait + "."
	}
	return "Nomad: " + wait + "."
}

// appliedLine returns the step's part of a summary of an applied plan.
func (n NomadStep) appliedLine() string {
	verb := "are"
	if n.Servers == 1 {
		verb = "is"
	}
	healthy := fmt.Sprintf("%d %s %s healthy.", n.Servers, serverNoun(n.Servers), verb)
	if n.Bootstrap {
		return "Nomad: bootstrapped the ACL system; " + healthy
	}
	return "Nomad: " + healthy
}

// serverNoun returns "server" for one and "servers" for any other count.
func serverNoun(n int) string {
	if n == 1 {
		return "server"
	}
	return "servers"
}

// writeText writes text, which is what, to w.
func writeText(w io.Writer, what, text string) error {
	if _, err := io.WriteString(w, text); err != nil {
		return fmt.Errorf("writing %s: %w", what, err)
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
		return fmt.Sprintf("~ node %s (ID %s, wait until it joins, scrub its user data)", c.Name, c.ID)
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
// for, 0 deleted.", the Nomad step, such as "Nomad: bootstrapped the ACL system; 3 servers are healthy.", and the
// objects it wrote to the state store, such as "Wrote secrets/gossip.key and cluster.completed.yaml.", each only when
// that part changed. A plan without changes is the line "No changes.".
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
	if p.Nomad != nil {
		parts = append(parts, p.Nomad.appliedLine())
	}
	if writes := p.stateWrites(); len(writes) > 0 {
		parts = append(parts, "Wrote "+english.And(writes)+".")
	}
	return writeSummary(w, parts)
}

// writeSummary writes the parts of a summary of an applied plan to w, on one line, or "No changes." for none.
func writeSummary(w io.Writer, parts []string) error {
	if len(parts) == 0 {
		return writeText(w, "the plan", "No changes.\n")
	}
	return writeText(w, "the plan", strings.Join(parts, " ")+"\n")
}

// MarshalJSON encodes the plan as {"applied": true, "infrastructure": <the engine's plan>, "nodes": [...],
// "outdated": [{"name": "prod-workers-0", "id": "instance-4", "group": "workers", "reason": "spec hash"}], "nomad":
// {"bootstrap": true, "servers": 3}, "secrets": ["pki/private/ca.key", ...], "completedSpec": true}, the node changes
// in the order they run and the secrets in the order they are written. Nodes and outdated are [] when there are none,
// infrastructure is null when the plan has no infrastructure plan, and applied, nomad, secrets and completedSpec are
// left out when they are false, nil or empty. It leaves HTML characters such as < and & as they are, so the caller's
// encoder decides whether to escape them.
func (p UpdatePlan) MarshalJSON() ([]byte, error) {
	return marshalJSON("the plan", struct {
		Applied        bool           `json:"applied,omitempty"`
		Infrastructure *engine.Plan   `json:"infrastructure"`
		Nodes          []NodeChange   `json:"nodes"`
		Outdated       []OutdatedNode `json:"outdated"`
		Nomad          *NomadStep     `json:"nomad,omitempty"`
		Secrets        []string       `json:"secrets,omitempty"`
		CompletedSpec  bool           `json:"completedSpec,omitempty"`
	}{p.Applied, p.Infra, orEmpty(p.Nodes), orEmpty(p.Outdated), p.Nomad, p.Secrets, p.Completed})
}

// marshalJSON encodes v, which is what, as JSON on one line, and leaves HTML characters such as < and & as they are.
func marshalJSON(what string, v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding %s: %w", what, err)
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
