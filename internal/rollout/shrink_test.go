package rollout_test

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// The states of these tests are run in the mode Shrink. A client state is baseState (the group workers, size 2, with
// a healthy server) with up to date workers; a server state is serversState of the given size, whose group is cut to a
// smaller size.

// shrinkClientsState is baseState with n up to date workers (worker i is Machines[i+1] and Nodes[i]) and a budget of
// maxUnavailable nodes.
func shrinkClientsState(n, maxUnavailable int) rollout.State {
	s := baseState()
	s.Groups[0].MaxUnavailable = maxUnavailable
	for i := range n {
		addWorker(&s, i, newHash)
	}
	return s
}

// shrinkServersState is serversState of n servers for a group of the given size.
func shrinkServersState(n, size int) rollout.State {
	s := serversState(n)
	s.Groups[0].Size = size
	return s
}

// markOf is the step that marks worker i ineligible; the machine is m-<i+1>, in ams unless the test says so.
func markOf(i int, zone string) outcome {
	id := machineIDOf(i)
	return outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(i), MachineID: id,
		Node: "n-" + id, Zone: zone}
}

// clientCase is a client state and the step that Shrink takes in it.
type clientCase struct {
	name  string
	build func(t *testing.T, s *rollout.State)
	want  outcome
}

func runShrinkClientCases(t *testing.T, base func() rollout.State, tests []clientCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.build(t, &s)
			checkOutcome(t, nextIn(t, rollout.Shrink, s), tt.want)
		})
	}
}

func TestShrinkClientVictimsAreChosenInOrder(t *testing.T) {
	// Three workers for size 2 and a budget that never decides: one victim. Worker 2 is the newest.
	base := func() rollout.State { return shrinkClientsState(3, 5) }
	runShrinkClientCases(t, base, []clientCase{
		{"the newest", func(*testing.T, *rollout.State) {}, markOf(2, "ams")},
		{"the highest ID when the ages are equal", func(_ *testing.T, s *rollout.State) {
			for i := range s.Machines {
				s.Machines[i].Created = epoch
			}
			s.Machines[1].ID = "m-9" // worker 0 is first by name and has the highest ID
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0), MachineID: "m-9",
			Node: "n-m-1", Zone: "ams"}},
		{"an unknown age counts as the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].Created = time.Time{}
		}, markOf(0, "ams")},
		{"in the zone with the most machines, before the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[3].Zone = "fra"
		}, markOf(1, "ams")},
		{"one that is not available, before the zone and the newest", func(t *testing.T, s *rollout.State) {
			s.Machines[3].Zone = "fra"
			setNodeStatus(t, s, 0, "disconnected")
		}, markOf(0, "ams")},
		{"an outdated one, before one that is not available", func(t *testing.T, s *rollout.State) {
			s.Machines[2].SpecHash = oldHash
			setNodeStatus(t, s, 0, "disconnected")
		}, markOf(1, "ams")},
		{"an outdated one, before the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash = oldHash
		}, markOf(0, "ams")},
	})
}

