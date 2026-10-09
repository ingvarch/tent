package app_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// configAddresses returns the address of each Nomad client that the service made after the first n, in order.
func configAddresses(w *nomadWorld, n int) []string {
	var out []string
	for _, cfg := range w.Configs()[n:] {
		out = append(out, cfg.Address)
	}
	return out
}

// callsOf returns the arguments of the calls of the vultr.API method name that reached f, in order.
func callsOf(f *vultrfake.Fake, name string) []string {
	var out []string
	for _, c := range f.Calls() {
		if c.Name == name {
			out = append(out, c.Arg)
		}
	}
	return out
}

// TestRollLoopSendsOneStopAndListsUntilTheCloudShowsTheServerStopped sends the stop once when the cloud keeps listing
// the server as running for two reads after it halted it, lists the machines at every observation meanwhile, and goes
// on with the removal once the list shows it stopped.
func TestRollLoopSendsOneStopAndListsUntilTheCloudShowsTheServerStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		f.SetHaltReads(t, 2)
		old := instanceNamed(t, f, "prod-servers-1")

		counts, err := rollUntil(svc, "node done delete prod-servers-1")

		wantInterrupted(t, err)
		if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); counts != want {
			t.Errorf("the roll did %+v, want %+v", counts, want)
		}
		if got := callsOf(f, "HaltInstance"); !slices.Equal(got, []string{old}) {
			t.Errorf("the roll halted %v, want %s once", got, old)
		}
		var halt int
		var lists []int
		for i, c := range f.Calls() {
			switch {
			case c.Name == "HaltInstance":
				halt = i
			case c.Name == "ListInstances" && halt > 0:
				lists = append(lists, i)
			}
		}
		if len(lists) < 3 {
			t.Fatalf("the cloud was listed %d times after the halt, want at least 3", len(lists))
		}
		// The first two lists read the server running and the third reads it stopped. Each read of Nomad before the
		// third list follows a list.
		reads := 0
		for _, c := range w.Log() {
			if c.Name == "Peers" && c.Cloud > halt && c.Cloud <= lists[2] {
				reads++
			}
		}
		if reads != 2 {
			t.Errorf("the roll read Nomad %d times between the halt and the list that shows the server stopped, "+
				"want 2, one after each of the two lists before it", reads)
		}
	})
}

// TestRollLoopEndsWhenTheCloudNeverListsTheServerStopped ends the run two minutes after the stop, says that the cloud
// still lists the server as running, and sends the stop only once. Autopilot counts the stopped server healthy
// meanwhile, so that the decisions give the stop again at every observation.
func TestRollLoopEndsWhenTheCloudNeverListsTheServerStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetFailAfter(time.Hour) })
		f.SetHaltReads(t, 1<<20)
		old := instanceNamed(t, f, "prod-servers-1")
		lines := recordProgress(svc)
		var stopped time.Time
		inner := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			if inner != nil {
				inner(p)
			}
			if progressLine(p) == "node done stop prod-servers-1" {
				stopped = time.Now()
			}
		}

		counts, err := roll(svc, app.RollOptions{})

		want := fmt.Sprintf("the cloud still lists node prod-servers-1 (ID %s) as running 2m0s after tent stopped it; "+
			"run tent rolling-update cluster again", old)
		wantError(t, err, want)
		if stopped.IsZero() {
			t.Fatal("the roll did not report the stop")
		}
		if got := time.Since(stopped); got < 2*time.Minute || got > 2*time.Minute+loopSlack {
			t.Errorf("the run ended %v after the stop, want 2m0s", got)
		}
		if got := callsOf(f, "HaltInstance"); !slices.Equal(got, []string{old}) {
			t.Errorf("the roll halted %v, want %s once", got, old)
		}
		if want := (app.RollCounts{Created: 1, Stopped: 1}); counts != want {
			t.Errorf("the roll did %+v, want %+v", counts, want)
		}
		if last := (*lines)[len(*lines)-1]; last != "node done stop prod-servers-1" {
			t.Errorf("the last progress line is %q, want the stop that was done", last)
		}
	})
}

