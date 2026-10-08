package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// The limits of the loop, as the tests expect them, and what an error that ends a wait tells the operator.
const (
	loopPoll         = 2 * time.Second
	loopNodeTimeout  = 10 * time.Minute // a wait for a node to join, a read of Nomad and a wait for the servers
	loopDownTimeout  = 6 * time.Minute
	loopGoneTimeout  = 5 * time.Minute
	loopDrainTimeout = 15 * time.Minute // a drain of 10 minutes and the 5 that a wait for it lasts beyond it
	loopPending      = time.Minute
	loopSlack        = 10 * time.Second // how far past a limit a run may end
	rollAgain        = "run tent rolling-update cluster again to go on waiting"
	drainTenMinutes  = "  rollingUpdate:\n" + tenMinuteDrain
)

// roll runs the loop of a rolling update of the test cluster, without the lock.
func roll(svc *app.Service, opts app.RollOptions) (app.RollCounts, error) {
	return app.RunRoll(context.Background(), svc, "prod", opts)
}

// rollBefore is roll under a deadline an hour after limit, the limit that the run under test is to give up at: a run
// that lacks its limit ends with the deadline in seconds, and not when go test's own timeout kills it.
func rollBefore(svc *app.Service, limit time.Duration) (app.RollCounts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit+time.Hour)
	defer cancel()
	return app.RunRoll(ctx, svc, "prod", app.RollOptions{})
}

// outdatedWith is outdatedWorld whose workers also hold the spec lines extra.
func outdatedWith(t *testing.T, extra string) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := rollWorld(t)
	mustReplace(t, svc, workersMetaYAML+extra)
	mustUpdate(t, svc)
	return svc, f, w
}

// nomadNodes returns the nodes that Nomad lists.
func nomadNodes(t *testing.T, svc *app.Service) []nomadops.Node {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	nodes, err := api.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	return nodes
}

// nomadWrites returns the methods of the calls that reached the Nomad fake after the first n and that write.
func nomadWrites(w *nomadWorld, n int) []string {
	var out []string
	for _, name := range nomadCallNames(w, n) {
		if !slices.Contains(nomadReads, name) {
			out = append(out, name)
		}
	}
	return out
}

// hasInstance reports whether f holds the instance id.
func hasInstance(f *vultrfake.Fake, id string) bool {
	for _, in := range f.Instances() {
		if in.ID == id {
			return true
		}
	}
	return false
}

// wantRolled fails the test unless the roll ended without an error and did want.
func wantRolled(t *testing.T, got app.RollCounts, err error, want app.RollCounts) {
	t.Helper()
	if err != nil {
		t.Fatalf("the roll failed: %v", err)
	}
	if got != want {
		t.Errorf("the roll did %+v, want %+v", got, want)
	}
}

// wantNothingToRoll fails the test unless a plan of the rolling update finds nothing outdated and has no next step.
func wantNothingToRoll(t *testing.T, svc *app.Service) {
	t.Helper()
	plan, err := rollingUpdate(svc, app.RollOptions{})
	if err != nil {
		t.Fatalf("RollingUpdate: %v", err)
	}
	if plan.Next != nil {
		t.Errorf("the next step is %+v, want none", plan.Next)
	}
	for _, g := range plan.Groups {
		if len(g.Outdated) > 0 {
			t.Errorf("group %s has outdated machines %+v, want none", g.Name, g.Outdated)
		}
	}
}

// wantInOrder fails the test unless each line of want is among lines exactly once, in the order of want.
func wantInOrder(t *testing.T, lines []string, want ...string) {
	t.Helper()
	at := -1
	for _, line := range want {
		if n := count(lines, line); n != 1 {
			t.Errorf("the progress has %q %d times, want once:\n%s", line, n, strings.Join(lines, "\n"))
			continue
		}
		i := slices.Index(lines, line)
		if i < at {
			t.Errorf("the progress has %q earlier than a line before it in %q:\n%s", line, want, strings.Join(lines, "\n"))
		}
		at = max(at, i)
	}
}

// count returns how often line is among lines.
func count(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}

// wantWithin fails the test unless got is limit or a little more.
func wantWithin(t *testing.T, got, limit time.Duration) {
	t.Helper()
	if got < limit || got > limit+loopSlack {
		t.Errorf("the wait took %v, want %v to %v", got, limit, limit+loopSlack)
	}
}

// wantPolls fails the test unless each of times came one poll after the one before: the loop waits once between two
// tries.
func wantPolls(t *testing.T, times []time.Time) {
	t.Helper()
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap != loopPoll {
			t.Errorf("try %d came %v after the one before, want %v", i+1, gap, loopPoll)
		}
	}
}

