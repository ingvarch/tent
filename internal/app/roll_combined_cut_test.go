package app_test

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// combinedWrites are the writes that a roll of combined nodes makes, as the first words of a call.
var combinedWrites = []string{
	"CreateInstance ", "UpdateInstance ", "nomad MarkIneligible ", "nomad Drain ", "nomad TransferLeadership ",
	"HaltInstance ", "nomad RemovePeer ", "nomad ForceLeave ", "DeleteInstance ", "nomad Purge ",
}

// uninterruptedCallsOf applies the roll of the world that open makes and returns the calls it makes to Vultr and Nomad,
// as flowCalls gives them.
func uninterruptedCallsOf(t *testing.T, open func(*testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld)) []string {
	t.Helper()
	var calls []string
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := open(t)
		calls = flowCalls(t, svc, f, w, func() *engine.Plan {
			if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
				t.Fatalf("RollingUpdate: %v", err)
			}
			return nil
		})
	})
	return calls
}

// uninterruptedCombinedRoll returns the calls of the roll of the three outdated combined nodes.
func uninterruptedCombinedRoll(t *testing.T) []string {
	t.Helper()
	return uninterruptedCallsOf(t, func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
		svc, f, w, _ := combinedFlowWorld(t)
		return svc, f, w
	})
}

// uninterruptedCombinedSingleRoll returns the calls of the roll of the one outdated combined node.
func uninterruptedCombinedSingleRoll(t *testing.T) []string {
	t.Helper()
	return uninterruptedCallsOf(t, func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
		s := newCombinedSingleWorld(t)
		return s.svc, s.f, s.w
	})
}

// wantCombinedCutDone fails the test unless the roll of three combined nodes ended with the machines of old replaced,
// CreateInstance called creates times in all, the nodes ready and the invariants kept at every call.
func wantCombinedCutDone(
	t *testing.T, svc *app.Service, f *vultrfake.Fake, inv *serverInvariants, old []serverMachine, creates int,
) {
	t.Helper()
	wantServersCutDone(t, svc, f, inv, combinedGroup, old, creates)
	wantCombinedNodesReady(t, svc, f)
}

// wantCombinedFinished fails the test unless the roll of the single combined node has ended with one new node, the
// nodes ready, the invariants kept, and the voters that run a quorum for the time that quorumSpan gives.
func (s *singleWorld) wantCombinedFinished(t *testing.T) {
	t.Helper()
	s.inv.wantQuorumFor(quorumSpan)
	s.wantNewServers(t, 1)
	wantRollEnded(t, s.svc, s.f, idsOf(s.old))
	wantServersRolledOf(t, s.svc, s.f, combinedGroup, 1, s.old, true)
	wantCombinedNodesReady(t, s.svc, s.f)
	s.inv.wantKept()
}

// nthOfKind returns the key of the nth call (from 1) in calls whose key starts with kind, and how many calls up to it
// have that key, which is the n of the cut or the lost answer that stands at it.
func nthOfKind(t *testing.T, calls []string, kind string, n int) (key string, occurrence int) {
	t.Helper()
	seen := map[string]int{}
	found := 0
	for _, call := range calls {
		k := callKey(call)
		seen[k]++
		if !strings.HasPrefix(k, kind) {
			continue
		}
		if found++; found == n {
			return k, seen[k]
		}
	}
	t.Fatalf("the roll makes %d calls %q, want a %dth", found, kind, n)
	return "", 0
}

// lostAnswerHooks returns the hooks that carry out the nth call with the key key and lose its answer, and set lost.
func lostAnswerHooks(
	f *vultrfake.Fake, w *nomadWorld, key string, n int, lost *atomic.Bool,
) (vultrfake.Hook, nomadHook) {
	onVultr := atCall(f, key, n, func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		lost.Store(true)
		_ = next(ctx)
		return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
	})
	onNomad := atNomadCall(w, key, n, func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		lost.Store(true)
		_ = next(ctx)
		return notReadyError(c.Name)
	})
	return onVultr, onNomad
}

