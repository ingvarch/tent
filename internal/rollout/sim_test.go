package rollout_test

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"golang.org/x/mod/semver"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// The simulator is a model of a cloud and of Nomad with a clock of its own: it starts at 2026-10-07 12:00:00 UTC and
// moves 10 s at every tick. Machine IDs are m-1, m-2, ..., private addresses start at 10.64.0.3, node IDs are n-1, ....
//
//   - A created machine is listed not ready, is ready after 1 tick, and after 2 a client registers ready and eligible
//     with two allocations and is labelled. A combined machine registers its node then too, and its server is modelled
//     as the servers are: it is labelled when it votes.
//   - A new machine gets an address that no machine has had, unless the scenario sets reuseAddresses: then the lowest
//     one that no machine holds.
//   - A drain makes the node ineligible and draining; after 2 ticks it completes with the machine ID of its meta in
//     DrainedFor, and the allocations go to the first available node.
//   - The node of a deleted machine stays ready for 2 ticks (a scenario may set more), then reads down.
//   - A purge removes a node.
//
// The servers are modelled in sim_servers_test.go.

const (
	tickLength      = 10 * time.Second
	refreshInterval = time.Minute
	maxRunSteps     = 1000
	allocsOfNode    = 2
	oldVersion      = "2.0.6"
	curVersion      = "2.0.7"
	// defaultDownAfter is how many ticks the node of a deleted machine stays ready unless a scenario sets it.
	defaultDownAfter = 2
)

type simMachine struct {
	rollout.Machine
	age     int
	version string // the Nomad version its agent runs
	stopped bool   // it was stopped and stays so
}

type simNode struct {
	rollout.Node
	owner      string // the ID of the machine it runs on
	allocs     int
	deadTicks  int
	drainTicks int
	drainMeta  string // the machine ID given with the drain
}

type world struct {
	now         time.Time
	cluster     string
	version     string
	groups      []rollout.Group
	machines    []simMachine
	nodes       []simNode
	servers     []simServer // the Raft configuration
	members     []simMember // the gossip pool
	removed     []simRemoved
	nextMachine int
	nextNode    int
	nextRaft    int
	unplaced    int
	lastChange  time.Time // when a server last joined the Raft configuration
	noCleanup   bool      // autopilot does not remove the peers of failed servers
	// keepsBudget has the groups that start with all their nodes available: the budget invariant holds for them.
	keepsBudget map[string]bool
	// startSize is how many machines each group had when arm was called.
	startSize map[string]int
	// reuseAddresses gives a new machine the lowest private address that no machine holds, as vultrfake does; without
	// it an address is never given twice.
	reuseAddresses bool
	// downAfter is how many ticks the node of a deleted machine stays ready; 0 means defaultDownAfter.
	downAfter int
}

func newWorld(version string) *world {
	return &world{
		now:         epoch,
		cluster:     "prod",
		version:     version,
		keepsBudget: map[string]bool{},
		startSize:   map[string]int{},
	}
}

// clone returns a copy that shares nothing with w that a run changes.
func (w *world) clone() *world {
	c := *w
	c.groups = slices.Clone(w.groups)
	c.machines = slices.Clone(w.machines)
	c.nodes = slices.Clone(w.nodes)
	c.servers = slices.Clone(w.servers)
	c.members = slices.Clone(w.members)
	c.removed = slices.Clone(w.removed)
	c.keepsBudget = make(map[string]bool, len(w.keepsBudget))
	for k, v := range w.keepsBudget {
		c.keepsBudget[k] = v
	}
	c.startSize = maps.Clone(w.startSize)
	return &c
}

func (w *world) newMachineID() string {
	w.nextMachine++
	return fmt.Sprintf("m-%d", w.nextMachine)
}

// newAddress is the address of the machine with the given number: 10.64.0.3 for the first.
func newAddress(number int) netip.Addr { return ip(2 + number) }

// addServer adds a running server machine to the group servers, with its peer and member; the first one leads.
func (w *world) addServer(hash, version string) { w.addControl("servers", hash, version) }

// addCombined adds a running combined machine to the group control: a server as addServer adds it, and a client with
// a registered node.
func (w *world) addCombined(hash, version string) { w.addControl("control", hash, version) }