// notReadyError is an error that matches nomadops.ErrNotReady, as the one of a server that did not answer does.
func notReadyError(call string) error { return fmt.Errorf("%s: %w", call, nomadops.ErrNotReady) }

// hideNewest hides the first instance that a create makes from the next lists lists by the cluster's tag, and from
// every list when lists is negative. created gets the instance's ID when the create returns. The provider's search by
// operation id still finds it.
func hideNewest(t *testing.T, f *vultrfake.Fake, lists int, created func(id string)) {
	t.Helper()
	var hidden string
	clusterTag := cloud.LabelCluster + "=prod"
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		switch {
		case c.Name == "CreateInstance" && hidden == "":
			err := next(ctx)
			if err == nil {
				all := f.Instances()
				hidden = all[len(all)-1].ID
				created(hidden)
			}
			return err
		case c.Name == "ListInstances" && c.Arg == clusterTag && hidden != "" && lists != 0:
			lists--
			for _, in := range f.Instances() {
				if in.ID == hidden {
					f.SetInstanceTags(t, hidden, slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
						return tag == clusterTag
					})...)
					defer f.SetInstanceTags(t, hidden, in.Tags...)
				}
			}
		}
		return next(ctx)
	})
}

// TestRollLoopReplacesTheOutdatedWorkers rolls both outdated workers: two new machines that carry the group's hash and
// the seed of the servers, two drained, deleted and purged nodes, and a cluster with nothing left to roll. Each step
// reports itself once, and a node is marked, drained, deleted and purged in that order.
func TestRollLoopReplacesTheOutdatedWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		old := []string{instanceNamed(t, f, "prod-workers-0"), instanceNamed(t, f, "prod-workers-1")}
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		wantNothingToRoll(t, svc)
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
			t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
		}
		for _, id := range old {
			if hasInstance(f, id) {
				t.Errorf("the machine %s of an old worker is still there", id)
			}
			if slices.ContainsFunc(nomadNodes(t, svc), func(n nomadops.Node) bool { return n.ID == nodeIDOf(id) }) {
				t.Errorf("Nomad still lists the node of the machine %s", id)
			}
		}
		wantJoined(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-2", "prod-workers-3")
		for _, name := range []string{"prod-workers-2", "prod-workers-3"} {
			if got := seedOf(configOf(t, f, name)); !slices.Equal(got, []string{"10.64.0.3", "10.64.0.4", "10.64.0.5"}) {
				t.Errorf("the seed of %s is %v, want the three servers", name, got)
			}
			if req, _ := f.CreateRequest(instanceNamed(t, f, name)); req.Plan != "vc2-2c-4gb" {
				t.Errorf("%s was created with the plan %q, want vc2-2c-4gb", name, req.Plan)
			}
			wantInOrder(t, *lines, "node started create "+name, "node done create "+name,
				"nomad started register "+name, "nomad done register "+name, "node started scrub "+name,
				"node done scrub "+name)
		}
		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			wantInOrder(t, *lines, "nomad done ineligible "+name, "nomad done drain "+name, "node done delete "+name,
				"nomad done purge "+name)
		}
	})
}

// TestRollLoopHasNothingToDoWhenNothingIsOutdated ends at the first decision: one list, one observation of Nomad, no
// write and no progress.
func TestRollLoopHasNothingToDoWhenNothingIsOutdated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{})
		wantNoWrites(t, f.Calls()[cloudCalls:])
		listed := 0
		for _, c := range f.Calls()[cloudCalls:] {
			if c.Name == "ListInstances" {
				listed++
			}
		}
		if listed != 1 {
			t.Errorf("the roll listed the machines %d times, want once", listed)
		}
		if diff := cmp.Diff([]string{"Peers", "Health", "Members", "Nodes"}, nomadCallNames(w, nomadCalls)); diff != "" {
			t.Errorf("the calls of Nomad (-want +got):\n%s", diff)
		}
		if len(*lines) != 0 {
			t.Errorf("the progress is %q, want none", *lines)
		}
	})
}

// TestRollLoopRefusesServerGroups ends with the refusal of the server group and its advice about --nodegroups, and
// writes nothing to the cloud or to Nomad.
func TestRollLoopRefusesServerGroups(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		withTolerance(svc)
		mustReplace(t, svc, keyedClusterYAML+"  nomad:\n    extraConfig:\n      server: 'raft_multiplier = 3'\n")
		mustUpdate(t, svc)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		counts, err := roll(svc, app.RollOptions{})

		wantError(t, err, "node group servers: tent cannot roll server and combined groups yet; "+
			"select client groups with --nodegroups")
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		if counts != (app.RollCounts{}) {
			t.Errorf("the roll did %+v, want nothing", counts)
		}
	})
}

