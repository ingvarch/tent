package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// removed is the diff of a field that loses its value.
func removed(field, value string) FieldDiff { return FieldDiff{Field: field, Old: value} }

// stub is the task k that plans ch whatever the snapshot holds.
func stub(k Key, ch Change) Task { return fakeTask{key: k, change: ch} }

// examplePlan is a plan with every action: two creates, an update and a replace with field diffs, then the deletes of
// an object that no task claims and of a duplicate. Its task without changes does not show.
func examplePlan(t *testing.T) *Plan {
	t.Helper()
	cloud := newFakeCloud(
		object(thing("prod-servers"), "thing-1", nil),
		object(thing("prod-99aabbcc"), "ssh-9", nil),
		object(base("other"), "base-1", nil),
		object(base("shared"), "base-2", nil),
		duplicate(object(base("shared"), "base-3", nil)),
	)
	return planFor(t, cloud,
		stub(thing("prod-1a2b3c4d"), Change{Action: Create}),
		stub(base("prod"), Change{Action: Create}),
		stub(thing("prod-servers"), Change{Action: Update, Diff: []FieldDiff{
			added("rules", "tcp/4646 from 203.0.113.7/32"),
			removed("rules", "tcp/22 from 0.0.0.0/0"),
			changed("description", "old", "new"),
		}}),
		stub(base("other"), Change{
			Action: Replace,
			Reason: "networking.cidr changed",
			Diff:   []FieldDiff{changed("cidr", "10.0.0.0/16", "10.1.0.0/16")},
		}),
		stub(base("shared"), Change{Action: Noop}),
	)
}

// checkGolden compares got with the file testdata/name. With -update it rewrites the file first.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("output differs from %s (-file +got):\n%s", path, diff)
	}
}

// text returns what p.WriteText writes.
func text(t *testing.T, p *Plan) string {
	t.Helper()
	var b strings.Builder
	if err := p.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

// encodeJSON encodes v as the CLI does, without escaping HTML characters, and indented by indent unless it is empty.
func encodeJSON(t *testing.T, v any, indent string) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b.String()
}

func TestPlanWriteTextGolden(t *testing.T) {
	checkGolden(t, "plan.golden", []byte(text(t, examplePlan(t))))
}

func TestPlanJSONGolden(t *testing.T) {
	checkGolden(t, "plan.json.golden", []byte(encodeJSON(t, examplePlan(t), "  ")))
}

func TestPlanWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
	}{
		{name: "no tasks and no objects"},
		{
			name:    "a task without changes",
			objects: []fakeObject{object(thing("a"), "1", nil)},
			tasks:   []Task{stub(thing("a"), Change{Action: Noop})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planFor(t, newFakeCloud(tc.objects...), tc.tasks...)
			if got, want := text(t, p), "No changes.\n"; got != want {
				t.Errorf("WriteText wrote %q, want %q", got, want)
			}
			want := `{"changes":[],"summary":{"create":0,"update":0,"replace":0,"delete":0}}` + "\n"
			if got := encodeJSON(t, p, ""); got != want {
				t.Errorf("JSON = %q, want %q", got, want)
			}
		})
	}
}

