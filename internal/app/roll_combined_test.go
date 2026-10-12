package app_test

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

const (
	// combinedGroup is the name of the combined node group of the test cluster.
	combinedGroup = "all"
	// drainReads is how many reads of the nodes show a drain of the combined worlds under way; the next read shows it
	// complete.
	drainReads = 3
	// combinedDrainTimeout is the drainTimeout of the combined group.
	combinedDrainTimeout = 30 * time.Minute
	// combinedDrainYAML is the combined group of the test cluster with its drainTimeout.
	combinedDrainYAML = combinedYAML + "  rollingUpdate:\n    drainTimeout: 30m\n"
)

// combinedRollWorld is the test cluster with the combined group all, three nodes that voted and registered, and the
// workers. A second update applied a change of the cluster's server configuration, so the three combined nodes are
// outdated and the workers are not. Its servers change over time, a drain reads three times before it completes, and
// setups set the world before the first update.
func combinedRollWorld(t *testing.T, setups ...func(*nomadWorld)) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t, keyedClusterYAML, combinedDrainYAML, workersYAML)
	w.ServersOverTime()
	w.SetDrainReads(drainReads)
	for _, setup := range setups {
		setup(w)
	}
	mustUpdate(t, svc)
	mustReplace(t, svc, keyedClusterYAML+serversExtraYAML)
	mustUpdate(t, svc)
	return svc, f, w
}

// combinedFlowWorld is combinedRollWorld with the invariants of a combined group watching.
func combinedFlowWorld(
	t *testing.T, setups ...func(*nomadWorld),
) (*app.Service, *vultrfake.Fake, *nomadWorld, *serverInvariants) {
	t.Helper()
	svc, f, w := combinedRollWorld(t, setups...)
	return svc, f, w, watchCombined(t, f, w, combinedGroup, 3)
}

// nodeNames returns the names of the nodes that Nomad lists, sorted.
func nodeNames(t *testing.T, svc *app.Service) []string {
	t.Helper()
	var out []string
	for _, n := range nomadNodes(t, svc) {
		out = append(out, n.Name)
	}
	slices.Sort(out)
	return out
}

// nomadWriteCalls returns the writes to Nomad after the first n calls, as "Name Arg".
func nomadWriteCalls(w *nomadWorld, n int) []string {
	var out []string
	for _, c := range w.Log()[n:] {
		if !slices.Contains(nomadReads, c.Name) {
			out = append(out, strings.TrimSpace(c.Name+" "+c.Arg))
		}
	}
	return out
}

// combinedRefusal is what a run says when the node of the machine called name cannot take work, for reason.
func combinedRefusal(name, reason string) string {
	return fmt.Sprintf("node group %s: node %s %s; run tent validate cluster to see what is wrong", combinedGroup, name,
		reason)
}

// TestRollLoopReplacesEveryCombinedNode rolls a combined group of three: each old node is replaced by a new one, the
// roll counts what it did, the invariants hold at every call, and Nomad lists the three new machines as voters and
// as ready nodes and nothing of the old ones.
func TestRollLoopReplacesEveryCombinedNode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 3, Drained: 3, Stopped: 3, Deleted: 3, Purged: 3})
		inv.wantKept()
		wantServersRolled(t, svc, f, combinedGroup, old, true)
		wantRollEnded(t, svc, f, idsOf(old))
		want := []string{"prod-all-3", "prod-all-4", "prod-all-5", "prod-workers-0", "prod-workers-1"}
		if got := nodeNames(t, svc); !slices.Equal(got, want) {
			t.Errorf("Nomad lists the nodes %v, want %v", got, want)
		}
	})
}

