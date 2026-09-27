package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// The kinds of the test tasks and objects.
const (
	thingKind = "test.Thing"
	baseKind  = "test.Base"
)

// thing is the key of the test.Thing named name.
func thing(name string) Key { return Key{Kind: thingKind, Name: name} }

// things are the keys of the test.Things named names.
func things(names ...string) []Key {
	var keys []Key
	for _, n := range names {
		keys = append(keys, thing(n))
	}
	return keys
}

// base is the key of the test.Base named name.
func base(name string) Key { return Key{Kind: baseKind, Name: name} }

// keysOf returns the keys of tasks.
func keysOf(tasks []Task) []Key {
	var keys []Key
	for _, t := range tasks {
		keys = append(keys, t.Key())
	}
	return keys
}

// fakeTask is a task with a key and dependencies. Its Plan sets outputs and returns change and err; Apply and Delete
// do nothing.
type fakeTask struct {
	key     Key
	deps    []Key
	outputs values
	change  Change
	err     error
}

func (t fakeTask) Key() Key    { return t.key }
func (t fakeTask) Deps() []Key { return t.deps }

func (t fakeTask) Plan(_ context.Context, env *Env) (Change, error) {
	for name, v := range t.outputs {
		env.Outputs.Set(t.key, name, v)
	}
	return t.change, t.err
}

func (fakeTask) Apply(context.Context, *Env, Change) error  { return nil }
func (fakeTask) Delete(context.Context, *Env, Object) error { return nil }

// task is the test.Thing task named name that depends on the test.Things named deps.
func task(name string, deps ...string) Task {
	return fakeTask{key: thing(name), deps: things(deps...)}
}

// thingKinds lists the kind of the fake tasks.
var thingKinds = []Kind{{Name: thingKind, Deleter: fakeTask{}}}

// values are the fields of a fake object by name.
type values = map[string]string

// fakeObject is an object of the fake cloud.
type fakeObject struct {
	Object
	Values values
}

// object is the fake object k with the given ID and values.
func object(k Key, id string, vals values) fakeObject {
	return fakeObject{Object: Object{Key: k, ID: id}, Values: vals}
}

// duplicate returns o marked as a duplicate, as a provider's inventory marks extra copies.
func duplicate(o fakeObject) fakeObject {
	o.Duplicate = true
	return o
}

// fakeCloud is an in-memory cloud for tests. Cloud tasks reach it through its snapshot: they read the snapshot in
// Plan and change the cloud in Apply and Delete. It logs their calls and is safe for concurrent use.
//
// Each Apply and Delete is a call to a target: the task's key, such as test.Thing/a, or the object's key and ID, such
// as test.Thing/a (ID 1). A call takes the cloud's delay, waits while its target is held, and then fails as the
// target's fault says.
type fakeCloud struct {
	mu       sync.Mutex
	objects  []fakeObject // in creation order
	created  int          // objects that tasks created, for their IDs
	log      []string
	delay    time.Duration            // how long each call takes
	faults   map[string]fault         // by target
	holds    map[string]chan struct{} // closed when the target is released, by target
	attempts map[string]int           // calls so far, by target
	busy     int                      // calls in progress
	maxBusy  int                      // the most calls in progress at once
}

// fault returns the error of attempt n of a call, counting from 1, or nil to let the attempt go on.
type fault func(n int) error

// failFirst is the fault of a call whose first n attempts fail with err.
func failFirst(n int, err error) fault {
	return func(i int) error {
		if i <= n {
			return err
		}
		return nil
	}
}

// setFault makes the calls to target fail as f says.
func (c *fakeCloud) setFault(target string, f fault) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faults == nil {
		c.faults = make(map[string]fault)
	}
	c.faults[target] = f
}

// hold makes the calls to target wait until release is called or their context ends.
func (c *fakeCloud) hold(target string) (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.holds == nil {
		c.holds = make(map[string]chan struct{})
	}
	held := make(chan struct{})
	c.holds[target] = held
	return sync.OnceFunc(func() { close(held) })
}

