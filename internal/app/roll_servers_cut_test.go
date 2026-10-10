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

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// serverWrites are the writes that a roll of the servers makes, as the first words of a call.
var serverWrites = []string{
	"CreateInstance ", "UpdateInstance ", "HaltInstance ", "DeleteInstance ",
	"nomad TransferLeadership ", "nomad RemovePeer ", "nomad ForceLeave ",
}

// uninterruptedServersRoll applies the roll of the three outdated servers and returns the calls it makes to Vultr and
// Nomad, as flowCalls gives them.
func uninterruptedServersRoll(t *testing.T) []string {
	t.Helper()
	var calls []string
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := serversFlowWorld(t)
		calls = flowCalls(t, svc, f, w, func() *engine.Plan {
			if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
				t.Fatalf("RollingUpdate: %v", err)
			}
			return nil
		})
	})
	return calls
}

// callsOfKind returns how many of the calls, as callKey gives them, start with kind.
func callsOfKind(calls []string, kind string) int {
	return len(slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(callKey(c), kind) }))
}

// TestServersRollCutsStandAtEveryWrite checks the calls of an uninterrupted roll of three servers: every call is a read
// or a write of rollWrites, so that a cut stands at every write, and the roll makes each write of serverWrites: three
// servers make three of each but the transfer, which the oldest server, the leader, makes once.
func TestServersRollCutsStandAtEveryWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedServersRoll(t)

	wantCutPlaces(t, calls, serverWrites)
	for _, kind := range serverWrites {
		want := serverGroupSize
		if kind == "nomad TransferLeadership " {
			want = 1
		}
		if got := callsOfKind(calls, kind); got != want {
			t.Errorf("the roll makes %d calls %q, want %d", got, kind, want)
		}
	}
}

// cutServersRun runs a rolling update with apply under the invariants and ends its context at the call of c, just
// before the call reaches the fake or just after the fake carried it out, as runCut does. It fails the test unless the
// run made the call and stopped with an error that matches context.Canceled, and changed nothing in the store but
// the lock, which it released.
func cutServersRun(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld, inv *serverInvariants, c cutCase) {
	t.Helper()
	stored := snapshot(t, svc.Store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cut atomic.Bool
	act := func(ctx context.Context, next func(context.Context) error) error {
		cut.Store(true)
		if c.after {
			_ = next(ctx) // carried out; its answer is lost
		} else {
			inv.cutBeforeFake()
		}
		cancel()
		return next(ctx)
	}
	inv.watch(
		atCall(f, c.key, c.n, func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
			return act(ctx, next)
		}),
		atNomadCall(w, c.key, c.n, func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
			return act(ctx, next)
		}))

	_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})

	if !cut.Load() {
		t.Fatalf("the run made no call %s", c.key)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the run cut at %s returned %v, want an error that matches context.Canceled", c.key, err)
	}
	allowGrownNames(t, svc.Store, stored)
	wantSnapshot(t, svc.Store, stored)
	wantLockFree(t, svc.Store)
}

// wantServersCutDone fails the test unless the roll ended as a roll of three servers does, with the machines of old
// replaced, CreateInstance called creates times in all, and the invariants kept at every call.
func wantServersCutDone(
	t *testing.T, svc *app.Service, f *vultrfake.Fake, inv *serverInvariants, old []serverMachine, creates int,
) {
	t.Helper()
	wantRollEnded(t, svc, f, idsOf(old))
	wantServersRolled(t, svc, f, old, true)
	if got := countCalls(f, "CreateInstance"); got != creates {
		t.Errorf("CreateInstance was called %d times in all, want %d", got, creates)
	}
	inv.wantKept()
}

