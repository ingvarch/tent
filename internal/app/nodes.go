package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
)

// NodeAction is what an update does to one node.
type NodeAction int

// Node actions.
const (
	// NodeCreate creates a missing node.
	NodeCreate NodeAction = iota + 1
	// NodeWait waits until a node that an earlier, interrupted run created is ready.
	NodeWait
	// NodeDelete deletes a node.
	NodeDelete
)

var nodeActionNames = [...]string{NodeCreate: "create", NodeWait: "wait", NodeDelete: "delete"}

// String returns the action's name in lower case, such as create.
func (a NodeAction) String() string {
	if a < NodeCreate || int(a) >= len(nodeActionNames) {
		return fmt.Sprintf("NodeAction(%d)", int(a))
	}
	return nodeActionNames[a]
}

// MarshalText returns the action's name, as String does.
func (a NodeAction) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// NodeChange is one change to the nodes of a cluster. A create and a wait hold what a create request needs; a wait and
// a delete hold the ID of the machine, a wait its operation id and a delete the reason.
type NodeChange struct {
	Action      NodeAction    `json:"action"`
	Name        string        `json:"name"`
	Group       string        `json:"group,omitempty"`
	Role        v1alpha1.Role `json:"role,omitempty"`
	Zone        string        `json:"zone,omitempty"`
	MachineType string        `json:"machineType,omitempty"`
	Image       string        `json:"image,omitempty"`
	ID          string        `json:"id,omitempty"`
	Op          string        `json:"op,omitempty"`
	Reason      string        `json:"reason,omitempty"`
}

// Reasons of node deletes.
const (
	reasonDuplicate = "duplicate"
	reasonSurplus   = "surplus"
	reasonNotInSpec = "not in the spec"
)

// planNodes returns the changes that bring the cluster's machines, as the cloud lists them, to the node groups of m.
// Only the machines with the cluster's label count; the others are left alone.
//
//   - A machine whose group label names no group of m, or is empty, is deleted.
//   - Of machines with one name, the oldest stays and the others are deleted as duplicates.
//   - A group with more machines than its size loses the newest ones.
//   - A group with fewer gets new machines named <cluster>-<group>-<index> with the lowest indexes whose names no
//     listed machine has, each in the group's zone with the fewest machines, the zone listed first on a tie.
//   - A machine in a zone that its group no longer lists counts toward the group's size and stays, unless it is among
//     the newest of a group that is too big. New machines go only into the listed zones.
//   - A machine that stays and is not ready yet is waited for when its operation id is valid: the wait repeats the
//     create call that made it, with its id. One with an empty or malformed id still counts, but nothing waits for it.
//
// The creates come first, those of server and combined groups before those of client groups, each by group, then
// index; then the waits by name; then the deletes by name, then ID.
func planNodes(m *model.Cluster, instances []cloud.Instance) []NodeChange {
	owned := slices.DeleteFunc(slices.Clone(instances), func(in cloud.Instance) bool { return in.Cluster != m.Name })
	slices.SortStableFunc(owned, compareAge)
	taken := make(map[string]bool, len(owned)) // the names of the listed machines and of the new ones
	kept := make(map[string]bool, len(owned))  // the names of the machines that stay
	members := make(map[string][]cloud.Instance, len(m.Groups))
	var creates, waits, deletes []NodeChange
	for _, in := range owned {
		taken[in.Name] = true
		switch {
		case !slices.ContainsFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == in.Group }):
			deletes = append(deletes, deleteNode(in, reasonNotInSpec))
		case kept[in.Name]:
			deletes = append(deletes, deleteNode(in, reasonDuplicate))
		default:
			kept[in.Name] = true
			members[in.Group] = append(members[in.Group], in)
		}
	}
	for _, g := range m.Groups {
		nodes := members[g.Name] // oldest first
		if size := max(g.Size, 0); len(nodes) > size {
			for _, in := range nodes[size:] {
				deletes = append(deletes, deleteNode(in, reasonSurplus))
			}
			nodes = nodes[:size]
		}
		for _, in := range nodes {
			if !in.Ready && cloud.ValidOpID(in.Op) {
				waits = append(waits, waitFor(g, in))
			}
		}
		creates = append(creates, createNodes(m.Name, g, nodes, taken)...)
	}
	slices.SortStableFunc(creates, func(a, b NodeChange) int {
		return cmp.Or(cmp.Compare(clientRank(a.Role), clientRank(b.Role)), strings.Compare(a.Group, b.Group))
	})
	slices.SortFunc(waits, compareNameID)
	slices.SortFunc(deletes, compareNameID)
	return slices.Concat(creates, waits, deletes)
}

// createNodes returns the creates that bring group g from its nodes to its size, and marks their names taken.
func createNodes(cluster string, g model.NodeGroup, nodes []cloud.Instance, taken map[string]bool) []NodeChange {
	perZone := make(map[string]int, len(g.Zones))
	for _, in := range nodes {
		perZone[in.Zone]++
	}
	var creates []NodeChange
	index := 0
	for range g.Size - len(nodes) {
		for taken[nodeName(cluster, g.Name, index)] {
			index++
		}
		name := nodeName(cluster, g.Name, index)
		taken[name] = true
		zone := leastUsedZone(g.Zones, perZone)
		perZone[zone]++
		creates = append(creates, NodeChange{
			Action: NodeCreate, Name: name, Group: g.Name, Role: g.Role, Zone: zone,
			MachineType: g.MachineType, Image: g.Image,
		})
	}
	return creates
}

// nodeName returns the machine name of the node index of a group.
func nodeName(cluster, group string, index int) string {
	return cluster + "-" + group + "-" + strconv.Itoa(index)
}

// leastUsedZone returns the zone with the fewest nodes, the one listed first on a tie; "" when there are no zones.
func leastUsedZone(zones []string, perZone map[string]int) string {
	var best string
	for i, z := range zones {
		if i == 0 || perZone[z] < perZone[best] {
			best = z
		}
	}
	return best
}

// waitFor returns the wait for the machine in of group g. It keeps the machine's name, group, role and zone, which a
// create request with its operation id must repeat, and takes the machine type and image from the group.
func waitFor(g model.NodeGroup, in cloud.Instance) NodeChange {
	return NodeChange{
		Action: NodeWait, Name: in.Name, Group: in.Group, Role: in.Role, Zone: in.Zone,
		MachineType: g.MachineType, Image: g.Image, ID: in.ID, Op: in.Op,
	}
}

// deleteNode returns the delete of the machine in for reason.
func deleteNode(in cloud.Instance, reason string) NodeChange {
	return NodeChange{Action: NodeDelete, Name: in.Name, ID: in.ID, Reason: reason}
}

// clientRank orders creates: server and combined nodes before clients.
func clientRank(r v1alpha1.Role) int {
	if r == v1alpha1.RoleClient {
		return 1
	}
	return 0
}

// compareAge orders machines oldest first: by creation time, a machine without one last, then by ID.
func compareAge(a, b cloud.Instance) int {
	return cmp.Or(compareCreated(a.Created, b.Created), strings.Compare(a.ID, b.ID))
}

// compareCreated orders creation times, the earliest first; the zero time, an unknown one, counts as the latest.
func compareCreated(a, b time.Time) int {
	switch {
	case a.IsZero() == b.IsZero():
		return a.Compare(b)
	case a.IsZero():
		return 1
	default:
		return -1
	}
}

// compareNameID orders changes by name, then ID.
func compareNameID(a, b NodeChange) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
}
