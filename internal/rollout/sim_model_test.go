package rollout_test

import (
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// These tests pin the model that the goldens and the resume test stand on.

func TestSimCreatedMachineTimeline(t *testing.T) {
	w := outdatedWorkers(1, 0).arm()
	create := rollout.Step{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
		Name: "prod-workers-3", Zone: "fra"}}
	if err := w.apply(create); err != nil {
		t.Fatal(err)
	}
	newest := func() rollout.Machine { return w.observe().Machines[len(w.machines)-1] }
	nodes := func() int { return len(w.observe().Nomad.Nodes) }

	m := newest()
	if m.Name != "prod-workers-3" || m.Zone != "fra" || m.SpecHash != newHash || m.Ready || m.Joined || nodes() != 3 {
		t.Errorf("a created machine = %+v with %d nodes, want it listed, not ready, not joined, with no node", m, nodes())
	}
	w.tick()
	if m = newest(); !m.Ready || m.Joined || nodes() != 3 {
		t.Errorf("after 1 tick: %+v with %d nodes, want it ready, not joined, with no node", m, nodes())
	}
	w.tick()
	m = newest()
	n := w.observe().Nomad.Nodes[3]
	if !m.Ready || !m.Joined || n.Name != "prod-workers-3" || n.Address != m.PrivateIP || n.Status != "ready" ||
		!n.Eligible || n.Version != curVersion {
		t.Errorf("after 2 ticks: %+v and node %+v, want a joined machine and its ready, eligible node", m, n)
	}
}

func TestSimDrainMovesAllocationsAfterTwoTicks(t *testing.T) {
	w := outdatedWorkers(1, 0).arm()
	drain := w.machineStep(rollout.Drain, "prod-workers-0")
	if err := w.apply(drain); err != nil {
		t.Fatal(err)
	}
	node := func() rollout.Node { return w.observe().Nomad.Nodes[0] }
	if n := node(); !n.Draining || n.Eligible || n.DrainedFor != "" {
		t.Errorf("after the drain starts: %+v, want it draining and ineligible", n)
	}
	w.tick()
	if n := node(); !n.Draining {
		t.Errorf("after 1 tick: %+v, want it still draining", n)
	}
	w.tick()
	n := node()
	if n.Draining || n.Eligible || n.DrainedFor != drain.Machine.ID {
		t.Errorf("after 2 ticks: %+v, want the drain complete for %s, the node ineligible", n, drain.Machine.ID)
	}
	if w.nodes[0].allocs != 0 || w.nodes[1].allocs != 2*allocsOfNode || w.unplaced != 0 {
		t.Errorf("allocations = %d, %d, unplaced %d, want 0, %d, 0", w.nodes[0].allocs, w.nodes[1].allocs, w.unplaced,
			2*allocsOfNode)
	}
}

func TestSimDrainWithNowhereToGoLeavesAllocationsUnplaced(t *testing.T) {
	w := newWorld(curVersion)
	w.addServer(newHash, curVersion)
	w.addGroup(workersGroup(1, 0))
	w.addClients("workers", 1, oldHash, oldVersion)
	if err := w.apply(w.machineStep(rollout.Drain, "prod-workers-0")); err != nil {
		t.Fatal(err)
	}
	w.tick()
	w.tick()
	if w.unplaced != allocsOfNode {
		t.Errorf("unplaced = %d, want %d", w.unplaced, allocsOfNode)
	}
}

func TestSimNodeOfADeletedMachineGoesDownAfterTwoTicksAndPurgeRemovesIt(t *testing.T) {
	w := outdatedWorkers(1, 0).arm()
	if err := w.apply(w.machineStep(rollout.Drain, "prod-workers-0")); err != nil {
		t.Fatal(err)
	}
	w.tick()
	w.tick()
	del := w.machineStep(rollout.Delete, "prod-workers-0")
	if err := w.apply(del); err != nil {
		t.Fatal(err)
	}
	if len(w.observe().Machines) != 3 {
		t.Errorf("machines after the delete = %d, want 3", len(w.observe().Machines))
	}
	w.tick()
	if got := w.observe().Nomad.Nodes[0].Status; got != "ready" {
		t.Errorf("node after 1 tick = %s, want ready", got)
	}
	w.tick()
	node := w.observe().Nomad.Nodes[0]
	if node.Status != "down" {
		t.Fatalf("node after 2 ticks = %s, want down", node.Status)
	}
	if err := w.apply(rollout.Step{Action: rollout.Purge, Node: node}); err != nil {
		t.Fatal(err)
	}
	if got := len(w.observe().Nomad.Nodes); got != 2 {
		t.Errorf("nodes after the purge = %d, want 2", got)
	}
}

