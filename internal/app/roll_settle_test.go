package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/rollout"
)

const (
	// settleLimit is how long the loop waits for a refusal to clear.
	settleLimit = time.Minute
	// settleRefusal is what the decisions say while autopilot reports the cluster unhealthy and names no server.
	settleRefusal = "node group servers: autopilot reports the servers unhealthy; " +
		"tent replaces a server only while every server is healthy"
	// versionRefusalPrefix starts what the decisions say while a node runs a version older than the pin.
	versionRefusalPrefix = "tent never moves a node to an older Nomad: the cluster is pinned to "
)

// refuseVersionFor makes the decisions refuse because the node prod-workers-1 runs an older version, and lets the
// refusal clear d later.
func refuseVersionFor(w *nomadWorld, d time.Duration) {
	w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Version = "9.9.9" })
	time.AfterFunc(d, func() { w.ChangeNode("prod-workers-1", func(*nomadops.Node) {}) })
}

// unhealthyAfterScrub makes autopilot report the servers unhealthy right after the first update of a machine, the
// scrub of the new server, and clear that d later; a d of zero or less keeps it so.
func unhealthyAfterScrub(f *vultrfake.Fake, w *nomadWorld, d time.Duration) {
	done := false
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		err := next(ctx)
		if c.Name == "UpdateInstance" && err == nil && !done {
			done = true
			w.SetUnhealthy(true)
			if d > 0 {
				time.AfterFunc(d, func() { w.SetUnhealthy(false) })
			}
		}
		return err
	})
}

// settleRun records the progress lines of a run and its settle events, and when the first settle started.
type settleRun struct {
	lines   []string
	settles []app.Progress
	began   time.Time
}

// recordSettles makes svc record its progress into the returned run.
func recordSettles(svc *app.Service) *settleRun {
	r := &settleRun{}
	svc.OnProgress = func(p app.Progress) {
		r.lines = append(r.lines, progressLine(p))
		if p.Nomad != nil && p.Nomad.Action == app.NomadSettle {
			r.settles = append(r.settles, p)
			if p.Step == app.NodeStarted && r.began.IsZero() {
				r.began = time.Now()
			}
		}
	}
	return r
}

// wantMatchesRefusal fails the test unless err is a refusal of the decisions that says want.
func wantMatchesRefusal(t *testing.T, err error, want string) {
	t.Helper()
	wantError(t, err, want)
	if !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("error %v does not match rollout.ErrRefused", err)
	}
}

// TestRollLoopEndsAtOnceAtARefusalOfTheFirstDecision ends a run whose first decision is a refusal with that refusal,
// without a settle wait, a poll or a write: nothing was done yet, and the cluster is as the operator left it.
func TestRollLoopEndsAtOnceAtARefusalOfTheFirstDecision(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		w.SetUnhealthy(true)
		run := recordSettles(svc)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		start := time.Now()

		counts, err := roll(svc, app.RollOptions{})

		wantMatchesRefusal(t, err, settleRefusal)
		if got := time.Since(start); got != 0 {
			t.Errorf("the run took %v, want it to end at the first decision", got)
		}
		if len(run.settles) > 0 || len(run.lines) > 0 {
			t.Errorf("the run reported %q, want no progress", run.lines)
		}
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		if counts != (app.RollCounts{}) {
			t.Errorf("the roll did %+v, want nothing", counts)
		}
	})
}

// TestRollLoopGoesOnWhenARefusalClearsAfterAStep waits for a refusal that comes right after the new server was
// created and scrubbed and lasts ten seconds, reports one settle wait that starts with the refusal and ends done, and
// goes on with the window of the stability wait.
func TestRollLoopGoesOnWhenARefusalClearsAfterAStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 10*time.Second)
		run := recordSettles(svc)

		_, err := rollUntil(svc, "nomad done stable")

		wantInterrupted(t, err)
		wantInOrder(t, run.lines, "node done scrub prod-servers-3", "nomad started settle", "nomad done settle",
			"nomad started stable", "nomad done stable")
		if len(run.settles) != 2 {
			t.Fatalf("the run reported %d settle events, want a start and an end", len(run.settles))
		}
		started := run.settles[0].Nomad
		if started.Reason != settleRefusal || started.Deadline != settleLimit {
			t.Errorf("the settle wait started with the reason %q and the deadline %v, want %q and %v",
				started.Reason, started.Deadline, settleRefusal, settleLimit)
		}
		if run.settles[1].Step != app.NodeDone || run.settles[1].Err != nil {
			t.Errorf("the settle wait ended with %+v, want done", run.settles[1])
		}
	})
}

