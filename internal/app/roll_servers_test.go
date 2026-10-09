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

// serversExtraYAML is a line of the test cluster's spec that changes the spec hash of the server group alone.
const serversExtraYAML = "  nomad:\n    extraConfig:\n      server: 'raft_multiplier = 3'\n"

// serversWorld is the test cluster after an update, whose server group's spec then changed and was applied by a
// second update, so that its three servers are outdated. setup sets the world before the first update.
func serversWorld(t *testing.T, setup func(*nomadWorld)) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t)
	if setup != nil {
		setup(w)
	}
	mustUpdate(t, svc)
	mustReplace(t, svc, keyedClusterYAML+serversExtraYAML)
	mustUpdate(t, svc)
	return svc, f, w
}

// rollUntil runs the loop as roll does, and ends the run once svc has reported the progress line stop.
func rollUntil(svc *app.Service, stop string) (app.RollCounts, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := svc.OnProgress
	svc.OnProgress = func(p app.Progress) {
		if inner != nil {
			inner(p)
		}
		if progressLine(p) == stop {
			cancel()
		}
	}
	return app.RunRoll(ctx, svc, "prod", app.RollOptions{})
}

// wantInterrupted fails the test unless err says that the context ended.
func wantInterrupted(t *testing.T, err error) {
	t.Helper()
	if err == nil || err.Error() != "interrupted" || !errors.Is(app.Stands(err), context.Canceled) {
		t.Errorf("the roll ended with %v, want an interruption", err)
	}
}

// serverSteps are the progress lines of one server that a roll replaces: the new server's create, vote and scrub, the
// window, and the old server's stop, wait, removal, forced leave, wait and delete.
func serverSteps(created, victim string) []string {
	return []string{
		"node started create " + created, "node done create " + created,
		"nomad started vote " + created, "nomad done vote " + created,
		"node started scrub " + created, "node done scrub " + created,
		"nomad started stable", "nomad done stable",
		"node started stop " + victim, "node done stop " + victim,
		"nomad started server-down " + victim, "nomad done server-down " + victim,
		"nomad started remove-peer " + victim, "nomad done remove-peer " + victim,
		"nomad started force-leave " + victim + ".global", "nomad done force-leave " + victim + ".global",
		"nomad started healthy", "nomad done healthy",
		"node started delete " + victim, "node done delete " + victim,
	}
}

// peerNames returns the names of the servers in the Raft configuration, sorted.
func peerNames(t *testing.T, svc *app.Service) []string {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	peers, err := api.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	var names []string
	for _, p := range peers {
		names = append(names, strings.TrimSuffix(p.Name, ".global"))
	}
	slices.Sort(names)
	return names
}

// TestRollLoopReplacesAServerWithOneMoreFirst carries out each step of the removal of the first outdated server, in
// order: a new server with the seed of the old ones and its group's hash that votes and is scrubbed, the window, the
// stop, the waits, the removal from the Raft configuration and the gossip pool, and the delete. The cloud halts and
// deletes only that server.
func TestRollLoopReplacesAServerWithOneMoreFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		old := instanceNamed(t, f, "prod-servers-1")
		oldHash := tagOf(instanceTags(t, f, old), cloud.LabelSpecHash)
		creates := countCalls(f, "CreateInstance")
		lines := recordProgress(svc)

		counts, err := rollUntil(svc, "node done delete prod-servers-1")

		wantInterrupted(t, err)
		if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); counts != want {
			t.Errorf("the roll did %+v, want %+v", counts, want)
		}
		wantLines(t, *lines, serverSteps("prod-servers-3", "prod-servers-1"))
		if got := countCalls(f, "CreateInstance") - creates; got != 1 {
			t.Errorf("CreateInstance was called %d times, want once", got)
		}
		halted, deleted := callsOf(f, "HaltInstance"), callsOf(f, "DeleteInstance")
		if !slices.Equal(halted, []string{old}) || !slices.Equal(deleted, []string{old}) {
			t.Errorf("the roll halted %v and deleted %v, want only %s", halted, deleted, old)
		}
		wantSeed := []string{"10.64.0.3", "10.64.0.4", "10.64.0.5"}
		if got := seedOf(configOf(t, f, "prod-servers-3")); !slices.Equal(got, wantSeed) {
			t.Errorf("the seed of the new server is %v, want the three old servers", got)
		}
		added := instanceTags(t, f, instanceNamed(t, f, "prod-servers-3"))
		if hash := tagOf(added, cloud.LabelSpecHash); hash == "" || hash == oldHash {
			t.Errorf("the new server has the spec hash %q, want the new hash of its group, not %q", hash, oldHash)
		}
		wantJoined(t, f, "prod-servers-0", "prod-servers-2", "prod-servers-3", "prod-workers-0", "prod-workers-1")
		if got := peerNames(t, svc); !slices.Equal(got, []string{"prod-servers-0", "prod-servers-2", "prod-servers-3"}) {
			t.Errorf("the Raft configuration lists %v, want the two old servers that stay and the new one", got)
		}
	})
}

