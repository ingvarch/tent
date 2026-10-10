package app_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// nodeHCL is the path of the file that holds the settings of one node.
const nodeHCL = "/etc/nomad.d/10-node.hcl"

// serverDelays are the times of the world's model of its servers. Every delay is 0 by default, which makes each
// change of a server take effect at once.
type serverDelays struct {
	voteAfter    time.Duration // a new server joins as a nonvoter and votes this long after it joined
	failAfter    time.Duration // a halted or deleted server stays alive and healthy this long
	cleanupAfter time.Duration // autopilot removes the peer of a failed server this long after it failed
	reportLag    time.Duration // the report keeps a removed peer this long
	transferBlip time.Duration // one follower reads unhealthy this long after a transfer of the leadership
	noCleanup    bool          // autopilot never removes the peer of a failed server
}

// raftServer is the world's record of a server machine that was ready once: its place in the Raft configuration, in
// autopilot's report and in the gossip pool. A record stays for as long as its cluster lives. A server that started a
// cluster of its own has a record that is out of the Raft configuration and the report, with its member alive.
type raftServer struct {
	id, name, address string     // the instance ID, the hostname and the public address
	private           netip.Addr // the address in the cluster's VPC
	bootstrap         bool       // it joined before the cluster had a leader, and votes from then on
	joined, stable    time.Time  // when it joined, and StableSince before it is truncated to whole seconds
	down              time.Time  // when the world first saw its machine halted or gone; zero while it runs
	removed           bool       // its peer is out of the Raft configuration
	alone             bool       // it started a cluster of its own: it never joined this one, and its node never registers
	removedAt         time.Time
	forced            bool // a ForceLeave turned its member to leaving
	shown             bool // a read of Members showed it leaving, and it is not listed again
	left              bool // a ForceLeave dropped its member, which failed
}

// raftCluster is the world's servers, in the order they joined, with the server that leads, by instance ID, and the
// follower that reads unhealthy after a transfer.
type raftCluster struct {
	servers   []*raftServer
	leader    string
	blipID    string
	blipUntil time.Time
}

// raftIDOf returns the Raft ID of the server machine with the instance ID id.
func raftIDOf(id string) string { return "r-" + id }

// memberName returns the name that the gossip pool and autopilot give the server.
func (r *raftServer) memberName() string { return r.name + ".global" }

// running reports whether the server's machine runs, as far as the world has seen.
func (r *raftServer) running() bool { return r.down.IsZero() }

// votes reports whether the server votes at now.
func (r *raftServer) votes(now time.Time, d serverDelays) bool {
	return r.bootstrap || !now.Before(r.joined.Add(d.voteAfter))
}

// failedAt returns when the server counts as failed, and whether its machine stopped at all.
func (r *raftServer) failedAt(d serverDelays) (time.Time, bool) {
	return r.down.Add(d.failAfter), !r.running()
}

// failed reports whether the server counts as failed at now.
func (r *raftServer) failed(now time.Time, d serverDelays) bool {
	at, stopped := r.failedAt(d)
	return stopped && !now.Before(at)
}

// find returns the record of the server machine with the instance ID id, or nil.
func (c *raftCluster) find(id string) *raftServer {
	i := slices.IndexFunc(c.servers, func(r *raftServer) bool { return r.id == id })
	if i < 0 {
		return nil
	}
	return c.servers[i]
}

// peer returns the record of the server in the Raft configuration with the Raft ID raftID, or nil.
func (c *raftCluster) peer(raftID string) *raftServer {
	i := slices.IndexFunc(c.servers, func(r *raftServer) bool { return !r.removed && raftIDOf(r.id) == raftID })
	if i < 0 {
		return nil
	}
	return c.servers[i]
}

// firstPeer returns the first server in the Raft configuration that votes at now and is not skip, or nil.
func (c *raftCluster) firstPeer(now time.Time, d serverDelays, skip func(*raftServer) bool) *raftServer {
	i := slices.IndexFunc(c.servers, func(r *raftServer) bool { return !r.removed && r.votes(now, d) && !skip(r) })
	if i < 0 {
		return nil
	}
	return c.servers[i]
}

// ServersOverTime makes the servers change as Nomad's do: a new server votes after 15 s, a stopped one fails after
// 40 s, autopilot removes its peer 2 s after, and the report keeps a removed peer 2 s.
func (w *nomadWorld) ServersOverTime() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.voteAfter, w.delays.failAfter = 15*time.Second, 40*time.Second
	w.delays.cleanupAfter, w.delays.reportLag = 2*time.Second, 2*time.Second
}

// SetVoteAfter makes a server that joins from now on vote d after it joined; a d beyond what a wait lasts gives a
// server that never votes. A server that joins before the cluster has a leader votes at once.
func (w *nomadWorld) SetVoteAfter(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.voteAfter = d
}

// SetFailAfter makes a server whose machine stopped stay alive and healthy for d from when the world first saw it
// stopped.
func (w *nomadWorld) SetFailAfter(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.failAfter = d
}