// TestRollLoopScrubsAMachineWhoseNodeJoined labels the machine and replaces its user data once Nomad lists its node
// ready and eligible, and reports the wait and the scrub.
func TestRollLoopScrubsAMachineWhoseNodeJoined(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := unjoinedWorld(t, func(*vultrfake.Fake, *nomadWorld) {})
		lines := recordProgress(svc)
		updates := countCalls(f, "UpdateInstance")

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{})
		wantJoined(t, f, allNames...)
		want := []string{
			"nomad started register prod-workers-1", "nomad done register prod-workers-1",
			"node started scrub prod-workers-1", "node done scrub prod-workers-1",
		}
		if diff := cmp.Diff(want, *lines); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
		if got := countCalls(f, "UpdateInstance") - updates; got != 1 {
			t.Errorf("UpdateInstance was called %d times, want once", got)
		}
	})
}

// TestRollLoopFailsTheWaitForAMachineWithoutAnAddress ends the run at the first poll, with the wait reported failed
// and nothing written.
func TestRollLoopFailsTheWaitForAMachineWithoutAnAddress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		id := instanceNamed(t, f, "prod-workers-1")
		for _, in := range f.Instances() {
			if in.ID != id {
				continue
			}
			if err := f.DeleteInstance(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			in.ID, in.MainIP = "", ""
			in.Tags = withoutJoined(in.Tags)
			f.AddInstance(t, in) // no VPC: the cloud reports no private address
		}
		w.DropNode("prod-workers-1")
		lines := recordProgress(svc)
		cloudCalls := len(f.Calls())

		_, err := roll(svc, app.RollOptions{})

		const why = "node prod-workers-1: the cloud reports no private address for it yet; run the command again"
		wantError(t, err, why)
		wantNoWrites(t, f.Calls()[cloudCalls:])
		want := []string{"nomad started register prod-workers-1", "nomad failed register prod-workers-1: " + why}
		if diff := cmp.Diff(want, *lines); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
	})
}

// TestRollLoopRefusesAClientThatNeverJoined ends the run at the first poll for a client that is older than the life of
// its intro token and that Nomad does not list, without a write.
func TestRollLoopRefusesAClientThatNeverJoined(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedWorld(t, func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") })
		svc.Now = func() time.Time { return time.Now().Add(32 * time.Minute) }
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "node prod-workers-1 has not joined within 31 minutes of its creation; "+
			"run tent update cluster, which deletes it and creates it again")
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
	})
}

// TestRollLoopGivesUpWaitingForANodeToJoin ends the run when a node has not joined within ten minutes, and says what
// Nomad showed; the wait starts once and fails once.
func TestRollLoopGivesUpWaitingForANodeToJoin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := unjoinedWorld(t, func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") })
		lines := recordProgress(svc)
		cloudCalls := len(f.Calls())
		start := time.Now()

		_, err := rollBefore(svc, loopNodeTimeout)

		want := "node prod-workers-1 did not join within 10m0s (Nomad lists no node of that name at its address); " +
			rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(start), loopNodeTimeout)
		wantNoWrites(t, f.Calls()[cloudCalls:])
		wantLines(t, *lines, []string{
			"nomad started register prod-workers-1", "nomad failed register prod-workers-1: " + want,
		})
	})
}

// TestRollLoopEndsWhenInterrupted ends a wait when the context ends, reports the wait failed, and returns the
// interruption.
func TestRollLoopEndsWhenInterrupted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := unjoinedWorld(t, func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") })
		lines := recordProgress(svc)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(30*time.Second, cancel)

		_, err := app.RunRoll(ctx, svc, "prod", app.RollOptions{})

		if err == nil || err.Error() != "interrupted" || !errors.Is(app.Stands(err), context.Canceled) {
			t.Errorf("the roll ended with %v (standing for %v), want an interruption", err, app.Stands(err))
		}
		// The poll that ends the wait and the end of the context come at the same moment, so the error may come from a
		// sleep or from a read.
		if len(*lines) != 2 || (*lines)[0] != "nomad started register prod-workers-1" ||
			!strings.HasPrefix((*lines)[1], "nomad failed register prod-workers-1: ") ||
			!strings.HasSuffix((*lines)[1], context.Canceled.Error()) {
			t.Errorf("the progress is %q, want the wait started and failed because the context ended", *lines)
		}
	})
}

