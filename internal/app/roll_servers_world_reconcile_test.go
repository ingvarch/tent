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

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// SetReconcile makes the leader of the world add a removed server again, as Nomad's leader does. A pass of the leader
// comes every `every` after the moment it took the leadership (the election and each transfer); at a pass, each server
// whose peer was removed and whose member is alive goes back into the Raft configuration as a nonvoter, with its Raft
// ID and a StableSince of that pass. Autopilot makes it a voter `promote` later, also when its machine has stopped
// meanwhile, as long as its member is alive then. A server whose machine had stopped at the pass reads unhealthy and
// never votes. An `every` of 0, the default, turns all of this off. A server that started a cluster of its own is
// never added. With the reconcile on, a removed server whose member is alive is also listed alive and healthy in the
// report for the report lag, and the API of one that runs fails as Nomad's does (see reachesRemoved).
func (w *nomadWorld) SetReconcile(every, promote time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.reconcileEvery, w.delays.promoteAfter = every, promote
}

// SetHaltLag makes the machine of a halted or deleted server run for d more as far as Nomad goes, from when the world
// first saw it halted or gone: it answers calls, its member is alive, and a pass of the reconcile in that time adds it
// as a server that runs. The time that failAfter lets it stay alive counts from the end of the lag.
func (w *nomadWorld) SetHaltLag(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.delays.haltLag = d
}

// RemovedCalls returns the calls that reached the address of a running server that is no peer, in order, from 2 s after
// its removal on, with the reconcile on. Such a call fails with nomadops.ErrNotReady after 5 s and reaches neither the
// hook nor the Nomad fake. Members is not among them.
func (w *nomadWorld) RemovedCalls() []nomadfake.Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.removedCalls)
}

// The times of a removed server's own API.
const (
	removedAnswersFor = 2 * time.Second // a removed server that runs answers calls this long after its removal
	noLeaderAfter     = 5 * time.Second // then each call but Members waits this long and fails with no leader
)

// runsAt reports whether the machine of the server runs at t as far as Nomad goes: until the world saw it halted or
// gone, and for lag after.
func (r *raftServer) runsAt(t time.Time, lag time.Duration) bool {
	return r.down.IsZero() || t.Before(r.down.Add(lag))
}

// memberAliveAt reports whether the member of the server in the gossip pool is alive at t.
func (r *raftServer) memberAliveAt(t time.Time, d serverDelays) bool {
	return !r.forced && !r.left && !r.failed(t, d)
}

// promoted reports whether autopilot has made the server vote at now, after the leader added it again: it votes from
// promoteAt on, if its member was still alive then.
func (r *raftServer) promoted(now time.Time, d serverDelays) bool {
	return !r.promoteAt.IsZero() && !now.Before(r.promoteAt) && !r.failed(r.promoteAt, d)
}

// cancelPromotion keeps the server from voting when a ForceLeave takes its member out at now before autopilot has
// promoted it.
func (r *raftServer) cancelPromotion(now time.Time) {
	if now.Before(r.promoteAt) {
		r.promoteAt = time.Time{}
	}
}

// lingers reports whether the report lists the removed server as alive and healthy while it lists it at all: with the
// reconcile on, that is a removed server whose member is alive.
func (r *raftServer) lingers(now time.Time, d serverDelays) bool {
	return d.reconcileEvery > 0 && r.removed && r.memberStatus(now, d) == "alive"
}

// readd puts the removed server back into the Raft configuration at the first pass of the leader after its removal, if
// that pass came by now and the server's member was alive at it. The server comes back as a nonvoter with its
// StableSince at the pass. A server whose machine had stopped at the pass reads unhealthy and never votes; one that
// ran is promoted promoteAfter later. A server that started a cluster of its own is never added. The caller holds
// w.mu.
func (w *nomadWorld) readd(r *raftServer, now time.Time) {
	c, d := &w.raft, w.delays
	if d.reconcileEvery == 0 || c.leader == "" || r.alone || !r.removed {
		return
	}
	from := laterOf(r.removedAt, c.ledAt)
	pass := c.ledAt.Add((from.Sub(c.ledAt)/d.reconcileEvery + 1) * d.reconcileEvery)
	if pass.After(now) || !r.memberAliveAt(pass, d) {
		return
	}
	runs := r.runsAt(pass, d.haltLag)
	r.removed, r.readded, r.stable, r.deadWhenAdded = false, pass, pass, !runs
	r.promoteAt = time.Time{}
	if runs {
		r.promoteAt = pass.Add(d.promoteAfter)
	}
}

