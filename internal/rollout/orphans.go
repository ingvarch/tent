package rollout

import (
	"cmp"
	"slices"
	"strings"
)

// orphans returns the nodes of the group's name pattern that no machine of the cluster has, by name and ID.
func orphans(s State, g Group) []Node {
	var found []Node
	for _, n := range s.Nomad.Nodes {
		if isNodeOf(s.Cluster, g.Name, n.Name) && !hasMachine(s, g, n) {
			found = append(found, n)
		}
	}
	slices.SortStableFunc(found, func(a, b Node) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return found
}

// hasMachine reports whether a listed machine of the cluster has the node's name and address, or a machine of the
// group of that name lacks an address, so that its node cannot be told from an orphan.
func hasMachine(s State, g Group, n Node) bool {
	return slices.ContainsFunc(s.Machines, func(m Machine) bool {
		return at(m, n) || (m.Group == g.Name && m.Name == n.Name && !m.PrivateIP.IsValid())
	})
}

// isNodeOf reports whether name is <cluster>-<group>-<digits>.
func isNodeOf(cluster, group, name string) bool {
	index, ok := strings.CutPrefix(name, namePrefix(cluster, group))
	return ok && index != "" && strings.Trim(index, "0123456789") == ""
}

// purgeOrphan purges a node of the group that is down and that no machine has.
func purgeOrphan(s State, g Group) (Step, bool, error) {
	for _, n := range orphans(s, g) {
		if n.Status == nodeDown {
			return Step{Action: Purge, Group: g.Name, Node: n}, true, nil
		}
	}
	return Step{}, false, nil
}

// waitOrphan waits for a node of the group that no machine has to go down.
func waitOrphan(s State, g Group) (Step, bool, error) {
	if found := orphans(s, g); len(found) > 0 {
		return Step{Action: WaitNodeDown, Group: g.Name, Node: found[0]}, true, nil
	}
	return Step{}, false, nil
}