// TestRollLoopEndsWithTheRefusalWhenItLastsAMinute ends the run a minute after the first refusal of the series with
// that refusal, and reports the settle wait as failed with it.
func TestRollLoopEndsWithTheRefusalWhenItLastsAMinute(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 0)
		run := recordSettles(svc)

		_, err := rollBefore(svc, settleLimit)

		wantMatchesRefusal(t, err, settleRefusal)
		if run.began.IsZero() {
			t.Fatal("the run did not wait for the refusal to clear")
		}
		wantWithin(t, time.Since(run.began), settleLimit)
		wantLines(t, run.lines[len(run.lines)-2:],
			[]string{"nomad started settle", "nomad failed settle: " + settleRefusal})
		if got := run.settles[len(run.settles)-1].Err; !errors.Is(got, rollout.ErrRefused) {
			t.Errorf("the settle wait failed with %v, want the refusal", got)
		}
	})
}

// TestRollLoopFailsTheSettleWaitWhenTheRunIsInterrupted reports the settle wait as failed with the context's error
// when the run ends while it waits.
func TestRollLoopFailsTheSettleWaitWhenTheRunIsInterrupted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 0)
		run := recordSettles(svc)

		_, err := rollUntil(svc, "nomad started settle")

		wantInterrupted(t, err)
		wantLines(t, run.lines[len(run.lines)-2:],
			[]string{"nomad started settle", "nomad failed settle: context canceled"})
	})
}

// TestRollLoopListsTheMachinesAtEveryPollOfASettle lists the machines at each observation of a settle wait, as it
// does at any observation that follows a step, and reads Nomad as often.
func TestRollLoopListsTheMachinesAtEveryPollOfASettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 10*time.Second)
		var cloudAt, nomadAt []int
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			if inner != nil {
				inner(p)
			}
			if p.Nomad != nil && p.Nomad.Action == app.NomadSettle {
				cloudAt, nomadAt = append(cloudAt, len(f.Calls())), append(nomadAt, len(w.Log()))
			}
		}

		_, err := rollUntil(svc, "nomad done settle")

		wantInterrupted(t, err)
		if len(cloudAt) != 2 {
			t.Fatalf("the run reported %d settle events, want a start and an end", len(cloudAt))
		}
		lists := 0
		for _, c := range f.Calls()[cloudAt[0]:cloudAt[1]] {
			if c.Name == "ListInstances" {
				lists++
			}
		}
		reads := 0
		for _, name := range nomadCallNames(w, nomadAt[0])[:nomadAt[1]-nomadAt[0]] {
			if name == "Peers" {
				reads++
			}
		}
		if lists != reads || lists < 3 {
			t.Errorf("the settle wait listed the machines %d times and read Nomad %d times, want as often, at least 3",
				lists, reads)
		}
	})
}

// TestRollLoopLogsEachObservationOfASettleWithTheRefusal logs one line for each observation of a settle wait, with the
// refusal as the next step.
func TestRollLoopLogsEachObservationOfASettleWithTheRefusal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 10*time.Second)
		var buf bytes.Buffer
		svc.Log = debugLog(&buf)
		calls := len(w.Log())

		_, err := rollUntil(svc, "nomad done stable")

		wantInterrupted(t, err)
		records := logRecords(t, &buf)
		if want := peerReads(w, calls); len(records) != want {
			t.Errorf("the roll logged %d lines and observed %d times, want one line for each", len(records), want)
		}
		refused := 0
		for _, rec := range records {
			if rec["next"] == settleRefusal {
				refused++
			}
		}
		if refused < 3 {
			t.Errorf("%d lines have the refusal as the next step, want one for each observation of the wait", refused)
		}
	})
}

// TestRollLoopWaitsForARefusalAfterAWriteThatNoServerAnswered waits for a refusal that comes after the run's first
// write, a create whose intro token no server answered, and finishes the roll once the refusal clears.
func TestRollLoopWaitsForARefusalAfterAWriteThatNoServerAnswered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		tokens := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "IntroToken" {
				return next(ctx)
			}
			if tokens++; tokens == 1 {
				refuseVersionFor(w, 10*time.Second)
			}
			if tokens <= 3 { // the first try asks each of the three servers
				return notReadyError("intro token")
			}
			return next(ctx)
		})
		run := recordSettles(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		wantInOrder(t, run.lines, "nomad started settle", "nomad done settle")
	})
}

