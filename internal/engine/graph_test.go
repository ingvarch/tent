package engine

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestNewGraphOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []Task
		kinds []Kind // thingKinds when nil
		want  []Key
	}{
		{name: "no tasks"},
		{
			name:  "chain",
			tasks: []Task{task("c", "b"), task("b", "a"), task("a")},
			want:  things("a", "b", "c"),
		},
		{
			name:  "diamond",
			tasks: []Task{task("d", "b", "c"), task("c", "a"), task("b", "a"), task("a")},
			want:  things("a", "c", "b", "d"),
		},
		{
			name:  "independent tasks keep input order",
			tasks: []Task{task("z"), task("y"), task("x")},
			want:  things("z", "y", "x"),
		},
		{
			name:  "a task that becomes ready goes before later ready tasks",
			tasks: []Task{task("b", "a"), task("a"), task("c")},
			want:  things("a", "b", "c"),
		},
		{
			name:  "dependency listed twice",
			tasks: []Task{task("b", "a", "a"), task("a")},
			want:  things("a", "b"),
		},
		{
			name:  "kind without tasks",
			tasks: []Task{task("a")},
			kinds: []Kind{{Name: thingKind, Deleter: fakeTask{}}, {Name: "test.Other", Deleter: fakeTask{}}},
			want:  things("a"),
		},
		{
			name:  "a task depends on a task of a later kind",
			tasks: []Task{fakeTask{key: thing("a"), deps: []Key{base("b")}}, fakeTask{key: base("b")}},
			kinds: fakeKinds,
			want:  []Key{base("b"), thing("a")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := tc.kinds
			if kinds == nil {
				kinds = thingKinds
			}
			g, err := newGraph(tc.tasks, kinds)
			if err != nil {
				t.Fatalf("newGraph: %v", err)
			}
			if diff := cmp.Diff(tc.want, keysOf(g.order)); diff != "" {
				t.Errorf("order (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewGraphEdges(t *testing.T) {
	g, err := newGraph([]Task{task("d", "b", "c", "b"), task("c", "a"), task("b", "a"), task("a")}, thingKinds)
	if err != nil {
		t.Fatalf("newGraph: %v", err)
	}
	for _, tc := range []struct {
		task             string
		deps, dependents []Key
	}{
		{"a", nil, things("c", "b")},
		{"b", things("a"), things("d")},
		{"c", things("a"), things("d")},
		{"d", things("b", "c"), nil},
	} {
		k := thing(tc.task)
		if diff := cmp.Diff(tc.deps, g.deps[k], cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("deps of %s (-want +got):\n%s", k, diff)
		}
		if diff := cmp.Diff(tc.dependents, g.dependents[k], cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("dependents of %s (-want +got):\n%s", k, diff)
		}
	}
}

func TestNewGraphRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []Task
		kinds []Kind // thingKinds when nil
		want  string
	}{
		{
			name:  "task without kind",
			tasks: []Task{task("a"), fakeTask{key: Key{Name: "x"}}},
			want:  "task /x has no kind",
		},
		{
			name:  "task without name",
			tasks: []Task{task("a"), fakeTask{key: Key{Kind: thingKind}}},
			want:  "task of kind test.Thing has no name",
		},
		{
			name:  "duplicate task",
			tasks: []Task{task("a"), task("b"), task("a")},
			want:  "duplicate task test.Thing/a",
		},
		{
			name:  "kind without name",
			tasks: []Task{task("a")},
			kinds: []Kind{{Name: thingKind, Deleter: fakeTask{}}, {}},
			want:  "a kind has no name",
		},
		{
			name:  "kind listed twice",
			tasks: []Task{task("a")},
			kinds: []Kind{{Name: thingKind, Deleter: fakeTask{}}, {Name: thingKind, Deleter: fakeTask{}}},
			want:  "kind test.Thing is listed twice",
		},
		{
			name:  "kind without deleter",
			tasks: []Task{task("a")},
			kinds: []Kind{{Name: thingKind}},
			want:  "kind test.Thing has no deleter",
		},
		{
			name:  "task of a kind that is not listed",
			tasks: []Task{task("a"), fakeTask{key: Key{Kind: "test.Other", Name: "x"}}},
			want:  "task test.Other/x has kind test.Other, which is not among the kinds",
		},
		{
			name:  "dependency that is not a task",
			tasks: []Task{task("b"), task("a", "b", "c")},
			want:  "task test.Thing/a depends on test.Thing/c, which is not a task",
		},
		{
			name:  "dependency on itself",
			tasks: []Task{task("x", "x")},
			want:  "dependency cycle: test.Thing/x -> test.Thing/x",
		},
		{
			name:  "cycle of three",
			tasks: []Task{task("a", "b"), task("b", "c"), task("c", "a")},
			want:  "dependency cycle: test.Thing/a -> test.Thing/b -> test.Thing/c -> test.Thing/a",
		},
		{
			name:  "cycle starts at its first task in input order",
			tasks: []Task{task("d", "a"), task("b", "a"), task("a", "b")},
			want:  "dependency cycle: test.Thing/b -> test.Thing/a -> test.Thing/b",
		},
		{
			name:  "a missing kind is found before a duplicate",
			tasks: []Task{task("a"), task("a"), fakeTask{key: Key{Name: "x"}}},
			want:  "task /x has no kind",
		},
		{
			name:  "a task depends on a task of an earlier kind",
			tasks: []Task{task("a"), fakeTask{key: base("b"), deps: things("a")}},
			kinds: fakeKinds,
			want:  "kind test.Base must come before kind test.Thing, because task test.Base/b depends on test.Thing/a",
		},
		{
			name:  "a missing dependency is found before a cycle",
			tasks: []Task{task("a", "a"), task("b", "c")},
			want:  "task test.Thing/b depends on test.Thing/c, which is not a task",
		},
		{
			name:  "a missing dependency is found before a kind order problem",
			tasks: []Task{fakeTask{key: base("b"), deps: things("a")}, task("a"), task("c", "d")},
			kinds: fakeKinds,
			want:  "task test.Thing/c depends on test.Thing/d, which is not a task",
		},
		{
			name:  "a cycle is found before a kind order problem",
			tasks: []Task{task("x", "x"), fakeTask{key: base("b"), deps: things("x")}},
			kinds: fakeKinds,
			want:  "dependency cycle: test.Thing/x -> test.Thing/x",
		},
		{
			name:  "a cycle across kinds is a cycle",
			tasks: []Task{fakeTask{key: thing("a"), deps: []Key{base("b")}}, fakeTask{key: base("b"), deps: things("a")}},
			kinds: fakeKinds,
			want:  "dependency cycle: test.Thing/a -> test.Base/b -> test.Thing/a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := tc.kinds
			if kinds == nil {
				kinds = thingKinds
			}
			g, err := newGraph(tc.tasks, kinds)
			if err == nil {
				t.Fatalf("newGraph = %v, want error %q", keysOf(g.order), tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("newGraph error = %q, want %q", err, tc.want)
			}
		})
	}
}
