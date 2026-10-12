package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// scalePoint names the n-th call, from 1, of a method to Vultr or to Nomad in an update.
type scalePoint struct {
	nomad  bool
	method string
	n      int
}

// atVultr names the n-th call of a method to Vultr.
func atVultr(method string, n int) scalePoint { return scalePoint{method: method, n: n} }

// atNomad names the n-th call of a method to Nomad.
func atNomad(method string, n int) scalePoint { return scalePoint{nomad: true, method: method, n: n} }

// label returns the method of the point, and its place when it is not the first call.
func (p scalePoint) label() string {
	if p.n == 1 {
		return p.method
	}
	return fmt.Sprintf("%s %d", p.method, p.n)
}

// scaleFault says what happens to the call of a scalePoint: it ends the run's context or loses the answer. It carries
// the call out with next, and gets the error that a client gets for an answer that never came.
type scaleFault func(ctx context.Context, next func(context.Context) error, lost error) error

// hooks returns the hooks of both fakes that hand the call of the point to fault and carry every other call out.
func (p scalePoint) hooks(fault scaleFault) (vultrfake.Hook, nomadHook) {
	var seen atomic.Int32
	hit := func(nomad bool, method string) bool {
		return nomad == p.nomad && method == p.method && int(seen.Add(1)) == p.n
	}
	onVultr := func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		if !hit(false, c.Name) {
			return next(ctx)
		}
		return fault(ctx, next, vultr.NewNoAnswerError(c.Name, c.Arg, errLost))
	}
	onNomad := func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		if !hit(true, c.Name) {
			return next(ctx)
		}
		return fault(ctx, next, notReadyError(c.Name))
	}
	return onVultr, onNomad
}

// scaleWatch holds the invariants of a scale-down: watch makes every call check them and go through the given hooks.
// cutBefore notes a call whose context a cut ended before the call reached the fake; it is nil when the invariants
// need not know.
type scaleWatch struct {
	watch     func(onVultr vultrfake.Hook, onNomad nomadHook)
	cutBefore func()
}

// cutUpdate applies the update of svc and ends its context at the call of p, just after the fake carried it out when
// after is set and just before it reaches the fake otherwise. It fails the test unless the run made the call, ended
// with an error that matches context.Canceled, and left no lock. It leaves only the hooks of the invariants on the
// fakes.
func (s scaleWatch) cutUpdate(t *testing.T, svc *app.Service, p scalePoint, after bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cut atomic.Bool
	s.watch(p.hooks(func(ctx context.Context, next func(context.Context) error, _ error) error {
		cut.Store(true)
		if after {
			_ = next(ctx)
		} else if s.cutBefore != nil {
			s.cutBefore()
		}
		cancel()
		return next(ctx)
	}))
	_, err := svc.Update(ctx, "prod", true)
	s.watch(nil, nil)
	if !cut.Load() {
		t.Fatalf("the run made no %s", p.label())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the run cut at %s returned %v, want an error that matches context.Canceled", p.label(), err)
	}
	wantLockFree(t, svc.Store)
}

// loseUpdate applies the update of svc and loses the answer of the call of p: the fake carries the call out, and the
// client gets no answer. It fails the test unless the run made the call, and returns the error of the run. It leaves
// only the hooks of the invariants on the fakes.
func (s scaleWatch) loseUpdate(t *testing.T, svc *app.Service, p scalePoint) error {
	t.Helper()
	var lost atomic.Bool
	s.watch(p.hooks(func(ctx context.Context, next func(context.Context) error, answerLost error) error {
		lost.Store(true)
		_ = next(ctx)
		return answerLost
	}))
	_, err := svc.Update(t.Context(), "prod", true)
	s.watch(nil, nil)
	if !lost.Load() {
		t.Fatalf("the run made no %s", p.label())
	}
	return err
}

// goneNodes returns the nodes that Nomad lists and no machine that the cloud lists has.
func goneNodes(t *testing.T, svc *app.Service, f *vultrfake.Fake) []nomadops.Node {
	t.Helper()
	var listed []string
	for _, in := range f.Instances() {
		listed = append(listed, nodeIDOf(in.ID))
	}
	return slices.DeleteFunc(nomadNodes(t, svc), func(n nomadops.Node) bool { return slices.Contains(listed, n.ID) })
}

