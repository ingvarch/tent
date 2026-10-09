package app_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nomadops"
)

// wantStable fails the test unless every entry of the report has the stable time want.
func wantStable(t *testing.T, health nomadops.Health, want time.Time) {
	t.Helper()
	if len(health.Servers) == 0 {
		t.Fatal("the report has no server")
	}
	for _, e := range health.Servers {
		if !e.StableSince.Equal(want) {
			t.Errorf("StableSince of %s = %v, want %v", e.Name, e.StableSince, want)
		}
	}
}

// TestWorldTransferMovesTheLeaderForGoodAndSetsEveryStableTime checks that a TransferLeadership through a client of
// the world keeps the new leader at later calls, and sets the stable time of every server to the time of the
// transfer, cut to whole seconds.
func TestWorldTransferMovesTheLeaderForGoodAndSetsEveryStableTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		target := b.followers(t)[1]
		atHalfSecond()
		at := time.Now()

		if err := b.api.TransferLeadership(t.Context(), target.raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}
		for range 2 {
			if got := b.leader(t); got != target {
				t.Errorf("the leader is %s, want %s", got, target)
			}
			wantStable(t, b.health(t), at.Truncate(time.Second))
			time.Sleep(time.Minute)
		}
		leaders := 0
		for _, e := range b.health(t).Servers {
			if e.Leader {
				leaders++
			}
		}
		if leaders != 1 {
			t.Errorf("%d entries of the report lead, want 1", leaders)
		}
	})
}

// TestWorldTransferToTheLeaderChangesNothing checks that a transfer to the server that leads keeps the leader, the
// stable times and the blip as they are.
func TestWorldTransferToTheLeaderChangesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetTransferBlip(time.Minute)
		lead, before := b.leader(t), b.health(t)
		time.Sleep(5 * time.Second)

		if err := b.api.TransferLeadership(t.Context(), lead.raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}
		after := b.health(t)
		if b.leader(t) != lead || !after.Healthy {
			t.Errorf("the leader is %s in a cluster that is healthy %v, want %s and healthy", b.leader(t), after.Healthy, lead)
		}
		for i, e := range after.Servers {
			if !e.StableSince.Equal(before.Servers[i].StableSince) {
				t.Errorf("StableSince of %s moved from %v to %v", e.Name, before.Servers[i].StableSince, e.StableSince)
			}
		}
	})
}

// TestWorldTransferToANonvoterGivesTheLeadershipToTheFirstOtherVoter checks that a transfer to a server that does not
// vote yet makes the first voter that does not lead the leader, as the Nomad fake does.
func TestWorldTransferToANonvoterGivesTheLeadershipToTheFirstOtherVoter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetVoteAfter(time.Hour)
		added := b.addServer(t, "prod-servers-3")
		want := b.followers(t)[0]

		if err := b.api.TransferLeadership(t.Context(), added.raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}
		if got := b.leader(t); got != want {
			t.Errorf("the leader is %s, want the first voter other than the old leader, %s", got, want)
		}
	})
}

// TestWorldTransferBlipMakesOneFollowerUnhealthyForItsDuration checks that after a transfer with a blip one follower
// reads unhealthy, with the failure tolerance that the formula gives, until the blip ends, and that the stable times
// stay as the transfer set them.
func TestWorldTransferBlipMakesOneFollowerUnhealthyForItsDuration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetTransferBlip(2 * time.Second)
		target := b.followers(t)[0]
		atHalfSecond()
		at := time.Now()
		if err := b.api.TransferLeadership(t.Context(), target.raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}

		for _, tc := range []struct {
			wait      time.Duration
			unhealthy int
			tolerance int
		}{{0, 1, 0}, {time.Second, 1, 0}, {time.Second, 0, 1}} {
			time.Sleep(tc.wait)
			health := b.health(t)
			var unhealthy []string
			for _, e := range health.Servers {
				if !e.Healthy {
					if e.Leader || e.Serf != "alive" {
						t.Errorf("after %v: the unhealthy entry %+v is the leader or not alive", tc.wait, e)
					}
					unhealthy = append(unhealthy, e.ID)
				}
			}
			wantHealthy := tc.unhealthy == 0
			if len(unhealthy) != tc.unhealthy || health.Healthy != wantHealthy || health.FailureTolerance != tc.tolerance {
				t.Errorf("after %v: unhealthy %v, cluster healthy %v, tolerance %d; want %d unhealthy, healthy %v, tolerance %d",
					tc.wait, unhealthy, health.Healthy, health.FailureTolerance, tc.unhealthy, wantHealthy, tc.tolerance)
			}
			wantStable(t, health, at.Truncate(time.Second))
		}
	})
}

// TestWorldTransferWithoutABlipLeavesTheClusterHealthy checks that without SetTransferBlip a transfer is not followed
// by an unhealthy report.
func TestWorldTransferWithoutABlipLeavesTheClusterHealthy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		if err := b.api.TransferLeadership(t.Context(), b.followers(t)[0].raftID()); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}
		if health := b.health(t); !health.Healthy || health.FailureTolerance != 1 {
			t.Errorf("after the transfer: healthy %v, tolerance %d, want healthy and 1", health.Healthy, health.FailureTolerance)
		}
	})
}

