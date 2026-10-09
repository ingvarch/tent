package rollout_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/rollout"
)

// ruleCase is a state of the group workers and the step that the first rule that applies gives.
type ruleCase struct {
	name  string
	build func(t *testing.T, s *rollout.State)
	want  outcome
}

func runRuleCases(t *testing.T, tests []ruleCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			tt.build(t, &s)
			checkOutcome(t, nextRoll(t, s), tt.want)
		})
	}
}

// drainedFor marks the node of worker i as ineligible, and as drained for the given machine ID.
func drainedFor(t *testing.T, s *rollout.State, i int, id string, draining bool) {
	t.Helper()
	n := nodeNamed(t, s, workerName(i))
	n.Eligible = false
	n.Draining = draining
	n.DrainedFor = id
}

func setNodeStatus(t *testing.T, s *rollout.State, i int, status string) {
	t.Helper()
	nodeNamed(t, s, workerName(i)).Status = status
}

func TestRuleC1PurgesADownOrphan(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"a down node that no machine has", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addOrphan(s, workerName(7), "down", 50)
		}, outcome{Action: rollout.Purge, Group: "workers", Node: "n-gone-prod-workers-7"}},
		{"it wins over deleting a drained machine", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
			drainedFor(t, s, 0, "m-1", false)
			addOrphan(s, workerName(7), "down", 50)
		}, outcome{Action: rollout.Purge, Group: "workers", Node: "n-gone-prod-workers-7"}},
		{"it comes before the refusal of an older server", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addOrphan(s, workerName(7), "down", 50)
			s.Version = "2.0.8"
		}, outcome{Action: rollout.Purge, Group: "workers", Node: "n-gone-prod-workers-7"}},
		{"the first orphan by name goes first", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addOrphan(s, workerName(9), "down", 51)
			addOrphan(s, workerName(7), "down", 50)
		}, outcome{Action: rollout.Purge, Group: "workers", Node: "n-gone-prod-workers-7"}},
	})
}

func TestRuleC2DeletesAMachineThatIsDrainedOrDown(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"its drain completed", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
			drainedFor(t, s, 0, "m-1", false)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Node: "n-m-1",
			Zone: "ams"}},
		{"its node is down", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
			drainedFor(t, s, 0, "", false)
			setNodeStatus(t, s, 0, "down")
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Node: "n-m-1",
			Zone: "ams"}},
		{"it wins over draining another machine", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			drainedFor(t, s, 0, "", false)
			drainedFor(t, s, 1, "m-2", false)
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(1), MachineID: "m-2", Node: "n-m-2",
			Zone: "ams"}},
	})
}

func TestRuleC2LeavesTheMachineOfADrainThatRuns(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	drainedFor(t, &s, 0, "m-1", true)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestRuleC3DrainsAnIneligibleNode(t *testing.T) {
	drain := outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(0), MachineID: "m-1",
		Node: "n-m-1", Zone: "ams", Deadline: time.Hour}
	runRuleCases(t, []ruleCase{
		{"an outdated machine's node is ineligible", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "", false)
		}, drain},
		{"a drain of an earlier machine of that name does not count", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "m-99", false)
		}, drain},
		{"it wins over waiting for another node to drain", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "", false)
			drainedFor(t, s, 1, "", true)
		}, drain},
	})
}

func TestRuleC3ComesAfterCreatingAndMarking(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"creating a node comes before the drain", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			drainedFor(t, s, 0, "", false)
		}, outcome{Action: rollout.Create, Group: "workers", Machine: "prod-workers-2", Zone: "ams"}},
		{"marking the rest of a batch comes before the drain", func(t *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 2
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			addWorker(s, 3, newHash)
			drainedFor(t, s, 0, "", false)
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "ams"}},
	})
}

func TestRuleC3UsesTheGroupsDrainTimeout(t *testing.T) {
	s := baseState()
	s.Groups[0].DrainTimeout = 90 * time.Second
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	drainedFor(t, &s, 0, "", false)
	if got := nextRoll(t, s).Deadline; got != 90*time.Second {
		t.Errorf("deadline = %s, want 1m30s", got)
	}
}