// SetCleanupAfter makes autopilot remove the peer of a failed server d after it failed.
func (w *nomadWorld) SetCleanupAfter(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.cleanupAfter = d
}

// NoCleanup keeps autopilot from removing the peer of a failed server: only RemovePeer does.
func (w *nomadWorld) NoCleanup() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.noCleanup = true
}

// SetReportLag makes the autopilot report keep a removed peer for d, as left and unhealthy.
func (w *nomadWorld) SetReportLag(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.reportLag = d
}

// SetTransferBlip makes one follower read unhealthy for d after a transfer of the leadership, and the cluster
// unhealthy with it.
func (w *nomadWorld) SetTransferBlip(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.transferBlip = d
}

// FailOnLeaderLoss makes the world fail tb when the machine of the server that leads halts or is deleted.
func (w *nomadWorld) FailOnLeaderLoss(tb testing.TB) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tb = tb
}

// HaltedCalls returns the calls that reached the address of a server whose machine is halted or gone, in order. Such
// a call fails with nomadops.ErrNotReady and reaches neither the hook nor the Nomad fake.
func (w *nomadWorld) HaltedCalls() []nomadfake.Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.haltedCalls)
}

// reachesHalted reports whether the call goes to the address of a server whose machine is halted or gone, and notes
// it in HaltedCalls when it does.
func (w *nomadWorld) reachesHalted(call nomadfake.Call) bool {
	host, _, _ := strings.Cut(call.Server, ":")
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.IndexFunc(w.raft.servers, func(r *raftServer) bool { return r.address == host && !r.running() })
	if i >= 0 {
		w.haltedCalls = append(w.haltedCalls, call)
	}
	return i >= 0
}

// errHalted is what a call to a halted or gone server returns, as a refused connection does.
func errHalted(server string) error {
	return fmt.Errorf("dial %s: connection refused: %w", server, nomadops.ErrNotReady)
}

// raftView is what the world sets the Nomad fake to.
type raftView struct {
	leader  string // the leader's RPC address; empty without a leader
	health  nomadops.Health
	peers   []nomadops.Peer
	members []nomadops.Member
}

// raftView brings the servers to now from the server machines and returns what the fake shows. want returns how many
// servers the specs give, which the cluster needs ready to elect its first leader.
func (w *nomadWorld) raftView(servers []machine, want func() int) raftView {
	w.mu.Lock()
	defer w.mu.Unlock()
	now, d, c := time.Now(), w.delays, &w.raft
	w.syncServers(now, servers, want)
	var v raftView
	var voters, healthyVoters int
	reportHealthy := true
	for _, r := range c.servers {
		failed := r.failed(now, d)
		blipped := r.id == c.blipID && now.Before(c.blipUntil)
		voter := r.votes(now, d)
		counted := !r.removed && !failed
		if !r.removed {
			v.peers = append(v.peers, nomadops.Peer{
				ID: raftIDOf(r.id), Name: r.memberName(), Address: netip.AddrPortFrom(r.private, 4647), Voter: voter,
				Leader: r.id == c.leader,
			})
			if voter {
				voters++
				if counted && !blipped {
					healthyVoters++
				}
			}
		}
		if !r.left && !r.shown {
			v.members = append(v.members, nomadops.Member{
				Name: r.memberName(), Address: r.private, Status: r.memberStatus(now, d),
			})
		}
		if r.removed && !now.Before(r.removedAt.Add(d.reportLag)) {
			continue
		}
		entry := nomadops.ServerHealth{
			ID: raftIDOf(r.id), Name: r.memberName(), Address: netip.AddrPortFrom(r.private, 4647), Serf: "alive",
			Healthy: counted && !blipped, Voter: voter, Leader: r.id == c.leader, Version: w.pinned(),
			StableSince: r.stable.Truncate(time.Second),
		}
		if !counted {
			entry.Serf = "left"
		}
		reportHealthy = reportHealthy && entry.Healthy
		v.health.Servers = append(v.health.Servers, entry)
	}
	// Nomad does not keep an order for the entries; the world gives the reverse of the order of the servers.
	slices.Reverse(v.health.Servers)
	v.health.Healthy = reportHealthy && !w.unhealthy
	v.health.Voters = voters
	v.health.FailureTolerance = max(0, healthyVoters-(voters/2+1))
	if lead := c.find(c.leader); lead != nil && !w.noLeader {
		v.leader = lead.address + ":4647"
	}
	return v
}

// memberStatus returns the status of the server's member in the gossip pool.
func (r *raftServer) memberStatus(now time.Time, d serverDelays) string {
	switch {
	case r.forced:
		return "leaving"
	case r.failed(now, d):
		return "failed"
	}
	return "alive"
}

