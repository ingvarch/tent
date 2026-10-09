package app_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/secret"
)

// TestWorldServersOverTimeJoinAsHealthyNonvotersThatVoteAfter15s checks that a server machine that becomes ready in
// a cluster with a leader is a healthy nonvoter with the time it joined as its stable time, cut to whole seconds, and
// votes 15 s later.
func TestWorldServersOverTimeJoinAsHealthyNonvotersThatVoteAfter15s(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
		atHalfSecond()
		joined := time.Now()
		added := b.addServer(t, "prod-servers-3")

		for _, tc := range []struct {
			wait   time.Duration
			voter  bool
			voters int
		}{{0, false, 3}, {14 * time.Second, false, 3}, {time.Second, true, 4}} {
			time.Sleep(tc.wait)
			st := b.state(t, added)
			switch {
			case st.peer == nil || st.entry == nil:
				t.Fatalf("after %v: the new server is not in the Raft configuration and the report: %+v", tc.wait, st)
			case st.peer.Voter != tc.voter || st.entry.Voter != tc.voter || st.health.Voters != tc.voters:
				t.Errorf("after %v: peer votes %v, entry votes %v, %d voters; want %v and %d", tc.wait, st.peer.Voter,
					st.entry.Voter, st.health.Voters, tc.voter, tc.voters)
			case !st.entry.Healthy || st.entry.Serf != "alive" || !st.health.Healthy:
				t.Errorf("after %v: the new server is %+v in a cluster that is healthy %v, want healthy and alive", tc.wait,
					st.entry, st.health.Healthy)
			case !st.entry.StableSince.Equal(joined.Truncate(time.Second)):
				t.Errorf("after %v: StableSince = %v, want the join at %v cut to whole seconds", tc.wait, st.entry.StableSince,
					joined.Truncate(time.Second))
			}
		}
	})
}

// TestWorldSetVoteAfterMovesTheDelay checks that SetVoteAfter sets how long a new server is a nonvoter, so that a
// delay beyond what a wait lasts leaves it one.
func TestWorldSetVoteAfterMovesTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		b.w.SetVoteAfter(time.Hour)
		added := b.addServer(t, "prod-servers-3")

		for _, tc := range []struct {
			wait  time.Duration
			voter bool
		}{{0, false}, {59 * time.Minute, false}, {time.Minute, true}} {
			time.Sleep(tc.wait)
			if st := b.state(t, added); st.peer == nil || st.peer.Voter != tc.voter {
				t.Errorf("after %v: the new server's peer is %+v, want it voting: %v", tc.wait, st.peer, tc.voter)
			}
		}
	})
}

// TestWorldServersOfTheFirstBootstrapVoteAtOnce checks that the servers that are ready before the cluster has a leader
// vote and are healthy when it gets one, whatever the delay to vote.
func TestWorldServersOfTheFirstBootstrapVoteAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorldWith(t, (*nomadWorld).ServersOverTime)
		peers := b.peers(t)
		health := b.health(t)
		if len(peers) != 3 || health.Voters != 3 || !health.Healthy {
			t.Fatalf("peers %v and the report %+v, want three voters and a healthy cluster", peers, health)
		}
		for _, p := range peers {
			if !p.Voter {
				t.Errorf("the bootstrap server %+v does not vote", p)
			}
		}
	})
}

// TestWorldWithoutDelaysAServerVotesAtOnceAndAHaltedOneIsGone checks the defaults: a new server votes at its first
// call, and a halted follower is out of the Raft configuration and the report at the next call, with its member
// failed.
func TestWorldWithoutDelaysAServerVotesAtOnceAndAHaltedOneIsGone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		added := b.addServer(t, "prod-servers-3")
		if st := b.state(t, added); st.peer == nil || !st.peer.Voter || st.health.Voters != 4 {
			t.Errorf("the new server's peer is %+v with %d voters, want it voting at once, as the fourth", st.peer,
				st.health.Voters)
		}

		follower := b.followers(t)[0]
		b.haltInstance(t, follower)
		st := b.state(t, follower)
		if st.peer != nil || st.entry != nil || st.member == nil || st.member.Status != "failed" || !st.health.Healthy {
			t.Errorf("after the halt: peer %+v, entry %+v, member %+v, healthy %v; want a failed member only, and a healthy "+
				"cluster", st.peer, st.entry, st.member, st.health.Healthy)
		}
	})
}