func TestRuleC4CreatesNodes(t *testing.T) {
	create := func(name, zone string) outcome {
		return outcome{Action: rollout.Create, Group: "workers", Machine: name, Zone: zone}
	}
	runRuleCases(t, []ruleCase{
		{"the machines are outdated and the surge allows one more", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
		}, create("prod-workers-2", "ams")},
		{"the zone with the fewest up to date machines", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
		}, create("prod-workers-2", "fra")},
		{"the lowest free index", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 1, oldHash)
			addWorker(s, 2, oldHash)
		}, create("prod-workers-0", "ams")},
		{"a machine of the cluster holds its name, whatever its group", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			other := workerMachine(2, newHash)
			other.Group = "legacy"
			s.Machines = append(s.Machines, other)
		}, create("prod-workers-3", "ams")},
		{"a group short of its size gets its missing node", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
		}, create("prod-workers-1", "fra")},
		{"a surge of 2 allows a second new node", func(_ *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 2
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
		}, create("prod-workers-3", "fra")},
		{"a surge of 0 still fills a group short of its size", func(_ *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 0
			addWorker(s, 0, newHash)
		}, create("prod-workers-1", "fra")},
	})
}

// A new node never takes a name that Nomad lists, whatever the status of the node listed (a down node that no machine
// has is purged before any create, so it never reaches this rule).
func TestRuleC4SkipsNamesThatNomadLists(t *testing.T) {
	create := func(name string) outcome {
		return outcome{Action: rollout.Create, Group: "workers", Machine: name, Zone: "ams"}
	}
	outdatedPair := func(s *rollout.State) {
		addWorker(s, 0, oldHash)
		addWorker(s, 1, oldHash)
	}
	runRuleCases(t, []ruleCase{
		{"a ready node of the lowest free name", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "ready", 50)
		}, create("prod-workers-3")},
		{"an ineligible node of the lowest free name", func(t *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "ready", 50)
			nodeNamed(t, s, workerName(2)).Eligible = false
		}, create("prod-workers-3")},
		{"a disconnected node of the lowest free name", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "disconnected", 50)
		}, create("prod-workers-3")},
		{"an initializing node of the lowest free name", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "initializing", 50)
		}, create("prod-workers-3")},
		{"two nodes of one name count once", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "ready", 50)
			s.Nomad.Nodes = append(s.Nomad.Nodes, s.Nomad.Nodes[len(s.Nomad.Nodes)-1])
		}, create("prod-workers-3")},
		{"nodes of consecutive names", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "ready", 50)
			addOrphan(s, workerName(3), "ready", 51)
		}, create("prod-workers-4")},
		{"a node above the lowest free name leaves it free", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(3), "ready", 50)
		}, create("prod-workers-2")},
		{"a node below the machines' names takes its name", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 1, oldHash)
			addWorker(s, 2, oldHash)
			addOrphan(s, workerName(0), "ready", 50)
		}, create("prod-workers-3")},
		{"a node of another name does not count", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, "prod-web-2", "ready", 50)
		}, create("prod-workers-2")},
		{"a name is free again once its node is purged", func(_ *testing.T, s *rollout.State) {
			outdatedPair(s)
			addOrphan(s, workerName(2), "ready", 50)
			dropNode(s, workerName(2))
		}, create("prod-workers-2")},
	})
}

func TestRuleC4CreatesNothingBeyondSizePlusSurge(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	if got := nextRoll(t, s); got.Action == rollout.Create {
		t.Errorf("step = %q, want no create at size 2 plus surge 1", got)
	}
}

func TestRuleC4CreatesNothingOnceTheNewNodesCoverTheSize(t *testing.T) {
	s := baseState()
	s.Groups[0].MaxSurge = 2
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	addWorker(&s, 3, newHash)
	if got := nextRoll(t, s); got.Action == rollout.Create {
		t.Errorf("step = %q, want no create with two up to date nodes of size 2", got)
	}
}

