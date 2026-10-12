package rollout_test

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/rollout"
)

// surplusNames returns the names of the machines that Surplus lists for the first group of the state.
func surplusNames(s rollout.State) []string {
	var names []string
	for _, m := range rollout.Surplus(s, s.Groups[0]) {
		names = append(names, m.Name)
	}
	return names
}

type surplusCase struct {
	name  string
	build func(t *testing.T, s *rollout.State)
	want  []string
}

func runSurplusCases(t *testing.T, base func() rollout.State, tests []surplusCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.build(t, &s)
			if diff := cmp.Diff(tt.want, surplusNames(s)); diff != "" {
				t.Errorf("Surplus (-want +got):\n%s", diff)
			}
		})
	}
}

func machineNames(f func(int) string, indexes ...int) []string {
	var out []string
	for _, i := range indexes {
		out = append(out, f(i))
	}
	return out
}

func TestSurplusOfAClientGroupFollowsTheOrderOfVictims(t *testing.T) {
	// Three workers for size 2: one machine to lose. Worker 2 is the newest, and all are in ams.
	one := func(i int) []string { return machineNames(workerName, i) }
	runSurplusCases(t, func() rollout.State { return shrinkClientsState(3, 0) }, []surplusCase{
		{"the newest", func(*testing.T, *rollout.State) {}, one(2)},
		{"the highest ID when the ages are equal", func(_ *testing.T, s *rollout.State) {
			for i := range s.Machines {
				s.Machines[i].Created = epoch
			}
			s.Machines[2].ID = "m-9"
		}, one(1)},
		{"an unknown age counts as the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].Created = time.Time{}
		}, one(0)},
		{"the zone with the most machines, before the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[3].Zone = "fra"
		}, one(1)},
		{"a node that is not available, before the zone and the newest", func(t *testing.T, s *rollout.State) {
			s.Machines[3].Zone = "fra"
			setNodeStatus(t, s, 0, "disconnected")
		}, one(0)},
		{"an outdated machine, before one that is not available", func(t *testing.T, s *rollout.State) {
			s.Machines[2].SpecHash = oldHash
			setNodeStatus(t, s, 0, "disconnected")
		}, one(1)},
		{"a machine that has not joined, before an outdated one", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].Joined = false
			s.Machines[2].SpecHash = oldHash
		}, one(0)},
		{"a machine whose node is ineligible, before one that has not joined", func(t *testing.T, s *rollout.State) {
			s.Machines[3].Joined = false
			nodeNamed(t, s, workerName(0)).Eligible = false
		}, one(0)},
		{"a node that drained, before a newer one that is only ineligible", func(t *testing.T, s *rollout.State) {
			drainedFor(t, s, 0, "m-1", false)
			nodeNamed(t, s, workerName(1)).Eligible = false
		}, one(0)},
		{"an eligible node with the drain meta of its machine has not started", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(0)).DrainedFor = "m-1"
		}, one(2)},
	})
}

func TestSurplusOfAClientGroupLosesDrainedBeforeDrainingBeforeIneligible(t *testing.T) {
	// Four workers for size 1: three to lose. By age alone the order would be 2, 1, 0; worker 3 has not started.
	s := shrinkClientsState(4, 0)
	drainedFor(t, &s, 0, "m-1", false)
	drainedFor(t, &s, 1, "", true)
	nodeNamed(t, &s, workerName(2)).Eligible = false
	s.Groups[0].Size = 1
	want := machineNames(workerName, 0, 1, 2)
	if diff := cmp.Diff(want, surplusNames(s)); diff != "" {
		t.Errorf("Surplus (-want +got):\n%s", diff)
	}
}

