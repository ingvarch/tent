package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// holdWorld is a cluster of two servers as a run sees it: the victim, which runs and may have a peer in the Raft
// configuration, and another server that leads and votes. The tests change the world between two passes of the loop.
type holdWorld struct {
	t        *testing.T
	ctx      context.Context
	victim   cloud.Instance
	other    cloud.Instance
	listed   []cloud.Instance // what the cloud lists
	peers    []nomadops.Peer  // the Raft configuration
	members  []nomadops.Member
	deleted  []string // the IDs of the machines the cloud was asked to delete
	stops    []string // the IDs of the machines the cloud was asked to stop
	removals []string // the Raft IDs of the peers the run removed
	lists    int
	progress []Progress
	warnings []string

	peersTook   time.Duration // how long the read of the Raft configuration holds
	membersTook time.Duration // how long the read of the members holds
	removeTook  time.Duration // how long the removal of a peer holds
	removeErr   error         // when set, the removal of a peer fails with it and removes nothing
	readErr     error         // when set, the read of the Raft configuration fails with it
	stopCall    func(ctx context.Context) error
	override    *rollout.Step // when set, the step that a pass takes in place of the one the world calls for
}

// holdMachine is a running server that joined, with the private address 10.64.0.<n>.
func holdMachine(id, name string, n byte) cloud.Instance {
	in := apiServer(id, name, n)
	in.PrivateIP = netip.AddrFrom4([4]byte{10, 64, 0, n})
	in.SpecHash = "new"
	return in
}

// newHoldWorld returns the world of a victim that has no peer, beside one voter that leads.
func newHoldWorld(t *testing.T) *holdWorld {
	t.Helper()
	w := &holdWorld{
		t: t, ctx: t.Context(), victim: holdMachine("i-1", "prod-servers-0", 1),
		other: holdMachine("i-2", "prod-servers-1", 2),
	}
	w.victim.SpecHash = "old"
	w.listed = []cloud.Instance{w.victim, w.other}
	w.peers = []nomadops.Peer{{
		ID: "raft-2", Name: "prod-servers-1.global", Address: netip.AddrPortFrom(w.other.PrivateIP, 4647), Voter: true,
		Leader: true,
	}}
	w.setMember("")
	return w
}

// victimPeer returns the victim's peer in the Raft configuration, if it has one.
func (w *holdWorld) victimPeer() (nomadops.Peer, bool) {
	i := slices.IndexFunc(w.peers, func(p nomadops.Peer) bool { return p.ID == "raft-1" })
	if i < 0 {
		return nomadops.Peer{}, false
	}
	return w.peers[i], true
}

// addVictim puts the victim's peer into the Raft configuration, as a voter or as a nonvoter.
func (w *holdWorld) addVictim(voter bool) {
	w.peers = append(w.peers, nomadops.Peer{
		ID: "raft-1", Name: "prod-servers-0.global", Address: netip.AddrPortFrom(w.victim.PrivateIP, 4647), Voter: voter,
	})
}

// addVoters puts n more servers that vote into the Raft configuration.
func (w *holdWorld) addVoters(n int) {
	for i := range n {
		w.peers = append(w.peers, nomadops.Peer{
			ID: fmt.Sprintf("raft-%d", 10+i), Name: fmt.Sprintf("prod-servers-%d.global", 10+i),
			Address: netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 64, 0, byte(10 + i)}), 4647), Voter: true,
		})
	}
}

// setMember sets the status of the victim's member in the gossip pool, beside the member of the other server, which
// is alive; "" leaves the victim's out.
func (w *holdWorld) setMember(status string) {
	w.members = []nomadops.Member{{Name: "prod-servers-1.global", Address: w.other.PrivateIP, Status: "alive"}}
	if status != "" {
		w.members = append(w.members, nomadops.Member{
			Name: "prod-servers-0.global", Address: w.victim.PrivateIP, Status: status,
		})
	}
}

// stopMachine makes the cloud list the victim as stopped.
func (w *holdWorld) stopMachine() {
	w.victim.Ready = false
	w.listed[0] = w.victim
}

// holdNomad is the Nomad API of a holdWorld.
type holdNomad struct {
	nomadops.API
	w *holdWorld
}

func (n holdNomad) Peers(context.Context) ([]nomadops.Peer, error) {
	time.Sleep(n.w.peersTook)
	if n.w.readErr != nil {
		return nil, n.w.readErr
	}
	return slices.Clone(n.w.peers), nil
}

func (n holdNomad) Health(context.Context) (nomadops.Health, error) {
	return nomadops.Health{Healthy: true, Voters: voting(holdServers(n.w.peers)), Servers: []nomadops.ServerHealth{
		{ID: "raft-2", Name: "prod-servers-1.global", Healthy: true, Voter: true, Version: "2.0.7"},
	}}, nil
}

func (n holdNomad) Members(context.Context) ([]nomadops.Member, error) {
	time.Sleep(n.w.membersTook)
	return slices.Clone(n.w.members), nil
}

func (holdNomad) Nodes(context.Context) ([]nomadops.Node, error) { return nil, nil }

func (n holdNomad) RemovePeer(_ context.Context, raftID string) error {
	time.Sleep(n.w.removeTook)
	if n.w.removeErr != nil {
		return n.w.removeErr
	}
	n.w.removals = append(n.w.removals, raftID)
	n.w.peers = slices.DeleteFunc(n.w.peers, func(p nomadops.Peer) bool { return p.ID == raftID })
	return nil
}

func (n holdNomad) ForceLeave(_ context.Context, name string) error {
	n.w.members = slices.DeleteFunc(n.w.members, func(m nomadops.Member) bool { return m.Name == name })
	return nil
}

// holdServers returns the peers as the decisions see their servers.
func holdServers(peers []nomadops.Peer) []rollout.Server {
	return nomadReading{peers: peers}.state().Servers
}

// holdCloud is the cloud of a holdWorld.
type holdCloud struct {
	cloud.Nodes
	w *holdWorld
}

func (c holdCloud) List(context.Context, string) ([]cloud.Instance, error) {
	c.w.lists++
	return slices.Clone(c.w.listed), nil
}

func (c holdCloud) Stop(ctx context.Context, in cloud.Instance) error {
	c.w.stops = append(c.w.stops, in.ID)
	if c.w.stopCall == nil {
		return nil
	}
	return c.w.stopCall(ctx)
}

func (c holdCloud) Delete(_ context.Context, in cloud.Instance) error {
	c.w.deleted = append(c.w.deleted, in.ID)
	c.w.listed = slices.DeleteFunc(c.w.listed, func(i cloud.Instance) bool { return i.ID == in.ID })
	return nil
}

// newRun returns a run over the world that has listed the machines and made its API.
func (w *holdWorld) newRun() *rollRun {
	w.t.Helper()
	var lines []string
	s := testService(&lines)
	s.Nomad = func(nomadops.Config) (nomadops.API, error) { return holdNomad{w: w}, nil }
	s.OnProgress = func(p Progress) { w.progress = append(w.progress, p) }
	s.OnWarning = func(msg string) { w.warnings = append(w.warnings, msg) }
	r := &rollRun{
		s: s, kit: testKit(w.t, holdCloud{w: w}), model: apiModel, rollLoop: newRollLoop(), version: "2.0.7",
		listed: slices.Clone(w.listed),
		groups: []rollout.Group{{Name: "servers", Role: v1alpha1.RoleServer, Size: 1, SpecHash: "new"}},
	}
	if err := r.followAPI(); err != nil {
		w.t.Fatalf("followAPI: %v", err)
	}
	return r
}

