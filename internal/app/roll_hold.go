package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// The limits of a held stop: the stop of a server that has no peer in the Raft configuration, beside fewer than two
// voters. Nomad's leader adds a removed server whose member is alive again at its next reconcile, once a minute, and
// autopilot promotes it 10 to 20 s later, also after its machine has stopped; a stop that the run sends right after a
// reconcile that it saw leaves most of a minute to the next one.
const (
	// reAddGap is the longest time between two observations of a held stop that still count as one after the other.
	reAddGap = 10 * time.Second
	// stopAfterReAdd is how long after the observation that showed the leader's re-add of a server its held stop may be
	// sent: three times what a run on Vultr needs from that observation to the stop call.
	stopAfterReAdd = 10 * time.Second
	// absentBeforeStop is how long a running server whose member is not alive must stay out of the Raft configuration,
	// in the run's own observations, before its held stop is sent without a re-add: the leader reconciles every 60 s.
	absentBeforeStop = 65 * time.Second
	// haltCallTimeout is how long the run waits for the cloud's answer to the stop call of a held stop.
	haltCallTimeout = 10 * time.Second
	// holdTimeout is how long the holds of one victim's stop last together in a run, from the first poll of the first.
	holdTimeout = 5 * time.Minute
)

// moment is a time by both clocks of the operator's machine. Go's monotonic clock stops while the machine sleeps, so a
// time by it alone can be too short; the wall clock can be set.
type moment struct {
	mono time.Time // with its monotonic reading
	wall time.Time // the same instant without it
}

// now returns the moment of the call.
func now() moment {
	t := time.Now()
	return moment{mono: t, wall: t.Round(0)}
}

// until returns how long m was before to: the larger of what the two clocks say.
func (m moment) until(to moment) time.Duration {
	return max(to.mono.Sub(m.mono), to.wall.Sub(m.wall))
}

// heldStop is what a run remembers of a victim whose stop it holds: a running machine with no server in the Raft
// configuration, beside fewer than two voters.
type heldStop struct {
	first time.Time // the first poll of a hold of this victim, for holdTimeout
	// since is the first observation of the stretch that showed the machine out of the Raft configuration; zero while
	// the last observation showed it in.
	since   moment
	last    moment // the last observation
	reAdded moment // the observation that showed it in the configuration again, as a nonvoter, within reAddGap of last
	removed bool   // the run removed its peer after reAdded
}

// observe notes an observation that began at at: whether the machine's server was in the Raft configuration, and
// whether it voted. An observation that comes more than reAddGap after the one before sees no re-add, and starts the
// stretch of absence again when it shows the server out of the configuration; one that shows the server in the
// configuration ends the stretch.
func (h *heldStop) observe(at moment, inRaft, voter bool) {
	continued := h.last.until(at) <= reAddGap
	absent := !h.since.mono.IsZero()
	switch {
	case inRaft:
		if continued && absent && !voter {
			h.reAdded, h.removed = at, false
		}
		h.since = moment{}
	case !absent || !continued:
		h.since = at
	}
	h.last = at
}

// noteRemoval notes that the run removed the machine's peer: it counts for the stop only after a re-add that the run
// saw.
func (h *heldStop) noteRemoval() { h.removed = !h.reAdded.mono.IsZero() }

// allows reports whether the held stop may be sent at sent, on the observation that began at at and showed the machine
// out of the Raft configuration. alive says whether that observation lists the machine's member as alive. It may be
// sent right after a re-add that the run saw and removed, when the observation of the re-add is at most stopAfterReAdd
// old; or after absentBeforeStop out of the Raft configuration, counted by the monotonic clock from the start of the
// stretch, when the member is not alive and the observation is at most reAddGap old.
func (h *heldStop) allows(at, sent moment, alive bool) bool {
	if h.removed && h.reAdded.until(sent) <= stopAfterReAdd {
		return true
	}
	absent := !h.since.mono.IsZero() && at.mono.Sub(h.since.mono) >= absentBeforeStop
	return !alive && absent && at.until(sent) <= reAddGap
}

// memberIsAlive reports whether the members list one at the private address addr as alive.
func memberIsAlive(members []nomadops.Member, addr netip.Addr) bool {
	for _, m := range members {
		if m.Address == addr && m.Status == rollout.MemberAlive {
			return true
		}
	}
	return false
}