// nodeReadsBeforeTheNodeLeaves returns how many reads of the nodes the roll made from its Drain of the node of the
// machine victim to the first transfer of the leadership or the first halt of a machine after it.
func nodeReadsBeforeTheNodeLeaves(t *testing.T, f *vultrfake.Fake, w *nomadWorld, victim string) int {
	t.Helper()
	log := w.Log()
	node := nodeIDOf(victim) + " "
	drain := slices.IndexFunc(log, func(c nomadCall) bool { return c.Name == "Drain" && strings.HasPrefix(c.Arg, node) })
	if drain < 0 {
		t.Fatalf("the roll sent no Drain of %s", node)
	}
	halted := math.MaxInt
	if i := slices.IndexFunc(f.Calls()[log[drain].Cloud:], func(c vultrfake.Call) bool {
		return c.Name == "HaltInstance"
	}); i >= 0 {
		halted = log[drain].Cloud + i
	}
	reads := 0
	for _, c := range log[drain:] {
		if c.Name == "TransferLeadership" || c.Cloud > halted {
			break
		}
		if c.Name == "Nodes" {
			reads++
		}
	}
	return reads
}

// wantFirstStepWaitsForTheDrain fails the test unless the first progress line of a run that resumed at a node that
// drains is the start of the wait for that node's drain.
func wantFirstStepWaitsForTheDrain(t *testing.T, lines []string, node string) {
	t.Helper()
	if want := "nomad started drained " + node; len(lines) == 0 || lines[0] != want {
		t.Errorf("the first progress lines of the run are %q, want %q first", lines[:min(len(lines), 3)], want)
	}
}

// TestCombinedRollCutsStandAtEveryWrite checks the calls of an uninterrupted roll of three combined nodes: every call
// is a read or a write of rollWrites, so that a cut stands at every write, and the roll makes each write of
// combinedWrites: three nodes make three of each but the transfer, which the oldest node, the leader, makes once. The
// filter of the first and the last replacement keeps two calls of each kind, one in each, but the transfer, which only
// the last makes.
func TestCombinedRollCutsStandAtEveryWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedRoll(t)

	wantCutPlaces(t, calls, combinedWrites)
	for _, kind := range combinedWrites {
		want := 3
		if kind == "nomad TransferLeadership " {
			want = 1
		}
		if got := callsOfKind(calls, kind); got != want {
			t.Errorf("the roll makes %d calls %q, want %d", got, kind, want)
		}
	}
	keep := firstAndLastReplacement(t, calls)
	kept := map[string]int{}
	for i, call := range calls {
		if keep(i) {
			method, _, _ := strings.Cut(strings.TrimPrefix(callKey(call), "nomad "), " ")
			kept[method]++
		}
	}
	for _, kind := range combinedWrites {
		method := strings.TrimPrefix(strings.TrimSpace(kind), "nomad ")
		want := 2
		if method == "TransferLeadership" {
			want = 1
		}
		if kept[method] != want {
			t.Errorf("the filter keeps %d calls %s, want %d", kept[method], method, want)
		}
	}
}

// TestCombinedSingleRollCutsStandAtEveryWrite checks the calls of an uninterrupted roll of one combined node: every
// call is a read or a write of rollWrites, and the roll makes each write of combinedWrites once but the removal of a
// peer, which it makes twice.
func TestCombinedSingleRollCutsStandAtEveryWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedSingleRoll(t)

	wantCutPlaces(t, calls, combinedWrites)
	for _, kind := range combinedWrites {
		want := 1
		if kind == "nomad RemovePeer " {
			want = 2
		}
		if got := callsOfKind(calls, kind); got != want {
			t.Errorf("the roll makes %d calls %q, want %d", got, kind, want)
		}
	}
}

// firstAndLastReplacement returns the filter of the writes of calls that belong to the first replacement and to the
// last one, which is the leader's. A replacement starts at its CreateInstance; the second one repeats the first.
func firstAndLastReplacement(t *testing.T, calls []string) func(index int) bool {
	t.Helper()
	var creates []int
	for i, call := range calls {
		if strings.HasPrefix(callKey(call), "CreateInstance ") {
			creates = append(creates, i)
		}
	}
	if len(creates) != 3 {
		t.Fatalf("the roll makes %d calls CreateInstance, want 3", len(creates))
	}
	return func(i int) bool {
		return isRollWrite(callKey(calls[i])) && (i < creates[1] || i >= creates[2])
	}
}

