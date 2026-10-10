package rollout_test

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/rollout"
)

// The servers of the world follow what a local Nomad 2.0.7 did:
//
//   - A created server machine is ready after 1 tick, joins the Raft configuration as a healthy nonvoter after 2 (its
//     StableSince is then), votes after 4 and is labelled then.
//   - A stopped machine keeps its alive member and its healthy server for 4 ticks, then the member is failed and
//     autopilot reports the server unhealthy; a tick later autopilot removes the peer (noCleanup turns that off).
//   - Force-leave turns an alive member into leaving and drops it a tick later, and drops a failed one at once.
//   - The leader reconciles every 6 ticks from the moment it took the leadership (the start of the world, or the last
//     transfer to another server): each removed peer whose member is alive comes back as a healthy nonvoter with the
//     same Raft ID. A server that comes back while its machine runs votes 1 tick later, also when the machine has
//     stopped meanwhile, as long as its member is alive. One that comes back while its machine is stopped never votes.
//   - A transfer of the leadership to another server sets the StableSince of every server to the clock.
//   - Autopilot is healthy when every server of the Raft configuration has an alive member, and the failure
//     tolerance is the number of healthy voters beyond a majority, at least 0.

const (
	memberAlive   = "alive"
	memberLeaving = "leaving"
	memberFailed  = "failed"
)

// simServer is a server of the Raft configuration.
type simServer struct {
	machine string // the ID of its machine
	id      string // the Raft ID
	leader  bool
	voter   bool
	since   time.Time // autopilot's StableSince
	joined  time.Time // when it joined the Raft configuration; zero for a server the world started with
	ticks   int       // ticks it has been a healthy nonvoter
	// promoteAfter is how many ticks as a healthy nonvoter make it a voter; 0 means never.
	promoteAfter int
}

// simMember is a member of the gossip pool.
type simMember struct {
	machine string
	name    string
	address netip.Addr
	status  string // alive, leaving or failed
	ticks   int    // alive: ticks its machine has been down; failed: ticks since it failed
}

// simRemoved is a peer that was removed, and that the leader adds again at a reconcile while its member is alive.
type simRemoved struct{ machine, id string }

const (
	// newServerPromoteAfter and readdedServerPromoteAfter are the ticks as a healthy nonvoter that make a new server,
	// and a server that the leader added again while its machine ran, a voter.
	newServerPromoteAfter     = 2
	readdedServerPromoteAfter = 1
	// reconcileEvery is the ticks between two reconciles of the leader.
	reconcileEvery = 6
)

func (w *world) newRaftID() string {
	w.nextRaft++
	return fmt.Sprintf("r-%d", w.nextRaft)
}

func (w *world) serverIndexByID(id string) int {
	return slices.IndexFunc(w.servers, func(s simServer) bool { return s.id == id })
}

func (w *world) serverIndexByMachine(id string) int {
	return slices.IndexFunc(w.servers, func(s simServer) bool { return s.machine == id })
}

func (w *world) memberIndexByMachine(id string) int {
	return slices.IndexFunc(w.members, func(m simMember) bool { return m.machine == id })
}

// up reports whether the machine is listed and not stopped.
func (w *world) up(id string) bool {
	i := w.machineIndex(id)
	return i >= 0 && !w.machines[i].stopped
}

// leads reports whether the machine runs the leader.
func (w *world) leads(id string) bool {
	i := w.serverIndexByMachine(id)
	return i >= 0 && w.servers[i].leader
}

// healthy reports whether autopilot counts the server healthy: its member is alive.
func (w *world) healthy(srv simServer) bool {
	i := w.memberIndexByMachine(srv.machine)
	return i >= 0 && w.members[i].status == memberAlive
}

// joinRaft adds a server to the Raft configuration and gives its machine an alive member if it has none. A server
// that leads starts the leadership now.
func (w *world) joinRaft(srv simServer) {
	w.servers = append(w.servers, srv)
	if srv.leader {
		w.leaderSince = w.now
	}
	machine := srv.machine
	if w.memberIndexByMachine(machine) < 0 {
		m := w.machines[w.machineIndex(machine)]
		w.members = append(w.members, simMember{
			machine: machine, name: m.Name + ".global", address: m.PrivateIP, status: memberAlive,
		})
	}
}

