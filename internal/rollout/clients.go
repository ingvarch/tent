package rollout

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

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
		c.purgeOrphan, c.deleteRemoved, c.create, c.startRemoval, c.drain, c.waitDrained, c.waitJoined, c.waitOrphan,
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
	return ok && m.Joined && m.Ready && n.Status == nodeReady && n.Eligible && !n.Draining
}

// orphans returns the nodes of the group's name pattern that no machine of the cluster has, by name and ID.
func (c *clientGroup) orphans() []Node {
	var orphans []Node
	for _, n := range c.s.Nomad.Nodes {
		if isNodeOf(c.s.Cluster, c.g.Name, n.Name) && !c.hasMachine(n) {
			orphans = append(orphans, n)
		}
	}
	slices.SortStableFunc(orphans, func(a, b Node) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return orphans
}

// hasMachine reports whether a listed machine of the cluster has the node's name and address, or a machine of the
// group of that name lacks an address, so that its node cannot be told from an orphan.
func (c *clientGroup) hasMachine(n Node) bool {
	return slices.ContainsFunc(c.s.Machines, func(m Machine) bool {
		return at(m, n) || (m.Group == c.g.Name && m.Name == n.Name && !m.PrivateIP.IsValid())
	})
}

// isNodeOf reports whether name is <cluster>-<group>-<digits>.
func isNodeOf(cluster, group, name string) bool {
	index, ok := strings.CutPrefix(name, cluster+"-"+group+"-")
	return ok && index != "" && strings.Trim(index, "0123456789") == ""
}

// purgeOrphan purges a node that is down and that no machine has.
func (c *clientGroup) purgeOrphan() (Step, bool, error) {
	for _, n := range c.orphans() {
		if n.Status == nodeDown {
			return Step{Action: Purge, Group: c.g.Name, Node: n}, true, nil
		}
	}
	return Step{}, false, nil
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
	upToDate := 0
	perZone := map[string]int{}
	for _, m := range c.ms {
		if !outdated(c.s, c.g, m) {
			upToDate++
			perZone[m.Zone]++
		}
	}
	if upToDate >= c.g.Size || len(c.ms) >= c.g.Size+c.g.MaxSurge {
		return Step{}, false, nil
	}
	if err := c.requireServersAtVersion(); err != nil {
		return c.drainOr(err)
	}
	taken := make(map[string]bool, len(c.s.Machines))
	for _, m := range c.s.Machines {
		taken[m.Name] = true
	}
	m := Machine{
		Name:  FreeName(c.s.Cluster, c.g.Name, taken),
		Group: c.g.Name,
		Role:  c.g.Role,
		Zone:  LeastUsedZone(c.g.Zones, perZone),
	}
	return Step{Action: Create, Group: c.g.Name, Machine: m}, true, nil
}

// startRemoval starts the removal of the first victim when the group keeps enough available nodes without it. A
// victim that is not available costs no availability. A victim whose node is missing or down is deleted, any other
// is marked ineligible. A joined machine without a private address is no victim: its node may still run work. It
// refuses, as create does, while a server runs an older Nomad, unless a drain is due.
func (c *clientGroup) startRemoval() (Step, bool, error) {
	var victims []Machine
	perZone := map[string]int{}
	available := 0
	for _, m := range c.ms {
		perZone[m.Zone]++
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
	victimOrder(victims, c.available, perZone)
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

// waitJoined waits for a machine that has not joined.
func (c *clientGroup) waitJoined() (Step, bool, error) {
	for _, m := range c.ms {
		if !m.Joined {
			return Step{Action: WaitJoined, Group: c.g.Name, Machine: m}, true, nil
		}
	}
	return Step{}, false, nil
}

// waitOrphan waits for a node that no machine has to go down.
func (c *clientGroup) waitOrphan() (Step, bool, error) {
	if orphans := c.orphans(); len(orphans) > 0 {
		return Step{Action: WaitNodeDown, Group: c.g.Name, Node: orphans[0]}, true, nil
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
			why = append(why, m.Name+" "+c.unavailableWhy(m))
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

// unavailableWhy says why a machine of the group is not available.
func (c *clientGroup) unavailableWhy(m Machine) string {
	n, ok := c.nodes[m.ID]
	switch {
	case !m.Ready:
		return "is not running"
	case !ok && !m.PrivateIP.IsValid():
		return "has no node in Nomad: the cloud reports no private address for it"
	case !ok:
		return "has no node in Nomad"
	case n.Status != nodeReady:
		return fmt.Sprintf("is %s in Nomad", n.Status)
	case !n.Eligible:
		return "is not eligible"
	default:
		return "is draining"
	}
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
