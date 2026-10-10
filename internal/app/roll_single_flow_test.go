package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// singleWorld is a cluster of one outdated server and the workers, with the leader's reconcile on, a halted machine
// that runs for 9 s more as far as Nomad goes, and the invariants of a group of one watching. T, the time of the
// roll's transfer of the leadership, is when the leader's minute starts: its reconciles come at T plus a multiple of
// the reconcile interval.
type singleWorld struct {
	svc *app.Service
	f   *vultrfake.Fake
	w   *nomadWorld
	inv *serverInvariants
	old []serverMachine // the server before the roll
	// creates is how many times CreateInstance had been called when the world was made.
	creates int

	mu    sync.Mutex
	halts []time.Time // when the roll sent each HaltInstance
}

// singleHaltLag is how long the machine of a halted server runs for more, as far as Nomad goes: thirty times what the
// early run on a real cloud saw.
const singleHaltLag = 9 * time.Second

// singleCluster returns the test cluster with one server and two workers after an update, in a world whose leader
// reconciles every `every`, whose autopilot promotes a server that was added again `promote` later, and whose halted
// machines run for singleHaltLag more.
func singleCluster(t *testing.T, every, promote time.Duration) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t, keyedClusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
	w.ServersOverTime()
	w.SetReconcile(every, promote)
	w.SetHaltLag(singleHaltLag)
	mustUpdate(t, svc)
	return svc, f, w
}

// newSingleWorld returns a singleWorld in the world of singleCluster. The server's spec changed and the change was
// applied by an update.
func newSingleWorld(t *testing.T, every, promote time.Duration) *singleWorld {
	t.Helper()
	svc, f, w := singleCluster(t, every, promote)
	mustReplace(t, svc, keyedClusterYAML+serversExtraYAML)
	mustUpdate(t, svc)
	s := &singleWorld{svc: svc, f: f, w: w, inv: watchServers(t, f, w, 1), old: serverMachines(f),
		creates: countCalls(f, "CreateInstance")}
	s.hooks(nil, nil)
	return s
}

// hooks makes every call to Vultr and to Nomad check the invariants and then go through the hook for it, which a nil
// hook leaves out, and notes when the roll sends a HaltInstance.
func (s *singleWorld) hooks(onVultr vultrfake.Hook, onNomad nomadHook) {
	s.inv.watch(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		if c.Name == "HaltInstance" {
			s.mu.Lock()
			s.halts = append(s.halts, time.Now())
			s.mu.Unlock()
		}
		if onVultr == nil {
			return next(ctx)
		}
		return onVultr(ctx, c, next)
	}, onNomad)
}

// transferred returns T, when the roll moved the leadership; zero if it has not.
func (s *singleWorld) transferred() time.Time {
	s.inv.mu.Lock()
	defer s.inv.mu.Unlock()
	return s.inv.transferred
}

// haltTimes returns when the roll sent each HaltInstance.
func (s *singleWorld) haltTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.halts)
}

// stamped is a progress line and when the roll reported it.
type stamped struct {
	at   time.Time
	line string
}

// stampProgress makes svc record its progress lines with their times, and returns them.
func stampProgress(svc *app.Service) *[]stamped {
	var out []stamped
	svc.OnProgress = func(p app.Progress) { out = append(out, stamped{time.Now(), progressLine(p)}) }
	return &out
}

// whenLine returns when the progress first had the line, or the zero time.
func whenLine(lines []stamped, line string) time.Time {
	for _, l := range lines {
		if l.line == line {
			return l.at
		}
	}
	return time.Time{}
}

// cutRun runs a rolling update with apply that ends its context at the nth call (from 1) of the Vultr or Nomad
// method name, just before the call reaches the fake or, when after is set, just after the fake carried it out, as
// cutServersRun does. It fails the test unless the run made the call and returned an error that matches
// context.Canceled, and puts the plain hooks back.
func (s *singleWorld) cutRun(t *testing.T, name string, nth int, after bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	var cut atomic.Bool
	act := func(ctx context.Context, next func(context.Context) error) error {
		cut.Store(true)
		if after {
			_ = next(ctx) // carried out; its answer is lost
		} else {
			s.inv.cutBeforeFake()
		}
		cancel()
		return next(ctx)
	}
	s.hooks(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		if c.Name == name && int(calls.Add(1)) == nth {
			return act(ctx, next)
		}
		return next(ctx)
	}, func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		if c.Name == name && int(calls.Add(1)) == nth {
			return act(ctx, next)
		}
		return next(ctx)
	})

	_, err := s.svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})

	s.hooks(nil, nil)
	if !cut.Load() {
		t.Fatalf("the run made no call %s %d", name, nth)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the run cut at %s %d returned %v, want an error that matches context.Canceled", name, nth, err)
	}
	wantLockFree(t, s.svc.Store)
}