// TestRollLoopCreatesCombinedNodesWithTheSeedAndNoIntroToken boots each new combined node with the private addresses of
// the listed machines of the group other than itself, and without an intro token, and asks Nomad for no intro token.
func TestRollLoopCreatesCombinedNodesWithTheSeedAndNoIntroToken(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := combinedFlowWorld(t)
		address := map[string]string{}
		for _, m := range serverMachines(f, combinedGroup) {
			address[m.name] = privateOf(t, f, m.id).String()
		}
		nomadCalls := len(w.Log())

		_, err := roll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("the roll failed: %v", err)
		}
		for _, m := range serverMachines(f, combinedGroup) {
			address[m.name] = privateOf(t, f, m.id).String()
		}
		for name, others := range map[string][]string{
			"prod-all-3": {"prod-all-0", "prod-all-1", "prod-all-2"},
			"prod-all-4": {"prod-all-0", "prod-all-2", "prod-all-3"},
			"prod-all-5": {"prod-all-0", "prod-all-3", "prod-all-4"},
		} {
			var want []string
			for _, o := range others {
				want = append(want, address[o])
			}
			nc := configOf(t, f, name)
			if got := seedOf(nc); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
				t.Errorf("the seed of %s is %v, want the addresses %v of %v", name, got, want, others)
			}
			if claims := introClaims(t, nc); claims != nil {
				t.Errorf("%s has an intro token", name)
			}
		}
		for _, c := range w.Log()[nomadCalls:] {
			if c.Name == "IntroToken" {
				t.Errorf("the roll asked Nomad for an intro token: %s", c.Arg)
			}
		}
	})
}

// TestRollLoopReplacesACombinedNodeStepByStep carries out each step of the replacement of the first outdated combined
// node, in order, with its calls: a new node that joins and is scrubbed, the window, the mark, the drain with the
// group's drainTimeout and the machine in its meta, the wait for the drain over at least three reads, the stop, the
// waits, the removal from the Raft configuration and the gossip pool, the delete and the purge of the node.
func TestRollLoopReplacesACombinedNodeStepByStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := instanceNamed(t, f, "prod-all-1")
		lines := recordProgress(svc)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		counts, err := rollUntil(svc, "nomad done purge prod-all-1")

		wantInterrupted(t, err)
		if want := (app.RollCounts{Created: 1, Drained: 1, Stopped: 1, Deleted: 1, Purged: 1}); counts != want {
			t.Errorf("the roll did %+v, want %+v", counts, want)
		}
		wantLines(t, *lines, []string{
			"node started create prod-all-3", "node done create prod-all-3",
			"nomad started join prod-all-3", "nomad done join prod-all-3",
			"node started scrub prod-all-3", "node done scrub prod-all-3",
			"nomad started stable", "nomad done stable",
			"nomad started ineligible prod-all-1", "nomad done ineligible prod-all-1",
			"nomad started drain prod-all-1", "nomad done drain prod-all-1",
			"nomad started drained prod-all-1", "nomad done drained prod-all-1",
			"node started stop prod-all-1", "node done stop prod-all-1",
			"nomad started server-down prod-all-1", "nomad done server-down prod-all-1",
			"nomad started remove-peer prod-all-1", "nomad done remove-peer prod-all-1",
			"nomad started force-leave prod-all-1.global", "nomad done force-leave prod-all-1.global",
			"nomad started healthy", "nomad done healthy",
			"node started delete prod-all-1", "node done delete prod-all-1",
			"nomad started purge prod-all-1", "nomad done purge prod-all-1",
		})
		drain := nomadfake.DrainArg(nodeIDOf(old), nomadops.DrainRequest{
			Deadline: combinedDrainTimeout, Meta: map[string]string{"tent_machine": old},
		})
		wantNomad := []string{
			"MarkIneligible " + nodeIDOf(old), "Drain " + drain, "RemovePeer " + raftIDOf(old),
			"ForceLeave prod-all-1.global", "Purge " + nodeIDOf(old),
		}
		if diff := cmp.Diff(wantNomad, nomadWriteCalls(w, nomadCalls)); diff != "" {
			t.Errorf("the writes to Nomad (-want +got):\n%s", diff)
		}
		wantWrites := []string{"CreateInstance", "UpdateInstance", "HaltInstance", "DeleteInstance"}
		var writes []string
		for _, c := range f.Calls()[cloudCalls:] {
			if slices.Contains(wantWrites, c.Name) {
				writes = append(writes, c.Name)
			}
		}
		if diff := cmp.Diff(wantWrites, writes); diff != "" {
			t.Errorf("the writes to the cloud (-want +got):\n%s", diff)
		}
		halted, deleted := callsOf(f, "HaltInstance"), callsOf(f, "DeleteInstance")
		if !slices.Equal(halted, []string{old}) || !slices.Equal(deleted, []string{old}) {
			t.Errorf("the roll halted %v and deleted %v, want only %s", halted, deleted, old)
		}
		if reads := nodeReadsBeforeTheNodeLeaves(t, f, w, old); reads < drainReads+2 {
			t.Errorf("the roll read the nodes %d times from the Drain to the stop, want at least %d: the %d that show the "+
				"drain under way, the one that shows it complete and the one with the list before the stop", reads,
				drainReads+2, drainReads)
		}
		inv.wantKept()
	})
}