// syncServers brings the records to now: a ready machine that has no record joins, or starts a cluster of its own when
// bootstrapsAlone says so and the cluster has a leader, a machine that stopped is noted,
// a server that failed long enough has its peer removed by autopilot, and a leader is elected when the cluster has
// none and as many servers are ready as the specs give. want reads the specs, so it is asked only while the cluster
// has no leader. The caller holds w.mu.
func (w *nomadWorld) syncServers(now time.Time, servers []machine, want func() int) {
	c, d := &w.raft, w.delays
	byID := map[string]machine{}
	ready := 0
	for _, m := range servers {
		byID[m.id] = m
		if !m.ready {
			continue
		}
		ready++
		if c.find(m.id) == nil {
			alone := c.leader != "" && w.bootstrapsAlone(m.id)
			c.servers = append(c.servers, &raftServer{
				id: m.id, name: m.name, address: m.address, private: m.private, bootstrap: c.leader == "", joined: now, stable: now,
				alone: alone, removed: alone,
			})
		}
	}
	for _, r := range c.servers {
		if _, found := byID[r.id]; r.running() && (!found || w.cloud.Halted(r.id)) {
			r.down = now
		}
		at, stopped := r.failedAt(d)
		if !stopped || now.Before(at) {
			continue
		}
		r.stable = laterOf(r.stable, at)
		if cleanup := at.Add(d.cleanupAfter); !d.noCleanup && !r.removed && !now.Before(cleanup) {
			r.removed, r.removedAt = true, cleanup
		}
	}
	if lead := c.find(c.leader); lead != nil && !lead.running() {
		if w.tb != nil {
			w.tb.Errorf("the machine of the leader %s halted or is gone while it led", lead.name)
		}
		c.leader = ""
		c.elect(now, d)
	}
	if c.leader == "" && ready > 0 {
		if n := want(); n > 0 && ready >= n {
			c.elect(now, d)
		}
	}
}

// bootstrapsAlone reports whether the create request of the instance id carried a NodeConfig with bootstrap_expect = 1
// in its 10-node.hcl: a server that starts with it and an empty data directory leads a cluster of its own. An
// instance with no request or no NodeConfig does not.
func (w *nomadWorld) bootstrapsAlone(id string) bool {
	req, ok := w.cloud.CreateRequest(id)
	if !ok {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(req.UserData)
	if err != nil {
		return false
	}
	payload, err := app.PayloadFrom(data)
	if err != nil {
		return false
	}
	nc, err := nodeconfig.Decode(payload)
	if err != nil {
		return false
	}
	f := fileOf(nc, nodeHCL)
	return f != nil && bytes.Contains(f.Content, []byte("bootstrap_expect = 1\n"))
}

// startedAlone reports whether the machine with the instance ID id started a cluster of its own. The caller holds w.mu.
func (w *nomadWorld) startedAlone(id string) bool {
	r := w.raft.find(id)
	return r != nil && r.alone
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// elect makes the first running server that is in the Raft configuration and votes the leader.
func (c *raftCluster) elect(now time.Time, d serverDelays) {
	if r := c.firstPeer(now, d, func(r *raftServer) bool { return !r.running() }); r != nil {
		c.leader = r.id
	}
}

// transferred records a TransferLeadership to the server with the Raft ID raftID that the fake carried out, as the
// fake does: a server that does not vote hands the leadership to the first voter other than the leader, and every
// server's StableSince becomes now. One follower then reads unhealthy for the transfer blip.
func (w *nomadWorld) transferred(raftID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now, d, c := time.Now(), w.delays, &w.raft
	target := c.peer(raftID)
	if target == nil || target.id == c.leader {
		return
	}
	if !target.votes(now, d) {
		target = c.firstPeer(now, d, func(r *raftServer) bool { return r.id == c.leader })
		if target == nil {
			return
		}
	}
	c.leader = target.id
	for _, r := range c.servers {
		r.stable = now
	}
	if r := c.firstPeer(now, d, func(r *raftServer) bool { return r.id == c.leader }); r != nil {
		c.blipID, c.blipUntil = r.id, now.Add(d.transferBlip)
	}
}

// peerRemoved records a RemovePeer of the server with the Raft ID raftID that the fake carried out.
func (w *nomadWorld) peerRemoved(raftID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.raft.peer(raftID); r != nil && r.id != w.raft.leader {
		r.removed, r.removedAt = true, time.Now()
	}
}

// memberForced records a ForceLeave of the member name that the fake carried out, as the fake does: a failed member
// is gone at once, and an alive one leaves: the next read of Members lists it as leaving, and no read lists it after.
func (w *nomadWorld) memberForced(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for _, r := range w.raft.servers {
		if r.memberName() != name {
			continue
		}
		switch r.memberStatus(now, w.delays) {
		case "failed":
			r.left = true
		case "alive":
			r.forced = true
		}
	}
}

// membersRead records a read of Members that the fake carried out: a member that a ForceLeave turned to leaving has
// been shown so, and is not listed again.
func (w *nomadWorld) membersRead() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.raft.servers {
		r.shown = r.forced
	}
}