// TestRollLoopKeepsTheSeriesOfTriesOfAStepAcrossASettle tries a step three times and ends the run, though a settle wait
// came between the first and the second try: the wait is no try, and it does not start the count again.
func TestRollLoopKeepsTheSeriesOfTriesOfAStepAcrossASettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		var marks []time.Time
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "MarkIneligible" {
				return next(ctx)
			}
			marks = append(marks, time.Now())
			if len(marks) == 1 {
				refuseVersionFor(w, 10*time.Second)
				return fmt.Errorf("no such node: %w", nomadops.ErrGone)
			}
			return nil
		})
		run := recordSettles(svc)

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "mark node prod-workers-0 ineligible had no effect after 3 tries")
		if len(marks) != 3 {
			t.Errorf("MarkIneligible was sent %d times, want 3", len(marks))
		}
		if len(marks) > 1 && marks[1].Sub(marks[0]) < 10*time.Second {
			t.Errorf("the second try came %v after the first, want it after the settle wait of ten seconds",
				marks[1].Sub(marks[0]))
		}
		wantInOrder(t, run.lines, "nomad started settle", "nomad done settle")
	})
}

// TestRollLoopKeepsTheDeadlineOfAWaitThatIsUnderWayThroughASettle gives up on a node that never registers ten minutes
// after the wait began though a settle wait came meanwhile, and reports the wait for the node once.
func TestRollLoopKeepsTheDeadlineOfAWaitThatIsUnderWayThroughASettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		created := false
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "CreateInstance" && err == nil && !created {
				created = true
				all := f.Instances()
				w.WithholdInstance(all[len(all)-1].ID)
			}
			return err
		})
		run := recordSettles(svc)
		var startedAt time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if progressLine(p) == "nomad started register prod-workers-2" {
				startedAt = time.Now()
				time.AfterFunc(time.Minute, func() { refuseVersionFor(w, 30*time.Second) })
			}
		}

		_, err := roll(svc, app.RollOptions{})

		const want = "node prod-workers-2 did not join within 10m0s ("
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("error = %v\nwant it to start with %s", err, want)
		}
		wantWithin(t, time.Since(startedAt), loopNodeTimeout)
		if n := count(run.lines, "nomad started register prod-workers-2"); n != 1 {
			t.Errorf("the wait for prod-workers-2 started %d times, want once", n)
		}
		started := slices.Index(run.lines, "nomad started settle")
		ended := slices.Index(run.lines, "nomad done settle")
		if started < 0 || ended < started {
			t.Fatalf("the run did not wait for the refusal to clear:\n%s", strings.Join(run.lines, "\n"))
		}
		if slices.Contains(run.lines[started:ended], "nomad done register prod-workers-2") {
			t.Errorf("the settle wait ended the wait for the node:\n%s", strings.Join(run.lines, "\n"))
		}
		if reason := run.settles[0].Nomad.Reason; !strings.Contains(reason, versionRefusalPrefix) {
			t.Errorf("the settle wait started with the reason %q, want the refusal of the old node", reason)
		}
	})
}

// TestRollLoopRemovesALeadingServerAfterOneSettle rolls three servers while the first follower reads unhealthy for two
// seconds after the leadership moves: the leader, the last victim, hands its leadership over, the refusal of the
// decisions that follows ends within one settle wait, and the roll finishes.
func TestRollLoopRemovesALeadingServerAfterOneSettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetTransferBlip(2 * time.Second) })
		run := recordSettles(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 3, Stopped: 3, Deleted: 3})
		if n := count(run.lines, "nomad started settle"); n != 1 {
			t.Errorf("the roll waited for a refusal to clear %d times, want once:\n%s", n, strings.Join(run.lines, "\n"))
		}
		if n := count(run.lines, "nomad done settle"); n != 1 {
			t.Errorf("the roll ended %d settle waits as done, want one", n)
		}
		if n := len(run.settles); n != 2 {
			t.Fatalf("the roll reported %d settle events, want a start and an end", n)
		}
		const reason = "node group servers: autopilot reports the servers unhealthy"
		if got := run.settles[0].Nomad.Reason; !strings.HasPrefix(got, reason) {
			t.Errorf("the settle wait started with the reason %q, want it to start with %q", got, reason)
		}
	})
}