func TestSimNodeOfADeletedMachineGoesDownAfterTheScenarioDelay(t *testing.T) {
	w := outdatedWorkers(1, 0).arm()
	w.downAfter = 5
	if err := w.apply(w.machineStep(rollout.Drain, "prod-workers-0")); err != nil {
		t.Fatal(err)
	}
	w.tick()
	w.tick()
	if err := w.apply(w.machineStep(rollout.Delete, "prod-workers-0")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		w.tick()
		if got := w.observe().Nomad.Nodes[0].Status; got != "ready" {
			t.Fatalf("node after %d ticks = %s, want ready", i, got)
		}
	}
	w.tick()
	if got := w.observe().Nomad.Nodes[0].Status; got != "down" {
		t.Errorf("node after 5 ticks = %s, want down", got)
	}
}

func TestSimNewMachineTakesTheLowestFreeAddressWhenTheScenarioAsksForIt(t *testing.T) {
	create := rollout.Step{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
		Name: "prod-workers-3", Zone: "fra"}}
	newest := func(w *world) netip.Addr { return w.machines[len(w.machines)-1].PrivateIP }
	for _, tt := range []struct {
		name  string
		reuse bool
		want  netip.Addr
	}{
		{"an address is never used twice by default", false, ip(7)},
		{"the lowest address that no machine holds", true, ip(4)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := outdatedWorkers(1, 0).arm()
			w.reuseAddresses = tt.reuse
			if err := w.apply(w.machineStep(rollout.Drain, "prod-workers-0")); err != nil {
				t.Fatal(err)
			}
			w.tick()
			w.tick()
			if err := w.apply(w.machineStep(rollout.Delete, "prod-workers-0")); err != nil {
				t.Fatal(err)
			}
			if err := w.apply(create); err != nil {
				t.Fatal(err)
			}
			if got := newest(w); got != tt.want {
				t.Errorf("address of the new machine = %s, want %s", got, tt.want)
			}
		})
	}
}

// Which client machine got which name depends on when its predecessor's node was purged; the shape ignores that and
// nothing else.
func TestSimClientShapeIgnoresTheNamesOfClientsOnly(t *testing.T) {
	a := outdatedWorkers(1, 0)
	rename := func(w *world, name, other string) {
		i := w.machineIndexByName(name)
		w.machines[i].Name = other
		if j := w.nodeIndexByOwner(w.machines[i].ID); j >= 0 {
			w.nodes[j].Name = other
		}
	}
	b := a.clone()
	rename(b, "prod-workers-0", "prod-workers-9")
	if slices.Equal(a.summary(), b.summary()) {
		t.Error("the summary does not show the name of a client")
	}
	if !slices.Equal(a.clientShape(), b.clientShape()) {
		t.Errorf("the shapes differ by the name of a client (-a +b):\n%s", cmp.Diff(a.clientShape(), b.clientShape()))
	}
	c := a.clone()
	rename(c, "prod-servers-0", "prod-servers-9")
	if slices.Equal(a.clientShape(), c.clientShape()) {
		t.Error("the shape does not show the name of a server")
	}
	d := a.clone()
	d.makeIneligible("prod-workers-1")
	if slices.Equal(a.clientShape(), d.clientShape()) {
		t.Error("the shape does not show an ineligible client node")
	}
}

