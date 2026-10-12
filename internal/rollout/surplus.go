package rollout

import (
	"slices"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Surplus returns the machines that a shrink removes from the group g of s, in the order in which the group loses
// them, and none when the group has no more machines than its size. It picks one machine at a time: each time the
// first of the machines that are left, by the order below, with the zones counted over the machines that are left.
// A client group loses first the machines whose removal has started: those whose drain completed, then those that
// drain, then those whose node is ineligible. Then come the machines that have not joined, the outdated ones, those
// whose node is not available, those in the zone with the most machines, and the newest. A server or combined group
// loses first the machines whose removal has started, then those that have not joined, the outdated ones, those that
// autopilot does not count healthy, those that do not lead, those in the zone with the most machines, and the newest.
// A client that joined and for which the cloud reports no private address is never among them, so the list can be
// shorter than the surplus. Surplus refuses nothing: Next does.
func Surplus(s State, g Group) []Machine {
	switch g.Role {
	case v1alpha1.RoleClient:
		return newClientGroup(s, Shrink, g).lose
	case v1alpha1.RoleServer, v1alpha1.RoleCombined:
		return newServerGroup(s, Shrink, g).lose
	default:
		return nil
	}
}

// surplusBy returns the machines that the group loses: one at a time, the first of the machines that are left by the
// rules of sortVictims, with the newest first, until the group is at its size. A machine for which skip says true is
// never taken; skip may be nil.
func (v *groupView) surplusBy(skip func(Machine) bool, rules ...func(Machine) bool) []Machine {
	left := slices.Clone(v.ms)
	var lost []Machine
	for len(left) > v.g.Size {
		candidates := slices.DeleteFunc(slices.Clone(left), func(m Machine) bool { return skip != nil && skip(m) })
		if len(candidates) == 0 {
			break
		}
		sortVictims(candidates, left, true, rules...)
		first := candidates[0]
		lost = append(lost, first)
		left = slices.DeleteFunc(left, func(m Machine) bool { return m.ID == first.ID })
	}
	return lost
}
