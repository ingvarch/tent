package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// nodeTimeout is how long the create of a node, or the wait for one, may take.
const nodeTimeout = 10 * time.Minute

// Progress is one thing that happened while an update or a delete applied its plan. When Infra is set, it is an event
// of an infrastructure change. When Nomad is set, it is a step of the Nomad step of an update, and Step and Err say how
// far it got and why it failed. When Going is set, a delete starts to wait until the cloud stops listing that many
// nodes that it deleted. Otherwise it is a step of the node change Node; for NodeScrub, which no plan holds, Node
// has only the action, the name and the machine's ID, and Instance is zero. Err says why a failed step failed. Instance
// is the machine of a create or a wait that is done, as the provider reports it, with its ID and its private address
// when the cloud gave one; it is the zero Instance for the other steps.
type Progress struct {
	Infra    *engine.Event
	Going    int
	Node     NodeChange
	Step     NodeStep
	Err      error
	Instance cloud.Instance
	Nomad    *NomadEvent
}

// NomadAction is what the Nomad step of an update does.
type NomadAction int

// Nomad actions.
const (
	// NomadLeader waits for the servers to elect a leader.
	NomadLeader NomadAction = iota + 1
	// NomadBootstrap bootstraps the ACL system.
	NomadBootstrap
	// NomadHealthy waits until the servers are healthy and vote.
	NomadHealthy
	// NomadRegister waits until a node has registered with the servers.
	NomadRegister
)

var nomadActionNames = [...]string{
	NomadLeader: "leader", NomadBootstrap: "bootstrap", NomadHealthy: "healthy", NomadRegister: "register",
}

// String returns the action's name in lower case, such as leader.
func (a NomadAction) String() string {
	if a < NomadLeader || int(a) >= len(nomadActionNames) {
		return fmt.Sprintf("NomadAction(%d)", int(a))
	}
	return nomadActionNames[a]
}

// MarshalText returns the action's name, as String does.
func (a NomadAction) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// NomadEvent is what a step of the Nomad step of an update works on. Node names the node that a register waits for.
// Leader is the leader's RPC address once a leader wait is done. Voters is how many healthy servers a healthy wait
// waits for, and once it is done how many of them vote.
type NomadEvent struct {
	Action NomadAction
	Node   string
	Leader string
	Voters int
}

// NodeStep is how far a node change got.
type NodeStep int

// Node steps. A node change sends NodeStarted, then NodeDone or NodeFailed.
const (
	NodeStarted NodeStep = iota + 1
	NodeDone
	NodeFailed
)

var nodeStepNames = [...]string{NodeStarted: "started", NodeDone: "done", NodeFailed: "failed"}

// String returns the step's name in lower case, such as started.
func (s NodeStep) String() string {
	if s < NodeStarted || int(s) >= len(nodeStepNames) {
		return fmt.Sprintf("NodeStep(%d)", int(s))
	}
	return nodeStepNames[s]
}