// TestCombinedRollFinishesAfterACutAtAnyWrite cuts a roll of three combined nodes just before and just after each
// write of its first replacement and of its last, the leader's. The next run, with a fresh context, finishes the roll:
// every node is replaced, with as many CreateInstance calls in the two runs as the uninterrupted roll makes. The
// cluster keeps the invariants at every call of both runs, and a name differs from the uninterrupted roll's only by the
// rule that names grow.
func TestCombinedRollFinishesAfterACutAtAnyWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedRoll(t)
	eachCutAt(t, calls, firstAndLastReplacement(t, calls), func(t *testing.T, c cutCase) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")

		cutServersRun(t, svc, f, w, inv, c)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		wantCombinedCutDone(t, svc, f, inv, old, creates)
	})
}

// TestCombinedSingleRollFinishesAfterACutAtAnyWrite cuts a roll of one combined node just before and just after each
// write of an uninterrupted roll. The clock then runs for two reconciles and a promotion with nobody watching, and the
// voters that run must stay a quorum. The next run, with a fresh context, finishes the roll: the node is replaced with
// one CreateInstance call in the two runs. The cluster keeps the invariants at every call of both runs.
func TestCombinedSingleRollFinishesAfterACutAtAnyWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedSingleRoll(t)
	eachCutAt(t, calls, func(i int) bool { return isRollWrite(callKey(calls[i])) }, func(t *testing.T, c cutCase) {
		s := newCombinedSingleWorld(t)

		cutServersRun(t, s.svc, s.f, s.w, s.inv, c)
		s.inv.wantQuorumFor(quorumSpan)
		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		s.wantCombinedFinished(t)
	})
}

// TestCombinedRollWaitsForTheDrainOfARunThatResumesWhileTheLeaderDrains cuts a roll of three right after the Drain of
// the leader's node, so that the next run starts while the node still drains. Its first step is the wait for the
// drain: it sends no transfer of the leadership and no halt before a read shows the drain complete, which is the read
// after those that show it under way.
func TestCombinedRollWaitsForTheDrainOfARunThatResumesWhileTheLeaderDrains(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedRoll(t)
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		leader := instanceNamed(t, f, "prod-all-0")
		creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")
		key, n := nthOfKind(t, calls, "nomad Drain ", 3)

		cutServersRun(t, svc, f, w, inv, cutCase{key: key, n: n, after: true})
		lines := recordProgress(svc)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		wantFirstStepWaitsForTheDrain(t, *lines, "prod-all-0")
		if got := nodeReadsBeforeTheNodeLeaves(t, f, w, leader); got < drainReads+2 {
			t.Errorf("the roll read the nodes %d times from the leader's Drain to its transfer, want at least %d: the %d "+
				"that show the drain under way, the one that shows it complete and the one with the list before the "+
				"transfer", got, drainReads+2, drainReads)
		}
		wantCombinedCutDone(t, svc, f, inv, old, creates)
	})
}

// TestCombinedSingleRollWaitsForTheDrainOfARunThatResumesWhileTheNodeDrains cuts a roll of one combined node right
// after its Drain, so that the next run starts while the node still drains: it sends no transfer of the leadership
// and no halt before a read shows the drain complete.
func TestCombinedSingleRollWaitsForTheDrainOfARunThatResumesWhileTheNodeDrains(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newCombinedSingleWorld(t)

		s.cutRun(t, "Drain", 1, true)
		lines := recordProgress(s.svc)
		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		wantFirstStepWaitsForTheDrain(t, *lines, s.old[0].name)
		if got := nodeReadsBeforeTheNodeLeaves(t, s.f, s.w, s.old[0].id); got < drainReads+2 {
			t.Errorf("the roll read the nodes %d times from the Drain to the transfer, want at least %d: the %d that show "+
				"the drain under way, the one that shows it complete and the one with the list before the transfer", got,
				drainReads+2, drainReads)
		}
		s.wantCombinedFinished(t)
	})
}