func TestSurplusOfAClientGroupLosesAJoinedMachineWithoutAnAddressFirst(t *testing.T) {
	// Three workers for size 1: two to lose, and workers 1 and 2 joined and have no private address, so the cloud
	// lists them though it deleted them. Both go, and worker 0 stays.
	s := shrinkClientsState(3, 0)
	s.Groups[0].Size = 1
	for _, i := range []int{2, 3} {
		s.Machines[i].PrivateIP = netip.Addr{}
	}
	if diff := cmp.Diff(machineNames(workerName, 2, 1), surplusNames(s)); diff != "" {
		t.Errorf("Surplus (-want +got):\n%s", diff)
	}
	t.Run("a machine that has not joined is no machine without an address", func(t *testing.T) {
		s := s
		s.Machines = append([]rollout.Machine(nil), s.Machines...)
		s.Machines[3].Joined = false
		if diff := cmp.Diff(machineNames(workerName, 1, 2), surplusNames(s)); diff != "" {
			t.Errorf("Surplus (-want +got):\n%s", diff)
		}
	})
}

func TestSurplusTakesTheMachineWithoutAnAddressBeforeEveryOtherMachine(t *testing.T) {
	// Three workers for size 2: one machine to lose. One worker joined and has no private address, and another is
	// what the order takes first among machines with addresses. The group loses the one without an address, and Next
	// takes no step that removes the other.
	for _, tt := range []struct {
		name      string
		noAddress int
		build     func(t *testing.T, s *rollout.State)
	}{
		{"one whose node is ineligible", 2, func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(1)).Eligible = false
		}},
		{"one whose node drains", 2, func(t *testing.T, s *rollout.State) { drainedFor(t, s, 1, "", true) }},
		{"one whose drain completed", 2, func(t *testing.T, s *rollout.State) { drainedFor(t, s, 1, "m-2", false) }},
		{"one that has not joined", 2, func(_ *testing.T, s *rollout.State) {
			s.Machines[2].Joined = false
			dropNode(s, workerName(1))
		}},
		{"an outdated one", 2, func(_ *testing.T, s *rollout.State) { s.Machines[2].SpecHash = oldHash }},
		{"a newer one whose node is down", 0, func(t *testing.T, s *rollout.State) { setNodeStatus(t, s, 2, "down") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := shrinkClientsState(3, 1)
			tt.build(t, &s)
			s.Machines[tt.noAddress+1].PrivateIP = netip.Addr{}
			if diff := cmp.Diff(machineNames(workerName, tt.noAddress), surplusNames(s)); diff != "" {
				t.Errorf("Surplus (-want +got):\n%s", diff)
			}
			step, err := rollout.Next(s, rollout.Shrink)
			switch step.Action {
			case rollout.MarkIneligible, rollout.Drain, rollout.WaitDrained, rollout.Delete:
				t.Errorf("Next = %q, %v, want no step that removes a machine", step, err)
			}
		})
	}
}