// TestRollLoopRepeatsTheCreateOfAMachineThatIsNotReady finishes a roll that was cut during a create: the cloud lists
// the machine not ready, the wait repeats its create with its operation id, and the roll makes no machine beyond the
// two that an uninterrupted roll makes.
func TestRollLoopRepeatsTheCreateOfAMachineThatIsNotReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		created := false
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			created = created || c.Name == "CreateInstance"
			if c.Name == "GetInstance" && created {
				cancel()
			}
			return next(ctx)
		})

		_, err := app.RunRoll(ctx, svc, "prod", app.RollOptions{})

		if !errors.Is(app.Stands(err), context.Canceled) {
			t.Fatalf("the cut roll ended with %v, want an interruption", err)
		}
		f.SetHook(nil)
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 1, Drained: 2, Deleted: 2, Purged: 2})
		wantNothingToRoll(t, svc)
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
			t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
		}
		wantInOrder(t, *lines, "nomad started register prod-workers-2", "node started wait prod-workers-2",
			"node done wait prod-workers-2", "nomad done register prod-workers-2", "node done scrub prod-workers-2")
	})
}

// TestRollLoopKeepsAMachineItCreatedUntilTheCloudListsIt makes no third machine when the next four lists miss the
// roll's second one, which is the surge create that brings a group to its size and its surge: the machine stays among
// the decisions' machines, and its scrub, which falls in the hidden lists, marks it joined. A create beyond the second
// ends the run at once, so that a regression does not make an endless roll.
func TestRollLoopKeepsAMachineItCreatedUntilTheCloudListsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		w.SetDownAfter(0)
		clusterTag := cloud.LabelCluster + "=prod"
		creates, lists, workers := 0, 4, 0
		var hidden string
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			now := 0
			for _, in := range f.Instances() {
				if strings.HasPrefix(in.Label, "prod-workers-") {
					now++
				}
			}
			workers = max(workers, now)
			switch {
			case c.Name == "CreateInstance":
				creates++
				if creates > 2 {
					return errors.New("the roll sent a third create")
				}
				err := next(ctx)
				if err == nil && creates == 2 {
					all := f.Instances()
					hidden = all[len(all)-1].ID
				}
				return err
			case c.Name == "ListInstances" && c.Arg == clusterTag && hidden != "" && lists > 0:
				lists--
				for _, in := range f.Instances() {
					if in.ID == hidden {
						f.SetInstanceTags(t, hidden, slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
							return tag == clusterTag
						})...)
						defer f.SetInstanceTags(t, hidden, in.Tags...)
					}
				}
			}
			return next(ctx)
		})
		updates := countCalls(f, "UpdateInstance")

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
			t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
		}
		if workers > 3 {
			t.Errorf("the cloud held %d workers at a call, want at most 3", workers)
		}
		if got := countCalls(f, "UpdateInstance") - updates; got != 2 {
			t.Errorf("UpdateInstance was called %d times, want once for each new machine", got)
		}
		f.SetHook(nil)
		wantNothingToRoll(t, svc)
	})
}

// TestRollLoopEndsWhenTheCloudNeverListsAMachineItCreated fails with the machine's name and ID after a minute of lists
// that miss it, and makes no second machine.
func TestRollLoopEndsWhenTheCloudNeverListsAMachineItCreated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		var id string
		var createdAt time.Time
		hideNewest(t, f, -1, func(created string) {
			id, createdAt = created, time.Now()
			w.WithholdInstance(created) // its node never registers, so the run keeps waiting for it
		})

		_, err := rollBefore(svc, loopPending)

		wantError(t, err, "the cloud does not list node prod-workers-2 (ID "+id+"), which this run created")
		wantWithin(t, time.Since(createdAt), loopPending)
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+1 {
			t.Errorf("CreateInstance was called %d times in all, want the update's %d and 1 more", got, len(allNames))
		}
	})
}

// TestRollLoopStopsAStepThatHasNoEffect ends the run when the third try of a step still leaves the same next step: a
// mark that Nomad drops. Each try comes a poll after the one before.
func TestRollLoopStopsAStepThatHasNoEffect(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		var times []time.Time
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				times = append(times, time.Now())
				return nil
			}
			return next(ctx)
		})

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "mark node prod-workers-0 ineligible had no effect after 3 tries")
		if len(times) != 3 {
			t.Fatalf("MarkIneligible was sent %d times, want 3", len(times))
		}
		wantPolls(t, times)
	})
}

// TestRollLoopForgetsAnEarlierFailureOfAStep ends a step that has no effect with that, not with the answer of a try
// that failed before: the first try finds the node gone, the next two do nothing.
func TestRollLoopForgetsAnEarlierFailureOfAStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		marks := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "MarkIneligible" {
				return next(ctx)
			}
			marks++
			if marks == 1 {
				return fmt.Errorf("no such node: %w", nomadops.ErrGone)
			}
			return nil
		})

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "mark node prod-workers-0 ineligible had no effect after 3 tries")
		if marks != 3 {
			t.Errorf("MarkIneligible was sent %d times, want 3", marks)
		}
	})
}

