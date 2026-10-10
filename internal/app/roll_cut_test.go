package app_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// cutShape is the shape of the rolls that the cuts, the lost answers and the deadlines interrupt: one machine beyond
// the group's size.
var cutShape = rollShape{surge: 1}

// tenMinuteDrain is the line that gives the drains of a group ten minutes; it follows the lines of cutShape.
const tenMinuteDrain = "    drainTimeout: 10m\n"

// cutWorld is the test cluster whose workers are outdated and roll by cutShape, with the hashes and the IDs of the
// workers that the roll replaces.
func cutWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld, map[string]string, []string) {
	t.Helper()
	svc, f, w := outdatedWith(t, cutShape.yaml())
	return svc, f, w, workerHashes(f), workerIDs(f)
}

// uninterruptedRoll applies the roll of cutWorld and returns the calls it makes to Vultr and Nomad, as flowCalls gives
// them.
func uninterruptedRoll(t *testing.T) []string {
	t.Helper()
	var calls []string
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _, _ := cutWorld(t)
		calls = flowCalls(t, svc, f, w, func() *engine.Plan {
			if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
				t.Fatalf("RollingUpdate: %v", err)
			}
			return nil
		})
	})
	return calls
}

// wantRollDone fails the test unless the roll ended as wantWorkersRolled says, with the two machines that an
// uninterrupted roll makes and no more, and the workers' machines as many as the group's size.
func wantRollDone(t *testing.T, svc *app.Service, f *vultrfake.Fake, before map[string]string, old []string) {
	t.Helper()
	wantWorkersRolled(t, svc, f, before, old)
	if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
		t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
	}
	if got := len(workerIDs(f)); got != workerCount {
		t.Errorf("the workers have %d machines, want %d", got, workerCount)
	}
}

// rollWrites are the writes of a roll to Vultr and to Nomad, as the first words of a call: those of a roll of the
// workers and those of a roll of the servers. The update of an instance is the label of a machine that a forced roll
// replaces, and the scrub of a new machine.
var rollWrites = slices.Concat(clientWrites, serverWrites)

// clientWrites are the writes that a roll of the workers makes.
var clientWrites = []string{
	"CreateInstance ", "DeleteInstance ", "UpdateInstance ",
	"nomad IntroToken ", "nomad MarkIneligible ", "nomad Drain ", "nomad Purge ",
}

// isRead reports whether the call, as callKey gives it, only reads: a call to Vultr whose method starts with List or
// Get, or is AvailablePlans, as wantNoWrites says, or a call to Nomad of nomadReads.
func isRead(key string) bool {
	if rest, ok := strings.CutPrefix(key, "nomad "); ok {
		method, _, _ := strings.Cut(rest, " ")
		return slices.Contains(nomadReads, method)
	}
	method, _, _ := strings.Cut(key, " ")
	return strings.HasPrefix(method, "List") || strings.HasPrefix(method, "Get") || method == "AvailablePlans"
}

// isRollWrite reports whether the call, as callKey gives it, is a write of a roll.
func isRollWrite(key string) bool {
	return slices.ContainsFunc(rollWrites, func(p string) bool { return strings.HasPrefix(key, p) })
}

// cutPlaces says for each of the calls of a roll, as callKey gives them, whether a cut stands at it: at every write,
// and at the first read of each observation. An observation is a run of reads after a write, or at the start; a read
// that repeats a call of the run starts the next observation. A cut at any other read leaves what a cut at the first
// read of its observation leaves, since a read changes nothing.
func cutPlaces(calls []string) []bool {
	places := make([]bool, len(calls))
	seen := map[string]bool{}
	for i, call := range calls {
		key := callKey(call)
		switch {
		case isRollWrite(key):
			places[i] = true
			clear(seen)
		case seen[key] || len(seen) == 0:
			places[i] = true
			clear(seen)
			seen[key] = true
		default:
			seen[key] = true
		}
	}
	return places
}