// voterCounts returns the number of voters and how many of them run.
func (w *world) voterCounts() (voters, running int) {
	for _, srv := range w.servers {
		if srv.voter {
			voters++
			if w.up(srv.machine) {
				running++
			}
		}
	}
	return voters, running
}

// promoteServers makes a nonvoter that has been healthy for its promoteAfter ticks a voter, and labels its machine.
func (w *world) promoteServers() {
	for i := range w.servers {
		srv := &w.servers[i]
		if srv.voter || srv.promoteAfter == 0 || !w.healthy(*srv) {
			continue
		}
		if srv.ticks++; srv.ticks >= srv.promoteAfter {
			srv.voter = true
			if m := w.machineIndex(srv.machine); m >= 0 {
				w.machines[m].Joined = true
			}
		}
	}
}

// tickMembers drops the leaving members, fails the members of machines that were down for 4 ticks and lets autopilot
// remove the peer of a member that has been failed for a tick.
func (w *world) tickMembers() {
	kept := w.members[:0:0]
	for _, mem := range w.members {
		if mem.status == memberLeaving {
			continue
		}
		down := !w.up(mem.machine)
		switch mem.status {
		case memberAlive:
			if down {
				if mem.ticks++; mem.ticks >= 4 {
					mem.status, mem.ticks = memberFailed, 0
					if i := w.serverIndexByMachine(mem.machine); i >= 0 {
						w.servers[i].since = w.now
					}
				}
			}
		case memberFailed:
			mem.ticks++
			if i := w.serverIndexByMachine(mem.machine); i >= 0 && !w.noCleanup && !w.servers[i].leader {
				w.dropServer(i)
			}
		}
		kept = append(kept, mem)
	}
	w.members = kept
}

// dropServer removes a server from the Raft configuration.
func (w *world) dropServer(i int) {
	w.servers = slices.Delete(w.servers, i, i+1)
}

// reconcile is the pass of the leader: every reconcileEvery ticks from the moment it took the leadership it adds
// each removed peer whose member is alive as a nonvoter with its old Raft ID. The peer of a deleted machine stays
// out: the world has no server without a machine.
func (w *world) reconcile() {
	if w.now.Sub(w.leaderSince)%(reconcileEvery*tickLength) != 0 {
		return
	}
	var kept []simRemoved
	for _, r := range w.removed {
		mem := w.memberIndexByMachine(r.machine)
		if mem < 0 || w.members[mem].status != memberAlive || w.machineIndex(r.machine) < 0 {
			kept = append(kept, r)
			continue
		}
		srv := simServer{machine: r.machine, id: r.id, since: w.now, joined: w.now}
		if w.up(r.machine) {
			srv.promoteAfter = readdedServerPromoteAfter
		}
		w.joinRaft(srv)
	}
	w.removed = kept
}

// checkWindow is the invariant that a machine is stopped, or its server removed while it runs, only when no server
// of another machine has joined the Raft configuration for a refresh interval, and, when its server votes, when the
// leadership has not moved to another server for a refresh interval. The peer of a machine that is down goes at any
// time. A removal starts no window: every node that knows the servers that stay can still reach the cluster.
func (w *world) checkWindow(step rollout.Step, machine string) error {
	for _, srv := range w.servers {
		if age := w.now.Sub(srv.joined); srv.machine != machine && age < refreshInterval {
			return violated("%s: a server joined the Raft configuration %s ago, less than the refresh interval of %s", step,
				age, refreshInterval)
		}
	}
	if i := w.serverIndexByMachine(machine); i >= 0 && w.servers[i].voter {
		if age := w.now.Sub(w.transferredAt); age < refreshInterval {
			return violated("%s: its server votes and the leadership moved %s ago, less than the refresh interval of %s",
				step, age, refreshInterval)
		}
	}
	return nil
}

// checkVoters is the invariant that a machine whose server is in the Raft configuration is stopped only while at least
// two other voters run: a nonvoter that autopilot promotes after its machine stopped would leave no quorum.
func (w *world) checkVoters(step rollout.Step, machine string) error {
	if w.serverIndexByMachine(machine) < 0 {
		return nil
	}
	others := 0
	for _, srv := range w.servers {
		if srv.voter && srv.machine != machine && w.up(srv.machine) {
			others++
		}
	}
	if others < 2 {
		return violated("%s: its server is in the Raft configuration and %d other voters run, fewer than the 2 it needs",
			step, others)
	}
	return nil
}

