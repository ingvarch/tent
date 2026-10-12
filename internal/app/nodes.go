package app

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/rollout"
)

// NodeAction is what an update does to one node. A plan holds NodeCreate, NodeWait and NodeDelete. NodeScrub and
// NodeStop are steps of the progress only and never a change of a plan.
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
	// NodeStop powers off the machine of a server that a rolling update replaces or a scale-down removes. The server's
	// peer leaves the Raft configuration after the stop, except when the server has no peer there.
	NodeStop
)

var nodeActionNames = [...]string{
	NodeCreate: "create", NodeWait: "wait", NodeDelete: "delete", NodeScrub: "scrub", NodeStop: "stop",
}

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
// hash; a wait, a delete and a stop hold the ID of the machine, a wait its operation id, when it repeats a create, and
// a delete the reason, and Drain and Server when its node joined.
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
	// Drain is the deadline of the drain that comes before the delete of a node that joined and runs a client, as Go
	// writes a duration, such as 1h0m0s; it is empty for every other change.
	Drain string `json:"drain,omitempty"`
	// Server reports that the delete removes a node that joined and runs a server: the server leaves the Raft
	// configuration and the gossip pool before its machine is deleted.
	Server bool `json:"server,omitempty"`
}

// Reasons of node deletes.
const (
	reasonDuplicate     = "duplicate"
	reasonSurplus       = "surplus"
	reasonNotInSpec     = "not in the spec"
	reasonNotRegistered = "not registered"
)

// defaultDrain is how long the drain of a node lasts when no spec says: the API's default drain timeout.
const defaultDrain = time.Hour

// shrinkView is what the node planner knows when it chooses the machines that a group loses: a reading of Nomad, if
// read is set, the spec hash of each node group, the IDs of the machines to replace whatever their hash, and the drain
// timeout of each node group. The zero view has none of them: the planner then takes the joined label at its word, no
// machine is outdated, and a drain lasts defaultDrain.
type shrinkView struct {
	read   bool
	nomad  rollout.Nomad
	hash   func(group string) string
	forced map[string]bool
	drain  func(group string) time.Duration
}

// drainOf returns how long the drain of a node of the group lasts.
func (v shrinkView) drainOf(group string) time.Duration {
	if v.drain == nil {
		return defaultDrain
	}
	return v.drain(group)
}

// surplus returns the machines that group g loses, as rollout.Surplus chooses them: the IDs, each with its place in the
// order in which the group loses them, counted from 0.
func (v shrinkView) surplus(cluster string, g model.NodeGroup, members []cloud.Instance) map[string]int {
	machines := rolloutMachines(members)
	group := rollout.Group{Name: g.Name, Role: g.Role, Size: g.Size}
	if v.hash != nil {
		group.SpecHash = v.hash(g.Name)
	} else {
		for i := range machines {
			machines[i].SpecHash = ""
		}
	}
	s := rollout.State{Cluster: cluster, Machines: machines, Nomad: v.nomad, Forced: v.forced}
	if !v.read {
		s.Nomad = nomadAsLabelled(machines)
	}
	lost := rollout.Surplus(s, group)
	order := make(map[string]int, len(lost))
	for i, m := range lost {
		order[m.ID] = i
	}
	return order
}

// raftPort is the port of a server's Raft address.
const raftPort = 4647

// nomadAsLabelled returns what Nomad would report if every machine were what its labels say: a healthy voter at the
// address of a joined machine that runs a server, and a ready, eligible node for a joined machine that runs a
// client; a machine that has not joined gets neither. They are matched to machines by private address, so a machine
// without one finds none.
func nomadAsLabelled(machines []rollout.Machine) rollout.Nomad {
	var n rollout.Nomad
	for _, m := range machines {
		if !m.Joined {
			continue
		}
		if m.Role.RunsServer() {
			n.Servers = append(n.Servers, rollout.Server{
				ID: m.ID, Name: m.Name, Address: netip.AddrPortFrom(m.PrivateIP, raftPort), Voter: true, Healthy: true,
			})
		}
		if m.Role.RunsClient() {
			n.Nodes = append(n.Nodes, rollout.Node{
				ID: m.ID, Name: m.Name, Address: m.PrivateIP, Status: "ready", Eligible: true,
			})
		}
	}
	return n
}

