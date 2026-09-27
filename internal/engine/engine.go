// Package engine reconciles cloud objects with the objects a cluster wants. Providers implement a Task for each
// desired object; the engine orders the tasks by their dependencies, plans every change from one snapshot of the
// cloud, applies the changes in parallel and deletes owned objects that no task wants. It never calls a cloud itself.
package engine

import (
	"context"
	"fmt"
	"sync"
)

// Key identifies one desired cloud object: its kind and its deterministic name.
type Key struct {
	Kind string `json:"kind"` // e.g. vultr.FirewallGroup
	Name string `json:"name"` // e.g. prod-servers
}

// String returns kind/name, such as vultr.FirewallGroup/prod-servers.
func (k Key) String() string { return k.Kind + "/" + k.Name }

// Action is what a change does to a cloud object.
type Action int

// Actions.
const (
	// Noop leaves the object as it is.
	Noop Action = iota
	// Create creates the object.
	Create
	// Update changes the object in place.
	Update
	// Replace creates the object anew, for changes that cannot be made in place.
	Replace
	// Delete deletes the object.
	Delete
)

var actionNames = [...]string{"noop", "create", "update", "replace", "delete"}

// String returns the action's name in lower case, such as create.
func (a Action) String() string {
	if a < 0 || int(a) >= len(actionNames) {
		return fmt.Sprintf("Action(%d)", int(a))
	}
	return actionNames[a]
}

// MarshalText returns the action's name, as String does.
func (a Action) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// FieldDiff is one changed field, rendered by the task. Old is empty for an added value, New for a removed one.
type FieldDiff struct {
	Field string `json:"field"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
}

// Change is what a task plans to do to its object.
type Change struct {
	Action Action      `json:"action"`
	Reason string      `json:"reason,omitempty"`
	Diff   []FieldDiff `json:"diff,omitempty"`
}

// Object is one cloud object the cluster owns, as the snapshot saw it.
type Object struct {
	Key       Key
	ID        string
	Duplicate bool // the provider keeps another object with this key; this one goes
}

// Snapshot is every object the cluster owns, listed once per run. Tasks read the provider's own snapshot type
// through a type assertion; the engine reads only Objects.
type Snapshot interface {
	Objects() []Object
}

// Env is what tasks see during a run.
type Env struct {
	Snapshot Snapshot
	Outputs  *Outputs
}

// Task is one desired cloud object. Providers implement tasks; the engine orders, plans and applies them.
//
// Tasks treat the snapshot as read-only, because Apply calls run concurrently. They ignore the objects that the
// snapshot marks Duplicate; the engine deletes those.
type Task interface {
	// Key identifies the object. No two tasks of a run have the same key.
	Key() Key
	// Deps lists the keys of the tasks whose changes must be applied before this one; the order holds through tasks
	// without changes.
	Deps() []Key
	// Plan compares the desired object with env.Snapshot. It must not call the cloud. When the object exists, Plan
	// sets its outputs: the engine does not call Apply for a task without changes, so its dependents see what Plan
	// set.
	Plan(ctx context.Context, env *Env) (Change, error)
	// Apply carries out a planned change.
	//
	// After a create or a replace, Apply sets the outputs again: the engine clears them after planning such a
	// change. A value that was unknown at plan time is read from env.Outputs, not from ch.Diff.
	//
	// After a Retryable error the engine calls Apply again with the same ch, so Apply must be safe to run again. An
	// operation id must therefore stay the same across attempts: the task keeps it and does not make a new one per
	// call. After an ambiguous failure, the next attempt searches by operation id before it creates.
	Apply(ctx context.Context, env *Env, ch Change) error
	// Delete removes an owned object of the task's kind, as Deleter says.
	Delete(ctx context.Context, env *Env, obj Object) error
}

// Deleter deletes owned objects of one kind; a task of the kind is one. Delete must be safe to run again: an object
// that is already gone counts as deleted.
type Deleter interface {
	Delete(ctx context.Context, env *Env, obj Object) error
}

// Kind is one kind of object that tasks manage. The engine deletes objects of the kinds in the order they are given,
// so a task's kind must come before the kinds of the tasks it depends on.
type Kind struct {
	Name    string // the Key.Kind of its tasks and objects
	Deleter Deleter
}

// KnownAfterApply is what a diff shows for a value that is not known until a task is applied.
const KnownAfterApply = "(known after apply)"

// Outputs holds the values that tasks produce for their dependents, such as IDs and addresses. A task's Plan sets the
// outputs of an object that exists. After a task plans a create or a replace, the engine clears its outputs, and its
// Apply sets them again. Outputs is safe for concurrent use. The zero Outputs is empty and ready to use.
type Outputs struct {
	mu     sync.RWMutex
	values map[Key]map[string]string
}

// Set records the task k's output called name.
func (o *Outputs) Set(k Key, name, value string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.values == nil {
		o.values = make(map[Key]map[string]string)
	}
	if o.values[k] == nil {
		o.values[k] = make(map[string]string)
	}
	o.values[k][name] = value
}

// Get returns the task k's output called name, and whether it is known.
func (o *Outputs) Get(k Key, name string) (value string, known bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	value, known = o.values[k][name]
	return value, known
}

// clear forgets every output of the task k.
func (o *Outputs) clear(k Key) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.values, k)
}