func TestRuleC4CountsAMachineThatHasNotJoinedAsUpToDate(t *testing.T) {
	s := baseState()
	s.Groups[0].MaxSurge = 2
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	addWorker(&s, 3, newHash)
	machineOf(t, &s, workerName(3)).Joined = false
	if got := nextRoll(t, s); got.Action == rollout.Create {
		t.Errorf("step = %q, want no create while the new node has not joined", got)
	}
}

func TestRuleC4RefusesWhileAServerRunsAnOlderNomad(t *testing.T) {
	s := baseState()
	s.Version = "2.0.8"
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	checkRefused(t, s, "node group workers: a new node would run Nomad 2.0.8, newer than the 2.0.7 of server "+
		"prod-servers-0.global; roll the servers first")
}

func TestRuleC4AcceptsServersOfTheNewVersion(t *testing.T) {
	s := baseState()
	s.Version = "2.0.8"
	s.Nomad.Servers[0].Version = "2.0.8"
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "workers", Machine: "prod-workers-2",
		Zone: "ams"})
}

func TestRuleC5StartsTheRemovalOfAVictim(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"the budget allows it: the node is marked ineligible", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"a victim without a node is deleted", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			dropNode(s, workerName(0))
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1", Zone: "ams"}},
		{"a victim whose node is down is deleted", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			setNodeStatus(t, s, 0, "down")
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"a dead victim does not wait for the budget", func(t *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 0
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			setNodeStatus(t, s, 0, "down")
		}, outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"an unavailable victim that is up is marked without waiting for the budget", func(t *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 0
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			setNodeStatus(t, s, 0, "disconnected")
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"maxUnavailable 1 lets a node go without a new one", func(_ *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 0
			s.Groups[0].MaxUnavailable = 1
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"it wins over waiting for another node to drain", func(t *testing.T, s *rollout.State) {
			s.Groups[0].MaxSurge = 2
			addWorker(s, 0, oldHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			addWorker(s, 3, newHash)
			drainedFor(t, s, 0, "", true)
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "ams"}},
	})
}

func TestRuleC5LeavesAMachineThatHasNotJoined(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	machineOf(t, &s, workerName(0)).Joined = false
	// Worker 0 cannot be a victim; worker 1 is, but the budget is short by the unjoined worker 0.
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.WaitJoined, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Zone: "ams"})
}

func TestRuleC5DoesNotStartWhenTheBudgetIsShort(t *testing.T) {
	s := baseState()
	s.Groups[0].MaxSurge = 0
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	checkRefused(t, s, "node group workers: cannot go on: with maxSurge 0 and maxUnavailable 0 no outdated node "+
		"can be replaced")
}

func TestRuleC5RefusesWhileAServerRunsAnOlderNomad(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	s.Version = "2.0.8"
	checkRefused(t, s, "node group workers: a new node would run Nomad 2.0.8, newer than the 2.0.7 of server "+
		"prod-servers-0.global; roll the servers first")
}

func TestRuleC4ARemovalUnderWayDrainsBeforeTheCreateRefuses(t *testing.T) {
	s := baseState()
	s.Version = "2.0.8"
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	drainedFor(t, &s, 0, "", false)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams", Deadline: time.Hour})
}

func TestRuleC5ARemovalUnderWayDrainsBeforeTheMarkRefuses(t *testing.T) {
	s := baseState()
	s.Version = "2.0.8"
	s.Groups[0].MaxSurge = 2
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	addWorker(&s, 3, newHash)
	drainedFor(t, &s, 0, "", false)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams", Deadline: time.Hour})
}

func TestRuleC5PrefersTheReadyNodeOfAMachineWithTwoNodes(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	dead := nodeOf(workerMachine(0, oldHash))
	dead.ID = "n-dead"
	dead.Status = "down"
	s.Nomad.Nodes = append([]rollout.Node{dead}, s.Nomad.Nodes...)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestRuleC6WaitsForADrain(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"a node is draining", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "", true)
		}, outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
		{"it wins over waiting for a machine to join", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, oldHash)
			addWorker(s, 1, newHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "", true)
			machineOf(t, s, workerName(2)).Joined = false
		}, outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(0), MachineID: "m-1",
			Node: "n-m-1", Zone: "ams"}},
	})
}

