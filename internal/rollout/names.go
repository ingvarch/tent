package rollout

import (
	"strconv"
	"strings"
	"time"
)

// NodeName returns the machine name of the node index of a group.
func NodeName(cluster, group string, index int) string {
	return namePrefix(cluster, group) + strconv.Itoa(index)
}

// namePrefix is what the machine names of a group's nodes start with.
func namePrefix(cluster, group string) string { return cluster + "-" + group + "-" }

// nodeIndex returns the index in a node name of the group: the digits that follow the group's prefix. It reports false
// for a name of another form, such as one with a suffix, a sign or an index above 2147483647.
func nodeIndex(cluster, group, name string) (int, bool) {
	if !isNodeOf(cluster, group, name) {
		return 0, false
	}
	index, err := strconv.ParseInt(strings.TrimPrefix(name, namePrefix(cluster, group)), 10, 32)
	return int(index), err == nil
}

// FreeName returns the group's node name of the lowest index that taken does not hold as true. The caller marks the
// name it uses.
func FreeName(cluster, group string, taken map[string]bool) string {
	index := 0
	for taken[NodeName(cluster, group, index)] {
		index++
	}
	return NodeName(cluster, group, index)
}

// LeastUsedZone returns the zone with the fewest nodes, the one listed first on a tie; "" when there are no zones.
func LeastUsedZone(zones []string, perZone map[string]int) string {
	var best string
	for i, z := range zones {
		if i == 0 || perZone[z] < perZone[best] {
			best = z
		}
	}
	return best
}

// CompareCreated orders creation times, the earliest first; the zero time, an unknown one, counts as the latest.
func CompareCreated(a, b time.Time) int {
	switch {
	case a.IsZero() == b.IsZero():
		return a.Compare(b)
	case a.IsZero():
		return 1
	default:
		return -1
	}
}

// NodeOfServer returns the name of the node from the name of its server, which is <node name>.<region>.
func NodeOfServer(name string) string {
	node, _, _ := strings.Cut(name, ".")
	return node
}