// TestCombinedSingleRollSendsNoDrainAfterACutWhenTheNewNodeCannotTakeWork cuts a roll of one combined node right after
// the mark of the old node, and then makes the new node ineligible, or down: the next run sends no write, so no Drain
// and no halt, and ends with the refusal that names the new node. With the new node set back, the run after that
// finishes the roll.
func TestCombinedSingleRollSendsNoDrainAfterACutWhenTheNewNodeCannotTakeWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*nomadops.Node)
		reason string
	}{
		{"ineligible", func(n *nomadops.Node) { n.Eligible = false }, "is not eligible"},
		{"down", func(n *nomadops.Node) { n.Status = "down" }, "is down in Nomad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newCombinedSingleWorld(t)
				s.cutRun(t, "MarkIneligible", 1, true)
				s.w.ChangeNode("prod-all-1", tc.change)
				cloudCalls, nomadCalls := len(s.f.Calls()), len(s.w.Log())

				_, err := applyRoll(s.svc, app.RollOptions{})

				wantMatchesRefusal(t, err, combinedRefusal("prod-all-1", tc.reason))
				wantNoWrites(t, s.f.Calls()[cloudCalls:])
				if got := nomadWrites(s.w, nomadCalls); len(got) > 0 {
					t.Errorf("the run wrote to Nomad: %q", got)
				}
				wantLockFree(t, s.svc.Store)

				s.w.ChangeNode("prod-all-1", nil)
				if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
					t.Fatalf("the run after the node was set back failed: %v", err)
				}
				s.wantCombinedFinished(t)
			})
		})
	}
}

// combinedLostAnswers are the lost answers of the writes of a roll of three combined nodes. A lost answer of a stop
// ends the run here, since the roll of three holds no stop.
var combinedLostAnswers = []lostAnswer{
	{"CreateInstance ", false, "the provider finds the machine by its operation id and returns it"},
	{"UpdateInstance ", true, "a failed scrub ends the run; the next run lists the machine with the joined label"},
	{"nomad MarkIneligible ", false, "Servers sends the call to the next server, which marks a node that is marked"},
	{"nomad Drain ", false,
		"Servers sends the call to the next server, which moves the deadline of a drain under way"},
	{"nomad TransferLeadership ", false, "Servers sends the call to the next server, which answers a leader that leads"},
	{"HaltInstance ", true, "a failed Stop ends the run; the next run lists the machine as stopped"},
	{"nomad RemovePeer ", false, "Servers sends the call to the next server, which counts a gone peer as removed"},
	{"nomad ForceLeave ", false, "Servers sends the call to the next server, which answers a repeat"},
	{"DeleteInstance ", true, "a failed delete ends the run; the next run no longer lists the machine"},
	{"nomad Purge ", false, "Servers sends the call to the next server, which counts a node that is gone as purged"},
}

// TestCombinedRollFinishesAfterALostAnswerOfEachWrite loses the answer to the first write of each kind of a roll of
// three combined nodes: the fake carries the call out, and the client gets no answer. The run ends with the error or
// goes on as combinedLostAnswers says; when it ends, the next run finishes the roll. The cluster ends as
// wantCombinedCutDone says.
func TestCombinedRollFinishesAfterALostAnswerOfEachWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedRoll(t)
	for _, tc := range combinedLostAnswers {
		key, n := nthOfKind(t, calls, tc.kind, 1)
		t.Run(strings.TrimPrefix(strings.TrimSpace(tc.kind), "nomad "), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := combinedFlowWorld(t)
				old := serverMachines(f, combinedGroup)
				creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")
				var lost atomic.Bool
				inv.watch(lostAnswerHooks(f, w, key, n, &lost))

				_, err := applyRoll(svc, app.RollOptions{})

				if !lost.Load() {
					t.Fatalf("the roll made no call %s", key)
				}
				if (err != nil) != tc.ends {
					t.Errorf("the roll ended with %v, want an error: %v, because %s", err, tc.ends, tc.why)
				}
				if err != nil {
					if _, err = applyRoll(svc, app.RollOptions{}); err != nil {
						t.Fatalf("the run after the lost answer failed: %v", err)
					}
				}
				wantCombinedCutDone(t, svc, f, inv, old, creates)
			})
		})
	}
}

// combinedSingleLostAnswers are the lost answers of the writes of a roll of one combined node. The stop is a held one
// there, so its lost answer does not end the run.
var combinedSingleLostAnswers = []singleLostAnswer{
	{"CreateInstance ", 1, false, "the provider finds the machine by its operation id and returns it"},
	{"UpdateInstance ", 1, true, "a failed scrub ends the run; the next run lists the machine with the joined label"},
	{"nomad MarkIneligible ", 1, false, "Servers sends the call to the next server, which marks a node that is marked"},
	{"nomad Drain ", 1, false,
		"Servers sends the call to the next server, which moves the deadline of a drain under way"},
	{"nomad TransferLeadership ", 1, false,
		"Servers sends the call to the next server, which answers a leader that leads"},
	{"nomad RemovePeer ", 1, false, "the run waits a poll, and the next observation shows the peer gone"},
	{"nomad RemovePeer ", 2, false,
		"the run does not count the removal: it holds the stop until the leader adds the node again"},
	{"HaltInstance ", 1, false, "the stop is a held one: it counts as sent, and the run lists the machine as stopped"},
	{"nomad ForceLeave ", 1, false, "the run waits a poll, and the next observation shows the member gone"},
	{"DeleteInstance ", 1, true, "a failed delete ends the run; the next run no longer lists the machine"},
	{"nomad Purge ", 1, false, "the run waits a poll, and the next observation shows the node gone"},
}