// TestRollLoopSendsNoDrainWhileAnotherCombinedNodeCannotTakeWork ends a run that resumes at a node that was marked
// ineligible with the refusal, and sends no write, when another node of the group is down, is ineligible or drains:
// the allocations of the marked node would have nowhere to go.
func TestRollLoopSendsNoDrainWhileAnotherCombinedNodeCannotTakeWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*nomadops.Node)
		reason string
	}{
		{"a node that is down", func(n *nomadops.Node) { n.Status = "down" }, "is down in Nomad"},
		{"a node that is ineligible", func(n *nomadops.Node) { n.Eligible = false }, "is not eligible"},
		{"a node that drains", func(n *nomadops.Node) { n.Draining = true }, "is draining"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := combinedFlowWorld(t)
				_, err := rollUntil(svc, "nomad done ineligible prod-all-1")
				wantInterrupted(t, err)
				w.ChangeNode("prod-all-3", tc.change)
				cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

				counts, err := roll(svc, app.RollOptions{})

				wantMatchesRefusal(t, err, combinedRefusal("prod-all-3", tc.reason))
				wantNoWrites(t, f.Calls()[cloudCalls:])
				if got := nomadWrites(w, nomadCalls); len(got) > 0 {
					t.Errorf("the roll wrote to Nomad: %q", got)
				}
				if counts != (app.RollCounts{}) {
					t.Errorf("the roll did %+v, want nothing", counts)
				}
				inv.wantKept()
			})
		})
	}
}

// TestRollLoopSettlesTheRefusalOfAnotherNodeForAMinuteAfterAWrite sends no drain when another node goes down right
// after the run marked the old node, and waits a minute for the node before it ends with the refusal, since the run
// has sent a write.
func TestRollLoopSettlesTheRefusalOfAnotherNodeForAMinuteAfterAWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, inv := combinedFlowWorld(t)
		run := recordSettles(svc)
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if progressLine(p) == "nomad done ineligible prod-all-1" {
				w.ChangeNode("prod-all-3", func(n *nomadops.Node) { n.Status = "down" })
			}
		}

		_, err := rollBefore(svc, settleLimit)

		want := combinedRefusal("prod-all-3", "is down in Nomad")
		wantMatchesRefusal(t, err, want)
		if run.began.IsZero() {
			t.Fatal("the run did not wait for the refusal to clear")
		}
		wantWithin(t, time.Since(run.began), settleLimit)
		wantLines(t, run.lines[len(run.lines)-2:], []string{"nomad started settle", "nomad failed settle: " + want})
		for _, c := range w.Log() {
			if c.Name == "Drain" {
				t.Errorf("the roll sent %s %s", c.Name, c.Arg)
			}
		}
		inv.wantKept()
	})
}

