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
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// uninterruptedSingleRoll applies the roll of the one outdated server and returns the calls it makes to Vultr and
// Nomad, as flowCalls gives them.
func uninterruptedSingleRoll(t *testing.T) []string {
	t.Helper()
	var calls []string
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		calls = flowCalls(t, s.svc, s.f, s.w, func() *engine.Plan {
			if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
				t.Fatalf("RollingUpdate: %v", err)
			}
			return nil
		})
	})
	return calls
}

// TestSingleRollCutsStandAtEveryWrite checks the calls of an uninterrupted roll of one server: every call is a read or
// a write of rollWrites, so that a cut stands at every write, and the roll makes each write of serverWrites once but
// the removal of a peer, which it makes twice.
func TestSingleRollCutsStandAtEveryWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedSingleRoll(t)

	wantCutPlaces(t, calls, serverWrites)
	for _, kind := range serverWrites {
		want := 1
		if kind == "nomad RemovePeer " {
			want = 2
		}
		if got := callsOfKind(calls, kind); got != want {
			t.Errorf("the roll makes %d calls %q, want %d", got, kind, want)
		}
	}
}

// TestSingleRollFinishesAfterACutAtAnyWrite cuts a roll of one server just before and just after each write of an
// uninterrupted roll to Vultr and to Nomad. The clock then runs for two reconciles and a promotion with nobody
// watching, and the voters that run must stay a quorum. The next run, with a fresh context, finishes the roll: the
// server is replaced with one CreateInstance call in the two runs. The cluster keeps the invariants at every call of
// both runs.
func TestSingleRollFinishesAfterACutAtAnyWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedSingleRoll(t)
	eachCutAt(t, calls, func(i int) bool { return isRollWrite(callKey(calls[i])) }, func(t *testing.T, c cutCase) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)

		cutServersRun(t, s.svc, s.f, s.w, s.inv, c)
		s.inv.wantQuorumFor(quorumSpan)
		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		s.wantFinished(t, workers)
	})
}

// TestSingleRollFinishesAfterACutAtTheStopThatTheNextRunStartsLater cuts a roll of one server just before and just
// after the stop, and starts the next run at once, 25 s and 50 s after the cut. A cut before the stop leaves both
// servers running and the old one out of the Raft configuration; a cut after it leaves a machine that stops within
// seconds. The voters that run are a quorum until the next run, which finishes the roll.
func TestSingleRollFinishesAfterACutAtTheStopThatTheNextRunStartsLater(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before", "after"} {
		after := when == "after"
		for _, later := range []time.Duration{0, 25 * time.Second, 50 * time.Second} {
			t.Run(fmt.Sprintf("%s, %v later", when, later), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					s := newSingleWorld(t, time.Minute, 10*time.Second)
					workers := workerIDs(s.f)

					s.cutRun(t, "HaltInstance", 1, after)
					s.inv.wantQuorumFor(later)
					if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
						t.Fatalf("the run after the cut failed: %v", err)
					}

					s.wantFinished(t, workers)
					if got := countCalls(s.f, "HaltInstance"); got != 1 {
						t.Errorf("HaltInstance was called %d times in all, want 1", got)
					}
				})
			})
		}
	}
}

// TestSingleRollRemovesAStoppedServerThatTheLeaderAddedAfterACut cuts a roll right after the stop of the old server,
// whose member stays alive for 70 s after the machine stopped, and starts the next run 70 s later. The leader's
// reconcile between added the stopped server as a nonvoter, which never votes. The run removes its peer, forces the
// member out and deletes the machine.
func TestSingleRollRemovesAStoppedServerThatTheLeaderAddedAfterACut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		s.w.SetFailAfter(70 * time.Second)
		workers := workerIDs(s.f)

		s.cutRun(t, "HaltInstance", 1, true)
		s.inv.wantQuorumFor(70 * time.Second)
		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		removed := s.since(s.inv.reads("RemovePeer"))
		if len(removed) != 3 || removed[2] < 3*time.Minute {
			t.Errorf("the roll removed the peer at %v, want three times, the third after the reconcile of 3m0s", removed)
		}
		s.wantFinished(t, workers)
	})
}

// singleLostAnswer says what the roll of one server does when the answer to a write is lost, and why.
type singleLostAnswer struct {
	kind string
	n    int  // the call is the nth of its kind
	ends bool // the run ends with the error; otherwise it goes on to the end
	why  string
}

