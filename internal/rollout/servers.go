package rollout

import (
	"slices"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/english"
)

// Statuses of a member of the gossip pool as Serf lists them.
const (
	memberAlive  = "alive"
	memberFailed = "failed"
)

// stableMargin is added to the refresh interval to make the stability window.
const stableMargin = 10 * time.Second

// serverGroup is a server or combined group with its machines and their servers, members and nodes at one moment.
type serverGroup struct {
	s       State
	g       Group
	ms      []Machine         // the group's machines by name
	servers map[string]Server // the server of each machine that has one, by machine ID
	members map[string]Member // the gossip member of each machine that has one, by machine ID
	nodes   map[string]Node   // combined groups: the client node of each machine that has one, by machine ID
	voters  int               // how many servers of the Raft configuration vote
}

// nextServer returns the first step of the group that applies, and false when the group is done.
func nextServer(s State, g Group) (Step, bool, error) {
	sg := serverGroup{
		s: s, g: g, ms: machinesOf(s, g), servers: map[string]Server{}, members: map[string]Member{},
		nodes: map[string]Node{},
	}
	for _, m := range sg.ms {
		if !m.PrivateIP.IsValid() {
			continue
		}
		atServer := func(srv Server) bool { return srv.Address.Addr() == m.PrivateIP }
		if i := slices.IndexFunc(s.Nomad.Servers, atServer); i >= 0 {
			sg.servers[m.ID] = s.Nomad.Servers[i]
		}
		atMember := func(mem Member) bool { return mem.Address == m.PrivateIP }
		if i := slices.IndexFunc(s.Nomad.Members, atMember); i >= 0 {
			sg.members[m.ID] = s.Nomad.Members[i]
		}
		if g.Role == v1alpha1.RoleCombined {
			if n, ok := nodeOf(s.Nomad.Nodes, m); ok {
				sg.nodes[m.ID] = n
			}
		}
	}
	for _, srv := range s.Nomad.Servers {
		if srv.Voter {
			sg.voters++
		}
	}
	rules := []func() (Step, bool, error){
		func() (Step, bool, error) { return waitJoined(g, sg.ms) }, sg.removeVictim, sg.settleOrphans, sg.requireSize,
		sg.create,
	}
	for _, rule := range rules {
		if step, found, err := rule(); err != nil || found {
			return step, found, err
		}
	}
	return Step{}, false, nil
}

// started reports whether the removal of the machine has begun: it is stopped, or its server is gone or no longer
// votes, or, in a combined group, its node is ineligible, draining or drained for it. A running machine without a
// private address counts as not started, so the checks at rest refuse it.
func (sg *serverGroup) started(m Machine) bool {
	if !m.Ready {
		return true
	}
	srv, hasServer := sg.servers[m.ID]
	n, hasNode := sg.nodes[m.ID]
	return m.PrivateIP.IsValid() && (!hasServer || !srv.Voter) ||
		hasNode && (!n.Eligible || n.Draining || n.DrainedFor == m.ID)
}