// since returns when each of the times came after the transfer.
func (s *singleWorld) since(times []time.Time) []time.Duration {
	moved := s.transferred()
	out := make([]time.Duration, len(times))
	for i, at := range times {
		out[i] = at.Sub(moved)
	}
	return out
}

// wantHaltIn fails the test unless the roll sent one HaltInstance and it came at least from and less than to after
// the transfer.
func (s *singleWorld) wantHaltIn(t *testing.T, from, to time.Duration) {
	t.Helper()
	halts := s.since(s.haltTimes())
	if len(halts) != 1 {
		t.Fatalf("the roll sent %d HaltInstance calls, at %v, want 1", len(halts), halts)
	}
	if halts[0] < from || halts[0] >= to {
		t.Errorf("the roll halted the old server %v after the transfer, want it from %v to %v", halts[0], from, to)
	}
}

// holdRead returns a Nomad hook that holds the first call of the method name that is made at or after from, a time
// after the transfer, until until, also a time after the transfer.
func (s *singleWorld) holdRead(name string, from, until time.Duration) nomadHook {
	var held atomic.Bool
	return func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		moved := s.transferred()
		if c.Name == name && !moved.IsZero() && !time.Now().Before(moved.Add(from)) && held.CompareAndSwap(false, true) {
			time.Sleep(time.Until(moved.Add(until)))
		}
		return next(ctx)
	}
}

// wantNewServers fails the test unless the runs since the world was made called CreateInstance n times.
func (s *singleWorld) wantNewServers(t *testing.T, n int) {
	t.Helper()
	if got := countCalls(s.f, "CreateInstance") - s.creates; got != n {
		t.Errorf("CreateInstance was called %d times since the update, want %d", got, n)
	}
}

// wantRemovedCalls fails the test when more than most calls went to the address of a removed server that runs.
func (s *singleWorld) wantRemovedCalls(t *testing.T, most int) {
	t.Helper()
	if got := s.w.RemovedCalls(); len(got) > most {
		t.Errorf("the roll called a removed server that runs %d times, want at most %d: %v", len(got), most, got)
	}
}

// wantRolled fails the test unless the roll of the single server ended as wantRollEnded and wantServersRolledOf
// say, with no call to a server that the roll stopped, the workers on the machines they had, and their nodes ready.
func (s *singleWorld) wantRolled(t *testing.T, workers []string) {
	t.Helper()
	wantRollEnded(t, s.svc, s.f, idsOf(s.old))
	wantServersRolledOf(t, s.svc, s.f, 1, s.old, true)
	if got := workerIDs(s.f); !slices.Equal(got, workers) {
		t.Errorf("the workers are the machines %v, want %v", got, workers)
	}
	if got := availableWorkers(workers, nomadNodes(t, s.svc)); got != len(workers) {
		t.Errorf("%d of the %d workers are ready, want all", got, len(workers))
	}
	s.inv.wantKept()
}

// TestSingleFlowRollsTheServerThroughTwoVoters applies the roll of a cluster of one outdated server and matches the
// calls to Vultr and Nomad to a golden file. The leadership moves once; the old server's peer goes while it runs, and
// again after the leader added it; the machine is halted in the 10 s after the leader's reconcile at two minutes after
// the transfer, and the roll waits for no server to go down. The cluster ends with one voter, the new server.
func TestSingleFlowRollsTheServerThroughTwoVoters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		var plan app.RollPlan
		var err error

		calls := flowCalls(t, s.svc, s.f, s.w, func() *engine.Plan {
			plan, err = applyRoll(s.svc, app.RollOptions{})
			return nil
		})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); !plan.Applied || plan.Rolled != want {
			t.Errorf("the plan says applied %v and rolled %+v, want applied and %+v", plan.Applied, plan.Rolled, want)
		}
		checkGolden(t, "flow_roll_single_server.calls.golden", callsText(collapsePolls(calls)))
		s.wantHaltIn(t, 2*time.Minute, 2*time.Minute+10*time.Second)
		old := raftIDOf(s.old[0].id)
		if diff := cmp.Diff([]string{old, old}, nomadArgs(s.w, "RemovePeer")); diff != "" {
			t.Errorf("the peers that the roll removed (-want +got):\n%s", diff)
		}
		s.wantRemovedCalls(t, 0)
		s.wantFinished(t, workers)
	})
}