// step returns the step the world calls for, made by hand: the removal of the victim's peer while it has one, else its
// stop.
func (w *holdWorld) step() rollout.Step {
	if w.override != nil {
		return *w.override
	}
	machine := rolloutMachines([]cloud.Instance{w.victim})[0]
	step := rollout.Step{Action: rollout.Stop, Group: w.victim.Group, Machine: machine}
	if p, ok := w.victimPeer(); ok {
		step.Action, step.Server = rollout.RemovePeer, rollout.Server{ID: p.ID}
	}
	return step
}

// pass is one pass of the loop over the step that step gives: an observation, then the hold or the carrying out of the
// step. It says what happened: "relisted" for an observation that must list first, "held" for a poll of a hold, "stop
// sent" for a stop that went to the cloud, "waited" for a stop that the run has sent, and "carried <action>" for
// another step.
func (w *holdWorld) pass(r *rollRun) (string, error) {
	w.t.Helper()
	reading, ok, err := r.observe(w.ctx)
	if err != nil || !ok {
		w.t.Fatalf("observe = %v, %v", ok, err)
	}
	step := w.step()
	if r.needsList(step) {
		r.relist = true
		return "relisted", nil
	}
	stops := len(w.stops)
	handled, err := r.holdStop(w.ctx, step, reading)
	switch {
	case handled && len(w.stops) > stops:
		return "stop sent", err
	case handled:
		return "held", err
	}
	err = r.act(w.ctx, step)
	switch {
	case step.Action == rollout.RemovePeer:
		return "carried remove-peer", err
	case step.Action != rollout.Stop:
		return "carried a step", err
	case len(w.stops) > stops:
		return "stop sent", err
	}
	return "waited", err
}

// mustPass is a pass that must give the outcome want and no error.
func (w *holdWorld) mustPass(r *rollRun, want string) {
	w.t.Helper()
	if got, err := w.pass(r); got != want || err != nil {
		w.t.Fatalf("pass = %q, %v; want %q", got, err, want)
	}
}

// holdFor passes until d has gone by since start; every pass must be a poll of a hold or an observation that lists.
func (w *holdWorld) holdFor(r *rollRun, start time.Time, d time.Duration) {
	w.t.Helper()
	for time.Since(start) < d {
		got, err := w.pass(r)
		if err != nil || got != "held" && got != "relisted" {
			w.t.Fatalf("pass %v after the start = %q, %v; want a poll of the hold", time.Since(start), got, err)
		}
	}
}

// usualCourse takes the run through the usual course of a held stop up to the observation that sends the stop: the
// observation that lists, a poll of the hold, the leader's re-add of the victim as a nonvoter, and the removal of
// that peer.
func (w *holdWorld) usualCourse(r *rollRun) {
	w.t.Helper()
	w.mustPass(r, "relisted")
	w.mustPass(r, "held")
	w.addVictim(false)
	w.mustPass(r, "carried remove-peer")
}

// reconcileEvents returns the events of the hold that the run reported, as "started <deadline>", "done" and "failed:
// <error>".
func (w *holdWorld) reconcileEvents() []string {
	var got []string
	for _, p := range w.progress {
		if p.Nomad == nil || p.Nomad.Action != NomadReconcile {
			continue
		}
		if p.Nomad.Node != w.victim.Name {
			w.t.Errorf("the hold names the node %q, want %q", p.Nomad.Node, w.victim.Name)
		}
		switch p.Step {
		case NodeStarted:
			got = append(got, "started "+p.Nomad.Deadline.String())
		case NodeDone:
			got = append(got, "done")
		case NodeFailed:
			got = append(got, "failed: "+p.Err.Error())
		}
	}
	return got
}

// bubble runs f in a synctest bubble with the world.
func bubble(t *testing.T, f func(t *testing.T, w *holdWorld, r *rollRun)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		w := newHoldWorld(t)
		f(t, w, w.newRun())
	})
}

// momentAt returns a moment that is mono after base by the monotonic clock and wall after it by the wall clock.
func momentAt(base time.Time, mono, wall time.Duration) moment {
	return moment{mono: base.Add(mono), wall: base.Round(0).Add(wall)}
}