// Update brings a cluster to its specs: its infrastructure, such as its network, firewalls and SSH keys, its nodes, and
// Nomad on them. It loads the specs from the store, fills in the defaults, validates them, checks them against the
// cluster's channel, has the provider check them against its live API, and plans the changes. The completed spec, the
// specs with every default as last applied, counts as a change when the stored one is missing or differs. Its Nomad
// version is the one that the spec sets, else the one of the stored completed spec, else the one that the channel
// recommends; a stored one that the channel does not allow, or a stored completed spec that does not decode when its
// version is needed, fails the plan. Each of the cluster's secrets that the store lacks counts as a change too: the
// CA's key and bundle, the gossip key and the ACL bootstrap secret. A stored CA key without its bundle gets a bundle
// signed with it; a bundle without its key, or a stored secret that does not load, fails the plan, since tent never
// replaces a cluster's secrets.
//
// A node that is created or waited for with an operation id boots with the NodeConfig of its group. A plan that does
// so reads the release files that the nodes download, and fails when a node's user data does not fit what a provider
// takes. A machine of any role that has not joined its cluster, which carries no joined label, is waited for until
// its node joins and its user data is scrubbed; the wait repeats the create only for a machine that the cloud reports
// not ready. The Nomad step is part of the plan until the store holds the mark of the ACL bootstrap, and also, with
// the mark, when no server or combined machine of the cluster is left: the servers that come are a new Nomad, so it
// bootstraps again. After the bootstrap it is part of the plan when the plan creates or waits for a server or
// combined node. Without apply, or when nothing changes, Update returns the plan and writes nothing.
//
// With apply it takes the cluster's lock and plans again under it. When that plan has changes, it calls OnUpdatePlan
// with it, then OnWarning with each warning about the cluster, such as a Nomad API that the whole internet may reach
// or a Nomad version that the channel has not tested. Then it raises the tent version, writes the missing secrets and
// the completed spec, and applies the plan in this order: the infrastructure's changes other than its deletes; the
// deletion of a stale mark of the bootstrap; the waits that repeat a create, then the creates, of the server and
// combined nodes; the Nomad step, which waits for a leader, bootstraps the ACL system with the stored secret, waits
// for healthy servers that all vote, reads the Raft configuration, and then, for each server and combined node of the
// plan in order, waits for a combined node to register, checks that its server votes at its private address, and
// replaces its user data with a stub and labels its machine as joined, and last writes the mark; then the waits for
// client nodes and the creates of the missing ones, each client booting with an intro token, registering, and then
// being scrubbed and labelled as a server is, before the next is made; a wait for a client without an operation id
// asks for no token and calls no cloud until the scrub; the node deletes; and the infrastructure's deletes, so that a
// firewall group goes only once its nodes are gone. Nodes are created one at a time. A create, a wait and each wait of
// the Nomad step may take 10 minutes, and a scrub 5. The first step that fails stops the update, and running it again
// finishes the job. It returns the plan it applied, made under the lock, with the error; the plan says Applied once
// every step has succeeded, or at once when it has no changes.
func (s *Service) Update(ctx context.Context, cluster string, apply bool) (_ UpdatePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return UpdatePlan{}, err
	}
	cache := assetCache{} // both plans of this run find the release files once
	u, err := s.planUpdate(ctx, l, cache)
	switch {
	case err != nil || !apply:
		return u.plan, err
	case !u.plan.HasChanges():
		u.plan.Applied = true
		return u.plan, nil
	}
	err = s.locked(ctx, l, "update", func(ctx context.Context) error {
		var err error
		if u, err = s.planUpdate(ctx, l, cache); err != nil {
			return err
		}
		if u.plan.HasChanges() {
			if err := s.beginUpdate(u); err != nil {
				return err
			}
			if err := s.applyUpdate(ctx, l, u); err != nil {
				return err
			}
		}
		u.plan.Applied = true
		return nil
	})
	return u.plan, err
}

// beginUpdate tells OnUpdatePlan the plan of u, then OnWarning each warning about the cluster. It returns the error
// of OnUpdatePlan.
func (s *Service) beginUpdate(u updateRun) error {
	if s.OnUpdatePlan != nil {
		if err := s.OnUpdatePlan(u.plan); err != nil {
			return err
		}
	}
	s.warn(u.warnings...)
	return nil
}

// updateRun is the plan of an update, with what applying it needs.
type updateRun struct {
	plan      UpdatePlan
	cluster   string
	layout    statestore.Layout
	region    string // the Nomad region
	nodes     cloud.Nodes
	secrets   clusterSecrets
	completed []byte           // the completed spec
	warnings  []string         // about the cluster
	servers   []cloud.Instance // the machines of the server and combined groups that stay, by name
	listed    []cloud.Instance // every machine the cloud listed, for the machine that a wait names by ID
	// staleMark is set when the store holds the mark of a bootstrap that the servers of the plan do not stand behind.
	staleMark bool
	// builder makes the NodeConfig of the nodes that the plan creates, or waits for with an operation id; it is nil when
	// the plan has none.
	builder *nodeBuilder
}