func TestSurplusOfAClientGroupLosesTheMachineWithoutAnAddressInAnyPosition(t *testing.T) {
	// Three workers for size 1; one of them has no address. It goes first, and the newest of the others goes after
	// it, whatever the age and the zone of the machine without an address.
	for ghost := range 3 {
		for _, zone := range []string{"ams", "fra"} {
			t.Run(workerName(ghost)+" in "+zone, func(t *testing.T) {
				s := shrinkClientsState(3, 0)
				s.Groups[0].Size = 1
				s.Machines[ghost+1].PrivateIP = netip.Addr{}
				s.Machines[ghost+1].Zone = zone
				others := slices.DeleteFunc([]int{2, 1, 0}, func(i int) bool { return i == ghost })
				want := machineNames(workerName, ghost, others[0])
				if diff := cmp.Diff(want, surplusNames(s)); diff != "" {
					t.Errorf("Surplus (-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestSurplusTakesAMachineWithoutAnAddressBeforeTheMachineOfTheCrowdedZone(t *testing.T) {
	// Worker 0 is in ams and counts as the newest; workers 1 and 2 are in fra, and by age and zone worker 1 would go.
	// Worker 2 joined and has no private address, so it goes in its place.
	s := shrinkClientsState(3, 0)
	s.Machines[1].Created = time.Time{}
	s.Machines[2].Zone, s.Machines[3].Zone = "fra", "fra"
	s.Machines[3].PrivateIP = netip.Addr{}
	if diff := cmp.Diff(machineNames(workerName, 2), surplusNames(s)); diff != "" {
		t.Errorf("Surplus (-want +got):\n%s", diff)
	}
}

func TestSurplusIsEmptyForAGroupAtOrBelowItsSize(t *testing.T) {
	for name, s := range map[string]rollout.State{
		"clients at their size":         shrinkClientsState(2, 0),
		"clients below their size":      shrinkClientsState(1, 0),
		"no clients":                    shrinkClientsState(0, 0),
		"servers at their size":         shrinkServersState(3, 3),
		"servers below their size":      shrinkServersState(2, 3),
		"combined nodes at their size":  combinedState(3),
		"outdated machines at the size": outdatedSurplus(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := surplusNames(s); len(got) != 0 {
				t.Errorf("Surplus = %q, want none", got)
			}
		})
	}
}

func TestSurplusIsEmptyForAGroupOfAnUnknownRole(t *testing.T) {
	s := shrinkClientsState(3, 0)
	s.Groups[0].Role = "bogus"
	if got := surplusNames(s); len(got) != 0 {
		t.Errorf("Surplus = %q, want none", got)
	}
}

func outdatedSurplus() rollout.State {
	s := shrinkClientsState(2, 0)
	s.Machines[1].SpecHash, s.Machines[2].SpecHash = oldHash, oldHash
	return s
}

func TestSurplusOfAServerGroupFollowsTheOrderOfVictims(t *testing.T) {
	// Four servers for size 3: server 0 leads; 0 and 2 are in ams, 1 and 3 in fra; by age 3 is the newest.
	one := func(i int) []string { return machineNames(serverName, i) }
	runSurplusCases(t, func() rollout.State { return shrinkServersState(4, 3) }, []surplusCase{
		{"the newest of the servers that do not lead", func(*testing.T, *rollout.State) {}, one(3)},
		{"the leader goes last", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 0).Leader, serverNode(t, s, 3).Leader = false, true
		}, one(2)},
		{"the zone with the most servers, before the newest", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].Zone = "ams"
		}, one(2)},
		{"a server that autopilot does not count healthy, before the zone and the newest", func(t *testing.T,
			s *rollout.State) {
			serverNode(t, s, 1).Healthy = false
			s.Machines[3].Zone = "ams"
		}, one(1)},
		{"an outdated server, before an unhealthy one", func(t *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash = oldHash
			serverNode(t, s, 2).Healthy = false
		}, one(1)},
		{"a server that has not joined, before an outdated one", func(_ *testing.T, s *rollout.State) {
			s.Machines[2].Joined = false
			s.Machines[1].SpecHash = oldHash
		}, one(2)},
		{"a server whose removal has started, before one that has not joined", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 0)
			s.Machines[2].Joined = false
		}, one(0)},
	})
}

func TestSurplusOfACombinedGroupStartsWithTheNodeThatDrains(t *testing.T) {
	base := func() rollout.State {
		s := combinedState(3)
		addCombinedNode(&s, 3, newHash)
		return s
	}
	runSurplusCases(t, base, []surplusCase{
		{"the newest", func(*testing.T, *rollout.State) {}, machineNames(serverName, 3)},
		{"a node that drains, before the newest", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, serverName(1)).Draining = true
		}, machineNames(serverName, 1)},
		{"a node that is ineligible, before the newest", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, serverName(2)).Eligible = false
		}, machineNames(serverName, 2)},
	})
}

