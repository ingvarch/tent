package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// nodeDown is the status of a node that Nomad lists as down.
const nodeDown = "down"

// updateCommand is the command of an update, as an error tells the operator to run it again.
const updateCommand = "tent update cluster"

// planInput is what the plan of the node changes needs besides the cluster's model and its listed machines.
type planInput struct {
	access  nomadAccess
	next    map[string]int                 // the lowest index of a new machine of each server and combined group
	marked  bool                           // the store holds the mark of the ACL bootstrap
	specs   map[string]*v1alpha1.NodeGroup // the specs of the node groups, by name
	version string                         // the pinned Nomad version
	builder func() (*nodeBuilder, error)   // makes the builder of the nodes' NodeConfig once
}

// nodePlan is the plan of the node changes of an update: the changes in the order they run, the server and combined
// machines that stay, the nodes without a machine that it purges, what the apply carries out through the rollout
// decisions, what it read of Nomad when that was what a roll observes, and the warning about a read of Nomad that
// failed, or "".
type nodePlan struct {
	changes []NodeChange
	servers []cloud.Instance
	purges  []NodePurge
	removal removalPlan
	reading *nomadReading
	warning string
}

// removalPlan is the part of an update's plan that the rollout decisions carry out.
type removalPlan struct {
	model *model.Cluster
	// groups are the groups that lose a node that joined or that have a node to purge, as the decisions see them, by
	// name. It has none when the plan removes no node that joined and purges none.
	groups  []rollout.Group
	version string          // the pinned Nomad version, which a shrink ignores
	planned map[string]bool // the machines that the plan removes this way, by ID
}

// removalInput is what the plan of the removals reads of the cluster besides Nomad.
type removalInput struct {
	m       *model.Cluster
	groups  []rollout.Group  // the node groups of the specs, with the settings of their specs
	listed  []cloud.Instance // every machine the cloud lists
	changes []NodeChange     // the node changes of the plan
	version string           // the pinned Nomad version
	now     time.Time
}

// removesJoined reports whether an update of the listed machines of the cluster m may remove a node that joined: a
// group of m has more machines than its size and one of them carries the joined label, or a machine that carries it is
// a client with a group label of a group that m lacks.
func removesJoined(m *model.Cluster, listed []cloud.Instance) bool {
	machines := make(map[string]int, len(m.Groups))
	joined := make(map[string]bool, len(m.Groups))
	for _, in := range listed {
		switch {
		case in.Cluster != m.Name:
		case !inSpec(m, in):
			if in.Joined && in.Role == v1alpha1.RoleClient && in.Group != "" {
				return true
			}
		default:
			machines[in.Group]++
			joined[in.Group] = joined[in.Group] || in.Joined
		}
	}
	return slices.ContainsFunc(m.Groups, func(g model.NodeGroup) bool {
		return joined[g.Name] && machines[g.Name] > g.Size
	})
}

// readForRemoval reads what a roll observes of Nomad, through the API that ask makes over the machines that a roll
// calls (apiMachines): the Raft configuration, autopilot's report, the gossip members and the client nodes. It fails
// when no such machine exists, with an error that matches errNoServerToAsk and names the removals among the node
// changes, if any; when ask fails; and when a read fails, with the advice to start a stopped server when no server
// answers.
func readForRemoval(ctx context.Context, m *model.Cluster, listed []cloud.Instance, changes []NodeChange,
	ask func(servers []cloud.Instance) (nomadops.API, error),
) (nomadReading, error) {
	servers := apiMachines(m, listed, nil, nil)
	if len(servers) == 0 {
		return nomadReading{}, noServerToAsk(m.Name, changes)
	}
	api, err := ask(servers)
	if err != nil {
		return nomadReading{}, err
	}
	reading, err := readNomad(ctx, api)
	if err != nil {
		return nomadReading{}, stoppedAdvice(m, listed, err)
	}
	return reading, nil
}

// errNoServerToAsk is what readForRemoval's error matches when the cluster has no server to ask, with and without a
// removal among the changes: a caller that goes on by the labels checks first that the changes hold no removal.
var errNoServerToAsk = errors.New("no server that joined and runs for tent to ask")

