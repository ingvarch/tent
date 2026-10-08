package app_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
)

// replaceTag is the tag of a machine that a roll replaces.
const replaceTag = cloud.LabelReplace + "=true"

// labelReplace puts the replace tag on the machine of f called name, as an earlier forced roll did.
func labelReplace(t *testing.T, f *vultrfake.Fake, name string) {
	t.Helper()
	id := instanceNamed(t, f, name)
	for _, in := range f.Instances() {
		if in.ID == id {
			f.SetInstanceTags(t, id, append(slices.Clone(in.Tags), replaceTag)...)
		}
	}
}

// labelled returns the names of the machines of f that carry the replace tag, sorted.
func labelled(f *vultrfake.Fake) []string {
	var names []string
	for _, in := range f.Instances() {
		if slices.Contains(in.Tags, replaceTag) {
			names = append(names, in.Hostname)
		}
	}
	slices.Sort(names)
	return names
}

// forcedCutWorld is the test cluster whose workers are up to date and roll by cutShape, with the IDs of the workers.
func forcedCutWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld, []string) {
	t.Helper()
	svc, f, w := rollWorld(t)
	mustReplace(t, svc, workersYAML+cutShape.yaml())
	mustUpdate(t, svc)
	return svc, f, w, workerIDs(f)
}

// forcedWorkers are the options of the forced roll of the workers.
var forcedWorkers = app.RollOptions{Apply: true, NodeGroups: []string{"workers"}, Force: true}

// wantForcedRollDone fails the test unless the roll of forcedCutWorld ended as wantRollEnded says, with the two
// machines that one roll of the workers makes and no more, and with every node that Nomad lists eligible.
func wantForcedRollDone(t *testing.T, svc *app.Service, f *vultrfake.Fake, old []string) {
	t.Helper()
	wantRollEnded(t, svc, f, old)
	if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
		t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
	}
	if got := len(workerIDs(f)); got != workerCount {
		t.Errorf("the workers have %d machines, want %d", got, workerCount)
	}
	for _, n := range nomadNodes(t, svc) {
		if !n.Eligible {
			t.Errorf("Nomad lists the node %s ineligible", n.Name)
		}
	}
	if got := labelled(f); len(got) != 0 {
		t.Errorf("the machines %q carry the replace tag after the roll, want none", got)
	}
}

// TestRollingUpdatePlansTheLabelledMachinesAsForced plans, without force, the machine that carries the replace tag as
// forced and the other machine of its group as up to date, and writes nothing.
func TestRollingUpdatePlansTheLabelledMachinesAsForced(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		labelReplace(t, f, "prod-workers-0")
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		u.check(t)
		var got []string
		for _, g := range plan.Groups {
			for _, n := range g.Outdated {
				got = append(got, n.Name+" "+n.Reason)
			}
		}
		if want := []string{"prod-workers-0 forced"}; !slices.Equal(want, got) {
			t.Errorf("outdated = %q, want %q", got, want)
		}
		if plan.Next == nil || plan.Next.Action != "create" || plan.Next.Node != "prod-workers-2" {
			t.Errorf("Next = %+v, want the create of prod-workers-2", plan.Next)
		}
	})
}

// TestRollFlowReplacesTheLabelledMachinesWithoutForce rolls a cluster whose machines are up to date but one: that
// machine carries the replace tag, so the run replaces it, once, writes no label, and leaves the other machine.
func TestRollFlowReplacesTheLabelledMachinesWithoutForce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		holdLimits(t, f, w, rollShape{surge: 1})
		labelReplace(t, f, "prod-workers-0")
		kept := instanceNamed(t, f, "prod-workers-1")
		replaced := instanceNamed(t, f, "prod-workers-0")
		updates := countCalls(f, "UpdateInstance")

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 1, Drained: 1, Deleted: 1, Purged: 1}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, []string{replaced})
		if !hasInstance(f, kept) {
			t.Errorf("the machine %s without the replace tag is gone", kept)
		}
		if got := countCalls(f, "UpdateInstance") - updates; got != 1 {
			t.Errorf("the roll updated %d instances, want only the scrub of the new machine", got)
		}
	})
}