// TestMomentUntilTakesTheLargerOfTheTwoClocks counts a time by the clock that says more: a machine that sleeps stops
// the monotonic clock and a person can set the wall clock.
func TestMomentUntilTakesTheLargerOfTheTwoClocks(t *testing.T) {
	t.Parallel()
	base := time.Now()
	for _, tc := range []struct {
		name     string
		from, to moment
		want     time.Duration
	}{
		{"both clocks agree", momentAt(base, 0, 0), momentAt(base, 3*time.Second, 3*time.Second), 3 * time.Second},
		{"the wall clock says more", momentAt(base, 0, 0), momentAt(base, 2*time.Second, 30*time.Second), 30 * time.Second},
		{"the monotonic clock says more", momentAt(base, 0, 0), momentAt(base, 30*time.Second, 2*time.Second),
			30 * time.Second},
		{"the wall clock was set back", momentAt(base, 0, 0), momentAt(base, 4*time.Second, -time.Hour), 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.from.until(tc.to); got != tc.want {
				t.Errorf("until = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNowGivesAWallTimeWithoutAMonotonicReading keeps the monotonic reading in one time only, so that the wall clock
// is compared as it reads.
func TestNowGivesAWallTimeWithoutAMonotonicReading(t *testing.T) {
	t.Parallel()
	m := now()

	if !strings.Contains(m.mono.String(), "m=") {
		t.Errorf("the monotonic time %q has no monotonic reading", m.mono)
	}
	if strings.Contains(m.wall.String(), "m=") {
		t.Errorf("the wall time %q has a monotonic reading", m.wall)
	}
	if !m.mono.Equal(m.wall) {
		t.Errorf("the two times %v and %v are not one instant", m.mono, m.wall)
	}
}

// TestHeldStopAllowsAStopOnlyRightAfterAReAddThatTheRunSaw lets the stop go when the run saw the victim leave the Raft
// configuration and come back as a nonvoter at its next observation, removed that peer, and the observation of the
// re-add is at most 10 s old.
func TestHeldStopAllowsAStopOnlyRightAfterAReAddThatTheRunSaw(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	for _, tc := range []struct {
		name string
		// the observations: the victim absent at 0 s, then in the configuration at reAdd, voting or not
		reAdd   int
		voter   bool
		removed bool
		stopAt  int
		want    bool
	}{
		{"the stop 2 s after the observation of the re-add", 2, false, true, 4, true},
		{"the stop 10 s after it", 2, false, true, 12, true},
		{"the stop 11 s after it", 2, false, true, 13, false},
		{"the re-add 10 s after the last observation of absence", 10, false, true, 12, true},
		{"the re-add 11 s after it", 11, false, true, 13, false},
		{"a peer that the run did not remove", 2, false, false, 4, false},
		{"a voter that comes back", 2, true, true, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &heldStop{first: base, since: at(0), last: at(0)}
			h.observe(at(tc.reAdd), true, tc.voter)
			if tc.removed {
				h.noteRemoval()
			}
			h.observe(at(tc.stopAt), false, false)

			if got := h.allows(at(tc.stopAt), at(tc.stopAt), true); got != tc.want {
				t.Errorf("allows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHeldStopDoesNotAllowAStopForANonvoterTheRunDidNotSeeAppear holds the stop of a victim that the run first saw as a
// nonvoter: it cannot tell how old the nonvoter is.
func TestHeldStopDoesNotAllowAStopForANonvoterTheRunDidNotSeeAppear(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	h := &heldStop{first: base, last: at(0)}
	h.observe(at(2), true, false)
	h.noteRemoval()
	h.observe(at(3), false, false)

	if h.allows(at(3), at(3), true) {
		t.Error("allows a stop for a nonvoter that the run saw first in the configuration")
	}
}

// TestHeldStopNeedsTheRemovalOfTheLastReAdd does not allow a stop after a second re-add that the run did not remove,
// whatever it removed after the first.
func TestHeldStopNeedsTheRemovalOfTheLastReAdd(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	h := &heldStop{first: base, since: at(0), last: at(0)}
	h.observe(at(2), true, false)
	h.noteRemoval()
	h.observe(at(3), false, false)
	if !h.allows(at(3), at(3), true) {
		t.Fatal("does not allow a stop right after the removal of a re-added nonvoter")
	}

	h.observe(at(5), true, false)
	h.observe(at(6), false, false)

	if h.allows(at(6), at(6), true) {
		t.Error("allows a stop after a second re-add that the run did not remove")
	}
}

// TestHeldStopAllowsAStopAfterSixtyFiveSecondsOutOfTheConfigurationOfAMemberThatIsNotAlive counts the 65 s on the
// monotonic clock from the first observation of the stretch, and asks that the member is not alive and the last
// observation is at most 10 s old.
func TestHeldStopAllowsAStopAfterSixtyFiveSecondsOutOfTheConfigurationOfAMemberThatIsNotAlive(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	newHeld := func() *heldStop { return &heldStop{first: base, since: at(0), last: at(0)} }
	for _, tc := range []struct {
		name        string
		at, sent    int
		memberAlive bool
		want        bool
	}{
		{"64 s", 64, 64, false, false},
		{"65 s", 65, 65, false, true},
		{"66 s", 66, 66, false, true},
		{"66 s, a member that is alive", 66, 66, true, false},
		{"4 minutes, a member that is alive", 240, 240, true, false},
		{"64 s at the observation and 66 s when the stop goes", 64, 66, false, false},
		{"the observation 10 s old when the stop goes", 70, 80, false, true},
		{"the observation 11 s old when the stop goes", 70, 81, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := newHeld().allows(at(tc.at), at(tc.sent), tc.memberAlive); got != tc.want {
				t.Errorf("allows = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHeldStopStartsTheStretchAgainAfterAGapOrAnObservationInTheConfiguration needs 65 s again from the observation
// after a gap of more than 10 s, and from the first one after the victim showed in the Raft configuration.
func TestHeldStopStartsTheStretchAgainAfterAGapOrAnObservationInTheConfiguration(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	for _, tc := range []struct {
		name  string
		steps func(h *heldStop)
		begin int
	}{
		{"a gap of 11 s", func(h *heldStop) { h.observe(at(60), false, false); h.observe(at(71), false, false) }, 71},
		{"the victim in the configuration", func(h *heldStop) {
			h.observe(at(58), false, false)
			h.observe(at(60), true, true)
			h.observe(at(62), false, false)
		}, 62},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &heldStop{first: base, since: at(0), last: at(0)}
			for s := 2; s <= 58; s += 2 {
				h.observe(at(s), false, false)
			}
			tc.steps(h)
			begin := tc.begin
			for s := begin + 2; s <= begin+66; s += 2 {
				h.observe(at(s), false, false)
				if got, want := h.allows(at(s), at(s), false), s-begin >= 65; got != want {
					t.Errorf("allows %d s after the new stretch began = %v, want %v", s-begin, got, want)
				}
			}
		})
	}
}

// TestHeldStopCountsAgesByTheLargerOfTheTwoClocksAndTheSixtyFiveSecondsByTheMonotonicOne builds moments whose clocks
// differ, as after a sleep of the operator's machine.
func TestHeldStopCountsAgesByTheLargerOfTheTwoClocksAndTheSixtyFiveSecondsByTheMonotonicOne(t *testing.T) {
	t.Parallel()
	base := time.Now()
	sec := func(s int) time.Duration { return time.Duration(s) * time.Second }

	t.Run("two observations 2 s apart by one clock and 30 s by the other end the stretch", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{first: base, since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		h.observe(momentAt(base, sec(2), sec(30)), true, false)
		h.noteRemoval()
		after := momentAt(base, sec(3), sec(31))
		h.observe(after, false, false)

		if h.allows(after, after, true) {
			t.Error("allows a stop after a re-add that the run did not see: the two observations were 30 s apart")
		}
	})

	t.Run("two observations 2 s apart by both clocks keep it", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{first: base, since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		h.observe(momentAt(base, sec(2), sec(2)), true, false)
		h.noteRemoval()
		after := momentAt(base, sec(3), sec(3))
		h.observe(after, false, false)

		if !h.allows(after, after, true) {
			t.Error("does not allow a stop after a re-add that the run saw")
		}
	})

	t.Run("a re-add 2 s old by the monotonic clock and 30 s by the wall clock", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{reAdded: momentAt(base, 0, 0), removed: true}
		after := momentAt(base, sec(2), sec(30))

		if h.allows(after, after, true) {
			t.Error("allows a stop 30 s after the re-add by the wall clock")
		}
	})

	t.Run("an observation 2 s old by the monotonic clock and 30 s by the wall clock", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		seen := momentAt(base, sec(66), sec(66))

		if h.allows(seen, momentAt(base, sec(68), sec(96)), false) {
			t.Error("allows a stop on an observation that is 30 s old by the wall clock")
		}
	})

	t.Run("a gap of 30 s by the monotonic clock and 2 s by the wall clock ends the stretch", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{first: base, since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		h.observe(momentAt(base, sec(30), sec(2)), true, false)
		h.noteRemoval()
		after := momentAt(base, sec(31), sec(3))
		h.observe(after, false, false)

		if h.allows(after, after, true) {
			t.Error("allows a stop after a re-add that the run did not see: the two observations were 30 s apart")
		}
	})

	t.Run("a re-add 30 s old by the monotonic clock and 2 s by the wall clock", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{reAdded: momentAt(base, 0, 0), removed: true}
		after := momentAt(base, sec(30), sec(2))

		if h.allows(after, after, true) {
			t.Error("allows a stop 30 s after the re-add by the monotonic clock")
		}
	})

	t.Run("an observation 30 s old by the monotonic clock and 2 s by the wall clock", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		seen := momentAt(base, sec(66), sec(66))

		if h.allows(seen, momentAt(base, sec(96), sec(68)), false) {
			t.Error("allows a stop on an observation that is 30 s old by the monotonic clock")
		}
	})

	t.Run("65 s by the wall clock and 64 s by the monotonic clock", func(t *testing.T) {
		t.Parallel()
		h := &heldStop{since: momentAt(base, 0, 0), last: momentAt(base, 0, 0)}
		seen := momentAt(base, sec(64), sec(100))

		if h.allows(seen, seen, false) {
			t.Error("counts the 65 s by the wall clock: the machine may have slept")
		}
	})
}

// TestHoldAppliesToTheStopOfARunningServerWithoutAPeerBesideFewerThanTwoVoters holds that stop, and sends every other
// stop as before.
func TestHoldAppliesToTheStopOfARunningServerWithoutAPeerBesideFewerThanTwoVoters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(w *holdWorld, r *rollRun)
		want  string
	}{
		{"a running server without a peer beside one voter", func(*holdWorld, *rollRun) {}, "held"},
		{"beside two voters", func(w *holdWorld, _ *rollRun) { w.addVoters(1) }, "stop sent"},
		{"beside three voters", func(w *holdWorld, _ *rollRun) { w.addVoters(2) }, "stop sent"},
		{"beside no voter", func(w *holdWorld, _ *rollRun) { w.peers = nil }, "held"},
		{"a machine of a client group", func(w *holdWorld, _ *rollRun) {
			w.victim.Group, w.victim.Role = "workers", v1alpha1.RoleClient
			w.listed[0] = w.victim
		}, "stop sent"},
		{"a machine that the list shows stopped", func(w *holdWorld, _ *rollRun) { w.stopMachine() }, "stop sent"},
		{"a machine whose stop the run has sent", func(_ *holdWorld, r *rollRun) {
			r.stopping["i-1"] = time.Now()
		}, "waited"},
		{"a step that is no stop", func(w *holdWorld, _ *rollRun) {
			step := rollout.Step{
				Action: rollout.RemovePeer, Group: "servers", Machine: rolloutMachines(w.listed[:1])[0],
				Server: rollout.Server{ID: "raft-1"},
			}
			w.override = &step
		}, "carried remove-peer"},
		{"a server with a peer that does not vote", func(w *holdWorld, _ *rollRun) {
			w.addVictim(false)
			step := rollout.Step{Action: rollout.Stop, Group: "servers", Machine: rolloutMachines(w.listed[:1])[0]}
			w.override = &step
		}, "stop sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				tc.setup(w, r)
				if _, stopping := r.stopping["i-1"]; !stopping && w.step().Action == rollout.Stop {
					w.mustPass(r, "relisted")
				}

				got, err := w.pass(r)

				if got != tc.want || err != nil {
					t.Errorf("pass = %q, %v; want %q", got, err, tc.want)
				}
				if tc.want != "held" && len(w.reconcileEvents()) != 0 {
					t.Errorf("the run reported the hold %v for a stop that is not held", w.reconcileEvents())
				}
			})
		})
	}
}

// TestObservationBeginsBeforeTheReadOfTheRaftConfiguration takes the time of an observation before its first read, so
// that a read that holds makes the observation older.
func TestObservationBeginsBeforeTheReadOfTheRaftConfiguration(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.peersTook = 30 * time.Second
		before := time.Now()

		if _, ok, err := r.observe(t.Context()); !ok || err != nil {
			t.Fatalf("observe = %v, %v", ok, err)
		}

		if !r.observedAt.mono.Equal(before) || !r.observedAt.wall.Equal(before) {
			t.Errorf("the observation began at %v / %v, want %v: before the read that held for %v", r.observedAt.mono,
				r.observedAt.wall, before, w.peersTook)
		}
		if got := r.observedAt.until(now()); got != 30*time.Second {
			t.Errorf("the observation is %v old after the read, want 30s", got)
		}
	})
}

// TestFirstPollOfAHoldListsTakesTheVictimOutOfTheAPIAndForgetsTheLastStep lists the machines at every poll, leaves the
// victim out of the API, polls once and forgets the step it carried out last, so that the removal that follows is a
// first try with no poll before it.
func TestFirstPollOfAHoldListsTakesTheVictimOutOfTheAPIAndForgetsTheLastStep(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.mustPass(r, "relisted")
		r.last, r.tries, r.settled = keyOf(rollout.Step{
			Action: rollout.RemovePeer, Group: "servers", Machine: rollout.Machine{ID: "i-1"},
			Server: rollout.Server{ID: "raft-1"},
		}), 2, true
		lists, start := w.lists, time.Now()

		w.mustPass(r, "held")

		if want := []string{"203.0.113.2:4646"}; !slices.Equal(r.apiAt, want) || !r.unpeered["i-1"] {
			t.Errorf("the API is over %v and unpeered is %v, want the victim out of the API and noted", r.apiAt, r.unpeered)
		}
		if w.lists != lists+1 || !r.listing {
			t.Errorf("the poll listed %d times, want once", w.lists-lists)
		}
		if got := time.Since(start); got != rollPoll {
			t.Errorf("the poll took %v, want one poll of %v", got, rollPoll)
		}
		if r.last != (stepKey{}) || r.tries != 0 || r.settled {
			t.Errorf("the run still remembers the step %v after %d tries (settled %v)", r.last, r.tries, r.settled)
		}

		w.addVictim(false)
		before := time.Now()
		w.mustPass(r, "carried remove-peer")

		if got := time.Since(before); got != 0 {
			t.Errorf("the removal of the re-added peer came %v after the observation, want no poll before it", got)
		}
		if r.tries != 1 || !slices.Equal(w.removals, []string{"raft-1"}) {
			t.Errorf("the removal was tried %d times and sent as %v, want one first try", r.tries, w.removals)
		}
		lists = w.lists
		w.mustPass(r, "stop sent")
		if w.lists != lists+1 {
			t.Errorf("the observation after the removal listed %d times, want once", w.lists-lists)
		}
	})
}

