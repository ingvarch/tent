package rollout_test

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// The states of these tests have the combined group servers: every machine runs a server and a client, so it has a
// Raft server, a gossip member and a client node.

// combinedState is a cluster of size up to date combined nodes, the first leading: each is running, joined, voting,
// healthy and stable for an hour, and has a ready, eligible client node.
func combinedState(size int) rollout.State {
	s := serversState(size)
	s.Groups[0].Role = v1alpha1.RoleCombined
	s.Groups[0].DrainTimeout = time.Hour
	for i := range size {
		makeCombined(&s, i)
	}
	return s
}

// makeCombined turns the server machine i into a combined one with a client node.
func makeCombined(s *rollout.State, i int) {
	m := &s.Machines[i]
	m.Role = v1alpha1.RoleCombined
	s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(*m))
}

// addCombinedNode adds combined node i, up to date or not, with its server, member and client node.
func addCombinedNode(s *rollout.State, i int, hash string) {
	addServerNode(s, i, hash)
	makeCombined(s, len(s.Machines)-1)
}

// combinedMidRoll is a group of size 3 that rolls: nodes 0 to 2 are outdated, 0 leads, and node 3 is up to date and
// joined. The oldest outdated node that does not lead is node 1.
func combinedMidRoll() rollout.State {
	s := combinedState(3)
	for i := range 3 {
		s.Machines[i].SpecHash = oldHash
	}
	addCombinedNode(&s, 3, newHash)
	return s
}

// combinedLeaderRoll is combinedMidRoll with node 0, the leader, the only outdated node.
func combinedLeaderRoll() rollout.State {
	s := combinedMidRoll()
	s.Machines[1].SpecHash, s.Machines[2].SpecHash = newHash, newHash
	return s
}

// nodeIDOf is the ID of the client node of machine i.
func nodeIDOf(i int) string { return "n-" + machineIDOf(i) }

// serverDrained marks the node of machine i as ineligible and drained for it.
func serverDrained(t *testing.T, s *rollout.State, i int) {
	t.Helper()
	n := nodeNamed(t, s, serverName(i))
	n.Eligible, n.Draining, n.DrainedFor = false, false, machineIDOf(i)
}

func TestCombinedLinesBToDTakeTheVictimThroughItsDrain(t *testing.T) {
	// Node 1 is the victim of combinedMidRoll.
	node := func(t *testing.T, s *rollout.State) *rollout.Node { return nodeNamed(t, s, serverName(1)) }
	mark := serverOutcome{Action: rollout.MarkIneligible, Machine: serverName(1), Node: nodeIDOf(1)}
	drain := serverOutcome{Action: rollout.Drain, Machine: serverName(1), Node: nodeIDOf(1), Deadline: time.Hour}
	wait := serverOutcome{Action: rollout.WaitDrained, Machine: serverName(1), Node: nodeIDOf(1)}
	stop := serverOutcome{Action: rollout.Stop, Machine: serverName(1)}
	runServerCases(t, combinedMidRoll, []serverCase{
		{"b: the node is ready and eligible", func(*testing.T, *rollout.State) {}, mark},
		{"b: the machine is stopped and its node is still up", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
		}, mark},
		{"c: the node is ineligible", func(t *testing.T, s *rollout.State) {
			node(t, s).Eligible = false
		}, drain},
		{"c: the deadline is the group's", func(t *testing.T, s *rollout.State) {
			node(t, s).Eligible = false
			s.Groups[0].DrainTimeout = 90 * time.Minute
		}, serverOutcome{Action: rollout.Drain, Machine: serverName(1), Node: nodeIDOf(1), Deadline: 90 * time.Minute}},
		{"c: the node was drained for an earlier machine of the name", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.DrainedFor = false, "m-99"
		}, drain},
		{"c: the node is disconnected, which is not down", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Status = false, "disconnected"
		}, drain},
		{"d: the node is draining", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Draining = false, true
		}, wait},
		{"d: the node is draining and still eligible", func(t *testing.T, s *rollout.State) {
			node(t, s).Draining = true
		}, wait},
		{"d: the node is draining for the machine's earlier drain", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Draining, n.DrainedFor = false, true, machineIDOf(1)
		}, wait},
		{"g: the drain is complete", func(t *testing.T, s *rollout.State) { serverDrained(t, s, 1) }, stop},
		{"b: the node is eligible and carries the drain meta of its machine: a new drain, not a mark",
			func(t *testing.T, s *rollout.State) { node(t, s).DrainedFor = machineIDOf(1) }, drain},
		{"c: the node is ineligible and carries the drain meta of its machine: it counts as drained",
			func(t *testing.T, s *rollout.State) { serverDrained(t, s, 1) }, stop},
		{"c: an operator made the node eligible after its drain: it is drained again", func(t *testing.T,
			s *rollout.State) {
			serverDrained(t, s, 1)
			node(t, s).Eligible = true
		}, drain},
		{"g: the node is down while it drains: nothing to wait for", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Draining, n.Status = false, true, "down"
		}, stop},
		{"g: the node is down and ineligible, so nothing is left to drain", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Status = false, "down"
		}, stop},
		{"h: the machine is stopped and its node is down", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
			node(t, s).Status = "down"
		}, serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
		{"h: the machine is stopped and its node is gone", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
			dropNode(s, serverName(1))
		}, serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
	})
}

