package statestore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// clusterSpec is the name of a cluster's spec, the object that makes a top-level name a cluster.
const clusterSpec = "cluster.yaml"

// Layout names the objects of one cluster in the store.
type Layout struct{ cluster string }

// NewLayout returns the layout of a cluster. The name must be one valid path segment.
func NewLayout(cluster string) (Layout, error) {
	if err := validName("cluster", cluster); err != nil {
		return Layout{}, err
	}
	return Layout{cluster}, nil
}

// validName checks that the name of a cluster or node group is one valid path segment.
func validName(kind, name string) error {
	why := "empty"
	if name != "" {
		why = badSegment(name)
	}
	if why != "" {
		return errors.New("invalid " + kind + " name: " + why)
	}
	return nil
}

// Cluster returns the cluster's name.
func (l Layout) Cluster() string { return l.cluster }

// Prefix returns the prefix of all the cluster's objects.
func (l Layout) Prefix() string { return l.cluster + "/" }

// TentVersion returns the path of the minimum tent version that may change the cluster.
func (l Layout) TentVersion() string { return l.Prefix() + "tent-version" }

// ClusterSpec returns the path of the cluster's spec.
func (l Layout) ClusterSpec() string { return l.Prefix() + clusterSpec }

// NodeGroup returns the path of a node group's spec. The name must be one valid path segment.
func (l Layout) NodeGroup(name string) (string, error) {
	if err := validName("node group", name); err != nil {
		return "", err
	}
	return l.NodeGroups() + name + ".yaml", nil
}

// NodeGroups returns the prefix of the node groups' specs.
func (l Layout) NodeGroups() string { return l.Prefix() + "nodegroups/" }

// Completed returns the path of the spec last applied, with all defaults.
func (l Layout) Completed() string { return l.Prefix() + "cluster.completed.yaml" }

// Lock returns the path of the cluster's lock.
func (l Layout) Lock() string { return l.Prefix() + "lock" }

// Clusters lists the clusters in a store, sorted: the top-level names that have a cluster.yaml.
func Clusters(ctx context.Context, s Store) ([]string, error) {
	paths, err := s.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	var clusters []string
	for _, p := range paths {
		if name, rest, _ := strings.Cut(p, "/"); rest == clusterSpec {
			clusters = append(clusters, name)
		}
	}
	// Paths sort "a-b/" before "a/", names sort "a" before "a-b".
	slices.Sort(clusters)
	return clusters, nil
}