// finishUpdate applies the update that follows a cut or a lost answer, with a fresh context. After a cut right after
// the last delete of a group, which lastDelete says, Nomad still lists the nodes of the machines that are gone as up:
// it lets them read down and applies an update once more. It fails the test when the first update leaves such a node
// after any other cut, or none after that one.
func finishUpdate(t *testing.T, svc *app.Service, f *vultrfake.Fake, lastDelete bool) {
	t.Helper()
	mustUpdate(t, svc)
	if left := len(goneNodes(t, svc, f)) > 0; left != lastDelete {
		t.Errorf("the update after the cut left a node of a machine that is gone: %v, want %v", left, lastDelete)
	}
	if lastDelete {
		time.Sleep(time.Minute)
		mustUpdate(t, svc)
	}
}

// wantSettled fails the test unless a scale-down left the cluster at rest: a plan has no change, the cluster is
// valid, Nomad lists no node of a machine that is gone, and every machine has the joined label and the stub.
func wantSettled(t *testing.T, svc *app.Service, f *vultrfake.Fake) {
	t.Helper()
	wantConverged(t, svc)
	wantValid(t, svc)
	for _, n := range goneNodes(t, svc, f) {
		t.Errorf("Nomad lists the node %s (%s) of a machine that is gone", n.Name, n.ID)
	}
	var names []string
	for _, in := range f.Instances() {
		names = append(names, in.Hostname)
	}
	wantJoined(t, f, names...)
}

// wantSettledWith fails the test unless the cluster is as wantSettled says and the nodes that Nomad lists are called
// want.
func wantSettledWith(t *testing.T, svc *app.Service, f *vultrfake.Fake, want ...string) {
	t.Helper()
	wantSettled(t, svc, f)
	if diff := cmp.Diff(want, nodeNames(t, svc), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the nodes that Nomad lists (-want +got):\n%s", diff)
	}
}

// machinesOf returns the names of the machines of the group that the cloud lists, sorted.
func machinesOf(f *vultrfake.Fake, group string) []string {
	var names []string
	for _, in := range f.Instances() {
		if strings.HasPrefix(in.Hostname, machinePrefix(group)) {
			names = append(names, in.Hostname)
		}
	}
	slices.Sort(names)
	return names
}

// eachScaleCut runs test in a parallel subtest of its own, in a synctest bubble, for each of the points, first just
// before the call and then just after it. A subtest's name gives the place and the method, such as "after Drain".
func eachScaleCut(t *testing.T, points []scalePoint, test func(t *testing.T, p scalePoint, after bool)) {
	t.Helper()
	for _, after := range []bool{false, true} {
		when := "before"
		if after {
			when = "after"
		}
		for _, p := range points {
			t.Run(when+" "+p.label(), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) { test(t, p, after) })
			})
		}
	}
}

// TestScaleCutShrinkOfClients cuts the shrink of the workers from 3 to 1 just before and just after the first mark,
// the first drain, the first delete of a machine and the first purge. The next update, with a fresh context, finishes
// it: one worker is left, ready and eligible, and the invariants of a scale-down held at every call of both runs.
func TestScaleCutShrinkOfClients(t *testing.T) {
	t.Parallel()
	points := []scalePoint{
		atNomad("MarkIneligible", 1), atNomad("Drain", 1), atVultr("DeleteInstance", 1), atNomad("Purge", 1),
	}
	eachScaleCut(t, points, func(t *testing.T, p scalePoint, after bool) {
		svc, f, w := scaleWorld(t, 1)
		inv := watchShrink(t, f, w, "workers", 1)

		scaleWatch{watch: inv.watch}.cutUpdate(t, svc, p, after)
		finishUpdate(t, svc, f, false)

		wantSettledWith(t, svc, f, "prod-workers-0")
		if diff := cmp.Diff([]string{"prod-workers-0"}, machinesOf(f, "workers")); diff != "" {
			t.Errorf("the workers (-want +got):\n%s", diff)
		}
	})
}