// reconcileWaitStart and reconcileWaitEnd are when, after the transfer, the roll reports the wait for the leader's
// reconcile as started and as done: the first removal comes when the stop window ends, 70 s after the transfer.
const (
	reconcileWaitStart = 70 * time.Second
	reconcileWaitEnd   = 2 * time.Minute
)

// TestSingleFlowReportsEachStepOfTheRoll applies the same roll and matches its progress lines to a golden file: each
// step started and done, the wait for the reconcile between the two removals, and no wait for a server to go down.
// The wait for the reconcile starts when the first removal is done and ends at the reconcile that the roll sees.
func TestSingleFlowReportsEachStepOfTheRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		lines := stampProgress(s.svc)

		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		var text []string
		for _, l := range *lines {
			text = append(text, l.line)
		}
		checkGolden(t, "flow_roll_single_server.steps.golden", strings.Join(text, "\n")+"\n")
		if got := count(text, "nomad started server-down prod-servers-0"); got != 0 {
			t.Errorf("the roll waited for autopilot to drop the server %d times, want 0", got)
		}
		moved := s.transferred()
		start := whenLine(*lines, "nomad started reconcile prod-servers-0").Sub(moved)
		end := whenLine(*lines, "nomad done reconcile prod-servers-0").Sub(moved)
		if start != reconcileWaitStart || end != reconcileWaitEnd {
			t.Errorf("the wait for the reconcile ran from %v to %v after the transfer, want %v to %v", start, end,
				reconcileWaitStart, reconcileWaitEnd)
		}
	})
}

// TestSingleFlowSettlesOnceAfterTheTransferBlip rolls the server where the new leader reads unhealthy for 2 s after
// the transfer: the roll waits for one settle after it, and goes on to the removals.
func TestSingleFlowSettlesOnceAfterTheTransferBlip(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		s.w.SetTransferBlip(2 * time.Second)
		workers := workerIDs(s.f)
		lines := recordProgress(s.svc)

		plan, err := applyRoll(s.svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantInOrder(t, *lines, "nomad done transfer prod-servers-0", "nomad started settle", "nomad done settle",
			"nomad started reconcile prod-servers-0", "nomad done reconcile prod-servers-0",
			"node started stop prod-servers-0")
		if got := count(*lines, "nomad started settle"); got != 1 {
			t.Errorf("the roll settled %d times, want once", got)
		}
		if got := callsOf(s.f, "HaltInstance"); len(got) != 1 {
			t.Errorf("the cloud halted %v, want one machine", got)
		}
		s.wantFinished(t, workers)
	})
}

// TestSingleFlowForceReplacesTheServerAndTheWorkersOnce rolls an up-to-date cluster of one server with force: the
// server and the workers are each replaced once, the new server has the hash of the old one, and the machines that
// the roll makes are not forced, so it ends.
func TestSingleFlowForceReplacesTheServerAndTheWorkersOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := singleCluster(t, time.Minute, 10*time.Second)
		inv := watchServers(t, f, w, 1)
		oldServers, oldWorkers := serverMachines(f), workerIDs(f)

		plan, err := applyRoll(svc, app.RollOptions{Force: true})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Drained: 2, Stopped: 1, Deleted: 3, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, append(idsOf(oldServers), oldWorkers...))
		wantServersRolledOf(t, svc, f, 1, oldServers, false)
		if got := len(workerIDs(f)); got != workerCount {
			t.Errorf("the workers have %d machines, want %d", got, workerCount)
		}
		inv.wantKept()
		inv.wantQuorumFor(quorumSpan)
	})
}