// TestWorldIgnoresAWriteThatFailed checks that a TransferLeadership, a RemovePeer and a ForceLeave that the Nomad fake
// fails change nothing in the world's servers.
func TestWorldIgnoresAWriteThatFailed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		follower := b.followers(t)[0]
		lead := b.leader(t)
		errFault := errors.New("the call failed")
		writes := map[string]func() error{
			"TransferLeadership": func() error { return b.api.TransferLeadership(t.Context(), follower.raftID()) },
			"RemovePeer":         func() error { return b.api.RemovePeer(t.Context(), follower.raftID()) },
			"ForceLeave": func() error {
				return b.api.ForceLeave(t.Context(), follower.member())
			},
		}
		for _, name := range []string{"TransferLeadership", "RemovePeer", "ForceLeave"} {
			b.w.Fail(t, name, errFault)
			if err := writes[name](); !errors.Is(err, errFault) {
				t.Fatalf("%s error = %v, want the fault", name, err)
			}
		}

		st := b.state(t, follower)
		if st.peer == nil || st.entry == nil || st.member == nil || st.member.Status != "alive" || b.leader(t) != lead {
			t.Errorf("after the failed writes: peer %+v, entry %+v, member %+v, leader %s; want all as before, led by %s",
				st.peer, st.entry, st.member, b.leader(t), lead)
		}
	})
}

// TestWorldFailsTheTestWhenTheLeaderIsHaltedOrGone checks that the machine of the leader that halts or is deleted
// fails the test once, and that the cluster goes on with a running voter as its leader; and that the machine of a
// follower does not fail it.
func TestWorldFailsTheTestWhenTheLeaderIsHaltedOrGone(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]func(*testing.T, liveWorld, worldServer){
		"halted":  func(t *testing.T, b liveWorld, s worldServer) { b.haltInstance(t, s) },
		"deleted": func(t *testing.T, b liveWorld, s worldServer) { b.deleteInstance(t, s) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorld(t)
				rec := &failureTB{TB: t}
				b.w.FailOnLeaderLoss(rec)
				b.w.SetFailAfter(time.Minute) // the stopped follower stays a voting peer, which the election skips
				followers, lead := b.followers(t), b.leader(t)

				stop(t, b, followers[0])
				b.health(t)
				if got := rec.failures(); len(got) != 0 {
					t.Fatalf("a follower's machine stopped and the world failed the test: %v", got)
				}

				stop(t, b, lead)
				for range 2 {
					b.health(t)
				}
				got := rec.failures()
				if len(got) != 1 || !strings.Contains(got[0], lead.name) {
					t.Fatalf("the world failed the test with %v, want one failure that names the leader", got)
				}
				if now := b.leader(t); now != followers[1] {
					t.Errorf("the leader is %s, want the running voter %s", now, followers[1])
				}
			})
		})
	}
}

// TestWorldChangeServerCanMoveTheStableTimeAtEachAnswer checks that an edit that sets the stable time to the present
// gives a time that moves with each answer, as a window that never ends, and leaves the other servers' times alone.
func TestWorldChangeServerCanMoveTheStableTimeAtEachAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.ChangeServer("prod-servers-0", func(s *nomadops.ServerHealth) { s.StableSince = time.Now() })
		var reads []map[string]time.Time
		for range 2 {
			read := map[string]time.Time{}
			for _, e := range b.health(t).Servers {
				read[e.Name] = e.StableSince
			}
			reads = append(reads, read)
			time.Sleep(3 * time.Second)
		}
		for name, first := range reads[0] {
			want := first
			if name == "prod-servers-0.global" {
				want = first.Add(3 * time.Second)
			}
			if got := reads[1][name]; !got.Equal(want) {
				t.Errorf("StableSince of %s moved from %v to %v, want %v", name, first, got, want)
			}
		}
	})
}

// TestWorldWritesForAServerItDoesNotKnowChangeNothing checks that the records of a transfer, a removal and a
// force-leave of a server that is not in the Raft configuration, and the removal of the leader, which the fake
// refuses, leave the leader, the peers and the members as they are.
func TestWorldWritesForAServerItDoesNotKnowChangeNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		lead := b.leader(t)
		before, member := b.peers(t), b.state(t, lead).member
		if member == nil {
			t.Fatal("the leader has no member")
		}

		b.w.transferred("r-nobody")
		b.w.peerRemoved("r-nobody")
		b.w.peerRemoved(lead.raftID())
		b.w.memberForced("nobody.global")

		if got := b.peers(t); !slices.Equal(before, got) {
			t.Errorf("the peers changed from %v to %v", before, got)
		}
		if got := b.state(t, lead).member; got == nil || *got != *member {
			t.Errorf("the leader's member changed from %+v to %+v", member, got)
		}
	})
}
