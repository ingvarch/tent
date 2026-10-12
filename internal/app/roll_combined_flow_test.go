package app_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
)

// wantCombinedNodesReady fails the test unless Nomad lists a node for each combined machine that f lists, with the ID
// n-<instance ID>, the machine's name, ready and eligible, and lists no other node whose name starts with the group's.
func wantCombinedNodesReady(t *testing.T, svc *app.Service, f *vultrfake.Fake) {
	t.Helper()
	machines := serverMachines(f, combinedGroup)
	var want, got []string
	for _, m := range machines {
		want = append(want, nodeIDOf(m.id)+" "+m.name+" ready eligible")
	}
	for _, n := range nomadNodes(t, svc) {
		if !strings.HasPrefix(n.Name, machinePrefix(combinedGroup)) {
			continue
		}
		state := n.Status + " ineligible"
		if n.Eligible {
			state = n.Status + " eligible"
		}
		got = append(got, n.ID+" "+n.Name+" "+state)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("Nomad lists the combined nodes %q, want %q", got, want)
	}
}

// TestRollCombinedFlowReplacesTheNodes applies the roll of the three outdated combined nodes, with the invariants of
// a combined group watching: the plan that it returns and the calls to Vultr and Nomad match golden files, and the
// cluster ends as wantRollEnded, wantServersRolled and wantCombinedNodesReady say.
func TestRollCombinedFlowReplacesTheNodes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		var plan app.RollPlan
		var err error

		calls := flowCalls(t, svc, f, w, func() *engine.Plan {
			plan, err = applyRoll(svc, app.RollOptions{})
			return nil
		})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Drained: 3, Stopped: 3, Deleted: 3, Purged: 3}); !plan.Applied ||
			plan.Rolled != want {
			t.Errorf("the plan says applied %v and rolled %+v, want applied and %+v", plan.Applied, plan.Rolled, want)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		checkGolden(t, "flow_roll_combined.plan.golden", text.String())
		checkGolden(t, "flow_roll_combined.calls.golden", callsText(collapsePolls(calls)))
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, combinedGroup, old, true)
		wantCombinedNodesReady(t, svc, f)
		inv.wantKept()
	})
}

// TestRollCombinedFlowReportsEachStepOfTheRoll applies the same roll and matches its progress lines to a golden file.
func TestRollCombinedFlowReportsEachStepOfTheRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := combinedFlowWorld(t)
		lines := recordProgress(svc)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		checkGolden(t, "flow_roll_combined.steps.golden", strings.Join(*lines, "\n")+"\n")
	})
}

// combinedClusterFlowWorld is combinedRollWorld whose workers are outdated too, with the invariants of a combined group
// watching.
func combinedClusterFlowWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld, *serverInvariants) {
	t.Helper()
	svc, f, w := combinedRollWorld(t)
	mustReplace(t, svc, workersMetaYAML)
	mustUpdate(t, svc)
	return svc, f, w, watchCombined(t, f, w, combinedGroup, 3)
}

// TestRollCombinedFlowRollsTheCombinedNodesBeforeTheWorkers applies the roll of a cluster whose combined nodes and
// workers are outdated: the combined nodes are replaced first, then the workers, and the progress lines match a golden
// file.
func TestRollCombinedFlowRollsTheCombinedNodesBeforeTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := combinedClusterFlowWorld(t)
		oldNodes, oldWorkers := serverMachines(f, combinedGroup), workerIDs(f)
		before := workerHashes(f)
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 5, Drained: 5, Stopped: 3, Deleted: 5, Purged: 5}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		checkGolden(t, "flow_roll_combined_cluster.steps.golden", strings.Join(*lines, "\n")+"\n")
		wantInOrder(t, *lines, "nomad done purge prod-all-0", "node started create prod-workers-2")
		wantWorkersRolled(t, svc, f, before, append(idsOf(oldNodes), oldWorkers...))
		wantServersRolled(t, svc, f, combinedGroup, oldNodes, true)
		wantCombinedNodesReady(t, svc, f)
		inv.wantKept()
	})
}

// TestRollCombinedFlowForceReplacesEveryNodeOnceWhenTheyAreTheOnlyClients forces the roll of an up-to-date cluster of
// three combined nodes and no workers, so that the nodes are the only clients: each is replaced once, the new nodes
// have the hash of the old ones, and the machines that the roll makes are not forced, so it ends.
func TestRollCombinedFlowForceReplacesEveryNodeOnceWhenTheyAreTheOnlyClients(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, combinedDrainYAML)
		w.ServersOverTime()
		w.SetDrainReads(drainReads)
		mustUpdate(t, svc)
		inv := watchCombined(t, f, w, combinedGroup, 3)
		old := serverMachines(f, combinedGroup)

		plan, err := applyRoll(svc, app.RollOptions{Force: true})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Drained: 3, Stopped: 3, Deleted: 3, Purged: 3}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, combinedGroup, old, false)
		wantCombinedNodesReady(t, svc, f)
		inv.wantKept()
	})
}

