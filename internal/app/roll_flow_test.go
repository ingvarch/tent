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

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// workerCount is the size of the test cluster's workers.
const workerCount = 2

// rollShape is how a group of workers rolls: how many nodes beyond its size it may have, and how many of its nodes may
// be unavailable.
type rollShape struct{ surge, unavailable int }

// yaml returns the lines of a node group's spec that give the shape.
func (s rollShape) yaml() string {
	return fmt.Sprintf("  rollingUpdate:\n    maxSurge: %d\n    maxUnavailable: %d\n", s.surge, s.unavailable)
}

// applyRoll runs a rolling update of the test cluster with apply.
func applyRoll(svc *app.Service, opts app.RollOptions) (app.RollPlan, error) {
	opts.Apply = true
	return svc.RollingUpdate(context.Background(), "prod", opts)
}

// workerIDs returns the IDs of the worker machines that f lists.
func workerIDs(f *vultrfake.Fake) []string {
	var ids []string
	for _, in := range f.Instances() {
		if strings.HasPrefix(in.Hostname, "prod-workers-") {
			ids = append(ids, in.ID)
		}
	}
	return ids
}

// availableWorkers returns how many of the nodes belong to the machines with the IDs ids and are ready, eligible and
// not draining.
func availableWorkers(ids []string, nodes []nomadops.Node) int {
	available := 0
	for _, n := range nodes {
		if slices.ContainsFunc(ids, func(id string) bool { return n.ID == nodeIDOf(id) }) &&
			n.Status == "ready" && n.Eligible && !n.Draining {
			available++
		}
	}
	return available
}

// TestAvailableWorkersCountsTheReadyEligibleNodesOfTheMachines counts a node of a listed machine that is ready,
// eligible and not draining, and none that is down, ineligible, draining or of another machine.
func TestAvailableWorkersCountsTheReadyEligibleNodesOfTheMachines(t *testing.T) {
	t.Parallel()
	ready := func(id string) nomadops.Node {
		return nomadops.Node{ID: nodeIDOf(id), Status: "ready", Eligible: true}
	}
	down, ineligible, draining := ready("c"), ready("d"), ready("e")
	down.Status, ineligible.Eligible, draining.Draining = "down", false, true
	nodes := []nomadops.Node{ready("a"), ready("b"), down, ineligible, draining, ready("gone")}

	if got := availableWorkers([]string{"a", "b", "c", "d", "e"}, nodes); got != 2 {
		t.Errorf("availableWorkers = %d, want the two ready and eligible nodes of listed machines", got)
	}
}

// holdLimits makes every call to Vultr and to Nomad check, before it runs, that the workers keep to the limits of the
// shape: no more machines than the size and maxSurge, and no fewer machines that are listed and whose node is ready,
// eligible and not draining than the size less maxUnavailable. It reads the nodes from the Nomad fake itself, which
// the world does not log. Its reads of the nodes are reads of the fake: they advance drains and are subject to Fail
// and LoseResponse.
func holdLimits(t *testing.T, f *vultrfake.Fake, w *nomadWorld, s rollShape) {
	t.Helper()
	check := func(ctx context.Context, call string) {
		ids := workerIDs(f)
		if len(ids) > workerCount+s.surge {
			t.Errorf("before %s the workers have %d machines, want at most %d", call, len(ids), workerCount+s.surge)
		}
		nodes, err := w.Client(nomadops.Config{Address: "198.51.100.1:4646"}).Nodes(ctx)
		if err != nil {
			t.Errorf("before %s: read the nodes: %v", call, err)
			return
		}
		available := availableWorkers(ids, nodes)
		if want := workerCount - s.unavailable; available < want {
			t.Errorf("before %s %d workers are available, want at least %d", call, available, want)
		}
	}
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		check(ctx, "Vultr "+c.Name+" "+c.Arg)
		return next(ctx)
	})
	w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		check(ctx, "Nomad "+c.Name+" "+c.Arg)
		return next(ctx)
	})
}