// TestCutPlacesStandAtWritesAndAtTheFirstReadOfEachObservation checks the places on a log made for it: the first read
// of the log, a read that repeats one of its observation, a write after reads, a write after a write, and the first
// read after a write.
func TestCutPlacesStandAtWritesAndAtTheFirstReadOfEachObservation(t *testing.T) {
	t.Parallel()
	calls := []string{
		"ListInstances tent/cluster=prod",             // 0: the first read
		"nomad Peers (prod-servers-0)",                // 1
		"nomad Health (prod-servers-0)",               // 2
		"ListInstances tent/cluster=prod",             // 3: repeats a read of its observation
		"nomad Peers (prod-servers-0)",                // 4
		"nomad Drain prod-workers-0 (prod-servers-0)", // 5: a write after reads
		"nomad Purge prod-workers-0 (prod-servers-0)", // 6: a write after a write
		"nomad Peers (prod-servers-0)",                // 7: the first read after a write
		"ListInstances tent/cluster=prod",             // 8
	}
	want := []bool{true, false, false, true, false, true, true, true, false}

	if diff := cmp.Diff(want, cutPlaces(calls)); diff != "" {
		t.Errorf("the places (-want +got):\n%s", diff)
	}
}

// TestCutPlacesOfARollHoldEveryWriteAndAReadBeforeEach checks the places on the calls of an uninterrupted roll, as
// wantCutPlaces says.
func TestCutPlacesOfARollHoldEveryWriteAndAReadBeforeEach(t *testing.T) {
	t.Parallel()
	wantCutPlaces(t, uninterruptedRoll(t), clientWrites)
}

// TestCutPlacesOfAForcedRollHoldEveryWriteAndAReadBeforeEach checks the places on the calls of an uninterrupted forced
// roll, which writes the replace label of each worker first, as wantCutPlaces says.
func TestCutPlacesOfAForcedRollHoldEveryWriteAndAReadBeforeEach(t *testing.T) {
	t.Parallel()
	wantCutPlaces(t, uninterruptedForcedRoll(t), clientWrites)
}

// wantCutPlaces fails the test unless the places of cutPlaces on the calls of an uninterrupted roll are as the cut
// tests need them: every call is a read or a write of rollWrites; every kind of write of want occurs in the
// calls and has its cut at each of its calls; a cut at a read stands between each write and the write before it; and
// the places are at most half of the calls, which is the reason to choose them.
func wantCutPlaces(t *testing.T, calls, want []string) {
	t.Helper()
	places := cutPlaces(calls)

	kinds := map[string]bool{}
	readBefore, cuts := false, 0
	for i, call := range calls {
		key := callKey(call)
		if places[i] {
			cuts++
		}
		if !isRollWrite(key) && !isRead(key) {
			t.Errorf("call %03d %s writes, and rollWrites lacks it", i+1, key)
		}
		if !isRollWrite(key) {
			readBefore = readBefore || places[i]
			continue
		}
		for _, p := range rollWrites {
			if strings.HasPrefix(key, p) {
				kinds[p] = true
			}
		}
		if !places[i] {
			t.Errorf("call %03d %s is a write, and no cut stands at it", i+1, key)
		}
		if !readBefore {
			t.Errorf("call %03d %s is a write, and no cut at a read stands between it and the write before it", i+1, key)
		}
		readBefore = false
	}
	for _, p := range want {
		if !kinds[p] {
			t.Errorf("the roll makes no write %q", p)
		}
	}
	if cuts*2 > len(calls) {
		t.Errorf("the cuts stand at %d of %d calls, want at most half", cuts, len(calls))
	}
}

// TestEachCutAtCutsOnlyTheKeptCalls checks that eachCutAt gives a case, before and after, to each call that keep names
// and to no other, and that a case's n counts the calls with its key that keep leaves out.
func TestEachCutAtCutsOnlyTheKeptCalls(t *testing.T) {
	t.Parallel()
	calls := []string{"A x", "B y", "A x", "A x", "B y"}
	var mu sync.Mutex
	var got []string
	t.Run("cases", func(t *testing.T) {
		eachCutAt(t, calls, func(i int) bool { return i == 2 || i == 4 }, func(_ *testing.T, c cutCase) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, fmt.Sprintf("%v %d %s %d", c.after, c.index, c.key, c.n))
		})
	})
	slices.Sort(got)

	want := []string{"false 3 A x 2", "false 5 B y 2", "true 3 A x 2", "true 5 B y 2"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the cases (-want +got):\n%s", diff)
	}
}

