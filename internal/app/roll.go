package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// RollOptions say what a rolling update does.
type RollOptions struct {
	// Apply carries the roll out under the cluster's lock; without it, RollingUpdate only plans.
	Apply bool
	// NodeGroups are the groups to roll, by name; every group of the specs when empty.
	NodeGroups []string
	// Force replaces every machine of those groups that the cloud lists when the run starts, whatever its hash.
	Force bool
}

// RollingUpdate plans the replacement of the outdated machines of the cluster's node groups. A machine is outdated
// when its spec hash is not its group's, when it carries none, or, with Force, whatever its hash. It fails when the
// store's layout needs a newer tent. It loads the specs as an update does, and then fails, in this order, unless: the
// selected node groups are in the specs; the cloud's live API accepts the specs; the store holds the mark of the Nomad
// bootstrap, then the completed spec as the specs make it now, then the cluster's secrets; and a server of the cluster
// has joined. The completed spec and the infrastructure are the business of tent update cluster, so specs that it has
// not applied fail the plan.
//
// Each plan lists the machines once and reads Nomad's Raft configuration, autopilot's report, gossip members and nodes,
// in that order. The plan holds the selected groups with their outdated machines, and in Next the step that comes next.
// Next is nil when nothing is left to roll. A failed check or read returns no plan. The plan comes with an error and no
// Next when the decisions of rollout refuse the run, when the next step is one of a server or combined group, which a
// roll does not replace yet, and when the next step waits for a machine to join that a run would refuse to wait for.
// Without Apply it changes nothing in the cloud, in Nomad and in the store, and takes no lock.
//
// With Apply, a plan that has no next step or an error ends the run as it is, without a lock; a plan without a next
// step comes back applied. Otherwise it takes the cluster's lock and plans again under it. When that plan has a next
// step, it calls OnRollPlan with it, then OnWarning with each warning about the cluster, such as a Nomad API that the
// whole internet may reach, and carries the steps out until none is left. The plan that it returns is the one under
// the lock, or the one made without it when it cannot take the lock, with Rolled holding what the roll did, also when
// it stopped, and Applied set once it reached its end.
func (s *Service) RollingUpdate(ctx context.Context, cluster string, opts RollOptions) (_ RollPlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return RollPlan{}, err
	}
	cache := assetCache{} // both plans of this run find the release files once
	_, plan, err := s.planRoll(ctx, l, opts, cache)
	switch {
	case err != nil || !opts.Apply:
		return plan, err
	case plan.Next == nil:
		plan.Applied = true
		return plan, nil
	}
	err = s.locked(ctx, l, "rolling-update", func(ctx context.Context) error {
		var r *rollRun
		var err error
		if r, plan, err = s.planRoll(ctx, l, opts, cache); err != nil {
			return err
		}
		if plan.Next != nil {
			if err := s.beginRoll(r, plan); err != nil {
				return err
			}
			if plan.Rolled, err = r.run(ctx); err != nil {
				return err
			}
		}
		plan.Applied = true
		return nil
	})
	return plan, err
}

// planRoll prepares the run, as prepareRoll does, and plans it. A failed check returns no run and no plan; after that
// it returns the run with what plan returns, the plan and its error.
func (s *Service) planRoll(ctx context.Context, l statestore.Layout, opts RollOptions, cache assetCache,
) (*rollRun, RollPlan, error) {
	r, err := s.prepareRoll(ctx, l, opts, cache)
	if err != nil {
		return nil, RollPlan{}, err
	}
	plan, err := r.plan(ctx)
	return r, plan, err
}

// beginRoll tells OnRollPlan the plan of r, then OnWarning each warning about the cluster. It returns the error of
// OnRollPlan.
func (s *Service) beginRoll(r *rollRun, plan RollPlan) error {
	if s.OnRollPlan != nil {
		if err := s.OnRollPlan(plan); err != nil {
			return err
		}
	}
	s.warn(r.warnings...)
	return nil
}

// rollRun is a rolling update of a cluster: what it knows of the cluster when it starts.
type rollRun struct {
	s       *Service
	kit     nodeKit
	model   *model.Cluster
	groups  []rollout.Group  // the selected groups, by name
	version string           // the Nomad version that a new node runs
	forced  map[string]bool  // the machines to replace whatever their hash, by ID
	listed  []cloud.Instance // the last list of the machines
	api     nomadops.API     // over the servers that have joined
	// warnings are about the cluster, for the run to tell before its first step.
	warnings []string
	rollLoop
}

