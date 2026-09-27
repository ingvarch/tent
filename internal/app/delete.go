package app

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/statestore"
)

// DeleteState lists a cluster's state, and deletes it when apply is set: the tent version, the specs and the completed
// spec. Other objects of the cluster make it refuse, unless force is set: then it deletes them too. The lock's lease
// is left to the lock, which removes it when it is released. The tent version goes last, so a delete that stops can
// run again. It returns the paths in the order of deletion. When the lock is lost after the deletes, the paths come
// back together with an error that matches statestore.ErrLockLost.
func (s *Service) DeleteState(ctx context.Context, cluster string, apply, force bool) (_ []string, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := clusterLayout(cluster)
	if err != nil {
		return nil, err
	}
	var paths []string
	plan := func(ctx context.Context) ([]step, error) {
		state, err := s.state(ctx, l, force)
		paths = state
		steps := make([]step, len(state))
		for i, p := range state {
			steps[i] = func(ctx context.Context) error { return s.Store.Delete(ctx, p) }
		}
		return steps, err
	}
	err = s.change(ctx, l, "delete", apply, plan)
	if err != nil && !saved(err) {
		return nil, err
	}
	return paths, err
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