// instanceTags returns the tags of the instance id of f.
func instanceTags(t *testing.T, f *vultrfake.Fake, id string) []string {
	t.Helper()
	for _, in := range f.Instances() {
		if in.ID == id {
			return in.Tags
		}
	}
	t.Fatalf("the fake has no instance %s", id)
	return nil
}

// TestRollLoopReportsWhatEachServerStepWorksOn tells the node of each Nomad step of a server's removal, with the end of
// the window that the stable step waits for.
func TestRollLoopReportsWhatEachServerStepWorksOn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		var events []app.NomadEvent
		var until, joined time.Time
		svc.OnProgress = func(p app.Progress) {
			if p.Nomad == nil {
				return
			}
			if p.Step == app.NodeDone {
				events = append(events, *p.Nomad)
			}
			if p.Step == app.NodeStarted && p.Nomad.Action == app.NomadStable {
				until, joined = p.Nomad.Until, stableSince(t, svc, "prod-servers-3")
			}
		}

		if _, err := rollUntil(svc, "node done delete prod-servers-1"); err == nil {
			t.Fatal("the roll ended without an interruption")
		}

		want := []app.NomadEvent{
			{Action: app.NomadVote, Node: "prod-servers-3"},
			{Action: app.NomadStable, Until: until},
			{Action: app.NomadServerDown, Node: "prod-servers-1"},
			{Action: app.NomadRemovePeer, Node: "prod-servers-1"},
			{Action: app.NomadForceLeave, Node: "prod-servers-1.global"},
			{Action: app.NomadHealthy, Voters: 3},
		}
		if diff := cmp.Diff(want, events); diff != "" {
			t.Errorf("the events of the first server (-want +got):\n%s", diff)
		}
		if joined.IsZero() || until.Sub(joined) != 70*time.Second {
			t.Errorf("the window ends at %v, want 70 s after the new server joined at %v", until, joined)
		}
	})
}

// stableSince returns the time that autopilot's report gives as the stable time of the server called name.
func stableSince(t *testing.T, svc *app.Service, name string) time.Time {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	health, err := api.Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, s := range health.Servers {
		if s.Name == name+".global" {
			return s.StableSince
		}
	}
	t.Fatalf("autopilot's report has no server %s", name)
	return time.Time{}
}

// TestRollLoopGivesUpWaitingForANewServerToVote ends the run when the new server does not vote within ten minutes,
// says what the Raft configuration shows of it, and stops no server.
func TestRollLoopGivesUpWaitingForANewServerToVote(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetVoteAfter(time.Hour) })
		lines := recordProgress(svc)
		halts := countCalls(f, "HaltInstance")
		start := time.Now()

		_, err := rollBefore(svc, loopNodeTimeout)

		want := "node prod-servers-3 did not vote within 10m0s (its server does not vote yet); " + rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(start), loopNodeTimeout)
		wantLines(t, (*lines)[len(*lines)-2:],
			[]string{"nomad started vote prod-servers-3", "nomad failed vote prod-servers-3: " + want})
		if got := countCalls(f, "HaltInstance") - halts; got != 0 {
			t.Errorf("HaltInstance was called %d times, want none", got)
		}
	})
}

