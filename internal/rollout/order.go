package rollout

import (
	"cmp"
	"fmt"
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

// nodeWhy says why the node of a machine cannot take work, and "" when it can; ok is false when Nomad lists no node
// for the machine.
func nodeWhy(m Machine, n Node, ok bool) string {
	switch {
	case !m.Ready:
		return "is not running"
	case !ok && !m.PrivateIP.IsValid():
		return "has no node in Nomad: the cloud reports no private address for it"
	case !ok:
		return "has no node in Nomad"
	case n.Status != nodeReady:
		return fmt.Sprintf("is %s in Nomad", n.Status)
	case !n.Eligible:
		return "is not eligible"
	case n.Draining:
		return "is draining"
	default:
		return ""
	}
}

// victimOrder sorts machines in the order in which a group loses them. A machine for which a later predicate holds
// comes after one for which it does not, the predicates taken in turn. Then come the machines in the zone with the
// most machines of the group, the oldest, and the lowest ID on a tie.
func victimOrder(ms []Machine, perZone map[string]int, later ...func(Machine) bool) {
	slices.SortStableFunc(ms, func(a, b Machine) int {
		for _, after := range later {
			if c := compareBool(after(a), after(b)); c != 0 {
				return c
			}
		}
		return cmp.Or(
			cmp.Compare(perZone[b.Zone], perZone[a.Zone]),
			CompareCreated(a.Created, b.Created),
			cmp.Compare(a.ID, b.ID),
		)
	})
}

// upToDate returns the machines that are not outdated.
func upToDate(s State, g Group, ms []Machine) []Machine {
	var fresh []Machine
	for _, m := range ms {
		if !outdated(s, g, m) {
			fresh = append(fresh, m)
		}
	}
	return fresh
}

// zoneCounts returns how many of the machines each zone has.
func zoneCounts(ms []Machine) map[string]int {
	perZone := map[string]int{}
	for _, m := range ms {
		perZone[m.Zone]++
	}
	return perZone
}

// newMachine returns the machine that a group creates next: the lowest free name of the cluster, and the zone with the
// fewest machines that are up to date.
func newMachine(s State, g Group, ms []Machine) Machine {
	taken := make(map[string]bool, len(s.Machines))
	for _, m := range s.Machines {
		taken[m.Name] = true
	}
	return Machine{
		Name:  FreeName(s.Cluster, g.Name, taken),
		Group: g.Name,
		Role:  g.Role,
		Zone:  LeastUsedZone(g.Zones, zoneCounts(upToDate(s, g, ms))),
	}
}

// waitJoined waits for the first machine of the group that has not joined.
func waitJoined(g Group, ms []Machine) (Step, bool, error) {
	for _, m := range ms {
		if !m.Joined {
			return Step{Action: WaitJoined, Group: g.Name, Machine: m}, true, nil
		}
	}
	return Step{}, false, nil
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