// TestWorldNewClusterForgetsItsServers checks that a cluster whose servers are all gone and that gets new ones has
// only the new ones in the Raft configuration, with one of them leading, and that deleting every server does not fail
// the test, as losing the leader of a cluster that lives on does.
func TestWorldNewClusterForgetsItsServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		addServer := b.serverMaker(t)
		for _, name := range serverNames {
			if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, name)); err != nil {
				t.Fatalf("DeleteInstance: %v", err)
			}
		}
		if _, err := b.api.Leader(t.Context()); err == nil {
			t.Fatal("Leader with every server gone succeeded, want the cluster without a leader")
		}
		added := []worldServer{addServer("new-0"), addServer("new-1"), addServer("new-2")}
		b.w.SetBootstrapped(secret.Secret("test-token"))

		peers := b.peers(t)
		if len(peers) != 3 || !b.health(t).Healthy {
			t.Fatalf("peers %v, want the three new servers in a healthy cluster", peers)
		}
		for _, p := range peers {
			if !p.Voter || (p.Leader != (p.ID == b.leader(t).raftID())) {
				t.Errorf("peer %+v, want a voter, and the leader the one that Leader gives", p)
			}
		}
		if lead := b.leader(t); lead != added[0] {
			t.Errorf("the leader is %s, want the first new server %s", lead, added[0])
		}
		if got := b.w.HaltedCalls(); len(got) != 0 {
			t.Errorf("calls reached halted servers: %v, want none, as the old servers are forgotten", got)
		}
	})
}

// TestWorldElectsTheFirstReadyServerOnceAllServersOfTheSpecsAreReady checks that a cluster has no leader while fewer
// servers are ready than its specs give, and then the first server that was ready leads, at its public address.
func TestWorldElectsTheFirstReadyServerOnceAllServersOfTheSpecsAreReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := newRelease(t)
		api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
		if err != nil {
			t.Fatalf("Nomad: %v", err)
		}
		seedServer(t, f, "prod-servers-0", "198.19.0.1")
		seedServer(t, f, "prod-servers-1", "198.19.0.2")

		if got, err := api.Leader(t.Context()); err == nil {
			t.Fatalf("Leader with two of three servers ready = %q, want no leader", got)
		}
		seedServer(t, f, "prod-servers-2", "198.19.0.3")
		if got, err := api.Leader(t.Context()); err != nil || got != "198.19.0.1:4647" {
			t.Errorf("Leader with the three servers ready = %q, %v; want the first server, 198.19.0.1:4647", got, err)
		}
	})
}

// TestWorldReportListsTheServersInTheReverseOrderOfThePeers checks that the entries of the report come in the reverse
// order of the Raft configuration, as Nomad keeps no order for them, so that no test reads one by its place.
func TestWorldReportListsTheServersInTheReverseOrderOfThePeers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		var peers, report []string
		for _, p := range b.peers(t) {
			peers = append(peers, p.ID)
		}
		for _, e := range b.health(t).Servers {
			report = append(report, e.ID)
		}
		slices.Reverse(report)
		if len(peers) != 3 || !slices.Equal(peers, report) {
			t.Errorf("peers %v and the report reversed %v, want three equal IDs", peers, report)
		}
	})
}

// TestWorldHasNoLeaderForAClusterWithoutSpecs checks that a cluster whose specs the store does not hold has no leader,
// however many servers are ready, as nothing says how many it needs.
func TestWorldHasNoLeaderForAClusterWithoutSpecs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		f, _ := withCloud(svc)
		api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
		if err != nil {
			t.Fatalf("Nomad: %v", err)
		}
		seedServer(t, f, "prod-servers-0", "198.19.0.1")

		if got, err := api.Leader(t.Context()); err == nil {
			t.Errorf("Leader of a cluster without specs = %q, want no leader", got)
		}
	})
}