// TestRollLoopScrubsAServerOnceItVotesAndAutopilotCountsItHealthy waits for a new server that votes, and scrubs it
// only once autopilot's report counts it healthy.
func TestRollLoopScrubsAServerOnceItVotesAndAutopilotCountsItHealthy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		start := time.Now()
		w.ChangeServer("prod-servers-3", func(s *nomadops.ServerHealth) {
			s.Healthy = s.Healthy && time.Since(start) >= 2*time.Minute
		})
		var scrubbed time.Time
		svc.OnProgress = func(p app.Progress) {
			if progressLine(p) == "node started scrub prod-servers-3" {
				scrubbed = time.Now()
			}
		}

		_, err := rollUntil(svc, "node done scrub prod-servers-3")

		wantInterrupted(t, err)
		if got := scrubbed.Sub(start); got < 2*time.Minute || got > 2*time.Minute+loopSlack {
			t.Errorf("the server was scrubbed %v after the roll began, want once autopilot counted it healthy at 2m0s", got)
		}
	})
}

// TestRollLoopGivesUpWaitingForTheWindow ends the run when the servers keep changing for five minutes, so that the
// window never ends, and says when the window of the last poll ends.
func TestRollLoopGivesUpWaitingForTheWindow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		w.ChangeServer("prod-servers-3", func(s *nomadops.ServerHealth) { s.StableSince = time.Now() })
		lines := recordProgress(svc)
		halts := countCalls(f, "HaltInstance")
		var began time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if began.IsZero() && p.Nomad != nil && p.Nomad.Action == app.NomadStable {
				began = time.Now()
			}
		}

		_, err := rollBefore(svc, 20*time.Minute)

		const prefix = "the servers did not become stable within 5m0s (the window ends at "
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), "); "+rollAgain) {
			t.Fatalf("the roll ended with %v, want the window that never ended", err)
		}
		wantWithin(t, time.Since(began), 5*time.Minute)
		got := (*lines)[len(*lines)-2:]
		wantLines(t, []string{got[0], strings.TrimSuffix(got[1], err.Error()) + "<error>"},
			[]string{"nomad started stable", "nomad failed stable: <error>"})
		if got := countCalls(f, "HaltInstance") - halts; got != 0 {
			t.Errorf("HaltInstance was called %d times, want none", got)
		}
	})
}

// TestRollLoopGivesUpWaitingForAStoppedServerToGoDown ends the run when autopilot counts the stopped server a healthy
// voter for five minutes, and says so.
func TestRollLoopGivesUpWaitingForAStoppedServerToGoDown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetFailAfter(time.Hour) })
		lines := recordProgress(svc)
		var began time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if p.Nomad != nil && p.Nomad.Action == app.NomadServerDown && began.IsZero() {
				began = time.Now()
			}
		}

		_, err := rollBefore(svc, 20*time.Minute)

		want := "autopilot did not stop counting prod-servers-1 as a healthy voter within 5m0s " +
			"(autopilot counts it a healthy voter); " + rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(began), 5*time.Minute)
		wantLines(t, (*lines)[len(*lines)-2:],
			[]string{"nomad started server-down prod-servers-1", "nomad failed server-down prod-servers-1: " + want})
	})
}

// TestRollLoopGivesUpWaitingForTheServersToBeHealthyAfterTheRemoval ends the run when the servers that stay are not
// healthy ten minutes after the forced leave.
func TestRollLoopGivesUpWaitingForTheServersToBeHealthyAfterTheRemoval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "ForceLeave" {
				w.Unhealthy()
			}
			return err
		})
		lines := recordProgress(svc)
		var began time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if p.Nomad != nil && p.Nomad.Action == app.NomadHealthy && began.IsZero() {
				began = time.Now()
			}
		}

		_, err := rollBefore(svc, loopNodeTimeout)

		want := "the servers did not become healthy and vote within 10m0s " +
			"(autopilot reports 3 voters and the servers not healthy); " + rollAgain
		wantError(t, err, want)
		wantWithin(t, time.Since(began), loopNodeTimeout)
		wantLines(t, (*lines)[len(*lines)-2:], []string{"nomad started healthy", "nomad failed healthy: " + want})
	})
}

