package rollout

import (
	"fmt"
	"slices"

	"golang.org/x/mod/semver"

	"github.com/ingvarch/tent/internal/english"
)

// clientGroup is a client group with its machines and their nodes at one moment.
type clientGroup struct {
	groupView
	nodes map[string]Node // the node of each machine that has one, by machine ID
	// leaving holds the machines under removal, by machine ID
	leaving map[string]bool
}

// newClientGroup reads the nodes of the group's machines from the state, and finds the machines whose removal has
// started and that the run takes out. In a shrink it also lists the machines that the group loses.
func newClientGroup(s State, mode Mode, g Group) *clientGroup {
	c := &clientGroup{groupView: newGroupView(s, mode, g), nodes: map[string]Node{}, leaving: map[string]bool{}}
	for _, m := range c.ms {
		if n, ok := nodeOf(s.Nomad.Nodes, m); ok {
			c.nodes[m.ID] = n
		}
	}
	if mode == Shrink {
		c.lose = c.surplus()
	}
	for _, m := range c.leavers() {
		c.leaving[m.ID] = true
	}
	return c
}

// nextClient returns the first step of the group that applies, and false when the group is done.
func nextClient(s State, mode Mode, g Group) (Step, bool, error) {
	c := newClientGroup(s, mode, g)
	rules := []func() (Step, bool, error){
		func() (Step, bool, error) { return purgeOrphan(c.s, c.g) }, c.deleteRemoved, c.create, c.startRemoval, c.drain,
		c.waitDrained, c.settleUnjoined, func() (Step, bool, error) { return waitOrphan(c.s, c.g) },
	}
	for _, rule := range rules {
		if step, found, err := rule(); err != nil || found {
			return step, found, err
		}
	}
	return c.finish()
}

// surplus lists the machines that a shrink takes from the group: those whose removal has started, a completed drain
// before a drain under way before a node that is only ineligible, then those that have not joined, the outdated ones
// and those whose node is not available. A machine that joined and for which the cloud reports no private address is
// left out: its node may still run work.
func (c *clientGroup) surplus() []Machine {
	beyond := func(m Machine, rank int) bool { return c.started(m) && c.drainRank(m) > rank }
	return c.surplusBy(
		func(m Machine) bool { return m.Joined && !m.PrivateIP.IsValid() },
		not(c.started),
		func(m Machine) bool { return beyond(m, 0) },
		func(m Machine) bool { return beyond(m, 1) },
		func(m Machine) bool { return m.Joined },
		func(m Machine) bool { return !outdated(c.s, c.g, m) },
		c.available,
	)
}

// started reports whether the removal of the machine's node has begun: the machine has joined, and its node is
// ineligible or draining.
func (c *clientGroup) started(m Machine) bool {
	n, ok := c.nodes[m.ID]
	return ok && m.Joined && (!n.Eligible || n.Draining)
}

// removing reports whether the machine's removal has started and the run takes it out. (An up to date machine whose
// node an operator made ineligible is not removed by a roll, and a shrink takes no more of such nodes than the group
// must lose.)
func (c *clientGroup) removing(m Machine) bool { return c.leaving[m.ID] }

// leavers returns the machines whose removal has started and that the run takes out. In a roll these are the outdated
// ones. A shrink takes those that the group loses.
func (c *clientGroup) leavers() []Machine {
	if c.mode == Shrink {
		return slices.DeleteFunc(slices.Clone(c.lose), func(m Machine) bool { return !c.started(m) })
	}
	return slices.DeleteFunc(slices.Clone(c.ms), func(m Machine) bool {
		return !c.started(m) || !outdated(c.s, c.g, m)
	})
}

// drainRank orders the nodes that a shrink may take: a completed drain, then a drain under way, then the others.
func (c *clientGroup) drainRank(m Machine) int {
	n := c.nodes[m.ID]
	switch {
	case n.Draining:
		return 1
	case drainedFor(m, n):
		return 0
	default:
		return 2
	}
}

// available reports whether the machine's node can take work: the machine has joined and runs, and its node is ready,
// eligible and not draining. (A machine under removal has an ineligible or draining node, or is deleted at once.)
func (c *clientGroup) available(m Machine) bool {
	n, ok := c.nodes[m.ID]
	return m.Joined && nodeWhy(m, n, ok) == ""
}

// deleteRemoved deletes a machine under removal whose node is down or whose drain has completed.
func (c *clientGroup) deleteRemoved() (Step, bool, error) {
	for _, m := range c.ms {
		n := c.nodes[m.ID]
		if c.removing(m) && (n.Status == nodeDown || (drainedFor(m, n) && !n.Draining)) {
			return Step{Action: Delete, Group: c.g.Name, Machine: m, Node: n}, true, nil
		}
	}
	return Step{}, false, nil
}

// drain drains the node of a machine under removal that is not draining. (A node that a drain for the machine has
// emptied was dealt with by deleteRemoved, so the node is ineligible and has not been drained for the machine.)
func (c *clientGroup) drain() (Step, bool, error) {
	for _, m := range c.ms {
		n := c.nodes[m.ID]
		if c.removing(m) && !n.Draining {
			return Step{Action: Drain, Group: c.g.Name, Machine: m, Node: n, Deadline: c.g.DrainTimeout}, true, nil
		}
	}
	return Step{}, false, nil
}

