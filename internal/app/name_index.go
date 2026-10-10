package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/rollout"
	"github.com/ingvarch/tent/internal/statestore"
)

// nameIndexes is what tent remembers of the machine names of a cluster's server and combined groups: the highest
// index that a name of each group has had, as the state store holds it.
type nameIndexes struct {
	store   statestore.Store
	layout  statestore.Layout
	groups  []string                      // the groups that were read
	high    map[string]int                // by node group; a group without an object is absent
	version map[string]statestore.Version // of each group's object, as the run read or last wrote it
}

// serverGroups returns the names of the server and combined groups of m, in the order of m. When only is not empty,
// it returns just those that only names.
func serverGroups(m *model.Cluster, only []string) []string {
	var names []string
	for _, g := range m.Groups {
		if g.Role.RunsServer() && (len(only) == 0 || slices.Contains(only, g.Name)) {
			names = append(names, g.Name)
		}
	}
	return names
}

// readNames reads the highest index of each of the node groups from the store. A group without an object has none. It
// asks the store nothing but the objects, so that a plan writes nothing.
func (s *Service) readNames(ctx context.Context, l statestore.Layout, groups []string) (*nameIndexes, error) {
	n := &nameIndexes{
		store: s.Store, layout: l, groups: groups,
		high: map[string]int{}, version: map[string]statestore.Version{},
	}
	for _, group := range groups {
		p, err := l.NameIndex(group)
		if err != nil {
			return nil, err
		}
		data, v, err := s.Store.Get(ctx, p)
		if errors.Is(err, statestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		index, err := parseNameIndex(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s holds %q, which is no index of a node name; it must hold the highest index "+
				"that a machine name of node group %s has had", p, strings.TrimSuffix(string(data), "\n"), group)
		}
		n.high[group], n.version[group] = index, v
	}
	return n, nil
}

// parseNameIndex returns the index that the content of a name object holds: decimal digits and a newline, at most
// 2147483647.
func parseNameIndex(content string) (int, error) {
	digits := strings.TrimSuffix(content, "\n")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("no digits")
	}
	index, err := strconv.ParseInt(digits, 10, 32)
	return int(index), err
}

// next returns the lowest index that a new machine of the group may take: one above the highest that the store holds
// now, including what raise stored, and 0 when it holds none.
func (n *nameIndexes) next(group string) int {
	if high, ok := n.high[group]; ok {
		return high + 1
	}
	return 0
}

// raise stores index as the highest of the group when the store holds a lower one or none. Where the store has
// conditional puts, the write replaces only the object as n read or last wrote it, and creates only an object that n
// found missing. Callers hold the cluster's lock.
func (n *nameIndexes) raise(ctx context.Context, group string, index int) error {
	if high, ok := n.high[group]; ok && high >= index {
		return nil
	}
	p, err := n.layout.NameIndex(group)
	if err != nil {
		return err
	}
	caps, err := n.store.Capabilities(ctx)
	if err != nil {
		return err
	}
	var opts statestore.PutOptions
	if v, read := n.version[group]; caps.ConditionalPut {
		opts = statestore.PutOptions{IfNoneMatch: !read, IfMatch: v}
	}
	v, err := n.store.Put(ctx, p, []byte(strconv.Itoa(index)+"\n"), opts)
	switch {
	case errors.Is(err, statestore.ErrPreconditionFailed):
		return errors.New(p + " changed meanwhile; run the command again")
	case err != nil:
		return fmt.Errorf("write %s: %w", p, err)
	}
	n.high[group], n.version[group] = index, v
	return nil
}

// raiseToListed raises each group that n read to the highest index among the names of the listed machines that fit
// the group's pattern, and stops at the first write that fails. A group with no such machine is left as it is.
func (n *nameIndexes) raiseToListed(ctx context.Context, listed []cloud.Instance) error {
	for _, group := range n.groups {
		highest := -1
		for _, in := range listed {
			if index, ok := rollout.NodeIndex(n.layout.Cluster(), group, in.Name); ok {
				highest = max(highest, index)
			}
		}
		if highest < 0 {
			continue
		}
		if err := n.raise(ctx, group, highest); err != nil {
			return err
		}
	}
	return nil
}