// TestServersRollFinishesAfterACutAtAnyWrite cuts a roll of three servers just before and just after each write of an
// uninterrupted roll to Vultr and to Nomad; a read changes nothing, so a cut at a read leaves what a cut at the write
// before it leaves. The next run, with a fresh context, finishes the roll: every server is replaced, with as many
// CreateInstance calls in the two runs as the uninterrupted roll makes. The cluster keeps the invariants at every
// call of both runs, and a name differs from the uninterrupted roll's only by the rule that names grow.
func TestServersRollFinishesAfterACutAtAnyWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedServersRoll(t)
	eachCutAt(t, calls, func(i int) bool { return isRollWrite(callKey(calls[i])) }, func(t *testing.T, c cutCase) {
		svc, f, w, inv := serversFlowWorld(t)
		old := serverMachines(f)
		creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")

		cutServersRun(t, svc, f, w, inv, c)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		wantServersCutDone(t, svc, f, inv, old, creates)
	})
}

// TestServersRollSendsTheStopAgainAfterACutWhileTheCloudListsTheServerRunning cuts a roll just after each stop of a
// server while the cloud lists a halted machine as running for four more reads, as Vultr does for some seconds. The
// next run lists the server as running and sends the stop again; its first reads of Nomad may go to the stopped
// server, which its API holds until the list shows the server stopped. It finishes the roll, with the invariants kept
// at every call.
func TestServersRollSendsTheStopAgainAfterACutWhileTheCloudListsTheServerRunning(t *testing.T) {
	t.Parallel()
	calls := uninterruptedServersRoll(t)
	for i, call := range calls {
		key := callKey(call)
		if !strings.HasPrefix(key, "HaltInstance ") {
			continue
		}
		c := cutCase{index: i + 1, key: key, n: 1, after: true} // the roll stops each server once
		t.Run(fmt.Sprintf("after %03d HaltInstance", c.index), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := serversFlowWorld(t)
				f.SetHaltReads(t, 4)
				old := serverMachines(f)
				creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")
				halts := countCalls(f, "HaltInstance") + serverGroupSize + 1

				cutServersRun(t, svc, f, w, inv, c)
				if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
					t.Fatalf("the run after the cut failed: %v", err)
				}

				wantRollEnded(t, svc, f, idsOf(old))
				wantServersRolled(t, svc, f, old, true)
				if got := countCalls(f, "CreateInstance"); got != creates {
					t.Errorf("CreateInstance was called %d times in all, want %d", got, creates)
				}
				if got := countCalls(f, "HaltInstance"); got != halts {
					t.Errorf("HaltInstance was called %d times in all, want %d: each server's and the one sent again", got, halts)
				}
			})
		})
	}
}

// lostAnswer says what the roll of the servers does when the answer to the first write of a kind is lost, and why.
type lostAnswer struct {
	kind string
	ends bool // the run ends with the error; otherwise it goes on to the end
	why  string
}

// lostAnswers are the lost answers of the writes of a roll of servers.
var lostAnswers = []lostAnswer{
	{"CreateInstance ", false, "the provider finds the machine by its operation id and returns it"},
	{"UpdateInstance ", true, "a failed scrub ends the run; the next run lists the machine with the joined label"},
	{"HaltInstance ", true, "a failed Stop ends the run; the next run lists the machine as stopped"},
	{"DeleteInstance ", true, "a failed delete ends the run; the next run no longer lists the machine"},
	{"nomad TransferLeadership ", false, "Servers sends the call to the next server, which answers a leader that leads"},
	{"nomad RemovePeer ", false, "Servers sends the call to the next server, which counts a gone peer as removed"},
	{"nomad ForceLeave ", false, "Servers sends the call to the next server, which answers a repeat"},
}

// TestServersRollFinishesAfterALostAnswerOfEachWrite loses the answer to the first write of each kind of a roll of
// three servers: the fake carries the call out, and the client gets no answer. The run ends with the error or goes on
// as lostAnswers says; when it ends, the next run finishes the roll. The cluster ends as wantServersCutDone says.
func TestServersRollFinishesAfterALostAnswerOfEachWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedServersRoll(t)
	for _, tc := range lostAnswers {
		i := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(callKey(c), tc.kind) })
		if i < 0 {
			t.Fatalf("the roll makes no write %q", tc.kind)
		}
		key := callKey(calls[i]) // the first call of its kind, so the first with its key
		t.Run(strings.TrimPrefix(strings.TrimSpace(tc.kind), "nomad "), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := serversFlowWorld(t)
				old := serverMachines(f)
				creates := countCalls(f, "CreateInstance") + callsOfKind(calls, "CreateInstance ")
				lost := false
				inv.watch(
					atCall(f, key, 1, func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
						lost = true
						_ = next(ctx)
						return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
					}),
					atNomadCall(w, key, 1, func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
						lost = true
						_ = next(ctx)
						return notReadyError(c.Name)
					}))

				_, err := applyRoll(svc, app.RollOptions{})

				if !lost {
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
				wantServersCutDone(t, svc, f, inv, old, creates)
			})
		})
	}
}