// TestRollLoopSendsNoStopOrTransferWhenAMachineWasHaltedDuringTheDrain halts a new node from outside while an old node
// drains: when the drain is complete the run lists the machines, sends no halt of a follower and no transfer of the
// leadership from the leader, waits a minute for the machine, and ends with the refusal.
func TestRollLoopSendsNoStopOrTransferWhenAMachineWasHaltedDuringTheDrain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, victim string
		stopped      []string // the old nodes that the roll stops before the victim drains
	}{
		{"a follower, which the run would stop", "prod-all-1", nil},
		{"the leader, which the run would ask to hand over", "prod-all-0", []string{"prod-all-1", "prod-all-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := combinedFlowWorld(t)
				var wantHalts []string
				for _, name := range tc.stopped {
					wantHalts = append(wantHalts, instanceNamed(t, f, name))
				}
				run := recordSettles(svc)
				inner := svc.OnProgress
				svc.OnProgress = func(p app.Progress) {
					inner(p)
					if progressLine(p) == "nomad started drained "+tc.victim {
						halted := instanceNamed(t, f, "prod-all-3")
						wantHalts = append(wantHalts, halted)
						inv.halt(halted)
					}
				}

				_, err := rollBefore(svc, settleLimit)

				want := combinedRefusal("prod-all-3", "is not running")
				wantMatchesRefusal(t, err, want)
				if run.began.IsZero() {
					t.Fatal("the run did not wait for the refusal to clear")
				}
				wantWithin(t, time.Since(run.began), settleLimit)
				if halts := callsOf(f, "HaltInstance"); !slices.Equal(halts, wantHalts) {
					t.Errorf("HaltInstance was called for %v, want %v: the nodes before the victim and the machine that "+
						"the test halted", halts, wantHalts)
				}
				for _, c := range w.Log() {
					if c.Name == "TransferLeadership" {
						t.Errorf("the roll sent a transfer of the leadership")
					}
				}
				inv.wantKept()
			})
		})
	}
}

// TestRollLoopFinishesWhenAnOldMachineIsHaltedWhileTheFirstVictimDrains halts an old machine from outside while the
// first victim drains. The roll removes the halted machine, which brings the group back to its size with the victim's
// node ineligible, and goes on: the roll drains and stops two old nodes, deletes three and ends with three new nodes,
// and Nomad lists nothing of the old ones.
func TestRollLoopFinishesWhenAnOldMachineIsHaltedWhileTheFirstVictimDrains(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		svc.OnProgress = func(p app.Progress) {
			if progressLine(p) == "nomad started drained prod-all-1" {
				inv.halt(instanceNamed(t, f, "prod-all-2"))
			}
		}

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 3, Drained: 2, Stopped: 2, Deleted: 3, Purged: 3})
		inv.wantKept()
		wantServersRolled(t, svc, f, combinedGroup, old, true)
		wantRollEnded(t, svc, f, idsOf(old))
		want := []string{"prod-all-3", "prod-all-4", "prod-all-5", "prod-workers-0", "prod-workers-1"}
		if got := nodeNames(t, svc); !slices.Equal(got, want) {
			t.Errorf("Nomad lists the nodes %v, want %v", got, want)
		}
	})
}

// TestRollLoopEndsAtOnceWhenAMachineWasHaltedBeforeTheRunResumedAtADrainedNode ends a run that resumes at a drained old
// node, after a machine of the group was halted, with the refusal: nothing was written yet, so it does not wait.
func TestRollLoopEndsAtOnceWhenAMachineWasHaltedBeforeTheRunResumedAtADrainedNode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		_, err := rollUntil(svc, "nomad done drained prod-all-1")
		wantInterrupted(t, err)
		inv.halt(instanceNamed(t, f, "prod-all-3"))
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		start := time.Now()

		counts, err := roll(svc, app.RollOptions{})

		wantMatchesRefusal(t, err, combinedRefusal("prod-all-3", "is not running"))
		if got := time.Since(start); got != 0 {
			t.Errorf("the run took %v, want it to end at the first decision", got)
		}
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		if counts != (app.RollCounts{}) {
			t.Errorf("the roll did %+v, want nothing", counts)
		}
		inv.wantKept()
	})
}