// TestCombinedSingleRollFinishesAfterALostAnswerOfEachWrite loses the answer to a write of the roll of one combined
// node: the fake carries the call out, and the client gets no answer. The run ends with the error or goes on as
// combinedSingleLostAnswers says; when it ends, the next run finishes the roll. The roll sends one HaltInstance in all.
func TestCombinedSingleRollFinishesAfterALostAnswerOfEachWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedCombinedSingleRoll(t)
	for _, tc := range combinedSingleLostAnswers {
		key, n := nthOfKind(t, calls, tc.kind, tc.n)
		t.Run(fmt.Sprintf("%s %d", strings.TrimPrefix(strings.TrimSpace(tc.kind), "nomad "), tc.n), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newCombinedSingleWorld(t)
				var lost atomic.Bool
				s.hooks(lostAnswerHooks(s.f, s.w, key, n, &lost))

				plan, err := applyRoll(s.svc, app.RollOptions{})

				if !lost.Load() {
					t.Fatalf("the roll made no call %s", key)
				}
				if (err != nil) != tc.ends {
					t.Errorf("the roll ended with %v, want an error: %v, because %s", err, tc.ends, tc.why)
				}
				want := app.RollCounts{Created: 1, Drained: 1, Stopped: 1, Deleted: 1, Purged: 1}
				if tc.kind == "nomad Purge " {
					want.Purged = 0 // the roll counts a purge only when a server answered it
				}
				if err == nil && plan.Rolled != want {
					t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
				}
				if err != nil {
					if _, err = applyRoll(s.svc, app.RollOptions{}); err != nil {
						t.Fatalf("the run after the lost answer failed: %v", err)
					}
				}
				if got := countCalls(s.f, "HaltInstance"); got != 1 {
					t.Errorf("HaltInstance was called %d times in all, want 1", got)
				}
				s.wantCombinedFinished(t)
			})
		})
	}
}

// combinedDrainLimit is how long a wait for a drain of the combined group lasts: the group's drainTimeout and the 5
// minutes beyond it.
const combinedDrainLimit = combinedDrainTimeout + 5*time.Minute

// combinedDeadlines are the waits of a roll of three combined nodes that have a limit and are met before the first
// node is stopped. Other tests pin each error and its time.
var combinedDeadlines = []deadline{
	{
		name: "a combined node that votes and never registers", limit: 10 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) { w.Withhold("prod-all-3") },
		want: "node prod-all-3 did not join within 10m0s (Nomad lists no node of that name at its address); " + rollAgain,
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) { w.Release("prod-all-3") },
	}, {
		name: "a combined node that registers and never votes", limit: 10 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) { w.SetVoteAfter(time.Hour) },
		want: "node prod-all-3 did not join within 10m0s (its server does not vote yet); " + rollAgain,
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.SetVoteAfter(15 * time.Second)
		},
	}, {
		name: "a drain that never completes", limit: combinedDrainLimit,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			neverDrained(w, "prod-all-1")
		},
		want: "node prod-all-1 did not finish draining within 35m0s (Nomad lists it draining); " + rollAgain,
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.ChangeNode("prod-all-1", nil)
		},
	},
}

// TestCombinedRollGoesOnAfterADeadlineEndedTheRun lets each wait of a roll of three combined nodes that
// combinedDeadlines lists reach its limit: the run ends with the wait's error. The next run, with the world set back,
// finishes the roll with no machine more than the uninterrupted roll creates, and the invariants kept at every call of
// both runs.
func TestCombinedRollGoesOnAfterADeadlineEndedTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range combinedDeadlines {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := combinedFlowWorld(t)
				old := serverMachines(f, combinedGroup)
				creates := countCalls(f, "CreateInstance") + len(old)
				tc.hold(t, f, w, inv)

				_, err := rollBefore(svc, tc.limit)

				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the roll ended with %v, want an error that says %q", err, tc.want)
				}
				tc.back(t, f, w, inv)
				if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
					t.Fatalf("the next run failed: %v", err)
				}

				wantCombinedCutDone(t, svc, f, inv, old, creates)
			})
		})
	}
}