// neverEnds is a count of reads that a halted machine shows as running which no run of a roll reaches in the two
// minutes that it waits for the machine to read stopped.
const neverEnds = 1 << 10

// readUntilStopped reads the machines through ListInstances until the halted machine id reads stopped.
func readUntilStopped(t *testing.T, f *vultrfake.Fake, id string) {
	t.Helper()
	for range neverEnds + 1 {
		list, err := f.ListInstances(t.Context(), cloud.LabelCluster+"=prod")
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		for _, in := range list {
			if in.ID == id && in.PowerStatus == "stopped" {
				return
			}
		}
	}
	t.Fatalf("the machine %s still reads running after %d reads", id, neverEnds+1)
}

// afterNomadCall returns a Nomad hook that carries each call out and runs act after each call of the method name.
func afterNomadCall(name string, act func()) nomadHook {
	return func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		err := next(ctx)
		if c.Name == name {
			act()
		}
		return err
	}
}

// deadline is a wait of a roll of the servers that ends the run at its limit: how the world makes it last, the part of
// the error that ends the run, and how the world is set back for the next run.
type deadline struct {
	name  string
	limit time.Duration
	hold  func(t *testing.T, f *vultrfake.Fake, w *nomadWorld, inv *serverInvariants)
	want  string
	back  func(t *testing.T, f *vultrfake.Fake, w *nomadWorld, inv *serverInvariants)
}

// deadlines are the waits of a roll of the servers that have a limit. Other tests pin each error and its time.
var deadlines = []deadline{
	{
		name: "a stopped server that Serf never marks failed", limit: 5 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) { w.SetFailAfter(time.Hour) },
		want: "autopilot did not stop counting prod-servers-1 as a healthy voter within 5m0s",
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.SetFailAfter(40 * time.Second)
		},
	}, {
		name: "a server that never votes", limit: 10 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) { w.SetVoteAfter(time.Hour) },
		want: "node prod-servers-3 did not vote within 10m0s",
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.SetVoteAfter(15 * time.Second)
		},
	}, {
		name: "a window that keeps moving", limit: 5 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.ChangeServer("prod-servers-3", func(s *nomadops.ServerHealth) { s.StableSince = time.Now() })
		},
		want: "the servers did not become stable within 5m0s",
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			w.ChangeServer("prod-servers-3", nil)
		},
	}, {
		name: "a cluster that stays unhealthy after the force-leave", limit: 10 * time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, inv *serverInvariants) {
			inv.watch(nil, afterNomadCall("ForceLeave", w.Unhealthy))
		},
		want: "the servers did not become healthy and vote within 10m0s",
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, inv *serverInvariants) {
			w.SetUnhealthy(false)
			inv.watch(nil, nil)
		},
	}, {
		name: "a stop that the cloud never lists as stopped", limit: 2 * time.Minute,
		hold: func(t *testing.T, f *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			f.SetHaltReads(t, neverEnds)
			w.SetFailAfter(time.Hour)
		},
		want: "the cloud still lists node prod-servers-1",
		back: func(t *testing.T, f *vultrfake.Fake, w *nomadWorld, _ *serverInvariants) {
			f.SetHaltReads(t, 0)
			w.SetFailAfter(40 * time.Second)
			readUntilStopped(t, f, instanceNamed(t, f, "prod-servers-1"))
		},
	}, {
		name: "a refusal that lasts a minute after a step", limit: time.Minute,
		hold: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, inv *serverInvariants) {
			inv.watch(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
				err := next(ctx)
				if c.Name == "UpdateInstance" && err == nil {
					w.SetUnhealthy(true)
				}
				return err
			}, nil)
		},
		want: "autopilot reports the servers unhealthy",
		back: func(_ *testing.T, _ *vultrfake.Fake, w *nomadWorld, inv *serverInvariants) {
			w.SetUnhealthy(false)
			inv.watch(nil, nil)
		},
	},
}