func TestShrinkMachinesThatNeverJoinedGoFirst(t *testing.T) {
	// Worker 0 is the oldest and has not joined; worker 2 is the newest.
	unjoined := func(t *testing.T, s *rollout.State) {
		t.Helper()
		machineOf(t, s, workerName(0)).Joined = false
	}
	deleteOf := outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Zone: "ams"}
	waitOf := outcome{Action: rollout.WaitJoined, Group: "workers", Machine: workerName(0), MachineID: "m-1", Zone: "ams"}
	base := func() rollout.State { return shrinkClientsState(3, 5) }
	runShrinkClientCases(t, base, []clientCase{
		{"one that Nomad does not list is deleted at once", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			dropNode(s, workerName(0))
		}, deleteOf},
		{"one that Nomad lists is waited for", unjoined, waitOf},
		{"one without an address that Nomad lists by name is waited for", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			machineOf(t, s, workerName(0)).PrivateIP = netip.Addr{}
		}, waitOf},
		{"one without an address that Nomad does not list is deleted", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			machineOf(t, s, workerName(0)).PrivateIP = netip.Addr{}
			dropNode(s, workerName(0))
		}, deleteOf},
		{"it goes before a joined node that is not available", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			dropNode(s, workerName(0))
			setNodeStatus(t, s, 2, "disconnected")
		}, deleteOf},
		{"the newest of two is deleted", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			machineOf(t, s, workerName(1)).Joined = false
			dropNode(s, workerName(0))
			dropNode(s, workerName(1))
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(1), MachineID: "m-2", Zone: "ams"}},
		{"a group at its size waits instead of deleting", func(t *testing.T, s *rollout.State) {
			unjoined(t, s)
			dropNode(s, workerName(0))
			s.Groups[0].Size = 3
		}, waitOf},
		{"a removal under way is finished first", func(t *testing.T, s *rollout.State) {
			addWorker(s, 3, newHash)
			unjoined(t, s)
			dropNode(s, workerName(0))
			nodeNamed(t, s, workerName(3)).Eligible = false
		}, outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(3), MachineID: "m-4", Node: "n-m-4",
			Zone: "ams", Deadline: time.Hour}},
	})
}

func TestShrinkTakesTheWholeBatchOutBeforeDrainingAny(t *testing.T) {
	base := func() rollout.State { return shrinkClientsState(4, 0) }
	drainOf := func(i int) outcome {
		id := machineIDOf(i)
		return outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(i), MachineID: id, Node: "n-" + id,
			Zone: "ams", Deadline: time.Hour}
	}
	runShrinkClientCases(t, base, []clientCase{
		{"the newest is marked", func(*testing.T, *rollout.State) {}, markOf(3, "ams")},
		{"the next newest is marked", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(3)).Eligible = false
		}, markOf(2, "ams")},
		{"no third node goes; the first by name is drained", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(3)).Eligible = false
			nodeNamed(t, s, workerName(2)).Eligible = false
		}, drainOf(2)},
		{"the second is drained while the first drains", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(3)).Eligible = false
			drainedFor(t, s, 2, "", true)
		}, drainOf(3)},
		{"both drain: wait for the first by name", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 3, "", true)
			drainedFor(t, s, 2, "", true)
		}, outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(2), MachineID: "m-3",
			Node: "n-m-3", Zone: "ams"}},
		{"a drained machine is deleted while the other drains", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 3, "m-4", false)
			drainedFor(t, s, 2, "", true)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(3), MachineID: "m-4", Node: "n-m-4",
			Zone: "ams"}},
	})
}

func TestShrinkRemovesAnIneligibleNodeOnlyFromAGroupAboveItsSize(t *testing.T) {
	ineligible := func(t *testing.T, s *rollout.State) { nodeNamed(t, s, workerName(1)).Eligible = false }
	runShrinkClientCases(t, func() rollout.State { return shrinkClientsState(3, 0) }, []clientCase{
		{"above its size, up to date as it is", ineligible, outcome{Action: rollout.Drain, Group: "workers",
			Machine: workerName(1), MachineID: "m-2", Node: "n-m-2", Zone: "ams", Deadline: time.Hour}},
		{"above its size, and its node is down", func(t *testing.T, s *rollout.State) {
			ineligible(t, s)
			setNodeStatus(t, s, 1, "down")
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(1), MachineID: "m-2", Node: "n-m-2",
			Zone: "ams"}},
	})
	runShrinkClientCases(t, func() rollout.State { return shrinkClientsState(2, 0) }, []clientCase{
		{"at its size", ineligible, outcome{Action: rollout.Done}},
	})
	// With a budget to spare, a node that is leaving still uses up the surplus: no second node is marked.
	runShrinkClientCases(t, func() rollout.State { return shrinkClientsState(3, 5) }, []clientCase{
		{"it is the only one that goes", ineligible, outcome{Action: rollout.Drain, Group: "workers",
			Machine: workerName(1), MachineID: "m-2", Node: "n-m-2", Zone: "ams", Deadline: time.Hour}},
	})
}