// TestCombinedRollGoesOnWhenNomadEndsADrainAtItsDeadline holds the drain of the first old node until its deadline, the
// group's 30 minutes: Nomad stops what remains then, and the roll goes on within the wait's limit and finishes. The
// drains of the other nodes complete as usual, which keeps the test short.
func TestCombinedRollGoesOnWhenNomadEndsADrainAtItsDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t, func(w *nomadWorld) { w.SetDrainReads(1 << 30) })
		old := serverMachines(f, combinedGroup)
		creates := countCalls(f, "CreateInstance") + len(old)
		lines := stampProgress(svc)
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if progressLine(p) == "nomad done drain prod-all-1" {
				w.SetDrainReads(drainReads)
			}
		}

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		started := whenLine(*lines, "nomad started drained prod-all-1")
		done := whenLine(*lines, "nomad done drained prod-all-1")
		if waited := done.Sub(started); started.IsZero() || waited < combinedDrainTimeout || waited > combinedDrainLimit {
			t.Errorf("the roll waited %v for the drain, want the group's %v", waited, combinedDrainTimeout)
		}
		wantCombinedCutDone(t, svc, f, inv, old, creates)
	})
}

// TestCombinedRollLeavesADrainedNodeAsItIsWhenTheWaitForTheServerEndsTheRun stops the first old node, never lets Serf
// mark its server failed and keeps Nomad listing the node ready: the run ends at the wait for the server to go. The
// node stays drained and ineligible, and the next run goes on without a second Drain of it.
func TestCombinedRollLeavesADrainedNodeAsItIsWhenTheWaitForTheServerEndsTheRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		creates := countCalls(f, "CreateInstance") + len(old)
		victim := instanceNamed(t, f, "prod-all-1")
		w.SetFailAfter(time.Hour)
		w.SetDownAfter(time.Hour)

		_, err := rollBefore(svc, 5*time.Minute)

		if want := "autopilot did not stop counting prod-all-1 as a healthy voter within 5m0s"; err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Fatalf("the roll ended with %v, want an error that says %q", err, want)
		}
		var node *nomadops.Node
		for _, n := range nomadNodes(t, svc) {
			if n.ID == nodeIDOf(victim) {
				node = &n
			}
		}
		if node == nil || node.Status != "ready" || node.Eligible || node.Draining || node.LastDrain.Status != "complete" {
			t.Errorf("the node of the stopped machine is %+v, want it ready, ineligible and drained", node)
		}
		w.SetFailAfter(40 * time.Second)
		w.SetDownAfter(defaultDownAfter)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the next run failed: %v", err)
		}

		drains := 0
		for _, c := range w.Log() {
			if c.Name == "Drain" && strings.HasPrefix(c.Arg, nodeIDOf(victim)+" ") {
				drains++
			}
		}
		if drains != 1 {
			t.Errorf("the runs sent %d Drain calls for the stopped node, want 1", drains)
		}
		wantCombinedCutDone(t, svc, f, inv, old, creates)
	})
}

// TestCombinedSingleRollEndsWhenTheOldNodeDoesNotGoDownAndTheNextRunPurgesIt ends the run six minutes after the delete
// of the old machine, whose node Nomad lists as ready for seven minutes. The next run waits until the node reads down,
// purges it and finishes the roll.
func TestCombinedSingleRollEndsWhenTheOldNodeDoesNotGoDownAndTheNextRunPurgesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newCombinedSingleWorld(t)
		s.w.SetDownAfter(7 * time.Minute)
		address := privateOf(t, s.f, s.old[0].id)
		waiting := stampOf(s.svc, "nomad started down prod-all-0")

		_, err := rollBefore(s.svc, loopDownTimeout)

		wantError(t, err, fmt.Sprintf("node prod-all-0 (%s) did not go down within 6m0s (Nomad lists it ready); %s",
			address, rollAgain))
		wantWithin(t, time.Since(*waiting), loopDownTimeout)
		lines := recordProgress(s.svc)

		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the next run failed: %v", err)
		}

		wantInOrder(t, *lines, "nomad started down prod-all-0", "nomad done down prod-all-0",
			"nomad started purge prod-all-0")
		s.wantCombinedFinished(t)
	})
}