// singleLostAnswers are the lost answers of the writes of a roll of one server.
var singleLostAnswers = []singleLostAnswer{
	{"CreateInstance ", 1, false, "the provider finds the machine by its operation id and returns it"},
	{"UpdateInstance ", 1, true, "a failed scrub ends the run; the next run lists the machine with the joined label"},
	{"nomad TransferLeadership ", 1, false,
		"Servers sends the call to the next server, which answers a leader that leads"},
	{"nomad RemovePeer ", 1, false, "the run waits a poll, and the next observation shows the peer gone"},
	{"nomad RemovePeer ", 2, false,
		"the run does not count the removal: it holds the stop until the leader adds the server again"},
	{"HaltInstance ", 1, false, "the stop is a held one: it counts as sent, and the run lists the machine as stopped"},
	{"nomad ForceLeave ", 1, false, "Servers sends the call to the next server, which answers a repeat"},
	{"DeleteInstance ", 1, true, "a failed delete ends the run; the next run no longer lists the machine"},
}

// TestSingleRollFinishesAfterALostAnswerOfEachWrite loses the answer to a write of the roll of one server: the fake
// carries the call out, and the client gets no answer. The run ends with the error or goes on as singleLostAnswers
// says; when it ends, the next run finishes the roll. A lost answer of the stop does not end the run, so the roll
// sends one HaltInstance in all.
func TestSingleRollFinishesAfterALostAnswerOfEachWrite(t *testing.T) {
	t.Parallel()
	calls := uninterruptedSingleRoll(t)
	for _, tc := range singleLostAnswers {
		keys := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(callKey(c), tc.kind) })
		if tc.n > len(keys) {
			t.Fatalf("the roll makes %d writes %q, want a %dth", len(keys), tc.kind, tc.n)
		}
		key := callKey(keys[tc.n-1])
		t.Run(fmt.Sprintf("%s %d", strings.TrimPrefix(strings.TrimSpace(tc.kind), "nomad "), tc.n), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newSingleWorld(t, time.Minute, 10*time.Second)
				workers := workerIDs(s.f)
				lost := false
				s.hooks(
					atCall(s.f, key, tc.n, func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
						lost = true
						_ = next(ctx)
						return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
					}),
					atNomadCall(s.w, key, tc.n, func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
						lost = true
						_ = next(ctx)
						return notReadyError(c.Name)
					}))

				plan, err := applyRoll(s.svc, app.RollOptions{})

				if !lost {
					t.Fatalf("the roll made no call %s", key)
				}
				if (err != nil) != tc.ends {
					t.Errorf("the roll ended with %v, want an error: %v, because %s", err, tc.ends, tc.why)
				}
				if want := (app.RollCounts{Created: 1, Stopped: 1, Deleted: 1}); err == nil && plan.Rolled != want {
					t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
				}
				if err != nil {
					if _, err = applyRoll(s.svc, app.RollOptions{}); err != nil {
						t.Fatalf("the run after the lost answer failed: %v", err)
					}
				}
				if got := countCalls(s.f, "HaltInstance"); got != 1 {
					t.Errorf("HaltInstance was called %d times in all, want 1", got)
				}
				s.wantFinished(t, workers)
			})
		})
	}
}

// singleDeadline is a wait of the roll of one server that ends the run at its limit: how the world makes it last, the
// part of the error that ends the run, what the world shows when it has, and how the world is set back for the next
// run.
type singleDeadline struct {
	name  string
	limit time.Duration
	hold  func(t *testing.T, s *singleWorld)
	want  string
	ended func(t *testing.T, s *singleWorld, lines []string) // what the run left; may be nil
	back  func(t *testing.T, s *singleWorld)
}

// wantNoHalt fails the test when the roll sent a HaltInstance, as no deadline that it meets before the stop allows.
func wantNoHalt(t *testing.T, s *singleWorld) {
	t.Helper()
	if got := countCalls(s.f, "HaltInstance"); got != 0 {
		t.Errorf("the roll sent %d HaltInstance calls, want none", got)
	}
}

// holdLimit is the text that ends a run which waited for the leader's reconcile for holdTimeout.
const holdLimit = "tent could not stop node prod-servers-0 right after a reconcile of Nomad's leader within 5m0s; " +
	"both servers run; run tent rolling-update cluster again"

// reconcileBack sets the leader's reconcile back to the interval and the promotion that the world started with.
func reconcileBack(_ *testing.T, s *singleWorld) { s.w.SetReconcile(time.Minute, 10*time.Second) }

