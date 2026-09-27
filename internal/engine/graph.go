package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// graph is a valid set of tasks with their dependencies and kinds.
type graph struct {
	// order holds the tasks so that each comes after its dependencies. Ties keep the input order.
	order []Task
	// deps holds the dependencies of each task, each once, in the order the task lists them.
	deps map[Key][]Key
	// dependents holds the tasks that depend on each task, in input order.
	dependents map[Key][]Key
	// kinds holds each kind by name.
	kinds map[string]kindEntry
}

// kindEntry is a kind's place in the deletion order and its deleter.
type kindEntry struct {
	rank    int
	deleter Deleter
}

// newGraph checks the tasks and kinds and orders the tasks by their dependencies. It returns the first problem it
// finds.
func newGraph(tasks []Task, kinds []Kind) (*graph, error) {
	keys := make([]Key, len(tasks))
	for i, t := range tasks {
		keys[i] = t.Key()
	}
	index, err := indexKeys(keys)
	if err != nil {
		return nil, err
	}
	kindIndex, err := indexKinds(keys, kinds)
	if err != nil {
		return nil, err
	}
	g := &graph{deps: make(map[Key][]Key), dependents: make(map[Key][]Key), kinds: kindIndex}
	for i, t := range tasks {
		k := keys[i]
		for _, d := range t.Deps() {
			if _, ok := index[d]; !ok {
				return nil, fmt.Errorf("task %s depends on %s, which is not a task", k, d)
			}
			if !slices.Contains(g.deps[k], d) {
				g.deps[k] = append(g.deps[k], d)
				g.dependents[d] = append(g.dependents[d], k)
			}
		}
	}
	// A cycle first: no kind order fits a cycle across kinds, so its kind order error would mislead.
	if err := g.sort(tasks, keys, index); err != nil {
		return nil, err
	}
	if err := g.checkKindOrder(keys); err != nil {
		return nil, err
	}
	return g, nil
}

// checkKindOrder checks that the kind of each task comes before the kinds of its dependencies, because the objects of
// the earlier kinds are deleted first.
func (g *graph) checkKindOrder(keys []Key) error {
	for _, k := range keys {
		for _, d := range g.deps[k] {
			if g.kinds[k.Kind].rank > g.kinds[d.Kind].rank {
				return fmt.Errorf("kind %s must come before kind %s, because task %s depends on %s", k.Kind, d.Kind, k, d)
			}
		}
	}
	return nil
}

// indexKeys returns the input position of each key. Every key needs a kind and a name, and no two keys are equal.
func indexKeys(keys []Key) (map[Key]int, error) {
	for _, k := range keys {
		if k.Kind == "" {
			return nil, fmt.Errorf("task %s has no kind", k)
		}
		if k.Name == "" {
			return nil, fmt.Errorf("task of kind %s has no name", k.Kind)
		}
	}
	index := make(map[Key]int, len(keys))
	for i, k := range keys {
		if _, ok := index[k]; ok {
			return nil, fmt.Errorf("duplicate task %s", k)
		}
		index[k] = i
	}
	return index, nil
}

// indexKinds returns each kind by name. Every kind needs a name and a deleter and is listed once, and every task's
// kind is listed.
func indexKinds(keys []Key, kinds []Kind) (map[string]kindEntry, error) {
	index := make(map[string]kindEntry, len(kinds))
	for i, kd := range kinds {
		if kd.Name == "" {
			return nil, errors.New("a kind has no name")
		}
		if _, ok := index[kd.Name]; ok {
			return nil, fmt.Errorf("kind %s is listed twice", kd.Name)
		}
		if kd.Deleter == nil {
			return nil, fmt.Errorf("kind %s has no deleter", kd.Name)
		}
		index[kd.Name] = kindEntry{rank: i, deleter: kd.Deleter}
	}
	for _, k := range keys {
		if _, ok := index[k.Kind]; !ok {
			return nil, fmt.Errorf("task %s has kind %s, which is not among the kinds", k, k.Kind)
		}
	}
	return index, nil
}

// sort puts the tasks in g.order by Kahn's algorithm, always taking the ready task that comes first in the input. It
// fails when the dependencies have a cycle.
func (g *graph) sort(tasks []Task, keys []Key, index map[Key]int) error {
	waiting := make(map[Key]int, len(keys)) // dependencies not yet in the order
	var ready []int                         // input positions, ascending
	for i, k := range keys {
		waiting[k] = len(g.deps[k])
		if waiting[k] == 0 {
			ready = append(ready, i)
		}
	}
	g.order = make([]Task, 0, len(tasks))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		g.order = append(g.order, tasks[i])
		for _, d := range g.dependents[keys[i]] {
			waiting[d]--
			if waiting[d] == 0 {
				j := index[d]
				at, _ := slices.BinarySearch(ready, j)
				ready = slices.Insert(ready, at, j)
			}
		}
	}
	if len(g.order) < len(tasks) {
		return g.cycle(keys, index, waiting)
	}
	return nil
}

// cycle returns the error for a dependency cycle among the tasks that sort left out. The cycle starts at its first
// task in input order.
func (g *graph) cycle(keys []Key, index map[Key]int, waiting map[Key]int) error {
	left := func(k Key) bool { return waiting[k] > 0 }
	// Each task left out waits for another one, so a walk along such dependencies comes back to a task on its path.
	var path []Key
	k := keys[slices.IndexFunc(keys, left)]
	for !slices.Contains(path, k) {
		path = append(path, k)
		for _, d := range g.deps[k] {
			if left(d) {
				k = d
				break
			}
		}
	}
	path = path[slices.Index(path, k):]
	first := 0
	for i, p := range path {
		if index[p] < index[path[first]] {
			first = i
		}
	}
	names := make([]string, 0, len(path)+1)
	for _, p := range slices.Concat(path[first:], path[:first+1]) {
		names = append(names, p.String())
	}
	return fmt.Errorf("dependency cycle: %s", strings.Join(names, " -> "))
}