// heldVictim returns the machine of the step, and true when the step is a stop that the run holds: the stop of a
// machine of a server or combined group that the last list shows running and that the run has not stopped, whose
// server is not in the Raft configuration, while fewer than two of the servers vote.
func (r *rollRun) heldVictim(step rollout.Step, servers []rollout.Server) (cloud.Instance, bool) {
	in, _ := instanceByID(r.listed, step.Machine.ID)
	if step.Action != rollout.Stop || !in.Ready || !isServerMachine(r.model, in) {
		return in, false
	}
	if _, stopped := r.stopping[in.ID]; stopped {
		return in, false
	}
	if _, hasServer := serverAt(servers, in.PrivateIP); hasServer {
		return in, false
	}
	return in, voting(servers) < 2
}

// noteHeld notes the observation, with the servers of its Raft configuration, in each held stop.
func (r *rollRun) noteHeld(servers []rollout.Server) {
	for id, h := range r.held {
		in, _ := instanceByID(r.listed, id)
		srv, inRaft := serverAt(servers, in.PrivateIP)
		h.observe(r.observedAt, inRaft, srv.Voter)
	}
}

// holdStop holds the step when it is a stop that the run holds (see heldVictim): it sends it when the held stop allows,
// and otherwise polls once, with the victim out of the run's API, and forgets the step it carried out last, so that
// the removal of a server that the leader adds again is a first try. It reports whether it handled the step; it
// ends a hold that is open when the step is another. It fails once the holds of the victim have lasted holdTimeout.
func (r *rollRun) holdStop(ctx context.Context, step rollout.Step, reading nomadReading) (bool, error) {
	in, held := r.heldVictim(step, reading.state().Servers)
	if !held {
		r.endHold(nil)
		return false, nil
	}
	m := step.Machine
	h, ok := r.held[m.ID]
	if !ok {
		h = &heldStop{first: time.Now(), since: r.observedAt, last: r.observedAt}
		r.held[m.ID] = h
	}
	if h.allows(r.observedAt, now(), memberIsAlive(reading.members, in.PrivateIP)) {
		r.endHold(nil)
		return true, r.sendHeldStop(ctx, step)
	}
	if time.Since(h.first) >= holdTimeout {
		return true, fmt.Errorf("tent could not stop node %s right after a reconcile of Nomad's leader within %s; "+
			"both servers run; run tent rolling-update cluster again", m.Name, holdTimeout)
	}
	r.startHold(m, h)
	if !r.unpeered[m.ID] {
		if _, err := r.unpeer(m); err != nil {
			return true, fmt.Errorf("%s: %w", step, err)
		}
	}
	r.forgetStep()
	return true, r.sleep(ctx)
}

// sendHeldStop sends the stop of the step and forgets the hold of its machine. The stop is the step carried out last.
func (r *rollRun) sendHeldStop(ctx context.Context, step rollout.Step) error {
	r.wrote = true
	r.last, r.tries, r.settled = keyOf(step), 1, false
	err := r.stopHeld(ctx, step.Machine)
	delete(r.held, step.Machine.ID)
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	return nil
}

// forgetStep makes the next step a first try, with no poll before it.
func (r *rollRun) forgetStep() { r.last, r.tries, r.settled = stepKey{}, 0, false }

// startHold reports the hold of the stop of m as started, with what is left of holdTimeout in whole seconds, rounded
// up, unless it has been.
func (r *rollRun) startHold(m rollout.Machine, h *heldStop) {
	if r.holding != nil {
		return
	}
	left := (holdTimeout - time.Since(h.first) + time.Second - 1).Truncate(time.Second)
	event := NomadEvent{Action: NomadReconcile, Node: m.Name, Deadline: left}
	r.holding = &event
	r.s.progress(Progress{Step: NodeStarted, Nomad: &event})
}

// endHold reports the hold, if there is one, as failed with err, or as done when err is nil.
func (r *rollRun) endHold(err error) {
	if r.holding == nil {
		return
	}
	event := *r.holding
	r.holding = nil
	r.ended(event, err)
}

// withStoppedAdvice returns err, and for reads that no server answered adds the advice to start the first machine
// of a server or combined group that the last list shows joined and not running: the servers may have lost their
// quorum with it.
func (r *rollRun) withStoppedAdvice(err error) error {
	if !errors.Is(err, nomadops.ErrNotReady) {
		return err
	}
	stopped := slices.DeleteFunc(slices.Clone(r.listed), func(in cloud.Instance) bool {
		return in.Ready || !in.Joined || !isServerMachine(r.model, in)
	})
	if len(stopped) == 0 {
		return err
	}
	slices.SortFunc(stopped, compareName)
	return fmt.Errorf("%w; node %s (ID %s) is stopped: if the servers have lost their quorum, start that instance "+
		"again and run the command again", err, stopped[0].Name, stopped[0].ID)
}