// singleDeadlines are the waits of a roll of one server that have a limit. Other tests pin each error and its time.
var singleDeadlines = []singleDeadline{
	{
		name: "a new server that never votes", limit: 10 * time.Minute,
		hold: func(_ *testing.T, s *singleWorld) { s.w.SetVoteAfter(time.Hour) },
		want: "node prod-servers-1 did not vote within 10m0s",
		ended: func(t *testing.T, s *singleWorld, _ []string) {
			t.Helper()
			voters, leader, _ := s.w.raftVoters("")
			if want := []string{s.old[0].id}; !slices.Equal(voters, want) || leader != s.old[0].id {
				t.Errorf("the voters are %v and the leader %s, want only the old server %s, which leads", voters, leader, want[0])
			}
			wantNoHalt(t, s)
		},
		back: func(_ *testing.T, s *singleWorld) { s.w.SetVoteAfter(15 * time.Second) },
	}, {
		name: "a stop that the cloud never lists as stopped", limit: 2 * time.Minute,
		hold: func(t *testing.T, s *singleWorld) {
			s.f.SetHaltReads(t, neverEnds)
			s.w.SetFailAfter(time.Hour)
		},
		want: "the cloud still lists node prod-servers-0 (ID instance-1) as running 2m0s after tent stopped it; " +
			"run tent rolling-update cluster again",
		ended: func(t *testing.T, s *singleWorld, _ []string) {
			t.Helper()
			if removed := s.since(s.inv.reads("RemovePeer")); len(removed) < 3 {
				t.Errorf("the roll removed the peer at %v, want it removed again after the reconcile of 3m0s", removed)
			}
		},
		back: func(t *testing.T, s *singleWorld) {
			s.f.SetHaltReads(t, 0)
			s.w.SetFailAfter(40 * time.Second)
			readUntilStopped(t, s.f, instanceNamed(t, s.f, "prod-servers-0"))
		},
	}, {
		name: "a stop call that gets no answer and a halt that the cloud never carries out", limit: 2 * time.Minute,
		hold: func(_ *testing.T, s *singleWorld) {
			s.hooks(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
				if c.Name != "HaltInstance" {
					return next(ctx)
				}
				<-ctx.Done()
				s.inv.cutBeforeFake()
				return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
			}, nil)
		},
		want: "stop node prod-servers-0 (ID instance-1): stop node prod-servers-0 (instance-1): " +
			vultr.NewNoAnswerError("HaltInstance", "instance-1", errLost).Error() +
			"; the cloud still lists it as running after 2m0s; run tent rolling-update cluster again",
		ended: func(t *testing.T, s *singleWorld, lines []string) {
			t.Helper()
			want := []time.Duration{70 * time.Second, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute}
			if removed := s.since(s.inv.reads("RemovePeer")); !slices.Equal(removed, want) {
				t.Errorf("the roll removed the peer at %v, want %v: again at each reconcile of the 2 minutes, with no poll "+
					"before it", removed, want)
			}
			if got := count(lines, "nomad started reconcile prod-servers-0"); got != 1 {
				t.Errorf("the roll waited for a reconcile %d times, want once, before the stop", got)
			}
		},
		back: func(_ *testing.T, s *singleWorld) { s.hooks(nil, nil) },
	}, {
		name: "a server that votes again at every reconcile", limit: holdTimeoutLimit,
		hold: func(_ *testing.T, s *singleWorld) { s.w.SetReconcile(time.Minute, 0) },
		want: holdLimit,
		ended: func(t *testing.T, s *singleWorld, _ []string) {
			t.Helper()
			if removed := s.since(s.inv.reads("RemovePeer")); len(removed) != 6 {
				t.Errorf("the roll removed the peer at %v, want 6 times: once, and at each reconcile of the 5 minutes", removed)
			}
			wantNoHalt(t, s)
		},
		back: reconcileBack,
	}, {
		name: "a server that the leader never adds again", limit: holdTimeoutLimit,
		hold: func(_ *testing.T, s *singleWorld) { s.w.SetReconcile(0, 0) },
		want: holdLimit,
		ended: func(t *testing.T, s *singleWorld, _ []string) {
			t.Helper()
			wantNoHalt(t, s)
		},
		back: reconcileBack,
	}, {
		name: "a removal that has no effect", limit: time.Minute,
		hold: func(_ *testing.T, s *singleWorld) {
			var kept *nomadops.Peer
			s.w.ChangePeers(func(peers []nomadops.Peer) []nomadops.Peer {
				for _, p := range peers {
					if p.ID == raftIDOf(s.old[0].id) {
						kept = &p
					}
				}
				if kept != nil && !slices.ContainsFunc(peers, func(p nomadops.Peer) bool { return p.ID == kept.ID }) {
					return append(peers, *kept)
				}
				return peers
			})
		},
		want: "remove prod-servers-0 from the Raft configuration had no effect after 3 tries",
		back: func(_ *testing.T, s *singleWorld) { s.w.ChangePeers(nil) },
	}, {
		name: "a cluster that stays unhealthy after the force-leave", limit: 10 * time.Minute,
		hold: func(_ *testing.T, s *singleWorld) { s.hooks(nil, afterNomadCall("ForceLeave", s.w.Unhealthy)) },
		want: "the servers did not become healthy and vote within 10m0s",
		back: func(_ *testing.T, s *singleWorld) {
			s.w.SetUnhealthy(false)
			s.hooks(nil, nil)
		},
	},
}