func TestSurplusTakesOneMachineAtATime(t *testing.T) {
	t.Run("servers: the zones are counted again without the machine that went", func(t *testing.T) {
		// ams has 0, 2 and 4; fra has 1 and 3. Server 4 goes first; then each zone has two, and the newest, 3, goes.
		// One sort over all five servers gives 4 and 2.
		want := machineNames(serverName, 4, 3)
		if diff := cmp.Diff(want, surplusNames(shrinkServersState(5, 3))); diff != "" {
			t.Errorf("Surplus (-want +got):\n%s", diff)
		}
	})
	// Workers 0 to 2 are in ams and 3 and 4 in fra, created in that order, for size 3.
	clients := func() rollout.State {
		s := shrinkClientsState(5, 5)
		s.Groups[0].Size = 3
		s.Machines[4].Zone, s.Machines[5].Zone = "fra", "fra"
		return s
	}
	t.Run("clients: the zones are counted again without the machine that went", func(t *testing.T) {
		// Worker 2 goes first; then each zone has two and the newest, 4, goes. One sort gives 2 and 1.
		if diff := cmp.Diff(machineNames(workerName, 2, 4), surplusNames(clients())); diff != "" {
			t.Errorf("Surplus (-want +got):\n%s", diff)
		}
	})
	t.Run("clients: Next marks worker 4 after worker 2", func(t *testing.T) {
		s := clients()
		checkOutcome(t, nextIn(t, rollout.Shrink, s), markOf(2, "ams"))
		nodeNamed(t, &s, workerName(2)).Eligible = false
		checkOutcome(t, nextIn(t, rollout.Shrink, s), markOf(4, "fra"))
	})
}

func TestSurplusDoesNotChangeItsState(t *testing.T) {
	reversed := func() rollout.State {
		s := shrinkServersState(5, 3)
		slices.Reverse(s.Machines)
		return s
	}
	s := reversed()
	surplusNames(s)
	if diff := cmp.Diff(reversed(), s, cmpopts.EquateComparable(netip.Addr{}, netip.AddrPort{})); diff != "" {
		t.Errorf("Surplus changed its state (-before +after):\n%s", diff)
	}
}

func orphanNames(s rollout.State, group string) []string {
	var found []string
	for _, n := range rollout.Orphans(s, groupNamed(&s, group)) {
		found = append(found, n.ID)
	}
	return found
}

func groupNamed(s *rollout.State, name string) rollout.Group {
	for _, g := range s.Groups {
		if g.Name == name {
			return g
		}
	}
	return rollout.Group{Name: name}
}

func TestOrphansAreTheNodesOfAGroupThatNoMachineHas(t *testing.T) {
	base := func() rollout.State { return shrinkClientsState(2, 0) }
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
		want  []string
	}{
		{"none", func(*testing.T, *rollout.State) {}, nil},
		{"a node of the group's name that no machine has", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, workerName(7), "down", 40)
		}, []string{"n-gone-prod-workers-7"}},
		{"its status does not matter", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, workerName(7), "ready", 40)
		}, []string{"n-gone-prod-workers-7"}},
		{"a node of a machine's name at another address", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Nodes[0].Address = ip(90)
		}, []string{"n-m-1"}},
		{"a node of a machine's name while the machine has no address is none", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].PrivateIP = netip.Addr{}
			s.Nomad.Nodes[0].Address = ip(90)
		}, nil},
		{"the machine of another group at that name and address has the node", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].Group = "elsewhere"
		}, nil},
		{"a node of a machine's name and address is none", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, workerName(0), "down", 3)
		}, nil},
		{"a node of another group", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, "prod-others-3", "down", 40)
		}, nil},
		{"a node of another cluster", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, "dev-workers-3", "down", 40)
		}, nil},
		{"a node of the group's name with a suffix", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, workerName(3)+"-x", "down", 40)
		}, nil},
		{"by name, then ID", func(_ *testing.T, s *rollout.State) {
			addOrphan(s, workerName(9), "down", 41)
			addOrphan(s, workerName(3), "down", 42)
			twin := rollout.Node{ID: "n-a", Name: workerName(3), Address: ip(43), Status: "down"}
			s.Nomad.Nodes = append(s.Nomad.Nodes, twin)
		}, []string{"n-a", "n-gone-prod-workers-3", "n-gone-prod-workers-9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.build(t, &s)
			if diff := cmp.Diff(tt.want, orphanNames(s, "workers")); diff != "" {
				t.Errorf("Orphans (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOrphansOfAGroupTheStateLacks(t *testing.T) {
	s := shrinkClientsState(2, 0)
	addOrphan(&s, "prod-old-0", "down", 40)
	if diff := cmp.Diff([]string{"n-gone-prod-old-0"}, orphanNames(s, "old")); diff != "" {
		t.Errorf("Orphans (-want +got):\n%s", diff)
	}
}