// reachesRemoved checks a call to the address of a server that the Raft configuration lacks. The caller has sent a
// call to a halted or gone machine away already, so the machine runs. It returns the error that the call fails with
// and how long it takes to fail; the call then reaches neither the hook nor the Nomad fake. A server that started a
// cluster of its own fails every call at once with an error that does not match nomadops.ErrNotReady, with the
// reconcile on or off. With the reconcile on, a removed server answers for removedAnswersFor after its removal, as
// Nomad does; then each call but Members fails with no leader after noLeaderAfter, and shows in RemovedCalls.
func (w *nomadWorld) reachesRemoved(call nomadfake.Call) (time.Duration, error) {
	host, _, _ := strings.Cut(call.Server, ":")
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.IndexFunc(w.raft.servers, func(r *raftServer) bool { return r.address == host && r.removed })
	if i < 0 {
		return 0, nil
	}
	now, r := time.Now(), w.raft.servers[i]
	switch {
	case r.alone:
		return 0, fmt.Errorf("%s started a cluster of its own", r.name)
	case w.delays.reconcileEvery == 0 || call.Name == "Members" || now.Sub(r.removedAt) < removedAnswersFor:
		return 0, nil
	}
	w.removedCalls = append(w.removedCalls, call)
	return noLeaderAfter, fmt.Errorf("%s: No cluster leader: %w", call.Server, nomadops.ErrNotReady)
}

// reconcileWorld is the test cluster with the servers over time, a pass of the leader every minute and a promotion
// 10 s after a server was added again, set further by setups.
func reconcileWorld(t *testing.T, setups ...func(*nomadWorld)) liveWorld {
	t.Helper()
	return newLiveWorldWith(t, func(w *nomadWorld) {
		w.ServersOverTime()
		w.SetReconcile(time.Minute, 10*time.Second)
		for _, setup := range setups {
			setup(w)
		}
	})
}

// sleepUntil sleeps to d after at.
func sleepUntil(at time.Time, d time.Duration) { time.Sleep(time.Until(at.Add(d))) }

// startLeadership moves the leadership to a follower at a moment with a part of a second, and returns the moment and
// a follower that runs and is no leader, which is not the old leader: the one to remove.
func (b liveWorld) startLeadership(t *testing.T) (time.Time, worldServer) {
	t.Helper()
	atHalfSecond()
	fs := b.followers(t)
	if err := b.api.TransferLeadership(t.Context(), fs[1].raftID()); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}
	return time.Now(), fs[0]
}

// removePeer removes the peer of s.
func (b liveWorld) removePeer(t *testing.T, s worldServer) {
	t.Helper()
	if err := b.api.RemovePeer(t.Context(), s.raftID()); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
}

// forceLeave forces the member of s out of the gossip pool.
func (b liveWorld) forceLeave(t *testing.T, s worldServer) {
	t.Helper()
	if err := b.api.ForceLeave(t.Context(), s.member()); err != nil {
		t.Fatalf("ForceLeave: %v", err)
	}
}

// clientAt returns a Nomad client that calls the address of s.
func (b liveWorld) clientAt(t *testing.T, s worldServer) nomadops.API {
	t.Helper()
	api, err := b.w.svc.Nomad(nomadops.Config{Address: s.address + ":4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	return api
}

// wantRaftPlace fails the test unless the server of st is in the Raft configuration as wanted: absent, a nonvoter or a
// voter.
func wantRaftPlace(t *testing.T, st serverState, when, want string) {
	t.Helper()
	got := "absent"
	switch {
	case st.peer == nil:
	case st.peer.Voter:
		got = "voter"
	default:
		got = "nonvoter"
	}
	if got != want {
		t.Errorf("%s: the server is %s in the Raft configuration (%+v), want %s", when, got, st.peer, want)
	}
}

// wantUnhealthyAlive fails the test unless the report lists the server with an alive Serf status and not healthy, and
// the cluster is not healthy.
func wantUnhealthyAlive(t *testing.T, st serverState, when string) {
	t.Helper()
	if st.entry == nil || st.entry.Serf != "alive" || st.entry.Healthy || st.health.Healthy {
		t.Errorf("%s: the entry is %+v and the cluster healthy %v, want an alive server that is not healthy, in an "+
			"unhealthy cluster", when, st.entry, st.health.Healthy)
	}
}

// TestWorldReconcileAddsARemovedLiveServerAsANonvoterAndPromotesIt checks that a running server whose peer was removed
// 25 s after the leader took the leadership is a nonvoter again at 60 s, with its Raft ID and a StableSince of that
// moment, and a voter at 70 s.
func TestWorldReconcileAddsARemovedLiveServerAsANonvoterAndPromotesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)

		sleepUntil(at, 59*time.Second)
		wantRaftPlace(t, b.state(t, victim), "59 s after the leadership", "absent")
		sleepUntil(at, 60*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "60 s after", "nonvoter")
		if want := at.Add(time.Minute).Truncate(time.Second); st.entry == nil || !st.entry.StableSince.Equal(want) {
			t.Errorf("the entry of the server is %+v, want StableSince %v", st.entry, want)
		}
		if st.health.Voters != 2 {
			t.Errorf("60 s after: %d voters, want the two servers that were not removed", st.health.Voters)
		}
		sleepUntil(at, 69*time.Second)
		wantRaftPlace(t, b.state(t, victim), "69 s after", "nonvoter")
		sleepUntil(at, 70*time.Second)
		st = b.state(t, victim)
		wantRaftPlace(t, st, "70 s after", "voter")
		if st.health.Voters != 3 {
			t.Errorf("70 s after: %d voters, want 3", st.health.Voters)
		}
	})
}

