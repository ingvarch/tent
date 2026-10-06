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

// NodeAction is what an update does to one node. A plan holds NodeCreate, NodeWait and NodeDelete. NodeScrub is a step
// of the progress only and never a change of a plan.
type NodeAction int

// Node actions.
const (
	// NodeCreate creates a missing node.
	NodeCreate NodeAction = iota + 1
	// NodeWait waits for a node that an earlier run left unfinished: until the cloud reports it ready when the wait
	// repeats its create, and until it has joined and its user data is scrubbed.
	NodeWait
	// NodeDelete deletes a node.
	NodeDelete
	// NodeScrub replaces the user data of a node that has joined its cluster and labels its machine.
	NodeScrub
)

var nodeActionNames = [...]string{NodeCreate: "create", NodeWait: "wait", NodeDelete: "delete", NodeScrub: "scrub"}

// String returns the action's name in lower case, such as create.
func (a NodeAction) String() string {
	if a < NodeCreate || int(a) >= len(nodeActionNames) {
		return fmt.Sprintf("NodeAction(%d)", int(a))
	}
	return nodeActionNames[a]
}

// MarshalText returns the action's name, as String does.
func (a NodeAction) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// NodeChange is one change to the nodes of a cluster. A create and a wait hold what a create request needs, with the
// hash of the group's node configuration, except a wait without an operation id, which calls no cloud and holds no
// hash; a wait and a delete hold the ID of the machine, a wait its operation id, when it repeats a create, and a
// delete the reason.
type NodeChange struct {
	Action      NodeAction    `json:"action"`
	Name        string        `json:"name"`
	Group       string        `json:"group,omitempty"`
	Role        v1alpha1.Role `json:"role,omitempty"`
	Zone        string        `json:"zone,omitempty"`
	MachineType string        `json:"machineType,omitempty"`
	Image       string        `json:"image,omitempty"`
	SpecHash    string        `json:"specHash,omitempty"`
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
//   - Of machines with one name, one that has joined stays before one that has not, and among equals the oldest
//     stays; the others are deleted as duplicates.
//   - A group with more machines than its size loses the newest ones.
//   - A group with fewer gets new machines named <cluster>-<group>-<index> with the lowest indexes whose names no
//     listed machine has, each in the group's zone with the fewest machines, the zone listed first on a tie.
//   - A machine in a zone that its group no longer lists counts toward the group's size and stays, unless it is among
//     the newest of a group that is too big. New machines go only into the listed zones.
//   - A machine that stays and has not joined, of any role, is waited for, until its node joins and its user data is
//     scrubbed. When the cloud reports it not ready and its operation id is valid, the wait repeats the create call
//     that made it, with its id; otherwise the wait has no operation id and calls no cloud. A machine that joined is
//     never waited for.
//
// The waits and creates of server and combined nodes come first: the waits by name, then the creates by group, then
// index. A server's seed holds the private addresses of the servers that exist, and a server that is not ready has
// none until its wait returns, so the waits run before the creates. The waits and creates of client nodes follow in
// the same order; the deletes come last, by name, then ID.
//
// It also returns the machines of the server and combined groups that stay, by name: the servers the cluster has.
func planNodes(m *model.Cluster, instances []cloud.Instance) (changes []NodeChange, servers []cloud.Instance) {
	owned := slices.DeleteFunc(slices.Clone(instances), func(in cloud.Instance) bool { return in.Cluster != m.Name })
	slices.SortStableFunc(owned, compareAge)
	taken := make(map[string]bool, len(owned)) // the names of the listed machines and of the new ones
	stays := keepers(m, owned)
	members := make(map[string][]cloud.Instance, len(m.Groups))
	var creates, waits, deletes []NodeChange
	for _, in := range owned {
		taken[in.Name] = true
		switch {
		case !inSpec(m, in):
			deletes = append(deletes, deleteNode(in, reasonNotInSpec))
		case stays[in.Name].ID != in.ID:
			deletes = append(deletes, deleteNode(in, reasonDuplicate))
		default:
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
			if w, ok := waitFor(g, in); ok {
				waits = append(waits, w)
			}
		}
		if g.Role.RunsServer() {
			servers = append(servers, nodes...)
		}
		creates = append(creates, createNodes(m.Name, g, nodes, taken)...)
	}
	slices.SortStableFunc(creates, func(a, b NodeChange) int {
		return cmp.Or(cmp.Compare(clientRank(a), clientRank(b)), strings.Compare(a.Group, b.Group))
	})
	slices.SortFunc(waits, func(a, b NodeChange) int {
		return cmp.Or(cmp.Compare(clientRank(a), clientRank(b)), compareNameID(a, b))
	})
	slices.SortFunc(deletes, compareNameID)
	slices.SortFunc(servers, func(a, b cloud.Instance) int { return strings.Compare(a.Name, b.Name) })
	// Both are sorted by rank, so the server and combined changes are a prefix of each.
	waitsOfServers, createsOfServers := serverCount(waits), serverCount(creates)
	return slices.Concat(
		waits[:waitsOfServers], creates[:createsOfServers], waits[waitsOfServers:], creates[createsOfServers:], deletes,
	), servers
}

// inSpec reports whether the group of the machine in is one of m's.
func inSpec(m *model.Cluster, in cloud.Instance) bool {
	return slices.ContainsFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == in.Group })
}