// holdTimeoutLimit is how long the roll may last before it gives up on the holds: their five minutes and the minutes
// of the roll before the first of them.
const holdTimeoutLimit = 10 * time.Minute

// TestSingleRollGoesOnAfterADeadlineEndedTheRun lets each wait of a roll of one server that has a limit reach it: the
// run ends with the wait's error. The next run, with the world set back, finishes the roll with no machine more than
// the uninterrupted roll creates, the voters that run a quorum for two reconciles and a promotion, and the invariants
// kept at every call of both runs. Other tests pin the text and the time of each of these ends.
func TestSingleRollGoesOnAfterADeadlineEndedTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range singleDeadlines {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newSingleWorld(t, time.Minute, 10*time.Second)
				workers := workerIDs(s.f)
				tc.hold(t, s)
				lines := recordProgress(s.svc)

				_, err := rollBefore(s.svc, tc.limit)

				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the roll ended with %v, want an error that says %q", err, tc.want)
				}
				if tc.ended != nil {
					tc.ended(t, s, *lines)
				}
				s.inv.wantQuorumFor(quorumSpan)
				tc.back(t, s)
				if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
					t.Fatalf("the next run failed: %v", err)
				}

				s.wantFinished(t, workers)
			})
		})
	}
}

// TestSingleRollGoesOnWhenTheStopCallAnswersAfterNineSeconds delays the answer of the stop by 9 s, less than the 10 s
// that the call may take: the stop goes on as every answered stop, and the roll ends with one HaltInstance call.
func TestSingleRollGoesOnWhenTheStopCallAnswersAfterNineSeconds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		workers := workerIDs(s.f)
		s.hooks(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "HaltInstance" {
				time.Sleep(9 * time.Second)
			}
			return next(ctx)
		}, nil)

		if _, err := applyRoll(s.svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		if got := countCalls(s.f, "HaltInstance"); got != 1 {
			t.Errorf("HaltInstance was called %d times in all, want 1", got)
		}
		s.wantFinished(t, workers)
	})
}

// TestSingleRollEndsWithTheAdviceToStartAStoppedServerWhenNoServerAnswers halts the old server's machine from outside
// while it votes beside the new server and the cluster has no leader, as two voters with one stopped do: the plan and
// the run both end with the reads' error and the advice to start the stopped instance again.
func TestSingleRollEndsWithTheAdviceToStartAStoppedServerWhenNoServerAnswers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		s.cutRun(t, "TransferLeadership", 1, true)
		s.f.SetHook(nil) // the invariants would fail the test for the cluster that this one makes
		s.w.SetHook(nil)
		if err := s.f.HaltInstance(t.Context(), s.old[0].id); err != nil {
			t.Fatalf("HaltInstance: %v", err)
		}
		s.w.NoLeader()
		const advice = "; node prod-servers-0 (ID instance-1) is stopped: if the servers have lost their quorum, " +
			"start that instance again and run the command again"
		time.Sleep(singleHaltLag)

		_, planErr := rollingUpdate(s.svc, app.RollOptions{})
		_, runErr := rollBefore(s.svc, 10*time.Minute)

		for name, err := range map[string]error{"plan": planErr, "run": runErr} {
			if !errors.Is(err, nomadops.ErrNotReady) || !strings.HasSuffix(err.Error(), advice) {
				t.Errorf("the %s ended with %v, want an error from the reads that ends with %q", name, err, advice)
			}
		}
	})
}