func TestShrinkRespectsTheBudgetOfUnavailableNodes(t *testing.T) {
	// Five workers for size 3; workers 3 and 4 are joined and the cloud reports no address for them, so they are no
	// victims and not available.
	base := func(maxUnavailable int) rollout.State {
		s := shrinkClientsState(3, maxUnavailable)
		s.Groups[0].Size = 3
		for i := 3; i < 5; i++ {
			m := workerMachine(i, newHash)
			m.PrivateIP = netip.Addr{}
			s.Machines = append(s.Machines, m)
		}
		return s
	}
	t.Run("no node can go when the others are needed", func(t *testing.T) {
		checkRefusedIn(t, rollout.Shrink, base(0), "node group workers: cannot go on: prod-workers-3 has no node in "+
			"Nomad: the cloud reports no private address for it and prod-workers-4 has no node in Nomad: the cloud "+
			"reports no private address for it; with maxUnavailable 0 no surplus node can be removed")
	})
	t.Run("a node goes when the budget allows", func(t *testing.T) {
		checkOutcome(t, nextIn(t, rollout.Shrink, base(1)), markOf(2, "ams"))
	})
}

func TestShrinkLeavesAGroupAtOrBelowItsSizeDone(t *testing.T) {
	done := outcome{Action: rollout.Done}
	runShrinkClientCases(t, func() rollout.State { return shrinkClientsState(2, 0) }, []clientCase{
		{"at its size", func(*testing.T, *rollout.State) {}, done},
		{"at its size with outdated machines, which only a roll replaces", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash, s.Machines[2].SpecHash = oldHash, oldHash
		}, done},
		{"below its size, which only update fills", func(_ *testing.T, s *rollout.State) {
			s.Groups[0].Size = 5
		}, done},
	})
	runShrinkClientCases(t, func() rollout.State { return shrinkClientsState(0, 0) }, []clientCase{
		{"empty", func(*testing.T, *rollout.State) {}, done},
	})
}

func TestShrinkIgnoresTheNomadVersions(t *testing.T) {
	// A roll refuses all of these; a shrink creates no node and so cares for none of them.
	for name, edit := range map[string]func(s *rollout.State){
		"a server runs an older Nomad than a new node would": func(s *rollout.State) { s.Version = "2.0.9" },
		"a node runs a newer Nomad than a new node would":    func(s *rollout.State) { s.Version = "2.0.1" },
		"the version of a new node is not a version":         func(s *rollout.State) { s.Version = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := shrinkClientsState(3, 0)
			edit(&s)
			checkOutcome(t, nextIn(t, rollout.Shrink, s), markOf(2, "ams"))
		})
	}
}

func TestShrinkStillRefusesDuplicatesAndOrdersGroups(t *testing.T) {
	t.Run("two machines of one name", func(t *testing.T) {
		s := shrinkClientsState(3, 0)
		twin := s.Machines[2]
		twin.ID = "m-77"
		s.Machines = append(s.Machines, twin)
		checkRefusedIn(t, rollout.Shrink, s, "node group workers: machines m-2 and m-77 share the name "+
			"prod-workers-1; run tent update cluster first")
	})
	t.Run("the server group comes before the clients", func(t *testing.T) {
		s := shrinkServersState(3, 2)
		s.Groups[0].Name = "zeta"
		for i := range s.Machines {
			s.Machines[i].Group = "zeta"
		}
		s.Groups = append(s.Groups, rollout.Group{Name: "alpha", Role: v1alpha1.RoleClient, Size: 1,
			Zones: []string{"ams"}, SpecHash: newHash, DrainTimeout: time.Hour})
		for i := range 2 {
			w := workerMachine(i, newHash)
			w.ID, w.Group, w.Name = fmt.Sprintf("m-a%d", i), "alpha", fmt.Sprintf("prod-alpha-%d", i)
			s.Machines = append(s.Machines, w)
			s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(w))
		}
		checkServerStep(t, nextIn(t, rollout.Shrink, s), serverOutcome{Action: rollout.Stop, Machine: serverName(2)})
	})
}