// TestSingleFlowRefusesAGroupOfOneServerWithoutAllowSingleServer rolls with the validation of a service that does not
// allow a single server: the run fails with the validation's error and calls neither the cloud nor Nomad.
func TestSingleFlowRefusesAGroupOfOneServerWithoutAllowSingleServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		s.svc.Validate = v1alpha1.ValidateOptions{}
		cloudCalls, nomadCalls := len(s.f.Calls()), len(s.w.Log())

		_, err := applyRoll(s.svc, app.RollOptions{})

		wantError(t, err, "NodeGroup servers: spec.size: size 1 needs --allow-single-server")
		if got := len(s.f.Calls()) - cloudCalls; got != 0 {
			t.Errorf("the refused run made %d calls to the cloud, want none", got)
		}
		if got := len(s.w.Log()) - nomadCalls; got != 0 {
			t.Errorf("the refused run made %d calls to Nomad, want none", got)
		}
	})
}

// wantFinished fails the test unless the roll of the single server has ended, with the invariants kept, the voters
// that run a quorum for the time that quorumSpan gives after the last write, and the machine of one server made once.
func (s *singleWorld) wantFinished(t *testing.T, workers []string) {
	t.Helper()
	s.inv.wantQuorumFor(quorumSpan)
	s.wantNewServers(t, 1)
	s.wantRolled(t, workers)
}

// quorumSpan is how long the flows let the clock run after the last write of the roll: two reconciles and a
// promotion.
const quorumSpan = 2*time.Minute + 15*time.Second

// TestSingleHoldSendsNoStopOnAReAddThatTheRunSawLate holds the Health read of the observation that shows the leader's
// re-add at two minutes after the transfer, until 165 s, 15 s before the next reconcile. The roll removes the server,
// which votes again by then, and holds the stop: the re-add was seen 45 s ago. It sees the re-add of 180 s, removes
// the server and stops it then.
func TestSingleHoldSendsNoStopOnAReAddThatTheRunSawLate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		s.hooks(nil, s.holdRead("Health", 2*time.Minute, 165*time.Second))

		_, err := applyRoll(s.svc, app.RollOptions{})

		s.inv.wantQuorumFor(quorumSpan)
		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		s.wantHaltIn(t, 3*time.Minute, 3*time.Minute+10*time.Second)
		s.wantSecondRemovalAt(t, 165*time.Second)
		s.wantRolled(t, workers)
	})
}

// TestSingleHoldFollowsTheReAddOfAResumedRun cuts a run right after its transfer and starts the next at 119 s, where
// the window is over and the first write is the removal of the voter. The run holds the stop, sees the re-add of 120 s
// at 121 s, removes it and sends the stop, and is cut right after the call. The machine stops while the leader's next
// reconcile is a minute off, the voters that run stay a quorum, and a third run finishes.
func TestSingleHoldFollowsTheReAddOfAResumedRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		s.cutRun(t, "TransferLeadership", 1, true)
		sleepUntil(s.transferred(), 119*time.Second)

		s.cutRun(t, "HaltInstance", 1, true)

		s.inv.wantQuorumFor(quorumSpan)
		s.wantHaltIn(t, 2*time.Minute, 2*time.Minute+10*time.Second)
		if removed := s.since(s.inv.reads("RemovePeer")); len(removed) != 2 || removed[0] != 119*time.Second {
			t.Errorf("the roll removed the peer at %v, want twice, the first at 1m59s", removed)
		}
		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}
		s.wantFinished(t, workers)
	})
}

// TestSingleHoldStartsAtAServerWithoutAPeerInAResumedRun cuts a run right after its first removal and starts the next
// at 105 s: the old server runs with no peer, and the run has not seen the leader add it. The run plans twice, without
// the lock and under it, each time over an API of its own whose first read goes to the removed server and takes 5 s,
// so its first observation comes at 115 s. It holds the stop from that observation, sees the re-add of 120 s, removes
// the server and stops it. No read of the loop goes to the removed server.
func TestSingleHoldStartsAtAServerWithoutAPeerInAResumedRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)

		s.resumeAfterFirstRemoval(t, 105*time.Second)

		s.wantHaltIn(t, 2*time.Minute, 2*time.Minute+10*time.Second)
		s.wantRemovedCalls(t, 2)
		s.wantFinished(t, workers)
	})
}