// TestRollLoopStopsAfterThreeTriesAtANodeThatIsGone ends the run with the write's own error after the third answer that
// the node is not in the cluster while the list still shows it, a poll after each try.
func TestRollLoopStopsAfterThreeTriesAtANodeThatIsGone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		var times []time.Time
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				times = append(times, time.Now())
				return fmt.Errorf("no such node: %w", nomadops.ErrGone)
			}
			return next(ctx)
		})

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "mark node prod-workers-0 ineligible: no such node: "+nomadops.ErrGone.Error())
		if !errors.Is(err, nomadops.ErrGone) {
			t.Errorf("the error %v does not match ErrGone", err)
		}
		if len(times) != 3 {
			t.Fatalf("MarkIneligible was sent %d times, want 3", len(times))
		}
		wantPolls(t, times)
	})
}

// TestRollLoopGoesOnAtANodeThatWentBetweenTheObservationAndTheWrite leads to the next decision when the mark finds its
// node gone: the machine of the missing node is deleted, and the mark is not sent for it again.
func TestRollLoopGoesOnAtANodeThatWentBetweenTheObservationAndTheWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		marks := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				marks++
				if marks == 1 {
					if err := w.Fake.Client(nomadops.Config{Address: "198.51.100.1:4646"}).Purge(ctx, c.Arg); err != nil {
						t.Errorf("Purge: %v", err)
					}
				}
			}
			return next(ctx)
		})

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 1, Deleted: 2, Purged: 1})
		if marks != 2 {
			t.Errorf("MarkIneligible was sent %d times, want once for each worker: the first found its node gone", marks)
		}
		wantNothingToRoll(t, svc)
	})
}

// TestRollLoopTriesAgainWhenNoServerAnswersAWrite goes on after two writes that no server answered, and ends with the
// error of the write after three.
func TestRollLoopTriesAgainWhenNoServerAnswersAWrite(t *testing.T) {
	t.Parallel()
	t.Run("two failures", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := outdatedWorld(t)
			lines := recordProgress(svc)
			for range 6 { // each try goes to the three servers
				w.Fail(t, "Drain", notReadyError("drain"))
			}

			counts, err := roll(svc, app.RollOptions{})

			wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
			failed := 0
			for _, line := range *lines {
				if strings.HasPrefix(line, "nomad failed drain prod-workers-0: ") {
					failed++
				}
			}
			if failed != 2 {
				t.Errorf("the drain of prod-workers-0 was reported failed %d times, want 2", failed)
			}
		})
	})
	t.Run("three failures", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := outdatedWorld(t)
			for range 9 {
				w.Fail(t, "Drain", notReadyError("drain"))
			}

			counts, err := roll(svc, app.RollOptions{})

			if err == nil || !errors.Is(err, nomadops.ErrNotReady) ||
				!strings.HasPrefix(err.Error(), "drain node prod-workers-0 within 1h0m0s: ") {
				t.Errorf("the roll ended with %v, want the error of the drain", err)
			}
			if counts.Drained != 0 {
				t.Errorf("the roll counts %d drained nodes, want none: no drain was sent", counts.Drained)
			}
		})
	})
}

// TestRollLoopCountsANodeOnceThatItDrainsTwice sends the drain again when Nomad shows no sign of the first one, and
// counts the node once.
func TestRollLoopCountsANodeOnceThatItDrainsTwice(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		drains, dropped := 0, false
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "Drain" {
				return next(ctx)
			}
			drains++
			if !dropped {
				dropped = true
				return nil
			}
			return next(ctx)
		})

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		if drains != 3 {
			t.Errorf("Drain was sent %d times, want 3: the first was lost, the others made the two drains", drains)
		}
	})
}

// TestRollLoopDoesNotSendADeleteAgain lists the machines at every poll while the cloud still lists a machine that it
// deleted, sends the delete once, and ends the run five minutes after it.
func TestRollLoopDoesNotSendADeleteAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		var deletedAt time.Time
		var id, name string
		deletes, lists := 0, 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			switch c.Name {
			case "DeleteInstance":
				deletes++
				id, deletedAt = c.Arg, time.Now()
				for _, in := range f.Instances() {
					if in.ID == id {
						name = in.Hostname
					}
				}
				return nil // the cloud took the delete and keeps listing the machine
			case "ListInstances":
				if !deletedAt.IsZero() {
					lists++
				}
			}
			return next(ctx)
		})

		_, err := rollBefore(svc, loopGoneTimeout)

		wantError(t, err, fmt.Sprintf("the cloud still lists node %s (ID %s) 5m0s after its delete", name, id))
		wantWithin(t, time.Since(deletedAt), loopGoneTimeout)
		if deletes != 1 {
			t.Errorf("DeleteInstance was sent %d times, want once", deletes)
		}
		if want := int(loopGoneTimeout / loopPoll); lists < want-5 {
			t.Errorf("the machines were listed %d times during the wait, want one at each poll (about %d)", lists, want)
		}
	})
}