// TestHeldStopIsSentRightAfterTheRemovalOfAReAddedVictim goes through the usual course: the victim is held, the leader
// adds it again as a nonvoter, the run removes that peer and sends the stop at once.
func TestHeldStopIsSentRightAfterTheRemovalOfAReAddedVictim(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.usualCourse(r)
		start := time.Now()

		w.mustPass(r, "stop sent")

		if got := time.Since(start); got != 0 {
			t.Errorf("the stop went %v after the observation, want at once", got)
		}
		if !slices.Equal(w.stops, []string{"i-1"}) || r.rolled.counts().Stopped != 1 {
			t.Errorf("the cloud was asked to stop %v and the run counts %d, want i-1 once", w.stops, r.rolled.counts().Stopped)
		}
		if _, ok := r.stopping["i-1"]; !ok || len(r.held) != 0 {
			t.Errorf("stopping %v and held %v, want the victim stopping and forgotten as held", r.stopping, r.held)
		}
		if want := []string{"started 5m0s", "done"}; !slices.Equal(w.reconcileEvents(), want) {
			t.Errorf("the hold reported %v, want %v", w.reconcileEvents(), want)
		}
	})
}

// TestHeldStopWaitsForTheNextReconcileWhenTheReAddIsTooOldOrWasNotSeen holds the stop after a removal that came 11 s
// after the observation of the re-add, after reads that end 11 s after it, after two observations 11 s apart, also when
// the first one's reads took most of that time, after a nonvoter that was there at the first observation and after a
// voter's removal.
func TestHeldStopWaitsForTheNextReconcileWhenTheReAddIsTooOldOrWasNotSeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
		run  func(t *testing.T, w *holdWorld, r *rollRun) string
	}{
		{"the removal takes 11 s", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.removeTook = 11 * time.Second
			w.usualCourse(r)
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the removal takes 9 s", "stop sent", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.removeTook = 9 * time.Second
			w.usualCourse(r)
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the re-add is read for 8 s and the removal takes 3 s", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.removeTook = 3 * time.Second
			w.mustPass(r, "relisted")
			w.mustPass(r, "held")
			w.addVictim(false)
			w.peersTook = 8 * time.Second
			w.mustPass(r, "carried remove-peer")
			w.peersTook = 0
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the reads after the removal take 11 s", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.usualCourse(r)
			w.membersTook = 11 * time.Second
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the reads after the removal take 10 s", "stop sent", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.usualCourse(r)
			w.membersTook = 10 * time.Second
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the first observation of the hold is read for 9 s", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.mustPass(r, "relisted")
			w.peersTook = 9 * time.Second
			w.mustPass(r, "held")
			w.peersTook = 0
			w.addVictim(false)
			w.mustPass(r, "carried remove-peer")
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the observations are 11 s apart", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.mustPass(r, "relisted")
			w.mustPass(r, "held")
			time.Sleep(9 * time.Second)
			w.addVictim(false)
			w.mustPass(r, "carried remove-peer")
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"the observations are 10 s apart", "stop sent", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.mustPass(r, "relisted")
			w.mustPass(r, "held")
			time.Sleep(8 * time.Second)
			w.addVictim(false)
			w.mustPass(r, "carried remove-peer")
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"a nonvoter at the first observation", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.addVictim(false)
			w.mustPass(r, "carried remove-peer")
			w.mustPass(r, "relisted")
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
		{"a voter that is removed", "held", func(t *testing.T, w *holdWorld, r *rollRun) string {
			w.mustPass(r, "relisted")
			w.mustPass(r, "held")
			w.addVictim(true)
			w.mustPass(r, "carried remove-peer")
			got, err := w.pass(r)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				if got := tc.run(t, w, r); got != tc.want {
					t.Errorf("the pass after the removal = %q, want %q", got, tc.want)
				}
			})
		})
	}
}

