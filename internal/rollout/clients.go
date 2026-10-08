package rollout

import (
	"slices"

	"golang.org/x/mod/semver"

	"github.com/ingvarch/tent/internal/english"
)

// clientGroup is a client group with its machines and their nodes at one moment.
type clientGroup struct {
	s     State
	g     Group
	ms    []Machine       // the group's machines by name
	nodes map[string]Node // the node of each machine that has one, by machine ID
}

// nextClient returns the first step of the group that applies, and false when the group is done.
func nextClient(s State, g Group) (Step, bool, error) {
	c := clientGroup{s: s, g: g, ms: machinesOf(s, g), nodes: map[string]Node{}}
	for _, m := range c.ms {
		if n, ok := nodeOf(s.Nomad.Nodes, m); ok {
			c.nodes[m.ID] = n
		}
	}
	rules := []func() (Step, bool, error){
		func() (Step, bool, error) { return purgeOrphan(c.s, c.g) }, c.deleteRemoved, c.create, c.startRemoval, c.drain,
		c.waitDrained, func() (Step, bool, error) { return waitJoined(c.g, c.ms) },
		func() (Step, bool, error) { return waitOrphan(c.s, c.g) },
	}
	for _, rule := range rules {
		if step, found, err := rule(); err != nil || found {
			return step, found, err
		}
	}
	return c.finish()
}

// removing reports whether the machine's removal has started: it has joined, its node is ineligible, draining or
// drained for it, and it is outdated. (An up to date machine whose node an operator made ineligible is not removed.)
func (c *clientGroup) removing(m Machine) bool {
	n, ok := c.nodes[m.ID]
	return ok && m.Joined && outdated(c.s, c.g, m) && (!n.Eligible || n.Draining || n.DrainedFor == m.ID)
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
		if c.removing(m) && (n.Status == nodeDown || (n.DrainedFor == m.ID && !n.Draining)) {
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

// create creates a node while the up to date machines are fewer than the size and the group is below its surge limit.
// It refuses while a server runs an older Nomad than the new node would, unless a drain is due.
func (c *clientGroup) create() (Step, bool, error) {
	if len(upToDate(c.s, c.g, c.ms)) >= c.g.Size || len(c.ms) >= c.g.Size+c.g.MaxSurge {
		return Step{}, false, nil
	}
	if err := c.requireServersAtVersion(); err != nil {
		return c.drainOr(err)
	}
	return Step{Action: Create, Group: c.g.Name, Machine: newMachine(c.s, c.g, c.ms)}, true, nil
}

// startRemoval starts the removal of the first victim when the group keeps enough available nodes without it. A
// victim that is not available costs no availability. A victim whose node is missing or down is deleted, any other
// is marked ineligible. A joined machine without a private address is no victim: its node may still run work. It
// refuses, as create does, while a server runs an older Nomad, unless a drain is due.
func (c *clientGroup) startRemoval() (Step, bool, error) {
	var victims []Machine
	available := 0
	for _, m := range c.ms {
		if c.available(m) {
			available++
		}
		if m.Joined && m.PrivateIP.IsValid() && !c.removing(m) && outdated(c.s, c.g, m) {
			victims = append(victims, m)
		}
	}
	if len(victims) == 0 {
		return Step{}, false, nil
	}
	victimOrder(victims, zoneCounts(c.ms), c.available)
	v := victims[0]
	if c.available(v) && available-1 < c.g.Size-c.g.MaxUnavailable {
		return Step{}, false, nil
	}
	if err := c.requireServersAtVersion(); err != nil {
		return c.drainOr(err)
	}
	n, ok := c.nodes[v.ID]
	action := MarkIneligible
	if !ok || n.Status == nodeDown {
		action = Delete
	}
	return Step{Action: action, Group: c.g.Name, Machine: v, Node: n}, true, nil
}

// drainOr gives the drain of a machine under removal when one is due, and the refusal otherwise: a removal that has
// started still drains while a server runs an older Nomad.
func (c *clientGroup) drainOr(err error) (Step, bool, error) {
	if step, found, _ := c.drain(); found {
		return step, true, nil
	}
	return Step{}, false, err
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

// finish ends the group when no machine is outdated, and refuses it otherwise: no rule found a step to take.
func (c *clientGroup) finish() (Step, bool, error) {
	if !slices.ContainsFunc(c.ms, func(m Machine) bool { return outdated(c.s, c.g, m) }) {
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
	return Step{}, false, refuse(
		"node group %s: cannot go on: %swith maxSurge %d and maxUnavailable %d no outdated node can be replaced",
		c.g.Name, reasons, c.g.MaxSurge, c.g.MaxUnavailable)
}

// requireServersAtVersion refuses while a server runs an older Nomad than a new node would.
func (c *clientGroup) requireServersAtVersion() error {
	for _, srv := range c.s.Nomad.Servers {
		if semver.Compare("v"+srv.Version, "v"+c.s.Version) < 0 {
			return refuse("node group %s: a new node would run Nomad %s, newer than the %s of server %s; "+
				"roll the servers first", c.g.Name, c.s.Version, srv.Version, srv.Name)
		}
	}
	return nil
}