// TestRollLoopEndsAtOnceAtARefusalAfterAWaitAlone ends a run that has only waited, and sent no write, at once at a
// refusal, without a settle wait: the run resumes in the window of a server that an earlier run created.
func TestRollLoopEndsAtOnceAtARefusalAfterAWaitAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		_, err := rollUntil(svc, "nomad started stable")
		wantInterrupted(t, err)
		svc.OnProgress = nil
		run := recordSettles(svc)
		var refusedAt time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if progressLine(p) == "nomad started stable" {
				time.AfterFunc(5*time.Second, func() {
					refusedAt = time.Now()
					w.SetUnhealthy(true)
				})
			}
		}

		_, err = roll(svc, app.RollOptions{})

		wantMatchesRefusal(t, err, settleRefusal)
		if len(run.settles) > 0 {
			t.Errorf("the run waited for the refusal to clear, want it to end at once:\n%s", strings.Join(run.lines, "\n"))
		}
		if refusedAt.IsZero() || time.Since(refusedAt) > loopPoll {
			t.Errorf("the run ended %v after the refusal began, want within a poll", time.Since(refusedAt))
		}
		wantLines(t, run.lines, []string{"nomad started stable", "nomad failed stable: " + settleRefusal})
	})
}

// TestRollLoopKeepsTheWaitForTheWindowThroughASettle gives up on a window that never ends five minutes after the wait
// for it began though a settle wait came meanwhile, and neither ends that wait nor starts it again.
func TestRollLoopKeepsTheWaitForTheWindowThroughASettle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		w.ChangeServer("prod-servers-3", func(s *nomadops.ServerHealth) { s.StableSince = time.Now() })
		run := recordSettles(svc)
		var began time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if began.IsZero() && progressLine(p) == "nomad started stable" {
				began = time.Now()
				time.AfterFunc(time.Minute, func() { w.SetUnhealthy(true) })
				time.AfterFunc(time.Minute+10*time.Second, func() { w.SetUnhealthy(false) })
			}
		}

		_, err := rollBefore(svc, 20*time.Minute)

		const prefix = "the servers did not become stable within 5m0s ("
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("the roll ended with %v, want the window that never ended", err)
		}
		wantWithin(t, time.Since(began), 5*time.Minute)
		wantInOrder(t, run.lines, "nomad started stable", "nomad started settle", "nomad done settle")
		if n := count(run.lines, "nomad done stable"); n != 0 {
			t.Errorf("the wait for the window ended as done %d times, want none:\n%s", n, strings.Join(run.lines, "\n"))
		}
	})
}

// TestRollLoopGivesEachSeriesOfRefusalsItsOwnMinute waits for a second refusal that begins more than a minute after
// the first one of the run and clears: the minute counts from the first refusal of each series.
func TestRollLoopGivesEachSeriesOfRefusalsItsOwnMinute(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := outdatedWorld(t)
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "MarkIneligible" {
				refuseVersionFor(w, 40*time.Second)
			}
			return next(ctx)
		})
		run := recordSettles(svc)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2})
		started, done := count(run.lines, "nomad started settle"), count(run.lines, "nomad done settle")
		if started != 2 || done != 2 {
			t.Errorf("the roll started %d settle waits and ended %d as done, want 2 and 2:\n%s", started, done,
				strings.Join(run.lines, "\n"))
		}
	})
}

// TestRollLoopListsTheMachinesInASettleAfterReadsThatNoServerAnswered lists the machines at every observation of a
// settle wait, also at the one that follows an observation whose reads of Nomad no server answered.
func TestRollLoopListsTheMachinesInASettleAfterReadsThatNoServerAnswered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		unhealthyAfterScrub(f, w, 20*time.Second)
		run := recordSettles(svc)
		before, failed := 0, 0
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if progressLine(p) == "nomad started settle" {
				before = countCalls(f, "ListInstances")
			}
		}
		w.SetHook(func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
			if !run.began.IsZero() && time.Since(run.began) == 2*loopPoll {
				failed++
				return notReadyError("read")
			}
			return next(ctx)
		})

		_, err := rollUntil(svc, "nomad done settle")

		wantInterrupted(t, err)
		if failed == 0 {
			t.Fatal("the hook never failed a read of Nomad, so the test checked nothing about it")
		}
		polls := int(time.Since(run.began) / loopPoll)
		if got := countCalls(f, "ListInstances") - before; got != polls || polls < 3 {
			t.Errorf("the settle wait listed the machines %d times in %d polls, want once in each of at least 3", got, polls)
		}
	})
}