// workerHashes returns the spec hash tag of each worker machine of f, by name.
func workerHashes(f *vultrfake.Fake) map[string]string {
	out := map[string]string{}
	for _, in := range f.Instances() {
		if strings.HasPrefix(in.Hostname, "prod-workers-") {
			out[in.Hostname] = tagOf(in.Tags, cloud.LabelSpecHash)
		}
	}
	return out
}

// wantWorkersRolled fails the test unless the roll ended as wantRollEnded says and every worker has a new spec hash.
// before holds the workers' hashes before the roll, old the IDs of the machines that it replaced.
func wantWorkersRolled(t *testing.T, svc *app.Service, f *vultrfake.Fake, before map[string]string, old []string) {
	t.Helper()
	wantRollEnded(t, svc, f, old)
	for name, hash := range workerHashes(f) {
		if hash == "" || slices.Contains([]string{before["prod-workers-0"], before["prod-workers-1"]}, hash) {
			t.Errorf("worker %s has the spec hash %q after the roll, want a new one", name, hash)
		}
	}
}

// wantRollEnded fails the test unless the roll left the cluster as one that nobody needs to roll: nothing to roll,
// a valid cluster, Nomad with no node of a machine that is gone, the machines of old gone, and every machine with the
// joined label and the stub. old holds the IDs of the machines that the roll replaced.
func wantRollEnded(t *testing.T, svc *app.Service, f *vultrfake.Fake, old []string) {
	t.Helper()
	wantNothingToRoll(t, svc)
	v, err := svc.ValidateCluster(t.Context(), "prod")
	if err != nil {
		t.Fatalf("ValidateCluster: %v", err)
	}
	if !v.Valid() {
		t.Errorf("the cluster is not valid after the roll: %+v", v.Failures)
	}
	var listed []string
	for _, in := range f.Instances() {
		listed = append(listed, nodeIDOf(in.ID))
	}
	for _, n := range nomadNodes(t, svc) {
		if !slices.Contains(listed, n.ID) {
			t.Errorf("Nomad lists the node %s (%s) of a machine that is gone", n.Name, n.ID)
		}
	}
	for _, id := range old {
		if hasInstance(f, id) {
			t.Errorf("the machine %s that the roll replaced is still there", id)
		}
	}
	var names []string
	for _, in := range f.Instances() {
		names = append(names, in.Hostname)
	}
	wantJoined(t, f, names...)
}

// rollFlowWorld returns the test cluster whose workers are outdated and have the shape, and the limits that the roll
// has to keep to.
func rollFlowWorld(t *testing.T, s rollShape) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := outdatedWith(t, s.yaml())
	holdLimits(t, f, w, s)
	return svc, f, w
}

// TestRollFlowReplacesTheWorkersBySurgeOne applies the roll of the two outdated workers with maxSurge 1 and
// maxUnavailable 0: the plan that it returns and the calls to Vultr and Nomad match golden files, and the cluster ends
// as wantWorkersRolled says.
func TestRollFlowReplacesTheWorkersBySurgeOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollFlowWorld(t, rollShape{surge: 1})
		before := workerHashes(f)
		old := workerIDs(f)
		var plan app.RollPlan
		var err error

		calls := flowCalls(t, svc, f, w, func() *engine.Plan {
			plan, err = applyRoll(svc, app.RollOptions{})
			return nil
		})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if !plan.Applied || plan.Rolled != (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}) {
			t.Errorf("the plan says applied %v and rolled %+v, want the roll of both workers", plan.Applied, plan.Rolled)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		checkGolden(t, "flow_roll.plan.golden", text.String())
		checkGolden(t, "flow_roll.calls.golden", callsText(calls))
		wantWorkersRolled(t, svc, f, before, old)
	})
}