// prepareRoll checks what a rolling update starts from, as RollingUpdate says, lists the machines and returns the run
// over them. The release files that it finds go into cache.
func (s *Service) prepareRoll(ctx context.Context, l statestore.Layout, opts RollOptions, cache assetCache,
) (*rollRun, error) {
	c, err := s.loadCluster(ctx, l)
	if err != nil {
		return nil, err
	}
	names, err := selectGroups(c.m, opts.NodeGroups)
	if err != nil {
		return nil, err
	}
	p, err := s.provider(c.m.Provider)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(ctx, c.objs.Cluster, c.objs.NodeGroups); err != nil {
		return nil, err
	}
	if err := s.checkApplied(ctx, l, c); err != nil {
		return nil, err
	}
	secrets, err := s.storedSecrets(ctx, l)
	if missing, ok := errors.AsType[*missingSecretsError](err); ok {
		return nil, fmt.Errorf("%w; run tent update cluster first", missing)
	}
	if err != nil {
		return nil, err
	}
	builder, err := s.planBuilder(ctx, p, c.m, c.objs, c.ch, secrets, cache)
	if err != nil {
		return nil, err
	}
	nodes := p.Nodes()
	listed, err := nodes.List(ctx, c.m.Name)
	if err != nil {
		return nil, err
	}
	kit := nodeKit{
		cluster: c.m.Name, region: c.objs.Cluster.Spec.Nomad.Region, nodes: nodes, secrets: secrets, builder: builder,
	}
	api, err := s.rollAPI(c.m, listed, kit)
	if err != nil {
		return nil, err
	}
	groups, err := rolloutGroups(c.m, groupSpecs(c.objs.NodeGroups), builder, names)
	if err != nil {
		return nil, err
	}
	r := &rollRun{
		s: s, kit: kit, model: c.m, groups: groups, version: c.objs.Cluster.Spec.Nomad.Version, listed: listed, api: api,
		warnings: s.updateWarnings(c.objs.Cluster, c.objs.NodeGroups, c.ch),
	}
	if opts.Force {
		r.forced = forcedMachines(listed, groups)
	}
	return r, nil
}

// selectGroups returns the names of the groups that a roll handles, by name, each once: those in requested, or every
// group of m when requested is empty. A name that m lacks is an error.
func selectGroups(m *model.Cluster, requested []string) ([]string, error) {
	all := make([]string, len(m.Groups))
	for i, g := range m.Groups {
		all[i] = g.Name
	}
	if len(requested) == 0 {
		return all, nil
	}
	for _, name := range requested {
		if !slices.Contains(all, name) {
			return nil, fmt.Errorf("node group %s is not in the specs of %s; its node groups are %s", name,
				clusterLabel(m.Name), english.And(all))
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(requested))), nil
}

// checkApplied fails unless tent update cluster has applied the specs of the loaded cluster c: the store holds the
// mark of the Nomad bootstrap and the completed spec that the specs make now.
func (s *Service) checkApplied(ctx context.Context, l statestore.Layout, c loadedCluster) error {
	name := clusterLabel(c.m.Name)
	marked, err := s.bootstrapped(ctx, l)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf("%s has no Nomad yet; run tent update cluster first", name)
	}
	completed, err := spec.Encode(c.objs)
	if err != nil {
		return fmt.Errorf("encode the completed spec of %s: %w", name, err)
	}
	switch {
	case c.stored == nil:
		return fmt.Errorf("%s has no completed spec; run tent update cluster first", name)
	case !bytes.Equal(c.stored, completed):
		return fmt.Errorf("the specs of %s changed since the last tent update cluster; run it first", name)
	}
	return nil
}

// isServerMachine reports whether the machine in belongs to a server or combined group of m.
func isServerMachine(m *model.Cluster, in cloud.Instance) bool {
	g, _ := findGroup(m, in.Group)
	return g.Role.RunsServer()
}

// rollAPI returns the API over the machines of the server and combined groups of m among listed that have joined.
func (s *Service) rollAPI(m *model.Cluster, listed []cloud.Instance, kit nodeKit) (nomadops.API, error) {
	joined := slices.DeleteFunc(slices.Clone(listed), func(in cloud.Instance) bool {
		return !isServerMachine(m, in) || !in.Joined
	})
	if len(joined) == 0 {
		return nil, fmt.Errorf("%s has no server that joined; run tent update cluster first", clusterLabel(m.Name))
	}
	api, err := s.nomadOver(joined, kit.nomad())
	if err != nil { // not returned with api: a nil *Servers in an interface is not nil
		return nil, err
	}
	return api, nil
}

