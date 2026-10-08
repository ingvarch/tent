package rollout

import (
	"strconv"
	"strings"
	"time"
)

// NodeName returns the machine name of the node index of a group.
func NodeName(cluster, group string, index int) string {
	return cluster + "-" + group + "-" + strconv.Itoa(index)
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

// nodeOfServer returns the node name in the name of a server, which is <node name>.<region>.
func nodeOfServer(name string) string {
	node, _, _ := strings.Cut(name, ".")
	return node
}
