package rollout_test

import (
	"fmt"
	"net/netip"
	"slices"
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
//   - A removed peer whose machine runs and whose member is alive comes back as a healthy nonvoter after 4 ticks, with
//     the same Raft ID, and votes 2 ticks later.
//   - A transfer of the leadership sets the StableSince of every server to the clock.
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
	ticks   int       // ticks it has been a healthy nonvoter
}

// simMember is a member of the gossip pool.
type simMember struct {
	machine string
	name    string
	address netip.Addr
	status  string // alive, leaving or failed
	ticks   int    // alive: ticks its machine has been down; failed: ticks since it failed
}

// simRemoved is a peer that was removed while its machine ran, and that the leader may add again.
type simRemoved struct {
	machine, id string
	ticks       int
}

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

// serverJoined records that a server joined the Raft configuration. A removal is no such change: every node that
// knows the servers that stay can still reach the cluster.
func (w *world) serverJoined() { w.lastChange = w.now }

// joinRaft adds a server of a machine to the Raft configuration and gives its machine an alive member if it has none.
func (w *world) joinRaft(machine, id string, leader, voter bool, since time.Time) {
	w.servers = append(w.servers, simServer{machine: machine, id: id, leader: leader, voter: voter, since: since})
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

// promoteServers makes a nonvoter that has been healthy for 2 ticks a voter, and labels its machine.
func (w *world) promoteServers() {
	for i := range w.servers {
		srv := &w.servers[i]
		if srv.voter || !w.healthy(*srv) {
			continue
		}
		if srv.ticks++; srv.ticks >= 2 {
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

// readdServers adds a removed peer again after 4 ticks, while its machine runs and its member is alive.
func (w *world) readdServers() {
	var kept []simRemoved
	for _, r := range w.removed {
		mem := w.memberIndexByMachine(r.machine)
		if !w.up(r.machine) || mem < 0 || w.members[mem].status != memberAlive {
			continue
		}
		if r.ticks++; r.ticks >= 4 {
			w.joinRaft(r.machine, r.id, false, false, w.now)
			w.serverJoined()
			continue
		}
		kept = append(kept, r)
	}
	w.removed = kept
}

// checkWindow is the invariant that a server is stopped, or removed while its machine runs, only when no server has
// joined the Raft configuration for a refresh interval. The peer of a machine that is down goes at any time.
func (w *world) checkWindow(step rollout.Step) error {
	if age := w.now.Sub(w.lastChange); age < refreshInterval {
		return violated("%s: a server joined the Raft configuration %s ago, less than the refresh interval of %s", step, age,
			refreshInterval)
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
	if err := w.checkWindow(step); err != nil {
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
		if err := w.checkWindow(step); err != nil {
			return err
		}
	}
	w.dropServer(i)
	if w.up(srv.machine) {
		w.removed = append(w.removed, simRemoved{machine: srv.machine, id: srv.id})
	}
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