func TestShrinkServerVictimsAreChosenInOrder(t *testing.T) {
	stop := func(i int) serverOutcome { return serverOutcome{Action: rollout.Stop, Machine: serverName(i)} }
	// Servers 0 (leader, ams), 1 (fra), 2 (ams), 3 (fra), 4 (ams) for size 3: ams has three servers and 4 is the newest.
	base := func() rollout.State { return shrinkServersState(5, 3) }
	runServerCasesIn(t, rollout.Shrink, base, []serverCase{
		{"the newest in the zone with the most servers", func(*testing.T, *rollout.State) {}, stop(4)},
		{"the leader goes last", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 0).Leader, serverNode(t, s, 4).Leader = false, true
		}, stop(2)},
		{"the zone with the most servers, before the newest", func(t *testing.T, s *rollout.State) {
			for _, i := range []int{1, 3} {
				machineOf(t, s, serverName(i)).Zone = "ams"
			}
			machineOf(t, s, serverName(4)).Zone = "fra"
		}, stop(3)},
		{"an outdated server, before the newest", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(1)).SpecHash = oldHash
		}, stop(1)},
		{"the highest ID when the ages are equal", func(t *testing.T, s *rollout.State) {
			for i := range 5 {
				machineOf(t, s, serverName(i)).Created = epoch
			}
			machineOf(t, s, serverName(2)).ID = "m-9"
		}, stop(2)},
		{"an unknown age counts as the newest", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(2)).Created = time.Time{}
		}, stop(2)},
		{"a server whose removal has started goes before all others", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
		}, serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
		{"of two started, the one that autopilot does not count healthy goes first", func(t *testing.T,
			s *rollout.State) {
			stopServer(t, s, 1)
			stopServer(t, s, 2)
			serverNode(t, s, 2).Healthy = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(2), Server: "r-3"}},
	})
}

func TestShrinkServerGroupsNeedNoRollChecks(t *testing.T) {
	t.Run("a group at its size is done, also with outdated servers", func(t *testing.T) {
		s := shrinkServersState(3, 3)
		s.Machines[1].SpecHash = oldHash
		checkOutcome(t, nextIn(t, rollout.Shrink, s), outcome{Action: rollout.Done})
	})
	t.Run("a group short of its size is done, not refused", func(t *testing.T) {
		checkOutcome(t, nextIn(t, rollout.Shrink, shrinkServersState(2, 3)), outcome{Action: rollout.Done})
	})
	stop := serverOutcome{Action: rollout.Stop, Machine: serverName(2)}
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(3, 2) }, []serverCase{
		{"no failure tolerance is needed to remove a server", func(_ *testing.T, s *rollout.State) {
			s.Nomad.FailureTolerance = 0
		}, stop},
		{"the Nomad versions do not matter", func(_ *testing.T, s *rollout.State) { s.Version = "" }, stop},
	})
}

// shrinkUnhealthyAdvice ends the refusal of a shrink while autopilot reports unhealthy servers.
const shrinkUnhealthyAdvice = "; tent removes a server only while every server is healthy"

func TestShrinkServerChecksAtRestAndTheWindowStillApply(t *testing.T) {
	base := func() rollout.State { return shrinkServersState(3, 2) }
	runRefusalCasesIn(t, rollout.Shrink, base, []refusalCase{
		{"autopilot reports unhealthy servers", func(_ *testing.T, s *rollout.State) { s.Nomad.Healthy = false },
			"node group servers: autopilot reports the servers unhealthy" + shrinkUnhealthyAdvice},
		{"a server that is not healthy is the victim and is not stopped", func(t *testing.T, s *rollout.State) {
			s.Nomad.Healthy = false
			serverNode(t, s, 1).Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy (prod-servers-1)" + shrinkUnhealthyAdvice},
	})
	runServerCasesIn(t, rollout.Shrink, base, []serverCase{
		{"a server changed 10 seconds ago", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 0).StableSince = epoch.Add(-10 * time.Second)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(2), Until: epoch.Add(time.Minute)}},
	})
}