// TestHeldStopIsSentAfterSixtyFiveSecondsWhenTheMemberIsNotAlive sends the stop of a victim that the leader did not add
// again, in polls 2 s apart, 66 s after the first observation of its absence, when its member is failed, leaving or
// left, or when there is none; with a member that is alive it holds it, also after 4 minutes.
func TestHeldStopIsSentAfterSixtyFiveSecondsWhenTheMemberIsNotAlive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		member string
		after  time.Duration
		want   string
	}{
		{"failed, 64 s", "failed", 64 * time.Second, "held"},
		{"failed, 66 s", "failed", 66 * time.Second, "stop sent"},
		{"none, 66 s", "", 66 * time.Second, "stop sent"},
		{"leaving, 66 s", "leaving", 66 * time.Second, "stop sent"},
		{"left, 66 s", "left", 66 * time.Second, "stop sent"},
		{"alive, 66 s", "alive", 66 * time.Second, "held"},
		{"alive, 4 minutes", "alive", 4 * time.Minute, "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				w.setMember(tc.member)
				w.mustPass(r, "relisted")
				start := time.Now()
				w.holdFor(r, start, tc.after)

				w.mustPass(r, tc.want)
				if tc.want == "stop sent" && (len(w.reconcileEvents()) != 2 || !r.wrote) {
					t.Errorf("the hold reported %v and the run has sent a write: %v; want it started and done, and a write",
						w.reconcileEvents(), r.wrote)
				}
			})
		})
	}
}

// TestHeldStopNeedsSixtyFiveSecondsAgainAfterAGapOrAnObservationInTheConfiguration restarts the stretch of absence: 66
// s after the observation that follows a gap of 11 s, or after the observation that shows the victim in the Raft
// configuration, and not 66 s after the first one.
func TestHeldStopNeedsSixtyFiveSecondsAgainAfterAGapOrAnObservationInTheConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		disturb func(w *holdWorld, r *rollRun)
	}{
		{"a gap of 11 s", func(*holdWorld, *rollRun) { time.Sleep(9 * time.Second) }},
		{"the victim in the configuration", func(w *holdWorld, r *rollRun) {
			w.addVictim(true)
			w.mustPass(r, "carried remove-peer")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				w.setMember("failed")
				w.mustPass(r, "relisted")
				w.holdFor(r, time.Now(), 60*time.Second)

				tc.disturb(w, r)
				restart := time.Now()
				w.holdFor(r, restart, 66*time.Second)

				w.mustPass(r, "stop sent")
				if got := time.Since(restart); got < 66*time.Second {
					t.Errorf("the stop went %v after the stretch began again, want 66s or more", got)
				}
			})
		})
	}
}

// TestHeldStopIsHeldWhenTheLastObservationIsTooOld holds a stop whose stretch of absence is 70 s long, and whose member
// reads failed at last, when the observation is 47 s old by the time the stop would go, as when a read held.
func TestHeldStopIsHeldWhenTheLastObservationIsTooOld(t *testing.T) {
	t.Parallel()
	bubble(t, func(_ *testing.T, w *holdWorld, r *rollRun) {
		w.setMember("alive")
		w.mustPass(r, "relisted")
		w.holdFor(r, time.Now(), 70*time.Second)
		w.setMember("failed")
		w.membersTook = 47 * time.Second

		w.mustPass(r, "held")
	})
}

// TestHoldReportsTheReconcileOnceAndEndsItWithTheNextStepOrTheStop reports the start at the first poll with what is
// left of the limit, the end when the decisions give another step, and a start again, with less left, for a later hold
// of the same victim.
func TestHoldReportsTheReconcileOnceAndEndsItWithTheNextStepOrTheStop(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.removeTook = 11*time.Second + 300*time.Millisecond
		w.mustPass(r, "relisted")
		w.mustPass(r, "held")
		w.mustPass(r, "held")
		if want := []string{"started 5m0s"}; !slices.Equal(w.reconcileEvents(), want) {
			t.Fatalf("after two polls the hold reported %v, want %v", w.reconcileEvents(), want)
		}
		w.addVictim(false)
		w.mustPass(r, "carried remove-peer")
		if want := []string{"started 5m0s", "done"}; !slices.Equal(w.reconcileEvents(), want) {
			t.Fatalf("after the removal the hold reported %v, want %v", w.reconcileEvents(), want)
		}

		w.mustPass(r, "held")

		want := []string{"started 5m0s", "done", "started 4m45s"}
		if !slices.Equal(w.reconcileEvents(), want) {
			t.Errorf("the second hold reported %v, want %v", w.reconcileEvents(), want)
		}
	})
}

// victimRun is a run over a world whose victim runs without a peer beside one voter, as the decisions see it: the
// victim's group is outdated and has no peer.
func victimRun(t *testing.T, member string) (*holdWorld, *rollRun) {
	t.Helper()
	w := newHoldWorld(t)
	w.setMember(member)
	return w, w.newRun()
}

// TestRunSendsTheHeldStopOfAVictimWhoseMemberIsNotAliveAndFinishesTheRemoval runs the loop over the decisions: the stop
// of a server without a peer beside one voter is held for 65 s, goes out, and the removal ends with the force-leave and
// the delete.
func TestRunSendsTheHeldStopOfAVictimWhoseMemberIsNotAliveAndFinishesTheRemoval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w, r := victimRun(t, "failed")
		start := time.Now()
		var stoppedAt time.Duration
		w.stopCall = func(context.Context) error {
			stoppedAt = time.Since(start)
			w.stopMachine()
			return nil
		}

		counts, err := r.run(t.Context())

		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if stoppedAt < 65*time.Second || stoppedAt > 70*time.Second {
			t.Errorf("the stop went %v after the start, want it held for 65 s of absence", stoppedAt)
		}
		if want := (RollCounts{Stopped: 1, Deleted: 1}); counts != want {
			t.Errorf("counts = %+v, want %+v", counts, want)
		}
		if want := []string{"started 5m0s", "done"}; !slices.Equal(w.reconcileEvents(), want) {
			t.Errorf("the hold reported %v, want %v", w.reconcileEvents(), want)
		}
		if !slices.Equal(w.stops, []string{"i-1"}) || !slices.Equal(w.deleted, []string{"i-1"}) {
			t.Errorf("stops %v and deletes %v, want the victim once each", w.stops, w.deleted)
		}
	})
}