// TestRollLoopSettlesOnceAfterTheTransferOfALeadingCombinedNode reads the cluster unhealthy for 2 s after the transfer
// of the leadership, as Nomad does after some: the removal of the leader goes on after one settle.
func TestRollLoopSettlesOnceAfterTheTransferOfALeadingCombinedNode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := combinedFlowWorld(t, func(w *nomadWorld) { w.SetTransferBlip(2 * time.Second) })
		old := serverMachines(f, combinedGroup)
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 3, Drained: 3, Stopped: 3, Deleted: 3, Purged: 3})
		wantInOrder(t, *lines, "nomad started transfer prod-all-0", "nomad done transfer prod-all-0",
			"nomad started settle", "nomad done settle", "node started stop prod-all-0")
		if got := count(*lines, "nomad started settle"); got != 1 {
			t.Errorf("the roll waited for the cluster to settle %d times, want once", got)
		}
		wantRollEnded(t, svc, f, idsOf(old))
		inv.wantKept()
	})
}

// TestRollLoopStopsADrainedLeaderOnlyAfterTheWindowThatFollowsItsTransfer waits the stability window, 70 s, after the
// transfer of the leadership before it stops the leader's machine, and waits for no node to go down: each node reads
// down before the delete of its machine.
func TestRollLoopStopsADrainedLeaderOnlyAfterTheWindowThatFollowsItsTransfer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, inv := combinedFlowWorld(t)
		lines := stampProgress(svc)

		_, err := roll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("the roll failed: %v", err)
		}
		transferred := whenLine(*lines, "nomad done transfer prod-all-0")
		stopped := whenLine(*lines, "node started stop prod-all-0")
		if gap := stopped.Sub(transferred); transferred.IsZero() || gap < stopWindow || gap > 70*time.Second+loopSlack {
			t.Errorf("the stop came %v after the transfer, want the window of 70 s", gap)
		}
		for _, name := range []string{"prod-all-0", "prod-all-1", "prod-all-2"} {
			if at := whenLine(*lines, "nomad started down "+name); !at.IsZero() {
				t.Errorf("the roll waited for the node %s to go down, want its node down before the delete", name)
			}
		}
		inv.wantKept()
	})
}

// combinedKit is a combined cluster after an update, with a client of its Nomad and invariants that watch it.
type combinedKit struct {
	f   *vultrfake.Fake
	w   *nomadWorld
	api nomadops.API
	inv *serverInvariants
}

// id returns the instance ID of the machine called name.
func (k combinedKit) id(t *testing.T, name string) string { return instanceNamed(t, k.f, name) }

// node returns the ID of the node of the machine called name.
func (k combinedKit) node(t *testing.T, name string) string { return nodeIDOf(k.id(t, name)) }

// mark marks the node of the machine called name ineligible.
func (k combinedKit) mark(t *testing.T, name string) {
	t.Helper()
	if err := k.api.MarkIneligible(t.Context(), k.node(t, name)); err != nil {
		t.Fatalf("MarkIneligible: %v", err)
	}
}

// drain drains the node of the machine called name for the machine called forName, and reads the nodes the given
// number of times.
func (k combinedKit) drain(t *testing.T, name, forName string, reads int) {
	t.Helper()
	req := nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": k.id(t, forName)}}
	if err := k.api.Drain(t.Context(), k.node(t, name), req); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	for range reads {
		if _, err := k.api.Nodes(t.Context()); err != nil {
			t.Fatalf("Nodes: %v", err)
		}
	}
}