func TestCombinedDrainStartedVictimIsNotCheckedAtRestOrForTheWindowAgain(t *testing.T) {
	since := func(t *testing.T, s *rollout.State, i int, ago time.Duration) {
		t.Helper()
		serverNode(t, s, i).StableSince = s.Now.Add(-ago)
	}
	runServerCases(t, combinedMidRoll, []serverCase{
		{"before the drain the window decides", func(t *testing.T, s *rollout.State) {
			since(t, s, 3, 0)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(70 * time.Second)}},
		{"a drain that has begun does not wait for the window", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, serverName(1)).Eligible = false
			since(t, s, 3, 0)
		}, serverOutcome{Action: rollout.Drain, Machine: serverName(1), Node: nodeIDOf(1), Deadline: time.Hour}},
		{"a drained victim does not wait for the window", func(t *testing.T, s *rollout.State) {
			serverDrained(t, s, 1)
			since(t, s, 3, 0)
		}, serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
		{"a drain that has begun is not checked at rest again", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, serverName(1)).Eligible = false
			s.Nomad.Healthy = false
			serverNode(t, s, 3).Healthy = false
		}, serverOutcome{Action: rollout.Drain, Machine: serverName(1), Node: nodeIDOf(1), Deadline: time.Hour}},
	})
	t.Run("before the drain a machine that fails the checks refuses the roll", func(t *testing.T) {
		s := combinedMidRoll()
		s.Nomad.Healthy = false
		checkRefused(t, s, "node group servers: autopilot reports the servers unhealthy"+unhealthyAdvice)
	})
}

func TestCombinedDrainedVictimIsCheckedAgainBeforeItsServerLeavesTheQuorum(t *testing.T) {
	// The drain may last long and a run may resume days later, so another server can have failed since the checks.
	unhealthy := func(t *testing.T, s *rollout.State) {
		t.Helper()
		s.Nomad.Healthy = false
		serverNode(t, s, 2).Healthy = false
	}
	failures := []refusalCase{
		{"autopilot reports a server unhealthy", unhealthy,
			"node group servers: autopilot reports the servers unhealthy (prod-servers-2)" + unhealthyAdvice},
		{"a machine of the group stopped and autopilot has not noticed", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 3)
		}, "node group servers: node prod-servers-3 is not running" + restAdvice},
		{"a server of the group does not vote", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 3).Voter = false
		}, "node group servers: node prod-servers-3 is not a voting server" + restAdvice},
		{"a server of the group is not in the Raft configuration", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 3)
		}, "node group servers: node prod-servers-3 is not a server in the Raft configuration" + restAdvice},
		{"the cluster can lose no voter", func(_ *testing.T, s *rollout.State) {
			s.Nomad.FailureTolerance = 0
		}, "node group servers: the servers can lose no voter (failure tolerance 0); tent removes a server only from " +
			"a cluster that can lose one"},
		{"autopilot comes first", func(t *testing.T, s *rollout.State) {
			unhealthy(t, s)
			stopServer(t, s, 3)
		}, "node group servers: autopilot reports the servers unhealthy (prod-servers-2)" + unhealthyAdvice},
	}
	t.Run("before the stop", func(t *testing.T) {
		runRefusalCases(t, func() rollout.State {
			s := combinedMidRoll()
			serverDrained(t, &s, 1)
			return s
		}, failures)
	})
	t.Run("before the leadership moves", func(t *testing.T) {
		runRefusalCases(t, func() rollout.State {
			s := combinedLeaderRoll()
			serverDrained(t, &s, 0)
			return s
		}, failures)
	})
	t.Run("three voters need a failure tolerance too", func(t *testing.T) {
		s := combinedState(3)
		s.Groups[0].Size = 1
		serverDrained(t, &s, 1)
		s.Nomad.FailureTolerance = 0
		checkRefusedIn(t, rollout.Shrink, s, "node group servers: the servers can lose no voter (failure tolerance 0); "+
			"tent removes a server only from a cluster that can lose one")
	})
	t.Run("a shrink takes the same care", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 1)
		unhealthy(t, &s)
		checkRefusedIn(t, rollout.Shrink, s, "node group servers: autopilot reports the servers unhealthy "+
			"(prod-servers-2); tent removes a server only while every server is healthy")
	})
	t.Run("the drain itself goes on while a server is down", func(t *testing.T) {
		s := combinedMidRoll()
		nodeNamed(t, &s, serverName(1)).Eligible = false
		unhealthy(t, &s)
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Drain, Machine: serverName(1),
			Node: nodeIDOf(1), Deadline: time.Hour})
	})
	t.Run("a victim whose server does not vote is stopped whatever the others do", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 1)
		serverNode(t, &s, 1).Voter = false
		unhealthy(t, &s)
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
	})
}