// TestRollLoopMakesTheAPIFollowTheServers makes a new API over the servers when the new server has joined and when
// the stop is about to be sent, and makes none otherwise. The API is over the new server when the cloud is asked to
// halt the old one, and without the old one.
func TestRollLoopMakesTheAPIFollowTheServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		before := len(w.Configs())
		old := apiAddresses(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2")
		atHalt := -1
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "HaltInstance" {
				atHalt = len(w.Configs()) - before
			}
			return next(ctx)
		})

		_, err := rollUntil(svc, "node done delete prod-servers-1")

		wantInterrupted(t, err)
		added := apiAddresses(t, f, "prod-servers-3")[0]
		// The API the run starts with, the API once the new server joined, and the API when the old one is about to be
		// stopped.
		want := slices.Concat(old, old, []string{added}, []string{old[0], old[2], added})
		if got := configAddresses(w, before); !slices.Equal(got, want) {
			t.Errorf("the clients were made for %v, want %v", got, want)
		}
		if atHalt != 3+4+3 {
			t.Errorf("%d clients were made when the cloud was asked to halt the server, want 10: the last API was ready",
				atHalt)
		}
	})
}

// TestRollLoopCallsNoServerAfterItsStop never calls the address of a server that the run stopped, also while the cloud
// lists the server as running and when a server answers last that is the one to stop: the world refuses a call to a
// halted server and notes it.
func TestRollLoopCallsNoServerAfterItsStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		f.SetHaltReads(t, 3)
		first := apiAddresses(t, f, "prod-servers-0")[0]
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Server == first {
				return fmt.Errorf("dial %s: %w", c.Server, nomadops.ErrNotReady)
			}
			return next(ctx)
		})

		_, err := rollUntil(svc, "nomad done remove-peer prod-servers-1")

		wantInterrupted(t, err)
		if got := w.HaltedCalls(); len(got) > 0 {
			t.Errorf("the roll called a server it had stopped: %v", got)
		}
	})
}

// TestRollLoopBuildsItsAPIWithoutAVictimThatTheListShowsStopped starts a run after a cut run stopped a server: the
// run begins with the API over the servers that run, and calls no halted server.
func TestRollLoopBuildsItsAPIWithoutAVictimThatTheListShowsStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		_, err := rollUntil(svc, "node done stop prod-servers-1")
		wantInterrupted(t, err)
		before := len(w.Configs())
		running := apiAddresses(t, f, "prod-servers-0", "prod-servers-2", "prod-servers-3")

		_, err = rollUntil(svc, "node done delete prod-servers-1")

		wantInterrupted(t, err)
		if got := configAddresses(w, before); len(got) < 3 || !slices.Equal(got[:3], running) {
			t.Errorf("the run started with the clients %v, want %v: the halted server is not among them", got, running)
		}
		if got := w.HaltedCalls(); len(got) > 0 {
			t.Errorf("the roll called a halted server: %v", got)
		}
	})
}

// TestRollLoopReplacesThreeServersAndEndsWithNothingLeft rolls the whole group of three servers while the cloud keeps
// listing each stopped server as running for two reads: every server is new at the end, each old server is halted and
// deleted once, and no call reaches a server after its machine halted.
func TestRollLoopReplacesThreeServersAndEndsWithNothingLeft(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, (*nomadWorld).ServersOverTime)
		f.SetHaltReads(t, 2)
		var old []string
		for _, name := range serverNames {
			old = append(old, instanceNamed(t, f, name))
		}
		oldHash := tagOf(instanceTags(t, f, old[0]), cloud.LabelSpecHash)

		counts, err := roll(svc, app.RollOptions{})

		wantRolled(t, counts, err, app.RollCounts{Created: 3, Stopped: 3, Deleted: 3})
		if got := w.HaltedCalls(); len(got) > 0 {
			t.Errorf("the roll called a server after its machine halted: %v", got)
		}
		for _, name := range []string{"HaltInstance", "DeleteInstance"} {
			if got := callsOf(f, name); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(old))) {
				t.Errorf("the roll sent %s for %v, want once for each old server %v", name, got, old)
			}
		}
		var names []string
		for _, in := range f.Instances() {
			if strings.HasPrefix(in.Hostname, "prod-servers-") {
				names = append(names, in.Hostname)
				if hash := tagOf(in.Tags, cloud.LabelSpecHash); hash == oldHash {
					t.Errorf("server %s still has the old spec hash", in.Hostname)
				}
			}
		}
		if want := []string{"prod-servers-3", "prod-servers-4", "prod-servers-5"}; !slices.Equal(names, want) {
			t.Errorf("the servers are %v, want %v", names, want)
		}
		if got := peerNames(t, svc); !slices.Equal(got, []string{"prod-servers-3", "prod-servers-4", "prod-servers-5"}) {
			t.Errorf("the Raft configuration lists %v, want the three new servers", got)
		}
		again, err := roll(svc, app.RollOptions{})
		wantRolled(t, again, err, app.RollCounts{})
	})
}