// addControl adds a running machine that runs a server to a group of server or combined role, with its peer and
// member, and with a node when it runs a client too. A group that the world does not know has the role server. The
// first server leads.
func (w *world) addControl(group, hash, version string) {
	id := w.newMachineID()
	index := w.countOf(group)
	zone, role := "ams", v1alpha1.RoleServer
	if g, ok := w.group(group); ok {
		zone, role = g.Zones[index%len(g.Zones)], g.Role
	}
	m := simMachine{
		Machine: rollout.Machine{
			ID: id, Name: rollout.NodeName(w.cluster, group, index), Group: group, Role: role,
			Zone: zone, SpecHash: hash, PrivateIP: newAddress(w.nextMachine), Ready: true, Joined: true,
			Created: w.now.Add(-time.Duration(1000-w.nextMachine) * time.Hour),
		},
		age: 100, version: version,
	}
	w.machines = append(w.machines, m)
	w.joinRaft(id, w.newRaftID(), len(w.servers) == 0, true, w.now.Add(-time.Hour))
	if role.RunsClient() {
		w.register(m)
	}
}

func (w *world) addGroup(g rollout.Group) { w.groups = append(w.groups, g) }

func (w *world) group(name string) (rollout.Group, bool) {
	for _, g := range w.groups {
		if g.Name == name {
			return g, true
		}
	}
	return rollout.Group{}, false
}

// addClients adds n running machines to a group, each with a registered node, all of one hash and Nomad version.
func (w *world) addClients(group string, n int, hash, version string) {
	g, _ := w.group(group)
	for range n {
		index := w.countOf(group)
		id := w.newMachineID()
		m := simMachine{
			Machine: rollout.Machine{
				ID: id, Name: rollout.NodeName(w.cluster, group, index), Group: group, Role: g.Role,
				Zone: g.Zones[index%len(g.Zones)], SpecHash: hash, PrivateIP: newAddress(w.nextMachine), Ready: true,
				Joined:  true,
				Created: w.now.Add(-time.Duration(1000-w.nextMachine) * time.Hour),
			},
			age: 100, version: version,
		}
		w.machines = append(w.machines, m)
		w.register(m)
	}
}

func (w *world) countOf(group string) int {
	n := 0
	for _, m := range w.machines {
		if m.Group == group {
			n++
		}
	}
	return n
}

// register adds the node of a machine, as its agent does when it starts.
func (w *world) register(m simMachine) {
	w.nextNode++
	w.nodes = append(w.nodes, simNode{
		Node: rollout.Node{
			ID: fmt.Sprintf("n-%d", w.nextNode), Name: m.Name, Address: m.PrivateIP, Status: "ready", Eligible: true,
			Version: m.version,
		},
		owner: m.ID, allocs: allocsOfNode,
	})
}

// duplicate lists a second machine of the name of an existing one, with no node.
func (w *world) duplicate(name string) {
	i := w.machineIndexByName(name)
	twin := w.machines[i]
	twin.ID = w.newMachineID()
	twin.PrivateIP = newAddress(w.nextMachine)
	w.machines = append(w.machines, twin)
}

// makeIneligible marks the node of the machine ineligible, as an operator does.
func (w *world) makeIneligible(name string) {
	w.nodes[w.nodeIndexByOwner(w.machines[w.machineIndexByName(name)].ID)].Eligible = false
}

// staleDrain gives the node of the machine the drain meta of its own machine while it stays eligible, as an operator
// leaves it by making a drained node eligible again.
func (w *world) staleDrain(name string) {
	id := w.machines[w.machineIndexByName(name)].ID
	w.nodes[w.nodeIndexByOwner(id)].DrainedFor = id
}

func (w *world) machineIndexByName(name string) int {
	return slices.IndexFunc(w.machines, func(m simMachine) bool { return m.Name == name })
}

func (w *world) machineIndex(id string) int {
	return slices.IndexFunc(w.machines, func(m simMachine) bool { return m.ID == id })
}

func (w *world) nodeIndex(id string) int {
	return slices.IndexFunc(w.nodes, func(n simNode) bool { return n.ID == id })
}

func (w *world) nodeIndexByOwner(id string) int {
	return slices.IndexFunc(w.nodes, func(n simNode) bool { return n.owner == id })
}

// arm records which groups start with every node available, for the invariant of the budget, and how many machines
// each group starts with, for the invariant of the limit.
func (w *world) arm() *world {
	for _, g := range w.groups {
		w.keepsBudget[g.Name] = w.availableCount(g) >= g.Size
		w.startSize[g.Name] = w.countOf(g.Name)
	}
	return w
}

