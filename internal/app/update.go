package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// placeholderUserData is what new nodes boot with for now: a cloud-config without secrets that turns off the package
// updates and upgrades of the first boot.
const placeholderUserData = "#cloud-config\npackage_update: false\npackage_upgrade: false\n"

// nodeTimeout is how long the create of a node, or the wait for one, may take.
const nodeTimeout = 10 * time.Minute

// Progress is one thing that happened while an update or a delete applied its plan. When Infra is set, it is an event
// of an infrastructure change. When Going is set, a delete starts to wait until the cloud stops listing that many
// nodes that it deleted. Otherwise it is a step of the node change Node. Err says why a failed step failed, in the
// provider's words. Instance is the machine of a create or a wait that is done, as the provider reports it, with its
// ID and its private address when the cloud gave one; it is the zero Instance for the other steps.
type Progress struct {
	Infra    *engine.Event
	Going    int
	Node     NodeChange
	Step     NodeStep
	Err      error
	Instance cloud.Instance
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

// Update brings a cluster's cloud objects to its specs: its infrastructure, such as its network, firewalls and SSH
// keys, and its nodes, which boot a placeholder without secrets. It loads the specs from the store, fills in the
// defaults, validates them, has the provider check them against its live API, and plans the changes. The completed
// spec, the specs with every default as last applied, counts as a change when the stored one is missing or differs.
// Without apply, or when nothing changes, it returns the plan and writes nothing.
//
// With apply it takes the cluster's lock and plans again under it. When that plan has changes, it calls OnUpdatePlan
// with it, then OnOpenAPI when the whole internet may reach the cluster's Nomad API. Then it raises the tent version
// and applies the plan in this order: the infrastructure's changes other than its deletes; the node creates, one at a
// time, servers first; the waits for nodes that an interrupted update created; the node deletes; and the
// infrastructure's deletes, so that a firewall group goes only once its nodes are gone. A create or a wait may take 10
// minutes. The first step that fails stops the update, and running it again finishes the job. Once every step has
// succeeded, Update writes the completed spec when the plan says so. It returns the plan it applied, made under the
// lock, with the error; the plan says Applied once every step has succeeded, or at once when it has no changes.
func (s *Service) Update(ctx context.Context, cluster string, apply bool) (_ UpdatePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return UpdatePlan{}, err
	}
	u, err := s.planUpdate(ctx, l)
	switch {
	case err != nil || !apply:
		return u.plan, err
	case !u.plan.HasChanges():
		u.plan.Applied = true
		return u.plan, nil
	}
	err = s.locked(ctx, l, "update", func(ctx context.Context) error {
		var err error
		if u, err = s.planUpdate(ctx, l); err != nil {
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

// beginUpdate tells OnUpdatePlan the plan of u, then OnOpenAPI when the whole internet may reach the cluster's Nomad
// API. It returns the error of OnUpdatePlan.
func (s *Service) beginUpdate(u updateRun) error {
	if s.OnUpdatePlan != nil {
		if err := s.OnUpdatePlan(u.plan); err != nil {
			return err
		}
	}
	if u.openAPI && s.OnOpenAPI != nil {
		s.OnOpenAPI()
	}
	return nil
}

// updateRun is the plan of an update, with what applying it needs.
type updateRun struct {
	plan      UpdatePlan
	cluster   string
	nodes     cloud.Nodes
	completed []byte // the completed spec
	openAPI   bool   // the whole internet may reach the cluster's Nomad API
}

// planUpdate loads and checks a cluster's specs, as Update says, and plans the changes that bring the cloud to them.
func (s *Service) planUpdate(ctx context.Context, l statestore.Layout) (updateRun, error) {
	objs, err := s.get(ctx, l, true)
	if err != nil {
		return updateRun{}, err
	}
	if err := v1alpha1.Validate(objs.Cluster, objs.NodeGroups, s.Validate); err != nil {
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
	stale, err := s.completedDiffers(ctx, l, completed)
	if err != nil {
		return updateRun{}, err
	}
	plan, err := planChanges(ctx, p, m)
	if err != nil {
		return updateRun{}, err
	}
	plan.Completed = stale
	return updateRun{
		plan: plan, cluster: m.Name, nodes: p.Nodes(), completed: completed, openAPI: openAPI(objs.Cluster),
	}, nil
}

// completedDiffers reports whether the cluster's stored completed spec is missing or differs from want.
func (s *Service) completedDiffers(ctx context.Context, l statestore.Layout, want []byte) (bool, error) {
	data, _, err := s.Store.Get(ctx, l.Completed())
	switch {
	case errors.Is(err, statestore.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return !bytes.Equal(data, want), nil
}

// provider returns the cloud provider called name.
func (s *Service) provider(name v1alpha1.Provider) (cloud.Provider, error) {
	if s.Providers == nil {
		return nil, errors.New("no cloud providers are set up")
	}
	return s.Providers(name)
}

// planChanges plans the changes that bring the cloud to the model m: those of the infrastructure, from a fresh
// inventory, and those of the nodes, from a fresh list of the machines.
func planChanges(ctx context.Context, p cloud.Provider, m *model.Cluster) (UpdatePlan, error) {
	snap, err := p.Inventory(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, err
	}
	tasks, err := p.BuildInfra(ctx, m)
	if err != nil {
		return UpdatePlan{}, err
	}
	infra, err := engine.NewPlan(ctx, tasks, p.InfraKinds(), snap)
	if err != nil {
		return UpdatePlan{}, err
	}
	instances, err := p.Nodes().List(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, err
	}
	return UpdatePlan{Infra: infra, Nodes: planNodes(m, instances)}, nil
}

// applyUpdate applies the plan of u in the order that Update gives, and writes the completed spec once every step has
// succeeded.
func (s *Service) applyUpdate(ctx context.Context, l statestore.Layout, u updateRun) error {
	if err := statestore.RaiseVersion(ctx, s.Store, l, s.Version); err != nil {
		return err
	}
	opts := s.applyOptions()
	if err := u.plan.Infra.ApplyTaskChanges(ctx, opts); err != nil {
		return fmt.Errorf("apply the infrastructure: %w", err)
	}
	for _, c := range u.plan.Nodes {
		if err := s.applyNode(ctx, u.nodes, u.cluster, c); err != nil {
			return err
		}
	}
	if err := u.plan.Infra.ApplyDeletes(ctx, opts); err != nil {
		return fmt.Errorf("delete infrastructure objects: %w", err)
	}
	if !u.plan.Completed {
		return nil
	}
	if _, err := s.Store.Put(ctx, l.Completed(), u.completed, statestore.PutOptions{}); err != nil {
		return fmt.Errorf("write the completed spec: %w", err)
	}
	return nil
}

// applyOptions returns the options that apply an infrastructure plan and report the engine's events as progress.
func (s *Service) applyOptions() engine.ApplyOptions {
	return engine.ApplyOptions{OnEvent: func(e engine.Event) { s.progress(Progress{Infra: &e}) }}
}

// applyNode carries out the node change c of the cluster and reports its steps: a failed one with the provider's
// error, which applyNode returns naming the node for a wait.
func (s *Service) applyNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange) error {
	s.progress(Progress{Node: c, Step: NodeStarted})
	in, err := changeNode(ctx, nodes, cluster, c)
	if err != nil {
		s.progress(Progress{Node: c, Step: NodeFailed, Err: err})
		if c.Action == NodeWait {
			return fmt.Errorf("wait for node %s: %w", c.Name, err)
		}
		return err
	}
	s.progress(Progress{Node: c, Step: NodeDone, Instance: in})
	return nil
}

// changeNode carries out the node change c of the cluster: a create with a new operation id; a wait, which repeats
// the create of the machine with its operation id; or a delete. It returns the machine of a create or a wait.
func changeNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange) (cloud.Instance, error) {
	switch c.Action {
	case NodeCreate:
		return createNode(ctx, nodes, cluster, c, cloud.NewOpID())
	case NodeWait:
		return createNode(ctx, nodes, cluster, c, c.Op)
	case NodeDelete:
		return cloud.Instance{}, nodes.Delete(ctx, cloud.Instance{ID: c.ID, Name: c.Name, Cluster: cluster})
	}
	return cloud.Instance{}, fmt.Errorf("node %s: unknown change %s", c.Name, c.Action)
}

// createNode creates the machine of the node change c with the operation id op, or finds the one that an earlier call
// with op created, waits until it is ready and returns it. It gives the call nodeTimeout.
func createNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange, op string) (
	cloud.Instance, error,
) {
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	return nodes.Create(ctx, cloud.CreateRequest{
		Cluster: cluster, Group: c.Group, Role: c.Role, Zone: c.Zone, Name: c.Name, MachineType: c.MachineType,
		Image: c.Image, Op: op, UserData: cloud.UserData(placeholderUserData),
	})
}

// progress tells OnProgress about p, when it is set.
func (s *Service) progress(p Progress) {
	if s.OnProgress != nil {
		s.OnProgress(p)
	}
}