// TestSingleHoldRemovesANonvoterOfUnknownAgeAndWaitsForTheNextReAdd cuts a run right after its first removal and starts
// the next at 162 s, where the leader added the server 42 s ago and autopilot promotes a server 50 s after it. The
// run's first observation shows a nonvoter that it did not see appear: it removes it at once and sends no stop until
// it has seen the re-add of 180 s.
func TestSingleHoldRemovesANonvoterOfUnknownAgeAndWaitsForTheNextReAdd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 50*time.Second)
		workers := workerIDs(s.f)

		s.resumeAfterFirstRemoval(t, 162*time.Second)

		s.wantHaltIn(t, 3*time.Minute, 3*time.Minute+10*time.Second)
		s.wantSecondRemovalAt(t, 162*time.Second)
		s.wantFinished(t, workers)
	})
}

// TestSingleHoldRemovesAPromotedServerAfterTheWindowAndHoldsTheStop cuts a run right after its first removal. The
// leader adds the server at 120 s and it votes at 130 s. The next run starts at 175 s with two voters that run: its
// first write is the removal of the voter, at its first observation, since the window over the other voter is long
// over. It holds the stop, sees the re-add of 180 s, removes the server again and stops it.
func TestSingleHoldRemovesAPromotedServerAfterTheWindowAndHoldsTheStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)

		s.resumeAfterFirstRemoval(t, 175*time.Second)

		s.wantHaltIn(t, 3*time.Minute, 3*time.Minute+10*time.Second)
		s.wantSecondRemovalAt(t, 175*time.Second)
		s.wantFinished(t, workers)
	})
}

// resumeAfterFirstRemoval cuts a roll right after its first removal of the old server's peer and starts the next run
// resume after the transfer, which finishes the roll.
func (s *singleWorld) resumeAfterFirstRemoval(t *testing.T, resume time.Duration) {
	t.Helper()
	s.cutRun(t, "RemovePeer", 1, true)
	sleepUntil(s.transferred(), resume)

	if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
		t.Fatalf("the run after the cut failed: %v", err)
	}
}

// wantSecondRemovalAt fails the test unless the roll removed the peer three times, the second at the time after the
// transfer.
func (s *singleWorld) wantSecondRemovalAt(t *testing.T, at time.Duration) {
	t.Helper()
	if removed := s.since(s.inv.reads("RemovePeer")); len(removed) != 3 || removed[1] != at {
		t.Errorf("the roll removed the peer at %v, want three times, the second at %v", removed, at)
	}
}

// forceMemberOutAfterRemoval makes svc force the member of the server called name out of the gossip pool, outside the
// roll, when the roll has removed the server's peer for the first time.
func forceMemberOutAfterRemoval(t *testing.T, svc *app.Service, name string) {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	var done bool
	inner := svc.OnProgress
	svc.OnProgress = func(p app.Progress) {
		if inner != nil {
			inner(p)
		}
		if done || progressLine(p) != "nomad done remove-peer "+name {
			return
		}
		done = true
		if err := api.ForceLeave(t.Context(), name+".global"); err != nil {
			t.Errorf("ForceLeave: %v", err)
		}
	}
}

// TestSingleHoldSendsTheStopAfterAMinuteWhenTheMemberIsNotAlive forces the old server's member out of the gossip pool
// right after the first removal, with the machine running: no reconcile adds a member that is not alive. The run holds
// the stop for 65 s of its own observations and sends it then, at the first poll after them.
func TestSingleHoldSendsTheStopAfterAMinuteWhenTheMemberIsNotAlive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		lines := stampProgress(s.svc)
		forceMemberOutAfterRemoval(t, s.svc, "prod-servers-0")

		_, err := applyRoll(s.svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		removed := s.since(s.inv.reads("RemovePeer"))
		if len(removed) != 1 {
			t.Fatalf("the roll removed the peer at %v, want once", removed)
		}
		halt := s.since(s.haltTimes())
		if len(halt) != 1 || halt[0]-removed[0] < 65*time.Second || halt[0]-removed[0] >= 65*time.Second+rollPollSlack {
			t.Errorf("the roll removed the peer at %v and halted at %v, want the halt 65 s after, within a poll", removed, halt)
		}
		end := whenLine(*lines, "nomad done reconcile prod-servers-0").Sub(s.transferred())
		if end != halt[0] {
			t.Errorf("the wait for the reconcile ended at %v, want %v, when the roll sent the stop", end, halt[0])
		}
		s.wantFinished(t, workers)
	})
}

// countContaining returns how many of the texts contain part.
func countContaining(texts []string, part string) int {
	n := 0
	for _, text := range texts {
		if strings.Contains(text, part) {
			n++
		}
	}
	return n
}