// TestWorldReconcileAddsAServerRemovedAgainAtTheNextPass checks that a server removed at 61 s, just after the pass
// that added it again, is a nonvoter again at 120 s and not before.
func TestWorldReconcileAddsAServerRemovedAgainAtTheNextPass(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 60*time.Second)
		wantRaftPlace(t, b.state(t, victim), "60 s after", "nonvoter")

		sleepUntil(at, 61*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 119*time.Second)
		wantRaftPlace(t, b.state(t, victim), "119 s after", "absent")
		sleepUntil(at, 120*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "120 s after", "nonvoter")
		if want := at.Add(2 * time.Minute).Truncate(time.Second); st.entry == nil || !st.entry.StableSince.Equal(want) {
			t.Errorf("the entry of the server is %+v, want StableSince %v", st.entry, want)
		}
	})
}

// TestWorldReconcileMinuteStartsAgainAtATransfer checks that a transfer of the leadership at 40 s moves the pass that
// adds a server removed at 25 s from 60 s to 100 s.
func TestWorldReconcileMinuteStartsAgainAtATransfer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)

		sleepUntil(at, 40*time.Second)
		lead := b.leader(t)
		var next worldServer
		for _, s := range b.followers(t) {
			if s != victim {
				next = s
			}
		}
		if err := b.api.TransferLeadership(t.Context(), next.raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}
		if b.leader(t) == lead {
			t.Fatalf("the leadership stayed with %s", lead)
		}
		sleepUntil(at, 99*time.Second)
		wantRaftPlace(t, b.state(t, victim), "99 s after the first leadership", "absent")
		sleepUntil(at, 100*time.Second)
		wantRaftPlace(t, b.state(t, victim), "100 s after", "nonvoter")
	})
}

// TestWorldReconcileMinuteStartsAtTheElection checks that a leader elected after the leader's machine halted counts
// its minute from the election.
func TestWorldReconcileMinuteStartsAtTheElection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		b.w.FailOnLeaderLoss(&failureTB{TB: t}) // the test halts the leader
		old := b.leader(t)
		atHalfSecond()
		b.haltInstance(t, old)
		at := time.Now()
		lead := b.leader(t)
		if lead == old {
			t.Fatalf("the halted server %s still leads", old)
		}
		var victim worldServer
		for _, s := range b.followers(t) {
			if s != old && s != lead {
				victim = s
			}
		}
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)

		sleepUntil(at, 59*time.Second)
		wantRaftPlace(t, b.state(t, victim), "59 s after the election", "absent")
		sleepUntil(at, 60*time.Second)
		wantRaftPlace(t, b.state(t, victim), "60 s after", "nonvoter")
	})
}

// TestWorldReconcilePromoteOfZeroVotesAtTheReAdd checks that with a promotion of 0 the server votes at the pass, and
// that a read after the pass still shows the pass as its StableSince.
func TestWorldReconcilePromoteOfZeroVotesAtTheReAdd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t, func(w *nomadWorld) { w.SetReconcile(time.Minute, 0) })
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)

		sleepUntil(at, 59*time.Second)
		wantRaftPlace(t, b.state(t, victim), "59 s after", "absent")
		sleepUntil(at, 65*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "65 s after, the first read since the pass", "voter")
		if want := at.Add(time.Minute).Truncate(time.Second); st.entry == nil || !st.entry.StableSince.Equal(want) {
			t.Errorf("the entry of the server is %+v, want StableSince %v, the pass", st.entry, want)
		}
	})
}

