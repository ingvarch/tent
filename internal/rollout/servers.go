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

// minOtherVoters is how many servers besides the victim's must vote for its machine to stop while its server is in the
// Raft configuration: Nomad's leader adds a removed server again, autopilot may promote it after its machine has
// stopped, and the voters that run must still be a quorum then.
const minOtherVoters = 2

// serverGroup is a server or combined group with its machines and their servers, members and nodes at one moment.
type serverGroup struct {
	groupView
	servers map[string]Server // the server of each machine that has one, by machine ID
	members map[string]Member // the gossip member of each machine that has one, by machine ID
	nodes   map[string]Node   // combined groups: the client node of each machine that has one, by machine ID
	voters  int               // how many servers of the Raft configuration vote
}

// nextServer returns the first step of the group that applies, and false when the group is done.
func nextServer(s State, mode Mode, g Group) (Step, bool, error) {
	sg := serverGroup{
		groupView: newGroupView(s, mode, g), servers: map[string]Server{}, members: map[string]Member{},
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
		func() (Step, bool, error) { return sg.settleUnjoined(sg.started) }, sg.removeVictim,
		sg.settleOrphans, sg.requireSize, sg.create,
	}
	for _, rule := range rules {
		if step, found, err := rule(); err != nil || found {
			return step, found, err
		}
	}
	return Step{}, false, nil
}

// started reports whether the removal of the machine has begun: it has joined, and it is stopped, or its server is
// gone or no longer votes, or, in a combined group, its node is ineligible or draining. A running machine without a
// private address counts as not started, so the checks at rest refuse it. So does a running server whose peer was
// removed, once Nomad's leader has added it again and it votes: its removal starts again with the checks at rest.
func (sg *serverGroup) started(m Machine) bool {
	if !m.Joined {
		return false
	}
	if !m.Ready {
		return true
	}
	srv, hasServer := sg.servers[m.ID]
	n, hasNode := sg.nodes[m.ID]
	return m.PrivateIP.IsValid() && (!hasServer || !srv.Voter) ||
		hasNode && (!n.Eligible || n.Draining)
}