func TestSimServersAreHealthyAndTheFirstLeads(t *testing.T) {
	w := newWorld(curVersion)
	w.addServer(newHash, curVersion)
	w.addServer(newHash, curVersion)
	s := w.observe()
	if !s.Nomad.Healthy || len(s.Nomad.Servers) != 2 || len(s.Nomad.Members) != 2 {
		t.Fatalf("Nomad = %+v, want two healthy servers and two members", s.Nomad)
	}
	first, second := s.Nomad.Servers[0], s.Nomad.Servers[1]
	if !first.Leader || second.Leader || !first.Voter || !first.Healthy || first.Name != "prod-servers-0.global" ||
		second.Name != "prod-servers-1.global" || first.Version != curVersion {
		t.Errorf("servers = %+v and %+v, want the first to lead", first, second)
	}
	if first.Address.Addr() != s.Machines[0].PrivateIP || first.Address.Port() != 4647 {
		t.Errorf("address = %s, want the machine's private address on port 4647", first.Address)
	}
}

func TestSimSummaryTellsWorldsApart(t *testing.T) {
	a := outdatedWorkers(1, 0)
	b := outdatedWorkers(1, 0)
	if !slices.Equal(a.summary(), b.summary()) {
		t.Fatal("equal worlds have different summaries")
	}
	b.makeIneligible("prod-workers-1")
	if slices.Equal(a.summary(), b.summary()) {
		t.Error("the summary does not show an ineligible node")
	}
}

// serverWorld is a cluster of n up to date servers of the group servers, the first leading.
func serverWorld(n int) *world {
	w := newWorld(curVersion)
	w.addGroup(serversGroup(n))
	for range n {
		w.addServer(newHash, curVersion)
	}
	return w
}

// ticks moves the world on by n ticks.
func (w *world) ticks(n int) {
	for range n {
		w.tick()
	}
}

func serverOf(t *testing.T, s rollout.State, name string) rollout.Server {
	t.Helper()
	for _, srv := range s.Nomad.Servers {
		if srv.Name == name+".global" {
			return srv
		}
	}
	t.Fatalf("no server %s in %+v", name, s.Nomad.Servers)
	return rollout.Server{}
}

func memberOf(s rollout.State, name string) (rollout.Member, bool) {
	for _, m := range s.Nomad.Members {
		if m.Name == name+".global" {
			return m, true
		}
	}
	return rollout.Member{}, false
}

func TestSimNewServerJoinsAsANonvoterAndVotesTwoTicksLater(t *testing.T) {
	w := serverWorld(3)
	create := rollout.Step{Action: rollout.Create, Group: "servers", Machine: rollout.Machine{
		Name: "prod-servers-3", Zone: "ams"}}
	if err := w.apply(create); err != nil {
		t.Fatal(err)
	}
	newest := func() rollout.Machine { return w.observe().Machines[3] }
	if m := newest(); m.Ready || m.Joined || len(w.observe().Nomad.Servers) != 3 {
		t.Errorf("a created server machine = %+v, want it listed, not ready, with no server", m)
	}
	w.ticks(1)
	if m := newest(); !m.Ready || m.Joined || len(w.observe().Nomad.Servers) != 3 {
		t.Errorf("after 1 tick: %+v, want it ready with no server", m)
	}
	w.ticks(1)
	joined := w.now
	s := w.observe()
	srv := serverOf(t, s, "prod-servers-3")
	if srv.Voter || !srv.Healthy || srv.Leader || !srv.StableSince.Equal(joined) || srv.Version != curVersion ||
		srv.Address.Addr() != newest().PrivateIP || newest().Joined {
		t.Errorf("after 2 ticks: server %+v, want a healthy nonvoter stable since %s, not labelled", srv, joined)
	}
	if m, ok := memberOf(s, "prod-servers-3"); !ok || m.Status != "alive" || m.Address != newest().PrivateIP {
		t.Errorf("after 2 ticks: member %+v, want it alive at the machine's address", m)
	}
	w.ticks(1)
	if serverOf(t, w.observe(), "prod-servers-3").Voter || newest().Joined {
		t.Error("after 3 ticks: the server votes or is labelled, want a nonvoter")
	}
	w.ticks(1)
	srv = serverOf(t, w.observe(), "prod-servers-3")
	if !srv.Voter || !newest().Joined || !srv.StableSince.Equal(joined) {
		t.Errorf("after 4 ticks: server %+v and machine %+v, want a voter, labelled, stable since %s", srv, newest(), joined)
	}
}