func TestRuleC7WaitsForAMachineToJoin(t *testing.T) {
	waitJoined := outcome{Action: rollout.WaitJoined, Group: "workers", Machine: workerName(2), MachineID: "m-3",
		Zone: "ams"}
	runRuleCases(t, []ruleCase{
		{"a new machine has not joined", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addWorker(s, 2, newHash)
			dropNode(s, workerName(2))
			machineOf(t, s, workerName(2)).Joined = false
		}, waitJoined},
		{"it wins over waiting for an orphan node", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addWorker(s, 2, newHash)
			dropNode(s, workerName(2))
			machineOf(t, s, workerName(2)).Joined = false
			addOrphan(s, workerName(7), "ready", 50)
		}, waitJoined},
	})
}

func TestRuleC8WaitsForAnOrphanNodeToGoDown(t *testing.T) {
	waitDown := outcome{Action: rollout.WaitNodeDown, Group: "workers", Node: "n-gone-prod-workers-7"}
	runRuleCases(t, []ruleCase{
		{"a ready node that no machine has", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addOrphan(s, workerName(7), "ready", 50)
		}, waitDown},
		{"a node of a machine's name at another address", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addOrphan(s, workerName(7), "ready", 50)
			s.Machines = append(s.Machines, workerMachine(7, newHash))
			s.Machines[len(s.Machines)-1].Group = "legacy"
			s.Machines[len(s.Machines)-1].PrivateIP = ip(60)
		}, waitDown},
		{"a node of another name at a machine's address", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addOrphan(s, workerName(7), "ready", 3)
		}, waitDown},
	})
}

func TestAStaleNodeAtAMachinesAddressIsNotItsNode(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	stale := rollout.Node{ID: "n-stale", Name: workerName(7), Address: ip(3), Status: "ready", Eligible: true,
		Version: "2.0.7"}
	s.Nomad.Nodes = append([]rollout.Node{stale}, s.Nomad.Nodes...)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.MarkIneligible, Group: "workers",
		Machine: workerName(0), MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestOrphansOfOtherGroupsAndOtherNamesAreLeftAlone(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, newHash)
	addWorker(&s, 1, newHash)
	addOrphan(&s, "prod-web-0", "down", 50)
	addOrphan(&s, "prod-workers-x", "down", 51)
	addOrphan(&s, "prod-workers-", "down", 52)
	addOrphan(&s, "dev-workers-0", "down", 53)
	addOrphan(&s, "prod-workers-1-extra", "down", 54)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestRuleC9GroupIsDone(t *testing.T) {
	runRuleCases(t, []ruleCase{
		{"nothing is outdated", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
		}, outcome{Action: rollout.Done}},
		{"machines beyond the size are left to update", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			addWorker(s, 2, newHash)
		}, outcome{Action: rollout.Done}},
		{"an ineligible node of an up to date machine stays as it is", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, newHash)
			drainedFor(t, s, 0, "", false)
		}, outcome{Action: rollout.Done}},
	})
}

func TestOutdatedMachines(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
		want  outcome
	}{
		{"a machine without a hash", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, workerName(0)).SpecHash = ""
		}, outcome{Action: rollout.Create, Group: "workers", Machine: "prod-workers-2", Zone: "fra"}},
		{"a forced machine, whatever its hash", func(_ *testing.T, s *rollout.State) {
			s.Forced = map[string]bool{"m-1": true}
		}, outcome{Action: rollout.Create, Group: "workers", Machine: "prod-workers-2", Zone: "fra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			addWorker(&s, 0, newHash)
			addWorker(&s, 1, newHash)
			tt.build(t, &s)
			checkOutcome(t, nextRoll(t, s), tt.want)
		})
	}
}

