package nomadfake_test

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// quorum is a cluster of four servers that the Raft calls work on: s1 leads, s2 does not vote, s3 and s4 vote.
func quorum(t *testing.T) (*nomadfake.Fake, nomadops.API) {
	t.Helper()
	f, a := newBootstrappedAPI(t)
	f.SetLeader("10.0.0.1:4647")
	f.SetPeers([]nomadops.Peer{
		{ID: "p-1", Name: "s1", Address: netip.MustParseAddrPort("10.0.0.1:4647"), Voter: true, Leader: true},
		{ID: "p-2", Name: "s2", Address: netip.MustParseAddrPort("10.0.0.2:4647")},
		{ID: "p-3", Name: "s3", Address: netip.MustParseAddrPort("10.0.0.3:4647"), Voter: true},
		{ID: "p-4", Name: "s4", Address: netip.MustParseAddrPort("10.0.0.4:4647"), Voter: true},
	})
	f.SetHealth(nomadops.Health{Healthy: true, Voters: 3, FailureTolerance: 1, Servers: []nomadops.ServerHealth{
		{ID: "p-1", Name: "s1.eu", Healthy: true, Voter: true, Leader: true, StableSince: stableOnce},
		{ID: "p-2", Name: "s2.eu", Healthy: true, StableSince: stableOnce},
		{ID: "p-3", Name: "s3.eu", Healthy: true, Voter: true, StableSince: stableOnce},
		{ID: "p-4", Name: "s4.eu", Healthy: true, Voter: true, StableSince: stableOnce},
	}})
	return f, a
}

// stableOnce is when autopilot saw the servers of quorum change last.
var stableOnce = time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)

// raftState is what a test compares: who leads by the peers, by the report and by the leader's address, and which
// peers exist.
type raftState struct {
	Peers        []string // the Raft IDs, with a "*" after the leader's
	HealthLeader string   // the Raft ID that the report shows as leader
	Leader       string   // the RPC address
	Stable       []time.Time
}

// stateOf reads the cluster through a.
func stateOf(t *testing.T, a nomadops.API) raftState {
	t.Helper()
	var s raftState
	peers, err := a.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	for _, p := range peers {
		if p.Leader {
			s.Peers = append(s.Peers, p.ID+"*")
		} else {
			s.Peers = append(s.Peers, p.ID)
		}
	}
	h, err := a.Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for _, sv := range h.Servers {
		if sv.Leader {
			s.HealthLeader += sv.ID
		}
		s.Stable = append(s.Stable, sv.StableSince)
	}
	if s.Leader, err = a.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	return s
}

// stableAll is n times at.
func stableAll(at time.Time, n int) []time.Time {
	return slices.Repeat([]time.Time{at}, n)
}

// TestTransferLeadershipToAVoter checks that the leadership moves to the voter in the peers, in the report and in the
// leader's address, that every server's stable time becomes the time of the call, and that the rest of the report
// stays.
func TestTransferLeadershipToAVoter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := quorum(t)
		time.Sleep(time.Hour)

		if err := a.TransferLeadership(t.Context(), "p-4"); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}

		want := raftState{Peers: []string{"p-1", "p-2", "p-3", "p-4*"}, HealthLeader: "p-4", Leader: "10.0.0.4:4647",
			Stable: stableAll(time.Now(), 4)}
		if diff := cmp.Diff(want, stateOf(t, a)); diff != "" {
			t.Errorf("cluster (-want +got):\n%s", diff)
		}
		h, _ := a.Health(t.Context())
		if !h.Healthy || h.Voters != 3 || h.FailureTolerance != 1 || len(h.Servers) != 4 || h.Servers[0].Leader {
			t.Errorf("Health() = %+v, want the report as set, with p-1 no longer leading", h)
		}
		wantCalls(t, f, bootstrapCall, nomadfake.Call{Name: "TransferLeadership", Arg: "p-4"},
			nomadfake.Call{Name: "Peers"}, nomadfake.Call{Name: "Health"}, nomadfake.Call{Name: "Leader"},
			nomadfake.Call{Name: "Health"})
	})
}

// TestTransferLeadershipToTheLeaderChangesNothing checks Nomad's Noop: the leader keeps the leadership and the stable
// times stay.
func TestTransferLeadershipToTheLeaderChangesNothing(t *testing.T) {
	_, a := quorum(t)

	if err := a.TransferLeadership(t.Context(), "p-1"); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}

	want := raftState{Peers: []string{"p-1*", "p-2", "p-3", "p-4"}, HealthLeader: "p-1", Leader: "10.0.0.1:4647",
		Stable: stableAll(stableOnce, 4)}
	if diff := cmp.Diff(want, stateOf(t, a)); diff != "" {
		t.Errorf("cluster (-want +got):\n%s", diff)
	}
}

// TestTransferLeadershipToANonvoter checks that, as in Nomad, the call succeeds and the first voter in the peers'
// order other than the leader takes the leadership.
func TestTransferLeadershipToANonvoter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, a := quorum(t)
		time.Sleep(time.Minute)

		if err := a.TransferLeadership(t.Context(), "p-2"); err != nil {
			t.Fatalf("TransferLeadership: %v", err)
		}

		want := raftState{Peers: []string{"p-1", "p-2", "p-3*", "p-4"}, HealthLeader: "p-3", Leader: "10.0.0.3:4647",
			Stable: stableAll(time.Now(), 4)}
		if diff := cmp.Diff(want, stateOf(t, a)); diff != "" {
			t.Errorf("cluster (-want +got):\n%s", diff)
		}
	})
}