// retire marks the node of the machine called name ineligible, reads the nodes, as the roll does after a write, and
// drains the node for the machine, to the end.
func (k combinedKit) retire(t *testing.T, name string) {
	t.Helper()
	k.mark(t, name)
	if _, err := k.api.Nodes(t.Context()); err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	k.drain(t, name, name, 4)
}

// halt halts the machine called name as the roll would.
func (k combinedKit) halt(t *testing.T, name string) {
	t.Helper()
	if err := k.f.HaltInstance(t.Context(), k.id(t, name)); err != nil {
		t.Fatalf("HaltInstance: %v", err)
	}
}

// combinedCase is a world that breaks one invariant of a combined group, or none.
type combinedCase struct {
	name    string
	plain   bool // the invariants of a group that is not combined watch
	breakIt func(t *testing.T, k combinedKit)
	want    string // a part of the failure; empty when the case expects none
	without string // a part that no failure has; empty for none
}

// wantCombinedInvariants runs each case on a healthy combined cluster of three nodes with the invariants watching,
// which fail the test only for what the case breaks.
func wantCombinedInvariants(t *testing.T, cases []combinedCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := combinedRollWorld(t)
				api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
				if err != nil {
					t.Fatalf("Nomad: %v", err)
				}
				ftb := &failureTB{TB: t}
				watchGroup := watchCombined
				if tc.plain {
					watchGroup = watchServers
				}
				k := combinedKit{f: f, w: w, api: api, inv: watchGroup(ftb, f, w, combinedGroup, 3)}
				if _, err := api.Nodes(t.Context()); err != nil {
					t.Fatalf("Nodes: %v", err)
				}
				if got := ftb.failures(); len(got) > 0 {
					t.Fatalf("the invariants failed for a healthy cluster: %q", got)
				}

				tc.breakIt(t, k)

				got := strings.Join(ftb.failures(), "\n")
				if tc.want != "" && !strings.Contains(got, tc.want) {
					t.Errorf("the invariants failed with %q, want a failure that says %q", got, tc.want)
				}
				if tc.without != "" && strings.Contains(got, tc.without) {
					t.Errorf("the invariants failed with %q, want no failure that says %q", got, tc.without)
				}
			})
		})
	}
}

// TestCombinedInvariantsFailTheTestForAHaltOfANodeThatIsNotDrained halts a machine of a combined group whose node is
// eligible, drains, was marked and not drained, or was drained for another machine, and fails the test for each. It
// fails nothing for a node that is ineligible and drained for the machine, that is down or not listed, for a group
// that is not combined, and for a machine that the test halted itself.
func TestCombinedInvariantsFailTheTestForAHaltOfANodeThatIsNotDrained(t *testing.T) {
	t.Parallel()
	const fails = "halt of "
	wantCombinedInvariants(t, []combinedCase{
		{name: "a node that is eligible", breakIt: func(t *testing.T, k combinedKit) {
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a node that is eligible after its drain", breakIt: func(t *testing.T, k combinedKit) {
			k.retire(t, "prod-all-1")
			k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) { n.Eligible = true })
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a node that drains again after its drain", breakIt: func(t *testing.T, k combinedKit) {
			k.retire(t, "prod-all-1")
			k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) { n.Draining = true })
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a drain that is not complete after its drain", breakIt: func(t *testing.T, k combinedKit) {
			k.retire(t, "prod-all-1")
			k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) { n.LastDrain.Status = "draining" })
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a node that drains", breakIt: func(t *testing.T, k combinedKit) {
			k.mark(t, "prod-all-1")
			k.drain(t, "prod-all-1", "prod-all-1", 1)
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a node that was marked and not drained", breakIt: func(t *testing.T, k combinedKit) {
			k.mark(t, "prod-all-1")
			if _, err := k.api.Nodes(t.Context()); err != nil {
				t.Fatalf("Nodes: %v", err)
			}
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a drain for another machine", breakIt: func(t *testing.T, k combinedKit) {
			k.mark(t, "prod-all-1")
			k.drain(t, "prod-all-1", "prod-all-2", 4)
			k.halt(t, "prod-all-1")
		}, want: fails},
		{name: "a node that is ineligible and drained for the machine", breakIt: func(t *testing.T, k combinedKit) {
			k.retire(t, "prod-all-1")
			k.halt(t, "prod-all-1")
		}, without: fails},
		{name: "a node that is down", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) { n.Status = "down" })
			k.halt(t, "prod-all-1")
		}, without: fails},
		{name: "a node that is not listed", breakIt: func(t *testing.T, k combinedKit) {
			k.w.DropNode("prod-all-1")
			k.halt(t, "prod-all-1")
		}, without: fails},
		{name: "a machine of another group", breakIt: func(t *testing.T, k combinedKit) {
			k.halt(t, "prod-workers-0")
		}, without: fails},
		{name: "a group that is not combined", plain: true, breakIt: func(t *testing.T, k combinedKit) {
			k.halt(t, "prod-all-1")
		}, without: fails},
		{name: "a machine that the test halted itself", breakIt: func(t *testing.T, k combinedKit) {
			k.inv.halt(k.id(t, "prod-all-1"))
		}, without: fails},
	})
}

