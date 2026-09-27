package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

var (
	errBoom = errors.New("boom")
	errBusy = errors.New("busy")
)

// anyOrder compares string slices as sets with repeats.
var anyOrder = cmpopts.SortSlices(func(a, b string) bool { return a < b })

// recorder collects the events of an Apply. It has no lock, so the race detector reports an Apply that calls
// OnEvent concurrently. Read its events only after Apply has returned.
type recorder struct{ events []Event }

func (r *recorder) add(e Event) { r.events = append(r.events, e) }

// text returns the events as describe writes them.
func (r *recorder) text() []string {
	var lines []string
	for _, e := range r.events {
		lines = append(lines, describe(e))
	}
	return lines
}

// describe returns e as text: its type, key and action, then its ID, error, wait and cause when they are set, such as
// "retrying test.Thing/a create: busy (wait 2s)" or "skipped test.Thing/b create: test.Thing/a failed".
func describe(e Event) string {
	s := fmt.Sprintf("%s %s %s", e.Type, e.Key, e.Action)
	if e.ID != "" {
		s += " (ID " + e.ID + ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	if e.Wait != 0 {
		s += fmt.Sprintf(" (wait %s)", e.Wait)
	}
	if e.Cause != "" {
		s += ": " + e.Cause
	}
	return s
}

// changeCalls returns the calls that tasks made to change the cloud, in order: applies and deletes, not plans.
func changeCalls(c *fakeCloud) []string {
	return slices.DeleteFunc(c.calls(), func(call string) bool { return strings.HasPrefix(call, "plan ") })
}

// planFor plans tasks against the fake cloud.
func planFor(t *testing.T, c *fakeCloud, tasks ...Task) *Plan {
	t.Helper()
	p, err := NewPlan(t.Context(), tasks, fakeKinds, c.snapshot())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// applyAsync applies p in the background; its result arrives on the channel.
func applyAsync(ctx context.Context, p *Plan, opts ApplyOptions) <-chan error {
	done := make(chan error, 1)
	go func() { done <- p.Apply(ctx, opts) }()
	return done
}

// wantErr checks that err has the message want and wraps each of is.
func wantErr(t *testing.T, err error, want string, is ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Apply succeeded, want error %q", want)
	}
	if err.Error() != want {
		t.Errorf("Apply error = %q, want %q", err, want)
	}
	for _, target := range is {
		if !errors.Is(err, target) {
			t.Errorf("Apply error %q does not wrap %q", err, target)
		}
	}
}

func TestEventTypeString(t *testing.T) {
	for _, tc := range []struct {
		typ  EventType
		want string
	}{
		{Started, "started"},
		{Succeeded, "succeeded"},
		{Failed, "failed"},
		{Retrying, "retrying"},
		{Skipped, "skipped"},
		{EventType(9), "EventType(9)"},
		{EventType(-1), "EventType(-1)"},
	} {
		if got := tc.typ.String(); got != tc.want {
			t.Errorf("EventType(%d).String() = %q, want %q", int(tc.typ), got, tc.want)
		}
		if text, err := tc.typ.MarshalText(); err != nil || string(text) != tc.want {
			t.Errorf("EventType(%d).MarshalText() = %q, %v, want %q, nil", int(tc.typ), text, err, tc.want)
		}
	}
}

func TestApplyEmptyPlan(t *testing.T) {
	cloud := newFakeCloud()
	var rec recorder
	if err := planFor(t, cloud).Apply(t.Context(), ApplyOptions{OnEvent: rec.add}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(rec.events) > 0 {
		t.Errorf("events = %q, want none", rec.text())
	}
}

// TestApplyEvents checks the events and calls of small plans. One change runs at a time, so their order is fixed.
func TestApplyEvents(t *testing.T) {
	a, b, c := thing("a"), thing("b"), thing("c")
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
		faults  map[string]fault
		events  []string
		calls   []string
		err     string // empty for none
	}{
		{
			name: "changes in order, a retry and prunes kind by kind",
			objects: []fakeObject{
				object(a, "1", values{"size": "small"}),
				duplicate(object(a, "9", nil)),
				object(base("q"), "5", nil),
				object(thing("z"), "7", nil),
			},
			// test.Thing/d is ready from the start, but b and c come before it in the order.
			tasks: []Task{
				cloudTask{key: c, deps: []Key{b}},
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
				cloudTask{key: a, want: values{"size": "large"}},
				cloudTask{key: thing("d")},
			},
			faults: map[string]fault{"test.Thing/b": failFirst(1, Retryable(errBusy, 2*time.Second))},
			events: []string{
				"started test.Thing/a update",
				"succeeded test.Thing/a update",
				"started test.Thing/b create",
				"retrying test.Thing/b create: busy (wait 2s)",
				"succeeded test.Thing/b create",
				"started test.Thing/c create",
				"succeeded test.Thing/c create",
				"started test.Thing/d create",
				"succeeded test.Thing/d create",
				"started test.Thing/a delete (ID 9)",
				"succeeded test.Thing/a delete (ID 9)",
				"started test.Thing/z delete (ID 7)",
				"succeeded test.Thing/z delete (ID 7)",
				"started test.Base/q delete (ID 5)",
				"succeeded test.Base/q delete (ID 5)",
			},
			calls: []string{
				"apply test.Thing/a update",
				"apply test.Thing/b create",
				"apply test.Thing/b create",
				"apply test.Thing/c create",
				"apply test.Thing/d create",
				"delete test.Thing/a (ID 9)",
				"delete test.Thing/z (ID 7)",
				"delete test.Base/q (ID 5)",
			},
		},
		{
			name:    "a failure",
			objects: []fakeObject{object(base("q"), "5", nil)},
			tasks:   []Task{cloudTask{key: b, deps: []Key{a}}, cloudTask{key: a}, cloudTask{key: c}},
			faults:  map[string]fault{"test.Thing/a": failFirst(1, errBoom)},
			events: []string{
				"started test.Thing/a create",
				"failed test.Thing/a create: boom",
				"skipped test.Thing/b create: test.Thing/a failed",
				"started test.Thing/c create",
				"succeeded test.Thing/c create",
				"skipped test.Base/q delete (ID 5): an earlier change failed",
			},
			calls: []string{"apply test.Thing/a create", "apply test.Thing/c create"},
			err:   "test.Thing/a: boom",
		},
		{
			name: "noop tasks send no events",
			objects: []fakeObject{
				object(a, "1", nil),
				object(c, "3", values{"parent": "1"}),
			},
			tasks: []Task{
				cloudTask{key: a},
				cloudTask{key: b, deps: []Key{a}},
				cloudTask{key: c, deps: []Key{a}, refs: map[string]Key{"parent": a}},
			},
			events: []string{"started test.Thing/b create", "succeeded test.Thing/b create"},
			calls:  []string{"apply test.Thing/b create"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud(tc.objects...)
				for target, f := range tc.faults {
					cloud.setFault(target, f)
				}
				p := planFor(t, cloud, tc.tasks...)
				var rec recorder
				err := p.Apply(t.Context(), ApplyOptions{Parallelism: 1, OnEvent: rec.add})
				if tc.err == "" && err != nil {
					t.Errorf("Apply: %v", err)
				} else if tc.err != "" {
					wantErr(t, err, tc.err)
				}
				if diff := cmp.Diff(tc.events, rec.text()); diff != "" {
					t.Errorf("events (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(tc.calls, changeCalls(cloud)); diff != "" {
					t.Errorf("calls (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestApplyParallelism(t *testing.T) {
	for _, tc := range []struct {
		parallelism int
		most        int           // calls at once
		took        time.Duration // six changes that take a second each
	}{
		{parallelism: 1, most: 1, took: 6 * time.Second},
		{parallelism: 2, most: 2, took: 3 * time.Second},
		{parallelism: 4, most: 4, took: 2 * time.Second},
		{parallelism: 0, most: 4, took: 2 * time.Second},
		{parallelism: -1, most: 4, took: 2 * time.Second},
		{parallelism: 10, most: 6, took: time.Second},
	} {
		t.Run(fmt.Sprint(tc.parallelism), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud()
				cloud.delay = time.Second
				var tasks []Task
				for _, k := range things("a", "b", "c", "d", "e", "f") {
					tasks = append(tasks, cloudTask{key: k})
				}
				p := planFor(t, cloud, tasks...)
				start := time.Now()
				if err := p.Apply(t.Context(), ApplyOptions{Parallelism: tc.parallelism}); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				if took := time.Since(start); took != tc.took {
					t.Errorf("Apply took %v, want %v", took, tc.took)
				}
				if busy, most := cloud.load(); busy != 0 || most != tc.most {
					t.Errorf("calls in progress = %d, at most %d; want 0, at most %d", busy, most, tc.most)
				}
			})
		})
	}
}

// TestApplyWaitsForDependencies holds a change and checks that only the changes that do not depend on it run.
func TestApplyWaitsForDependencies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, c, d, n := thing("a"), thing("b"), thing("c"), thing("d"), thing("n")
		cloud := newFakeCloud(object(n, "1", nil))
		release := cloud.hold(a.String())
		p := planFor(t, cloud,
			cloudTask{key: a},
			cloudTask{key: n},
			cloudTask{key: b, deps: []Key{a, n, d}},
			cloudTask{key: c, deps: []Key{b}},
			cloudTask{key: d, deps: []Key{n}}, // n does not change, so d starts at once
		)
		done := applyAsync(t.Context(), p, ApplyOptions{})
		synctest.Wait()
		first := []string{"apply test.Thing/a create", "apply test.Thing/d create"}
		if diff := cmp.Diff(first, changeCalls(cloud), anyOrder); diff != "" {
			t.Errorf("calls while test.Thing/a runs (-want +got):\n%s", diff)
		}
		release()
		if err := <-done; err != nil {
			t.Fatalf("Apply: %v", err)
		}
		calls := changeCalls(cloud)
		if len(calls) != 4 {
			t.Fatalf("calls = %q, want 4", calls)
		}
		if diff := cmp.Diff(first, calls[:2], anyOrder); diff != "" {
			t.Errorf("first calls (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"apply test.Thing/b create", "apply test.Thing/c create"}, calls[2:]); diff != "" {
			t.Errorf("later calls (-want +got):\n%s", diff)
		}
	})
}

func TestApplySkipsTheDependentsOfAFailure(t *testing.T) {
	a, b, c, d, e, x := thing("a"), thing("b"), thing("c"), thing("d"), thing("e"), thing("x")
	cloud := newFakeCloud(object(thing("z"), "1", nil), object(base("q"), "2", nil))
	cloud.setFault(a.String(), failFirst(1, errBoom))
	p := planFor(t, cloud,
		cloudTask{key: a},
		cloudTask{key: b, deps: []Key{a}},
		cloudTask{key: c, deps: []Key{b}},
		cloudTask{key: d},
		cloudTask{key: e, deps: []Key{d}},
		cloudTask{key: x, deps: []Key{d, a}},
	)
	var rec recorder
	err := p.Apply(t.Context(), ApplyOptions{OnEvent: rec.add})
	wantErr(t, err, "test.Thing/a: boom", errBoom)
	events := []string{
		"started test.Thing/a create",
		"failed test.Thing/a create: boom",
		"skipped test.Thing/b create: test.Thing/a failed",
		"skipped test.Thing/c create: test.Thing/a failed",
		"skipped test.Thing/x create: test.Thing/a failed",
		"started test.Thing/d create",
		"succeeded test.Thing/d create",
		"started test.Thing/e create",
		"succeeded test.Thing/e create",
		"skipped test.Thing/z delete (ID 1): an earlier change failed",
		"skipped test.Base/q delete (ID 2): an earlier change failed",
	}
	if diff := cmp.Diff(events, rec.text(), anyOrder); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	calls := []string{"apply test.Thing/a create", "apply test.Thing/d create", "apply test.Thing/e create"}
	if diff := cmp.Diff(calls, changeCalls(cloud), anyOrder); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// TestApplySkippingPassesThroughNoopTasks checks that a change that depends on a failed one through a task without
// changes is skipped.
func TestApplySkippingPassesThroughNoopTasks(t *testing.T) {
	a, n, c := thing("a"), thing("n"), thing("c")
	cloud := newFakeCloud(object(a, "1", values{"size": "small"}), object(n, "2", values{"parent": "1"}))
	cloud.setFault(a.String(), failFirst(1, errBoom))
	p := planFor(t, cloud,
		cloudTask{key: a, want: values{"size": "large"}},
		cloudTask{key: n, deps: []Key{a}, refs: map[string]Key{"parent": a}},
		cloudTask{key: c, deps: []Key{n}},
	)
	var rec recorder
	wantErr(t, p.Apply(t.Context(), ApplyOptions{Parallelism: 1, OnEvent: rec.add}), "test.Thing/a: boom")
	events := []string{
		"started test.Thing/a update",
		"failed test.Thing/a update: boom",
		"skipped test.Thing/c create: test.Thing/a failed",
	}
	if diff := cmp.Diff(events, rec.text()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

// TestApplyWaitsThroughNoopTasks holds a change and checks that a change that depends on it through tasks without
// changes waits for it.
func TestApplyWaitsThroughNoopTasks(t *testing.T) {
	a, n, m, c := thing("a"), thing("n"), thing("m"), thing("c")
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
	}{
		{
			name:    "one task without changes",
			objects: []fakeObject{object(n, "1", nil)},
			tasks:   []Task{cloudTask{key: a}, cloudTask{key: n, deps: []Key{a}}, cloudTask{key: c, deps: []Key{n}}},
		},
		{
			// c waits for a once, although two paths lead to it.
			name:    "two tasks without changes",
			objects: []fakeObject{object(n, "1", nil), object(m, "2", nil)},
			tasks: []Task{
				cloudTask{key: a},
				cloudTask{key: n, deps: []Key{a}},
				cloudTask{key: m, deps: []Key{a, n}},
				cloudTask{key: c, deps: []Key{n, m}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud(tc.objects...)
				release := cloud.hold(a.String())
				done := applyAsync(t.Context(), planFor(t, cloud, tc.tasks...), ApplyOptions{})
				synctest.Wait()
				if diff := cmp.Diff([]string{"apply test.Thing/a create"}, changeCalls(cloud)); diff != "" {
					t.Errorf("calls while test.Thing/a runs (-want +got):\n%s", diff)
				}
				release()
				if err := <-done; err != nil {
					t.Fatalf("Apply: %v", err)
				}
				want := []string{"apply test.Thing/a create", "apply test.Thing/c create"}
				if diff := cmp.Diff(want, changeCalls(cloud)); diff != "" {
					t.Errorf("calls (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestApplyJoinsErrorsInChangeOrder has the later of two changes fail first.
func TestApplyJoinsErrorsInChangeOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		objects       []fakeObject
		tasks         []Task
		first, second string // targets in change order
	}{
		{
			name:   "task changes",
			tasks:  []Task{cloudTask{key: thing("a")}, cloudTask{key: thing("b")}},
			first:  "test.Thing/a",
			second: "test.Thing/b",
		},
		{
			name:    "deletes",
			objects: []fakeObject{duplicate(object(thing("x"), "2", nil)), object(thing("x"), "1", nil)},
			first:   "test.Thing/x (ID 1)",
			second:  "test.Thing/x (ID 2)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud(tc.objects...)
				for _, target := range []string{tc.first, tc.second} {
					cloud.setFault(target, failFirst(1, fmt.Errorf("boom at %s", target)))
				}
				release := cloud.hold(tc.first)
				var rec recorder
				done := applyAsync(t.Context(), planFor(t, cloud, tc.tasks...), ApplyOptions{OnEvent: rec.add})
				synctest.Wait()
				release()
				err := <-done
				wantErr(t, err, fmt.Sprintf("%[1]s: boom at %[1]s\n%[2]s: boom at %[2]s", tc.first, tc.second))
				var failed []string
				for _, e := range rec.events {
					if e.Type == Failed {
						failed = append(failed, e.Err.Error())
					}
				}
				want := []string{"boom at " + tc.second, "boom at " + tc.first}
				if diff := cmp.Diff(want, failed); diff != "" {
					t.Errorf("failures in the order they happened (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestApplyPrunesAfterTheTasksKindByKind holds a task change, then a delete of the first kind, and checks what runs
// meanwhile.
func TestApplyPrunesAfterTheTasksKindByKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cloud := newFakeCloud(object(base("q"), "1", nil), object(thing("y"), "2", nil), object(thing("z"), "3", nil))
		releaseTask := cloud.hold("test.Thing/a")
		releaseDelete := cloud.hold("test.Thing/y (ID 2)")
		done := applyAsync(t.Context(), planFor(t, cloud, cloudTask{key: thing("a")}), ApplyOptions{})
		synctest.Wait()
		calls := []string{"apply test.Thing/a create"}
		if diff := cmp.Diff(calls, changeCalls(cloud)); diff != "" {
			t.Errorf("calls while the task runs (-want +got):\n%s", diff)
		}
		releaseTask()
		synctest.Wait()
		calls = append(calls, "delete test.Thing/y (ID 2)", "delete test.Thing/z (ID 3)")
		if diff := cmp.Diff(calls, changeCalls(cloud), anyOrder); diff != "" {
			t.Errorf("calls while a delete of the first kind runs (-want +got):\n%s", diff)
		}
		releaseDelete()
		if err := <-done; err != nil {
			t.Fatalf("Apply: %v", err)
		}
		got := changeCalls(cloud)
		if len(got) != len(calls)+1 {
			t.Fatalf("calls = %q, want %d", got, len(calls)+1)
		}
		if diff := cmp.Diff("delete test.Base/q (ID 1)", got[len(calls)]); diff != "" {
			t.Errorf("last call (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]Object{{Key: thing("a"), ID: "new-1"}}, cloud.snapshot().Objects()); diff != "" {
			t.Errorf("objects (-want +got):\n%s", diff)
		}
	})
}

// TestApplyAFailedDeleteSkipsTheLaterKinds checks that the deletes of its own kind still run.
func TestApplyAFailedDeleteSkipsTheLaterKinds(t *testing.T) {
	cloud := newFakeCloud(object(thing("x"), "1", nil), object(thing("y"), "2", nil), object(base("q"), "3", nil))
	cloud.setFault("test.Thing/x (ID 1)", failFirst(1, errBoom))
	var rec recorder
	err := planFor(t, cloud).Apply(t.Context(), ApplyOptions{Parallelism: 1, OnEvent: rec.add})
	wantErr(t, err, "test.Thing/x (ID 1): boom", errBoom)
	events := []string{
		"started test.Thing/x delete (ID 1)",
		"failed test.Thing/x delete (ID 1): boom",
		"started test.Thing/y delete (ID 2)",
		"succeeded test.Thing/y delete (ID 2)",
		"skipped test.Base/q delete (ID 3): an earlier change failed",
	}
	if diff := cmp.Diff(events, rec.text()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	calls := []string{"delete test.Thing/x (ID 1)", "delete test.Thing/y (ID 2)"}
	if diff := cmp.Diff(calls, changeCalls(cloud)); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	objects := []Object{{Key: thing("x"), ID: "1"}, {Key: base("q"), ID: "3"}}
	if diff := cmp.Diff(objects, cloud.snapshot().Objects()); diff != "" {
		t.Errorf("objects (-want +got):\n%s", diff)
	}
}

// objectLog is a deleter that records the objects it gets.
type objectLog struct {
	mu      sync.Mutex
	objects []Object
}

func (l *objectLog) Delete(_ context.Context, _ *Env, obj Object) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.objects = append(l.objects, obj)
	return nil
}

func TestApplyDeletesWithTheKindsDeleter(t *testing.T) {
	thingLog, baseLog := &objectLog{}, &objectLog{}
	kinds := []Kind{{Name: thingKind, Deleter: thingLog}, {Name: baseKind, Deleter: baseLog}}
	snap := newFakeCloud(
		duplicate(object(thing("a"), "1", nil)),
		object(thing("a"), "2", nil),
		object(base("b"), "3", nil),
	).snapshot()
	p, err := NewPlan(t.Context(), []Task{fakeTask{key: thing("a")}}, kinds, snap)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := p.Apply(t.Context(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if diff := cmp.Diff([]Object{{Key: thing("a"), ID: "1", Duplicate: true}}, thingLog.objects); diff != "" {
		t.Errorf("deleted things (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]Object{{Key: base("b"), ID: "3"}}, baseLog.objects); diff != "" {
		t.Errorf("deleted bases (-want +got):\n%s", diff)
	}
}

// between is a range of waits, both ends included.
type between struct{ min, max time.Duration }

func TestApplyRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		objects  []fakeObject
		target   string
		fault    fault
		attempts int
		waits    []between
	}{
		{
			name:     "after the given wait",
			target:   "test.Thing/a",
			fault:    failFirst(1, Retryable(errBusy, 10*time.Second)),
			attempts: 2,
			waits:    []between{{10 * time.Second, 10 * time.Second}},
		},
		{
			name:     "after a backoff",
			target:   "test.Thing/a",
			fault:    failFirst(3, Retryable(errBusy, 0)),
			attempts: 4,
			waits: []between{
				{500 * time.Millisecond, time.Second},
				{time.Second, 2 * time.Second},
				{2 * time.Second, 4 * time.Second},
			},
		},
		{
			name:     "a delete",
			objects:  []fakeObject{object(thing("z"), "1", nil)},
			target:   "test.Thing/z (ID 1)",
			fault:    failFirst(2, fmt.Errorf("delete: %w", Retryable(errBusy, 0))),
			attempts: 3,
			waits:    []between{{500 * time.Millisecond, time.Second}, {time.Second, 2 * time.Second}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud(tc.objects...)
				cloud.setFault(tc.target, tc.fault)
				var rec recorder
				start := time.Now()
				err := planFor(t, cloud, cloudTask{key: thing("a")}).Apply(t.Context(), ApplyOptions{OnEvent: rec.add})
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				if got := cloud.attemptsOf(tc.target); got != tc.attempts {
					t.Errorf("attempts = %d, want %d", got, tc.attempts)
				}
				var waited time.Duration
				var retries int
				for _, e := range rec.events {
					if e.Type != Retrying {
						continue
					}
					if retries < len(tc.waits) {
						if w := tc.waits[retries]; e.Wait < w.min || e.Wait > w.max {
							t.Errorf("retry %d waits %v, want between %v and %v", retries, e.Wait, w.min, w.max)
						}
					}
					if !errors.Is(e.Err, errBusy) {
						t.Errorf("retry %d error = %v, want busy", retries, e.Err)
					}
					waited += e.Wait
					retries++
				}
				if retries != len(tc.waits) {
					t.Errorf("retries = %d, want %d: %q", retries, len(tc.waits), rec.text())
				}
				if took := time.Since(start); took != waited {
					t.Errorf("Apply took %v, want the waits, %v", took, waited)
				}
			})
		})
	}
}

func TestApplyGivesUpAtTheDeadline(t *testing.T) {
	busy := func(n int) error { return Retryable(fmt.Errorf("busy %d", n), 4*time.Second) }
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		fault    fault
		hold     bool // the attempt waits until its context ends
		attempts int
		took     time.Duration
		err      string
		is       error
	}{
		{
			name:     "the next wait would pass the deadline",
			timeout:  10 * time.Second,
			fault:    busy,
			attempts: 3,
			took:     8 * time.Second,
			err:      "test.Thing/a: busy 3",
		},
		{
			name:     "an attempt runs past the deadline",
			timeout:  time.Minute,
			hold:     true,
			attempts: 1,
			took:     time.Minute,
			err:      "test.Thing/a: context deadline exceeded",
			is:       context.DeadlineExceeded,
		},
		{
			name:     "the default deadline",
			hold:     true,
			attempts: 1,
			took:     5 * time.Minute,
			err:      "test.Thing/a: context deadline exceeded",
			is:       context.DeadlineExceeded,
		},
		{
			name:     "a negative timeout means the default",
			timeout:  -time.Second,
			hold:     true,
			attempts: 1,
			took:     5 * time.Minute,
			err:      "test.Thing/a: context deadline exceeded",
			is:       context.DeadlineExceeded,
		},
		{
			name:     "an error that is not retryable fails at once",
			timeout:  10 * time.Second,
			fault:    failFirst(1, errBoom),
			attempts: 1,
			err:      "test.Thing/a: boom",
			is:       errBoom,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud()
				if tc.fault != nil {
					cloud.setFault("test.Thing/a", tc.fault)
				}
				if tc.hold {
					defer cloud.hold("test.Thing/a")()
				}
				var rec recorder
				start := time.Now()
				p := planFor(t, cloud, cloudTask{key: thing("a")})
				err := p.Apply(t.Context(), ApplyOptions{ChangeTimeout: tc.timeout, OnEvent: rec.add})
				wantErr(t, err, tc.err)
				if tc.is != nil && !errors.Is(err, tc.is) {
					t.Errorf("Apply error %q does not wrap %q", err, tc.is)
				}
				if took := time.Since(start); took != tc.took {
					t.Errorf("Apply took %v, want %v", took, tc.took)
				}
				if got := cloud.attemptsOf("test.Thing/a"); got != tc.attempts {
					t.Errorf("attempts = %d, want %d", got, tc.attempts)
				}
				if len(rec.events) == 0 {
					t.Fatal("no events, want the failure last")
				}
				if last := rec.events[len(rec.events)-1]; last.Type != Failed || !errors.Is(err, last.Err) {
					t.Errorf("last event = %q, want the failure", describe(last))
				}
			})
		})
	}
}

// TestApplyBackoffStopsBeforeTheDeadline checks that a change gives up rather than wait past its deadline, which
// would take it to the deadline itself.
func TestApplyBackoffStopsBeforeTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cloud := newFakeCloud()
		cloud.setFault("test.Thing/a", func(n int) error { return Retryable(fmt.Errorf("busy %d", n), 0) })
		var rec recorder
		start := time.Now()
		p := planFor(t, cloud, cloudTask{key: thing("a")})
		err := p.Apply(t.Context(), ApplyOptions{ChangeTimeout: 20 * time.Second, OnEvent: rec.add})
		attempts := cloud.attemptsOf("test.Thing/a")
		wantErr(t, err, fmt.Sprintf("test.Thing/a: busy %d", attempts))
		if took := time.Since(start); took >= 20*time.Second {
			t.Errorf("Apply took %v, want less than the deadline", took)
		}
		var retries int
		for _, e := range rec.events {
			if e.Type == Retrying {
				if b := backoff(retries, highest); e.Wait < b/2 || e.Wait > b {
					t.Errorf("retry %d waits %v, want between %v and %v", retries, e.Wait, b/2, b)
				}
				retries++
			}
		}
		// The waits before the fifth attempt add up to at most 1+2+4+8 = 15 s.
		if retries != attempts-1 || attempts < 5 {
			t.Errorf("%d attempts and %d retries, want at least 5 attempts and one retry fewer", attempts, retries)
		}
	})
}

func TestApplyCancelled(t *testing.T) {
	a, b, c, x, y := thing("a"), thing("b"), thing("c"), thing("x"), thing("y")
	for _, tc := range []struct {
		name        string
		objects     []fakeObject
		tasks       []Task
		hold        []string // the targets to hold; cancel once they wait
		fault       fault    // of test.Thing/a
		early       bool     // cancel before Apply
		parallelism int      // 1 when zero; above 1 the events are compared in any order
		events      []string
		err         string
	}{
		{
			name:    "before Apply",
			objects: []fakeObject{object(base("q"), "1", nil)},
			tasks:   []Task{cloudTask{key: a}, cloudTask{key: b, deps: []Key{a}}},
			early:   true,
			events: []string{
				"skipped test.Thing/a create: cancelled",
				"skipped test.Thing/b create: cancelled",
				"skipped test.Base/q delete (ID 1): cancelled",
			},
			err: "context canceled",
		},
		{
			name:    "while a change runs",
			objects: []fakeObject{object(base("q"), "1", nil)},
			tasks:   []Task{cloudTask{key: a}, cloudTask{key: b, deps: []Key{a}}, cloudTask{key: c}},
			hold:    []string{"test.Thing/a"},
			events: []string{
				"started test.Thing/a create",
				"failed test.Thing/a create: context canceled",
				"skipped test.Thing/b create: test.Thing/a failed",
				"skipped test.Thing/c create: cancelled",
				"skipped test.Base/q delete (ID 1): an earlier change failed",
			},
			err: "test.Thing/a: context canceled\ncontext canceled",
		},
		{
			name: "while two changes run, each failure skips its own dependents",
			tasks: []Task{
				cloudTask{key: a}, cloudTask{key: b, deps: []Key{a}},
				cloudTask{key: x}, cloudTask{key: y, deps: []Key{x}},
			},
			hold:        []string{"test.Thing/a", "test.Thing/x"},
			parallelism: 2,
			events: []string{
				"started test.Thing/a create",
				"started test.Thing/x create",
				"failed test.Thing/a create: context canceled",
				"failed test.Thing/x create: context canceled",
				"skipped test.Thing/b create: test.Thing/a failed",
				"skipped test.Thing/y create: test.Thing/x failed",
			},
			err: "test.Thing/a: context canceled\ntest.Thing/x: context canceled\ncontext canceled",
		},
		{
			name:  "while a change waits to retry",
			tasks: []Task{cloudTask{key: a}, cloudTask{key: c}},
			fault: failFirst(1, Retryable(errBusy, time.Minute)),
			events: []string{
				"started test.Thing/a create",
				"retrying test.Thing/a create: busy (wait 1m0s)",
				"failed test.Thing/a create: busy",
				"skipped test.Thing/c create: cancelled",
			},
			err: "test.Thing/a: busy\ncontext canceled",
		},
		{
			name: "while a delete runs",
			objects: []fakeObject{
				object(thing("z"), "1", nil),
				duplicate(object(thing("z"), "2", nil)),
				object(base("q"), "3", nil),
			},
			hold: []string{"test.Thing/z (ID 1)"},
			events: []string{
				"started test.Thing/z delete (ID 1)",
				"failed test.Thing/z delete (ID 1): context canceled",
				"skipped test.Thing/z delete (ID 2): cancelled",
				"skipped test.Base/q delete (ID 3): an earlier change failed",
			},
			err: "test.Thing/z (ID 1): context canceled\ncontext canceled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cloud := newFakeCloud(tc.objects...)
				for _, target := range tc.hold {
					cloud.hold(target)
				}
				if tc.fault != nil {
					cloud.setFault("test.Thing/a", tc.fault)
				}
				p := planFor(t, cloud, tc.tasks...)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.early {
					cancel()
				}
				opts := ApplyOptions{Parallelism: max(tc.parallelism, 1)}
				var rec recorder
				opts.OnEvent = rec.add
				start := time.Now()
				done := applyAsync(ctx, p, opts)
				synctest.Wait()
				cancel()
				wantErr(t, <-done, tc.err, context.Canceled)
				var order []cmp.Option
				if opts.Parallelism > 1 {
					order = append(order, anyOrder)
				}
				if diff := cmp.Diff(tc.events, rec.text(), order...); diff != "" {
					t.Errorf("events (-want +got):\n%s", diff)
				}
				if took := time.Since(start); took != 0 {
					t.Errorf("Apply took %v, want no time", took)
				}
			})
		})
	}
}

// TestApplyCancelledAfterEveryChangeSucceeded cancels on the last change's success: that is no error.
func TestApplyCancelledAfterEveryChangeSucceeded(t *testing.T) {
	cloud := newFakeCloud(object(thing("z"), "1", nil))
	p := planFor(t, cloud, cloudTask{key: thing("a")})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var rec recorder
	onEvent := func(e Event) {
		rec.add(e)
		if e.Type == Succeeded && e.Action == Delete {
			cancel()
		}
	}
	if err := p.Apply(ctx, ApplyOptions{OnEvent: onEvent}); err != nil {
		t.Errorf("Apply: %v", err)
	}
	if ctx.Err() == nil {
		t.Error("the context was not cancelled")
	}
	events := []string{
		"started test.Thing/a create",
		"succeeded test.Thing/a create",
		"started test.Thing/z delete (ID 1)",
		"succeeded test.Thing/z delete (ID 1)",
	}
	if diff := cmp.Diff(events, rec.text()); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

func TestApplyOnce(t *testing.T) {
	cloud := newFakeCloud()
	p := planFor(t, cloud, cloudTask{key: thing("a")})
	if err := p.Apply(t.Context(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var rec recorder
	wantErr(t, p.Apply(t.Context(), ApplyOptions{OnEvent: rec.add}), "the plan was applied already")
	if len(rec.events) > 0 {
		t.Errorf("events of the second Apply = %q, want none", rec.text())
	}
	if diff := cmp.Diff([]string{"apply test.Thing/a create"}, changeCalls(cloud)); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

func TestApplyOnceConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cloud := newFakeCloud()
		p := planFor(t, cloud, cloudTask{key: thing("a")})
		first, second := applyAsync(t.Context(), p, ApplyOptions{}), applyAsync(t.Context(), p, ApplyOptions{})
		errs := []error{<-first, <-second}
		if errs[0] != nil {
			errs[0], errs[1] = errs[1], errs[0]
		}
		if errs[0] != nil || errs[1] == nil || errs[1].Error() != "the plan was applied already" {
			t.Errorf("Apply errors = %v, want one nil and one %q", errs, "the plan was applied already")
		}
		if diff := cmp.Diff([]string{"apply test.Thing/a create"}, changeCalls(cloud)); diff != "" {
			t.Errorf("calls (-want +got):\n%s", diff)
		}
	})
}

func TestApplyPassesOutputsToDependents(t *testing.T) {
	a, b := thing("a"), thing("b")
	cloud := newFakeCloud()
	p := planFor(t, cloud, cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}}, cloudTask{key: a})
	if err := p.Apply(t.Context(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []fakeObject{object(a, "new-1", values{}), object(b, "new-2", values{"parent": "new-1"})}
	if diff := cmp.Diff(want, cloud.snapshot().objects); diff != "" {
		t.Errorf("objects (-want +got):\n%s", diff)
	}
}

// TestApplyConverges checks that planning again after Apply finds nothing to change.
func TestApplyConverges(t *testing.T) {
	a, b, d, f, g := thing("a"), thing("b"), thing("d"), thing("f"), base("g")
	cloud := newFakeCloud(
		object(a, "1", values{"size": "small"}),
		object(b, "2", values{"zone": "ams"}),
		object(thing("c"), "3", nil),
		object(d, "4", nil),
		duplicate(object(d, "5", nil)),
		object(base("e"), "6", nil),
	)
	tasks := []Task{
		cloudTask{key: g},
		cloudTask{key: a, deps: []Key{g}, want: values{"size": "large"}, refs: map[string]Key{"base": g}},
		cloudTask{key: b, want: values{"zone": "fra"}},
		cloudTask{key: d},
		cloudTask{key: f, deps: []Key{b, d}, refs: map[string]Key{"parent": b, "sibling": d}},
	}
	if err := planFor(t, cloud, tasks...).Apply(t.Context(), ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changes := planFor(t, cloud, tasks...).Changes(); len(changes) > 0 {
		t.Errorf("changes after Apply = %v, want none", changes)
	}
}