// limit is the most machines a group may have: its size plus its surge (a server or combined group: plus 1), or the
// number it started with when that is more, as a group that a shrink starts to reduce.
func (w *world) limit(g rollout.Group) int {
	limit := g.Size + 1
	if g.Role == v1alpha1.RoleClient {
		limit = g.Size + g.MaxSurge
	}
	return max(limit, w.startSize[g.Name])
}

// available reports whether a machine's node can take work, by the world's own record.
func (w *world) available(m simMachine) bool {
	i := w.nodeIndexByOwner(m.ID)
	if i < 0 || !m.Joined || !m.Ready {
		return false
	}
	n := w.nodes[i]
	return n.Status == "ready" && n.Eligible && !n.Draining
}

func (w *world) availableCount(g rollout.Group) int {
	count := 0
	for _, m := range w.machines {
		if m.Group == g.Name && w.available(m) {
			count++
		}
	}
	return count
}

// observe returns what the cloud and Nomad report now.
func (w *world) observe() rollout.State {
	s := rollout.State{
		Cluster: w.cluster, Groups: w.groups, Version: w.version, Refresh: refreshInterval, Now: w.now,
	}
	for _, m := range w.machines {
		s.Machines = append(s.Machines, m.Machine)
	}
	s.Nomad = w.observeNomad()
	return s
}

// tick moves the clock and what takes time.
func (w *world) tick() {
	w.now = w.now.Add(tickLength)
	w.promoteServers()
	for i := range w.machines {
		m := &w.machines[i]
		m.age++
		m.Ready = !m.stopped && (m.Ready || m.age >= 1)
		if m.Role.RunsClient() && m.age == 2 {
			w.register(*m)
			// a combined machine is labelled when its server votes
			if m.Role == v1alpha1.RoleClient {
				m.Joined = true
			}
		}
		if m.Role.RunsServer() && m.age == 2 {
			w.joinRaft(m.ID, w.newRaftID(), false, false, w.now)
			w.serverJoined()
		}
	}
	w.tickMembers()
	w.readdServers()
	for i := range w.nodes {
		n := &w.nodes[i]
		if w.machineIndex(n.owner) < 0 {
			if n.deadTicks++; n.deadTicks >= cmp.Or(w.downAfter, defaultDownAfter) {
				n.Status = "down"
			}
		}
		if n.Draining {
			if n.drainTicks++; n.drainTicks >= 2 {
				w.completeDrain(i)
			}
		}
	}
}

// completeDrain ends the drain of a node and moves its allocations to the first available node.
func (w *world) completeDrain(i int) {
	n := &w.nodes[i]
	n.Draining = false
	n.DrainedFor = n.drainMeta
	moved := n.allocs
	n.allocs = 0
	for j := range w.nodes {
		other := &w.nodes[j]
		if j != i && other.Status == "ready" && other.Eligible && !other.Draining {
			other.allocs += moved
			return
		}
	}
	w.unplaced += moved
}

// A violation is a broken invariant.
type violation struct{ msg string }

func (v *violation) Error() string { return v.msg }

func violated(format string, args ...any) error { return &violation{fmt.Sprintf(format, args...)} }

// apply carries out a step at once. A step that breaks an invariant of its own is an error.
func (w *world) apply(step rollout.Step) error {
	switch step.Action {
	case rollout.Create:
		return w.create(step)
	case rollout.MarkIneligible:
		i := w.nodeIndex(step.Node.ID)
		if i < 0 {
			return fmt.Errorf("%s: no such node", step)
		}
		w.nodes[i].Eligible = false
	case rollout.Drain:
		i := w.nodeIndex(step.Node.ID)
		if i < 0 {
			return fmt.Errorf("%s: no such node", step)
		}
		n := &w.nodes[i]
		n.Eligible, n.Draining, n.drainTicks, n.drainMeta = false, true, 0, step.Machine.ID
	case rollout.Delete:
		return w.delete(step)
	case rollout.Purge:
		i := w.nodeIndex(step.Node.ID)
		if i < 0 {
			return fmt.Errorf("%s: no such node", step)
		}
		w.nodes = slices.Delete(w.nodes, i, i+1)
	case rollout.TransferLeadership:
		return w.transfer(step)
	case rollout.Stop:
		return w.stop(step)
	case rollout.RemovePeer:
		return w.removePeer(step)
	case rollout.ForceLeave:
		w.forceLeave(step)
	default:
		return fmt.Errorf("%s: the world does not apply this step", step)
	}
	return nil
}