// TestCombinedInvariantsFailTheTestForAMarkOrADrainBesideANodeThatCannotTakeWork marks or drains a node of a combined
// group while another node is down, ineligible, draining or not listed, and fails the test for each. It fails nothing
// when the other nodes can take work, for a node of no machine of the group, for a group that is not combined, and
// after the test halted a machine itself.
func TestCombinedInvariantsFailTheTestForAMarkOrADrainBesideANodeThatCannotTakeWork(t *testing.T) {
	t.Parallel()
	const fails = "mark or drain of "
	down := func(n *nomadops.Node) { n.Status = "down" }
	wantCombinedInvariants(t, []combinedCase{
		{name: "a mark beside a node that is down", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-2", down)
			k.mark(t, "prod-all-1")
		}, want: fails},
		{name: "a mark beside a node that is ineligible", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-2", func(n *nomadops.Node) { n.Eligible = false })
			k.mark(t, "prod-all-1")
		}, want: fails},
		{name: "a mark beside a node that drains", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-2", func(n *nomadops.Node) { n.Draining = true })
			k.mark(t, "prod-all-1")
		}, want: fails},
		{name: "a mark beside a node that is not listed", breakIt: func(t *testing.T, k combinedKit) {
			k.w.DropNode("prod-all-2")
			k.mark(t, "prod-all-1")
		}, want: fails},
		{name: "a drain beside a node that is down", breakIt: func(t *testing.T, k combinedKit) {
			k.mark(t, "prod-all-1")
			k.w.ChangeNode("prod-all-2", down)
			k.drain(t, "prod-all-1", "prod-all-1", 0)
		}, want: fails},
		{name: "a mark and a drain beside nodes that can take work", breakIt: func(t *testing.T, k combinedKit) {
			k.retire(t, "prod-all-1")
		}, without: fails},
		{name: "a mark beside a node of another group that is down", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-workers-0", down)
			k.mark(t, "prod-all-1")
		}, without: fails},
		{name: "a mark of a node of another group", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-2", down)
			k.mark(t, "prod-workers-0")
		}, without: fails},
		{name: "a group that is not combined", plain: true, breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-2", down)
			k.mark(t, "prod-all-1")
		}, without: fails},
		{name: "a mark after the test halted a machine itself", breakIt: func(t *testing.T, k combinedKit) {
			k.inv.halt(k.id(t, "prod-all-2"))
			k.w.ChangeNode("prod-all-2", down)
			k.mark(t, "prod-all-1")
		}, without: fails},
	})
}

