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

// drainedFor reports whether a drain of the machine's node has completed. Callers ask it of ineligible nodes only: an
// eligible node that carries the machine's ID in its drain meta was made eligible again after the drain, and may hold
// work.
func drainedFor(m Machine, n Node) bool { return n.DrainedFor == m.ID }

// evictionOf is the first step that takes work off an eligible node: a mark, or a drain at once when the node carries
// its machine's drain meta, since a mark would make that old drain look like a new one.
func evictionOf(m Machine, n Node) Action {
	if n.DrainedFor == m.ID {
		return Drain
	}
	return MarkIneligible
}

// groupView is what the rules of one group share: the state, the mode, the group and its machines by name.
type groupView struct {
	s    State
	mode Mode
	g    Group
	ms   []Machine
}

func newGroupView(s State, mode Mode, g Group) groupView {
	return groupView{s: s, mode: mode, g: g, ms: machinesOf(s, g)}
}

// victimOrder sorts machines in the order in which the group loses them. A machine whose removal has started comes
// first. In a shrink, machines that have not joined come next, then outdated ones. A machine for which a later
// predicate holds comes after one for which it does not, the predicates taken in turn. Then come the machines in the
// zone with the most machines of the group, and the oldest, the lowest ID on a tie; a shrink takes the newest and the
// highest ID instead.
func (v *groupView) victimOrder(ms []Machine, started func(Machine) bool, later ...func(Machine) bool) {
	rules := []func(Machine) bool{func(m Machine) bool { return !started(m) }}
	age := 1
	if v.mode == Shrink {
		rules = append(rules, func(m Machine) bool { return m.Joined },
			func(m Machine) bool { return !outdated(v.s, v.g, m) })
		age = -1
	}
	rules = append(rules, later...)
	perZone := zoneCounts(v.ms)
	slices.SortStableFunc(ms, func(a, b Machine) int {
		for _, after := range rules {
			if c := compareBool(after(a), after(b)); c != 0 {
				return c
			}
		}
		return cmp.Or(
			cmp.Compare(perZone[b.Zone], perZone[a.Zone]),
			age*cmp.Or(CompareCreated(a.Created, b.Created), cmp.Compare(a.ID, b.ID)),
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

// spare is how many machines beyond its size the group has, not counting those whose removal has started.
func (v *groupView) spare(started func(Machine) bool) int {
	n := len(v.ms) - v.g.Size
	for _, m := range v.ms {
		if started(m) {
			n--
		}
	}
	return n
}

// settleUnjoined deals with the machines that have not joined. A roll waits for the first of them. A shrink deletes
// the one that the group would lose first when it has machines to spare and Nomad does not list it, though it may
// still join; a machine that Nomad lists is waited for until it is labelled.
func (v *groupView) settleUnjoined(started func(Machine) bool) (Step, bool, error) {
	if v.mode == Shrink && v.spare(started) > 0 {
		unjoined := slices.DeleteFunc(slices.Clone(v.ms), func(m Machine) bool { return m.Joined })
		v.victimOrder(unjoined, started)
		if len(unjoined) > 0 && !knownToNomad(v.s, unjoined[0]) {
			return Step{Action: Delete, Group: v.g.Name, Machine: unjoined[0]}, true, nil
		}
	}
	return waitJoined(v.g, v.ms)
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

// knownToNomad reports whether Nomad lists a node or a server for the machine: at its private address, or, while the
// cloud reports none, under its name.
func knownToNomad(s State, m Machine) bool {
	if m.PrivateIP.IsValid() {
		return slices.ContainsFunc(s.Nomad.Nodes, func(n Node) bool { return at(m, n) }) ||
			slices.ContainsFunc(s.Nomad.Servers, func(srv Server) bool { return srv.Address.Addr() == m.PrivateIP })
	}
	return slices.ContainsFunc(s.Nomad.Nodes, func(n Node) bool { return n.Name == m.Name }) ||
		slices.ContainsFunc(s.Nomad.Servers, func(srv Server) bool { return nodeOfServer(srv.Name) == m.Name })
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