// TestRollCombinedFlowRollsTheSelectedWorkersAndLeavesTheOutdatedCombinedNodes selects the workers while the combined
// nodes are outdated too: the workers roll alone, no combined machine or node is touched, and the next plan of the
// combined group still lists its three outdated nodes.
func TestRollCombinedFlowRollsTheSelectedWorkersAndLeavesTheOutdatedCombinedNodes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedClusterFlowWorld(t)
		oldNodes, oldWorkers := serverMachines(f, combinedGroup), workerIDs(f)
		before := workerHashes(f)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{"workers"}})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		for _, line := range *lines {
			if strings.Contains(line, machinePrefix(combinedGroup)) {
				t.Errorf("the roll reported %q, want no step of a combined node", line)
			}
		}
		for _, call := range nomadWriteCalls(w, nomadCalls) {
			_, arg, _ := strings.Cut(call, " ")
			subject, _, _ := strings.Cut(arg, " ")
			if slices.ContainsFunc(oldNodes, func(m serverMachine) bool {
				return slices.Contains([]string{nodeIDOf(m.id), raftIDOf(m.id), m.name, m.name + ".global"}, subject)
			}) {
				t.Errorf("the roll wrote %q to Nomad, want nothing of a combined node", call)
			}
		}
		for _, c := range f.Calls()[cloudCalls:] {
			if slices.Contains(idsOf(oldNodes), c.Arg) && !strings.HasPrefix(c.Name, "List") &&
				!strings.HasPrefix(c.Name, "Get") {
				t.Errorf("the roll called %s on the combined machine %s, want only reads", c.Name, c.Arg)
			}
		}
		if diff := cmp.Diff(idsOf(oldNodes), idsOf(serverMachines(f, combinedGroup))); diff != "" {
			t.Errorf("the combined machines (-want +got):\n%s", diff)
		}
		for _, id := range oldWorkers {
			if hasInstance(f, id) {
				t.Errorf("the old worker machine %s is still there", id)
			}
		}
		if got := len(workerIDs(f)); got != workerCount {
			t.Errorf("the workers have %d machines, want %d", got, workerCount)
		}
		wantNewWorkerHashes(t, f, before)
		wantCombinedNodesReady(t, svc, f)
		after, err := rollingUpdate(svc, app.RollOptions{})
		if err != nil {
			t.Fatalf("RollingUpdate after the roll: %v", err)
		}
		if len(after.Groups) == 0 || after.Groups[0].Name != combinedGroup || len(after.Groups[0].Outdated) != 3 {
			t.Errorf("the groups after the roll are %+v, want the combined group with three outdated nodes", after.Groups)
		}
		inv.wantKept()
	})
}

// newCombinedSingleWorld returns a singleWorld of a cluster of one outdated combined node and no workers, after an
// update that applied a change of the cluster's server configuration. A drain reads three times before it completes,
// and the invariants of a group of one combined node watch.
func newCombinedSingleWorld(t *testing.T) *singleWorld {
	t.Helper()
	svc, f, w := newRelease(t, keyedClusterYAML, edit(t, combinedDrainYAML, "size: 3", "size: 1"))
	w.ServersOverTime()
	w.SetDrainReads(drainReads)
	w.SetReconcile(time.Minute, 10*time.Second)
	w.SetHaltLag(singleHaltLag)
	mustUpdate(t, svc)
	mustReplace(t, svc, keyedClusterYAML+serversExtraYAML)
	mustUpdate(t, svc)
	s := &singleWorld{svc: svc, f: f, w: w, inv: watchCombined(t, f, w, combinedGroup, 1),
		old: serverMachines(f, combinedGroup), creates: countCalls(f, "CreateInstance")}
	s.hooks(nil, nil)
	return s
}

// wantPurgedAfterDown fails the test unless the roll sent one Purge, defaultDownAfter after its one HaltInstance,
// within a poll, after the delete and the wait for the node to read down that the progress lines show.
func (s *singleWorld) wantPurgedAfterDown(t *testing.T, lines []stamped) {
	t.Helper()
	halted, purged := s.haltTimes(), s.inv.reads("Purge")
	if len(halted) != 1 || len(purged) != 1 {
		t.Fatalf("the roll sent %d HaltInstance and %d Purge calls, want one of each", len(halted), len(purged))
	}
	if since := purged[0].Sub(halted[0]); since < defaultDownAfter || since >= defaultDownAfter+rollPollSlack {
		t.Errorf("the roll purged the old node %v after its halt, want %v, within a poll", since, defaultDownAfter)
	}
	name := s.old[0].name
	deleted := whenLine(lines, "node done delete "+name)
	if started := whenLine(lines, "nomad started down "+name); started.Before(deleted) || !started.Before(purged[0]) {
		t.Errorf("the wait for the node to read down started at %v, want it from the delete at %v to the purge at %v",
			started, deleted, purged[0])
	}
}