func TestPlanWriteTextReasons(t *testing.T) {
	a := thing("a")
	for _, tc := range []struct {
		name    string
		objects []fakeObject
		tasks   []Task
		want    string // the change's line
	}{
		{name: "create", tasks: []Task{stub(a, Change{Action: Create})}, want: "+ test.Thing/a"},
		{
			name:  "create with a reason",
			tasks: []Task{stub(a, Change{Action: Create, Reason: "gone"})},
			want:  "+ test.Thing/a (gone)",
		},
		{name: "update", tasks: []Task{stub(a, Change{Action: Update})}, want: "~ test.Thing/a"},
		{
			name:  "update with a reason",
			tasks: []Task{stub(a, Change{Action: Update, Reason: "tags changed"})},
			want:  "~ test.Thing/a (tags changed)",
		},
		{name: "replace", tasks: []Task{stub(a, Change{Action: Replace})}, want: "-/+ test.Thing/a"},
		{
			name:  "replace with a reason",
			tasks: []Task{stub(a, Change{Action: Replace, Reason: "zone changed"})},
			want:  "-/+ test.Thing/a (zone changed)",
		},
		{name: "delete", objects: []fakeObject{object(a, "1", nil)}, want: "- test.Thing/a (ID 1)"},
		{
			name:    "delete of a duplicate",
			objects: []fakeObject{object(a, "1", nil), duplicate(object(a, "2", nil))},
			tasks:   []Task{stub(a, Change{Action: Noop})},
			want:    "- test.Thing/a (ID 2, duplicate)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := strings.Cut(text(t, planFor(t, newFakeCloud(tc.objects...), tc.tasks...)), "\n")
			if got != tc.want {
				t.Errorf("first line = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanWriteTextDiff(t *testing.T) {
	p := planFor(t, newFakeCloud(), stub(thing("a"), Change{Action: Update, Diff: []FieldDiff{
		added("size", "large"),
		removed("zone", "ams"),
		changed("image", "ubuntu-24.04", "ubuntu-26.04"),
		{Field: "password"},
	}}))
	want := `~ test.Thing/a
    + size: large
    - zone: ams
    ~ image: ubuntu-24.04 -> ubuntu-26.04
    ~ password

Plan: 0 to create, 1 to update, 0 to replace, 0 to delete.
`
	if diff := cmp.Diff(want, text(t, p)); diff != "" {
		t.Errorf("WriteText (-want +got):\n%s", diff)
	}
}

// failWriter fails every write with err and counts the writes.
type failWriter struct {
	err    error
	writes int
}

func (w *failWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
}

func TestPlanWriteTextWriteError(t *testing.T) {
	errBroken := errors.New("broken pipe")
	for _, tc := range []struct {
		name string
		plan *Plan
	}{
		{"changes", examplePlan(t)},
		{"no changes", planFor(t, newFakeCloud())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &failWriter{err: errBroken}
			err := tc.plan.WriteText(w)
			if got, want := err, "writing the plan: broken pipe"; got == nil || got.Error() != want {
				t.Errorf("WriteText error = %v, want %q", got, want)
			}
			if !errors.Is(err, errBroken) {
				t.Errorf("WriteText error %v does not wrap %v", err, errBroken)
			}
			if w.writes != 1 {
				t.Errorf("WriteText wrote %d times, want 1: it stops at the first error", w.writes)
			}
		})
	}
}

// TestPlanSummary counts a different number of changes for each action, so that no two counts can be mixed up.
func TestPlanSummary(t *testing.T) {
	cloud := newFakeCloud(
		object(thing("u1"), "1", nil), object(thing("u2"), "2", nil),
		object(thing("r1"), "3", nil), object(thing("r2"), "4", nil), object(thing("r3"), "5", nil),
		object(base("d1"), "6", nil), object(base("d2"), "7", nil), object(base("d3"), "8", nil),
		object(base("d4"), "9", nil),
	)
	p := planFor(t, cloud,
		stub(thing("c1"), Change{Action: Create}),
		stub(thing("u1"), Change{Action: Update}), stub(thing("u2"), Change{Action: Update}),
		stub(thing("r1"), Change{Action: Replace}), stub(thing("r2"), Change{Action: Replace}),
		stub(thing("r3"), Change{Action: Replace}),
	)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encodeJSON(t, p, "")), &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := string(fields["summary"]), `{"create":1,"update":2,"replace":3,"delete":4}`; got != want {
		t.Errorf("JSON summary = %s, want %s", got, want)
	}
	const line = "Plan: 1 to create, 2 to update, 3 to replace, 4 to delete."
	lines := strings.Split(strings.TrimSuffix(text(t, p), "\n"), "\n")
	if got := lines[len(lines)-1]; got != line {
		t.Errorf("last line = %q, want %q", got, line)
	}
	s := p.Summary()
	if want := (Summary{Create: 1, Update: 2, Replace: 3, Delete: 4}); s != want {
		t.Errorf("Summary() = %+v, want %+v", s, want)
	}
	if got := s.String(); got != line {
		t.Errorf("Summary().String() = %q, want %q", got, line)
	}
}

// changesText returns what p.WriteChanges writes.
func changesText(t *testing.T, p *Plan) string {
	t.Helper()
	var b strings.Builder
	if err := p.WriteChanges(&b); err != nil {
		t.Fatalf("WriteChanges: %v", err)
	}
	return b.String()
}

// TestPlanWriteChanges writes the lines of the changes as WriteText does, without the blank line and the counts.
func TestPlanWriteChanges(t *testing.T) {
	p := examplePlan(t)
	all := text(t, p)
	want, _, ok := strings.Cut(all, "\n\n")
	if !ok {
		t.Fatalf("WriteText has no blank line:\n%s", all)
	}
	if got := changesText(t, p); got != want+"\n" {
		t.Errorf("WriteChanges wrote\n%s\nwant\n%s", got, want+"\n")
	}
	if got := changesText(t, planFor(t, newFakeCloud())); got != "" {
		t.Errorf("WriteChanges of a plan without changes wrote %q, want nothing", got)
	}
	if got := planFor(t, newFakeCloud()).Summary(); got != (Summary{}) {
		t.Errorf("Summary() of a plan without changes = %+v, want zeros", got)
	}

	errBroken := errors.New("broken pipe")
	w := &failWriter{err: errBroken}
	err := p.WriteChanges(w)
	const broken = "writing the plan: broken pipe"
	if err == nil || err.Error() != broken || !errors.Is(err, errBroken) {
		t.Errorf("WriteChanges error = %v, want %q that wraps %v", err, broken, errBroken)
	}
	if w.writes != 1 {
		t.Errorf("WriteChanges wrote %d times, want 1", w.writes)
	}
}

// TestPlanJSONLeavesEscapingToTheCaller checks that the plan's JSON escapes HTML characters such as < and & only when
// the caller's encoder does.
func TestPlanJSONLeavesEscapingToTheCaller(t *testing.T) {
	p := planFor(t, newFakeCloud(), stub(thing("a"), Change{
		Action: Create,
		Diff:   []FieldDiff{added("rule", "tcp/22 from <any> & more")},
	}))
	want := `{"changes":[{"kind":"test.Thing","name":"a","action":"create",` +
		`"diff":[{"field":"rule","new":"tcp/22 from <any> & more"}]}],` +
		`"summary":{"create":1,"update":0,"replace":0,"delete":0}}` + "\n"
	if got := encodeJSON(t, p, ""); got != want {
		t.Errorf("JSON without HTML escaping = %q, want %q", got, want)
	}
	escaped, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := "tcp/22 from \\u003cany\\u003e \\u0026 more"; !strings.Contains(string(escaped), want) {
		t.Errorf("json.Marshal = %s, want it to hold %s", escaped, want)
	}
}