func (w *world) create(step rollout.Step) error {
	g, ok := w.group(step.Group)
	if !ok {
		return fmt.Errorf("%s: no such group", step)
	}
	if g.Role == v1alpha1.RoleClient {
		for _, srv := range w.servers {
			if v := w.machines[w.machineIndex(srv.machine)].version; semver.Compare("v"+v, "v"+w.version) < 0 {
				return violated("%s: a server runs Nomad %s, older than the %s of the new node", step, v, w.version)
			}
		}
	}
	id := w.newMachineID()
	w.machines = append(w.machines, simMachine{
		Machine: rollout.Machine{
			ID: id, Name: step.Machine.Name, Group: g.Name, Role: g.Role, Zone: step.Machine.Zone, SpecHash: g.SpecHash,
			PrivateIP: w.addressForNew(), Created: w.now,
		},
		version: w.version,
	})
	return nil
}

// addressForNew is the private address of a machine that is created now.
func (w *world) addressForNew() netip.Addr {
	if !w.reuseAddresses {
		return newAddress(w.nextMachine)
	}
	for number := 1; ; number++ {
		if !slices.ContainsFunc(w.machines, func(m simMachine) bool { return m.PrivateIP == newAddress(number) }) {
			return newAddress(number)
		}
	}
}

func (w *world) delete(step rollout.Step) error {
	i := w.machineIndex(step.Machine.ID)
	if i < 0 {
		return fmt.Errorf("%s: no such machine", step)
	}
	id := w.machines[i].ID
	if w.leads(id) {
		return violated("%s: it is the machine of the leader", step)
	}
	if w.serverIndexByMachine(id) >= 0 {
		return violated("%s: its server is still in the Raft configuration", step)
	}
	if j := w.nodeIndexByOwner(id); j >= 0 && w.nodes[j].Status == "ready" && w.nodes[j].allocs > 0 {
		return violated("%s: its node is up and holds %d allocations", step, w.nodes[j].allocs)
	}
	w.machines = slices.Delete(w.machines, i, i+1)
	return nil
}

// check returns the first invariant of the world that does not hold.
func (w *world) check() error {
	if !slices.ContainsFunc(w.servers, func(s simServer) bool { return s.leader && w.up(s.machine) }) {
		return violated("no leader")
	}
	if voters, running := w.voterCounts(); running < voters/2+1 {
		return violated("no quorum: %d of %d voters run", running, voters)
	}
	names := map[string]bool{}
	for _, m := range w.machines {
		if names[m.Name] {
			return violated("two machines are called %s", m.Name)
		}
		names[m.Name] = true
	}
	for _, g := range w.groups {
		if n := w.countOf(g.Name); n > w.limit(g) {
			return violated("group %s has %d machines, more than the %d it may have", g.Name, n, w.limit(g))
		}
		if n := w.availableCount(g); g.Role == v1alpha1.RoleClient && w.keepsBudget[g.Name] &&
			n < g.Size-g.MaxUnavailable {
			return violated("group %s has %d available nodes, fewer than its size %d less %d unavailable", g.Name,
				n, g.Size, g.MaxUnavailable)
		}
	}
	return nil
}

// summary lists the machines, servers and nodes, for comparing the worlds that two runs end in.
func (w *world) summary() []string { return w.lines(true) }

// clientShape is summary without the names of client machines, of their nodes and of nodes whose machine is gone:
// which new client gets which free name depends on when the node of a deleted machine is purged.
func (w *world) clientShape() []string { return w.lines(false) }