// TestRollLoopTriesTheIntroTokenAgain goes on after two answers of no server to the request for an intro token, makes
// one machine for the slot, and ends before any create at an error that is not the answer of a missing server.
func TestRollLoopTriesTheIntroTokenAgain(t *testing.T) {
	t.Parallel()
	t.Run("no server answers twice", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := outdatedWorld(t)
			for range 6 { // each try goes to the three servers
				w.Fail(t, "IntroToken", notReadyError("intro token"))
			}

			counts, err := roll(svc, app.RollOptions{})

			wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
			if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
				t.Errorf("CreateInstance was called %d times in all, want the update's %d and 2 more", got, len(allNames))
			}
		})
	})
	t.Run("a permanent error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := outdatedWorld(t)
			w.Fail(t, "IntroToken", errors.New("permission denied"))

			counts, err := roll(svc, app.RollOptions{})

			wantError(t, err, "create node prod-workers-2 (client of workers, ams): "+
				"intro token for node prod-workers-2: permission denied")
			if got := countCalls(f, "CreateInstance"); got != len(allNames) {
				t.Errorf("CreateInstance was called %d times in all, want the update's %d", got, len(allNames))
			}
			if counts.Created != 0 {
				t.Errorf("the roll counts %d created machines, want none", counts.Created)
			}
		})
	})
}

// TestRollLoopWaitsForNomadToAnswerItsReads goes on when no server answers the reads of the observation for a while,
// and ends with the last error after ten minutes of it. An error that is not a missing answer ends the run at once.
func TestRollLoopWaitsForNomadToAnswerItsReads(t *testing.T) {
	t.Parallel()
	t.Run("for a while", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := outdatedWorld(t)
			start := time.Now()
			w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
				if c.Name == "Peers" && time.Since(start) < 20*time.Second {
					return notReadyError("peers")
				}
				return next(ctx)
			})

			counts, err := roll(svc, app.RollOptions{})

			wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
			if time.Since(start) < 20*time.Second {
				t.Errorf("the roll ended after %v, before the reads answered", time.Since(start))
			}
		})
	})
	t.Run("for ten minutes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := outdatedWorld(t)
			w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
				if c.Name == "Peers" {
					return notReadyError("peers")
				}
				return next(ctx)
			})
			start := time.Now()

			_, err := roll(svc, app.RollOptions{})

			if err == nil || !errors.Is(err, nomadops.ErrNotReady) ||
				!strings.HasPrefix(err.Error(), "read the Raft configuration: ") {
				t.Errorf("the roll ended with %v, want the error of the read", err)
			}
			wantWithin(t, time.Since(start), loopNodeTimeout)
		})
	})
	t.Run("another error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := outdatedWorld(t)
			w.Fail(t, "Health", errors.New("autopilot down"))
			start := time.Now()

			_, err := roll(svc, app.RollOptions{})

			wantError(t, err, "read autopilot's report: autopilot down")
			if time.Since(start) != 0 {
				t.Errorf("the run took %v, want it to end at once", time.Since(start))
			}
		})
	})
}

// TestRollLoopEndsWhenTheListFails ends the run at a list of the machines that fails, with what the run did so far
// counted.
func TestRollLoopEndsWhenTheListFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		created := false
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			created = created || c.Name == "CreateInstance"
			if created && c.Name == "ListInstances" && c.Arg == cloud.LabelCluster+"=prod" {
				return errors.New("cloud down")
			}
			return next(ctx)
		})

		counts, err := roll(svc, app.RollOptions{})

		if err == nil || !strings.Contains(err.Error(), "cloud down") {
			t.Errorf("the roll ended with %v, want the error of the list", err)
		}
		if counts != (app.RollCounts{Created: 1}) {
			t.Errorf("the roll did %+v, want the one create before the list", counts)
		}
	})
}

// TestRollLoopReportsAWaitForADrainOnce starts the wait for a drain once and ends it once, however many polls it takes.
func TestRollLoopReportsAWaitForADrainOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		w.SetDrainReads(3)
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			wantInOrder(t, *lines, "nomad done drain "+name, "nomad started drained "+name, "nomad done drained "+name,
				"node done delete "+name)
		}
	})
}

