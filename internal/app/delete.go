package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/statestore"
)

// How a delete waits for the cloud to stop listing the nodes it deleted.
const (
	goneTimeout    = 5 * time.Minute // how long it waits
	goneCheckEvery = 5 * time.Second // how often it lists them meanwhile
)

// DeleteCluster deletes everything a cluster owns: its nodes, its infrastructure, such as its network, firewalls and
// SSH keys, and last its state in the store. The stored cluster.yaml names the cluster's cloud. The specs are neither
// validated nor checked against the cloud, so a cluster that is half built or whose specs are invalid can still be
// deleted. A cluster without cluster.yaml, such as one left from an interrupted create, has no known cloud: the plan
// says so, and only the state is deleted. So is a cluster on a provider that tent cannot manage yet, for which
// Providers fails with an error that matches cloud.ErrUnsupportedProvider: tent made no cloud objects there, and the
// plan names the provider.
//
// The state is the tent version, the specs and the completed spec. Other objects of the cluster in the store make it
// refuse before it calls the cloud, unless force is set: then they are deleted with the rest of the state. The lock's
// lease is left to the lock, which removes it when it is released. Without apply, DeleteCluster returns the plan and
// changes nothing.
//
// With apply it takes the cluster's lock, plans again under it, calls OnDeletePlan with that plan and deletes in this
// order: every node, one at a time by name; then it lists the nodes every 5 seconds until the cloud lists none, for up
// to 5 minutes; then every infrastructure object that a fresh inventory finds, in the engine's order; then the state,
// with cluster.yaml and the tent version last, so that a delete that stops can run again. The first step that fails
// stops the delete, and the state stays until the cloud's part has succeeded. It returns the plan it applied, made
// under the lock, with the error; the plan says Applied once every step has succeeded. When the lock is lost after the
// deletes, the plan comes back together with an error that matches statestore.ErrLockLost.
func (s *Service) DeleteCluster(ctx context.Context, cluster string, apply, force bool) (_ DeletePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return DeletePlan{}, err
	}
	d, err := s.planDelete(ctx, l, force)
	if err != nil || !apply {
		return d.plan, err
	}
	err = s.locked(ctx, l, "delete", func(ctx context.Context) error {
		var err error
		if d, err = s.planDelete(ctx, l, force); err != nil {
			return err
		}
		if s.OnDeletePlan != nil {
			if err := s.OnDeletePlan(d.plan); err != nil {
				return err
			}
		}
		if err := s.applyDelete(ctx, &d); err != nil {
			return err
		}
		d.plan.Applied = true
		return nil
	})
	return d.plan, err
}

// deleteRun is the plan of a delete, with what applying it needs.
type deleteRun struct {
	plan     DeletePlan
	cluster  string
	provider cloud.Provider // nil when the cloud is unknown
}

// planDelete plans the delete of a cluster: its state first, which may refuse, then its nodes and its infrastructure
// on the cloud that the stored cluster.yaml names, unless tent cannot manage that cloud yet.
func (s *Service) planDelete(ctx context.Context, l statestore.Layout, force bool) (deleteRun, error) {
	state, err := s.state(ctx, l, force)
	if err != nil {
		return deleteRun{}, err
	}
	d := deleteRun{plan: DeletePlan{State: state}, cluster: l.Cluster()}
	name, known, err := s.storedProvider(ctx, l)
	switch {
	case err != nil:
		return deleteRun{}, err
	case !known:
		d.plan.CloudUnknown = true
		return d, nil
	}
	p, err := s.provider(name)
	switch {
	case errors.Is(err, cloud.ErrUnsupportedProvider):
		d.plan.Unsupported = name
		return d, nil
	case err != nil:
		return deleteRun{}, err
	}
	d.provider = p
	instances, err := clusterNodes(ctx, d.provider.Nodes(), d.cluster)
	if err != nil {
		return deleteRun{}, err
	}
	d.plan.Nodes = nodeDeletes(instances)
	if d.plan.Infra, err = planTeardown(ctx, d.provider, d.cluster); err != nil {
		return deleteRun{}, err
	}
	return d, nil
}