func TestForcedIsByMachineID(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, newHash)
	addWorker(&s, 1, newHash)
	s.Forced = map[string]bool{"prod-workers-0": true, "m-9": true, "m-1": false}
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestRuleC10RefusesAGroupThatCannotGoOn(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
		want  string
	}{
		{"an operator's ineligible node blocks the budget", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			drainedFor(t, s, 0, "", false)
		}, "node group workers: cannot go on: prod-workers-0 is not eligible; with maxSurge 1 and maxUnavailable 0 " +
			"no outdated node can be replaced"},
		{"a node that is not running", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			machineOf(t, s, workerName(0)).Ready = false
		}, "node group workers: cannot go on: prod-workers-0 is not running; with maxSurge 1 and maxUnavailable 0 " +
			"no outdated node can be replaced"},
		{"a machine without a node", func(_ *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			dropNode(s, workerName(0))
		}, "node group workers: cannot go on: prod-workers-0 has no node in Nomad; with maxSurge 1 and " +
			"maxUnavailable 0 no outdated node can be replaced"},
		{"a node that is disconnected", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			setNodeStatus(t, s, 0, "disconnected")
		}, "node group workers: cannot go on: prod-workers-0 is disconnected in Nomad; with maxSurge 1 and " +
			"maxUnavailable 0 no outdated node can be replaced"},
		{"a node that an operator drains", func(t *testing.T, s *rollout.State) {
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			nodeNamed(t, s, workerName(0)).Draining = true
		}, "node group workers: cannot go on: prod-workers-0 is draining; with maxSurge 1 and maxUnavailable 0 " +
			"no outdated node can be replaced"},
		{"two such nodes", func(t *testing.T, s *rollout.State) {
			s.Groups[0].Size = 3
			addWorker(s, 0, newHash)
			addWorker(s, 1, oldHash)
			addWorker(s, 2, newHash)
			addWorker(s, 3, newHash)
			drainedFor(t, s, 0, "", false)
			nodeNamed(t, s, workerName(2)).Draining = true
		}, "node group workers: cannot go on: prod-workers-0 is not eligible and prod-workers-2 is draining; with " +
			"maxSurge 1 and maxUnavailable 0 no outdated node can be replaced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			tt.build(t, &s)
			checkRefused(t, s, tt.want)
		})
	}
}

func TestVictimsAreChosenInOrder(t *testing.T) {
	// Three outdated machines for size 2 with maxUnavailable 5: a victim exists at once, and the budget never decides.
	build := func(t *testing.T, edit func(s *rollout.State)) rollout.State {
		t.Helper()
		s := baseState()
		s.Groups[0].MaxSurge = 0
		s.Groups[0].MaxUnavailable = 5
		for i := range 3 {
			addWorker(&s, i, oldHash)
		}
		edit(&s)
		return s
	}
	tests := []struct {
		name string
		edit func(s *rollout.State)
		want outcome
	}{
		{"the oldest", func(s *rollout.State) {
			s.Machines[3].Created = epoch.Add(-48 * time.Hour)
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(2), MachineID: "m-3",
			Node: "n-m-3", Zone: "ams"}},
		{"the lowest ID when the age is the same", func(s *rollout.State) {
			for i := range s.Machines {
				s.Machines[i].Created = epoch
			}
			s.Machines[1].ID = "m-9" // worker 0 is first by name, but worker 1 has the lowest ID
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "ams"}},
		{"an unknown age counts as the newest", func(s *rollout.State) {
			s.Machines[1].Created = time.Time{}
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "ams"}},
		{"in the zone with the most machines, before the oldest", func(s *rollout.State) {
			s.Machines[2].Zone = "fra"
			s.Machines[3].Zone = "fra"
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(1), MachineID: "m-2",
			Node: "n-m-2", Zone: "fra"}},
		{"one that is not available, before the zone and the age", func(s *rollout.State) {
			s.Machines[2].Zone = "fra"
			s.Machines[3].Zone = "fra"
			s.Nomad.Nodes[2].Status = "disconnected"
			s.Machines[3].Created = epoch
		}, outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(2), MachineID: "m-3",
			Node: "n-m-3", Zone: "fra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Machines[0] is the server, so worker i is Machines[i+1] and Nodes[i].
			checkOutcome(t, nextRoll(t, build(t, tt.edit)), tt.want)
		})
	}
}