func (w *world) transfer(step rollout.Step) error {
	i := w.serverIndexByID(step.Server.ID)
	if i < 0 {
		return fmt.Errorf("%s: no such server", step)
	}
	srv := w.servers[i]
	if !srv.voter || !w.up(srv.machine) || !w.healthy(srv) {
		return violated("%s: the server cannot take the leadership", step)
	}
	if srv.leader {
		return nil
	}
	for j := range w.servers {
		w.servers[j].leader = j == i
		w.servers[j].since = w.now
	}
	w.leaderSince, w.transferredAt = w.now, w.now
	return nil
}

func (w *world) stop(step rollout.Step) error {
	i := w.machineIndex(step.Machine.ID)
	if i < 0 {
		return fmt.Errorf("%s: no such machine", step)
	}
	if w.leads(step.Machine.ID) {
		return violated("%s: it is the machine of the leader", step)
	}
	if err := w.checkWindow(step, step.Machine.ID); err != nil {
		return err
	}
	if err := w.checkVoters(step, step.Machine.ID); err != nil {
		return err
	}
	w.machines[i].stopped, w.machines[i].Ready = true, false
	return nil
}

func (w *world) removePeer(step rollout.Step) error {
	i := w.serverIndexByID(step.Server.ID)
	if i < 0 {
		return fmt.Errorf("%s: no such server", step)
	}
	srv := w.servers[i]
	if srv.leader {
		return violated("%s: it is the leader", step)
	}
	if w.up(srv.machine) {
		if err := w.checkWindow(step, srv.machine); err != nil {
			return err
		}
	}
	w.dropServer(i)
	w.removed = append(w.removed, simRemoved{machine: srv.machine, id: srv.id})
	return nil
}

// forceLeave turns an alive member into a leaving one and drops a failed one.
func (w *world) forceLeave(step rollout.Step) {
	i := slices.IndexFunc(w.members, func(m simMember) bool { return m.name == step.Member.Name })
	if i < 0 {
		return
	}
	switch w.members[i].status {
	case memberAlive:
		w.members[i].status = memberLeaving
	case memberFailed:
		w.members = slices.Delete(w.members, i, i+1)
	}
}

// failServer stops a server's machine and fails its member at once, as if 4 ticks had passed.
func (w *world) failServer(name string) {
	w.stopMachine(name)
	id := w.machines[w.machineIndexByName(name)].ID
	w.members[w.memberIndexByMachine(id)].status = memberFailed
	w.servers[w.serverIndexByMachine(id)].since = w.now
}

// stopMachine stops a machine without the checks of a step.
func (w *world) stopMachine(name string) {
	i := w.machineIndexByName(name)
	w.machines[i].stopped, w.machines[i].Ready = true, false
}

// observeNomad returns what Nomad reports about the servers and the clients.
func (w *world) observeNomad() rollout.Nomad {
	n := rollout.Nomad{Healthy: true}
	healthyVoters, voters := 0, 0
	for _, srv := range w.servers {
		m := w.machines[w.machineIndex(srv.machine)]
		healthy := w.healthy(srv)
		n.Servers = append(n.Servers, rollout.Server{
			ID: srv.id, Name: m.Name + ".global", Address: netip.AddrPortFrom(m.PrivateIP, 4647), Voter: srv.voter,
			Leader: srv.leader, Healthy: healthy, StableSince: srv.since, Version: m.version,
		})
		n.Healthy = n.Healthy && healthy
		if srv.voter {
			voters++
			if healthy {
				healthyVoters++
			}
		}
	}
	n.FailureTolerance = max(0, healthyVoters-(voters/2+1))
	for _, mem := range w.members {
		n.Members = append(n.Members, rollout.Member{Name: mem.name, Address: mem.address, Status: mem.status})
	}
	for _, node := range w.nodes {
		n.Nodes = append(n.Nodes, node.Node)
	}
	return n
}