func TestSimStoppedServerFailsAfterFourTicksAndAutopilotRemovesItsPeerOneTickLater(t *testing.T) {
	w := serverWorld(3)
	if err := w.apply(w.serverStep(rollout.Stop, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	state := func() (rollout.State, rollout.Server) {
		s := w.observe()
		return s, serverOf(t, s, "prod-servers-1")
	}
	for tick := 1; tick <= 3; tick++ {
		w.tick()
		s, srv := state()
		if m, _ := memberOf(s, "prod-servers-1"); m.Status != "alive" || !srv.Healthy || !s.Nomad.Healthy ||
			s.Machines[1].Ready {
			t.Fatalf("after %d ticks: member %+v, server %+v, want it alive and healthy on a stopped machine", tick, m, srv)
		}
	}
	w.tick()
	failed := w.now
	s, srv := state()
	if m, _ := memberOf(s, "prod-servers-1"); m.Status != "failed" || srv.Healthy || s.Nomad.Healthy ||
		!srv.StableSince.Equal(failed) {
		t.Errorf("after 4 ticks: member %+v, server %+v, want it failed and unhealthy since %s", m, srv, failed)
	}
	if serverOf(t, s, "prod-servers-0").StableSince.Equal(failed) {
		t.Error("the health of the other servers did not change, but their StableSince did")
	}
	w.tick()
	s = w.observe()
	if len(s.Nomad.Servers) != 2 || !s.Nomad.Healthy {
		t.Errorf("after 5 ticks: servers %+v, want autopilot to have removed the peer and be healthy", s.Nomad.Servers)
	}
	if m, ok := memberOf(s, "prod-servers-1"); !ok || m.Status != "failed" {
		t.Errorf("member after the cleanup = %+v (%t), want it still listed as failed", m, ok)
	}
}

func TestSimAutopilotCleanupCanBeTurnedOff(t *testing.T) {
	w := serverWorld(3)
	w.noCleanup = true
	if err := w.apply(w.serverStep(rollout.Stop, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	w.ticks(8)
	if got := len(w.observe().Nomad.Servers); got != 3 {
		t.Errorf("servers = %d, want 3: nothing removes the peer", got)
	}
}

func TestSimForceLeave(t *testing.T) {
	member := func(w *world, name string) string {
		m, ok := memberOf(w.observe(), name)
		if !ok {
			return "gone"
		}
		return m.Status
	}
	t.Run("an alive member leaves and is dropped one tick later", func(t *testing.T) {
		w := serverWorld(3)
		if err := w.apply(w.serverStep(rollout.ForceLeave, "prod-servers-1")); err != nil {
			t.Fatal(err)
		}
		if got := member(w, "prod-servers-1"); got != "leaving" {
			t.Fatalf("member = %s, want leaving", got)
		}
		w.tick()
		if got := member(w, "prod-servers-1"); got != "gone" {
			t.Errorf("member after 1 tick = %s, want gone", got)
		}
	})
	t.Run("a failed member is dropped at once", func(t *testing.T) {
		w := serverWorld(3)
		w.noCleanup = true
		w.failServer("prod-servers-1")
		if err := w.apply(w.serverStep(rollout.ForceLeave, "prod-servers-1")); err != nil {
			t.Fatal(err)
		}
		if got := member(w, "prod-servers-1"); got != "gone" {
			t.Errorf("member = %s, want gone", got)
		}
	})
	t.Run("an unknown member changes nothing", func(t *testing.T) {
		w := serverWorld(3)
		step := rollout.Step{Action: rollout.ForceLeave, Member: rollout.Member{Name: "prod-servers-9.global"}}
		if err := w.apply(step); err != nil {
			t.Fatal(err)
		}
		if got := len(w.observe().Nomad.Members); got != 3 {
			t.Errorf("members = %d, want 3", got)
		}
	})
}

func TestSimRemovedPeerOfARunningServerComesBackAsANonvoter(t *testing.T) {
	w := serverWorld(3)
	if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	if got := len(w.observe().Nomad.Servers); got != 2 {
		t.Fatalf("servers after the removal = %d, want 2", got)
	}
	w.ticks(3)
	if got := len(w.observe().Nomad.Servers); got != 2 {
		t.Fatalf("servers after 3 ticks = %d, want 2", got)
	}
	w.ticks(1)
	back := w.now
	srv := serverOf(t, w.observe(), "prod-servers-1")
	if srv.Voter || !srv.Healthy || srv.ID != "r-2" || !srv.StableSince.Equal(back) {
		t.Errorf("after 4 ticks: server %+v, want the same Raft ID back as a healthy nonvoter since %s", srv, back)
	}
	w.ticks(2)
	if !serverOf(t, w.observe(), "prod-servers-1").Voter {
		t.Error("after 2 more ticks: the server does not vote, want it promoted")
	}
}

func TestSimPeerOfAMachineThatIsDownGoesWithoutTheWindow(t *testing.T) {
	// The window protects the nodes that still list the removed server as reachable; a machine that is down is not.
	w := serverWorld(3)
	w.lastChange = w.now
	w.stopMachine("prod-servers-1")
	if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
		t.Errorf("removing the peer of a stopped machine right after a join: %v", err)
	}
	var v *violation
	if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-2")); !errors.As(err, &v) {
		t.Errorf("removing the peer of a running machine right after a join = %v, want a violation", err)
	}
}

func TestSimOnlyAServerThatJoinsStartsTheWindow(t *testing.T) {
	w := serverWorld(3)
	if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	if !w.lastChange.IsZero() {
		t.Errorf("a removal set the last change to %s, want it left alone", w.lastChange)
	}
	w.ticks(4)
	if !w.lastChange.Equal(w.now) {
		t.Errorf("after the server came back, the last change is %s, want %s", w.lastChange, w.now)
	}
}

func TestSimRemovedPeerDoesNotComeBackWithoutAnAliveMemberOnARunningMachine(t *testing.T) {
	t.Run("the member was forced out", func(t *testing.T) {
		w := serverWorld(3)
		if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
			t.Fatal(err)
		}
		if err := w.apply(w.serverStep(rollout.ForceLeave, "prod-servers-1")); err != nil {
			t.Fatal(err)
		}
		w.ticks(8)
		if got := len(w.observe().Nomad.Servers); got != 2 {
			t.Errorf("servers = %d, want 2", got)
		}
	})
	t.Run("the machine was stopped", func(t *testing.T) {
		w := serverWorld(3)
		w.noCleanup = true
		if err := w.apply(w.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
			t.Fatal(err)
		}
		w.stopMachine("prod-servers-1")
		w.ticks(8)
		if got := len(w.observe().Nomad.Servers); got != 2 {
			t.Errorf("servers = %d, want 2", got)
		}
	})
}

func TestSimTransferMovesTheLeaderAndResetsEveryStableSince(t *testing.T) {
	w := serverWorld(3)
	w.ticks(2)
	if err := w.apply(w.serverStep(rollout.TransferLeadership, "prod-servers-2")); err != nil {
		t.Fatal(err)
	}
	for _, srv := range w.observe().Nomad.Servers {
		if srv.Leader != (srv.Name == "prod-servers-2.global") || !srv.StableSince.Equal(w.now) {
			t.Errorf("server %+v after the transfer, want prod-servers-2 to lead and every StableSince %s", srv, w.now)
		}
	}
}

func TestSimTransferToTheLeaderChangesNothing(t *testing.T) {
	w := serverWorld(3)
	w.ticks(2)
	before := w.observe().Nomad.Servers
	if err := w.apply(w.serverStep(rollout.TransferLeadership, "prod-servers-0")); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, w.observe().Nomad.Servers, cmpopts.EquateComparable(netip.AddrPort{})); diff != "" {
		t.Errorf("servers changed (-before +after):\n%s", diff)
	}
}

