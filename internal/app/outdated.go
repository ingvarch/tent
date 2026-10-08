package app

import (
	"fmt"
	"slices"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/rollout"
)

// couldNotTellOutdated is the start of the warning of a report that failed: the release files that tell the spec hashes
// of the node groups could not be read.
const couldNotTellOutdated = "tent could not tell which nodes are outdated: "

// outdatedNodes returns the machines among stays that a rolling update replaces, group by group in the order of m:
// those without a spec hash or with another one than the builder b gives, and those with the replace label.
func outdatedNodes(m *model.Cluster, b *nodeBuilder, stays []cloud.Instance) []OutdatedNode {
	var out []OutdatedNode
	for _, g := range m.Groups {
		group := rollout.Group{Name: g.Name, SpecHash: b.specHash(g.Name)}
		out = append(out, outdatedOf(stays, group, forcedMachines(stays, []rollout.Group{group}, false))...)
	}
	return out
}

// outdatedNames returns the names of the outdated machines, such as "prod-workers-0 and prod-workers-1".
func outdatedNames(outdated []OutdatedNode) string {
	names := make([]string, len(outdated))
	for i, n := range outdated {
		names[i] = n.Name
	}
	return english.And(names)
}

// outdatedPlanLine returns the line of a text plan that names the outdated machines, with its newline.
func outdatedPlanLine(outdated []OutdatedNode) string {
	return "Outdated: " + outdatedNames(outdated) + "; " + replaces(outdated) + ".\n"
}

// replaces returns the clause that says what a rolling update does to the outdated machines: "tent rolling-update
// cluster replaces it" for one, and "... replaces them" for more.
func replaces(outdated []OutdatedNode) string {
	object := "them"
	if len(outdated) == 1 {
		object = "it"
	}
	return "tent rolling-update cluster replaces " + object
}

// outdatedWarning returns the warning about the outdated machines, such as "2 nodes are outdated: prod-workers-0 and
// prod-workers-1; tent rolling-update cluster replaces them", or "" for none.
func outdatedWarning(outdated []OutdatedNode) string {
	if len(outdated) == 0 {
		return ""
	}
	count := "1 node is"
	if len(outdated) > 1 {
		count = fmt.Sprintf("%d nodes are", len(outdated))
	}
	return count + " outdated: " + outdatedNames(outdated) + "; " + replaces(outdated)
}

// staying returns the listed machines that no delete of changes removes.
func staying(listed []cloud.Instance, changes []NodeChange) []cloud.Instance {
	return slices.DeleteFunc(slices.Clone(listed), func(in cloud.Instance) bool {
		return slices.ContainsFunc(changes, func(c NodeChange) bool { return c.Action == NodeDelete && c.ID == in.ID })
	})
}