func TestRemovalNeedsTheMachineToHaveJoined(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	drainedFor(t, &s, 0, "", false)
	machineOf(t, &s, workerName(0)).Joined = false
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.WaitJoined, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Zone: "ams"})
}

func TestADrainingNodeCountsAsRemovedEvenIfEligible(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	nodeNamed(t, &s, workerName(0)).Draining = true
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.WaitDrained, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestAnEligibleNodeWithTheDrainMetaOfItsMachineGetsANewDrain(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	nodeNamed(t, &s, workerName(0)).DrainedFor = "m-1"
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams", Deadline: time.Hour})
}

func TestADrainOfAnEligibleNodeIsFollowedByItsDelete(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	drainedFor(t, &s, 0, "m-1", false)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestOrphansOfOneNameGoByID(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, newHash)
	addWorker(&s, 1, newHash)
	for _, id := range []string{"n-gone-b", "n-gone-a"} {
		addOrphan(&s, workerName(7), "down", 50)
		s.Nomad.Nodes[len(s.Nomad.Nodes)-1].ID = id
	}
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Purge, Group: "workers", Node: "n-gone-a"})
}

func TestAMachineWithoutAnAddressHasNoNode(t *testing.T) {
	noAddress := func(t *testing.T, s *rollout.State) {
		t.Helper()
		machineOf(t, s, workerName(0)).PrivateIP = netip.Addr{}
	}
	refusal := "node group workers: cannot go on: prod-workers-0 has no node in Nomad: the cloud reports no " +
		"private address for it; with maxSurge 1 and maxUnavailable 0 no outdated node can be replaced"
	t.Run("it is neither a victim nor deleted: the group is refused", func(t *testing.T) {
		s := baseState()
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, newHash)
		addWorker(&s, 2, newHash)
		noAddress(t, &s)
		checkRefused(t, s, refusal)
	})
	t.Run("its down node is not purged", func(t *testing.T) {
		s := baseState()
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, newHash)
		addWorker(&s, 2, newHash)
		noAddress(t, &s)
		setNodeStatus(t, &s, 0, "down")
		checkRefused(t, s, refusal)
	})
	t.Run("another victim still goes on", func(t *testing.T) {
		s := baseState()
		s.Groups[0].MaxSurge = 2
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, oldHash)
		addWorker(&s, 2, newHash)
		addWorker(&s, 3, newHash)
		noAddress(t, &s)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.MarkIneligible, Group: "workers",
			Machine: workerName(1), MachineID: "m-2", Node: "n-m-2", Zone: "ams"})
	})
	t.Run("it does not hold a node of its name", func(t *testing.T) {
		s := baseState()
		addWorker(&s, 0, newHash)
		addWorker(&s, 1, newHash)
		pending := workerMachine(7, newHash)
		pending.Group = "legacy"
		pending.PrivateIP = netip.Addr{}
		s.Machines = append(s.Machines, pending)
		addOrphan(&s, workerName(7), "ready", 50)
		s.Nomad.Nodes[len(s.Nomad.Nodes)-1].Address = netip.Addr{}
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.WaitNodeDown, Group: "workers",
			Node: "n-gone-prod-workers-7"})
	})
}

func TestEveryServerMustRunTheNewVersion(t *testing.T) {
	s := baseState()
	s.Version = "2.0.8"
	s.Nomad.Servers[0].Version = "2.0.8"
	second := s.Nomad.Servers[0]
	second.Name, second.Version = "prod-servers-1.global", "2.0.7"
	s.Nomad.Servers = append(s.Nomad.Servers, second)
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	checkRefused(t, s, "node group workers: a new node would run Nomad 2.0.8, newer than the 2.0.7 of server "+
		"prod-servers-1.global; roll the servers first")
}