func TestCombinedEligibleNodeWithItsDrainMetaHasNotStarted(t *testing.T) {
	// An operator made the node eligible after its drain, so its removal starts again from the beginning.
	meta := func(t *testing.T, s *rollout.State, i int) {
		t.Helper()
		nodeNamed(t, s, serverName(i)).DrainedFor = machineIDOf(i)
	}
	runRefusalCases(t, combinedMidRoll, []refusalCase{
		{"the victim is checked at rest", func(t *testing.T, s *rollout.State) {
			meta(t, s, 1)
			s.Nomad.Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy" + unhealthyAdvice},
	})
	runServerCases(t, combinedMidRoll, []serverCase{
		{"the victim waits for the window", func(t *testing.T, s *rollout.State) {
			meta(t, s, 1)
			serverNode(t, s, 3).StableSince = s.Now
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(70 * time.Second)}},
		{"the node does not come first in the victim order", func(t *testing.T, s *rollout.State) {
			meta(t, s, 2)
		}, serverOutcome{Action: rollout.MarkIneligible, Machine: serverName(1), Node: nodeIDOf(1)}},
	})
}

func TestCombinedLeaderIsDrainedBeforeItsLeadershipMoves(t *testing.T) {
	// Node 0 leads and is the only outdated node; 1, 2 and 3 are up to date.
	node := func(t *testing.T, s *rollout.State) *rollout.Node { return nodeNamed(t, s, serverName(0)) }
	step := func(action rollout.Action) serverOutcome {
		return serverOutcome{Action: action, Machine: serverName(0), Node: nodeIDOf(0)}
	}
	transfer := serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: "r-2"}
	runServerCases(t, combinedLeaderRoll, []serverCase{
		{"it is marked first", func(*testing.T, *rollout.State) {}, step(rollout.MarkIneligible)},
		{"then drained", func(t *testing.T, s *rollout.State) { node(t, s).Eligible = false }, serverOutcome{
			Action: rollout.Drain, Machine: serverName(0), Node: nodeIDOf(0), Deadline: time.Hour}},
		{"the drain is waited for while it leads", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Draining = false, true
		}, step(rollout.WaitDrained)},
		{"the leadership moves after the drain", func(t *testing.T, s *rollout.State) {
			serverDrained(t, s, 0)
		}, transfer},
		{"the move does not wait for the window", func(t *testing.T, s *rollout.State) {
			serverDrained(t, s, 0)
			serverNode(t, s, 3).StableSince = s.Now
		}, transfer},
		{"a leader whose node is down hands over at once", func(t *testing.T, s *rollout.State) {
			n := node(t, s)
			n.Eligible, n.Status = false, "down"
		}, transfer},
		{"after the move the machine is stopped, with no second window", func(t *testing.T, s *rollout.State) {
			serverDrained(t, s, 0)
			serverNode(t, s, 0).Leader = false
			serverNode(t, s, 1).Leader = true
			for i := range s.Nomad.Servers {
				s.Nomad.Servers[i].StableSince = s.Now
			}
		}, serverOutcome{Action: rollout.Stop, Machine: serverName(0)}},
	})
}