// TestRollLoopMovesTheLeadershipBeforeItStopsALeader gives the leadership to the first new server before it stops
// the old leader, with a window before and after the transfer, and names both servers in the event.
func TestRollLoopMovesTheLeadershipBeforeItStopsALeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		lines := recordProgress(svc)
		var events []app.NomadEvent
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			inner(p)
			if p.Nomad != nil && p.Nomad.Action == app.NomadTransfer && p.Step == app.NodeDone {
				events = append(events, *p.Nomad)
			}
		}

		_, err := rollUntil(svc, "node done stop prod-servers-0")

		wantInterrupted(t, err)
		want := []app.NomadEvent{{Action: app.NomadTransfer, Node: "prod-servers-0", Leader: "prod-servers-3"}}
		if !slices.Equal(events, want) {
			t.Errorf("the transfers are %+v, want %+v", events, want)
		}
		wantLines(t, (*lines)[len(*lines)-9:], []string{
			"node done scrub prod-servers-5",
			"nomad started stable", "nomad done stable", "nomad started transfer prod-servers-0",
			"nomad done transfer prod-servers-0", "nomad started stable", "nomad done stable",
			"node started stop prod-servers-0", "node done stop prod-servers-0",
		})
	})
}

// TestRollLoopTriesTheTransferAgainWhenItsTargetLeft goes on after a transfer that finds its target gone: the next
// try follows a poll and a new observation.
func TestRollLoopTriesTheTransferAgainWhenItsTargetLeft(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		transfers := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "TransferLeadership" {
				return next(ctx)
			}
			transfers++
			if transfers == 1 {
				return fmt.Errorf("transfer: %w", nomadops.ErrGone)
			}
			return next(ctx)
		})
		lines := recordProgress(svc)

		_, err := rollUntil(svc, "node done stop prod-servers-0")

		wantInterrupted(t, err)
		if transfers != 2 {
			t.Errorf("TransferLeadership was sent %d times, want twice: the first found its target gone", transfers)
		}
		if got := count(*lines, "nomad failed transfer prod-servers-0: transfer: "+nomadops.ErrGone.Error()); got != 1 {
			t.Errorf("the progress has the failed transfer %d times, want once:\n%s", got, strings.Join(*lines, "\n"))
		}
	})
}

// haltAtStable halts the machine called name, from outside the run, 40 s after the nth start of the wait for the
// window: autopilot still counts the halted server healthy when the window ends, and the list that the run took before
// the wait shows it running. Once it has, the returned slice records each halt of the run that finds another server of
// the cluster halted beside.
func haltAtStable(t *testing.T, svc *app.Service, f *vultrfake.Fake, name string, nth int) *[]string {
	t.Helper()
	var beside []string
	starts := 0
	inner := svc.OnProgress
	svc.OnProgress = func(p app.Progress) {
		if inner != nil {
			inner(p)
		}
		if p.Step != app.NodeStarted || p.Nomad == nil || p.Nomad.Action != app.NomadStable {
			return
		}
		if starts++; starts != nth {
			return
		}
		id := instanceNamed(t, f, name)
		time.AfterFunc(40*time.Second, func() {
			if err := f.HaltInstance(context.Background(), id); err != nil {
				t.Errorf("HaltInstance: %v", err)
			}
			f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
				if c.Name == "HaltInstance" {
					for _, in := range f.Instances() {
						if in.ID != c.Arg && f.Halted(in.ID) {
							beside = append(beside, fmt.Sprintf("%s halted beside %s", c.Arg, in.Hostname))
						}
					}
				}
				return next(ctx)
			})
		})
	}
	return &beside
}