func TestRuleC5PrefersTheReadyNodeWhenTheDownNodeIsListedAfterIt(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	dead := nodeOf(workerMachine(0, oldHash))
	dead.ID = "n-dead"
	dead.Status = "down"
	s.Nomad.Nodes = append(s.Nomad.Nodes, dead)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestAnUpToDateMachineWhoseNodeIsDownIsNotDeleted(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, newHash)
	addWorker(&s, 1, newHash)
	setNodeStatus(t, &s, 0, "down")
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestANodeThatDiesWhileItDrainsIsDeleted(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	addWorker(&s, 2, newHash)
	drainedFor(t, &s, 0, "", true)
	setNodeStatus(t, &s, 0, "down")
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Delete, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}

func TestAServerWithoutAVersionIsWaitedFor(t *testing.T) {
	// The servers' versions are not all known yet, so a new node or a removal waits and does not refuse.
	wait := func(t *testing.T, s rollout.State) {
		t.Helper()
		got := nextRoll(t, s)
		if got.Action != rollout.WaitHealthy || got.Group != "workers" || got.Voters != 1 || got.Machine.Name != "" {
			t.Errorf("step = %q (group %q, %d voters, machine %q), want a wait until 1 healthy server votes for "+
				"workers that names no machine (the nonvoter does not count)", got, got.Group, got.Voters,
				got.Machine.Name)
		}
	}
	unknown := func() rollout.State {
		s := baseState()
		s.Nomad.Servers[0].Version = ""
		second := s.Nomad.Servers[0]
		second.ID, second.Name, second.Voter, second.Leader = "r-1", "prod-servers-1.global", false, false
		s.Nomad.Servers = append(s.Nomad.Servers, second)
		return s
	}
	t.Run("before a node is created", func(t *testing.T) {
		s := unknown()
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, oldHash)
		wait(t, s)
	})
	t.Run("before a removal starts", func(t *testing.T) {
		s := unknown()
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, newHash)
		addWorker(&s, 2, newHash)
		wait(t, s)
	})
	t.Run("a drain that is due goes first", func(t *testing.T) {
		s := unknown()
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, newHash)
		addWorker(&s, 2, newHash)
		nodeNamed(t, &s, workerName(0)).Eligible = false
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Drain, Group: "workers", Machine: workerName(0),
			MachineID: "m-1", Node: "n-m-1", Zone: "ams", Deadline: time.Hour})
	})
	t.Run("an older server is refused first", func(t *testing.T) {
		s := unknown()
		s.Version = "2.0.8"
		s.Nomad.Servers[1].Version = "2.0.7"
		addWorker(&s, 0, oldHash)
		addWorker(&s, 1, oldHash)
		checkRefused(t, s, "node group workers: a new node would run Nomad 2.0.8, newer than the 2.0.7 of server "+
			"prod-servers-1.global; roll the servers first")
	})
	t.Run("a shrink does not look at versions", func(t *testing.T) {
		s := unknown()
		addWorker(&s, 0, newHash)
		addWorker(&s, 1, newHash)
		addWorker(&s, 2, newHash)
		checkOutcome(t, nextIn(t, rollout.Shrink, s), outcome{Action: rollout.MarkIneligible, Group: "workers",
			Machine: workerName(2), MachineID: "m-3", Node: "n-m-3", Zone: "ams"})
	})
}

func TestARollTakesAJoinedVictimWhileAnOutdatedMachineHasNotJoined(t *testing.T) {
	// maxUnavailable 2 leaves room for the victim whatever the others do; the machine that has not joined is no victim.
	s := baseState()
	s.Groups[0].Size, s.Groups[0].MaxSurge, s.Groups[0].MaxUnavailable = 3, 0, 2
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	addWorker(&s, 2, newHash)
	machineOf(t, &s, workerName(1)).Joined = false
	dropNode(&s, workerName(1))
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.MarkIneligible, Group: "workers", Machine: workerName(0),
		MachineID: "m-1", Node: "n-m-1", Zone: "ams"})
}