// TestRollFlowForceLabelsTheSelectedMachinesBeforeItsFirstStep checks, at the first create of a forced roll of the
// workers, that both workers carry the replace tag and no other machine does; and that the roll ends with no tag.
func TestRollFlowForceLabelsTheSelectedMachinesBeforeItsFirstStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		old := workerIDs(f)
		var atCreate []string
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "CreateInstance" && atCreate == nil {
				atCreate = labelled(f)
			}
			return next(ctx)
		})

		plan, err := svc.RollingUpdate(t.Context(), "prod", forcedWorkers)

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := []string{"prod-workers-0", "prod-workers-1"}; !slices.Equal(want, atCreate) {
			t.Errorf("the machines with the replace tag at the first create are %q, want %q", atCreate, want)
		}
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, old)
	})
}

// TestRollFlowForceLabelsOnlyTheMachinesWithoutTheLabel writes the label to the one worker that lacks it, and to no
// other machine, before the first create.
func TestRollFlowForceLabelsOnlyTheMachinesWithoutTheLabel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		labelReplace(t, f, "prod-workers-0")
		var updated []string
		created := false
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			created = created || c.Name == "CreateInstance"
			if c.Name == "UpdateInstance" && !created {
				updated = append(updated, c.Arg)
			}
			return next(ctx)
		})
		want := []string{instanceNamed(t, f, "prod-workers-1")}

		if _, err := svc.RollingUpdate(t.Context(), "prod", forcedWorkers); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		if !slices.Equal(want, updated) {
			t.Errorf("before the first create the roll updated the instances %q, want %q", updated, want)
		}
	})
}

// uninterruptedForcedRoll applies the forced roll of forcedCutWorld and returns the calls it makes to Vultr and Nomad,
// as flowCalls gives them.
func uninterruptedForcedRoll(t *testing.T) []string {
	t.Helper()
	var calls []string
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := forcedCutWorld(t)
		calls = flowCalls(t, svc, f, w, func() *engine.Plan {
			if _, err := svc.RollingUpdate(t.Context(), "prod", forcedWorkers); err != nil {
				t.Fatalf("RollingUpdate: %v", err)
			}
			return nil
		})
	})
	return calls
}

// lastLabelWrite returns the place, from 0, of the last write of the replace label of a forced roll among its calls:
// the last update of an instance before the first create.
func lastLabelWrite(t *testing.T, calls []string) int {
	t.Helper()
	last := -1
	for i, call := range calls {
		switch method, _, _ := strings.Cut(call, " "); method {
		case "CreateInstance":
			return last
		case "UpdateInstance":
			last = i
		}
	}
	t.Fatal("the forced roll makes no create")
	return -1
}

// TestForcedRollFinishesWithoutForceAfterACutAtAnyWriteOrObservation cuts a forced roll of the workers at each of the
// places of cutPlaces after its last label write, just before the call and just after it. The next run, without force
// and with a fresh context, replaces the labelled machines and finishes the roll: both workers are replaced, with
// as many CreateInstance calls in the two runs as the uninterrupted roll makes, no node is left ineligible and no
// machine keeps the label.
func TestForcedRollFinishesWithoutForceAfterACutAtAnyWriteOrObservation(t *testing.T) {
	t.Parallel()
	calls := uninterruptedForcedRoll(t)
	places := cutPlaces(calls)
	last := lastLabelWrite(t, calls)
	eachCutAt(t, calls, func(i int) bool { return places[i] && i > last }, func(t *testing.T, c cutCase) {
		svc, f, w, old := forcedCutWorld(t)
		runCut(t, f, w, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.RollingUpdate(ctx, "prod", forcedWorkers)
			return err
		})

		holdLimits(t, f, w, cutShape)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}
		f.SetHook(nil)
		w.SetHook(nil)

		wantForcedRollDone(t, svc, f, old)
	})
}