// planUpdate loads and checks a cluster's specs, as Update says, and plans the changes that bring the cloud to them.
// The release files that it finds go into cache.
func (s *Service) planUpdate(ctx context.Context, l statestore.Layout, cache assetCache) (updateRun, error) {
	objs, err := s.get(ctx, l, true)
	if err != nil {
		return updateRun{}, err
	}
	ch, err := checkCluster(objs.Cluster, objs.NodeGroups, s.Validate, s.channel)
	if err != nil {
		return updateRun{}, err
	}
	stored, err := s.readCompleted(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	if err := pinVersion(objs.Cluster, ch, l, stored); err != nil {
		return updateRun{}, err
	}
	m, err := model.New(objs.Cluster, objs.NodeGroups)
	if err != nil {
		return updateRun{}, err
	}
	p, err := s.provider(m.Provider)
	if err != nil {
		return updateRun{}, err
	}
	if err := p.Validate(ctx, objs.Cluster, objs.NodeGroups); err != nil {
		return updateRun{}, err
	}
	completed, err := spec.Encode(objs)
	if err != nil {
		return updateRun{}, fmt.Errorf("encode the completed spec of %s: %w", clusterLabel(m.Name), err)
	}
	secrets, err := s.planSecrets(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	plan, found, err := planChanges(ctx, p, m)
	if err != nil {
		return updateRun{}, err
	}
	marked, err := s.bootstrapped(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	plan.Secrets, plan.Completed = relativePaths(l, secrets.writes), !bytes.Equal(stored, completed)
	plan.Nomad = planNomad(m, plan.Nodes, found.servers, marked)
	u := updateRun{
		plan: plan, cluster: m.Name, layout: l, region: objs.Cluster.Spec.Nomad.Region, nodes: p.Nodes(),
		secrets: secrets, completed: completed, warnings: s.updateWarnings(objs.Cluster, ch),
		servers: found.servers, listed: found.listed, staleMark: marked && plan.Nomad != nil && plan.Nomad.Bootstrap,
	}
	if !changesNodes(plan.Nodes) {
		return u, nil
	}
	if u.builder, err = s.planBuilder(ctx, p, m, objs, ch, secrets, cache); err != nil {
		return updateRun{}, err
	}
	if err := u.prepareNodes(m, s.now()); err != nil {
		return updateRun{}, err
	}
	return u, nil
}

// updateWarnings returns the warnings about the cluster c that an update tells before its first change: those of every
// change, and that of development variables that a release build ignores.
func (s *Service) updateWarnings(c *v1alpha1.Cluster, ch *channels.Channel) []string {
	w := warnings(c, ch)
	if dev := devVariablesWarning(s.Version, s.Assets); dev != "" {
		w = append(w, dev)
	}
	return w
}

// bootstrapped reports whether the store holds the mark of the cluster's ACL bootstrap.
func (s *Service) bootstrapped(ctx context.Context, l statestore.Layout) (bool, error) {
	_, _, err := s.Store.Get(ctx, l.NomadBootstrapped())
	switch {
	case errors.Is(err, statestore.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// readCompleted returns the cluster's stored completed spec, or nil when the store lacks it.
func (s *Service) readCompleted(ctx context.Context, l statestore.Layout) ([]byte, error) {
	data, _, err := s.Store.Get(ctx, l.Completed())
	if errors.Is(err, statestore.ErrNotFound) {
		return nil, nil
	}
	return data, err
}

// pinVersion sets the Nomad version of the cluster c, which has its defaults and the channel ch, when its spec leaves
// it out: to the one pinned in stored, the cluster's stored completed spec, or else to the one that ch recommends. A
// pinned version that ch does not allow is an error, and so is a stored completed spec that does not decode or holds
// no Cluster: tent does not move a cluster to another Nomad version by itself.
func pinVersion(c *v1alpha1.Cluster, ch *channels.Channel, l statestore.Layout, stored []byte) error {
	n := &c.Spec.Nomad
	if n.Version != "" {
		return nil
	}
	var pinned string
	if stored != nil {
		var err error
		if pinned, err = pinnedVersion(stored); err != nil {
			return fmt.Errorf("%s: %w; set spec.nomad.version to the Nomad version the cluster was built with, or fix "+
				"%s in the state store by hand", l.Completed(), err, l.Completed())
		}
	}
	if pinned == "" {
		n.Version = ch.Nomad.Recommended
		return nil
	}
	// Allows returns nil or a *VersionError.
	if v, ok := errors.AsType[*channels.VersionError](ch.Allows(pinned)); ok {
		return fmt.Errorf("%s is pinned to Nomad %s (%s), which %s; set spec.nomad.version to a version that the "+
			"channel allows", clusterLabel(c.Metadata.Name), pinned, l.Completed(), v.Problem)
	}
	n.Version = pinned
	return nil
}

// pinnedVersion returns the Nomad version that a stored completed spec holds.
func pinnedVersion(completed []byte) (string, error) {
	objs, err := spec.Decode(completed)
	switch {
	case err != nil:
		return "", err
	case objs.Cluster == nil:
		return "", errors.New("holds no " + v1alpha1.KindCluster)
	}
	return objs.Cluster.Spec.Nomad.Version, nil
}

// provider returns the cloud provider called name.
func (s *Service) provider(name v1alpha1.Provider) (cloud.Provider, error) {
	if s.Providers == nil {
		return nil, errors.New("no cloud providers are set up")
	}
	return s.Providers(name)
}

// machines are the machines of a cluster that an update plans over.
type machines struct {
	servers []cloud.Instance // those of the server and combined groups that stay, by name, as planNodes says
	listed  []cloud.Instance // all that the cloud listed, for the machine that a wait names by ID
}

// planChanges plans the changes that bring the cloud to the model m: those of the infrastructure, from a fresh
// inventory, and those of the nodes, from a fresh list of the machines. It also returns the machines it planned over.
func planChanges(ctx context.Context, p cloud.Provider, m *model.Cluster) (UpdatePlan, machines, error) {
	snap, err := p.Inventory(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	tasks, err := p.BuildInfra(ctx, m)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	infra, err := engine.NewPlan(ctx, tasks, p.InfraKinds(), snap)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	instances, err := p.Nodes().List(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	changes, servers := planNodes(m, instances)
	return UpdatePlan{Infra: infra, Nodes: changes}, machines{servers: servers, listed: instances}, nil
}

// applyUpdate applies the plan of u in the order that Update gives.
func (s *Service) applyUpdate(ctx context.Context, l statestore.Layout, u updateRun) error {
	if u.needsNomad() && s.Nomad == nil {
		return errNoNomad
	}
	if err := statestore.RaiseVersion(ctx, s.Store, l, s.Version); err != nil {
		return err
	}
	if err := s.writeSecrets(ctx, u.secrets.writes); err != nil {
		return err
	}
	if u.plan.Completed {
		if _, err := s.Store.Put(ctx, l.Completed(), u.completed, statestore.PutOptions{}); err != nil {
			return fmt.Errorf("write the completed spec: %w", err)
		}
	}
	opts := s.applyOptions()
	if err := u.plan.Infra.ApplyTaskChanges(ctx, opts); err != nil {
		return fmt.Errorf("apply the infrastructure: %w", err)
	}
	if u.staleMark {
		path := l.NomadBootstrapped()
		if err := s.Store.Delete(ctx, path); err != nil {
			return fmt.Errorf("delete %s: %w", path, err)
		}
	}
	a := &applier{s: s, u: u, known: slices.Clone(u.servers)}
	if err := a.run(ctx); err != nil {
		return err
	}
	if err := u.plan.Infra.ApplyDeletes(ctx, opts); err != nil {
		return fmt.Errorf("delete infrastructure objects: %w", err)
	}
	return nil
}

// applyOptions returns the options that apply an infrastructure plan and report the engine's events as progress.
func (s *Service) applyOptions() engine.ApplyOptions {
	return engine.ApplyOptions{OnEvent: func(e engine.Event) { s.progress(Progress{Infra: &e}) }}
}

// applyNode carries out the delete c of a node of the cluster and reports its steps, as applyNodeWith does. It has no
// user data to boot a machine with, so a create or a wait fails.
func (s *Service) applyNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange) error {
	_, err := s.applyNodeWith(ctx, nodes, cluster, c, nil)
	return err
}

// applyNodeWith carries out the node change c of the cluster and reports its steps: a failed one with its error, which
// applyNodeWith returns naming the node for a wait. A create or a wait boots the machine with the user data that
// prepare returns; its error fails the change, and so does a nil prepare. It returns the machine of a create or a wait.
func (s *Service) applyNodeWith(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	s.progress(Progress{Node: c, Step: NodeStarted})
	in, err := changeNode(ctx, nodes, cluster, c, prepare)
	if err != nil {
		s.progress(Progress{Node: c, Step: NodeFailed, Err: err})
		if c.Action == NodeWait {
			return cloud.Instance{}, fmt.Errorf("wait for node %s: %w", c.Name, err)
		}
		return cloud.Instance{}, err
	}
	s.progress(Progress{Node: c, Step: NodeDone, Instance: in})
	return in, nil
}

// changeNode carries out the node change c of the cluster: a create with a new operation id; a wait, which repeats
// the create of the machine with its operation id; or a delete. It returns the machine of a create or a wait.
func changeNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	switch c.Action {
	case NodeCreate:
		return createNode(ctx, nodes, cluster, c, cloud.NewOpID(), prepare)
	case NodeWait:
		return createNode(ctx, nodes, cluster, c, c.Op, prepare)
	case NodeDelete:
		return cloud.Instance{}, nodes.Delete(ctx, cloud.Instance{ID: c.ID, Name: c.Name, Cluster: cluster})
	}
	return cloud.Instance{}, fmt.Errorf("node %s: unknown change %s", c.Name, c.Action)
}

// createNode creates the machine of the node change c with the operation id op and the user data that prepare
// returns, or finds the one that an earlier call with op created, waits until it is ready and returns it. It gives the
// call nodeTimeout, which the preparation does not use. Without prepare it fails before it calls the cloud: a machine
// without user data would never join the cluster.
func createNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange, op string,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	if prepare == nil {
		return cloud.Instance{}, fmt.Errorf("node %s: no user data", c.Name)
	}
	userData, err := prepare(ctx)
	if err != nil {
		return cloud.Instance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	return nodes.Create(ctx, cloud.CreateRequest{
		Cluster: cluster, Group: c.Group, Role: c.Role, Zone: c.Zone, Name: c.Name, MachineType: c.MachineType,
		Image: c.Image, SpecHash: c.SpecHash, Op: op, UserData: userData,
	})
}

// progress tells OnProgress about p, when it is set.
func (s *Service) progress(p Progress) {
	if s.OnProgress != nil {
		s.OnProgress(p)
	}
}