// planNodes returns the changes that bring the cluster's machines, as the cloud lists them, to the node groups of m.
// Only the machines with the cluster's label count; the others are left alone.
//
//   - A machine whose group label names no group of m, or is empty, is deleted.
//   - Of machines with one name, one that has joined stays before one that has not, then one that is not among the
//     unregistered machines before one that is, and among equals the oldest stays; the others are deleted as
//     duplicates.
//   - A group with more machines than its size loses the machines that rollout.Surplus names for it, in its order: a
//     joined server or combined machine that does not run, then those that have not joined, and so on, with the
//     newest last. view says what Surplus reads of Nomad.
//   - A group with fewer gets new machines named <cluster>-<group>-<index>, each in the group's zone with the fewest
//     machines, the zone listed first on a tie. A client group takes the lowest indexes whose names no listed machine
//     has. A server or combined group takes the indexes from the one above the highest index among the listed
//     machines whose names have the group's pattern, whatever the plan does with them, and not below its entry in
//     next, the lowest index that tent has not used for the group; without an entry that is 0.
//   - A machine in a zone that its group no longer lists counts toward the group's size and stays, unless it is among
//     the machines that a group that is too big loses. New machines go only into the listed zones.
//   - A machine that stays and has not joined, of any role, is waited for, until its node joins and its user data is
//     scrubbed. When the cloud reports it not ready and its operation id is valid, the wait repeats the create call
//     that made it, with its id; otherwise the wait has no operation id and calls no cloud. A machine that joined is
//     never waited for.
//
// A delete of a machine that joined, which the node planner knows by the joined label, comes with what the update does
// before it: Drain for a node that runs a client, within the drain timeout of its group, or defaultDrain when m lacks
// the group, and Server for a node that runs a server. It is a delete of a surplus machine of a group of m, or of a
// joined client of a group that m lacks when no other machine of its name joined and its group label is set. Every
// other delete of a machine that joined has neither; the guard refuses it.
//
// The unregistered machines are those that the caller says never registered, by ID. Only one that would stay under
// its name is deleted with the reason not registered; it does not count toward its group and does not hold its name,
// so that a create takes the name again. A machine of the set that would not stay is deleted as not in the spec or as
// a duplicate, like any other.
//
// The waits and creates of server and combined nodes come first: the waits by name, then the creates by group, then
// index. A server's seed holds the private addresses of the servers that exist, and a server that is not ready has
// none until its wait returns, so the waits run before the creates. The waits of client nodes follow in the same
// order, then the deletes of the unregistered machines by name, then ID, then the creates of client nodes in the same
// order as before; the deletes of machines without Drain and Server come next, by name, then ID. The removals of
// machines that joined come last: those of the server and combined groups by group, in the order in which each
// group loses its machines, then those of the client groups by group, then name.
//
// It also returns the machines of the server and combined groups that stay, by name: the servers the cluster has.
func planNodes(m *model.Cluster, instances []cloud.Instance, unregistered map[string]bool, next map[string]int,
	view shrinkView,
) (changes []NodeChange, servers []cloud.Instance) {
	owned := slices.DeleteFunc(slices.Clone(instances), func(in cloud.Instance) bool { return in.Cluster != m.Name })
	slices.SortStableFunc(owned, compareAge)
	stays := keepers(m, owned, unregistered)
	// taken holds the names that a create cannot take: those of the listed machines, except one whose machine never
	// registered, and of the new ones.
	taken := make(map[string]bool, len(owned))
	for _, in := range owned {
		taken[in.Name] = !unregistered[stays[in.Name].ID]
	}
	members := make(map[string][]cloud.Instance, len(m.Groups))
	var creates, waits, deletes, unregisteredDeletes []NodeChange
	var removals []removal
	var outside []cloud.Instance
	for _, in := range owned {
		switch {
		case unregistered[in.ID] && stays[in.Name].ID == in.ID:
			unregisteredDeletes = append(unregisteredDeletes, deleteNode(in, reasonNotRegistered))
		case !inSpec(m, in):
			outside = append(outside, in)
		case stays[in.Name].ID != in.ID:
			deletes = append(deletes, deleteNode(in, reasonDuplicate))
		default:
			members[in.Group] = append(members[in.Group], in)
		}
	}
	removedClients := removedClientNodes(outside)
	for _, in := range outside {
		if removedClients[in.ID] {
			removals = append(removals, removal{
				group: in.Group, role: v1alpha1.RoleClient,
				change: removeNode(in, reasonNotInSpec, v1alpha1.RoleClient, defaultDrain),
			})
		} else {
			deletes = append(deletes, deleteNode(in, reasonNotInSpec))
		}
	}
	for _, g := range m.Groups {
		nodes := members[g.Name] // oldest first
		if lost := view.surplus(m.Name, g, nodes); len(lost) > 0 {
			var stay []cloud.Instance
			for _, in := range nodes {
				place, isLost := lost[in.ID]
				switch {
				case !isLost:
					stay = append(stay, in)
				case in.Joined:
					r := removal{group: g.Name, role: g.Role, change: removeNode(in, reasonSurplus, g.Role, view.drainOf(g.Name))}
					if g.Role.RunsServer() {
						r.place = place
					}
					removals = append(removals, r)
				default:
					deletes = append(deletes, deleteNode(in, reasonSurplus))
				}
			}
			nodes = stay
		}
		for _, in := range nodes {
			if w, ok := waitFor(g, in); ok {
				waits = append(waits, w)
			}
		}
		if g.Role.RunsServer() {
			servers = append(servers, nodes...)
		}
		creates = append(creates, createNodes(m.Name, g, nodes, taken, next[g.Name], owned)...)
	}
	slices.SortStableFunc(creates, func(a, b NodeChange) int {
		return cmp.Or(cmp.Compare(clientRank(a), clientRank(b)), strings.Compare(a.Group, b.Group))
	})
	slices.SortFunc(waits, func(a, b NodeChange) int {
		return cmp.Or(cmp.Compare(clientRank(a), clientRank(b)), compareNameID(a, b))
	})
	slices.SortFunc(unregisteredDeletes, compareNameID)
	slices.SortFunc(deletes, compareNameID)
	slices.SortFunc(removals, compareRemovals)
	slices.SortFunc(servers, compareName)
	// Both are sorted by rank, so the server and combined changes are a prefix of each.
	waitsOfServers, createsOfServers := serverCount(waits), serverCount(creates)
	return slices.Concat(
		waits[:waitsOfServers], creates[:createsOfServers], waits[waitsOfServers:], unregisteredDeletes,
		creates[createsOfServers:], deletes, removalChanges(removals),
	), servers
}