func TestSimTransferToAServerThatCannotLeadIsAViolation(t *testing.T) {
	w := serverWorld(3)
	w.noCleanup = true
	w.stopMachine("prod-servers-1")
	var v *violation
	if err := w.apply(w.serverStep(rollout.TransferLeadership, "prod-servers-1")); !errors.As(err, &v) {
		t.Errorf("transfer to a stopped server = %v, want a violation", err)
	}
	unknown := rollout.Step{Action: rollout.TransferLeadership, Server: rollout.Server{ID: "r-99"}}
	if err := w.apply(unknown); err == nil || errors.As(err, &v) {
		t.Errorf("transfer to an unknown server = %v, want a plain error", err)
	}
}

func TestSimFailureTolerance(t *testing.T) {
	tests := []struct {
		voters int
		want   int
	}{{1, 0}, {2, 0}, {3, 1}, {4, 1}, {5, 2}}
	for _, tt := range tests {
		if got := serverWorld(tt.voters).observe().Nomad.FailureTolerance; got != tt.want {
			t.Errorf("failure tolerance with %d voters = %d, want %d", tt.voters, got, tt.want)
		}
	}
	w := serverWorld(3)
	w.noCleanup = true
	w.failServer("prod-servers-1")
	if got := w.observe().Nomad.FailureTolerance; got != 0 {
		t.Errorf("failure tolerance with 2 healthy of 3 voters = %d, want 0", got)
	}
}

