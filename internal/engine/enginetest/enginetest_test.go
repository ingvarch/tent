package enginetest_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
)

var errBoom = errors.New("boom")

// boxKind is the kind of the test objects.
const boxKind = "test.Box"

// boxKey is the key of the box named name.
func boxKey(name string) engine.Key { return engine.Key{Kind: boxKind, Name: name} }

// cloud is an in-memory cloud of boxes: the size of each box by name. A box's name is also its ID. It is safe for
// concurrent use.
type cloud struct {
	mu    sync.Mutex
	sizes map[string]string
}

// inventory lists the boxes of the cloud.
func (c *cloud) inventory(context.Context) (engine.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return snapshot(maps.Clone(c.sizes)), nil
}

// set gives the box name its size, and creates the box when there is none.
func (c *cloud) set(name, size string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sizes == nil {
		c.sizes = make(map[string]string)
	}
	c.sizes[name] = size
}

// remove deletes the box name.
func (c *cloud) remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sizes, name)
}

// kinds lists the kind of the boxes of c.
func (c *cloud) kinds() []engine.Kind { return []engine.Kind{{Name: boxKind, Deleter: box{cloud: c}}} }

// snapshot is the size of each box by name, as the cloud had them.
type snapshot map[string]string

// Objects returns the boxes by name.
func (s snapshot) Objects() []engine.Object {
	var objects []engine.Object
	for _, name := range slices.Sorted(maps.Keys(s)) {
		objects = append(objects, engine.Object{Key: boxKey(name), ID: name})
	}
	return objects
}

// box wants the box name in the cloud with size. Its Apply fails with err when err is set, and changes nothing when
// broken is set.
type box struct {
	cloud  *cloud
	name   string
	size   string
	broken bool
	err    error
}

func (b box) Key() engine.Key  { return boxKey(b.name) }
func (box) Deps() []engine.Key { return nil }

func (b box) Plan(_ context.Context, env *engine.Env) (engine.Change, error) {
	have, found := env.Snapshot.(snapshot)[b.name]
	switch {
	case !found:
		return engine.Change{Action: engine.Create, Diff: []engine.FieldDiff{{Field: "size", New: b.size}}}, nil
	case have != b.size:
		return engine.Change{Action: engine.Update, Diff: []engine.FieldDiff{{Field: "size", Old: have, New: b.size}}},
			nil
	default:
		return engine.Change{Action: engine.Noop}, nil
	}
}

func (b box) Apply(context.Context, *engine.Env, engine.Change) error {
	if b.err != nil {
		return b.err
	}
	if !b.broken {
		b.cloud.set(b.name, b.size)
	}
	return nil
}

func (b box) Delete(_ context.Context, _ *engine.Env, obj engine.Object) error {
	b.cloud.remove(obj.ID)
	return nil
}

// recorder is a testing.TB that records failures instead of failing the test. Like testing.T's, its Fatalf stops the
// goroutine that calls it, so ApplyReplan runs in a goroutine of its own (see applyReplan).
type recorder struct {
	testing.TB
	failures []string // "Errorf: message" or "Fatalf: message", in order
	helper   bool     // whether Helper was called
}

func (r *recorder) Helper() { r.helper = true }

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, "Errorf: "+fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, "Fatalf: "+fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// applyReplan runs enginetest.ApplyReplan with a recorder in a goroutine of its own, and returns the recorder once
// ApplyReplan has returned or stopped at Fatalf.
func applyReplan(t *testing.T, tasks []engine.Task, kinds []engine.Kind,
	inventory func(context.Context) (engine.Snapshot, error)) *recorder {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		enginetest.ApplyReplan(r, tasks, kinds, inventory)
	}()
	<-done
	return r
}

func TestApplyReplanConverges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes map[string]string // the cloud before
	}{
		{name: "create"},
		{name: "update", sizes: map[string]string{"a": "large"}},
		{name: "delete", sizes: map[string]string{"a": "small", "old": "small"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cloud{sizes: tc.sizes}
			r := applyReplan(t, []engine.Task{box{cloud: c, name: "a", size: "small"}}, c.kinds(), c.inventory)
			if len(r.failures) > 0 {
				t.Errorf("failures = %q, want none", r.failures)
			}
			if !r.helper {
				t.Error("ApplyReplan did not call Helper")
			}
			if diff := cmp.Diff(map[string]string{"a": "small"}, c.sizes); diff != "" {
				t.Errorf("cloud after ApplyReplan (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyReplanFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		box    box
		failAt int // the inventory call that fails with errBoom, counting from 1; 0 for none
		want   []string
		cloud  map[string]string // the cloud after ApplyReplan
	}{
		{
			name: "apply leaves the cloud as it was",
			box:  box{name: "a", size: "small", broken: true},
			want: []string{"Errorf: the plan after apply has changes, want none:\n" +
				"+ test.Box/a\n    + size: small\n\nPlan: 1 to create, 0 to update, 0 to replace, 0 to delete.\n"},
		},
		{
			name:   "inventory before apply fails",
			box:    box{name: "a", size: "small"},
			failAt: 1,
			want:   []string{"Fatalf: inventory before apply: boom"},
		},
		{
			name: "plan fails",
			box:  box{size: "small"},
			want: []string{"Fatalf: plan before apply: task of kind test.Box has no name"},
		},
		{
			name: "apply fails",
			box:  box{name: "a", size: "small", err: errBoom},
			want: []string{"Fatalf: apply: test.Box/a: boom"},
		},
		{
			name:   "inventory after apply fails",
			box:    box{name: "a", size: "small"},
			failAt: 2,
			want:   []string{"Fatalf: inventory after apply: boom"},
			cloud:  map[string]string{"a": "small"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cloud{}
			calls := 0
			inventory := func(ctx context.Context) (engine.Snapshot, error) {
				calls++
				if calls == tc.failAt {
					return nil, errBoom
				}
				return c.inventory(ctx)
			}
			tc.box.cloud = c
			r := applyReplan(t, []engine.Task{tc.box}, c.kinds(), inventory)
			if diff := cmp.Diff(tc.want, r.failures); diff != "" {
				t.Errorf("failures (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.cloud, c.sizes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("cloud after ApplyReplan (-want +got):\n%s", diff)
			}
		})
	}
}