// removal is the delete of a machine that joined, with the group and the role that decide its place in a plan. A
// server or combined group also gives the place in which it loses the machine, counted from 0.
type removal struct {
	change NodeChange
	group  string
	role   v1alpha1.Role
	place  int
}

// removedClientNodes returns the IDs of the machines among outside, which are those of the groups that the specs lack,
// that a plan removes with a drain: a client that joined, with a group label, and that no other machine of its name
// joined.
func removedClientNodes(outside []cloud.Instance) map[string]bool {
	joinedOf := make(map[string]int, len(outside))
	for _, in := range outside {
		if in.Joined {
			joinedOf[in.Name]++
		}
	}
	ids := make(map[string]bool, len(outside))
	for _, in := range outside {
		if in.Joined && in.Role == v1alpha1.RoleClient && in.Group != "" && joinedOf[in.Name] == 1 {
			ids[in.ID] = true
		}
	}
	return ids
}

// removeNode returns the delete of the machine in, which joined, for reason: with a drain of the given length when the
// role runs a client, and with the leaving of the server when it runs a server.
func removeNode(in cloud.Instance, reason string, role v1alpha1.Role, drain time.Duration) NodeChange {
	c := deleteNode(in, reason)
	if role.RunsClient() {
		c.Drain = drain.String()
	}
	c.Server = role.RunsServer()
	return c
}

