package rollout_test

import (
	"slices"
	"testing"

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
	w.addServer(curVersion)
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

func TestSimServersAreHealthyAndTheFirstLeads(t *testing.T) {
	w := newWorld(curVersion)
	w.addServer(curVersion)
	w.addServer(curVersion)
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