// TestRollLoopGivesUpWaitingForADrain ends the run five minutes after the deadline of a drain that never completes.
func TestRollLoopGivesUpWaitingForADrain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWith(t, drainTenMinutes)
		// Nomad's own deadline never ends this drain
		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			neverDrained(w, name)
		}
		id := instanceNamed(t, f, "prod-workers-0")
		var drainedAt time.Time
		var drain string
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Drain" && drainedAt.IsZero() {
				drainedAt, drain = time.Now(), c.Arg
			}
			return next(ctx)
		})
		lines := recordProgress(svc)

		_, err := rollBefore(svc, loopDrainTimeout)

		want := "node prod-workers-0 did not finish draining within 15m0s (Nomad lists it draining); " + rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(drainedAt), loopDrainTimeout)
		req := nomadops.DrainRequest{Deadline: 10 * time.Minute, Meta: map[string]string{"tent_machine": id}}
		if want := nomadfake.DrainArg(nodeIDOf(id), req); drain != want {
			t.Errorf("the first drain was %q, want %q: the node, the group's drain timeout and the machine", drain, want)
		}
		last := ""
		if n := len(*lines); n > 0 {
			last = (*lines)[n-1]
		}
		if last != "nomad failed drained prod-workers-0: "+want {
			t.Errorf("the last progress line is %q, want the failed wait", last)
		}
	})
}

// TestRollLoopGivesUpWaitingForANodeToGoDown ends the run six minutes after the last delete, when Nomad still lists
// the node of the deleted machine, and lists the machines once after that delete, not at each poll of the wait.
func TestRollLoopGivesUpWaitingForANodeToGoDown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		w.SetDownAfter(time.Hour)
		address := privateOf(t, f, instanceNamed(t, f, "prod-workers-0"))
		var deletedAt time.Time
		lists, listsAtDelete := 0, 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			switch c.Name {
			case "ListInstances":
				lists++
			case "DeleteInstance":
				defer func() { deletedAt, listsAtDelete = time.Now(), lists }()
			}
			return next(ctx)
		})

		_, err := rollBefore(svc, loopDownTimeout)

		wantError(t, err, fmt.Sprintf("node prod-workers-0 (%s) did not go down within 6m0s (Nomad lists it ready); %s",
			address, rollAgain))
		wantWithin(t, time.Since(deletedAt), loopDownTimeout)
		if lists-listsAtDelete > 1 {
			t.Errorf("the machines were listed %d times during the wait after the last delete, want once", lists-listsAtDelete)
		}
	})
}

// TestRollLoopGivesUpWaitingForTheServers ends the run after ten minutes in which a server reports no version, which
// holds the roll back.
func TestRollLoopGivesUpWaitingForTheServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		w.ChangeServer("prod-servers-0", func(s *nomadops.ServerHealth) { s.Version = "" })
		lines := recordProgress(svc)
		start := time.Now()

		_, err := rollBefore(svc, loopNodeTimeout)

		want := "the servers did not become healthy and vote within 10m0s " +
			"(autopilot reports 3 voters and the servers healthy); " + rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(start), loopNodeTimeout)
		wantLines(t, *lines, []string{"nomad started healthy", "nomad failed healthy: " + want})
	})
}

// TestRollLoopReportsWhatEachNomadStepWorksOn tells the node of each Nomad step, the node's address of a wait for it to
// go down and of a purge, and the deadline of a drain.
func TestRollLoopReportsWhatEachNomadStepWorksOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		address := privateOf(t, f, instanceNamed(t, f, "prod-workers-0"))
		var events []app.NomadEvent
		svc.OnProgress = func(p app.Progress) {
			if p.Nomad != nil && p.Step == app.NodeDone && p.Nomad.Node == "prod-workers-0" {
				events = append(events, *p.Nomad)
			}
		}

		if _, err := roll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the roll failed: %v", err)
		}

		want := []app.NomadEvent{
			{Action: app.NomadIneligible, Node: "prod-workers-0"},
			{Action: app.NomadDrain, Node: "prod-workers-0", Deadline: time.Hour},
			{Action: app.NomadDown, Node: "prod-workers-0", Address: address.String()},
			{Action: app.NomadPurge, Node: "prod-workers-0", Address: address.String()},
		}
		if diff := cmp.Diff(want, events); diff != "" {
			t.Errorf("the events of prod-workers-0 (-want +got):\n%s", diff)
		}
	})
}

// TestRollLoopEndsAtARefusalOfTheDecisions ends the run with the refusal of rollout, without a write.
func TestRollLoopEndsAtARefusalOfTheDecisions(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Version = "9.9.9" })
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		counts, err := roll(svc, app.RollOptions{})

		wantError(t, err, "tent never moves a node to an older Nomad: the cluster is pinned to "+
			decode(t, string(get(t, svc.Store, completedPath))).Cluster.Spec.Nomad.Version+
			", and node prod-workers-0 runs 9.9.9")
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		if counts != (app.RollCounts{}) {
			t.Errorf("the roll did %+v, want nothing", counts)
		}
	})
}