// TestWorldWithoutReconcileNothingIsAddedAgainAndARemovedServerIsAsBefore checks that a world with no SetReconcile
// never adds a removed server again, lists it left and unhealthy in the report for the report lag, and answers a call
// to its address.
func TestWorldWithoutReconcileNothingIsAddedAgainAndARemovedServerIsAsBefore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
		victim := b.followers(t)[0]
		b.removePeer(t, victim)

		wantServer(t, b.state(t, victim), "at the removal", false, "left", false, "alive")
		time.Sleep(10 * time.Minute)
		wantServer(t, b.state(t, victim), "ten minutes after", false, "", true, "alive")
		if _, err := b.clientAt(t, victim).Peers(t.Context()); err != nil {
			t.Errorf("Peers at the removed server: %v, want an answer", err)
		}
		if calls := b.w.RemovedCalls(); len(calls) != 0 {
			t.Errorf("RemovedCalls = %v, want none", calls)
		}
	})
}

// TestWorldReconcileSkipsAServerThatStartedAlone checks that three passes add nothing for a server created with
// bootstrap_expect = 1 while its member reads alive, and that a call to its address fails at once with an error that
// does not match ErrNotReady and reaches neither the hook nor the Nomad fake.
func TestWorldReconcileSkipsAServerThatStartedAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		added := b.createNode(t, "prod-servers-3", "server", b.nodeUserData(t, "prod-servers-3", 1))

		for range 3 {
			time.Sleep(time.Minute + time.Second)
			st := b.state(t, added)
			if st.peer != nil || st.entry != nil || st.member == nil || st.member.Status != "alive" {
				t.Errorf("peer %+v, entry %+v, member %+v; want the server out of the cluster with its member alive",
					st.peer, st.entry, st.member)
			}
		}

		hooked := 0
		b.w.SetHook(func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
			hooked++
			return next(ctx)
		})
		logged, before := len(b.w.Log()), time.Now()
		_, err := b.clientAt(t, added).Peers(t.Context())
		const alone = "prod-servers-3 started a cluster of its own"
		if err == nil || errors.Is(err, nomadops.ErrNotReady) || err.Error() != alone {
			t.Errorf("Peers at the server that started alone: %v, want the error that it started a cluster of its own", err)
		}
		if hooked != 0 || len(b.w.Log()) != logged || time.Since(before) != 0 {
			t.Errorf("the call reached the hook %d times and the fake %d times and took %v, want none and no time",
				hooked, len(b.w.Log())-logged, time.Since(before))
		}
	})
}

// TestWorldReconcileLeavesAServerThatWasForcedOutAlone checks that no pass adds a server that was halted and whose
// member a ForceLeave took.
func TestWorldReconcileLeavesAServerThatWasForcedOutAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 30*time.Second)
		b.haltInstance(t, victim)
		b.state(t, victim)
		sleepUntil(at, 35*time.Second)
		b.forceLeave(t, victim)

		for _, d := range []time.Duration{61 * time.Second, 121 * time.Second, 301 * time.Second} {
			sleepUntil(at, d)
			wantRaftPlace(t, b.state(t, victim), d.String()+" after the leadership", "absent")
		}
	})
}

// TestWorldReconcileLeavesAFailedServerThatWasRemovedByHandAlone checks that no pass adds a server whose member failed,
// when autopilot does not remove its peer and a RemovePeer did.
func TestWorldReconcileLeavesAFailedServerThatWasRemovedByHandAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t, (*nomadWorld).NoCleanup)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 5*time.Second)
		b.haltInstance(t, victim)
		b.state(t, victim)
		sleepUntil(at, 50*time.Second)
		b.removePeer(t, victim)

		for _, d := range []time.Duration{61 * time.Second, 121 * time.Second, 301 * time.Second} {
			sleepUntil(at, d)
			wantRaftPlace(t, b.state(t, victim), d.String()+" after the leadership", "absent")
		}
	})
}