// victim returns the outdated machine to remove while the group has more machines than its size: one whose removal
// has started, else the first by the order of victims.
func (sg *serverGroup) victim() (Machine, bool) {
	if len(sg.ms) <= sg.g.Size {
		return Machine{}, false
	}
	var candidates []Machine
	for _, m := range sg.ms {
		if outdated(sg.s, sg.g, m) {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Machine{}, false
	}
	victimOrder(candidates, zoneCounts(sg.ms),
		func(m Machine) bool { return !sg.started(m) },
		func(m Machine) bool { return sg.servers[m.ID].Healthy },
		func(m Machine) bool { return sg.servers[m.ID].Leader },
	)
	return candidates[0], true
}

// removeVictim takes the next step of the removal of the victim, if the group has one.
func (sg *serverGroup) removeVictim() (Step, bool, error) {
	v, ok := sg.victim()
	if !ok {
		return Step{}, false, nil
	}
	if !sg.started(v) {
		if err := sg.checkAtRest(); err != nil {
			return Step{}, false, err
		}
		if step, wait := sg.waitStable(v); wait {
			return step, true, nil
		}
	}
	step, err := sg.removal(v)
	return step, err == nil, err
}

// removal returns the step that follows in the removal of the victim. A combined victim whose node is up is marked
// ineligible and drained first. A victim that leads hands the leadership over, and with two voters the removal is
// refused. Otherwise the order is: stop the server, wait until autopilot no longer counts it a healthy voter, remove
// its peer, force its member out of the gossip pool, wait until the rest is healthy with one voter fewer, delete the
// machine. Each step is chosen from what is left to do, so a step that something else has done already is skipped.
func (sg *serverGroup) removal(v Machine) (Step, error) {
	srv, hasServer := sg.servers[v.ID] // the zero Server when there is none: it neither leads nor votes
	mem := sg.members[v.ID]            // the zero Member when there is none: neither alive nor failed
	n, hasNode := sg.nodes[v.ID]       // the zero Node when there is none
	nodeUp := hasNode && n.Status != nodeDown
	drained := n.DrainedFor == v.ID
	step := Step{Group: sg.g.Name, Machine: v}
	switch {
	// A combined victim's node is marked, drained and waited for; a node that is down has nothing to drain.
	case nodeUp && n.Eligible && !n.Draining && !drained:
		step.Action, step.Node = MarkIneligible, n
	case nodeUp && !n.Draining && !drained: // ineligible: the case above takes the eligible nodes
		step.Action, step.Node, step.Deadline = Drain, n, sg.g.DrainTimeout
	case nodeUp && n.Draining:
		step.Action, step.Node = WaitDrained, n
	case v.Ready && srv.Leader:
		target, ok := sg.successor(v)
		if !ok {
			return Step{}, refuse("node group %s: %s leads, and no healthy voter of the group that is up to date can "+
				"take the leadership", sg.g.Name, v.Name)
		}
		step.Action, step.Server = TransferLeadership, target
	case v.Ready && srv.Voter && sg.voters == 2:
		return Step{}, refuse("node group %s: removing %s would leave one voter of two: tent does not take a group "+
			"from two voters to one yet", sg.g.Name, v.Name)
	case v.Ready:
		step.Action = Stop
	case srv.Voter && srv.Healthy:
		step.Action = WaitServerDown
	case hasServer:
		step.Action, step.Server = RemovePeer, srv
	case mem.Status == memberAlive || mem.Status == memberFailed:
		step.Action, step.Member = ForceLeave, mem
	case !sg.s.Nomad.Healthy || sg.voters != len(sg.ms)-1:
		step = Step{Action: WaitHealthy, Group: sg.g.Name, Voters: len(sg.ms) - 1}
	default:
		step.Action = Delete
	}
	return step, nil
}

// successor returns the server that takes the leadership from the victim: the healthy voter of the group's other
// machines that is up to date and comes first by name.
func (sg *serverGroup) successor(v Machine) (Server, bool) {
	var best Server
	var found bool
	for _, m := range sg.ms {
		srv := sg.servers[m.ID]
		if m.ID != v.ID && srv.Voter && srv.Healthy && !outdated(sg.s, sg.g, m) && (!found || srv.Name < best.Name) {
			best, found = srv, true
		}
	}
	return best, found
}

// checkAtRest refuses unless autopilot reports every server healthy and every machine of the group runs and votes,
// and, in a combined group, every node can take work.
func (sg *serverGroup) checkAtRest() error {
	if !sg.s.Nomad.Healthy {
		var unhealthy []string
		for _, srv := range sg.s.Nomad.Servers {
			if !srv.Healthy {
				unhealthy = append(unhealthy, nodeOfServer(srv.Name))
			}
		}
		slices.Sort(unhealthy)
		which := ""
		if len(unhealthy) > 0 {
			which = " (" + english.And(unhealthy) + ")"
		}
		return refuse("node group %s: autopilot reports the servers unhealthy%s; tent replaces a server only while "+
			"every server is healthy", sg.g.Name, which)
	}
	for _, m := range sg.ms {
		if why := sg.notAtRest(m); why != "" {
			return refuse("node group %s: node %s %s; run tent update cluster or tent validate cluster first",
				sg.g.Name, m.Name, why)
		}
	}
	return nil
}

// notAtRest says why a machine does not run and vote, or, in a combined group, why its node cannot take work. It says
// "" when none applies.
func (sg *serverGroup) notAtRest(m Machine) string {
	srv, hasServer := sg.servers[m.ID]
	switch {
	case !m.Ready:
		return "is not running"
	case !m.PrivateIP.IsValid():
		return "is not a voting server: the cloud reports no private address for it"
	case !hasServer:
		return "is not a server in the Raft configuration"
	case !srv.Voter:
		return "is not a voting server"
	case sg.g.Role == v1alpha1.RoleCombined:
		n, hasNode := sg.nodes[m.ID]
		return nodeWhy(m, n, hasNode)
	default:
		return ""
	}
}

// waitStable returns the wait until every voter but the victim's has been stable for the stability window, so that
// every node has refreshed its list of servers since the servers last changed.
func (sg *serverGroup) waitStable(v Machine) (Step, bool) {
	var latest time.Time
	for _, srv := range sg.s.Nomad.Servers {
		if srv.Voter && srv.ID != sg.servers[v.ID].ID && srv.StableSince.After(latest) {
			latest = srv.StableSince
		}
	}
	until := latest.Add(sg.s.Refresh + stableMargin)
	if !until.After(sg.s.Now) {
		return Step{}, false
	}
	return Step{Action: WaitStable, Group: sg.g.Name, Until: until}, true
}

// settleOrphans purges the node that a deleted machine of a combined group leaves behind, once Nomad lists it down,
// and waits for it to go down. Server groups have no nodes.
func (sg *serverGroup) settleOrphans() (Step, bool, error) {
	if sg.g.Role != v1alpha1.RoleCombined {
		return Step{}, false, nil
	}
	if step, found, err := purgeOrphan(sg.s, sg.g); err != nil || found {
		return step, found, err
	}
	return waitOrphan(sg.s, sg.g)
}

// requireSize refuses a group that has fewer machines than its size.
func (sg *serverGroup) requireSize() (Step, bool, error) {
	if len(sg.ms) < sg.g.Size {
		return Step{}, false, refuse("node group %s: it has %d of its %d nodes; run tent update cluster first",
			sg.g.Name, len(sg.ms), sg.g.Size)
	}
	return Step{}, false, nil
}

// create creates a server while an outdated one is left, once the checks at rest pass. The group has exactly its size
// then: a larger group has a victim.
func (sg *serverGroup) create() (Step, bool, error) {
	if len(upToDate(sg.s, sg.g, sg.ms)) == len(sg.ms) {
		return Step{}, false, nil
	}
	if err := sg.checkAtRest(); err != nil {
		return Step{}, false, err
	}
	if err := sg.requireTolerance(); err != nil {
		return Step{}, false, err
	}
	return Step{Action: Create, Group: sg.g.Name, Machine: newMachine(sg.s, sg.g, sg.ms)}, true, nil
}

// requireTolerance refuses a cluster that can lose no voter, since a new server is added only to a cluster that
// survives it. A group of one server always has a tolerance of 0.
func (sg *serverGroup) requireTolerance() error {
	if sg.s.Nomad.FailureTolerance >= 1 {
		return nil
	}
	if sg.g.Size == 1 {
		return refuse("node group %s: a group of one server cannot roll: its failure tolerance is 0", sg.g.Name)
	}
	return refuse("node group %s: the servers can lose no voter (failure tolerance 0); tent adds a server only to a "+
		"cluster that can lose one", sg.g.Name)
}