// TestCombinedInvariantsFailTheTestForAPurgeOfANodeThatIsNotGone purges a node of a combined group that is up, or that
// is down while a machine has its name and address, and fails the test for each. It fails nothing for a node that is
// down and whose machine is gone or has another address or name, for a node of another group, for a node that is not
// listed, and for a group that is not combined.
func TestCombinedInvariantsFailTheTestForAPurgeOfANodeThatIsNotGone(t *testing.T) {
	t.Parallel()
	const fails = "purge of "
	down := func(n *nomadops.Node) { n.Status = "down" }
	purge := func(t *testing.T, k combinedKit, id string) {
		t.Helper()
		if err := k.api.Purge(t.Context(), id); err != nil {
			t.Fatalf("Purge: %v", err)
		}
	}
	wantCombinedInvariants(t, []combinedCase{
		{name: "a node that is up", breakIt: func(t *testing.T, k combinedKit) {
			purge(t, k, k.node(t, "prod-all-1"))
		}, want: "Nomad lists the node ready, want down"},
		{name: "a node that is down whose machine is listed", breakIt: func(t *testing.T, k combinedKit) {
			k.w.ChangeNode("prod-all-1", down)
			purge(t, k, k.node(t, "prod-all-1"))
		}, want: "has the node's name and address"},
		{name: "a node that is down and whose machine is gone", breakIt: func(t *testing.T, k combinedKit) {
			id := k.node(t, "prod-all-1")
			k.w.ChangeNode("prod-all-1", down)
			if err := k.f.DeleteInstance(t.Context(), k.id(t, "prod-all-1")); err != nil {
				t.Fatalf("DeleteInstance: %v", err)
			}
			purge(t, k, id)
		}, without: fails},
		{name: "a node that is down at another address than its name's machine",
			breakIt: func(t *testing.T, k combinedKit) {
				k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) {
					n.Status, n.Address = "down", netip.MustParseAddr("192.0.2.1")
				})
				purge(t, k, k.node(t, "prod-all-1"))
			}, without: fails},
		{name: "a node that is down under another name than the machine of its address",
			breakIt: func(t *testing.T, k combinedKit) {
				k.w.ChangeNode("prod-all-1", func(n *nomadops.Node) { n.Status, n.Name = "down", "prod-all-9" })
				purge(t, k, k.node(t, "prod-all-1"))
			}, without: fails},
		{name: "a node of another group", breakIt: func(t *testing.T, k combinedKit) {
			purge(t, k, k.node(t, "prod-workers-0"))
		}, without: fails},
		{name: "a node that is not listed", breakIt: func(t *testing.T, k combinedKit) {
			purge(t, k, "n-instance-99")
		}, without: fails},
		{name: "a group that is not combined", plain: true, breakIt: func(t *testing.T, k combinedKit) {
			purge(t, k, k.node(t, "prod-all-1"))
		}, without: fails},
	})
}

// TestCombinedInvariantsFailTheTestForAnIntroTokenOfANodeOfTheGroup asks for an intro token for a node of a combined
// group and fails the test; it fails nothing for a node of another group or for a group that is not combined.
func TestCombinedInvariantsFailTheTestForAnIntroTokenOfANodeOfTheGroup(t *testing.T) {
	t.Parallel()
	const fails = "intro token for "
	intro := func(t *testing.T, k combinedKit, node string) {
		t.Helper()
		req := nomadops.IntroRequest{NodeName: node, NodePool: "default", TTL: time.Minute}
		if _, err := k.api.IntroToken(t.Context(), req); err != nil {
			t.Fatalf("IntroToken: %v", err)
		}
	}
	wantCombinedInvariants(t, []combinedCase{
		{name: "a node of the group", breakIt: func(t *testing.T, k combinedKit) {
			intro(t, k, "prod-all-3")
		}, want: fails + "prod-all-3 default"},
		{name: "a node of another group", breakIt: func(t *testing.T, k combinedKit) {
			intro(t, k, "prod-workers-3")
		}, without: fails},
		{name: "a group that is not combined", plain: true, breakIt: func(t *testing.T, k combinedKit) {
			intro(t, k, "prod-all-3")
		}, without: fails},
	})
}