// begin starts a call to target: it takes the delay, waits while target is held and returns the fault of this
// attempt, or the context's error when the context ends first. end marks the call as over.
func (c *fakeCloud) begin(ctx context.Context, target string) (end func(), err error) {
	c.mu.Lock()
	if c.attempts == nil {
		c.attempts = make(map[string]int)
	}
	c.attempts[target]++
	attempt, f, held, delay := c.attempts[target], c.faults[target], c.holds[target], c.delay
	c.busy++
	c.maxBusy = max(c.maxBusy, c.busy)
	c.mu.Unlock()
	end = func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.busy--
	}
	if delay > 0 {
		if err := sleep(ctx, delay); err != nil {
			return end, err
		}
	}
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return end, ctx.Err()
		}
	}
	if f != nil {
		return end, f(attempt)
	}
	return end, nil
}

// attemptsOf returns the number of calls to target so far.
func (c *fakeCloud) attemptsOf(target string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[target]
}

// load returns the number of calls in progress and the most there were at once.
func (c *fakeCloud) load() (busy, most int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy, c.maxBusy
}

// newFakeCloud returns a fake cloud that holds objects.
func newFakeCloud(objects ...fakeObject) *fakeCloud {
	return &fakeCloud{objects: objects}
}

// snapshot returns a copy of the cloud's objects.
func (c *fakeCloud) snapshot() *fakeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	objects := make([]fakeObject, len(c.objects))
	for i, o := range c.objects {
		o.Values = maps.Clone(o.Values)
		objects[i] = o
	}
	return &fakeSnapshot{cloud: c, objects: objects}
}

// calls returns the calls that tasks made, in order, such as "plan test.Thing/a", "apply test.Thing/a create" and
// "delete test.Thing/b (ID 2)".
func (c *fakeCloud) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.log)
}

// record adds a call to the log.
func (c *fakeCloud) record(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, fmt.Sprintf(format, args...))
}

// create adds the object k with vals and returns its ID: new-1, new-2 and so on.
func (c *fakeCloud) create(k Key, vals values) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	id := fmt.Sprintf("new-%d", c.created)
	c.objects = append(c.objects, object(k, id, maps.Clone(vals)))
	return id
}

// update sets the values of the object id.
func (c *fakeCloud) update(id string, vals values) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.IndexFunc(c.objects, func(o fakeObject) bool { return o.ID == id })
	if i < 0 {
		return fmt.Errorf("object %s not found", id)
	}
	c.objects[i].Values = maps.Clone(vals)
	return nil
}

// remove deletes the object id. An object that is not there is already gone.
func (c *fakeCloud) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects = slices.DeleteFunc(c.objects, func(o fakeObject) bool { return o.ID == id })
}

// fakeSnapshot is a copy of the fake cloud's objects, with the cloud for tasks to change.
type fakeSnapshot struct {
	cloud   *fakeCloud
	objects []fakeObject
}

// Objects returns the objects in the cloud's order.
func (s *fakeSnapshot) Objects() []Object {
	objects := make([]Object, len(s.objects))
	for i, o := range s.objects {
		objects[i] = o.Object
	}
	return objects
}

// find returns the object with key k that is not a duplicate.
func (s *fakeSnapshot) find(k Key) (fakeObject, bool) {
	i := slices.IndexFunc(s.objects, func(o fakeObject) bool { return o.Key == k && !o.Duplicate })
	if i < 0 {
		return fakeObject{}, false
	}
	return s.objects[i], true
}

// fakeSnapshotOf returns env's snapshot, which must be a fake one.
func fakeSnapshotOf(env *Env) (*fakeSnapshot, error) {
	s, ok := env.Snapshot.(*fakeSnapshot)
	if !ok || s == nil {
		return nil, fmt.Errorf("the snapshot is a %T, not a fake one", env.Snapshot)
	}
	return s, nil
}

// cloudTask wants the fake object with its key to hold want and, for each field of refs, the output id of that task.
// A new zone replaces the object; any other change updates it in place. Plan sets the output id of an object it
// finds, and Apply that of an object it creates.
type cloudTask struct {
	key  Key
	deps []Key
	want values
	refs map[string]Key
}