// tickAfterDelete is decide with one tick of the world between the delete of a machine and the next decision, so that
// the member of the deleted server has left the gossip pool and nothing lists its name any more.
func (w *world) tickAfterDelete(decide decider) decider {
	deleted := false
	return func(s rollout.State, mode rollout.Mode) (rollout.Step, error) {
		if deleted {
			w.tick()
			s = w.observe()
		}
		step, err := decide(s, mode)
		deleted = err == nil && step.Action == rollout.Delete
		return step, err
	}
}

// dropsFloor is decide without the index that tent remembers: the decisions see every group with NextIndex 0.
func dropsFloor(decide decider) decider {
	return func(s rollout.State, mode rollout.Mode) (rollout.Step, error) {
		s.Groups = slices.Clone(s.Groups)
		for i := range s.Groups {
			s.Groups[i].NextIndex = 0
		}
		return decide(s, mode)
	}
}

// firstCreate returns the line of the first create in the lines of a run.
func firstCreate(t *testing.T, lines []string) string {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(line, "create node ") {
			return line
		}
	}
	t.Fatalf("no create among the lines: %q", lines)
	return ""
}

// rollRun rolls the world with the decider that newDecider makes for it. A roll that ends refused is an error.
func rollRun(w *world, newDecider func(*world) decider) ([]string, error) {
	res, err := w.run(rollout.Roll, newDecider(w), false)
	if err == nil && res.refused {
		err = fmt.Errorf("the roll ended with %q", res.lines[len(res.lines)-1])
	}
	return res.lines, err
}

// nameScenarios are the runs in which a server is removed, nothing lists its name any more when the next create is
// decided, and the highest name of the group would be taken again but for the index that tent remembers. want is the
// name of the first create, and reused the name that it takes without the remembered index.
var nameScenarios = []struct {
	name   string
	run    func(newDecider func(*world) decider) ([]string, error)
	want   string
	reused string
}{
	// Both servers of a group of one are outdated and the old one leads, as a forced roll that was cut after the create
	// and forced again leaves it: the new server goes first.
	{"two outdated servers in a group of one", func(newDecider func(*world) decider) ([]string, error) {
		w := newWorld(curVersion)
		w.addGroup(serversGroup(1))
		w.addServer(oldHash, oldVersion)
		w.addServer(oldHash, oldVersion)
		return rollRun(w.arm(), newDecider)
	}, serverName(2), serverName(1)},
	// Four outdated machines in a group of three in three zones, the first leading, as a forced roll that was cut after
	// the create and forced again leaves it: the newest is in the leader's zone, the fullest, and goes first.
	{"the newest of four servers in three zones", func(newDecider func(*world) decider) ([]string, error) {
		w := newWorld(curVersion)
		g := serversGroup(3)
		g.Zones = []string{"ams", "fra", "lon"}
		w.addGroup(g)
		for range 4 {
			w.addServer(oldHash, oldVersion)
		}
		return rollRun(w.arm(), newDecider)
	}, serverName(4), serverName(3)},
	{"five servers shrunk to three", rollAfterShrink, serverName(5), serverName(3)},
}

// With the remembered index the run of each scenario names its first new server above every name that its group has
// had, breaks no invariant and is not refused.
func TestARollNamesNoServerTwice(t *testing.T) {
	for _, sc := range nameScenarios {
		t.Run(sc.name, func(t *testing.T) {
			lines, err := sc.run(func(w *world) decider { return w.tickAfterDelete(rollout.Next) })
			if err != nil {
				t.Fatal(err)
			}
			if got := firstCreate(t, lines); !strings.HasPrefix(got, "create node "+sc.want+" ") {
				t.Errorf("first create = %q, want a server named %s", got, sc.want)
			}
		})
	}
}

// A decider that drops the remembered index takes the name of a removed server in each scenario, and the invariant
// of the world finds it.
func TestADeciderWithoutTheRememberedIndexNamesAServerTwice(t *testing.T) {
	for _, sc := range nameScenarios {
		t.Run(sc.name, func(t *testing.T) {
			_, err := sc.run(func(w *world) decider { return w.tickAfterDelete(dropsFloor(rollout.Next)) })
			want := ": " + sc.reused + " was the name of a machine, a server or a member before"
			var v *violation
			if !errors.As(err, &v) || !strings.HasSuffix(v.msg, want) {
				t.Errorf("run error = %v, want one that ends with %q", err, want)
			}
		})
	}
}