func TestSimSummaryShowsServersAndMembers(t *testing.T) {
	a, b := serverWorld(3), serverWorld(3)
	if !slices.Equal(a.summary(), b.summary()) {
		t.Fatal("equal worlds have different summaries")
	}
	b.noCleanup = true
	b.failServer("prod-servers-1")
	if slices.Equal(a.summary(), b.summary()) {
		t.Error("the summary does not show a failed member")
	}
	c := serverWorld(3)
	if err := c.apply(c.serverStep(rollout.RemovePeer, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	if slices.Equal(a.summary(), c.summary()) {
		t.Error("the summary does not show a removed peer")
	}
	d := serverWorld(3)
	if err := d.apply(d.serverStep(rollout.TransferLeadership, "prod-servers-1")); err != nil {
		t.Fatal(err)
	}
	if slices.Equal(a.summary(), d.summary()) {
		t.Error("the summary does not show the leader")
	}
}

func TestSimServerCreateIgnoresTheVersionOfOlderServers(t *testing.T) {
	w := serverWorld(3)
	w.version = "2.0.8"
	create := rollout.Step{Action: rollout.Create, Group: "servers", Machine: rollout.Machine{
		Name: "prod-servers-3", Zone: "ams"}}
	if err := w.apply(create); err != nil {
		t.Errorf("creating a server that runs a newer Nomad than the others: %v", err)
	}
}

func TestSimAutopilotKeepsTheLeadersPeerWhateverItsMember(t *testing.T) {
	w := serverWorld(3)
	w.failServer("prod-servers-0")
	w.ticks(3)
	if got := len(w.observe().Nomad.Servers); got != 3 {
		t.Errorf("servers = %d, want 3: autopilot does not remove the leader", got)
	}
}

// combinedWorld is a cluster of n up to date combined machines of the group control, the first leading.
func combinedWorld(n int) *world {
	w := newWorld(curVersion)
	w.addGroup(combinedGroup(n))
	for range n {
		w.addCombined(newHash, curVersion)
	}
	return w
}

func TestSimCombinedMachinesHaveAServerAndANode(t *testing.T) {
	w := combinedWorld(3)
	s := w.observe()
	if len(s.Nomad.Servers) != 3 || len(s.Nomad.Members) != 3 || len(s.Nomad.Nodes) != 3 {
		t.Fatalf("Nomad = %+v, want 3 servers, 3 members and 3 nodes", s.Nomad)
	}
	for i, m := range s.Machines {
		n := s.Nomad.Nodes[i]
		if m.Role != v1alpha1.RoleCombined || m.Group != "control" || n.Name != m.Name || n.Address != m.PrivateIP ||
			n.Status != "ready" || !n.Eligible {
			t.Errorf("machine %+v and node %+v, want a combined machine and its ready, eligible node", m, n)
		}
	}
	if !s.Nomad.Servers[0].Leader {
		t.Error("the first combined machine does not lead")
	}
}

func TestSimNewCombinedNodeRegistersAtTwoTicksAndIsLabelledWhenItVotes(t *testing.T) {
	w := combinedWorld(3)
	create := rollout.Step{Action: rollout.Create, Group: "control", Machine: rollout.Machine{
		Name: "prod-control-3", Zone: "ams"}}
	if err := w.apply(create); err != nil {
		t.Fatal(err)
	}
	newest := func() rollout.Machine { return w.observe().Machines[3] }
	counts := func() (servers, nodes int) {
		n := w.observe().Nomad
		return len(n.Servers), len(n.Nodes)
	}
	if m := newest(); m.Role != v1alpha1.RoleCombined || m.Ready || m.Joined {
		t.Errorf("a created machine = %+v, want a combined machine, not ready, not joined", m)
	}
	if servers, nodes := counts(); servers != 3 || nodes != 3 {
		t.Errorf("after the create: %d servers and %d nodes, want 3 and 3", servers, nodes)
	}
	w.ticks(1)
	if servers, nodes := counts(); servers != 3 || nodes != 3 || !newest().Ready {
		t.Errorf("after 1 tick: %d servers and %d nodes, want 3 and 3", servers, nodes)
	}
	w.ticks(1)
	s := w.observe()
	if len(s.Nomad.Nodes) != 4 {
		t.Fatalf("after 2 ticks: %d nodes, want 4: the new node registers", len(s.Nomad.Nodes))
	}
	node := s.Nomad.Nodes[3]
	if srv := serverOf(t, s, "prod-control-3"); srv.Voter || !srv.Healthy || newest().Joined {
		t.Errorf("after 2 ticks: server %+v and machine %+v, want a healthy nonvoter, not labelled", srv, newest())
	}
	if node.Name != "prod-control-3" || node.Address != newest().PrivateIP || node.Status != "ready" || !node.Eligible {
		t.Errorf("after 2 ticks: node %+v, want it registered ready and eligible", node)
	}
	w.ticks(1)
	if got := len(w.observe().Nomad.Nodes); got != 4 || newest().Joined {
		t.Errorf("after 3 ticks: %d nodes, joined %t, want 4 nodes and not labelled", got, newest().Joined)
	}
	w.ticks(1)
	if srv := serverOf(t, w.observe(), "prod-control-3"); !srv.Voter || !newest().Joined {
		t.Errorf("after 4 ticks: server %+v and machine %+v, want a voter, labelled", srv, newest())
	}
	if got := len(w.observe().Nomad.Nodes); got != 4 {
		t.Errorf("after 4 ticks: %d nodes, want the one that registered at 2 ticks", got)
	}
}

func TestSimCombinedCreateIgnoresTheVersionOfOlderServers(t *testing.T) {
	w := combinedWorld(3)
	w.version = "2.0.8"
	create := rollout.Step{Action: rollout.Create, Group: "control", Machine: rollout.Machine{
		Name: "prod-control-3", Zone: "ams"}}
	if err := w.apply(create); err != nil {
		t.Errorf("creating a combined node that runs a newer Nomad than the others: %v", err)
	}
}

func TestSimDrainOfACombinedNodeMovesItsAllocations(t *testing.T) {
	w := combinedWorld(3)
	if err := w.apply(w.machineStep(rollout.Drain, "prod-control-1")); err != nil {
		t.Fatal(err)
	}
	w.ticks(2)
	if w.nodes[1].allocs != 0 || w.nodes[0].allocs != 2*allocsOfNode || w.unplaced != 0 {
		t.Errorf("allocations = %d, %d, unplaced %d, want 0, %d, 0", w.nodes[1].allocs, w.nodes[0].allocs, w.unplaced,
			2*allocsOfNode)
	}
}