func TestCombinedLeaderHandsOverToAnUpToDateVoter(t *testing.T) {
	transfer := func(to string) serverOutcome {
		return serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: to}
	}
	runServerCases(t, combinedMidRoll, []serverCase{
		{"an outdated server is passed over", func(t *testing.T, s *rollout.State) {
			serverDrained(t, s, 0)
		}, transfer("r-4")},
	})
	t.Run("no healthy up to date voter refuses", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 0)
		serverNode(t, &s, 3).Healthy = false
		checkRefused(t, s, "node group servers: prod-servers-0 leads, and no healthy voter of the group that is up to date "+
			"can take the leadership")
	})
}

func TestCombinedChecksAtRestLookAtTheNodes(t *testing.T) {
	// target is an up to date node that no rule picks.
	places := []struct {
		name   string
		base   func() rollout.State
		target int
	}{
		{"before a victim starts", combinedMidRoll, 3},
		{"before a node is created", func() rollout.State {
			s := combinedState(3)
			s.Machines[2].SpecHash = oldHash
			return s
		}, 1},
	}
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State, target int)
		want  string
	}{
		{"its node is down", func(t *testing.T, s *rollout.State, i int) {
			nodeNamed(t, s, serverName(i)).Status = "down"
		}, "node group servers: node %s is down in Nomad" + restAdvice},
		{"its node is not eligible", func(t *testing.T, s *rollout.State, i int) {
			nodeNamed(t, s, serverName(i)).Eligible = false
		}, "node group servers: node %s is not eligible" + restAdvice},
		{"its node is draining", func(t *testing.T, s *rollout.State, i int) {
			nodeNamed(t, s, serverName(i)).Draining = true
		}, "node group servers: node %s is draining" + restAdvice},
		{"it has no node", func(_ *testing.T, s *rollout.State, i int) {
			dropNode(s, serverName(i))
		}, "node group servers: node %s has no node in Nomad" + restAdvice},
		{"the checks of the server come first", func(t *testing.T, s *rollout.State, i int) {
			serverNode(t, s, i).Voter = false
			nodeNamed(t, s, serverName(i)).Status = "down"
		}, "node group servers: node %s is not a voting server" + restAdvice},
	}
	for _, place := range places {
		for _, tt := range tests {
			t.Run(tt.name+" "+place.name, func(t *testing.T) {
				s := place.base()
				tt.build(t, &s, place.target)
				checkRefused(t, s, fmt.Sprintf(tt.want, serverName(place.target)))
			})
		}
	}
	t.Run("a server group does not look at nodes", func(t *testing.T) {
		s := midRoll()
		for _, m := range s.Machines {
			n := nodeOf(m)
			n.Status = "down"
			s.Nomad.Nodes = append(s.Nomad.Nodes, n)
		}
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
	})
	t.Run("an ineligible node does not start the removal of a server", func(t *testing.T) {
		s := midRoll()
		for _, m := range s.Machines {
			n := nodeOf(m)
			n.Eligible = false
			s.Nomad.Nodes = append(s.Nomad.Nodes, n)
		}
		s.Nomad.Healthy = false
		checkRefused(t, s, "node group servers: autopilot reports the servers unhealthy"+unhealthyAdvice)
	})
}