// TestForcedRollFinishesWithForceAfterALostAnswerOfALabel loses the answer of each label write of a forced roll, one at
// a time: the fake carries the write out and the run ends with the error, with no write to Nomad and no create. The
// next run, forced again, finishes the roll, and writes the label only to the machine that the first run did not
// reach.
func TestForcedRollFinishesWithForceAfterALostAnswerOfALabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		lost         string   // the machine whose label's answer is lost
		labelled     []string // the machines that carry the label after the first run
		secondLabels int      // the labels that the second run writes
	}{
		{"prod-workers-0", []string{"prod-workers-0"}, 1},
		{"prod-workers-1", []string{"prod-workers-0", "prod-workers-1"}, 0},
	} {
		t.Run(tc.lost, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, old := forcedCutWorld(t)
				lostID := instanceNamed(t, f, tc.lost)
				f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
					if c.Name != "UpdateInstance" || c.Arg != lostID {
						return next(ctx)
					}
					_ = next(ctx)
					return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
				})
				nomadCalls := len(w.Log())

				_, err := svc.RollingUpdate(t.Context(), "prod", forcedWorkers)

				if err == nil || !strings.Contains(err.Error(), "label node "+tc.lost) {
					t.Errorf("the run ended with %v, want the error of the label of %s", err, tc.lost)
				}
				if got := countCalls(f, "CreateInstance"); got != len(allNames) {
					t.Errorf("CreateInstance was called %d times, want the update's %d and no more", got, len(allNames))
				}
				if got := nomadWrites(w, nomadCalls); len(got) != 0 {
					t.Errorf("the run wrote %q to Nomad, want nothing", got)
				}
				if got := labelled(f); !slices.Equal(tc.labelled, got) {
					t.Errorf("the machines with the replace tag are %q, want %q", got, tc.labelled)
				}
				wantLockFree(t, svc.Store)
				f.SetHook(nil)
				updates := countCalls(f, "UpdateInstance")

				if _, err := svc.RollingUpdate(t.Context(), "prod", forcedWorkers); err != nil {
					t.Fatalf("the run after the lost answer failed: %v", err)
				}

				wantForcedRollDone(t, svc, f, old)
				if got, want := countCalls(f, "UpdateInstance")-updates, tc.secondLabels+2; got != want {
					t.Errorf("the second run updated %d instances, want %d: its labels and the scrubs of 2 new machines",
						got, want)
				}
			})
		})
	}
}

// TestForcedRollFinishesWithoutForceAfterACutBetweenTheLabels cuts a forced roll just before the label write of the
// second worker. The run after it, without force, replaces the labelled worker only, keeps the other one, leaves
// every node eligible and no replace tag.
func TestForcedRollFinishesWithoutForceAfterACutBetweenTheLabels(t *testing.T) {
	t.Parallel()
	calls := uninterruptedForcedRoll(t)
	var c cutCase
	for i, updates := 0, 0; i < len(calls) && c.key == ""; i++ {
		if method, _, _ := strings.Cut(calls[i], " "); method != "UpdateInstance" {
			continue
		}
		if updates++; updates == 2 {
			c = cutCase{index: i + 1, key: callKey(calls[i]), n: 1}
		}
	}
	if c.key == "" {
		t.Fatal("the forced roll makes no second update before its first create")
	}
	for _, call := range calls[:c.index-1] {
		if callKey(call) == c.key {
			c.n++
		}
	}
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := forcedCutWorld(t)
		replaced, kept := instanceNamed(t, f, "prod-workers-0"), instanceNamed(t, f, "prod-workers-1")
		runCut(t, f, w, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.RollingUpdate(ctx, "prod", forcedWorkers)
			return err
		})
		if got, want := labelled(f), []string{"prod-workers-0"}; !slices.Equal(want, got) {
			t.Fatalf("after the cut the machines with the replace tag are %q, want %q", got, want)
		}

		holdLimits(t, f, w, cutShape)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		wantRollEnded(t, svc, f, []string{replaced})
		if !hasInstance(f, kept) {
			t.Errorf("the machine %s without the replace tag is gone", kept)
		}
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+1 {
			t.Errorf("CreateInstance was called %d times in all, want the update's %d and 1 more", got, len(allNames))
		}
		for _, n := range nomadNodes(t, svc) {
			if !n.Eligible {
				t.Errorf("Nomad lists the node %s ineligible", n.Name)
			}
		}
		if got := labelled(f); len(got) != 0 {
			t.Errorf("the machines %q carry the replace tag after the roll, want none", got)
		}
	})
}