// storedProvider returns the cloud provider that the cluster's stored cluster.yaml names, and false when there is no
// cluster.yaml. It decodes the spec as stored, without defaults, and does not validate it.
func (s *Service) storedProvider(ctx context.Context, l statestore.Layout) (v1alpha1.Provider, bool, error) {
	data, _, err := s.Store.Get(ctx, l.ClusterSpec())
	switch {
	case errors.Is(err, statestore.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	c, err := decodeCluster(l.ClusterSpec(), data)
	if err != nil {
		return "", false, err
	}
	return c.Spec.Cloud.Provider, true, nil
}

// clusterNodes lists the machines of the cluster. Only the machines with the cluster's label count; the others are
// left alone.
func clusterNodes(ctx context.Context, nodes cloud.Nodes, cluster string) ([]cloud.Instance, error) {
	instances, err := nodes.List(ctx, cluster)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(instances, func(in cloud.Instance) bool { return in.Cluster != cluster }), nil
}

// nodeDeletes returns the deletes of the machines, by name, then ID.
func nodeDeletes(instances []cloud.Instance) []NodeChange {
	var deletes []NodeChange
	for _, in := range instances {
		deletes = append(deletes, deleteNode(in, ""))
	}
	slices.SortFunc(deletes, compareNameID)
	return deletes
}

// planTeardown plans the delete of every infrastructure object that the cluster owns, from a fresh inventory.
func planTeardown(ctx context.Context, p cloud.Provider, cluster string) (*engine.Plan, error) {
	snap, err := p.Inventory(ctx, cluster)
	if err != nil {
		return nil, err
	}
	return engine.NewPlan(ctx, nil, p.InfraKinds(), snap)
}

// applyDelete applies the plan of d: the cloud's part when the cloud is known, then the state.
func (s *Service) applyDelete(ctx context.Context, d *deleteRun) error {
	if d.provider != nil {
		if err := s.deleteCloud(ctx, d); err != nil {
			return err
		}
	}
	for _, p := range d.plan.State {
		if err := s.Store.Delete(ctx, p); err != nil {
			return fmt.Errorf("delete the state: %w", err)
		}
	}
	return nil
}

// deleteCloud deletes the nodes of d's plan, waits until the cloud lists none, and deletes the infrastructure that a
// fresh inventory finds. The plan's infrastructure becomes the one it applies.
func (s *Service) deleteCloud(ctx context.Context, d *deleteRun) error {
	nodes := d.provider.Nodes()
	for _, c := range d.plan.Nodes {
		if err := s.applyNode(ctx, nodes, d.cluster, c); err != nil {
			return err
		}
	}
	if err := s.waitNodesGone(ctx, nodes, d.cluster); err != nil {
		return fmt.Errorf("wait for the nodes to go: %w", err)
	}
	infra, err := planTeardown(ctx, d.provider, d.cluster)
	if err != nil {
		return fmt.Errorf("delete the infrastructure: %w", err)
	}
	d.plan.Infra = infra
	if err := infra.Apply(ctx, s.applyOptions()); err != nil {
		return fmt.Errorf("delete the infrastructure: %w", err)
	}
	return nil
}

// waitNodesGone lists the cluster's machines every goneCheckEvery until the cloud lists none. When the first list
// holds machines, it reports how many it waits for, once. It fails with the machines that the cloud still lists after
// goneTimeout.
func (s *Service) waitNodesGone(ctx context.Context, nodes cloud.Nodes, cluster string) error {
	deadline := time.Now().Add(goneTimeout)
	for first := true; ; first = false {
		left, err := clusterNodes(ctx, nodes, cluster)
		switch {
		case err != nil:
			return err
		case len(left) == 0:
			return nil
		case !time.Now().Before(deadline):
			names := make([]string, len(left))
			for i, c := range nodeDeletes(left) {
				names[i] = c.Name + " (" + c.ID + ")"
			}
			return fmt.Errorf("still listed after %v: %s", goneTimeout, strings.Join(names, ", "))
		}
		if first {
			s.progress(Progress{Going: len(left)})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(goneCheckEvery):
		}
	}
}

// state returns the paths of a cluster's state in the order of deletion: the cluster's spec and tent version last.
func (s *Service) state(ctx context.Context, l statestore.Layout, force bool) ([]string, error) {
	all, err := s.Store.List(ctx, l.Prefix())
	if err != nil {
		return nil, err
	}
	last := []string{l.ClusterSpec(), l.TentVersion()}
	var paths, unknown []string
	for _, p := range all {
		switch {
		case p == l.Lock() || slices.Contains(last, p):
			continue
		case p != l.Completed() && !isGroup(l, p):
			unknown = append(unknown, p)
		}
		paths = append(paths, p)
	}
	for _, p := range last {
		if slices.Contains(all, p) {
			paths = append(paths, p)
		}
	}
	switch {
	case len(paths) == 0:
		return nil, s.notFound(clusterLabel(l.Cluster()))
	case len(unknown) > 0 && !force:
		return nil, errors.New(clusterLabel(l.Cluster()) + " holds objects tent does not know: " +
			strings.Join(unknown, ", ") + "; delete them yourself or use --force")
	}
	return paths, nil
}