// create creates a node in a roll while the up to date machines are fewer than the size and the group is below its
// surge limit. It is held back as serversHold says, unless a drain is due.
func (c *clientGroup) create() (Step, bool, error) {
	if c.mode != Roll || len(upToDate(c.s, c.g, c.ms)) >= c.g.Size || len(c.ms) >= c.g.Size+c.g.MaxSurge {
		return Step{}, false, nil
	}
	if step, held, err := c.serversHold(); held || err != nil {
		return c.drainOr(step, held, err)
	}
	return Step{Action: Create, Group: c.g.Name, Machine: newMachine(c.s, c.g, c.ms)}, true, nil
}

// startRemoval starts the removal of the first victim when the group keeps enough available nodes without it. A
// victim that is not available costs no availability. A victim whose node is missing or down is deleted, any other
// is marked ineligible. A shrink takes the first machine that the group loses and whose removal has not started, and
// leaves it to settleUnjoined when it has not joined. A roll takes an outdated machine that has joined and has a
// private address, and is held back, as create is, unless a drain is due.
func (c *clientGroup) startRemoval() (Step, bool, error) {
	v, ok := c.firstVictim()
	if !ok {
		return Step{}, false, nil
	}
	available := 0
	for _, m := range c.ms {
		if c.available(m) {
			available++
		}
	}
	if !v.Joined || (c.available(v) && available-1 < c.g.Size-c.g.MaxUnavailable) {
		return Step{}, false, nil
	}
	if step, held, err := c.serversHold(); held || err != nil {
		return c.drainOr(step, held, err)
	}
	n, ok := c.nodes[v.ID]
	step := Step{Group: c.g.Name, Machine: v, Node: n}
	if !ok || n.Status == nodeDown {
		step.Action = Delete
	} else if step.Action = evictionOf(v, n); step.Action == Drain {
		step.Deadline = c.g.DrainTimeout
	}
	return step, true, nil
}

// firstVictim returns the machine whose removal starts next, and false when there is none.
func (c *clientGroup) firstVictim() (Machine, bool) {
	if c.mode == Shrink {
		i := slices.IndexFunc(c.lose, func(m Machine) bool { return !c.removing(m) })
		if i < 0 {
			return Machine{}, false
		}
		return c.lose[i], true
	}
	var victims []Machine
	for _, m := range c.ms {
		if !c.removing(m) && m.Joined && m.PrivateIP.IsValid() && outdated(c.s, c.g, m) {
			victims = append(victims, m)
		}
	}
	if len(victims) == 0 {
		return Machine{}, false
	}
	sortVictims(victims, c.ms, false, c.available)
	return victims[0], true
}

// drainOr gives the drain of a machine under removal when one is due, and what holds the roll back otherwise: a
// removal that has started still drains while the servers' versions hold it back.
func (c *clientGroup) drainOr(hold Step, held bool, err error) (Step, bool, error) {
	if step, found, _ := c.drain(); found {
		return step, true, nil
	}
	return hold, held, err
}

// waitDrained waits for the drain of a machine under removal.
func (c *clientGroup) waitDrained() (Step, bool, error) {
	for _, m := range c.ms {
		if n := c.nodes[m.ID]; c.removing(m) && n.Draining {
			return Step{Action: WaitDrained, Group: c.g.Name, Machine: m, Node: n}, true, nil
		}
	}
	return Step{}, false, nil
}

// finish ends the group when the run has nothing left to do with it, and refuses it otherwise: no rule found a step to
// take. A roll has something left while a machine is outdated, a shrink while the group is above its size.
func (c *clientGroup) finish() (Step, bool, error) {
	pending := len(c.ms) > c.g.Size
	if c.mode == Roll {
		pending = slices.ContainsFunc(c.ms, func(m Machine) bool { return outdated(c.s, c.g, m) })
	}
	if !pending {
		return Step{}, false, nil
	}
	var why []string
	for _, m := range c.ms {
		if !c.available(m) {
			n, ok := c.nodes[m.ID]
			why = append(why, m.Name+" "+nodeWhy(m, n, ok))
		}
	}
	reasons := ""
	if len(why) > 0 {
		reasons = english.And(why) + "; "
	}
	limits := fmt.Sprintf("with maxSurge %d and maxUnavailable %d no outdated node can be replaced", c.g.MaxSurge,
		c.g.MaxUnavailable)
	if c.mode == Shrink {
		limits = fmt.Sprintf("with maxUnavailable %d no surplus node can be removed", c.g.MaxUnavailable)
	}
	return Step{}, false, refuse("node group %s: cannot go on: %s%s", c.g.Name, reasons, limits)
}

// serversHold says what keeps a roll from a new node or a new removal: a refusal while a server runs an older Nomad
// than a new node would, otherwise a wait while a server reports no version, as a server does for a moment after it
// joins, before autopilot reports it. A shrink is never held back.
func (c *clientGroup) serversHold() (Step, bool, error) {
	if c.mode != Roll {
		return Step{}, false, nil
	}
	unknown, voters := false, 0
	for _, srv := range c.s.Nomad.Servers {
		if srv.Voter {
			voters++
		}
		if srv.Version == "" {
			unknown = true
		} else if semver.Compare("v"+srv.Version, "v"+c.s.Version) < 0 {
			return Step{}, false, refuse("node group %s: a new node would run Nomad %s, newer than the %s of server %s; "+
				"roll the servers first", c.g.Name, c.s.Version, srv.Version, srv.Name)
		}
	}
	if unknown {
		return Step{Action: WaitHealthy, Group: c.g.Name, Voters: voters}, true, nil
	}
	return Step{}, false, nil
}