// TestRollFlowReportsEachStepOfTheRoll applies the roll with other shapes: with maxSurge 2 both new workers come
// first; with maxSurge 0 and maxUnavailable 1 each worker goes before its successor comes. The progress lines match
// golden files, the workers keep to the limits of the shape at every call, and the cluster ends as wantWorkersRolled
// says.
func TestRollFlowReportsEachStepOfTheRoll(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		shape  rollShape
		golden string
	}{
		{"surge two", rollShape{surge: 2}, "flow_roll_surge2.steps.golden"},
		{"one unavailable", rollShape{unavailable: 1}, "flow_roll_unavailable1.steps.golden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, _ := rollFlowWorld(t, tc.shape)
				before := workerHashes(f)
				old := workerIDs(f)
				lines := recordProgress(svc)

				plan, err := applyRoll(svc, app.RollOptions{})

				if err != nil {
					t.Fatalf("RollingUpdate: %v", err)
				}
				if plan.Rolled != (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}) {
					t.Errorf("the roll did %+v, want both workers replaced", plan.Rolled)
				}
				checkGolden(t, tc.golden, strings.Join(*lines, "\n")+"\n")
				wantWorkersRolled(t, svc, f, before, old)
			})
		})
	}
}

// TestRollFlowForceReplacesTheSelectedWorkersOnce rolls two workers that are up to date because it is forced: it
// replaces each once, and the machines that it makes are not forced, so it ends.
func TestRollFlowForceReplacesTheSelectedWorkersOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		holdLimits(t, f, w, rollShape{surge: 1})
		old := workerIDs(f)

		plan, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{"workers"}, Force: true})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, old)
		if got := len(workerIDs(f)); got != workerCount {
			t.Errorf("the workers have %d machines, want %d", got, workerCount)
		}
	})
}

// TestRollFlowRollsTheWorkersWhileTheServersAreOutdated rolls the selected client group and leaves the outdated
// servers as they are.
func TestRollFlowRollsTheWorkersWhileTheServersAreOutdated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		mustReplace(t, svc, keyedClusterYAML+"  nomad:\n    extraConfig:\n      server: 'raft_multiplier = 3'\n",
			workersMetaYAML)
		mustUpdate(t, svc)
		before, _ := rollingUpdate(svc, app.RollOptions{})
		if len(before.Groups) == 0 || len(before.Groups[0].Outdated) != 3 {
			t.Fatalf("the servers are not outdated: %+v", before.Groups)
		}
		holdLimits(t, f, w, rollShape{surge: 1})
		servers := []string{
			instanceNamed(t, f, "prod-servers-0"), instanceNamed(t, f, "prod-servers-1"), instanceNamed(t, f, "prod-servers-2"),
		}

		plan, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{"workers"}})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		for _, id := range servers {
			if !hasInstance(f, id) {
				t.Errorf("the server machine %s is gone", id)
			}
		}
		plan, err = rollingUpdate(svc, app.RollOptions{NodeGroups: []string{"workers"}})
		if err != nil || plan.Next != nil {
			t.Errorf("a plan of the workers after the roll has the step %+v and the error %v, want neither", plan.Next, err)
		}
	})
}

// TestRollFlowTakesNoLockWithNothingToRoll applies a roll of a cluster that has nothing to roll: it only reads, does
// not take the lock that the test holds, and says it applied.
func TestRollFlowTakesNoLockWithNothingToRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		u := watch(t, svc, f, w)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		u.check(t)
		if !plan.Applied || plan.Next != nil || plan.Rolled != (app.RollCounts{}) {
			t.Errorf("the plan says applied %v, next %+v, rolled %+v; want applied, no step, nothing rolled",
				plan.Applied, plan.Next, plan.Rolled)
		}
	})
}

// TestRollFlowRefusesAtTheStartWithoutALock applies a forced roll of the default selection, which tent refuses at its
// start because the server group has a step: it returns the refusal with the groups, only reads, and does not take
// the lock that the test holds.
func TestRollFlowRefusesAtTheStartWithoutALock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		u := watch(t, svc, f, w)

		plan, err := applyRoll(svc, app.RollOptions{Force: true})

		wantError(t, err, "node group servers: tent cannot roll server and combined groups yet; "+
			"select client groups with --nodegroups")
		u.check(t)
		if len(plan.Groups) == 0 || plan.Next != nil || plan.Applied {
			t.Errorf("plan = %+v, want the groups, no next step and not applied", plan)
		}
	})
}