// TestScaleCutShrinkOfServersFromFiveToThree cuts the removal of the first server of five just before and just after
// its stop, tent's removal of its peer, its force-leave and the delete of its machine, with autopilot that never
// cleans up, so that tent's own call removes the peer. The next update finishes the shrink: three servers vote, the
// store holds the highest index, and a quorum of the voters that run held at every call.
func TestScaleCutShrinkOfServersFromFiveToThree(t *testing.T) {
	t.Parallel()
	points := []scalePoint{
		atVultr("HaltInstance", 1), atNomad("RemovePeer", 1), atNomad("ForceLeave", 1), atVultr("DeleteInstance", 1),
	}
	eachScaleCut(t, points, func(t *testing.T, p scalePoint, after bool) {
		svc, f, w := serversScaleWorld(t, 5, 3, func(w *nomadWorld) { w.NoCleanup() })
		inv := watchServers(t, f, w, serversGroup, 3).shrinking()

		scaleWatch{watch: inv.watch, cutBefore: inv.cutBeforeFake}.cutUpdate(t, svc, p, after)
		finishUpdate(t, svc, f, false)

		wantSettledWith(t, svc, f, "prod-workers-0", "prod-workers-1")
		wantRaftIs(t, svc, f, serversGroup)
		if diff := cmp.Diff([]string{"prod-servers-0", "prod-servers-1", "prod-servers-2"}, listedServers(f)); diff != "" {
			t.Errorf("the servers (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, namesPath, []byte("4\n"))
		inv.wantKept()
		inv.wantQuorumFor(quorumSpan)
	})
}

// TestScaleCutShrinkOfServersFromThreeToOne cuts the second removal of the shrink from 3 servers to 1 just before and
// just after each of its two removals of the peer and its stop. The voters that run are a quorum for the time that
// quorumSpan gives, also between the cut run and the next one; the next update finishes the shrink with one server.
func TestScaleCutShrinkOfServersFromThreeToOne(t *testing.T) {
	t.Parallel()
	points := []scalePoint{atNomad("RemovePeer", 2), atNomad("RemovePeer", 3), atVultr("HaltInstance", 2)}
	eachScaleCut(t, points, func(t *testing.T, p scalePoint, after bool) {
		svc, f, w := serversScaleWorld(t, 3, 1, reconciling)
		inv := watchServers(t, f, w, serversGroup, 1).shrinking()

		scaleWatch{watch: inv.watch, cutBefore: inv.cutBeforeFake}.cutUpdate(t, svc, p, after)
		inv.wantQuorumFor(quorumSpan)
		finishUpdate(t, svc, f, false)

		wantSettledWith(t, svc, f, "prod-workers-0", "prod-workers-1")
		wantRaftIs(t, svc, f, serversGroup)
		if diff := cmp.Diff([]string{"prod-servers-0"}, listedServers(f)); diff != "" {
			t.Errorf("the servers (-want +got):\n%s", diff)
		}
		inv.wantKept()
		inv.wantQuorumFor(quorumSpan)
	})
}

// TestScaleCutShrinkOfCombinedNodes cuts the second removal of the shrink from 3 combined nodes to 1 just before and
// just after its mark, its drain, the stop of its machine and the purge of its node. The next update finishes the
// shrink: the node that stays is the only voter and ready, and the invariants of a scale-down and of the servers held
// at every call.
func TestScaleCutShrinkOfCombinedNodes(t *testing.T) {
	t.Parallel()
	points := []scalePoint{
		atNomad("MarkIneligible", 2), atNomad("Drain", 2), atVultr("HaltInstance", 2), atNomad("Purge", 2),
	}
	eachScaleCut(t, points, func(t *testing.T, p scalePoint, after bool) {
		svc, f, w := combinedScaleWorld(t, 3, 1, reconciling)
		inv := watchCombined(t, f, w, combinedGroup, 1).shrinking()

		scaleWatch{watch: inv.watch, cutBefore: inv.cutBeforeFake}.cutUpdate(t, svc, p, after)
		finishUpdate(t, svc, f, false)

		wantSettledWith(t, svc, f, "prod-all-0")
		wantRaftIs(t, svc, f, combinedGroup)
		wantCombinedNodesReady(t, svc, f)
		if diff := cmp.Diff([]string{"prod-all-0"}, machinesOf(f, combinedGroup)); diff != "" {
			t.Errorf("the combined nodes (-want +got):\n%s", diff)
		}
		inv.wantKept()
		inv.wantQuorumFor(quorumSpan)
	})
}

// clientsFirewallKey is the key of the firewall group of the clients of the test cluster.
const clientsFirewallKey = "vultr.FirewallGroup/prod-clients"

// TestScaleCutRemovalOfAClientGroup cuts the removal of the last client group, which the specs no longer have, just
// before and just after the delete of its last machine and the delete of the firewall group of the clients. The next
// update finishes it: no worker, no node of one, and no firewall group of the clients.
func TestScaleCutRemovalOfAClientGroup(t *testing.T) {
	t.Parallel()
	points := []scalePoint{atVultr("DeleteInstance", 2), atVultr("DeleteFirewallGroup", 1)}
	eachScaleCut(t, points, func(t *testing.T, p scalePoint, after bool) {
		svc, f, w := newExample(t, exampleClusterYAML, exampleServersYAML, exampleWorkersYAML)
		w.SetDrainReads(drainReads)
		mustUpdate(t, svc)
		firewallGroupID(t, f, "client") // fails the test unless the build made the group that the update deletes
		inv := watchShrink(t, f, w, "workers", 0)
		if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
			t.Fatal(err)
		}

		scaleWatch{watch: inv.watch}.cutUpdate(t, svc, p, after)
		finishUpdate(t, svc, f, after && p == atVultr("DeleteInstance", 2))

		wantSettledWith(t, svc, f)
		wantRaftIs(t, svc, f, serversGroup)
		if got := machinesOf(f, "workers"); len(got) != 0 {
			t.Errorf("the workers %q are left", got)
		}
		for _, g := range f.FirewallGroups() {
			if key := markedKey(g.Description); key == clientsFirewallKey {
				t.Errorf("the firewall group %s is left", key)
			}
		}
	})
}

// TestScaleLosesTheAnswerOfAWrite shrinks the workers from 3 to 1 and loses the answer of the first mark, the first
// drain, the first purge and the first delete of a machine, after the fake carried the call out. The run goes on past
// a lost answer of Nomad, whose write goes to the next server, and ends at a lost answer of a delete; the next update
// finishes it. The invariants of a scale-down held at every call.
func TestScaleLosesTheAnswerOfAWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		point scalePoint
		ends  bool // the run ends with the error
		why   string
	}{
		{atNomad("MarkIneligible", 1), false, "the write goes to the next server"},
		{atNomad("Drain", 1), false, "the write goes to the next server"},
		{atNomad("Purge", 1), false, "the write goes to the next server"},
		{atVultr("DeleteInstance", 1), true, "a failed delete ends the run; the next run lists the machine as gone"},
	} {
		t.Run(tc.point.label(), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := scaleWorld(t, 1)
				inv := watchShrink(t, f, w, "workers", 1)

				err := scaleWatch{watch: inv.watch}.loseUpdate(t, svc, tc.point)

				if (err != nil) != tc.ends {
					t.Errorf("the run ended with %v, want an error: %v, because %s", err, tc.ends, tc.why)
				}
				if tc.ends {
					wantLockFree(t, svc.Store)
					finishUpdate(t, svc, f, false)
				}
				wantSettledWith(t, svc, f, "prod-workers-0")
				if diff := cmp.Diff([]string{"prod-workers-0"}, machinesOf(f, "workers")); diff != "" {
					t.Errorf("the workers (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestScaleCutEndsWhenADrainNeverCompletes shrinks the workers from 3 to 1 with a node whose drain never completes:
// the run ends at the group's drain time of ten minutes plus five, with a text that tells the operator to run tent
// update cluster again, the lock released, and a count of two drained and one purged, since the other worker was
// removed first. The next update, when the drain can complete, finishes the shrink.
func TestScaleCutEndsWhenADrainNeverCompletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 1)
		watchShrink(t, f, w, "workers", 1)
		neverDrained(w, "prod-workers-2")
		waiting := stampOf(svc, "nomad started drained prod-workers-2")

		ctx, cancel := context.WithTimeout(t.Context(), loopDrainTimeout+time.Hour) // a lost limit ends here
		defer cancel()
		plan, err := svc.Update(ctx, "prod", true)

		wantError(t, err, "node prod-workers-2 did not finish draining within 15m0s (Nomad lists it draining); "+
			"run tent update cluster again to go on waiting")
		wantWithin(t, time.Since(*waiting), loopDrainTimeout)
		if want := (app.RemovalCounts{Drained: 2, Purged: 1}); plan.Removed != want || plan.Applied {
			t.Errorf("the update removed %+v and applied %v, want %+v and not applied", plan.Removed, plan.Applied, want)
		}
		wantLockFree(t, svc.Store)
		w.ChangeNode("prod-workers-2", nil)

		finishUpdate(t, svc, f, false)

		wantSettledWith(t, svc, f, "prod-workers-0")
		if diff := cmp.Diff([]string{"prod-workers-0"}, machinesOf(f, "workers")); diff != "" {
			t.Errorf("the workers (-want +got):\n%s", diff)
		}
	})
}

// TestScaleCutGoesOnWhenNomadEndsADrainAtItsDeadline holds each drain until its deadline of ten minutes: Nomad stops
// what remains then, and the update goes on within the wait's limit and finishes the shrink.
func TestScaleCutGoesOnWhenNomadEndsADrainAtItsDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 1)
		watchShrink(t, f, w, "workers", 1)
		w.SetDrainReads(1 << 30)
		start := time.Now()

		plan := mustUpdate(t, svc)

		if took := time.Since(start); took < 10*time.Minute || took >= loopDrainTimeout {
			t.Errorf("the update took %v, want the drains' 10 minutes and less than %v", took, loopDrainTimeout)
		}
		wantScaledDown(t, svc, f, plan)
	})
}