// keepers returns the machine of each name that stays, of the machines owned, which are sorted oldest first: of those
// whose group is one of m's, a joined one stays before one that has not joined, and then the oldest.
func keepers(m *model.Cluster, owned []cloud.Instance) map[string]cloud.Instance {
	stays := make(map[string]cloud.Instance, len(owned))
	for _, in := range owned {
		if !inSpec(m, in) {
			continue
		}
		if cur, ok := stays[in.Name]; !ok || in.Joined && !cur.Joined {
			stays[in.Name] = in
		}
	}
	return stays
}

// serverCount returns how many of the changes, sorted by clientRank, are those of server and combined nodes.
func serverCount(changes []NodeChange) int {
	i := slices.IndexFunc(changes, func(c NodeChange) bool { return !isServerChange(c) })
	if i < 0 {
		return len(changes)
	}
	return i
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

// waitFor returns the wait for the machine in of group g, and whether there is one. It keeps the machine's name,
// group, role and zone, which a create request with its operation id must repeat, and takes the machine type and
// image from the group. Every machine is waited for until it has joined, whatever its role; one that joined never is.
// The wait keeps the operation id only for a machine that the cloud reports not ready.
func waitFor(g model.NodeGroup, in cloud.Instance) (NodeChange, bool) {
	if in.Joined {
		return NodeChange{}, false
	}
	repeats := !in.Ready && cloud.ValidOpID(in.Op)
	w := NodeChange{
		Action: NodeWait, Name: in.Name, Group: in.Group, Role: in.Role, Zone: in.Zone,
		MachineType: g.MachineType, Image: g.Image, ID: in.ID,
	}
	if repeats {
		w.Op = in.Op
	}
	return w, true
}

// instanceByID returns the machine of instances with the ID id, and whether there is one. An empty ID names none.
func instanceByID(instances []cloud.Instance, id string) (cloud.Instance, bool) {
	i := slices.IndexFunc(instances, func(in cloud.Instance) bool { return id != "" && in.ID == id })
	if i < 0 {
		return cloud.Instance{}, false
	}
	return instances[i], true
}

// deleteNode returns the delete of the machine in for reason.
func deleteNode(in cloud.Instance, reason string) NodeChange {
	return NodeChange{Action: NodeDelete, Name: in.Name, ID: in.ID, Reason: reason}
}

// isServerChange reports whether the change belongs to the servers' phase: any change that is not a client's. A
// machine with an empty role label counts with the servers.
func isServerChange(c NodeChange) bool { return c.Role != v1alpha1.RoleClient }

// clientRank orders creates and waits: server and combined nodes before clients.
func clientRank(c NodeChange) int {
	if isServerChange(c) {
		return 0
	}
	return 1
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