// TestRollLoopListsTheMachinesAgainBeforeAStop does not stop the planned server when another server was halted while
// the run waited for the window: the list that it takes before the stop shows the halted server, which is removed
// first, and no halt of the run finds another server halted.
func TestRollLoopListsTheMachinesAgainBeforeAStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		lines := recordProgress(svc)
		beside := haltAtStable(t, svc, f, "prod-servers-2", 1)

		_, err := rollUntil(svc, "node done delete prod-servers-2")

		wantInterrupted(t, err)
		if len(*beside) > 0 {
			t.Errorf("the run halted a server while another was halted: %q", *beside)
		}
		stops := count(*lines, "node started stop prod-servers-1") + count(*lines, "node started stop prod-servers-2")
		if stops != 0 {
			t.Errorf("the run stopped a server %d times, want none: the halted server was removed first:\n%s", stops,
				strings.Join(*lines, "\n"))
		}
		wantInOrder(t, *lines, "nomad started server-down prod-servers-2", "node done delete prod-servers-2")
	})
}

// TestRollLoopRefusesAfterTheListShowsANewServerHalted ends the run, without a stop, when the new server was halted
// while the run waited for the window: the list that it takes before the stop shows it.
func TestRollLoopRefusesAfterTheListShowsANewServerHalted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		lines := recordProgress(svc)
		beside := haltAtStable(t, svc, f, "prod-servers-3", 1)

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "node group servers: node prod-servers-3 is not running; "+
			"run tent update cluster or tent validate cluster first")
		stops := 0
		for _, old := range []string{"prod-servers-0", "prod-servers-1", "prod-servers-2"} {
			stops += count(*lines, "node started stop "+old)
		}
		if len(*beside) > 0 || stops != 0 {
			t.Errorf("the run stopped a server, want none:\n%s", strings.Join(*lines, "\n"))
		}
	})
}

// TestRollLoopListsTheMachinesAgainBeforeATransfer sends no transfer of the leadership when the new server was halted
// while the run waited for the window: the list that it takes before the transfer shows it.
func TestRollLoopListsTheMachinesAgainBeforeATransfer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		haltAtStable(t, svc, f, "prod-servers-5", 3)

		_, err := roll(svc, app.RollOptions{})

		wantError(t, err, "node group servers: node prod-servers-5 is not running; "+
			"run tent update cluster or tent validate cluster first")
		for _, c := range w.Log() {
			if c.Name == "TransferLeadership" {
				t.Errorf("the run sent a transfer of the leadership")
			}
		}
	})
}

// TestRollLoopRefusesCombinedGroups ends the run at the first step of a combined group, with the advice about
// --nodegroups when the specs have a client group, and writes nothing to the cloud or to Nomad.
func TestRollLoopRefusesCombinedGroups(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, combinedYAML, workersYAML)
		mustUpdate(t, svc)
		mustReplace(t, svc, keyedClusterYAML+serversExtraYAML)
		mustUpdate(t, svc)
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		counts, err := roll(svc, app.RollOptions{})

		wantError(t, err, "node group all: tent cannot roll combined groups yet; select client groups with --nodegroups")
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		if counts != (app.RollCounts{}) {
			t.Errorf("the roll did %+v, want nothing", counts)
		}
	})
}

// TestRollLoopEndsWhenTheCloudRefusesAStop ends the run with the cloud's error, names the machine, and takes no step
// that follows the stop.
func TestRollLoopEndsWhenTheCloudRefusesAStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		errRefused := errors.New("the cloud refused the halt")
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "HaltInstance" {
				return errRefused
			}
			return next(ctx)
		})
		lines := recordProgress(svc)
		id := instanceNamed(t, f, "prod-servers-1")

		counts, err := roll(svc, app.RollOptions{})

		if !errors.Is(err, errRefused) || !strings.HasPrefix(err.Error(), "stop node prod-servers-1 (ID "+id+"): ") {
			t.Errorf("the roll ended with %v, want the step and the cloud's error", err)
		}
		last := (*lines)[len(*lines)-2:]
		failed := "node failed stop prod-servers-1: "
		if last[0] != "node started stop prod-servers-1" || !strings.HasPrefix(last[1], failed) ||
			!strings.HasSuffix(last[1], errRefused.Error()) {
			t.Errorf("the last progress lines are %q, want the stop started and failed with the cloud's error", last)
		}
		if counts != (app.RollCounts{Created: 1}) {
			t.Errorf("the roll did %+v, want one create", counts)
		}
	})
}