// TestRollFlowFailsLikeThePlanAtTheStart applies a roll whose first check fails: it returns the error and no plan.
func TestRollFlowFailsLikeThePlanAtTheStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		u := watch(t, svc, f, w)

		plan, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{"db"}})

		wantError(t, err, "node group db is not in the specs of cluster prod; its node groups are servers and workers")
		u.check(t)
		if len(plan.Groups) != 0 || plan.Applied {
			t.Errorf("plan = %+v, want the zero plan", plan)
		}
	})
}

// TestRollFlowHoldsTheLockWhileItRolls tells OnRollPlan the plan under a lock whose lease names the roll, which the
// roll releases at its end. The plan that it returns holds the step that it began with.
func TestRollFlowHoldsTheLockWhileItRolls(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		var seen []*statestore.Lease
		var plans []app.RollPlan
		svc.OnRollPlan = func(p app.RollPlan) error {
			seen = append(seen, holder(t, svc.Store))
			plans = append(plans, p)
			return nil
		}
		svc.OnProgress = func(p app.Progress) {
			if len(seen) != 1 {
				t.Error("the roll reported progress before OnRollPlan")
			}
			if l := holder(t, svc.Store); l == nil || l.Operation != "rolling-update" {
				t.Errorf("%s outside the lease of rolling-update: %+v", progressLine(p), l)
			}
		}

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if len(seen) != 1 || seen[0] == nil || seen[0].Operation != "rolling-update" {
			t.Fatalf("OnRollPlan found the lease %+v in %d calls, want one call under the lease of rolling-update", seen,
				len(seen))
		}
		if plans[0].Next == nil || plans[0].Next.Action != "create" || plans[0].Applied {
			t.Errorf("OnRollPlan got %+v, want the plan with the first step, not applied", plans[0])
		}
		if plan.Next == nil || plan.Next.Text != plans[0].Next.Text {
			t.Errorf("the returned plan has the step %+v, want the one that OnRollPlan was told, %+v", plan.Next, plans[0].Next)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestRollFlowWaitsForTheLock waits as an update does while another tent holds the lock, then rolls.
func TestRollFlowWaitsForTheLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		held := holdLock(t, svc.Store)
		var waits int
		svc.LockTimeout = time.Minute
		svc.OnWait = func(error) { waits++ }
		done := make(chan error, 1)
		go func() {
			_, err := applyRoll(svc, app.RollOptions{})
			done <- err
		}()

		time.Sleep(10 * time.Second)
		synctest.Wait()
		if waits != 1 {
			t.Errorf("OnWait was called %d times, want once", waits)
		}
		if got := countCalls(f, "CreateInstance"); got != len(allNames) {
			t.Errorf("CreateInstance was called %d times while the lock was held, want the update's %d", got, len(allNames))
		}
		release(t, held)
		if err := <-done; err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
			t.Errorf("CreateInstance was called %d times, want the update's %d and 2 more", got, len(allNames))
		}
	})
}

// TestRollFlowStopsWhenTheLockIsLost ends the roll when its lock is removed, while a drain still runs, and returns what
// it did until then.
func TestRollFlowStopsWhenTheLockIsLost(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWith(t, drainTenMinutes)
		w.SetDrainReads(1 << 30) // the drain ends at its deadline
		svc.Store = forceUnlocked{unwrapped{svc.Store}, lockPath}

		plan, err := applyRoll(svc, app.RollOptions{})

		if !errors.Is(err, statestore.ErrLockLost) || app.Saved(err) ||
			!strings.HasPrefix(err.Error(), "lost the lock of cluster prod; stopped: ") {
			t.Errorf("error = %v, want the roll stopped for the loss of its lock", err)
		}
		if plan.Applied || plan.Rolled.Created != 1 || plan.Rolled.Deleted != 0 {
			t.Errorf("the plan says applied %v, rolled %+v; want a roll that created one machine and deleted none",
				plan.Applied, plan.Rolled)
		}
	})
}

