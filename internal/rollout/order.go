package rollout

import (
	"cmp"
	"slices"
)

// Statuses of a client node as Nomad lists them.
const (
	nodeReady = "ready"
	nodeDown  = "down"
)

// outdated reports whether the machine has to be replaced: its hash is not the group's, or it is forced.
func outdated(s State, g Group, m Machine) bool {
	return m.SpecHash != g.SpecHash || s.Forced[m.ID]
}

// at reports whether the node has the machine's name and private address; a machine without an address has no node.
func at(m Machine, n Node) bool {
	return m.PrivateIP.IsValid() && n.Name == m.Name && n.Address == m.PrivateIP
}

// nodeOf returns the node that Nomad lists for the machine: the one of its name and private address, one that is not
// down before one that is.
func nodeOf(nodes []Node, m Machine) (Node, bool) {
	var found Node
	var ok bool
	for _, n := range nodes {
		if !at(m, n) {
			continue
		}
		if !ok || (found.Status == nodeDown && n.Status != nodeDown) {
			found, ok = n, true
		}
	}
	return found, ok
}

// victimOrder sorts machines in the order in which a group loses them: the ones that are not available, then the ones
// in the zone with the most machines of the group, then the oldest, and the lowest ID on a tie. The machines are
// outdated, joined and not under removal already, so those rules decide nothing.
func victimOrder(ms []Machine, available func(Machine) bool, perZone map[string]int) {
	slices.SortStableFunc(ms, func(a, b Machine) int {
		return cmp.Or(
			compareBool(available(a), available(b)),
			cmp.Compare(perZone[b.Zone], perZone[a.Zone]),
			CompareCreated(a.Created, b.Created),
			cmp.Compare(a.ID, b.ID),
		)
	})
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
}
