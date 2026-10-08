package app

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/rollout"
)

// RollPlan is what a rolling update finds and does: the outdated machines of each group that it rolls, the next step,
// and once applied what it did.
type RollPlan struct {
	Groups  []RollGroup // the groups of the run, by name
	Next    *RollStep   // the next step; nil when nothing is left to roll or the run is refused
	Applied bool        // the roll ran to its end
	Rolled  RollCounts  // what an applied roll did

	refused bool // the run was refused: the plan has groups and no next step, and is not nothing to roll
}

// RollGroup is a node group of a rolling update with its outdated machines.
type RollGroup struct {
	Name     string         `json:"name"`
	Role     v1alpha1.Role  `json:"role"`
	Size     int            `json:"size"`
	Outdated []OutdatedNode `json:"outdated"` // by name, then ID; [] when none
}

// OutdatedNode is a machine that a rolling update replaces, and why.
type OutdatedNode struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Group  string `json:"group"`
	Reason string `json:"reason"` // "spec hash", "no spec hash" or "forced"
}

// Reasons that a machine is outdated.
const (
	reasonSpecHash   = "spec hash"
	reasonNoSpecHash = "no spec hash"
	reasonForced     = "forced"
)

// RollStep is a step of a rolling update as plans show it.
type RollStep struct {
	Action string `json:"action"`         // such as create, mark-ineligible, drain or wait-drained
	Group  string `json:"group"`          // the node group
	Node   string `json:"node,omitempty"` // the machine's name, else the node's
	ID     string `json:"id,omitempty"`   // the machine's ID; none for a create
	Text   string `json:"text"`           // what the step does, in words
}

// RollCounts count the distinct machines that an applied roll created and deleted and the distinct nodes that it
// drained and purged; a step tried twice counts once.
type RollCounts struct {
	Created int `json:"created"`
	Drained int `json:"drained"`
	Deleted int `json:"deleted"`
	Purged  int `json:"purged"`
}

// rollActionNames name the actions of rollout in plans.
var rollActionNames = [...]string{
	rollout.Done: "done", rollout.Create: "create", rollout.WaitJoined: "wait-joined",
	rollout.MarkIneligible: "mark-ineligible", rollout.Drain: "drain", rollout.WaitDrained: "wait-drained",
	rollout.Delete: "delete", rollout.WaitNodeDown: "wait-node-down", rollout.Purge: "purge",
	rollout.TransferLeadership: "transfer-leadership", rollout.Stop: "stop",
	rollout.WaitServerDown: "wait-server-down", rollout.RemovePeer: "remove-peer", rollout.ForceLeave: "force-leave",
	rollout.WaitHealthy: "wait-healthy", rollout.WaitStable: "wait-stable",
}

// rollActionName returns the name of the action in plans, such as mark-ineligible.
func rollActionName(a rollout.Action) string {
	if a < rollout.Done || int(a) >= len(rollActionNames) {
		return fmt.Sprintf("unknown action %d", int(a))
	}
	return rollActionNames[a]
}

// rollStepOf returns the step as plans show it.
func rollStepOf(s rollout.Step) RollStep {
	return RollStep{
		Action: rollActionName(s.Action), Group: s.Group, Node: cmp.Or(s.Machine.Name, s.Node.Name), ID: s.Machine.ID,
		Text: s.String(),
	}
}

// outdatedOf returns the listed machines of the group that a roll replaces, by name, then ID. A machine without a hash
// is outdated for that, one with another hash than the group's for its hash, and one with the group's hash only when
// forced names it: the hash comes before the force.
func outdatedOf(listed []cloud.Instance, g rollout.Group, forced map[string]bool) []OutdatedNode {
	var out []OutdatedNode
	for _, in := range listed {
		var reason string
		switch {
		case in.Group != g.Name:
			continue
		case in.SpecHash == "":
			reason = reasonNoSpecHash
		case in.SpecHash != g.SpecHash:
			reason = reasonSpecHash
		case forced[in.ID]:
			reason = reasonForced
		default:
			continue
		}
		out = append(out, OutdatedNode{Name: in.Name, ID: in.ID, Group: in.Group, Reason: reason})
	}
	slices.SortFunc(out, func(a, b OutdatedNode) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// WriteText writes the plan for people: a line for each group, such as "node group workers (client, size 2): 2
// outdated: prod-workers-0 (ID instance-6) and prod-workers-1 (ID instance-7)", or "up to date" for a group without
// outdated machines; a forced machine shows ", forced" after its ID. A blank line follows, then the next step, such as
// "Next: create node prod-workers-2 (client of workers, ams).", or "Nothing to roll." when no group has outdated
// machines and the run was not refused. A refused plan ends after its groups.
func (p RollPlan) WriteText(w io.Writer) error {
	var text strings.Builder
	for _, g := range p.Groups {
		text.WriteString(g.line() + "\n")
	}
	switch {
	case p.Next != nil:
		text.WriteString("\nNext: " + p.Next.Text + ".\n")
	case !p.refused && !p.hasOutdated():
		text.WriteString("\nNothing to roll.\n")
	}
	return writeText(w, "the plan", text.String())
}

// line returns the group's line in a text plan, without its newline.
func (g RollGroup) line() string {
	head := fmt.Sprintf("node group %s (%s, size %d): ", g.Name, g.Role, g.Size)
	if len(g.Outdated) == 0 {
		return head + "up to date"
	}
	items := make([]string, len(g.Outdated))
	for i, n := range g.Outdated {
		forced := ""
		if n.Reason == reasonForced {
			forced = ", forced"
		}
		items[i] = fmt.Sprintf("%s (ID %s%s)", n.Name, n.ID, forced)
	}
	return fmt.Sprintf("%s%d outdated: %s", head, len(g.Outdated), english.And(items))
}

// WriteApplied writes what applying the plan did, on one line, such as "Rolled: 2 created, 2 drained, 2 deleted, 2
// purged.".
func (p RollPlan) WriteApplied(w io.Writer) error {
	n := p.Rolled
	return writeText(w, "the summary", fmt.Sprintf("Rolled: %d created, %d drained, %d deleted, %d purged.\n",
		n.Created, n.Drained, n.Deleted, n.Purged))
}

// MarshalJSON encodes the plan as {"applied": true, "groups": [...], "next": {...}, "rolled": {...}}. Groups is a
// list even when empty; applied, next and rolled are left out when the plan did not apply or has no next step. It
// leaves HTML characters such as < and & as they are, so the caller's encoder decides whether to escape them.
func (p RollPlan) MarshalJSON() ([]byte, error) {
	out := struct {
		Applied bool        `json:"applied,omitempty"`
		Groups  []RollGroup `json:"groups"`
		Next    *RollStep   `json:"next,omitempty"`
		Rolled  *RollCounts `json:"rolled,omitempty"`
	}{Applied: p.Applied, Groups: orEmpty(p.Groups), Next: p.Next}
	if p.Applied {
		out.Rolled = &p.Rolled
	}
	return marshalJSON("the plan", out)
}

// MarshalJSON encodes the group, with its outdated machines as a list even when there are none.
func (g RollGroup) MarshalJSON() ([]byte, error) {
	type plain RollGroup // without the method, so that it does not call itself
	g.Outdated = orEmpty(g.Outdated)
	return marshalJSON("the node group", plain(g))
}

// hasOutdated reports whether any group of the plan has outdated machines.
func (p RollPlan) hasOutdated() bool {
	return slices.ContainsFunc(p.Groups, func(g RollGroup) bool { return len(g.Outdated) > 0 })
}