// TestServersRollGoesOnAfterADeadlineEndedTheRun lets each wait of a roll of three servers that has a limit reach it:
// the run ends with the wait's error. The next run, with the world set back, finishes the roll with no machine more
// than the uninterrupted roll creates, and the invariants kept at every call of both runs. Other tests pin the text
// and the time of each of these ends.
func TestServersRollGoesOnAfterADeadlineEndedTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range deadlines {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := serversFlowWorld(t)
				old := serverMachines(f)
				creates := countCalls(f, "CreateInstance") + serverGroupSize
				tc.hold(t, f, w, inv)

				_, err := rollBefore(svc, tc.limit)

				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the roll ended with %v, want an error that says %q", err, tc.want)
				}
				tc.back(t, f, w, inv)
				if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
					t.Fatalf("the next run failed: %v", err)
				}

				wantServersCutDone(t, svc, f, inv, old, creates)
			})
		})
	}
}

// rollUntilCreated runs a rolling update with apply that ends right after its next CreateInstance, which the fake
// carries out, and returns the ID of the machine that it made. The lists by the cluster's tag leave the machine
// hidden out until then. It fails the test unless the run made a create and ended with an error that matches
// context.Canceled.
func rollUntilCreated(t *testing.T, svc *app.Service, f *vultrfake.Fake, hidden string) (created string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		switch {
		case c.Name == "CreateInstance":
			err := next(ctx)
			if err == nil {
				all := f.Instances()
				created = all[len(all)-1].ID
			}
			cancel()
			return err
		case c.Name == "ListInstances" && c.Arg == cloud.LabelCluster+"=prod" && hidden != "":
			defer hideFromList(t, f, hidden)()
		}
		return next(ctx)
	})

	_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})

	f.SetHook(nil)
	if created == "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("the run created %q and ended with %v, want a create and an end by the context", created, err)
	}
	return created
}

// TestServersRollNamesTheNextServerWhenTheListsMissTheOneItCreated cuts a run right after the create of a new server
// that is not yet a Raft peer or a gossip member, and hides it from the lists of the next run by the cluster's tag.
// That run sees no trace of the server, but the store holds its index: the run creates the next name, not a twin, and
// is cut right after it. The run after that lists both: the group has two servers more than its size, and the roll
// removes two old servers before it creates the third new one and ends with the group at its size. No two machines
// share a name, and the store holds the highest index.
func TestServersRollNamesTheNextServerWhenTheListsMissTheOneItCreated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		old := serverMachines(f)

		first := rollUntilCreated(t, svc, f, "")
		second := rollUntilCreated(t, svc, f, first)
		lines := recordProgress(svc)
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run that lists both new servers failed: %v", err)
		}

		create := slices.Index(*lines, "node started create prod-servers-5")
		if create < 0 || count((*lines)[:create], "node done delete prod-servers-0")+
			count((*lines)[:create], "node done delete prod-servers-1")+
			count((*lines)[:create], "node done delete prod-servers-2") != 2 {
			t.Errorf("the roll created prod-servers-5 at line %d of its progress, want it after the deletes of two "+
				"old servers:\n%s", create, strings.Join(*lines, "\n"))
		}

		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, old, true)
		var names []string
		for _, m := range serverMachines(f) {
			names = append(names, m.name)
		}
		if want := []string{"prod-servers-3", "prod-servers-4", "prod-servers-5"}; !slices.Equal(names, want) {
			t.Errorf("the new servers are %v, want %v: the first run made %s and the second %s", names, want, first,
				second)
		}
		wantStored(t, svc.Store, namesPath, []byte("5\n"))
	})
}