// rollPollSlack is how much later than a limit the loop can notice that it passed: a poll, and the observation.
const rollPollSlack = 3 * time.Second

// TestSingleHoldDoesNotCountObservationsThatAreFarApart holds the Health read of the hold's observation at 114 s until
// 161 s, across the reconcile of 120 s, with a promotion 50 s after a re-add. The observation after it, at 163 s,
// shows a nonvoter 49 s after the last one that showed the server absent: the run does not take it for a re-add that it
// saw, removes the nonvoter, and sends the stop only after the reconcile of 180 s.
func TestSingleHoldDoesNotCountObservationsThatAreFarApart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 50*time.Second)
		workers := workerIDs(s.f)
		s.hooks(nil, s.holdRead("Health", 114*time.Second, 161*time.Second))

		_, err := applyRoll(s.svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		s.wantHaltIn(t, 3*time.Minute, 3*time.Minute+10*time.Second)
		s.wantSecondRemovalAt(t, 163*time.Second)
		s.wantFinished(t, workers)
	})
}

// TestSingleHoldGoesOnWhenTheStopCallGetsNoAnswerAndTheCloudHaltsLater makes the stop call get no answer: the hook
// waits for the call's 10 s, carries the halt out at 178 s, and returns the provider's no-answer error. The machine
// runs until 187 s, the cloud lists it as running for three more reads, and the leader adds it at 180 s. The run
// stays: at its first observation after 180 s it removes the server again, then lists the machine as stopped, forces
// the member out and deletes it, with one stop call, the failed stop and one warning. It checks the quorum before it
// looks at the run's error.
func TestSingleHoldGoesOnWhenTheStopCallGetsNoAnswerAndTheCloudHaltsLater(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		s.f.SetHaltReads(t, 3)
		s.hooks(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name != "HaltInstance" {
				return next(ctx)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Minute):
				t.Errorf("the stop call still waits for its answer after a minute, want it ended after 10 s")
			}
			go func() {
				sleepUntil(s.transferred(), 178*time.Second)
				_ = next(context.WithoutCancel(ctx))
			}()
			return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
		}, nil)
		var warnings []string
		s.svc.OnWarning = func(w string) { warnings = append(warnings, w) }
		lines := recordProgress(s.svc)

		plan, err := applyRoll(s.svc, app.RollOptions{})

		s.inv.wantQuorumFor(quorumSpan)
		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		if got := len(s.haltTimes()); got != 1 {
			t.Errorf("the roll sent %d HaltInstance calls, want 1", got)
		}
		noAnswer := vultr.NewNoAnswerError("HaltInstance", s.old[0].id, errLost)
		failed := fmt.Sprintf("node failed stop prod-servers-0: stop node prod-servers-0 (%s): %v", s.old[0].id, noAnswer)
		if got := count(*lines, failed); got != 1 {
			t.Errorf("the progress has %q %d times, want once:\n%s", failed, got, strings.Join(*lines, "\n"))
		}
		if got := countContaining(warnings, "may still be carried out"); got != 1 {
			t.Errorf("the warnings are %q, want one that says the stop may still be carried out", warnings)
		}
		removed := s.since(s.inv.reads("RemovePeer"))
		if len(removed) != 3 || removed[2] != 3*time.Minute {
			t.Errorf("the roll removed the peer at %v, want three times, the third at 3m0s, at the first observation "+
				"after the reconcile", removed)
		}
		if got := count(*lines, "nomad started reconcile prod-servers-0"); got != 1 {
			t.Errorf("the roll waited for a reconcile %d times, want once, before the stop", got)
		}
		s.wantFinished(t, workers)
	})
}

// TestSingleHoldWaitsForALateReconcileWhileTheMemberIsAlive makes the leader's first reconcile after the transfer come
// at 140 s. The first removal comes at 70 s and 65 s of absence are over at about 136 s, but the member reads alive,
// so the run sends nothing then. It sees the re-add of 140 s, removes the server and stops it.
func TestSingleHoldWaitsForALateReconcileWhileTheMemberIsAlive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, 140*time.Second, 10*time.Second)
		workers := workerIDs(s.f)

		_, err := applyRoll(s.svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		s.wantHaltIn(t, 140*time.Second, 150*time.Second)
		s.wantFinished(t, workers)
	})
}
