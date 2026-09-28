package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// PlannedChange is one change of a plan: a task's change to its object, or the delete of an owned object.
type PlannedChange struct {
	Key
	ID string `json:"id,omitempty"` // the object's ID for a delete; empty otherwise
	Change
}

// Plan is the changes that bring the cloud to the objects that the tasks want. It keeps the Env that its tasks
// planned with, so applying the plan sees the same snapshot and outputs.
type Plan struct {
	env     *Env
	graph   *graph
	planned map[Key]Change // the change of each task, Noop included
	pruned  []Object       // the objects to delete, as the snapshot had them, in apply order

	mu          sync.Mutex // guards stage and skipDeletes
	stage       stage
	skipDeletes string // why the deletes are skipped, set when the task changes end; empty when they may run
}

// NewPlan plans tasks against snap: each task in topological order, then a delete for every object of snap that no
// task claims or that is a duplicate. A nil snap has no objects.
func NewPlan(ctx context.Context, tasks []Task, kinds []Kind, snap Snapshot) (*Plan, error) {
	g, err := newGraph(tasks, kinds)
	if err != nil {
		return nil, err
	}
	var objects []Object
	if snap != nil {
		objects = snap.Objects()
	}
	if err := checkObjects(objects, g.kinds); err != nil {
		return nil, err
	}
	p := &Plan{
		env:     &Env{Snapshot: snap, Outputs: &Outputs{}},
		graph:   g,
		planned: make(map[Key]Change, len(tasks)),
	}
	for _, t := range g.order {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
		k := t.Key()
		ch, err := t.Plan(ctx, p.env)
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", k, err)
		}
		switch ch.Action {
		case Noop, Update:
		case Create, Replace:
			// The object gets new values, so its dependents must not see the old ones.
			p.env.Outputs.clear(k)
		default:
			return nil, fmt.Errorf("plan %s: a task may plan noop, create, update or replace, not %s", k, ch.Action)
		}
		p.planned[k] = ch
	}
	p.pruned = prune(objects, p.planned, g.kinds)
	return p, nil
}

// checkObjects checks that each object has one of the kinds and that no two objects other than duplicates share a
// key.
func checkObjects(objects []Object, kinds map[string]kindEntry) error {
	ids := make(map[Key]string, len(objects)) // the ID of each key's object that is not a duplicate
	for _, o := range objects {
		if _, ok := kinds[o.Key.Kind]; !ok {
			return fmt.Errorf("the snapshot has %s (ID %s) of unknown kind %s", o.Key, o.ID, o.Key.Kind)
		}
		if o.Duplicate {
			continue
		}
		if id, ok := ids[o.Key]; ok {
			return fmt.Errorf("the snapshot has %s twice (IDs %s and %s); the provider must mark duplicates",
				o.Key, id, o.ID)
		}
		ids[o.Key] = o.ID
	}
	return nil
}

// prune returns each object that no task claims or that is a duplicate, in the order of the kinds, and within a kind
// by name, then ID.
func prune(objects []Object, planned map[Key]Change, kinds map[string]kindEntry) []Object {
	var pruned []Object
	for _, o := range objects {
		if _, claimed := planned[o.Key]; o.Duplicate || !claimed {
			pruned = append(pruned, o)
		}
	}
	slices.SortStableFunc(pruned, func(a, b Object) int {
		return cmp.Or(cmp.Compare(kinds[a.Key.Kind].rank, kinds[b.Key.Kind].rank),
			strings.Compare(a.Key.Name, b.Key.Name), strings.Compare(a.ID, b.ID))
	})
	return pruned
}

// deleteOf returns the planned delete of the object o.
func deleteOf(o Object) PlannedChange {
	var reason string
	if o.Duplicate {
		reason = "duplicate"
	}
	return PlannedChange{Key: o.Key, ID: o.ID, Change: Change{Action: Delete, Reason: reason}}
}

// Changes returns every change other than Noop in apply order: task changes in topological order, then deletes in
// the order of the kinds, and within a kind by name, then ID. The caller may change the result.
func (p *Plan) Changes() []PlannedChange {
	var changes []PlannedChange
	for _, t := range p.graph.order {
		k := t.Key()
		if ch := p.planned[k]; ch.Action != Noop {
			changes = append(changes, PlannedChange{Key: k, Change: ch})
		}
	}
	for _, o := range p.pruned {
		changes = append(changes, deleteOf(o))
	}
	for i := range changes {
		changes[i].Diff = slices.Clone(changes[i].Diff)
	}
	return changes
}

// HasChanges reports whether the plan has a change other than Noop.
func (p *Plan) HasChanges() bool { return len(p.Changes()) > 0 }