// beforeLock runs a function before the first write of the lock's lease.
type beforeLock struct {
	statestore.Store
	run func()
}

func (b *beforeLock) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if run := b.run; p == lockPath && run != nil {
		b.run = nil
		run()
	}
	return b.Store.Put(ctx, p, data, opts)
}

// TestRollFlowPlansAgainUnderTheLock rolls what the cluster is when it has the lock: a machine that lost its hash
// before then is outdated for that, where the first plan found it outdated for its old hash.
func TestRollFlowPlansAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		id := instanceNamed(t, f, "prod-workers-0")
		svc.Store = &beforeLock{Store: unwrapped{svc.Store}, run: func() {
			for _, in := range f.Instances() {
				if in.ID == id {
					f.SetInstanceTags(t, id, slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
						return strings.HasPrefix(tag, cloud.LabelSpecHash+"=")
					})...)
				}
			}
		}}
		var told app.RollPlan
		svc.OnRollPlan = func(p app.RollPlan) error {
			told = p
			return nil
		}

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		var got []string
		for _, g := range told.Groups {
			for _, n := range g.Outdated {
				got = append(got, n.Name+" "+n.Reason)
			}
		}
		if want := []string{"prod-workers-0 no spec hash", "prod-workers-1 spec hash"}; !slices.Equal(want, got) {
			t.Errorf("OnRollPlan was told the outdated machines %q, want %q", got, want)
		}
	})
}

// TestRollFlowStopsWhenOnRollPlanFails returns the error of OnRollPlan before the roll changes anything, writes no
// replace label also with Force, and releases the lock.
func TestRollFlowStopsWhenOnRollPlanFails(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]app.RollOptions{
		"plain":  {},
		"forced": {NodeGroups: []string{"workers"}, Force: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := outdatedWorld(t)
				cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
				declined := errors.New("declined")
				svc.OnRollPlan = func(app.RollPlan) error { return declined }
				var events []string
				svc.OnWarning = func(string) { events = append(events, "warning") }
				svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }

				plan, err := applyRoll(svc, opts)

				if !errors.Is(err, declined) {
					t.Errorf("error = %v, want the error of OnRollPlan", err)
				}
				wantNoWrites(t, f.Calls()[cloudCalls:])
				if got := labelled(f); len(got) != 0 {
					t.Errorf("machines with the replace label = %q, want none", got)
				}
				if got := nomadWrites(w, nomadCalls); len(got) > 0 {
					t.Errorf("the roll wrote to Nomad before it started: %q", got)
				}
				if len(events) > 0 || plan.Applied || plan.Rolled != (app.RollCounts{}) {
					t.Errorf("events %q, applied %v, rolled %+v; want nothing after the refusal", events, plan.Applied,
						plan.Rolled)
				}
				wantLockFree(t, svc.Store)
			})
		})
	}
}

// TestRollFlowTellsTheWarningsOnceBeforeTheFirstStep tells OnWarning the warnings of the cluster after OnRollPlan and
// before the first step, only for a roll that applies a step.
func TestRollFlowTellsTheWarningsOnceBeforeTheFirstStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t) // access.api left out is 0.0.0.0/0
		var events []string
		svc.OnRollPlan = func(app.RollPlan) error {
			events = append(events, "plan")
			return nil
		}
		svc.OnWarning = func(w string) { events = append(events, w) }
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }

		if _, err := rollingUpdate(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate without apply: %v", err)
		}
		if len(events) > 0 {
			t.Errorf("a plan without apply told %q, want nothing", events)
		}
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		first := slices.Index(events, "node started create prod-workers-2")
		if first < 0 {
			t.Fatalf("the progress has no first step: %q", events)
		}
		if events[0] != "plan" || first < 2 || !slices.Contains(events[1:first], openAPIWarning) {
			t.Errorf("the events are %q, want the plan, the warnings, then the first step", events[:min(4, len(events))])
		}
		for _, e := range events[:first] {
			if strings.HasPrefix(e, "node ") || strings.HasPrefix(e, "nomad ") {
				t.Errorf("the event %q came before the first step", e)
			}
		}
		if n := count(events, openAPIWarning); n != 1 {
			t.Errorf("the warning was told %d times, want once", n)
		}
		before := len(events)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate with nothing to roll: %v", err)
		}
		if len(events) != before {
			t.Errorf("a roll with nothing to roll told %q, want nothing", events[before:])
		}
	})
}