// TestWorldReconcileJudgesAServerAsItWasAtThePass checks that a pass that the world works out at a later read adds the
// server as it was at the pass: one halted after the pass as a server that runs, which votes 10 s after the pass, and
// one whose member failed after the pass as a stopped server.
func TestWorldReconcileJudgesAServerAsItWasAtThePass(t *testing.T) {
	t.Parallel()
	t.Run("halted between the pass and the first read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			b := reconcileWorld(t)
			at, victim := b.startLeadership(t)
			sleepUntil(at, 25*time.Second)
			b.removePeer(t, victim)

			sleepUntil(at, 63*time.Second)
			b.haltInstance(t, victim)
			st := b.state(t, victim)
			wantRaftPlace(t, st, "63 s after, the first read since the pass", "nonvoter")
			if st.entry == nil || !st.entry.Healthy {
				t.Errorf("63 s after: the entry is %+v, want a healthy server, since it ran at the pass", st.entry)
			}
			sleepUntil(at, 70*time.Second)
			wantRaftPlace(t, b.state(t, victim), "70 s after", "voter")
		})
	})
	t.Run("member failed between the pass and the first read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			b := reconcileWorld(t)
			at, victim := b.startLeadership(t)
			sleepUntil(at, 25*time.Second)
			b.removePeer(t, victim)
			sleepUntil(at, 30*time.Second)
			b.haltInstance(t, victim)
			b.state(t, victim)

			sleepUntil(at, 71*time.Second)
			wantRaftPlace(t, b.state(t, victim), "71 s after, the first read since the pass", "nonvoter")
		})
	})
}

// TestWorldReconcileWorstCasePromotesAServerThatHalted checks that a server that was added again while it ran and
// halted 3 s later is a voter 10 s after the re-add while its member is alive, and a nonvoter for good when a
// ForceLeave took its member before.
func TestWorldReconcileWorstCasePromotesAServerThatHalted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		forcedAt time.Duration // when the ForceLeave comes; 0 for never
		want     string
	}{
		{"member alive", 0, "voter"},
		{"member forced out before the promotion", 66 * time.Second, "nonvoter"},
		{"member forced out after the promotion", 75 * time.Second, "voter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := reconcileWorld(t)
				at, victim := b.startLeadership(t)
				sleepUntil(at, 25*time.Second)
				b.removePeer(t, victim)
				sleepUntil(at, 60*time.Second)
				wantRaftPlace(t, b.state(t, victim), "60 s after", "nonvoter")

				sleepUntil(at, 63*time.Second)
				b.haltInstance(t, victim)
				b.state(t, victim)
				if tc.forcedAt > 0 && tc.forcedAt < 70*time.Second {
					sleepUntil(at, tc.forcedAt)
					b.forceLeave(t, victim)
				}
				sleepUntil(at, 69*time.Second)
				wantRaftPlace(t, b.state(t, victim), "69 s after", "nonvoter")
				sleepUntil(at, 70*time.Second)
				wantRaftPlace(t, b.state(t, victim), "70 s after", tc.want)
				if tc.forcedAt > 70*time.Second {
					sleepUntil(at, tc.forcedAt)
					b.forceLeave(t, victim)
				}
				sleepUntil(at, 80*time.Second)
				wantRaftPlace(t, b.state(t, victim), "80 s after", tc.want)
			})
		})
	}
}

// TestWorldReconcileDoesNotPromoteAServerWhoseMemberFailedFirst checks that a server that was added again while it ran
// and halted, and whose member failed before the promotion, stays a nonvoter.
func TestWorldReconcileDoesNotPromoteAServerWhoseMemberFailedFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t, func(w *nomadWorld) { w.SetFailAfter(5 * time.Second); w.NoCleanup() })
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 61*time.Second)
		b.haltInstance(t, victim)
		wantRaftPlace(t, b.state(t, victim), "61 s after", "nonvoter")

		sleepUntil(at, 66*time.Second)
		if st := b.state(t, victim); st.member == nil || st.member.Status != "failed" {
			t.Fatalf("66 s after: the member is %+v, want failed", st.member)
		}
		sleepUntil(at, 75*time.Second)
		wantRaftPlace(t, b.state(t, victim), "75 s after", "nonvoter")
	})
}

// TestWorldReconcileWaitsForTheNextPassWhenARemovalComesAtAPass checks that a peer removed in the very moment of a pass
// is added again at the next pass.
func TestWorldReconcileWaitsForTheNextPassWhenARemovalComesAtAPass(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 60*time.Second)
		b.removePeer(t, victim)

		sleepUntil(at, 119*time.Second)
		wantRaftPlace(t, b.state(t, victim), "119 s after", "absent")
		sleepUntil(at, 120*time.Second)
		wantRaftPlace(t, b.state(t, victim), "120 s after", "nonvoter")
	})
}