// plan reads Nomad and returns the plan of the run: its groups, and the step that comes next. A failed read returns no
// plan. A refusal of the decisions, of a step that the roll cannot carry out and of a wait that the run would refuse
// comes with the groups.
func (r *rollRun) plan(ctx context.Context) (RollPlan, error) {
	reading, err := readNomad(ctx, r.api)
	if err != nil {
		return RollPlan{}, err
	}
	groups := make([]RollGroup, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, RollGroup{
			Name: g.Name, Role: g.Role, Size: g.Size, Outdated: outdatedOf(r.listed, g, r.forced),
		})
	}
	plan := RollPlan{Groups: groups}
	step, err := rollout.Next(r.state(reading), rollout.Roll)
	if err == nil && step.Action != rollout.Done {
		err = r.refuse(step, reading.nodes)
	}
	if err != nil {
		plan.refused = true
		return plan, err
	}
	if step.Action == rollout.Done {
		return plan, nil
	}
	next := rollStepOf(step)
	plan.Next = &next
	return plan, nil
}

// state returns what the decisions see: the cluster as the cloud listed it, with the machines that the run created and
// no list has shown yet, and as the reading of Nomad shows it.
func (r *rollRun) state(reading nomadReading) rollout.State {
	return rollout.State{
		Cluster: r.kit.cluster, Groups: r.groups, Machines: rolloutMachines(r.machines()), Nomad: reading.state(),
		Version: r.version, Forced: r.forced, Refresh: joinRefresh, Now: r.s.now(),
	}
}

// refuse returns why the run cannot carry out the step, or nil: see refuseRole. A wait for a machine to join is refused
// as its first poll would refuse it, given the nodes that Nomad lists.
func (r *rollRun) refuse(step rollout.Step, nodes []nomadops.Node) error {
	if err := r.refuseRole(step); err != nil || step.Action != rollout.WaitJoined {
		return err
	}
	in, _ := instanceByID(r.listed, step.Machine.ID)
	_, err := joinCheck(in, nodes, r.s.now())
	return err
}

// refuseRole returns why the run cannot carry out the step of a group of its role, or nil. A roll replaces no server
// yet.
func (r *rollRun) refuseRole(step rollout.Step) error {
	if g, _ := findGroup(r.model, step.Group); g.Role != v1alpha1.RoleClient {
		msg := fmt.Sprintf("node group %s: tent cannot roll server and combined groups yet", step.Group)
		if r.model.HasClientGroup() {
			msg += "; select client groups with --nodegroups"
		}
		return errors.New(msg)
	}
	return nil
}

// joinStep is what a poll of the wait for a new node to join does.
type joinStep int

// What a poll does.
const (
	joinWait   joinStep = iota // the node has not joined: poll again
	joinCreate                 // the cloud reports the machine not ready: repeat its create with its operation id
	joinScrub                  // the node is ready and eligible: replace the machine's user data with the stub
)

// joinCheck says what a poll of the wait for the machine in to join does, at now, given the nodes that Nomad lists. In
// this order: a machine that the cloud reports not ready, with a valid operation id, has its create repeated; a
// machine without a private address fails the wait; a machine for which Nomad lists a ready and eligible node of its
// name and private address is scrubbed; a client created more than introLifetime ago is refused, as its intro token has
// run out; otherwise the wait goes on.
func joinCheck(in cloud.Instance, nodes []nomadops.Node, now time.Time) (joinStep, error) {
	if !in.Ready && cloud.ValidOpID(in.Op) {
		return joinCreate, nil
	}
	if err := privateAddress(in); err != nil {
		return joinWait, err
	}
	if slices.ContainsFunc(nodes, func(n nomadops.Node) bool { return n.Is(in.Name, in.PrivateIP) && n.Ready() }) {
		return joinScrub, nil
	}
	if in.Role == v1alpha1.RoleClient && !in.Created.IsZero() && now.Sub(in.Created) > introLifetime {
		return joinWait, fmt.Errorf("node %s has not joined within %d minutes of its creation; "+
			"run tent update cluster, which deletes it and creates it again", in.Name, int(introLifetime.Minutes()))
	}
	return joinWait, nil
}