func TestShrinkTakesAGroupFromThreeServersToTwoAndFromTwoToOne(t *testing.T) {
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(3, 1) }, []serverCase{
		{"3 to 2 starts with the stop of a server", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.Stop, Machine: serverName(2)}},
	})
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(2, 1) }, []serverCase{
		{"2 to 1 removes the peer while the server runs", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
	})
}

func TestShrinkMachinesOfAServerGroupThatNeverJoined(t *testing.T) {
	// Servers 0 to 4 for size 3: two machines to spare. Server 4 has not joined and is the newest.
	unjoined := func(t *testing.T, s *rollout.State, i int) {
		t.Helper()
		machineOf(t, s, serverName(i)).Joined = false
	}
	unknown := func(t *testing.T, s *rollout.State, i int) {
		t.Helper()
		unjoined(t, s, i)
		dropPeerAndMember(t, s, i)
	}
	deleteOf := func(i int) serverOutcome { return serverOutcome{Action: rollout.Delete, Machine: serverName(i)} }
	waitOf := func(i int) serverOutcome { return serverOutcome{Action: rollout.WaitJoined, Machine: serverName(i)} }
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(5, 3) }, []serverCase{
		{"one that Nomad does not list is deleted at once", func(t *testing.T, s *rollout.State) { unknown(t, s, 4) },
			deleteOf(4)},
		{"it goes before a victim that has joined", func(t *testing.T, s *rollout.State) { unknown(t, s, 0) },
			deleteOf(0)},
		{"one whose server Nomad lists is waited for", func(t *testing.T, s *rollout.State) {
			unjoined(t, s, 4)
			serverNode(t, s, 4).Voter = false
		}, waitOf(4)},
		{"the newest of two is deleted", func(t *testing.T, s *rollout.State) {
			unknown(t, s, 3)
			unknown(t, s, 4)
		}, deleteOf(4)},
		{"one without an address whose server Nomad lists by name is waited for", func(t *testing.T,
			s *rollout.State) {
			unjoined(t, s, 4)
			machineOf(t, s, serverName(4)).PrivateIP = netip.Addr{}
		}, waitOf(4)},
		{"one without an address that Nomad does not list is deleted", func(t *testing.T, s *rollout.State) {
			unknown(t, s, 4)
			machineOf(t, s, serverName(4)).PrivateIP = netip.Addr{}
		}, deleteOf(4)},
		{"a removal under way leaves one to spare", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
			unknown(t, s, 4)
		}, deleteOf(4)},
	})
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(4, 3) }, []serverCase{
		{"a group one above its size deletes it", func(t *testing.T, s *rollout.State) { unknown(t, s, 3) }, deleteOf(3)},
		{"a machine that does not run yet is no removal under way", func(t *testing.T, s *rollout.State) {
			unknown(t, s, 3)
			machineOf(t, s, serverName(3)).Ready = false
		}, deleteOf(3)},
		{"a removal under way uses up the surplus, so it is waited for", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
			unknown(t, s, 3)
		}, waitOf(3)},
	})
	runServerCasesIn(t, rollout.Shrink, func() rollout.State { return shrinkServersState(3, 3) }, []serverCase{
		{"a group at its size waits instead of deleting", func(t *testing.T, s *rollout.State) { unknown(t, s, 2) },
			waitOf(2)},
	})
	combined := func() rollout.State {
		s := combinedState(3)
		addCombinedNode(&s, 3, newHash)
		return s
	}
	runServerCasesIn(t, rollout.Shrink, combined, []serverCase{
		{"a combined machine whose node Nomad lists is waited for", func(t *testing.T, s *rollout.State) {
			unknown(t, s, 3)
			s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(*machineOf(t, s, serverName(3))))
		}, waitOf(3)},
		{"a combined machine that Nomad lists nowhere is deleted", func(t *testing.T, s *rollout.State) {
			unknown(t, s, 3)
			dropNode(s, serverName(3))
		}, deleteOf(3)},
	})
}

