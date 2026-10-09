package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// wantServer fails the test unless the server shows as want says: whether it is a peer, whether the report lists it
// with its Serf status, whether the cluster is healthy, and the status of its member ("" for no member).
func wantServer(t *testing.T, st serverState, when string, peer bool, serf string, healthy bool, member string) {
	t.Helper()
	gotSerf, gotMember := "", ""
	if st.entry != nil {
		gotSerf = st.entry.Serf
		if (serf == "alive") != st.entry.Healthy {
			t.Errorf("%s: the entry %+v is healthy %v, want %v", when, st.entry, st.entry.Healthy, serf == "alive")
		}
	}
	if st.member != nil {
		gotMember = st.member.Status
	}
	if (st.peer != nil) != peer || gotSerf != serf || st.health.Healthy != healthy || gotMember != member {
		t.Errorf("%s: peer %v, serf %q, cluster healthy %v, member %q; want %v, %q, %v, %q", when, st.peer != nil, gotSerf,
			st.health.Healthy, gotMember, peer, serf, healthy, member)
	}
}

// TestWorldStoppedServerFailsThenIsCleanedUpThenForgotten checks the time line of a follower whose machine halts or
// is deleted, with ServersOverTime: alive and healthy for 40 s, failed and unhealthy then, its peer removed by
// autopilot 2 s later, and the report keeping it 2 s more, while the cluster is unhealthy; its member stays failed.
func TestWorldStoppedServerFailsThenIsCleanedUpThenForgotten(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]func(*testing.T, liveWorld, worldServer){
		"halted":  func(t *testing.T, b liveWorld, s worldServer) { b.haltInstance(t, s) },
		"deleted": func(t *testing.T, b liveWorld, s worldServer) { b.deleteInstance(t, s) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
				follower := b.followers(t)[0]
				stoppedAt := time.Now()
				stop(t, b, follower)

				wantServer(t, b.state(t, follower), "at the halt", true, "alive", true, "alive")
				time.Sleep(39 * time.Second)
				wantServer(t, b.state(t, follower), "39 s after", true, "alive", true, "alive")
				time.Sleep(time.Second)
				st := b.state(t, follower)
				wantServer(t, st, "40 s after", true, "left", false, "failed")
				if st.health.FailureTolerance != 0 || st.health.Voters != 3 {
					t.Errorf("40 s after: tolerance %d with %d voters, want 0 with 3", st.health.FailureTolerance, st.health.Voters)
				}
				if want := stoppedAt.Add(40 * time.Second).Truncate(time.Second); !st.entry.StableSince.Equal(want) {
					t.Errorf("40 s after: StableSince = %v, want the time it failed, %v", st.entry.StableSince, want)
				}
				time.Sleep(time.Second)
				wantServer(t, b.state(t, follower), "41 s after", true, "left", false, "failed")
				time.Sleep(time.Second)
				st = b.state(t, follower)
				wantServer(t, st, "42 s after", false, "left", false, "failed")
				if st.health.Voters != 2 {
					t.Errorf("42 s after: %d voters, want 2", st.health.Voters)
				}
				time.Sleep(time.Second)
				wantServer(t, b.state(t, follower), "43 s after", false, "left", false, "failed")
				time.Sleep(time.Second)
				wantServer(t, b.state(t, follower), "44 s after", false, "", true, "failed")
			})
		})
	}
}

// TestWorldSetFailAfterMovesTheDelay checks that SetFailAfter sets how long a stopped server stays alive, so that a
// delay beyond the deadline of the wait for a stopped server keeps it alive and healthy, until its peer is a failed
// voter that nothing removes.
func TestWorldSetFailAfterMovesTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetFailAfter(6 * time.Minute)
		b.w.NoCleanup()
		follower := b.followers(t)[0]
		b.haltInstance(t, follower)

		wantServer(t, b.state(t, follower), "at the halt", true, "alive", true, "alive")
		time.Sleep(5*time.Minute + 59*time.Second)
		wantServer(t, b.state(t, follower), "5 min 59 s after", true, "alive", true, "alive")
		time.Sleep(time.Second)
		wantServer(t, b.state(t, follower), "6 min after", true, "left", false, "failed")
	})
}

