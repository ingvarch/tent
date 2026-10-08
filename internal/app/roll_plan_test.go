package app_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

// rollText returns what p.WriteText writes.
func rollText(t *testing.T, p app.RollPlan) string {
	t.Helper()
	var b strings.Builder
	if err := p.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

// rollJSON returns what the plan marshals to, on one line.
func rollJSON(t *testing.T, p app.RollPlan) string {
	t.Helper()
	data, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	return string(data)
}

// workersOutdated returns the group workers with the outdated machines of the names, which hold the IDs instance-<n>.
func workersOutdated(reason string, ids ...string) app.RollGroup {
	g := app.RollGroup{Name: "workers", Role: v1alpha1.RoleClient, Size: 2}
	for _, id := range ids {
		g.Outdated = append(g.Outdated, app.OutdatedNode{
			Name: "prod-workers-" + id, ID: "instance-" + id, Group: "workers", Reason: reason,
		})
	}
	return g
}

// errRollWrite is why the writers of these tests fail.
var errRollWrite = errors.New("broken pipe")

var upToDateServers = app.RollGroup{Name: "servers", Role: v1alpha1.RoleServer, Size: 3}

// TestRollPlanWriteText writes a line for each group, then a blank line and the next step or the words that say none
// is left. A forced machine shows ", forced" after its ID; the other reasons show nothing.
func TestRollPlanWriteText(t *testing.T) {
	t.Parallel()
	create := &app.RollStep{
		Action: "create", Group: "workers", Node: "prod-workers-2",
		Text: "create node prod-workers-2 (client of workers, ams)",
	}
	forced := workersOutdated("forced", "6")
	mixed := workersOutdated("spec hash", "6", "7", "8")
	mixed.Outdated[1].Reason = "no spec hash"
	mixed.Outdated[2].Reason = "forced"
	for _, tc := range []struct {
		name string
		plan app.RollPlan
		want string
	}{
		{"an up to date group, two outdated machines and the next step", app.RollPlan{
			Groups: []app.RollGroup{upToDateServers, workersOutdated("spec hash", "6", "7")}, Next: create,
		}, "node group servers (server, size 3): up to date\n" +
			"node group workers (client, size 2): 2 outdated: prod-workers-6 (ID instance-6) and " +
			"prod-workers-7 (ID instance-7)\n\n" +
			"Next: create node prod-workers-2 (client of workers, ams).\n"},
		{"one outdated machine, forced", app.RollPlan{Groups: []app.RollGroup{forced}, Next: create},
			"node group workers (client, size 2): 1 outdated: prod-workers-6 (ID instance-6, forced)\n\n" +
				"Next: create node prod-workers-2 (client of workers, ams).\n"},
		{"three machines with three reasons", app.RollPlan{Groups: []app.RollGroup{mixed}, Next: create},
			"node group workers (client, size 2): 3 outdated: prod-workers-6 (ID instance-6), " +
				"prod-workers-7 (ID instance-7) and prod-workers-8 (ID instance-8, forced)\n\n" +
				"Next: create node prod-workers-2 (client of workers, ams).\n"},
		{"nothing to roll", app.RollPlan{Groups: []app.RollGroup{upToDateServers}},
			"node group servers (server, size 3): up to date\n\nNothing to roll.\n"},
		{"outdated machines and no next step, a refusal", app.RollPlan{Groups: []app.RollGroup{
			upToDateServers, workersOutdated("spec hash", "6")}},
			"node group servers (server, size 3): up to date\n" +
				"node group workers (client, size 2): 1 outdated: prod-workers-6 (ID instance-6)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, rollText(t, tc.plan)); diff != "" {
				t.Errorf("text (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRollPlanWriteTextFails returns the error of the writer, with what was written.
func TestRollPlanWriteTextFails(t *testing.T) {
	t.Parallel()
	err := app.RollPlan{Groups: []app.RollGroup{upToDateServers}}.WriteText(&failWriter{err: errRollWrite})
	if want := "writing the plan: broken pipe"; err == nil || err.Error() != want {
		t.Errorf("WriteText = %v, want %q", err, want)
	}
}

// TestRollPlanMarshalJSON leaves out applied, next and rolled when they are false, nil or not applied, and shows an
// empty list of outdated machines as [], also for a group that was built without one.
func TestRollPlanMarshalJSON(t *testing.T) {
	t.Parallel()
	step := &app.RollStep{
		Action: "wait-joined", Group: "workers", Node: "prod-workers-2", ID: "instance-9",
		Text: "wait until node prod-workers-2 joins",
	}
	for _, tc := range []struct {
		name string
		plan app.RollPlan
		want string
	}{
		{"not applied", app.RollPlan{Groups: []app.RollGroup{upToDateServers, workersOutdated("spec hash", "6")}, Next: step},
			`{"groups":[{"name":"servers","role":"server","size":3,"outdated":[]},` +
				`{"name":"workers","role":"client","size":2,"outdated":[` +
				`{"name":"prod-workers-6","id":"instance-6","group":"workers","reason":"spec hash"}]}],` +
				`"next":{"action":"wait-joined","group":"workers","node":"prod-workers-2","id":"instance-9",` +
				`"text":"wait until node prod-workers-2 joins"}}`},
		{"applied", app.RollPlan{
			Groups: []app.RollGroup{upToDateServers}, Applied: true,
			Rolled: app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 1},
		}, `{"applied":true,"groups":[{"name":"servers","role":"server","size":3,"outdated":[]}],` +
			`"rolled":{"created":2,"drained":2,"deleted":2,"purged":1}}`},
		{"counts of a plan that did not apply stay out", app.RollPlan{
			Groups: []app.RollGroup{upToDateServers}, Rolled: app.RollCounts{Created: 1},
		}, `{"groups":[{"name":"servers","role":"server","size":3,"outdated":[]}]}`},
		{"no group", app.RollPlan{}, `{"groups":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, rollJSON(t, tc.plan)); diff != "" {
				t.Errorf("JSON (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRollPlanWriteApplied counts the machines and nodes that the roll created, drained, deleted and purged.
func TestRollPlanWriteApplied(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	p := app.RollPlan{Applied: true, Rolled: app.RollCounts{Created: 2, Drained: 3, Deleted: 4, Purged: 5}}
	if err := p.WriteApplied(&b); err != nil {
		t.Fatalf("WriteApplied: %v", err)
	}
	if want := "Rolled: 2 created, 3 drained, 4 deleted, 5 purged.\n"; b.String() != want {
		t.Errorf("WriteApplied wrote %q, want %q", b.String(), want)
	}
	err := p.WriteApplied(&failWriter{err: errRollWrite})
	if want := "writing the summary: broken pipe"; err == nil || err.Error() != want {
		t.Errorf("WriteApplied = %v, want %q", err, want)
	}
}