// TestRollLoopKeepsAWaitOpenWhileOtherStepsComeBetweenItsPolls reports the wait for a new node once and ends it when
// the node registered, though the decisions give the purge of an old node meanwhile: each new machine boots for a
// minute, and the node of a deleted machine reads down after the default delay.
func TestRollLoopKeepsAWaitOpenWhileOtherStepsComeBetweenItsPolls(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "CreateInstance" && err == nil {
				all := f.Instances()
				name := all[len(all)-1].Label
				w.Withhold(name)
				time.AfterFunc(time.Minute, func() { w.Release(name) })
			}
			return err
		})
		lines := recordProgress(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		for _, name := range []string{"prod-workers-2", "prod-workers-3"} {
			wantInOrder(t, *lines, "nomad started register "+name, "nomad done register "+name)
		}
		wantInOrder(t, *lines, "nomad done purge prod-workers-0", "nomad done register prod-workers-3")
	})
}

// TestRollLoopCountsAWaitsDeadlineFromItsFirstPollWhileOtherStepsComeBetween gives up on a node that never registers
// ten minutes after its wait began, though the purge of an old node came between the polls of the wait.
func TestRollLoopCountsAWaitsDeadlineFromItsFirstPollWhileOtherStepsComeBetween(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		creates := 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "CreateInstance" && err == nil {
				if creates++; creates == 2 {
					all := f.Instances()
					w.WithholdInstance(all[len(all)-1].ID)
				}
			}
			return err
		})
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Purge" {
				time.Sleep(time.Minute) // the step between the polls takes longer than the test's slack
			}
			return next(ctx)
		})
		lines := recordProgress(svc)
		record := svc.OnProgress
		var startedAt time.Time
		svc.OnProgress = func(p app.Progress) {
			record(p)
			if progressLine(p) == "nomad started register prod-workers-3" {
				startedAt = time.Now()
			}
		}

		_, err := roll(svc, app.RollOptions{})

		const want = "node prod-workers-3 did not join within 10m0s ("
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("error = %v\nwant it to start with %s", err, want)
		}
		wantWithin(t, time.Since(startedAt), loopNodeTimeout)
		wantInOrder(t, *lines, "nomad done purge prod-workers-0")
		wantInOrder(t, *lines, "nomad started register prod-workers-3")
		if at, purged := slices.Index(*lines, "nomad started register prod-workers-3"),
			slices.Index(*lines, "nomad done purge prod-workers-0"); purged < at {
			t.Errorf("the purge of prod-workers-0 came before the wait for prod-workers-3 began:\n%s",
				strings.Join(*lines, "\n"))
		}
	})
}

// TestRollLoopCountsEachWaitForTheServersFromItsOwnStart runs two waits for the servers in one roll, the second eleven
// minutes after the first, and finishes the roll: the second wait does not inherit the start of the first.
func TestRollLoopCountsEachWaitForTheServersFromItsOwnStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		blankFor := func(d time.Duration) {
			w.ChangeServer("prod-servers-0", func(s *nomadops.ServerHealth) { s.Version = "" })
			time.AfterFunc(d, func() { w.ChangeServer("prod-servers-0", func(*nomadops.ServerHealth) {}) })
		}
		blankFor(10 * time.Second)
		drains := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "Drain" && err == nil {
				if drains++; drains == 1 {
					time.Sleep(11 * time.Minute)
					blankFor(10 * time.Second)
				}
			}
			return err
		})

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
	})
}

// TestRollLoopForgetsAWaitForTheServersThatEndedWhileAnotherWaitWasOpen polls the wait for the servers while the wait
// for a drain is the open one, and again eleven minutes later: the second wait does not inherit the start of the first.
func TestRollLoopForgetsAWaitForTheServersThatEndedWhileAnotherWaitWasOpen(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWith(t, "  rollingUpdate:\n    maxSurge: 0\n    maxUnavailable: 2\n    drainTimeout: 1h\n")
		neverDrained(w, "prod-workers-0")
		w.SetDrainReads(3)
		blank := func() {
			w.ChangeServer("prod-servers-0", func(s *nomadops.ServerHealth) { s.Version = "" })
			time.AfterFunc(30*time.Second, func() { w.ChangeServer("prod-servers-0", func(*nomadops.ServerHealth) {}) })
		}
		deletes := 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "DeleteInstance" && err == nil {
				blank()
				if deletes++; deletes == 1 {
					time.AfterFunc(11*time.Minute, func() { w.ChangeNode("prod-workers-0", func(*nomadops.Node) {}) })
				}
			}
			return err
		})

		counts, err := rollBefore(svc, time.Hour)

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
	})
}