// TestRollLoopCountsEachWindowOfALeadingVictimFromItsOwnStart waits for two windows of three minutes, one before and
// one after the transfer of the leadership, which together last longer than the limit of one wait: the limit counts
// from the start of each window.
func TestRollLoopCountsEachWindowOfALeadingVictimFromItsOwnStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		const hold = 3 * time.Minute
		var firstWindow, transferred time.Time
		w.ChangeServer("prod-servers-5", func(s *nomadops.ServerHealth) {
			now := time.Now()
			if !firstWindow.IsZero() && now.Before(firstWindow.Add(hold)) ||
				!transferred.IsZero() && now.Before(transferred.Add(hold)) {
				s.StableSince = now
			}
		})
		windows := 0
		svc.OnProgress = func(p app.Progress) {
			switch {
			case p.Step == app.NodeStarted && p.Nomad != nil && p.Nomad.Action == app.NomadStable:
				if windows++; windows == 3 {
					firstWindow = time.Now()
				}
			case progressLine(p) == "nomad done transfer prod-servers-0":
				transferred = time.Now()
			}
		}

		_, err := rollUntil(svc, "node done stop prod-servers-0")

		wantInterrupted(t, err)
		if got := transferred.Sub(firstWindow); got != hold {
			t.Errorf("the first window lasted %v, want %v", got, hold)
		}
		if got := time.Since(transferred); got < hold {
			t.Errorf("the second window lasted %v, want at least %v", got, hold)
		}
	})
}

// TestRollLoopCountsTheWaitForTheServersOfEachVictimFromItsOwnStart waits for the servers after the removal of each of
// two victims for six minutes, which together last longer than the limit of one wait.
func TestRollLoopCountsTheWaitForTheServersOfEachVictimFromItsOwnStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetReportLag(6 * time.Minute) })
		start := time.Now()

		_, err := rollUntil(svc, "node done delete prod-servers-2")

		wantInterrupted(t, err)
		if got := time.Since(start); got < 2*6*time.Minute {
			t.Errorf("the roll of two servers took %v, want at least the two waits of 6 minutes", got)
		}
	})
}

// TestRollLoopRemovesThePeerOfAStoppedServerThatAutopilotKeeps removes the stopped server from the Raft configuration
// by its Raft ID when autopilot's cleanup never does, and forces its member out.
func TestRollLoopRemovesThePeerOfAStoppedServerThatAutopilotKeeps(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.NoCleanup() })
		old := instanceNamed(t, f, "prod-servers-1")

		_, err := rollUntil(svc, "node done delete prod-servers-1")

		wantInterrupted(t, err)
		var removed, forced []string
		for _, c := range w.Log() {
			switch c.Name {
			case "RemovePeer":
				removed = append(removed, c.Arg)
			case "ForceLeave":
				forced = append(forced, c.Arg)
			}
		}
		if !slices.Equal(removed, []string{"r-" + old}) || !slices.Equal(forced, []string{"prod-servers-1.global"}) {
			t.Errorf("the run removed the peers %q and forced out %q, want r-%s and prod-servers-1.global",
				removed, forced, old)
		}
		if got := peerNames(t, svc); !slices.Equal(got, []string{"prod-servers-0", "prod-servers-2", "prod-servers-3"}) {
			t.Errorf("the Raft configuration lists %v, want the servers that stay and the new one", got)
		}
	})
}