// TestRollCombinedFlowRollsASingleNodeThroughTwoVoters applies the roll of a cluster whose only node is a combined one,
// where Nomad's leader adds a removed server again every minute and a halted machine runs for 9 s more as far as
// Nomad goes. The new node is created first and boots without bootstrap_expect; the old node is drained into it and
// the leadership moves; the old peer is removed while it runs and again after the leader added it; the machine is
// halted in the 10 s after the reconcile at two minutes after the transfer, which is when a held stop goes out;
// nothing waits for the server to go down, and the node is purged only after it reads down, about 20 s after the
// halt. The progress lines match a golden file, and the voters that run are a quorum for two reconciles and a
// promotion after the last write.
func TestRollCombinedFlowRollsASingleNodeThroughTwoVoters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newCombinedSingleWorld(t)
		lines := stampProgress(s.svc)

		plan, err := applyRoll(s.svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 1, Drained: 1, Stopped: 1, Deleted: 1, Purged: 1}); !plan.Applied ||
			plan.Rolled != want {
			t.Errorf("the plan says applied %v and rolled %+v, want applied and %+v", plan.Applied, plan.Rolled, want)
		}
		var text []string
		for _, l := range *lines {
			text = append(text, l.line)
		}
		checkGolden(t, "flow_roll_combined_single.steps.golden", strings.Join(text, "\n")+"\n")
		old := s.old[0]
		for line, want := range map[string]int{
			"nomad started remove-peer " + old.name: 2, "nomad started reconcile " + old.name: 1,
			"nomad started down " + old.name: 1, "nomad done down " + old.name: 1,
			"nomad started server-down " + old.name: 0,
		} {
			if got := count(text, line); got != want {
				t.Errorf("the progress has %q %d times, want %d", line, got, want)
			}
		}
		s.wantHaltIn(t, 2*time.Minute, 2*time.Minute+10*time.Second)
		s.wantPurgedAfterDown(t, *lines)
		oldRaft := raftIDOf(old.id)
		if diff := cmp.Diff([]string{oldRaft, oldRaft}, nomadArgs(s.w, "RemovePeer")); diff != "" {
			t.Errorf("the peers that the roll removed (-want +got):\n%s", diff)
		}
		s.wantRemovedCalls(t, 0)
		nc := configOf(t, s.f, "prod-all-1")
		node := fileOf(nc, "/etc/nomad.d/10-node.hcl")
		if node == nil || strings.Contains(string(node.Content), "bootstrap_expect") {
			t.Errorf("the new node's 10-node.hcl is %+v, want a file with no bootstrap_expect", node)
		}
		s.wantNewServers(t, 1)
		s.inv.wantQuorumFor(quorumSpan)
		s.inv.wantKept()
		wantRollEnded(t, s.svc, s.f, idsOf(s.old))
		wantServersRolledOf(t, s.svc, s.f, combinedGroup, 1, s.old, true)
		wantCombinedNodesReady(t, s.svc, s.f)
	})
}

// TestRollCombinedFlowRefusesAGroupWhoseRoleChanged takes a cluster that a role change of one node group left behind,
// from server to combined and from combined to server: the stored spec has the new role and the machines have the old
// one, and an update applied the stored specs. The plan refuses with the machine and both roles, and a run with apply
// returns the same refusal, takes no lock and only reads.
func TestRollCombinedFlowRefusesAGroupWhoseRoleChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		docs     []string
		path     string // of the group's stored spec
		old, now string // the role of the group's machines and the role in its stored spec
		group    string
		from     string // the spec that the role changes in
		refusal  string
	}{
		{"server to combined", []string{keyedClusterYAML, serversYAML, workersYAML}, serversPath, "server", "combined",
			"servers", serversYAML, roleRefusal("servers", "prod-servers-0", "a server", "combined")},
		{"combined to server", []string{keyedClusterYAML, combinedYAML, workersYAML}, "prod/nodegroups/all.yaml",
			"combined", "server", combinedGroup, combinedYAML,
			roleRefusal(combinedGroup, "prod-all-0", "a combined node", "server")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := newRelease(t, tc.docs...)
				w.ServersOverTime()
				mustUpdate(t, svc)
				put(t, svc.Store, tc.path, encode(t, edit(t, tc.from, "role: "+tc.old, "role: "+tc.now)))
				mustUpdate(t, svc)
				u := watch(t, svc, f, w)

				planned, planErr := rollingUpdate(svc, app.RollOptions{})
				applied, err := applyRoll(svc, app.RollOptions{})

				wantError(t, planErr, tc.refusal)
				wantError(t, err, tc.refusal)
				u.check(t)
				for _, plan := range []app.RollPlan{planned, applied} {
					if i := slices.IndexFunc(plan.Groups, func(g app.RollGroup) bool { return g.Name == tc.group }); i < 0 ||
						string(plan.Groups[i].Role) != tc.now || plan.Next != nil || plan.Applied {
						t.Errorf("plan = %+v, want the group %s as %s, no next step and not applied", plan, tc.group, tc.now)
					}
				}
			})
		})
	}
}

// roleRefusal is the refusal of a group whose role changed, for its first machine called machine, which was created as
// created and which the group's stored spec now gives the role now.
func roleRefusal(group, machine, created, now string) string {
	return fmt.Sprintf("node group %s: node %s was created as %s and the group is now %s; tent does not replace or "+
		"remove the nodes of a group whose role changed; a node group keeps its role, and a cluster moves by creating "+
		"a new one", group,
		machine, created, now)
}