// TestWorldReconcileCountsItsMinuteFromTheElectionThatFollowsNoLeader checks that a removed server is not added again
// while the cluster has no leader, and not before a minute after the election that ends it, however long the removal
// was before.
func TestWorldReconcileCountsItsMinuteFromTheElectionThatFollowsNoLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		b.w.FailOnLeaderLoss(&failureTB{TB: t}) // the test halts the other two servers
		at, victim := b.startLeadership(t)
		sleepUntil(at, 5*time.Second)
		b.removePeer(t, victim)
		for _, s := range b.followers(t) {
			if s != victim {
				b.haltInstance(t, s)
			}
		}
		b.haltInstance(t, b.leader(t))
		_, noLeader := b.api.Peers(t.Context())
		sleepUntil(at, 300*time.Second)
		if _, err := b.api.Peers(t.Context()); noLeader == nil || err == nil {
			t.Fatalf("Peers answered %v right after the halts and %v 300 s after, want no leader both times", noLeader, err)
		}
		b.createNode(t, "prod-servers-3", "server", "")
		b.createNode(t, "prod-servers-4", "server", "")
		b.leader(t)
		elected := time.Now()

		sleepUntil(elected, 59*time.Second)
		wantRaftPlace(t, b.state(t, victim), "59 s after the election", "absent")
		sleepUntil(elected, 60*time.Second)
		wantRaftPlace(t, b.state(t, victim), "60 s after", "nonvoter")
	})
}

// TestWorldReconcileAddsAStoppedServerUnhealthyAndNeverPromotesIt checks that a server that was removed, halted, not
// forced out and added again within failAfter reads unhealthy with the cluster, never votes, and is removed by
// autopilot once its member failed, and not added again.
func TestWorldReconcileAddsAStoppedServerUnhealthyAndNeverPromotesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 30*time.Second)
		b.haltInstance(t, victim)
		b.state(t, victim)

		sleepUntil(at, 60*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "60 s after", "nonvoter")
		wantUnhealthyAlive(t, st, "60 s after")
		sleepUntil(at, 69*time.Second)
		st = b.state(t, victim)
		wantRaftPlace(t, st, "69 s after", "nonvoter")
		wantUnhealthyAlive(t, st, "69 s after")
		sleepUntil(at, 80*time.Second)
		wantRaftPlace(t, b.state(t, victim), "80 s after, failed and cleaned up", "absent")
		sleepUntil(at, 130*time.Second)
		wantRaftPlace(t, b.state(t, victim), "130 s after", "absent")
	})
}

// TestWorldHaltLagKeepsAHaltedMachineRunningForTheLag checks that with a halt lag of 9 s a halted server answers calls
// and has an alive, healthy member for 9 s, that failAfter counts from the end of the lag, and that with no lag the
// machine is gone at the halt.
func TestWorldHaltLagKeepsAHaltedMachineRunningForTheLag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lag  time.Duration
	}{{"lag of 9 s", 9 * time.Second}, {"no lag", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorldWith(t, func(w *nomadWorld) { w.ServersOverTime(); w.SetHaltLag(tc.lag) })
				follower := b.followers(t)[0]
				api := b.clientAt(t, follower)
				b.haltInstance(t, follower)
				at := time.Now()
				b.state(t, follower)

				for _, d := range []time.Duration{0, 8 * time.Second, 9 * time.Second} {
					sleepUntil(at, d)
					_, err := api.Peers(t.Context())
					answers := d < tc.lag
					if (err == nil) != answers || (err != nil && !errors.Is(err, nomadops.ErrNotReady)) {
						t.Errorf("%v after the halt: Peers at the halted server = %v; want an answer %v, else ErrNotReady",
							d, err, answers)
					}
				}
				sleepUntil(at, tc.lag+39*time.Second)
				wantServer(t, b.state(t, follower), "39 s after the lag", true, "alive", true, "alive")
				sleepUntil(at, tc.lag+40*time.Second)
				wantServer(t, b.state(t, follower), "40 s after the lag", true, "left", false, "failed")
			})
		})
	}
}