// TestRunEndsAfterFiveMinutesOfHoldsWhenTheMemberStaysAlive ends the run with no stop when the leader does not add the
// victim again and its member stays alive: the holds of one victim last five minutes together.
func TestRunEndsAfterFiveMinutesOfHoldsWhenTheMemberStaysAlive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w, r := victimRun(t, "alive")
		start := time.Now()

		_, err := r.run(t.Context())

		want := "tent could not stop node prod-servers-0 right after a reconcile of Nomad's leader within 5m0s; " +
			"both servers run; run tent rolling-update cluster again"
		if err == nil || err.Error() != want {
			t.Fatalf("run = %v, want %q", err, want)
		}
		if got := time.Since(start); got != holdTimeout {
			t.Errorf("the run lasted %v, want %v", got, holdTimeout)
		}
		if len(w.stops) != 0 {
			t.Errorf("the cloud was asked to stop %v, want nothing", w.stops)
		}
		if want := []string{"started 5m0s", "failed: " + want}; !slices.Equal(w.reconcileEvents(), want) {
			t.Errorf("the hold reported %v, want %v", w.reconcileEvents(), want)
		}
	})
}

// TestHeldStopCallThatAnswersCountsAtOnce counts the machine as stopped when the cloud answers within the limit of the
// call, whatever the time it took, and warns of nothing.
func TestHeldStopCallThatAnswersCountsAtOnce(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.stopCall = answersAfter(9 * time.Second)
		w.usualCourse(r)
		start := time.Now()

		w.mustPass(r, "stop sent")

		if got := time.Since(start); got != 9*time.Second {
			t.Errorf("the call took %v, want 9s", got)
		}
		if r.rolled.counts().Stopped != 1 || len(w.warnings) != 0 {
			t.Errorf("stopped %d, warnings %v; want the machine counted and no warning", r.rolled.counts().Stopped, w.warnings)
		}
		if err, ok := r.heldSent["i-1"]; !ok || err != nil {
			t.Errorf("heldSent = %v, %v; want the stop noted with no error", err, ok)
		}
	})
}

// answersAfter is a stop call that answers after d, or fails with the context's error if the context ends first.
func answersAfter(d time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sentHeldStop takes the run through the usual course and sends its held stop, whose call is stopCall.
func sentHeldStop(t *testing.T, w *holdWorld, r *rollRun, stopCall func(context.Context) error) {
	t.Helper()
	w.stopCall = stopCall
	w.usualCourse(r)
	w.mustPass(r, "stop sent")
}

// TestHeldStopCallThatFailsCountsAsSentAndTheRunGoesOn keeps the machine among those being stopped and out of the API
// when the call gets no answer in 10 s or fails at once, notes the error, reports the failed stop and one warning,
// does not count the machine and sends no second stop.
func TestHeldStopCallThatFailsCountsAsSentAndTheRunGoesOn(t *testing.T) {
	t.Parallel()
	errCloud := errors.New("the cloud broke the answer off")
	for _, tc := range []struct {
		name string
		call func(ctx context.Context) error
		took time.Duration
		want error
	}{
		{"no answer in 10 s", answersAfter(time.Hour), 10 * time.Second, context.DeadlineExceeded},
		{"an error at once", func(context.Context) error { return errCloud }, 0, errCloud},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				w.usualCourse(r)
				w.stopCall = tc.call
				start := time.Now()

				w.mustPass(r, "stop sent")

				if got := time.Since(start); got != tc.took {
					t.Errorf("the call took %v, want %v", got, tc.took)
				}
				if _, ok := r.stopping["i-1"]; !ok || !errors.Is(r.heldSent["i-1"], tc.want) {
					t.Errorf("stopping %v, heldSent %v; want the machine stopping and the call's error %v", r.stopping, r.heldSent,
						tc.want)
				}
				if slices.Contains(r.apiAt, "203.0.113.1:4646") {
					t.Errorf("the API is over %v, want it without the victim", r.apiAt)
				}
				if r.rolled.counts().Stopped != 0 || len(r.held) != 0 {
					t.Errorf("stopped %d and held %v, want none counted and the hold forgotten", r.rolled.counts().Stopped, r.held)
				}
				want := "the stop of node prod-servers-0 (ID i-1) may still be carried out; tent waits up to 2m0s for the " +
					"cloud to list it as stopped and removes it from the Raft configuration if Nomad's leader adds it again"
				if !slices.Equal(w.warnings, []string{want}) {
					t.Errorf("warnings = %q, want one: %q", w.warnings, want)
				}
				var failed []Progress
				for _, p := range w.progress {
					if p.Step == NodeFailed && p.Node.Action == NodeStop {
						failed = append(failed, p)
					}
				}
				if len(failed) != 1 || !errors.Is(failed[0].Err, tc.want) || failed[0].Node.Name != "prod-servers-0" {
					t.Errorf("failed stops = %+v, want one for the victim with the call's error", failed)
				}

				for range 3 {
					w.mustPass(r, "waited")
				}
				if len(w.stops) != 1 || len(w.warnings) != 1 {
					t.Errorf("stops %v and warnings %v, want no second stop and no second warning", w.stops, w.warnings)
				}
			})
		})
	}
}

// TestListCountsAHeldStopWhoseCallFailedOnceItShowsTheMachineStopped counts the machine when the cloud lists it as
// stopped, and forgets what the run noted of the call.
func TestListCountsAHeldStopWhoseCallFailedOnceItShowsTheMachineStopped(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		sentHeldStop(t, w, r, func(context.Context) error { return errors.New("no answer") })

		if err := r.list(t.Context()); err != nil || r.rolled.counts().Stopped != 0 {
			t.Fatalf("list while the cloud shows it running = %v, stopped %d; want nothing counted", err,
				r.rolled.counts().Stopped)
		}
		w.stopMachine()
		if err := r.list(t.Context()); err != nil {
			t.Fatalf("list: %v", err)
		}

		if r.rolled.counts().Stopped != 1 || len(r.stopping) != 0 || len(r.heldSent) != 0 {
			t.Errorf("stopped %d, stopping %v, heldSent %v; want the machine counted once and forgotten",
				r.rolled.counts().Stopped, r.stopping, r.heldSent)
		}
	})
}

// TestListForgetsAHeldVictimThatTheCloudShowsStoppedOrNoMore drops what the run remembers of a held victim.
func TestListForgetsAHeldVictimThatTheCloudShowsStoppedOrNoMore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		do   func(w *holdWorld)
	}{
		{"stopped", func(w *holdWorld) { w.stopMachine() }},
		{"gone", func(w *holdWorld) { w.listed = w.listed[1:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				w.mustPass(r, "relisted")
				w.mustPass(r, "held")
				if len(r.held) != 1 {
					t.Fatalf("held = %v, want the victim", r.held)
				}

				tc.do(w)
				if err := r.list(t.Context()); err != nil {
					t.Fatalf("list: %v", err)
				}

				if len(r.held) != 0 {
					t.Errorf("held = %v after the list, want none", r.held)
				}
			})
		})
	}
}

// TestRemovalOfAReAddedVictimIsSentAtOnceWhileItsStopIsWaitedFor sends the removal that the decisions give for a victim
// whose held stop went, with no poll before it and as a first try, three times in a row with a stop between.
func TestRemovalOfAReAddedVictimIsSentAtOnceWhileItsStopIsWaitedFor(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		sentHeldStop(t, w, r, func(context.Context) error { return errors.New("no answer") })
		started := len(w.reconcileEvents())

		for i := 1; i <= 3; i++ {
			w.addVictim(false)
			before := time.Now()
			w.mustPass(r, "carried remove-peer")
			if time.Since(before) != 0 || r.tries != 1 {
				t.Fatalf("removal %d: %v after the observation, %d tries; want no poll and a first try", i,
					time.Since(before), r.tries)
			}
			w.mustPass(r, "waited")
		}

		if len(w.removals) != 4 || len(w.reconcileEvents()) != started || r.holding != nil {
			t.Errorf("removals %v, hold events %v, holding %v; want 4 removals and no hold", w.removals,
				w.reconcileEvents(), r.holding)
		}
	})
}