func TestCombinedRuleS3PurgesTheNodesThatNoMachineHas(t *testing.T) {
	const gone = "n-gone-" // the ID prefix that addOrphan gives
	purge := func(name string) serverOutcome {
		return serverOutcome{Action: rollout.Purge, Node: gone + name}
	}
	wait := func(name string) serverOutcome {
		return serverOutcome{Action: rollout.WaitNodeDown, Node: gone + name}
	}
	runServerCases(t, func() rollout.State { return combinedState(3) }, []serverCase{
		{"a down node is purged", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, serverName(7), "down", 50)
		}, purge(serverName(7))},
		{"a node that is not down is waited for", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, serverName(7), "ready", 50)
		}, wait(serverName(7))},
		{"a down node is purged before one that is not down is waited for", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, serverName(8), "ready", 51)
			addOrphan(s, serverName(7), "down", 50)
		}, purge(serverName(7))},
		{"the first orphan by name is waited for", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, serverName(9), "ready", 52)
			addOrphan(s, serverName(8), "ready", 51)
		}, wait(serverName(8))},
		{"it comes before the refusal of a group short of its size", func(t *testing.T, s *rollout.State) {
			s.Machines = s.Machines[:2]
			nodeNamed(t, s, serverName(2)).Status = "down"
		}, serverOutcome{Action: rollout.Purge, Node: nodeIDOf(2)}},
		{"it comes before a node is created", func(_ *testing.T, s *rollout.State) {
			s.Machines[2].SpecHash = oldHash
			addOrphan(s, serverName(7), "down", 50)
		}, purge(serverName(7))},
	})
	runServerCases(t, combinedMidRoll, []serverCase{
		{"a victim comes before it", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, serverName(7), "down", 50)
		}, serverOutcome{Action: rollout.MarkIneligible, Machine: serverName(1), Node: nodeIDOf(1)}},
		{"a machine that has not joined comes before it", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(3)).Joined = false
			addOrphan(s, serverName(7), "down", 50)
		}, serverOutcome{Action: rollout.WaitJoined, Machine: serverName(3)}},
	})
	t.Run("a node that a machine has is no orphan", func(t *testing.T) {
		s := combinedState(3)
		nodeNamed(t, &s, serverName(1)).Status = "down"
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
	})
	t.Run("a machine without an address shields the node of its name", func(t *testing.T) {
		s := combinedState(3)
		machineOf(t, &s, serverName(1)).PrivateIP = netip.Addr{}
		nodeNamed(t, &s, serverName(1)).Status = "down"
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
	})
	t.Run("a node of another group is no orphan of this one", func(t *testing.T) {
		s := combinedState(3)
		addOrphan(&s, workerName(7), "down", 50)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
	})
	t.Run("a server group has no nodes to purge", func(t *testing.T) {
		s := serversState(3)
		addOrphan(&s, serverName(7), "down", 50)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
	})
}

func TestCombinedGroupRollsLikeAServerGroup(t *testing.T) {
	t.Run("a new node is a combined node", func(t *testing.T) {
		s := combinedState(3)
		s.Machines[0].SpecHash = oldHash
		step := nextRoll(t, s)
		checkOutcome(t, step, outcome{Action: rollout.Create, Group: "servers", Machine: serverName(3), Zone: "ams"})
		if step.Machine.Role != v1alpha1.RoleCombined {
			t.Errorf("new machine has the role %q, want %q", step.Machine.Role, v1alpha1.RoleCombined)
		}
		if got, want := step.String(), "create node prod-servers-3 (combined of servers, ams)"; got != want {
			t.Errorf("step text = %q, want %q", got, want)
		}
	})
	t.Run("a group at its size with nothing outdated is done", func(t *testing.T) {
		checkOutcome(t, nextRoll(t, combinedState(3)), outcome{Action: rollout.Done})
	})
	t.Run("a group short of its size is refused", func(t *testing.T) {
		s := combinedState(3)
		s.Machines = s.Machines[:2]
		dropNode(&s, serverName(2))
		checkRefused(t, s, "node group servers: it has 2 of its 3 nodes; run tent update cluster first")
	})
	t.Run("a group of one node cannot roll", func(t *testing.T) {
		s := combinedState(1)
		s.Machines[0].SpecHash = oldHash
		s.Nomad.FailureTolerance = 0
		checkRefused(t, s, "node group servers: a group of one server cannot roll: its failure tolerance is 0")
	})
	t.Run("a drained victim is refused when two voters would be left", func(t *testing.T) {
		s := combinedState(1)
		addCombinedNode(&s, 1, oldHash)
		serverDrained(t, &s, 1)
		checkRefused(t, s, "node group servers: removing prod-servers-1 would leave one voter of two: tent does not "+
			"take a group from two voters to one yet")
	})
	t.Run("with two voters the failure tolerance is 0, and the refusal says why it is two", func(t *testing.T) {
		s := combinedState(1)
		addCombinedNode(&s, 1, oldHash)
		serverDrained(t, &s, 1)
		s.Nomad.FailureTolerance = 0
		checkRefused(t, s, "node group servers: removing prod-servers-1 would leave one voter of two: tent does not "+
			"take a group from two voters to one yet")
	})
}

func TestShrinkOfACombinedGroupGivesAnEligibleNodeWithItsDrainMetaANewDrain(t *testing.T) {
	s := combinedState(3)
	s.Groups[0].Size = 2
	nodeNamed(t, &s, serverName(2)).DrainedFor = machineIDOf(2)
	step := nextIn(t, rollout.Shrink, s)
	checkServerStep(t, step, serverOutcome{Action: rollout.Drain, Machine: serverName(2), Node: nodeIDOf(2),
		Deadline: time.Hour})
}