// TestRollFinishesAfterACutAtAnyWriteOrObservation cuts a roll at each of the places of cutPlaces among the calls of an
// uninterrupted roll to Vultr and Nomad, just before the call and just after it. The next run, with a fresh context,
// finishes the roll: both workers are replaced, with as many CreateInstance calls in the two runs as the uninterrupted
// roll makes, and the cluster ends as wantRollDone says. The cut run changes nothing in the store and leaves no lock.
// The run after the cut keeps to the limits of cutShape at every call, as holdLimits checks.
func TestRollFinishesAfterACutAtAnyWriteOrObservation(t *testing.T) {
	t.Parallel()
	calls := uninterruptedRoll(t)
	places := cutPlaces(calls)
	eachCutAt(t, calls, func(i int) bool { return places[i] }, func(t *testing.T, c cutCase) {
		svc, f, w, before, old := cutWorld(t)
		runCut(t, f, w, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})
			return err
		})

		holdLimits(t, f, w, cutShape)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}
		f.SetHook(nil)
		w.SetHook(nil)

		wantRollDone(t, svc, f, before, old)
	})
}

// TestRollFinishesAfterALostAnswerOfAnyWrite loses the answer of each write of an uninterrupted roll, one at a time:
// the fake carries the call out, and the client gets no answer. A lost answer of the scrub or of the delete of a
// machine ends the run, and the next run finishes the roll; the roll goes on past every other lost answer: Nomad's
// writes go to the next server, and a create is found by its operation id. The cluster ends as wantRollDone says.
func TestRollFinishesAfterALostAnswerOfAnyWrite(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	for i, call := range uninterruptedRoll(t) {
		key := callKey(call)
		seen[key]++
		n := seen[key]
		if !isRollWrite(key) {
			continue
		}
		method, _, _ := strings.Cut(strings.TrimPrefix(key, "nomad "), " ")
		t.Run(fmt.Sprintf("%03d %s", i+1, method), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, before, old := cutWorld(t)
				lost := false
				f.SetHook(atCall(f, key, n, func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
					lost = true
					_ = next(ctx)
					return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
				}))
				w.SetHook(atNomadCall(w, key, n,
					func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
						lost = true
						_ = next(ctx)
						return notReadyError(c.Name)
					}))

				_, err := applyRoll(svc, app.RollOptions{})

				f.SetHook(nil)
				w.SetHook(nil)
				if !lost {
					t.Fatalf("the roll made no call %s", key)
				}
				if ends := method == "UpdateInstance" || method == "DeleteInstance"; (err != nil) != ends {
					t.Errorf("the roll ended with %v, want an error only for a lost scrub or delete: ends = %v", err, ends)
				}
				if err != nil {
					if _, err = applyRoll(svc, app.RollOptions{}); err != nil {
						t.Fatalf("the run after the lost answer failed: %v", err)
					}
				}
				wantRollDone(t, svc, f, before, old)
			})
		})
	}
}

// neverDrained keeps the drain of the node called name from ever completing, as a node whose allocations never stop.
func neverDrained(w *nomadWorld, name string) {
	w.ChangeNode(name, func(n *nomadops.Node) {
		if n.LastDrain.Status == "complete" {
			n.Draining, n.LastDrain.Status = true, "draining"
		}
	})
}

// stampOf returns when the first progress line of svc that is line came; it is zero until then.
func stampOf(svc *app.Service, line string) *time.Time {
	var at time.Time
	svc.OnProgress = func(p app.Progress) {
		if at.IsZero() && progressLine(p) == line {
			at = time.Now()
		}
	}
	return &at
}

// TestRollGoesOnWhenNomadEndsADrainAtItsDeadline holds each drain until its deadline of ten minutes: Nomad stops what
// remains then, and the roll goes on within the wait's limit and finishes.
func TestRollGoesOnWhenNomadEndsADrainAtItsDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWith(t, cutShape.yaml()+tenMinuteDrain)
		before, old := workerHashes(f), workerIDs(f)
		w.SetDrainReads(1 << 30)
		start := time.Now()

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		if took := time.Since(start); took < 2*10*time.Minute {
			t.Errorf("the roll took %v, want the two drains' 10 minutes each at least", took)
		}
		wantRollDone(t, svc, f, before, old)
	})
}