// TestWorldHaltLagLetsAPassAddAServerThatRuns checks that a pass of the reconcile inside the lag adds a halted server
// as one that runs, which then votes 10 s later, and that a pass at the end of the lag adds it as a stopped server,
// unhealthy and never voting.
func TestWorldHaltLagLetsAPassAddAServerThatRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		haltAt  time.Duration
		healthy bool
		voter   string
	}{
		{"pass inside the lag", 55 * time.Second, true, "voter"},
		{"pass at the end of the lag", 51 * time.Second, false, "nonvoter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := reconcileWorld(t, func(w *nomadWorld) { w.SetHaltLag(9 * time.Second) })
				at, victim := b.startLeadership(t)
				sleepUntil(at, 25*time.Second)
				b.removePeer(t, victim)
				sleepUntil(at, tc.haltAt)
				b.haltInstance(t, victim)
				b.state(t, victim)

				sleepUntil(at, 61*time.Second)
				st := b.state(t, victim)
				wantRaftPlace(t, st, "61 s after", "nonvoter")
				if st.entry == nil || st.entry.Healthy != tc.healthy {
					t.Errorf("61 s after: the entry is %+v, want healthy %v", st.entry, tc.healthy)
				}
				sleepUntil(at, 70*time.Second)
				wantRaftPlace(t, b.state(t, victim), "70 s after", tc.voter)
			})
		})
	}
}

// TestWorldReconcileReportListsARemovedLiveServerAliveForTheReportLag checks that the report lists a removed server
// that runs as alive and healthy for the report lag, with the cluster healthy, and then leaves it out; and that a
// failed server that autopilot removed reads left and unhealthy as before.
func TestWorldReconcileReportListsARemovedLiveServerAliveForTheReportLag(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, live := b.startLeadership(t)
		var failed worldServer
		for _, s := range b.followers(t) {
			if s != live {
				failed = s
			}
		}
		sleepUntil(at, 5*time.Second)
		b.haltInstance(t, failed)
		b.state(t, failed)
		sleepUntil(at, 10*time.Second)
		b.removePeer(t, live)

		wantServer(t, b.state(t, live), "at the removal", false, "alive", true, "alive")
		sleepUntil(at, 11*time.Second+999*time.Millisecond)
		wantServer(t, b.state(t, live), "just before the end of the lag", false, "alive", true, "alive")
		sleepUntil(at, 12*time.Second)
		wantServer(t, b.state(t, live), "2 s after", false, "", true, "alive")
		sleepUntil(at, 48*time.Second)
		wantServer(t, b.state(t, failed), "the failed server, 1 s after autopilot removed it", false, "left", false, "failed")
		sleepUntil(at, 49*time.Second)
		wantServer(t, b.state(t, failed), "the failed server, 2 s after", false, "", true, "failed")
	})
}

// TestWorldCallToARemovedLiveServerStopsWaitingWhenItsContextEnds checks that the 5 s that a call to a removed server
// takes end with the call's context.
func TestWorldCallToARemovedLiveServerStopsWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		_, victim := b.startLeadership(t)
		b.removePeer(t, victim)
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		started := time.Now()

		_, err := b.clientAt(t, victim).Peers(ctx)
		took := time.Since(started)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nomadops.ErrNotReady) || took != time.Second {
			t.Errorf("Peers with a context of 1 s: %v after %v, want the context's error after 1 s", err, took)
		}
	})
}

// TestWorldRemovedLiveServerFailsItsCallsAfterTwoSecondsWithNoLeader checks that a call to the address of a running
// server that is no peer answers 1 s after its removal and, from 2 s on, takes 5 s and fails with ErrNotReady; that the
// call shows in RemovedCalls and reaches neither the hook nor the Nomad fake; and that Members and a call to another
// server are not held.
func TestWorldRemovedLiveServerFailsItsCallsAfterTwoSecondsWithNoLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		lead := b.leader(t)
		hooked := 0
		b.w.SetHook(func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
			hooked++
			return next(ctx)
		})
		removed, leader := b.clientAt(t, victim), b.clientAt(t, lead)
		sleepUntil(at, 10*time.Second)
		b.removePeer(t, victim)
		removedAt := time.Now()
		hooked = 0

		time.Sleep(time.Second)
		if _, err := removed.Peers(t.Context()); err != nil || hooked != 1 {
			t.Errorf("Peers 1 s after the removal: %v with %d calls at the hook, want an answer through the hook", err, hooked)
		}
		sleepUntil(removedAt, 2*time.Second)
		logged, started := len(b.w.Log()), time.Now()
		_, err := removed.Peers(t.Context())
		if !errors.Is(err, nomadops.ErrNotReady) || !strings.Contains(err.Error(), "No cluster leader") {
			t.Errorf("Peers 2 s after the removal: %v, want ErrNotReady with No cluster leader", err)
		}
		if took := time.Since(started); took != 5*time.Second {
			t.Errorf("the call took %v, want 5 s", took)
		}
		if hooked != 1 || len(b.w.Log()) != logged {
			t.Errorf("the call reached the hook %d times in total and the fake %d times, want the first call's one and none",
				hooked, len(b.w.Log())-logged)
		}
		want := []nomadfake.Call{{Name: "Peers", Server: victim.address + ":4646"}}
		if got := b.w.RemovedCalls(); !slices.Equal(got, want) {
			t.Errorf("RemovedCalls = %v, want %v", got, want)
		}

		started = time.Now()
		if _, err := removed.Members(t.Context()); err != nil {
			t.Errorf("Members at the removed server: %v, want an answer", err)
		}
		if _, err := leader.Peers(t.Context()); err != nil {
			t.Errorf("Peers at the leader: %v, want an answer", err)
		}
		if took := time.Since(started); took != 0 || len(b.w.RemovedCalls()) != 1 {
			t.Errorf("Members and the call to the leader took %v and RemovedCalls has %d calls, want no time and one",
				took, len(b.w.RemovedCalls()))
		}
	})
}

