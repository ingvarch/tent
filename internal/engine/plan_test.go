package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// planned is the change a of the task k with diff.
func planned(k Key, a Action, diff ...FieldDiff) PlannedChange {
	return PlannedChange{Key: k, Change: Change{Action: a, Diff: diff}}
}

// replaced is the change of the cloud task k that replaces its object for a new zone.
func replaced(k Key, diff ...FieldDiff) PlannedChange {
	pc := planned(k, Replace, diff...)
	pc.Reason = "zone changed"
	return pc
}

// deleted is the delete of the object k with the given ID.
func deleted(k Key, id, reason string) PlannedChange {
	return PlannedChange{Key: k, ID: id, Change: Change{Action: Delete, Reason: reason}}
}

// added is the diff of a field that gets a value.
func added(field, value string) FieldDiff { return FieldDiff{Field: field, New: value} }

// changed is the diff of a field whose value changes.
func changed(field, from, to string) FieldDiff { return FieldDiff{Field: field, Old: from, New: to} }

func TestNewPlan(t *testing.T) {
	a, b, c := thing("a"), thing("b"), thing("c")
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
		want    []PlannedChange
		calls   []string
	}{
		{name: "no tasks and no objects"},
		{
			name:    "unchanged object",
			objects: []fakeObject{object(a, "1", values{"size": "small"})},
			tasks:   []Task{cloudTask{key: a, want: values{"size": "small"}}},
			calls:   []string{"plan test.Thing/a"},
		},
		{
			name:  "create",
			tasks: []Task{cloudTask{key: a, want: values{"size": "small", "zone": "ams"}}},
			want:  []PlannedChange{planned(a, Create, added("size", "small"), added("zone", "ams"))},
			calls: []string{"plan test.Thing/a"},
		},
		{
			name:    "update",
			objects: []fakeObject{object(a, "1", values{"size": "small", "zone": "ams"})},
			tasks:   []Task{cloudTask{key: a, want: values{"size": "large", "zone": "ams"}}},
			want:    []PlannedChange{planned(a, Update, changed("size", "small", "large"))},
			calls:   []string{"plan test.Thing/a"},
		},
		{
			name:    "replace",
			objects: []fakeObject{object(a, "1", values{"size": "small", "zone": "ams"})},
			tasks:   []Task{cloudTask{key: a, want: values{"size": "small", "zone": "fra"}}},
			want:    []PlannedChange{replaced(a, changed("zone", "ams", "fra"))},
			calls:   []string{"plan test.Thing/a"},
		},
		{
			name: "tasks plan after their dependencies",
			tasks: []Task{
				cloudTask{key: c, deps: []Key{b}, want: values{"size": "small"}},
				cloudTask{key: b, deps: []Key{a}, want: values{"size": "small"}},
				cloudTask{key: a, want: values{"size": "small"}},
			},
			want: []PlannedChange{
				planned(a, Create, added("size", "small")),
				planned(b, Create, added("size", "small")),
				planned(c, Create, added("size", "small")),
			},
			calls: []string{"plan test.Thing/a", "plan test.Thing/b", "plan test.Thing/c"},
		},
		{
			name: "the ID of a created task is known after apply",
			tasks: []Task{
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
				cloudTask{key: a, want: values{"size": "small"}},
			},
			want: []PlannedChange{
				planned(a, Create, added("size", "small")),
				planned(b, Create, added("parent", KnownAfterApply)),
			},
			calls: []string{"plan test.Thing/a", "plan test.Thing/b"},
		},
		{
			name: "the ID of a replaced task is known after apply",
			objects: []fakeObject{
				object(a, "1", values{"zone": "ams"}),
				object(b, "2", values{"parent": "1"}),
			},
			tasks: []Task{
				cloudTask{key: a, want: values{"zone": "fra"}},
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
			},
			want: []PlannedChange{
				replaced(a, changed("zone", "ams", "fra")),
				planned(b, Update, changed("parent", "1", KnownAfterApply)),
			},
			calls: []string{"plan test.Thing/a", "plan test.Thing/b"},
		},
		{
			name: "the ID of an unchanged task is known",
			objects: []fakeObject{
				object(a, "1", values{"size": "small"}),
				object(b, "2", values{"parent": "1"}),
			},
			tasks: []Task{
				cloudTask{key: a, want: values{"size": "small"}},
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
			},
			calls: []string{"plan test.Thing/a", "plan test.Thing/b"},
		},
		{
			name: "the ID of an updated task is known",
			objects: []fakeObject{
				object(a, "1", values{"size": "small"}),
				object(b, "2", values{"parent": "1"}),
			},
			tasks: []Task{
				cloudTask{key: a, want: values{"size": "large"}},
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
			},
			want:  []PlannedChange{planned(a, Update, changed("size", "small", "large"))},
			calls: []string{"plan test.Thing/a", "plan test.Thing/b"},
		},
		{
			name:    "an object that no task claims is deleted",
			objects: []fakeObject{object(a, "1", nil), object(b, "2", nil)},
			tasks:   []Task{cloudTask{key: a}},
			want:    []PlannedChange{deleted(b, "2", "")},
			calls:   []string{"plan test.Thing/a"},
		},
		{
			name: "a duplicate is deleted while its task plans as usual",
			objects: []fakeObject{
				duplicate(object(a, "1", values{"size": "large"})),
				object(a, "2", values{"size": "small"}),
			},
			tasks: []Task{cloudTask{key: a, want: values{"size": "large"}}},
			want: []PlannedChange{
				planned(a, Update, changed("size", "small", "large")),
				deleted(a, "1", "duplicate"),
			},
			calls: []string{"plan test.Thing/a"},
		},
		{
			name: "deletes follow the task changes in kind order, then by name and ID",
			objects: []fakeObject{
				object(base("a"), "1", nil),
				object(b, "4", nil),
				object(a, "3", nil),
				duplicate(object(b, "2", nil)),
				object(base("0"), "6", nil),
			},
			tasks: []Task{cloudTask{key: c}},
			want: []PlannedChange{
				planned(c, Create),
				deleted(a, "3", ""),
				deleted(b, "2", "duplicate"),
				deleted(b, "4", ""),
				deleted(base("0"), "6", ""),
				deleted(base("a"), "1", ""),
			},
			calls: []string{"plan test.Thing/c"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newFakeCloud(tc.objects...)
			p, err := NewPlan(t.Context(), tc.tasks, fakeKinds, cloud.snapshot())
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			if diff := cmp.Diff(tc.want, p.Changes(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Changes (-want +got):\n%s", diff)
			}
			if got, want := p.HasChanges(), len(tc.want) > 0; got != want {
				t.Errorf("HasChanges = %v, want %v", got, want)
			}
			if diff := cmp.Diff(tc.calls, cloud.calls(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewPlanClearsOutputs checks that a dependent sees the outputs of a created or replaced task as unknown, even
// when that task's Plan set them.
func TestNewPlanClearsOutputs(t *testing.T) {
	a, b := thing("a"), thing("b")
	unknownParent := planned(b, Update, changed("parent", "7", KnownAfterApply))
	for _, tc := range []struct {
		action Action
		want   []PlannedChange
	}{
		{Noop, nil},
		{Update, []PlannedChange{planned(a, Update)}},
		{Create, []PlannedChange{planned(a, Create), unknownParent}},
		{Replace, []PlannedChange{planned(a, Replace), unknownParent}},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			cloud := newFakeCloud(object(b, "2", values{"parent": "7"}))
			tasks := []Task{
				fakeTask{key: a, outputs: values{"id": "7"}, change: Change{Action: tc.action}},
				cloudTask{key: b, deps: []Key{a}, refs: map[string]Key{"parent": a}},
			}
			p, err := NewPlan(t.Context(), tasks, fakeKinds, cloud.snapshot())
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			if diff := cmp.Diff(tc.want, p.Changes(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Changes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewPlanEnv checks that the plan keeps the snapshot and the outputs that its tasks planned with, for Apply.
func TestNewPlanEnv(t *testing.T) {
	snap := newFakeCloud(object(thing("a"), "1", nil)).snapshot()
	p, err := NewPlan(t.Context(), []Task{cloudTask{key: thing("a")}}, fakeKinds, snap)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if p.env.Snapshot != snap {
		t.Errorf("env snapshot = %v, want the given snapshot", p.env.Snapshot)
	}
	if got, known := p.env.Outputs.Get(thing("a"), "id"); got != "1" || !known {
		t.Errorf("output id of test.Thing/a = %q, %v, want %q, true", got, known, "1")
	}
}

func TestNewPlanNilSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []Task
		want  []PlannedChange
	}{
		{name: "no tasks"},
		{
			name:  "a task",
			tasks: []Task{fakeTask{key: thing("a"), change: Change{Action: Create}}},
			want:  []PlannedChange{planned(thing("a"), Create)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPlan(t.Context(), tc.tasks, fakeKinds, nil)
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			if diff := cmp.Diff(tc.want, p.Changes(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Changes (-want +got):\n%s", diff)
			}
			if got, want := p.HasChanges(), len(tc.want) > 0; got != want {
				t.Errorf("HasChanges = %v, want %v", got, want)
			}
		})
	}
}

func TestNewPlanRejects(t *testing.T) {
	errBoom := errors.New("boom")
	a, b := thing("a"), thing("b")
	later := cloudTask{key: b, deps: []Key{a}}
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
		want    string
		is      error // an error that the result wraps, when set
	}{
		{
			name:    "a problem with the tasks comes first",
			objects: []fakeObject{object(Key{Kind: "test.Other", Name: "x"}, "7", nil)},
			tasks:   []Task{cloudTask{key: a}, cloudTask{key: a}},
			want:    "duplicate task test.Thing/a",
		},
		{
			name:    "an object of an unknown kind",
			objects: []fakeObject{object(a, "1", nil), object(Key{Kind: "test.Other", Name: "x"}, "7", nil)},
			tasks:   []Task{cloudTask{key: a}},
			want:    "the snapshot has test.Other/x (ID 7) of unknown kind test.Other",
		},
		{
			name:    "an object twice",
			objects: []fakeObject{object(a, "1", nil), object(b, "5", nil), object(a, "2", nil)},
			tasks:   []Task{cloudTask{key: a}},
			want:    "the snapshot has test.Thing/a twice (IDs 1 and 2); the provider must mark duplicates",
		},
		{
			name:  "a task fails",
			tasks: []Task{fakeTask{key: a, err: errBoom}, later},
			want:  "plan test.Thing/a: boom",
			is:    errBoom,
		},
		{
			name:  "a task plans a delete",
			tasks: []Task{fakeTask{key: a, change: Change{Action: Delete}}, later},
			want:  "plan test.Thing/a: a task may plan noop, create, update or replace, not delete",
		},
		{
			name:  "a task plans an unknown action",
			tasks: []Task{fakeTask{key: a, change: Change{Action: Action(9)}}, later},
			want:  "plan test.Thing/a: a task may plan noop, create, update or replace, not Action(9)",
		},
		{
			name:  "a task plans a negative action",
			tasks: []Task{fakeTask{key: a, change: Change{Action: Action(-1)}}, later},
			want:  "plan test.Thing/a: a task may plan noop, create, update or replace, not Action(-1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newFakeCloud(tc.objects...)
			p, err := NewPlan(t.Context(), tc.tasks, fakeKinds, cloud.snapshot())
			if err == nil {
				t.Fatalf("NewPlan = %v, want error %q", p.Changes(), tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("NewPlan error = %q, want %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("NewPlan error %q does not wrap %q", err, tc.is)
			}
			if calls := cloud.calls(); len(calls) > 0 {
				t.Errorf("tasks were called after the problem: %q", calls)
			}
		})
	}
}

// cancelTask cancels the run's context when it plans.
type cancelTask struct {
	fakeTask
	cancel context.CancelFunc
}

func (t cancelTask) Plan(context.Context, *Env) (Change, error) {
	t.cancel()
	return Change{}, nil
}

func TestNewPlanCancelled(t *testing.T) {
	a, b := thing("a"), thing("b")
	for _, tc := range []struct {
		name  string
		tasks func(cancel context.CancelFunc) []Task
		early bool // cancel before NewPlan
	}{
		{
			name:  "before planning",
			tasks: func(context.CancelFunc) []Task { return []Task{cloudTask{key: a}} },
			early: true,
		},
		{
			name: "between tasks",
			tasks: func(cancel context.CancelFunc) []Task {
				return []Task{cancelTask{fakeTask: fakeTask{key: a}, cancel: cancel}, cloudTask{key: b, deps: []Key{a}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.early {
				cancel()
			}
			cloud := newFakeCloud()
			p, err := NewPlan(ctx, tc.tasks(cancel), fakeKinds, cloud.snapshot())
			if err == nil {
				t.Fatalf("NewPlan = %v, want an error", p.Changes())
			}
			if got, want := err.Error(), "plan: context canceled"; got != want {
				t.Errorf("NewPlan error = %q, want %q", got, want)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("NewPlan error %q does not wrap context.Canceled", err)
			}
			if calls := cloud.calls(); len(calls) > 0 {
				t.Errorf("tasks were called after the cancel: %q", calls)
			}
		})
	}
}

func TestPlanChangesIsACopy(t *testing.T) {
	cloud := newFakeCloud(object(thing("a"), "1", values{"size": "small"}))
	p, err := NewPlan(t.Context(), []Task{cloudTask{key: thing("a"), want: values{"size": "large"}}}, fakeKinds,
		cloud.snapshot())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	want := []PlannedChange{planned(thing("a"), Update, changed("size", "small", "large"))}
	got := p.Changes()
	if len(got) != 1 || len(got[0].Diff) != 1 {
		t.Fatalf("Changes = %v, want one change with one field diff", got)
	}
	got[0].Action = Delete
	got[0].Reason = "changed"
	got[0].Diff[0].New = "changed"
	got[0].Diff = append(got[0].Diff, added("extra", "x"))
	if diff := cmp.Diff(want, p.Changes()); diff != "" {
		t.Errorf("Changes after changing a copy (-before +after):\n%s", diff)
	}
}