// noServerToAsk says that the cluster has no server to ask. When the changes hold the removal of a node that joined,
// it names those nodes and the advice for them; otherwise the plan may go on by the labels, and the error only says
// that no server answers.
func noServerToAsk(cluster string, changes []NodeChange) error {
	var names []string
	for _, c := range changes {
		if isRemoval(c) {
			names = append(names, fmt.Sprintf("%s (ID %s, %s)", c.Name, c.ID, c.Reason))
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("%s has %w", clusterLabel(cluster), errNoServerToAsk)
	}
	nodes, these := "a node that joined", "this node"
	if len(names) > 1 {
		nodes, these = "nodes that joined", "these nodes"
	}
	return fmt.Errorf("update would remove %s Nomad: %s; %s has %w: keep %s in the specs until the servers run, or "+
		"delete the whole cluster with tent delete cluster",
		nodes, english.And(names), clusterLabel(cluster), errNoServerToAsk, these)
}

// isRemoval reports whether the change is the delete of a node that joined: only such a delete comes with a drain or
// the leaving of a server.
func isRemoval(c NodeChange) bool { return c.Drain != "" || c.Server }

// decide asks the rollout decisions about the plan's removals, on the state that the loop starts from and then about
// each group that loses a node alone, and lists the nodes without a machine that the update purges. It returns the
// removal plan, the purges by name and then ID, and the refusal of the decisions, if any, which fails the plan. The
// plan has no groups when it removes no node that joined and purges none.
//
// A purge is a node that reads down and that no listed machine has, of a client or combined group of the specs, or of
// a client group that the specs lack, by the group in the node's name. A group that only purges is asked about alone,
// and gets no purge when the decisions refuse it.
func (in removalInput) decide(reading nomadReading) (removalPlan, []NodePurge, error) {
	groups := map[string]rollout.Group{}
	planned := map[string]bool{}
	var names []string
	for _, c := range in.changes {
		if isRemoval(c) {
			machine, _ := instanceByID(in.listed, c.ID)
			groups[machine.Group] = in.groupNamed(machine.Group)
			planned[c.ID] = true
			names = append(names, c.Name)
		}
	}
	if len(names) > 0 {
		asked := [][]rollout.Group{sortedByName(groups)}
		for _, g := range sortedByName(groups) {
			asked = append(asked, []rollout.Group{g})
		}
		for _, gs := range asked {
			if _, err := rollout.Next(in.stateOf(gs, reading), rollout.Shrink); err != nil {
				return removalPlan{}, nil, fmt.Errorf("update cannot remove %s: %w", english.And(names), err)
			}
		}
	}
	purges, purging := in.purges(reading, groups)
	maps.Copy(groups, purging)
	if len(groups) == 0 {
		return removalPlan{}, nil, nil
	}
	return removalPlan{model: in.m, groups: sortedByName(groups), version: in.version, planned: planned}, purges, nil
}

// purges returns the purges of the nodes that no machine has, by name and then ID, and the groups they belong to by
// name. A group in removing is asked about already; any other is asked about alone.
func (in removalInput) purges(reading nomadReading, removing map[string]rollout.Group,
) ([]NodePurge, map[string]rollout.Group) {
	var purges []NodePurge
	groups := map[string]rollout.Group{}
	for _, g := range in.purgeCandidates(reading) {
		s := in.stateOf([]rollout.Group{g}, reading)
		var found []NodePurge
		for _, n := range rollout.Orphans(s, g) {
			if n.Status == nodeDown {
				found = append(found, purgeOf(n, g.Name))
			}
		}
		if len(found) == 0 {
			continue
		}
		if _, asked := removing[g.Name]; !asked {
			if _, err := rollout.Next(s, rollout.Shrink); err != nil {
				continue
			}
		}
		purges = append(purges, found...)
		groups[g.Name] = g
	}
	slices.SortFunc(purges, func(a, b NodePurge) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return purges, groups
}

// purgeCandidates returns the groups whose nodes the update may purge, by name: the client and combined groups of the
// specs, and a group for each name that a node gives and the specs lack.
func (in removalInput) purgeCandidates(reading nomadReading) []rollout.Group {
	groups := map[string]rollout.Group{}
	for _, g := range in.groups {
		if g.Role.RunsClient() {
			groups[g.Name] = g
		}
	}
	for _, n := range reading.nodes {
		name, ok := rollout.GroupOfNode(in.m.Name, n.Name)
		if _, inSpecs := in.specGroup(name); ok && !inSpecs {
			groups[name] = groupWithoutSpec(name)
		}
	}
	return sortedByName(groups)
}

// purgeOf returns the purge of the node n, which belongs to the group.
func purgeOf(n rollout.Node, group string) NodePurge {
	p := NodePurge{Name: n.Name, ID: n.ID, Group: group}
	if n.Address.IsValid() {
		p.Address = n.Address.String()
	}
	return p
}

// specGroup returns the group of the specs called name, and whether the specs have it.
func (in removalInput) specGroup(name string) (rollout.Group, bool) {
	i := slices.IndexFunc(in.groups, func(g rollout.Group) bool { return g.Name == name })
	if i < 0 {
		return rollout.Group{}, false
	}
	return in.groups[i], true
}

// groupNamed returns the group of the specs called name as the decisions see it, or the group that stands for a group
// that the specs lack.
func (in removalInput) groupNamed(name string) rollout.Group {
	if g, ok := in.specGroup(name); ok {
		return g
	}
	return groupWithoutSpec(name)
}

// groupWithoutSpec returns the group that the decisions see for a client group that the specs lack: its spec is gone,
// and with it its own settings, so it has size 0, no limit on unavailable nodes and the default drain timeout.
func groupWithoutSpec(name string) rollout.Group {
	return rollout.Group{Name: name, Role: v1alpha1.RoleClient, DrainTimeout: defaultDrain}
}

// sortedByName returns the groups by name.
func sortedByName(groups map[string]rollout.Group) []rollout.Group {
	return slices.SortedFunc(maps.Values(groups), func(a, b rollout.Group) int { return strings.Compare(a.Name, b.Name) })
}

// stateOf returns what the decisions see of the cluster: the groups, every listed machine of the cluster but those
// that the plan deletes without a drain or the leaving of a server, and the reading of Nomad. The machines that carry
// the replace label of the groups count as outdated in the order.
func (in removalInput) stateOf(groups []rollout.Group, reading nomadReading) rollout.State {
	deleted := map[string]bool{}
	for _, c := range in.changes {
		if c.Action == NodeDelete && !isRemoval(c) {
			deleted[c.ID] = true
		}
	}
	kept := slices.DeleteFunc(slices.Clone(in.listed), func(m cloud.Instance) bool {
		return m.Cluster != in.m.Name || deleted[m.ID]
	})
	return rollout.State{
		Cluster: in.m.Name, Groups: groups, Machines: rolloutMachines(kept), Nomad: reading.state(),
		Version: in.version, Forced: forcedMachines(in.listed, groups, false), Refresh: joinRefresh, Now: in.now,
	}
}

// planNodeChanges plans the node changes of the cluster m for the machines listed, and reads Nomad as far as the plan
// needs (see readForPlan). The nodes tell which stale clients never registered, and which nodes no machine has: the
// plan purges those that read down. A plan that may remove a node that joined also asks the rollout decisions about
// the removals, and fails when they refuse.
func (s *Service) planNodeChanges(ctx context.Context, m *model.Cluster, listed []cloud.Instance, in planInput,
) (nodePlan, error) {
	changes, servers := planNodes(m, listed, nil, in.next, shrinkView{})
	stale := staleClients(m.Name, listed, s.now())
	seen, err := s.readForPlan(ctx, m, listed, changes, servers, stale, in)
	if err != nil {
		return nodePlan{}, err
	}
	var groups []rollout.Group
	var view shrinkView
	if seen.read {
		if groups, view, err = planningView(m, listed, seen, in); err != nil {
			return nodePlan{}, err
		}
	}
	changes, servers, _ = clientsSeen{seen.nodes, stale}.plan(m, listed, in.next, view, changes, servers)
	plan := nodePlan{changes: changes, servers: servers, warning: seen.warning}
	if seen.full {
		plan.reading = &seen.nomadReading
	}
	if groups == nil {
		return plan, nil
	}
	input := removalInput{m: m, groups: groups, listed: listed, changes: changes, version: in.version, now: s.now()}
	plan.removal, plan.purges, err = input.decide(seen.nomadReading)
	return plan, err
}

// planReading is what a plan read of Nomad. When read is false it holds nothing. The nodes alone are read unless full
// is set, which says the plan read what a roll observes.
type planReading struct {
	nomadReading
	read, full bool
	warning    string // why the plan lacks the nodes that a failed read would have given, or ""
}

// readForPlan reads Nomad for a plan whose node changes without any reading are changes. A plan that may remove a node
// that joined (removesJoined) reads what a roll observes, and fails when it cannot; when no server runs to ask, it
// goes on by the labels if no removal is among the changes. Any other plan reads the nodes alone, as readNodes says.
func (s *Service) readForPlan(ctx context.Context, m *model.Cluster, listed []cloud.Instance, changes []NodeChange,
	servers, stale []cloud.Instance, in planInput,
) (planReading, error) {
	if removesJoined(m, listed) {
		reading, err := readForRemoval(ctx, m, listed, changes, s.nomadAsker(in.access))
		switch {
		case err == nil:
			return planReading{nomadReading: reading, read: true, full: true}, nil
		case !errors.Is(err, errNoServerToAsk) || slices.ContainsFunc(changes, isRemoval):
			return planReading{}, err
		}
	}
	nodes, read, warning, err := s.readNodes(ctx, m, in.access, listed, stale, servers, in.marked)
	return planReading{nomadReading: nomadReading{nodes: nodes}, read: read, warning: warning}, err
}

// nomadAsker returns the function that makes the Nomad API over the servers with the access.
func (s *Service) nomadAsker(access nomadAccess) func(servers []cloud.Instance) (nomadops.API, error) {
	return func(servers []cloud.Instance) (nomadops.API, error) {
		api, err := s.nomadOver(servers, access)
		if err != nil { // not returned with api: a nil *Servers in an interface is not nil
			return nil, err
		}
		return api, nil
	}
}

// planningView returns the node groups of m as the decisions see them, and the view of the node planner that the
// reading gives. The groups need the release files: when they cannot be read, a plan that reads what a roll observes
// fails, and any other has no groups, so no purge. The view is the zero view unless the plan read what a roll
// observes.
func planningView(m *model.Cluster, listed []cloud.Instance, seen planReading, in planInput,
) ([]rollout.Group, shrinkView, error) {
	builder, err := in.builder()
	switch {
	case err != nil && seen.full:
		return nil, shrinkView{}, err
	case err != nil:
		return nil, shrinkView{}, nil
	}
	groups, err := specGroups(m, in.specs, builder)
	if err != nil || !seen.full {
		return groups, shrinkView{}, err
	}
	return groups, shrinkView{
		read: true, nomad: seen.state(), hash: builder.specHash, forced: forcedMachines(listed, groups, false),
		drain: drainOf(groups),
	}, nil
}

// specGroups returns the node groups of m as the decisions see them, with the settings of their specs and the spec
// hashes of the builder b.
func specGroups(m *model.Cluster, specs map[string]*v1alpha1.NodeGroup, b *nodeBuilder) ([]rollout.Group, error) {
	names, err := selectGroups(m, nil)
	if err != nil {
		return nil, err
	}
	return rolloutGroups(m, specs, b, names)
}

// drainOf returns how long the drain of a node of a group of the specs lasts: the group's drain timeout. The planner
// asks only for groups of the specs.
func drainOf(groups []rollout.Group) func(group string) time.Duration {
	timeouts := make(map[string]time.Duration, len(groups))
	for _, g := range groups {
		timeouts[g.Name] = g.DrainTimeout
	}
	return func(name string) time.Duration { return timeouts[name] }
}

// removals carries out the plan's removals of nodes that joined and its purges: the loop of a rolling update in the
// mode Shrink, over a fresh list of the machines and the machines that the apply deleted before it. It does nothing
// for a plan that removes and purges nothing, and returns what it did also when it fails.
func (a *applier) removals(ctx context.Context) (RemovalCounts, error) {
	plan := a.u.removal
	if len(plan.groups) == 0 {
		return RemovalCounts{}, nil
	}
	listed, err := a.u.nodes.List(ctx, a.u.cluster)
	if err != nil {
		return RemovalCounts{}, err
	}
	machines := apiMachines(plan.model, listed, nil, nil)
	api, err := a.s.rollAPI(a.u.cluster, machines, a.u.nodeKit)
	if err != nil {
		return RemovalCounts{}, err
	}
	r := &rollRun{
		s: a.s, kit: a.u.nodeKit, model: plan.model, groups: plan.groups, version: plan.version,
		forced: forcedMachines(listed, plan.groups, false), listed: listed, api: api, apiAt: addressesOf(machines),
		mode: rollout.Shrink, command: updateCommand, planned: plan.planned, gone: a.gone,
	}
	counts, err := r.run(ctx)
	return RemovalCounts{Drained: counts.Drained, Stopped: counts.Stopped, Purged: counts.Purged}, err
}