// TestRollFlowReturnsWhatItDidWhenInterrupted ends with the interruption when the context ends at the first drain, and
// returns the machine that it created by then. The lock is released.
func TestRollFlowReturnsWhatItDidWhenInterrupted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				cancel()
			}
			return next(ctx)
		})

		plan, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})

		if err == nil || err.Error() != "interrupted" || !errors.Is(app.Stands(err), context.Canceled) {
			t.Errorf("error = %v (standing for %v), want an interruption", err, app.Stands(err))
		}
		if want := (app.RollCounts{Created: 1}); plan.Rolled != want || plan.Applied {
			t.Errorf("rolled %+v, applied %v; want %+v, not applied", plan.Rolled, plan.Applied, want)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestRollFlowFinishesARollThatStopped runs a roll again after an interruption: the second run goes on from what the
// cloud and Nomad report, so that the two make two machines in all.
func TestRollFlowFinishesARollThatStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		before := workerHashes(f)
		old := workerIDs(f)
		ctx, cancel := context.WithCancel(t.Context())
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				cancel()
			}
			return next(ctx)
		})
		if _, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true}); err == nil {
			t.Fatal("the first run did not stop")
		}
		w.SetHook(nil)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if plan.Rolled.Created != 1 {
			t.Errorf("the second run created %d machines, want 1: the first made the other", plan.Rolled.Created)
		}
		if got := countCalls(f, "CreateInstance"); got != len(allNames)+2 {
			t.Errorf("CreateInstance was called %d times, want the update's %d and 2 more", got, len(allNames))
		}
		wantWorkersRolled(t, svc, f, before, old)
	})
}

// TestRollFlowFindsTheReleaseFilesOnce asks for as many release files for the two plans and the creates of a roll as
// for the plan alone.
func TestRollFlowFindsTheReleaseFilesOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		sites := withAssets(svc)
		if _, err := rollingUpdate(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate without apply: %v", err)
		}
		planned := len(sites.URLs())
		if planned == 0 {
			t.Fatal("the plan asked for no release file")
		}
		sites = withAssets(svc)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		if got := len(sites.URLs()); got != planned {
			t.Errorf("the roll asked for %d release files %v, want the %d that a plan asks for", got, sites.URLs(), planned)
		}
	})
}

// TestRollFlowEndsAppliedWhenAnotherRollFinishedFirst finds nothing to roll under the lock because another tent rolled
// the workers while this one waited for it: it tells neither OnRollPlan nor OnWarning, makes no machine, and says it
// applied.
func TestRollFlowEndsAppliedWhenAnotherRollFinishedFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		svc.Store = &beforeLock{Store: unwrapped{svc.Store}, run: func() {
			if _, err := roll(svc, app.RollOptions{}); err != nil {
				t.Errorf("the other roll failed: %v", err)
			}
		}}
		svc.OnRollPlan = func(app.RollPlan) error {
			t.Error("OnRollPlan was called for a plan without a step")
			return nil
		}
		svc.OnWarning = func(w string) { t.Errorf("OnWarning was called with %q for a plan without a step", w) }
		made := 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "CreateInstance" {
				made++
			}
			return next(ctx)
		})

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if !plan.Applied || plan.Next != nil || plan.Rolled != (app.RollCounts{}) {
			t.Errorf("the plan says applied %v, next %+v, rolled %+v; want applied, no step, nothing rolled",
				plan.Applied, plan.Next, plan.Rolled)
		}
		if made != workerCount {
			t.Errorf("%d machines were made, want the %d of the other roll", made, workerCount)
		}
	})
}