// TestWorldReconcileNeverPromotesAStoppedServerThatVotedBefore checks that a server that the leader added and autopilot
// promoted, and that was removed again and halted, comes back at the next pass unhealthy and does not vote.
func TestWorldReconcileNeverPromotesAStoppedServerThatVotedBefore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 70*time.Second)
		wantRaftPlace(t, b.state(t, victim), "70 s after", "voter")

		sleepUntil(at, 80*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 85*time.Second)
		b.haltInstance(t, victim)
		b.state(t, victim)
		sleepUntil(at, 120*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "120 s after", "nonvoter")
		wantUnhealthyAlive(t, st, "120 s after")
		sleepUntil(at, 124*time.Second)
		wantRaftPlace(t, b.state(t, victim), "124 s after", "nonvoter")
	})
}

// TestWorldReconcileKeepsTheVoteOfAPromotedServerWhoseMemberFailsLater checks that a server that halted and was
// promoted still votes once its member has failed, until autopilot removes its peer.
func TestWorldReconcileKeepsTheVoteOfAPromotedServerWhoseMemberFailsLater(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		at, victim := b.startLeadership(t)
		sleepUntil(at, 25*time.Second)
		b.removePeer(t, victim)
		sleepUntil(at, 60*time.Second)
		wantRaftPlace(t, b.state(t, victim), "60 s after", "nonvoter")
		sleepUntil(at, 63*time.Second)
		b.haltInstance(t, victim)
		b.state(t, victim)

		sleepUntil(at, 104*time.Second)
		st := b.state(t, victim)
		wantRaftPlace(t, st, "104 s after, 1 s after its member failed", "voter")
		if st.member == nil || st.member.Status != "failed" {
			t.Errorf("104 s after: the member is %+v, want failed", st.member)
		}
		sleepUntil(at, 105*time.Second)
		wantRaftPlace(t, b.state(t, victim), "105 s after, removed by autopilot", "absent")
	})
}

// TestWorldCallToARemovedServerThatHaltedIsRefusedAtOnce checks that a call to the address of a removed server whose
// machine is halted fails at once as a call to a halted server does, and shows in HaltedCalls and not in RemovedCalls.
func TestWorldCallToARemovedServerThatHaltedIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := reconcileWorld(t)
		_, victim := b.startLeadership(t)
		b.removePeer(t, victim)
		b.haltInstance(t, victim)
		b.state(t, victim)
		time.Sleep(3 * time.Second)
		started := time.Now()

		_, err := b.clientAt(t, victim).Peers(t.Context())
		if !errors.Is(err, nomadops.ErrNotReady) || time.Since(started) != 0 {
			t.Errorf("Peers at the halted server: %v after %v, want ErrNotReady at once", err, time.Since(started))
		}
		if halted, removed := b.w.HaltedCalls(), b.w.RemovedCalls(); len(halted) != 1 || len(removed) != 0 {
			t.Errorf("HaltedCalls = %v and RemovedCalls = %v, want the call among the first only", halted, removed)
		}
	})
}

// TestWorldServerThatStartedAloneFailsItsCallsWithTheReconcileOff checks that a call to the address of a server that
// started a cluster of its own fails with the error that does not match ErrNotReady in a world with no SetReconcile.
func TestWorldServerThatStartedAloneFailsItsCallsWithTheReconcileOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		added := b.createNode(t, "prod-servers-3", "server", b.nodeUserData(t, "prod-servers-3", 1))

		_, err := b.clientAt(t, added).Peers(t.Context())
		const alone = "prod-servers-3 started a cluster of its own"
		if err == nil || errors.Is(err, nomadops.ErrNotReady) || err.Error() != alone {
			t.Errorf("Peers at the server that started alone: %v, want %q", err, alone)
		}
	})
}