// victim returns the machine to remove while the group has more machines than its size: one whose removal has
// started, else the first by the order of victims. A roll takes outdated machines only, a shrink any.
func (sg *serverGroup) victim() (Machine, bool) {
	if len(sg.ms) <= sg.g.Size {
		return Machine{}, false
	}
	var candidates []Machine
	for _, m := range sg.ms {
		if sg.mode == Shrink || outdated(sg.s, sg.g, m) {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Machine{}, false
	}
	sg.victimOrder(candidates, sg.started,
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
// ineligible and drained first; then removeServer takes over. Each step is chosen from what is left to do, so a step
// that something else has done already is skipped.
func (sg *serverGroup) removal(v Machine) (Step, error) {
	n, hasNode := sg.nodes[v.ID] // the zero Node when there is none
	nodeUp := hasNode && n.Status != nodeDown
	drained := drainedFor(v, n)
	step := Step{Group: sg.g.Name, Machine: v}
	switch {
	// A combined victim's node is marked, drained and waited for; a node that is down has nothing to drain.
	case nodeUp && n.Eligible && !n.Draining:
		step.Action, step.Node = evictionOf(v, n), n
		if step.Action == Drain {
			step.Deadline = sg.g.DrainTimeout
		}
	case nodeUp && !n.Draining && !drained: // ineligible: the case above takes the eligible nodes
		step.Action, step.Node, step.Deadline = Drain, n, sg.g.DrainTimeout
	case nodeUp && n.Draining:
		step.Action, step.Node = WaitDrained, n
	default:
		return sg.removeServer(v)
	}
	return step, nil
}

// removeServer returns the step that removes the victim's server and machine. The order for a victim that runs:
//
//   - a victim that leads hands the leadership over;
//   - a victim that votes waits until the other voters have been stable for the stability window;
//   - with fewer than two other voters its peer is removed from the Raft configuration while it runs, so that no
//     server that stops can be counted a voter again; with two or more it is stopped first;
//   - the machine is stopped.
//
// Then, for a machine that is stopped: wait until autopilot no longer counts the server a healthy voter, remove its
// peer, force its member out of the gossip pool, wait until the rest is healthy with the servers that are left voting,
// delete the machine. A stopped voter beside fewer than two other voters is refused: the servers have no quorum. A
// running voter whose removal has started (a combined node that was drained: the drain may last long, and a run may
// resume days later) is checked again first, since a server may have failed meanwhile and the stop or the new leader
// would then cost the quorum.
func (sg *serverGroup) removeServer(v Machine) (Step, error) {
	srv, hasServer := sg.servers[v.ID] // the zero Server when there is none: it neither leads nor votes
	mem := sg.members[v.ID]            // the zero Member when there is none: neither alive nor failed
	step := Step{Group: sg.g.Name, Machine: v}
	if v.Ready && srv.Voter && sg.started(v) {
		if err := sg.checkServing(); err != nil {
			return Step{}, err
		}
	}
	otherVoters := sg.voters
	if srv.Voter {
		otherVoters--
	}
	wait, waiting := sg.waitStable(v)
	switch {
	case v.Ready && srv.Leader:
		target, ok := sg.successor(v)
		if !ok {
			who := "healthy voter of the group that is up to date"
			if sg.mode == Shrink {
				who = "other healthy voter of the group"
			}
			return Step{}, refuse("node group %s: %s leads, and no %s can take the leadership", sg.g.Name, v.Name, who)
		}
		step.Action, step.Server = TransferLeadership, target
	case v.Ready && srv.Voter && waiting:
		return wait, nil
	case v.Ready && hasServer && otherVoters < minOtherVoters:
		step.Action, step.Server = RemovePeer, srv
	case v.Ready:
		step.Action = Stop
	case srv.Voter && otherVoters < minOtherVoters:
		return Step{}, refuse("node group %s: node %s is stopped and its server votes beside one other voter, so the "+
			"servers have no quorum; start its instance again (ID %s), which tent has not deleted, and run the command "+
			"again", sg.g.Name, v.Name, v.ID)
	case srv.Voter && srv.Healthy:
		step.Action = WaitServerDown
	case hasServer:
		step.Action, step.Server = RemovePeer, srv
	case mem.Status == memberAlive || mem.Status == memberFailed:
		step.Action, step.Member = ForceLeave, mem
	default:
		voters := sg.withServer() // the victim has none here: it is not a voter and has no peer
		if !sg.s.Nomad.Healthy || sg.voters != voters {
			return Step{Action: WaitHealthy, Group: sg.g.Name, Machine: v, Voters: voters}, nil
		}
		step.Action = Delete
	}
	return step, nil
}

// withServer is how many machines of the group have a server in the Raft configuration: the voters that the cluster
// has once the servers that are joining vote.
func (sg *serverGroup) withServer() int {
	n := 0
	for _, m := range sg.ms {
		if _, ok := sg.servers[m.ID]; ok {
			n++
		}
	}
	return n
}

// successor returns the server that takes the leadership from the victim: the server of the group's other machines
// that autopilot counts healthy and that comes first by name; the checks before have shown that they all vote. In a
// roll it must be up to date too.
func (sg *serverGroup) successor(v Machine) (Server, bool) {
	var best Server
	var found bool
	for _, m := range sg.ms {
		srv := sg.servers[m.ID]
		if m.ID != v.ID && srv.Healthy && (sg.mode == Shrink || !outdated(sg.s, sg.g, m)) &&
			(!found || srv.Name < best.Name) {
			best, found = srv, true
		}
	}
	return best, found
}

// checkAtRest refuses unless autopilot reports every server healthy and every machine of the group runs and votes,
// and, in a combined group, every node can take work.
func (sg *serverGroup) checkAtRest() error {
	if err := sg.checkHealthy(); err != nil {
		return err
	}
	return sg.checkMachines(sg.notAtRest)
}

// checkServing refuses unless autopilot reports every server healthy, every machine of the group runs and votes and
// the cluster can lose a voter, which a server needs before it stops or hands over its leadership. The nodes of a
// combined group are not looked at: the victim's has been drained. With two voters the failure tolerance is 0, and
// none is asked for: the victim's peer is removed while it runs.
func (sg *serverGroup) checkServing() error {
	if err := sg.checkHealthy(); err != nil {
		return err
	}
	if err := sg.checkMachines(sg.notServing); err != nil {
		return err
	}
	if sg.voters > 2 {
		return sg.requireToleranceFor("removes a server only from")
	}
	return nil
}

// checkHealthy refuses while autopilot reports a server unhealthy, naming the servers that it lists so.
func (sg *serverGroup) checkHealthy() error {
	if sg.s.Nomad.Healthy {
		return nil
	}
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
	verb := "replaces"
	if sg.mode == Shrink {
		verb = "removes"
	}
	return refuse("node group %s: autopilot reports the servers unhealthy%s; tent %s a server only while every "+
		"server is healthy", sg.g.Name, which, verb)
}

// checkMachines refuses the first machine of the group, by name, for which why gives a reason.
func (sg *serverGroup) checkMachines(why func(Machine) string) error {
	for _, m := range sg.ms {
		if reason := why(m); reason != "" {
			return refuse("node group %s: node %s %s; run tent validate cluster to see what is wrong",
				sg.g.Name, m.Name, reason)
		}
	}
	return nil
}

// notAtRest says why a machine does not run and vote, or, in a combined group, why its node cannot take work. It says
// "" when none applies.
func (sg *serverGroup) notAtRest(m Machine) string {
	if why := sg.notServing(m); why != "" || sg.g.Role != v1alpha1.RoleCombined {
		return why
	}
	n, hasNode := sg.nodes[m.ID]
	return nodeWhy(m, n, hasNode)
}

// notServing says why a machine does not run and vote. It says "" when it does.
func (sg *serverGroup) notServing(m Machine) string {
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
	default:
		return ""
	}
}

// waitStable returns the wait until every voter but the victim's has been stable for the stability window, so that
// every node has refreshed its list of servers since the servers last changed. The step names the victim.
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
	return Step{Action: WaitStable, Group: sg.g.Name, Machine: v, Until: until}, true
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

// requireSize refuses a roll of a group that has fewer machines than its size.
func (sg *serverGroup) requireSize() (Step, bool, error) {
	if sg.mode == Roll && len(sg.ms) < sg.g.Size {
		return Step{}, false, refuse("node group %s: it has %d of its %d nodes; run tent update cluster first",
			sg.g.Name, len(sg.ms), sg.g.Size)
	}
	return Step{}, false, nil
}

// create creates a server in a roll while an outdated one is left, once the checks at rest pass. The group has exactly
// its size then: a larger group has a victim.
func (sg *serverGroup) create() (Step, bool, error) {
	if sg.mode != Roll || len(upToDate(sg.s, sg.g, sg.ms)) == len(sg.ms) {
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
// survives it. A group of one server passes: its one voter never has a failure tolerance, and the new server joins
// before the old one goes.
func (sg *serverGroup) requireTolerance() error {
	if sg.g.Size == 1 {
		return nil
	}
	return sg.requireToleranceFor("adds a server only to")
}

// requireToleranceFor refuses a cluster that can lose no voter. Does says what tent does only to a cluster that can
// lose one, such as "adds a server only to".
func (sg *serverGroup) requireToleranceFor(does string) error {
	if sg.s.Nomad.FailureTolerance >= 1 {
		return nil
	}
	return refuse("node group %s: the servers can lose no voter (failure tolerance 0); tent %s a cluster that can "+
		"lose one", sg.g.Name, does)
}