// TestTransferLeadershipToANonvoterWithoutAnotherVoter checks that the leader keeps the leadership when no other
// server can take it.
func TestTransferLeadershipToANonvoterWithoutAnotherVoter(t *testing.T) {
	f, a := quorum(t)
	f.SetPeers([]nomadops.Peer{
		{ID: "p-1", Name: "s1", Address: netip.MustParseAddrPort("10.0.0.1:4647"), Voter: true, Leader: true},
		{ID: "p-2", Name: "s2", Address: netip.MustParseAddrPort("10.0.0.2:4647")},
	})

	if err := a.TransferLeadership(t.Context(), "p-2"); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}

	peers, _ := a.Peers(t.Context())
	if !peers[0].Leader || peers[1].Leader {
		t.Errorf("Peers() = %+v, want p-1 to lead still", peers)
	}
	if got := stateOf(t, a); got.Leader != "10.0.0.1:4647" || !slices.Equal(got.Stable, stableAll(stableOnce, 4)) {
		t.Errorf("cluster = %+v, want the leader and the stable times as they were", got)
	}
}

func TestTransferLeadershipToAnUnknownServer(t *testing.T) {
	f, a := quorum(t)
	before := stateOf(t, a)

	checkGone(t, a.TransferLeadership(t.Context(), "p-9"),
		`nomadfake: TransferLeadership: id "p-9" was not found in the Raft configuration`)

	calls := f.Calls()
	if got, want := calls[len(calls)-1], (nomadfake.Call{Name: "TransferLeadership", Arg: "p-9"}); got != want {
		t.Errorf("last call = %+v, want %+v", got, want)
	}
	if diff := cmp.Diff(before, stateOf(t, a)); diff != "" {
		t.Errorf("cluster (-before +after):\n%s", diff)
	}
}

// TestRemovePeer checks that the peer leaves the Raft configuration and the report stays as the test set it, and that
// a repeat and an unknown ID succeed.
func TestRemovePeer(t *testing.T) {
	f, a := quorum(t)

	for _, id := range []string{"p-3", "p-3", "p-9"} {
		if err := a.RemovePeer(t.Context(), id); err != nil {
			t.Fatalf("RemovePeer(%s): %v, want success: a peer that is gone counts as removed", id, err)
		}
	}
	remove := func(id string) nomadfake.Call { return nomadfake.Call{Name: "RemovePeer", Arg: id} }
	wantCalls(t, f, bootstrapCall, remove("p-3"), remove("p-3"), remove("p-9"))

	want := raftState{Peers: []string{"p-1*", "p-2", "p-4"}, HealthLeader: "p-1", Leader: "10.0.0.1:4647",
		Stable: stableAll(stableOnce, 4)}
	if diff := cmp.Diff(want, stateOf(t, a)); diff != "" {
		t.Errorf("cluster (-want +got):\n%s", diff)
	}
	h, _ := a.Health(t.Context())
	if len(h.Servers) != 4 || h.Voters != 3 {
		t.Errorf("Health() = %+v, want the report as the test set it, with the removed server in it", h)
	}
}

// TestRemovePeerOfTheLeaderFails checks that the fake refuses to remove the leader's own peer, which tent never asks,
// with a permanent error, and changes nothing.
func TestRemovePeerOfTheLeaderFails(t *testing.T) {
	_, a := quorum(t)
	before := stateOf(t, a)

	err := a.RemovePeer(t.Context(), "p-1")

	checkErr(t, err, "nomadfake: RemovePeer: the peer p-1 leads the cluster", false)
	if errors.Is(err, nomadops.ErrGone) {
		t.Errorf("RemovePeer(p-1) = %v matches ErrGone, want a permanent error of another class", err)
	}

	if diff := cmp.Diff(before, stateOf(t, a)); diff != "" {
		t.Errorf("cluster (-before +after):\n%s", diff)
	}
}

func TestRaftWritesAreRefusedForABadIDBeforeAnyCall(t *testing.T) {
	f, a := quorum(t)
	for _, id := range []string{"", "p?1", "p/1", "p 1"} {
		if err := a.TransferLeadership(t.Context(), id); err == nil || errors.Is(err, nomadops.ErrGone) {
			t.Errorf("TransferLeadership(%q) = %v, want a permanent error", id, err)
		}
		if err := a.RemovePeer(t.Context(), id); err == nil || errors.Is(err, nomadops.ErrGone) {
			t.Errorf("RemovePeer(%q) = %v, want a permanent error", id, err)
		}
	}
	checkErr(t, a.TransferLeadership(t.Context(), ""), "nomadfake: TransferLeadership: no Raft ID", false)
	checkErr(t, a.RemovePeer(t.Context(), "p?1"),
		`nomadfake: RemovePeer: Raft ID "p?1" has a character other than an ASCII letter, a digit or "-"`, false)
	wantCalls(t, f, bootstrapCall)
}