// TestWorldSetCleanupAfterMovesTheDelay checks that SetCleanupAfter sets how long after a server failed autopilot
// removes its peer.
func TestWorldSetCleanupAfterMovesTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetFailAfter(5 * time.Second)
		b.w.SetCleanupAfter(10 * time.Second)
		b.w.SetReportLag(time.Hour)
		follower := b.followers(t)[0]
		b.haltInstance(t, follower)
		b.state(t, follower)

		time.Sleep(14 * time.Second)
		wantServer(t, b.state(t, follower), "14 s after", true, "left", false, "failed")
		time.Sleep(time.Second)
		wantServer(t, b.state(t, follower), "15 s after", false, "left", false, "failed")
	})
}

// TestWorldNoCleanupKeepsThePeerOfAFailedServer checks that with NoCleanup autopilot never removes the peer of a failed
// server, which counts as a voter that is not healthy, until a RemovePeer removes it.
func TestWorldNoCleanupKeepsThePeerOfAFailedServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
		b.w.NoCleanup()
		follower := b.followers(t)[0]
		b.haltInstance(t, follower)
		b.state(t, follower)

		time.Sleep(time.Hour)
		wantServer(t, b.state(t, follower), "an hour after", true, "left", false, "failed")
		if err := b.api.RemovePeer(t.Context(), follower.raftID()); err != nil {
			t.Fatalf("RemovePeer: %v", err)
		}
		wantServer(t, b.state(t, follower), "after the RemovePeer", false, "left", false, "failed")
	})
}

// TestWorldSetReportLagMovesTheDelay checks that SetReportLag sets how long the report keeps a peer that RemovePeer
// removed, as left and unhealthy, and that the cluster is unhealthy meanwhile.
func TestWorldSetReportLagMovesTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetReportLag(7 * time.Second)
		follower := b.followers(t)[0]
		if err := b.api.RemovePeer(t.Context(), follower.raftID()); err != nil {
			t.Fatalf("RemovePeer: %v", err)
		}

		wantServer(t, b.state(t, follower), "at the removal", false, "left", false, "alive")
		time.Sleep(6 * time.Second)
		wantServer(t, b.state(t, follower), "6 s after", false, "left", false, "alive")
		time.Sleep(time.Second)
		wantServer(t, b.state(t, follower), "7 s after", false, "", true, "alive")
	})
}

// TestWorldFailureToleranceFollowsTheQuorum checks that the failure tolerance is the healthy voters beyond a quorum, at
// 3, 4 and 5 voters, and with one voter that failed.
func TestWorldFailureToleranceFollowsTheQuorum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		voters, failed, want int
	}{{3, 0, 1}, {4, 0, 1}, {5, 0, 2}, {3, 1, 0}, {4, 1, 0}, {5, 1, 1}, {5, 2, 0}, {3, 2, 0}} {
		t.Run(fmt.Sprintf("%d voters, %d failed", tc.voters, tc.failed), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorld(t)
				b.w.NoCleanup()
				for i := 3; i < tc.voters; i++ {
					b.addServer(t, fmt.Sprintf("prod-servers-%d", i))
				}
				for _, s := range b.followers(t)[:tc.failed] {
					b.haltInstance(t, s)
				}
				health := b.health(t)
				if health.Voters != tc.voters || health.FailureTolerance != tc.want {
					t.Errorf("%d voters, %d with the machine halted: %d voters and tolerance %d, want %d voters and tolerance %d",
						tc.voters, tc.failed, health.Voters, health.FailureTolerance, tc.voters, tc.want)
				}
			})
		})
	}
}

// TestWorldRemovePeerStaysAndTheLeaderCannotBeRemoved checks that a RemovePeer through a client of the world keeps the
// peer out at later calls, and that the fake refuses to remove the leader, which changes nothing.
func TestWorldRemovePeerStaysAndTheLeaderCannotBeRemoved(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		follower, lead := b.followers(t)[0], b.leader(t)

		if err := b.api.RemovePeer(t.Context(), lead.raftID()); err == nil {
			t.Error("RemovePeer of the leader succeeded, want the fake's refusal")
		}
		if err := b.api.RemovePeer(t.Context(), follower.raftID()); err != nil {
			t.Fatalf("RemovePeer: %v", err)
		}
		for range 2 {
			if st := b.state(t, follower); st.peer != nil || len(b.peers(t)) != 2 || b.leader(t) != lead {
				t.Errorf("after the removal: peer %+v, %d peers, leader %s; want the follower out, two peers, led by %s", st.peer,
					len(b.peers(t)), b.leader(t), lead)
			}
			time.Sleep(time.Minute)
		}
	})
}