// TestRollEndsWhenADrainNeverCompletesAndTheNextRunGoesOn ends the run five minutes after the deadline of a drain that
// never completes, with what the run did, the lock released, and the wait's error. The next run, when the drain can
// complete, finishes the roll.
func TestRollEndsWhenADrainNeverCompletesAndTheNextRunGoesOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWith(t, cutShape.yaml()+tenMinuteDrain)
		before, old := workerHashes(f), workerIDs(f)
		neverDrained(w, "prod-workers-0")
		waiting := stampOf(svc, "nomad started drained prod-workers-0")

		ctx, cancel := context.WithTimeout(context.Background(), loopDrainTimeout+time.Hour) // a lost limit ends here
		defer cancel()
		plan, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})

		wantError(t, err, "node prod-workers-0 did not finish draining within 15m0s (Nomad lists it draining); "+rollAgain)
		wantWithin(t, time.Since(*waiting), loopDrainTimeout)
		if want := (app.RollCounts{Created: 1, Drained: 1}); plan.Rolled != want || plan.Applied {
			t.Errorf("rolled %+v, applied %v; want %+v, not applied", plan.Rolled, plan.Applied, want)
		}
		wantLockFree(t, svc.Store)
		w.ChangeNode("prod-workers-0", nil)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the next run failed: %v", err)
		}

		wantRollDone(t, svc, f, before, old)
	})
}

// TestRollEndsWhenANodeDoesNotGoDownAndTheNextRunGoesOn ends the run six minutes after the delete of a machine whose
// node Nomad keeps listing as ready. The next run, when the node reads down, purges it and finishes the roll.
func TestRollEndsWhenANodeDoesNotGoDownAndTheNextRunGoesOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, before, old := cutWorld(t)
		w.SetDownAfter(time.Hour)
		address := privateOf(t, f, instanceNamed(t, f, "prod-workers-0"))
		waiting := stampOf(svc, "nomad started down prod-workers-0")

		plan, err := applyRoll(svc, app.RollOptions{})

		wantError(t, err, fmt.Sprintf("node prod-workers-0 (%s) did not go down within 6m0s (Nomad lists it ready); %s",
			address, rollAgain))
		wantWithin(t, time.Since(*waiting), loopDownTimeout)
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2}); plan.Rolled != want || plan.Applied {
			t.Errorf("rolled %+v, applied %v; want %+v, not applied", plan.Rolled, plan.Applied, want)
		}
		wantLockFree(t, svc.Store)
		w.SetDownAfter(defaultDownAfter)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the next run failed: %v", err)
		}

		wantRollDone(t, svc, f, before, old)
	})
}

// TestRollEndsWhenANewNodeNeverRegistersAndTheNextRunGoesOn ends the run ten minutes after the create of a machine
// whose node never registers, before any node is drained. The next run, when the node registers, finishes the roll.
func TestRollEndsWhenANewNodeNeverRegistersAndTheNextRunGoesOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, before, old := cutWorld(t)
		w.Withhold("prod-workers-2")
		waiting := stampOf(svc, "nomad started register prod-workers-2")
		nomadCalls := len(w.Log())

		plan, err := applyRoll(svc, app.RollOptions{})

		wantError(t, err, "node prod-workers-2 did not join within 10m0s (Nomad lists no node of that name at its "+
			"address); "+rollAgain)
		wantWithin(t, time.Since(*waiting), loopNodeTimeout)
		if got := nomadWrites(w, nomadCalls); len(got) != 1 || got[0] != "IntroToken" {
			t.Errorf("the run wrote %q to Nomad, want only the intro token of the new node", got)
		}
		if want := (app.RollCounts{Created: 1}); plan.Rolled != want {
			t.Errorf("rolled %+v, want %+v", plan.Rolled, want)
		}
		wantLockFree(t, svc.Store)
		w.Release("prod-workers-2")

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the next run failed: %v", err)
		}

		wantRollDone(t, svc, f, before, old)
	})
}