// TestWaitForAHeldStopEndsAtTheLimitWithTheTextOfTheCall ends the run two minutes after the call, with the call's error
// when it failed and with the built text when it answered.
func TestWaitForAHeldStopEndsAtTheLimitWithTheTextOfTheCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(context.Context) error
		want string
	}{
		{"a call that failed", func(context.Context) error { return errors.New("boom") },
			"stop node prod-servers-0 (ID i-1): boom; the cloud still lists it as running after 2m0s; " +
				"run tent rolling-update cluster again"},
		{"a call that answered", func(context.Context) error { return nil },
			"the cloud still lists node prod-servers-0 (ID i-1) as running 2m0s after tent stopped it; " +
				"run tent rolling-update cluster again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				sentHeldStop(t, w, r, tc.call)
				start := time.Now()
				var err error

				for err == nil {
					_, err = w.pass(r)
				}

				if err.Error() != tc.want {
					t.Errorf("the run ended with %q, want %q", err, tc.want)
				}
				if got := time.Since(start); got != stopTimeout {
					t.Errorf("the run ended %v after the call, want %v", got, stopTimeout)
				}
			})
		})
	}
}

// TestHeldStopCallThatFailsBecauseTheRunEndedEndsTheRunWithoutAWarning treats the end of the run's own context as a
// cut: nothing counts as sent, and nothing is warned.
func TestHeldStopCallThatFailsBecauseTheRunEndedEndsTheRunWithoutAWarning(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		ctx, cancel := context.WithCancel(t.Context())
		w.ctx = ctx
		w.usualCourse(r)
		w.stopCall = func(c context.Context) error { cancel(); <-c.Done(); return c.Err() }

		_, err := w.pass(r)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("pass = %v, want %v", err, context.Canceled)
		}
		if len(r.stopping) != 0 || len(r.heldSent) != 0 || len(w.warnings) != 0 {
			t.Errorf("stopping %v, heldSent %v, warnings %v; want none", r.stopping, r.heldSent, w.warnings)
		}
	})
}

// TestStopBesideTwoOtherVotersHasNoLimitAndItsFailedCallEndsTheRun keeps the rule of a stop that is not held: the call
// has no limit of the run's, and a call that fails ends the run with nothing noted.
func TestStopBesideTwoOtherVotersHasNoLimitAndItsFailedCallEndsTheRun(t *testing.T) {
	t.Parallel()
	errCloud := errors.New("the cloud refused")
	t.Run("a call of 30 s", func(t *testing.T) {
		t.Parallel()
		bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
			w.addVoters(1)
			w.stopCall = answersAfter(30 * time.Second)
			w.mustPass(r, "relisted")

			w.mustPass(r, "stop sent")

			if r.rolled.counts().Stopped != 1 || len(r.heldSent) != 0 {
				t.Errorf("stopped %d, heldSent %v; want the machine counted and nothing noted", r.rolled.counts().Stopped,
					r.heldSent)
			}
		})
	})
	t.Run("a call that fails", func(t *testing.T) {
		t.Parallel()
		bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
			w.addVoters(1)
			w.stopCall = func(context.Context) error { return errCloud }
			w.mustPass(r, "relisted")

			got, err := w.pass(r)

			if got != "stop sent" || !errors.Is(err, errCloud) {
				t.Errorf("pass = %q, %v; want the cloud's error", got, err)
			}
			if len(r.stopping) != 0 || len(r.heldSent) != 0 || len(w.warnings) != 0 {
				t.Errorf("stopping %v, heldSent %v, warnings %v; want none", r.stopping, r.heldSent, w.warnings)
			}
		})
	})
}

// TestObserveDropsAMachineFromThoseWithoutAPeerWhenItVotesAgain makes the API over the machine again at the next list
// when an observation shows its server voting, and keeps it out while the server is a nonvoter or absent.
func TestObserveDropsAMachineFromThoseWithoutAPeerWhenItVotesAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(w *holdWorld)
		gone  bool
	}{
		{"it votes", func(w *holdWorld) { w.addVictim(true) }, true},
		{"it does not vote", func(w *holdWorld) { w.addVictim(false) }, false},
		{"it has no peer", func(*holdWorld) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				r.unpeered["i-1"] = true
				tc.setup(w)

				if _, ok, err := r.observe(t.Context()); !ok || err != nil {
					t.Fatalf("observe = %v, %v", ok, err)
				}

				if dropped := !r.unpeered["i-1"]; dropped != tc.gone || r.relist != tc.gone {
					t.Errorf("dropped %v and the next observation lists %v, want both %v", dropped, r.relist, tc.gone)
				}
			})
		})
	}
}

// TestFailedReadsEndWithTheAdviceToStartAStoppedServer adds the advice to start the stopped instance to the error of
// reads that no server answers for ten minutes, and to the error of the plan, when the list shows a server machine that
// joined and does not run; with every server machine running, or a stopped client, it adds nothing.
func TestFailedReadsEndWithTheAdviceToStartAStoppedServer(t *testing.T) {
	t.Parallel()
	const advice = "; node prod-servers-0 (ID i-1) is stopped: if the servers have lost their quorum, " +
		"start that instance again and run the command again"
	for _, tc := range []struct {
		name  string
		setup func(w *holdWorld)
		want  string
	}{
		{"a server machine that is stopped", func(w *holdWorld) { w.stopMachine() }, advice},
		{"two server machines that are stopped, listed out of order", func(w *holdWorld) {
			w.victim.Ready, w.other.Ready = false, false
			w.listed = []cloud.Instance{w.other, w.victim}
		}, advice},
		{"every server machine runs", func(*holdWorld) {}, ""},
		{"a client machine that is stopped", func(w *holdWorld) {
			w.victim.Group, w.victim.Role = "workers", v1alpha1.RoleClient
			w.stopMachine()
		}, ""},
		{"a server machine that has not joined", func(w *holdWorld) {
			w.victim.Joined = false
			w.stopMachine()
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
				tc.setup(w)
				w.readErr = fmt.Errorf("no leader: %w", nomadops.ErrNotReady)
				r.listed = slices.Clone(w.listed)

				_, planErr := r.plan(t.Context())

				var runErr error
				for runErr == nil {
					_, ok, err := r.observe(t.Context())
					if ok {
						t.Fatal("observe read while every read fails")
					}
					runErr = err
					time.Sleep(rollPoll)
				}
				for what, err := range map[string]error{"the plan": planErr, "the run": runErr} {
					if !errors.Is(err, nomadops.ErrNotReady) {
						t.Errorf("%s ended with %v, want an error that matches ErrNotReady", what, err)
					}
					if tc.want != "" && !strings.HasSuffix(err.Error(), tc.want) {
						t.Errorf("%s ended with %q, want it to end with %q", what, err, tc.want)
					}
					if tc.want == "" && strings.Contains(err.Error(), "start that instance") {
						t.Errorf("%s ended with %q, want no advice", what, err)
					}
				}
			})
		})
	}
}

// TestCarryOfRemovePeerNotesTheRemovalOfAReAddedVictimOnly notes the removal in the held stop of the victim after its
// re-add, and not a removal that came before the run saw one.
func TestCarryOfRemovePeerNotesTheRemovalOfAReAddedVictimOnly(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.mustPass(r, "relisted")
		w.mustPass(r, "held")
		w.addVictim(true)
		w.mustPass(r, "carried remove-peer")
		if r.held["i-1"].removed {
			t.Error("the removal of a voter is noted as the removal of a re-added nonvoter")
		}
		w.mustPass(r, "held")
		w.addVictim(false)
		w.mustPass(r, "carried remove-peer")
		if !r.held["i-1"].removed {
			t.Error("the removal of the nonvoter that the run saw appear is not noted")
		}
	})
}