// fakeKinds lists the kinds of the cloud tasks in deletion order: things, then the bases that things depend on.
var fakeKinds = []Kind{{Name: thingKind, Deleter: cloudTask{}}, {Name: baseKind, Deleter: cloudTask{}}}

func (t cloudTask) Key() Key    { return t.key }
func (t cloudTask) Deps() []Key { return t.deps }

func (t cloudTask) Plan(_ context.Context, env *Env) (Change, error) {
	snap, err := fakeSnapshotOf(env)
	if err != nil {
		return Change{}, err
	}
	snap.cloud.record("plan %s", t.key)
	want, _ := t.desired(env.Outputs)
	obj, ok := snap.find(t.key)
	if !ok {
		return Change{Action: Create, Diff: diffValues(nil, want)}, nil
	}
	env.Outputs.Set(t.key, "id", obj.ID)
	diff := diffValues(obj.Values, want)
	switch {
	case len(diff) == 0:
		return Change{Action: Noop}, nil
	case obj.Values["zone"] != want["zone"]:
		return Change{Action: Replace, Diff: diff, Reason: "zone changed"}, nil
	default:
		return Change{Action: Update, Diff: diff}, nil
	}
}

func (t cloudTask) Apply(ctx context.Context, env *Env, ch Change) error {
	snap, err := fakeSnapshotOf(env)
	if err != nil {
		return err
	}
	c := snap.cloud
	c.record("apply %s %s", t.key, ch.Action)
	end, err := c.begin(ctx, t.key.String())
	defer end()
	if err != nil {
		return err
	}
	want, known := t.desired(env.Outputs)
	if !known {
		return fmt.Errorf("apply %s: a referenced output is unknown", t.key)
	}
	old, found := snap.find(t.key)
	switch ch.Action {
	case Create:
		env.Outputs.Set(t.key, "id", c.create(t.key, want))
	case Update:
		if !found {
			return fmt.Errorf("apply %s: the object is not in the snapshot", t.key)
		}
		return c.update(old.ID, want)
	case Replace:
		if !found {
			return fmt.Errorf("apply %s: the object is not in the snapshot", t.key)
		}
		env.Outputs.Set(t.key, "id", c.create(t.key, want))
		c.remove(old.ID)
	default:
		return fmt.Errorf("apply %s: unexpected %s", t.key, ch.Action)
	}
	return nil
}

func (cloudTask) Delete(ctx context.Context, env *Env, obj Object) error {
	snap, err := fakeSnapshotOf(env)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s (ID %s)", obj.Key, obj.ID)
	snap.cloud.record("delete %s", target)
	end, err := snap.cloud.begin(ctx, target)
	defer end()
	if err != nil {
		return err
	}
	snap.cloud.remove(obj.ID)
	return nil
}

// desired returns what the task wants its object to hold, and whether every referenced output is known. An unknown
// output shows as KnownAfterApply.
func (t cloudTask) desired(o *Outputs) (vals values, known bool) {
	vals = maps.Clone(t.want)
	if vals == nil {
		vals = values{}
	}
	known = true
	for field, k := range t.refs {
		v, ok := o.Get(k, "id")
		if !ok {
			v, known = KnownAfterApply, false
		}
		vals[field] = v
	}
	return vals, known
}

// diffValues returns the fields that differ between have and want, by field name.
func diffValues(have, want values) []FieldDiff {
	fields := slices.Collect(maps.Keys(have))
	for f := range want {
		if _, ok := have[f]; !ok {
			fields = append(fields, f)
		}
	}
	slices.Sort(fields)
	var diff []FieldDiff
	for _, f := range fields {
		if have[f] != want[f] {
			diff = append(diff, FieldDiff{Field: f, Old: have[f], New: want[f]})
		}
	}
	return diff
}