// TestWorldForceLeaveOfAFailedMemberDropsItForGood checks that a ForceLeave through a client of the world drops a
// failed member at once, and that it does not come back while its machine is halted.
func TestWorldForceLeaveOfAFailedMemberDropsItForGood(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		follower := b.followers(t)[0]
		b.haltInstance(t, follower)
		if st := b.state(t, follower); st.member == nil || st.member.Status != "failed" {
			t.Fatalf("the member of the halted server is %+v, want failed", st.member)
		}

		if err := b.api.ForceLeave(t.Context(), follower.member()); err != nil {
			t.Fatalf("ForceLeave: %v", err)
		}
		for range 3 {
			if st := b.state(t, follower); st.member != nil {
				t.Errorf("the dropped member is listed again: %+v", st.member)
			}
			time.Sleep(time.Minute)
		}
	})
}

// TestWorldForceLeaveOfAnAliveMemberLeavesAtTheNextReadAndIsGoneAtTheOneAfter checks that the member of a running
// server that a ForceLeave turned to leaving shows so at the next read of Members, is not listed at the read after,
// and does not come back.
func TestWorldForceLeaveOfAnAliveMemberLeavesAtTheNextReadAndIsGoneAtTheOneAfter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		follower := b.followers(t)[0]
		if err := b.api.ForceLeave(t.Context(), follower.member()); err != nil {
			t.Fatalf("ForceLeave: %v", err)
		}
		for read, want := range []string{"leaving", "", "", ""} {
			got := ""
			if m := b.state(t, follower).member; m != nil {
				got = m.Status
			}
			if got != want {
				t.Errorf("read %d: the member's status is %q, want %q", read+1, got, want)
			}
			time.Sleep(time.Minute)
		}
	})
}

// TestWorldCallToAHaltedOrGoneServerFailsAtOnce checks that a call to the public address of a server whose machine is
// halted or deleted fails with ErrNotReady at once, even while the world still counts the server alive and while the
// Vultr fake still reads it running; that the call shows in HaltedCalls and reaches neither the hook nor the Nomad
// fake; and that a call to a server that runs goes through.
func TestWorldCallToAHaltedOrGoneServerFailsAtOnce(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]func(*testing.T, liveWorld, worldServer){
		"halted": func(t *testing.T, b liveWorld, s worldServer) { b.haltInstance(t, s) },
		"halted and still reading running": func(t *testing.T, b liveWorld, s worldServer) {
			b.f.SetHaltReads(t, 100)
			b.haltInstance(t, s)
		},
		"deleted": func(t *testing.T, b liveWorld, s worldServer) { b.deleteInstance(t, s) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
				follower, lead := b.followers(t)[0], b.leader(t)
				stoppedAt, leadAt := follower.address+":4646", lead.address+":4646"
				hooked := 0
				b.w.SetHook(func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
					hooked++
					return next(ctx)
				})
				stopped, err := b.w.svc.Nomad(nomadops.Config{Address: stoppedAt})
				if err != nil {
					t.Fatalf("Nomad: %v", err)
				}
				alive, err := b.w.svc.Nomad(nomadops.Config{Address: leadAt})
				if err != nil {
					t.Fatalf("Nomad: %v", err)
				}
				stop(t, b, follower)
				logged := len(b.w.Log())

				if _, err := stopped.Peers(t.Context()); !errors.Is(err, nomadops.ErrNotReady) {
					t.Errorf("Peers at the stopped server: %v, want ErrNotReady", err)
				}
				if err := stopped.RemovePeer(t.Context(), "r-x"); !errors.Is(err, nomadops.ErrNotReady) {
					t.Errorf("RemovePeer at the stopped server: %v, want ErrNotReady", err)
				}
				if hooked != 0 || len(b.w.Log()) != logged {
					t.Errorf("the calls to the stopped server reached the hook %d times and the Nomad fake %d times", hooked,
						len(b.w.Log())-logged)
				}
				want := []nomadfake.Call{{Name: "Peers", Server: stoppedAt}, {Name: "RemovePeer", Server: stoppedAt, Arg: "r-x"}}
				if got := b.w.HaltedCalls(); !slices.Equal(got, want) {
					t.Errorf("HaltedCalls = %v, want %v", got, want)
				}

				if _, err := alive.Peers(t.Context()); err != nil {
					t.Errorf("Peers at the server that runs: %v", err)
				}
				if hooked != 1 || len(b.w.HaltedCalls()) != 2 {
					t.Errorf("the call to the running server reached the hook %d times, HaltedCalls has %d, want 1 and 2", hooked,
						len(b.w.HaltedCalls()))
				}
			})
		})
	}
}