// lines lists the machines, servers and nodes, sorted; without clientNames the name of a client machine, of its node
// and of a node whose machine is gone reads "-".
func (w *world) lines(clientNames bool) []string {
	nameOf := func(role v1alpha1.Role, name string) string {
		if !clientNames && role == v1alpha1.RoleClient {
			return "-"
		}
		return name
	}
	var lines []string
	for _, m := range w.machines {
		lines = append(lines, fmt.Sprintf("machine %s %s %s hash=%s joined=%t stopped=%t", nameOf(m.Role, m.Name), m.ID,
			m.Group, m.SpecHash, m.Joined, m.stopped))
	}
	for _, srv := range w.servers {
		lines = append(lines, fmt.Sprintf("server %s %s leader=%t voter=%t", srv.machine, srv.id, srv.leader, srv.voter))
	}
	for _, mem := range w.members {
		lines = append(lines, fmt.Sprintf("member %s %s", mem.name, mem.status))
	}
	for _, n := range w.nodes {
		role := v1alpha1.RoleClient
		if i := w.machineIndex(n.owner); i >= 0 {
			role = w.machines[i].Role
		}
		lines = append(lines, fmt.Sprintf("node %s %s %s eligible=%t draining=%t", nameOf(role, n.Name), n.ID, n.Status,
			n.Eligible, n.Draining))
	}
	slices.Sort(lines)
	return lines
}

// decider is the function that a run asks for its next step; the tests of the invariants pass their own.
type decider func(rollout.State, rollout.Mode) (rollout.Step, error)

// snapshot is a copy of the world before a decision, with the index of the line that the decision prints.
type snapshot struct {
	world *world
	line  int
}

// result is what a run printed and how it ended.
type result struct {
	lines     []string
	refused   bool
	snapshots []snapshot
}

// appendLine adds the step's text to the lines; a wait that repeats the one before is not added again.
func appendLine(lines []string, step rollout.Step) []string {
	line := step.String()
	if step.Action.Waits() && len(lines) > 0 && lines[len(lines)-1] == line {
		return lines
	}
	return append(lines, line)
}

// run asks decide for the next step, carries it out (a wait moves the clock instead) and repeats until Done or a
// refusal. It returns an error when an invariant breaks, a step cannot be applied or the run does not end.
func (w *world) run(mode rollout.Mode, decide decider, keepSnapshots bool) (result, error) {
	var res result
	for range maxRunSteps {
		var snap snapshot
		if keepSnapshots {
			snap.world = w.clone()
		}
		step, err := decide(w.observe(), mode)
		refused := errors.Is(err, rollout.ErrRefused)
		switch {
		case refused:
			res.lines = append(res.lines, "refused: "+err.Error())
		case err != nil:
			return res, err
		default:
			res.lines = appendLine(res.lines, step)
		}
		if keepSnapshots {
			snap.line = len(res.lines) - 1
			res.snapshots = append(res.snapshots, snap)
		}
		if refused {
			res.refused = true
			return res, nil
		}
		if step.Action == rollout.Done {
			return res, nil
		}
		if err := w.checkOrder(mode, step); err != nil {
			return res, err
		}
		if err := w.checkShrink(mode, step); err != nil {
			return res, err
		}
		if step.Action.Waits() {
			w.tick()
		} else if err := w.apply(step); err != nil {
			return res, err
		}
		if err := w.check(); err != nil {
			return res, fmt.Errorf("after %q: %w", step, err)
		}
	}
	return res, fmt.Errorf("the run did not end in %d steps", maxRunSteps)
}

// checkOrder is the invariant that a roll takes no step on a client group before the server groups are done: each has
// its size, and every machine of it is up to date.
func (w *world) checkOrder(mode rollout.Mode, step rollout.Step) error {
	if g, ok := w.group(step.Group); mode != rollout.Roll || !ok || g.Role != v1alpha1.RoleClient {
		return nil
	}
	for _, g := range w.groups {
		if g.Role == v1alpha1.RoleClient {
			continue
		}
		done := w.countOf(g.Name) == g.Size
		for _, m := range w.machines {
			done = done && (m.Group != g.Name || m.SpecHash == g.SpecHash)
		}
		if !done {
			return violated("%s: group %s is not done", step, g.Name)
		}
	}
	return nil
}

// checkShrink is the invariant that a shrink creates no machine and deletes none from a group that has no more
// machines than its size.
func (w *world) checkShrink(mode rollout.Mode, step rollout.Step) error {
	g, ok := w.group(step.Group)
	if mode != rollout.Shrink || !ok {
		return nil
	}
	switch n := w.countOf(g.Name); {
	case step.Action == rollout.Create:
		return violated("%s: a shrink creates no machine", step)
	case step.Action == rollout.Delete && n <= g.Size:
		return violated("%s: group %s has %d machines, no more than its size %d", step, g.Name, n, g.Size)
	}
	return nil
}