// TestCloudTask checks that the cloud task's Apply and Delete bring the fake cloud to what its Plan wants.
func TestCloudTask(t *testing.T) {
	a := thing("a")
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		task    cloudTask
		outputs map[Key]string // output id of each key, set before each Plan
		action  Action
		want    []fakeObject // the cloud after Apply
	}{
		{
			name:   "create",
			task:   cloudTask{key: a, want: values{"size": "small"}},
			action: Create,
			want:   []fakeObject{object(a, "new-1", values{"size": "small"})},
		},
		{
			name:    "create with a reference",
			task:    cloudTask{key: a, deps: []Key{base("b")}, refs: map[string]Key{"base": base("b")}},
			outputs: map[Key]string{base("b"): "7"},
			action:  Create,
			want:    []fakeObject{object(a, "new-1", values{"base": "7"})},
		},
		{
			name:    "update",
			objects: []fakeObject{object(a, "1", values{"size": "small"})},
			task:    cloudTask{key: a, want: values{"size": "large"}},
			action:  Update,
			want:    []fakeObject{object(a, "1", values{"size": "large"})},
		},
		{
			name:    "replace",
			objects: []fakeObject{object(a, "1", values{"zone": "ams"})},
			task:    cloudTask{key: a, want: values{"zone": "fra"}},
			action:  Replace,
			want:    []fakeObject{object(a, "new-1", values{"zone": "fra"})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeCloud(tc.objects...)
			env := func() *Env {
				e := &Env{Snapshot: c.snapshot(), Outputs: &Outputs{}}
				for k, id := range tc.outputs {
					e.Outputs.Set(k, "id", id)
				}
				return e
			}
			first := env()
			ch, err := tc.task.Plan(t.Context(), first)
			if err != nil || ch.Action != tc.action {
				t.Fatalf("Plan = %v, %v, want %v, nil", ch.Action, err, tc.action)
			}
			if err := tc.task.Apply(t.Context(), first, ch); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if diff := cmp.Diff(tc.want, c.snapshot().objects, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("cloud after Apply (-want +got):\n%s", diff)
			}
			id, _ := first.Outputs.Get(a, "id")
			again := env()
			if ch, err := tc.task.Plan(t.Context(), again); err != nil || ch.Action != Noop {
				t.Errorf("Plan after Apply = %v, %v, want noop, nil", ch.Action, err)
			}
			if got, known := again.Outputs.Get(a, "id"); got != id || !known {
				t.Errorf("output id after Apply = %q, %v, want %q, true", got, known, id)
			}
		})
	}
}

func TestCloudTaskApplyNeedsKnownReferences(t *testing.T) {
	c := newFakeCloud()
	env := &Env{Snapshot: c.snapshot(), Outputs: &Outputs{}}
	tk := cloudTask{key: thing("a"), refs: map[string]Key{"base": base("b")}}
	ch, err := tk.Plan(t.Context(), env)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := []FieldDiff{{Field: "base", New: KnownAfterApply}}; !cmp.Equal(ch.Diff, want) {
		t.Errorf("Plan diff = %v, want %v", ch.Diff, want)
	}
	if err := tk.Apply(t.Context(), env, ch); err == nil {
		t.Error("Apply with an unknown reference succeeded")
	}
}

func TestCloudTaskDelete(t *testing.T) {
	c := newFakeCloud(object(thing("a"), "1", nil), duplicate(object(thing("a"), "2", nil)))
	env := &Env{Snapshot: c.snapshot(), Outputs: &Outputs{}}
	objects := env.Snapshot.Objects()
	if len(objects) != 2 {
		t.Fatalf("snapshot objects = %v, want 2", objects)
	}
	dup := objects[1]
	for range 2 {
		if err := (cloudTask{}).Delete(t.Context(), env, dup); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if diff := cmp.Diff([]Object{{Key: thing("a"), ID: "1"}}, c.snapshot().Objects()); diff != "" {
		t.Errorf("objects after Delete (-want +got):\n%s", diff)
	}
	want := []string{"delete test.Thing/a (ID 2)", "delete test.Thing/a (ID 2)"}
	if diff := cmp.Diff(want, c.calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}