func TestShrinkLeadershipMayGoToAnyHealthyVoter(t *testing.T) {
	// Combined machine 0 leads and was drained, so its removal has started. Machines 1 to 3 are outdated, which a roll
	// would not take as the new leader.
	base := combinedMidRoll
	drained := func(t *testing.T, s *rollout.State) { serverDrained(t, s, 0) }
	runServerCasesIn(t, rollout.Shrink, base, []serverCase{
		{"an outdated voter takes it", drained,
			serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: "r-2"}},
		{"one that autopilot does not count healthy does not", func(t *testing.T, s *rollout.State) {
			drained(t, s)
			serverNode(t, s, 1).Healthy = false
		}, serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: "r-3"}},
	})
	t.Run("a refusal when no other voter is healthy", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 0)
		for i := 1; i <= 3; i++ {
			serverNode(t, &s, i).Healthy = false
		}
		checkRefusedIn(t, rollout.Shrink, s, "node group servers: prod-servers-0 leads, and no other healthy voter of "+
			"the group can take the leadership")
	})
}

func TestShrinkDrainsOnlyAsManyIneligibleNodesAsTheGroupMustLose(t *testing.T) {
	base := func() rollout.State {
		s := shrinkClientsState(3, 0)
		nodeNamed(t, &s, workerName(0)).Eligible = false
		nodeNamed(t, &s, workerName(1)).Eligible = false
		return s
	}
	runShrinkClientCases(t, base, []clientCase{
		{"the newest of them is drained", func(*testing.T, *rollout.State) {}, outcome{Action: rollout.Drain,
			Group: "workers", Machine: workerName(1), MachineID: "m-2", Node: "n-m-2", Zone: "ams",
			Deadline: time.Hour}},
		{"the one that drained is deleted", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 1, "m-2", false)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(1), MachineID: "m-2", Node: "n-m-2",
			Zone: "ams"}},
		{"the one that drains is waited for", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 1, "", true)
		}, outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "ams"}},
		{"one that drained comes before one that drains", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 0, "m-1", false)
			drainedFor(t, s, 1, "", true)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Node: "n-m-1",
			Zone: "ams"}},
		{"one that drained comes before a newer one that is only ineligible", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 0, "m-1", false)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Node: "n-m-1",
			Zone: "ams"}},
		{"one that drains comes before a newer one that is only ineligible", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 0, "", true)
		}, outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
	})
	s := base()
	dropMachine(&s, "m-2")
	dropNode(&s, workerName(1))
	checkOutcome(t, nextIn(t, rollout.Shrink, s), outcome{Action: rollout.Done})
}

func TestShrinkGivesAnEligibleNodeWithTheDrainMetaOfItsMachineANewDrain(t *testing.T) {
	s := shrinkClientsState(3, 0)
	nodeNamed(t, &s, workerName(2)).DrainedFor = "m-3"
	checkOutcome(t, nextIn(t, rollout.Shrink, s), outcome{Action: rollout.Drain, Group: "workers",
		Machine: workerName(2), MachineID: "m-3", Node: "n-m-3", Zone: "ams", Deadline: time.Hour})
}

func TestShrinkDoesNotCountAnEligibleNodeWithTheDrainMetaOfItsMachineAsRemoved(t *testing.T) {
	s := shrinkClientsState(3, 0)
	nodeNamed(t, &s, workerName(0)).DrainedFor = "m-1"
	checkOutcome(t, nextIn(t, rollout.Shrink, s), markOf(2, "ams"))
}

// dropMachine removes the machine with that ID from the state.
func dropMachine(s *rollout.State, id string) {
	var kept []rollout.Machine
	for _, m := range s.Machines {
		if m.ID != id {
			kept = append(kept, m)
		}
	}
	s.Machines = kept
}