// TestHeldStopStretchBeginsWithTheObservationAndNotWithTheEndOfItsReads counts the 65 s from the moment the first
// observation of the hold began to read, when the second comes within 10 s of it.
func TestHeldStopStretchBeginsWithTheObservationAndNotWithTheEndOfItsReads(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.setMember("failed")
		w.mustPass(r, "relisted")
		start := time.Now()
		w.peersTook = 7 * time.Second
		w.mustPass(r, "held")
		w.peersTook = 0

		w.holdFor(r, start, 64*time.Second)

		w.mustPass(r, "stop sent")
		if got := time.Since(start); got != 65*time.Second {
			t.Errorf("the stop went %v after the first observation began, want 65s", got)
		}
	})
}

// TestFirstPollOfAHoldFailsWhenTheAPICannotLeaveTheVictim ends the run with the error of the API and notes no machine
// as without a peer.
func TestFirstPollOfAHoldFailsWhenTheAPICannotLeaveTheVictim(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.mustPass(r, "relisted")
		errNomad := errors.New("no client")
		r.s.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errNomad }

		got, err := w.pass(r)

		if got != "held" || !errors.Is(err, errNomad) || len(r.unpeered) != 0 {
			t.Errorf("pass = %q, %v with unpeered %v; want the error of the API and nothing noted", got, err, r.unpeered)
		}
	})
}

// TestHeldStopSendsNoCallWhenTheAPICannotBeMadeWithoutTheVictim ends the run with the error of the API, asks the cloud
// for nothing and notes nothing.
func TestHeldStopSendsNoCallWhenTheAPICannotBeMadeWithoutTheVictim(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.usualCourse(r)
		reading, ok, err := r.observe(t.Context())
		if !ok || err != nil {
			t.Fatalf("observe = %v, %v", ok, err)
		}
		errNomad := errors.New("no client")
		r.s.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errNomad }
		r.apiAt = nil

		handled, err := r.holdStop(t.Context(), w.step(), reading)

		if !handled || !errors.Is(err, errNomad) {
			t.Errorf("holdStop = %v, %v; want the error of the API", handled, err)
		}
		if len(w.stops) != 0 || len(r.stopping) != 0 || len(r.heldSent) != 0 || len(w.warnings) != 0 {
			t.Errorf("stops %v, stopping %v, heldSent %v, warnings %v; want nothing sent and nothing noted", w.stops,
				r.stopping, r.heldSent, w.warnings)
		}
	})
}

// TestFailedPlanReadAddsNoAdviceForAnErrorThatIsNotAMissingAnswer keeps the error of a read that failed for another
// reason as it is, also when the list shows a server machine stopped.
func TestFailedPlanReadAddsNoAdviceForAnErrorThatIsNotAMissingAnswer(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.stopMachine()
		r.listed = slices.Clone(w.listed)
		errRead := errors.New("forbidden")
		w.readErr = errRead

		_, err := r.plan(t.Context())

		if !errors.Is(err, errRead) || strings.Contains(err.Error(), "start that instance") {
			t.Errorf("plan = %v, want the read's error with no advice", err)
		}
	})
}

// TestHeldStopCountsTheAgeOfTheReAddToTheMomentTheStopGoes holds a stop whose observation began within 10 s of the
// observation of the re-add and whose reads ended later.
func TestHeldStopCountsTheAgeOfTheReAddToTheMomentTheStopGoes(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	h := &heldStop{first: base, since: at(0), last: at(0)}
	h.observe(at(2), true, false)
	h.noteRemoval()
	h.observe(at(4), false, false)

	if !h.allows(at(4), at(12), true) {
		t.Error("does not allow a stop that goes 10 s after the observation of the re-add")
	}
	if h.allows(at(4), at(13), true) {
		t.Error("allows a stop that goes 11 s after the observation of the re-add")
	}
}

// TestCarryOfRemovePeerNotesNoRemovalWhoseCallFailed does not count a removal of the re-added victim that failed: the
// run has not removed that peer.
func TestCarryOfRemovePeerNotesNoRemovalWhoseCallFailed(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.mustPass(r, "relisted")
		w.mustPass(r, "held")
		w.addVictim(false)
		w.removeErr = fmt.Errorf("no leader: %w", nomadops.ErrNotReady)

		w.mustPass(r, "carried remove-peer")

		if r.held["i-1"].removed {
			t.Error("a removal whose call failed is noted as the removal of the re-added nonvoter")
		}
	})
}

// TestHeldStopDoesNotAllowAStopWithoutAStretchOfAbsence holds the stop of a victim that the last observation showed in
// the Raft configuration, whatever its member reads: no stretch of absence has begun.
func TestHeldStopDoesNotAllowAStopWithoutAStretchOfAbsence(t *testing.T) {
	t.Parallel()
	base := time.Now()
	at := func(s int) moment { return momentAt(base, time.Duration(s)*time.Second, time.Duration(s)*time.Second) }
	h := &heldStop{first: base, since: at(0), last: at(0)}
	h.observe(at(2), true, true)

	if h.allows(at(70), at(70), false) {
		t.Error("allows a stop of a victim that the last observation showed in the Raft configuration")
	}
}

// TestWaitForAStopThatWasNotHeldKeepsTheLastStep leaves the repeat guard as it is while the cloud lists a machine as
// running whose stop beside two other voters the run sent.
func TestWaitForAStopThatWasNotHeldKeepsTheLastStep(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.addVoters(1)
		w.mustPass(r, "relisted")
		w.mustPass(r, "stop sent")

		w.mustPass(r, "waited")

		if r.last != keyOf(w.step()) || r.tries != 1 {
			t.Errorf("the run remembers the step %v after %d tries, want the stop after one try", r.last, r.tries)
		}
	})
}

// TestLaterPollOfAHoldTakesTheVictimOutOfTheAPIAgain leaves the victim out of the API at a later poll of the hold too,
// when a removal that failed has taken it back in between.
func TestLaterPollOfAHoldTakesTheVictimOutOfTheAPIAgain(t *testing.T) {
	t.Parallel()
	bubble(t, func(t *testing.T, w *holdWorld, r *rollRun) {
		w.mustPass(r, "relisted")
		w.mustPass(r, "held")
		delete(r.unpeered, "i-1")

		w.mustPass(r, "held")

		if want := []string{"203.0.113.2:4646"}; !slices.Equal(r.apiAt, want) || !r.unpeered["i-1"] {
			t.Errorf("the API is over %v and unpeered is %v, want the victim out of the API and noted", r.apiAt, r.unpeered)
		}
	})
}

// TestStartHoldRoundsWhatIsLeftUp reports what is left of the limit in whole seconds, rounded up, so that a hold that
// starts in the last half second of the limit does not report that no time is left.
func TestStartHoldRoundsWhatIsLeftUp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct{ since, want time.Duration }{
			{holdTimeout - 300*time.Millisecond, time.Second},
			{15*time.Second + 300*time.Millisecond, holdTimeout - 15*time.Second},
			{0, holdTimeout},
		} {
			var got []time.Duration
			r := &rollRun{rollLoop: newRollLoop(), s: &Service{OnProgress: func(p Progress) {
				got = append(got, p.Nomad.Deadline)
			}}}
			r.startHold(rollout.Machine{Name: "prod-servers-0"}, &heldStop{first: time.Now().Add(-tc.since)})
			if !slices.Equal(got, []time.Duration{tc.want}) {
				t.Errorf("a hold that starts %v into the limit reported %v, want %v", tc.since, got, tc.want)
			}
		}
	})
}