// compareRemovals orders removals as a run carries them out: the server and combined groups before the client groups,
// each by group name; the machines of a server or combined group in the order in which it loses them, those of a
// client group by name. The names of the removals differ.
func compareRemovals(a, b removal) int {
	return cmp.Or(
		cmp.Compare(removalRank(a.role), removalRank(b.role)),
		strings.Compare(a.group, b.group),
		cmp.Compare(a.place, b.place),
		strings.Compare(a.change.Name, b.change.Name),
	)
}

// removalRank is 0 for a role that runs a server and 1 for the other.
func removalRank(role v1alpha1.Role) int {
	if role.RunsServer() {
		return 0
	}
	return 1
}

// removalChanges returns the changes of the removals.
func removalChanges(removals []removal) []NodeChange {
	changes := make([]NodeChange, len(removals))
	for i, r := range removals {
		changes[i] = r.change
	}
	return changes
}

// inSpec reports whether the group of the machine in is one of m's.
func inSpec(m *model.Cluster, in cloud.Instance) bool {
	return slices.ContainsFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == in.Group })
}

// keepers returns the machine of each name that stays, of the machines owned, which are sorted oldest first: of those
// whose group is one of m's, a joined one stays before one that has not joined, then one that is not among the
// unregistered machines before one that is, and then the oldest.
func keepers(m *model.Cluster, owned []cloud.Instance, unregistered map[string]bool) map[string]cloud.Instance {
	stays := make(map[string]cloud.Instance, len(owned))
	for _, in := range owned {
		if !inSpec(m, in) {
			continue
		}
		if cur, ok := stays[in.Name]; !ok || keepsBefore(in, cur, unregistered) {
			stays[in.Name] = in
		}
	}
	return stays
}

// keepsBefore reports whether the machine in stays before cur of the same name, when in is not older than cur.
func keepsBefore(in, cur cloud.Instance, unregistered map[string]bool) bool {
	if in.Joined != cur.Joined {
		return in.Joined
	}
	return !unregistered[in.ID] && unregistered[cur.ID]
}

// serverCount returns how many of the changes, sorted by clientRank, are those of server and combined nodes.
func serverCount(changes []NodeChange) int {
	i := slices.IndexFunc(changes, func(c NodeChange) bool { return !isServerChange(c) })
	if i < 0 {
		return len(changes)
	}
	return i
}

// createNodes returns the creates that bring group g from its nodes to its size, and marks their names taken. A group
// whose role runs a server names its creates one index after the other, from the lowest that is above the index of
// every machine of the group that is listed, and not below next. Any other group takes the lowest free name.
func createNodes(cluster string, g model.NodeGroup, nodes []cloud.Instance, taken map[string]bool, next int,
	listed []cloud.Instance,
) []NodeChange {
	perZone := make(map[string]int, len(g.Zones))
	for _, in := range nodes {
		perZone[in.Zone]++
	}
	var index int
	if g.Role.RunsServer() {
		index = rollout.NextIndex(listedState(cluster, listed), rollout.Group{Name: g.Name, NextIndex: next})
	}
	var creates []NodeChange
	for range g.Size - len(nodes) {
		var name string
		if g.Role.RunsServer() {
			name = rollout.NodeName(cluster, g.Name, index)
			index++
		} else {
			name = rollout.FreeName(cluster, g.Name, taken)
		}
		taken[name] = true
		zone := rollout.LeastUsedZone(g.Zones, perZone)
		perZone[zone]++
		creates = append(creates, NodeChange{
			Action: NodeCreate, Name: name, Group: g.Name, Role: g.Role, Zone: zone,
			MachineType: g.MachineType, Image: g.Image,
		})
	}
	return creates
}

// listedState returns the state of cluster that holds the names of the listed machines alone.
func listedState(cluster string, listed []cloud.Instance) rollout.State {
	s := rollout.State{Cluster: cluster, Machines: make([]rollout.Machine, len(listed))}
	for i, in := range listed {
		s.Machines[i] = rollout.Machine{Name: in.Name}
	}
	return s
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
	return cmp.Or(rollout.CompareCreated(a.Created, b.Created), strings.Compare(a.ID, b.ID))
}

// compareName orders machines by name alone.
func compareName(a, b cloud.Instance) int { return strings.Compare(a.Name, b.Name) }

// compareNameID orders changes by name, then ID.
func compareNameID(a, b NodeChange) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
}
