package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/engine"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// checkGolden compares got with the file testdata/name. With -update it rewrites the file first.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("output differs from %s (-file +got):\n%s", path, diff)
	}
}

// planText returns what p.WriteText writes.
func planText(t *testing.T, p app.UpdatePlan) string {
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

// stubTask is an infrastructure task that plans its change whatever the snapshot holds.
type stubTask struct {
	key    engine.Key
	change engine.Change
}

func (s stubTask) Key() engine.Key { return s.key }

func (stubTask) Deps() []engine.Key { return nil }

func (s stubTask) Plan(context.Context, *engine.Env) (engine.Change, error) { return s.change, nil }

func (stubTask) Apply(context.Context, *engine.Env, engine.Change) error { return nil }

func (stubTask) Delete(context.Context, *engine.Env, engine.Object) error { return nil }

// cloudObjects is a snapshot that lists these objects.
type cloudObjects []engine.Object

func (o cloudObjects) Objects() []engine.Object { return o }

// Kinds of the stub infrastructure, named as those of Vultr.
const (
	kindFirewall = "vultr.FirewallGroup"
	kindVPC      = "vultr.VPC"
	kindSSHKey   = "vultr.SSHKey"
)

// infraPlan plans the stub tasks against a snapshot of the objects.
func infraPlan(t *testing.T, objects cloudObjects, tasks ...engine.Task) *engine.Plan {
	t.Helper()
	var kinds []engine.Kind
	for _, k := range []string{kindFirewall, kindVPC, kindSSHKey} {
		kinds = append(kinds, engine.Kind{Name: k, Deleter: stubTask{}})
	}
	p, err := engine.NewPlan(t.Context(), tasks, kinds, objects)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// exampleInfra is an infrastructure plan with two creates, an update with a field diff and a delete.
func exampleInfra(t *testing.T) *engine.Plan {
	t.Helper()
	servers := engine.Key{Kind: kindFirewall, Name: "prod-servers"}
	return infraPlan(t,
		cloudObjects{
			{Key: servers, ID: "fw-1"},
			{Key: engine.Key{Kind: kindSSHKey, Name: "prod-99aabbcc"}, ID: "ssh-9"},
		},
		stubTask{key: engine.Key{Kind: kindVPC, Name: "prod"}, change: engine.Change{Action: engine.Create}},
		stubTask{key: engine.Key{Kind: kindFirewall, Name: "prod-clients"}, change: engine.Change{Action: engine.Create}},
		stubTask{key: servers, change: engine.Change{
			Action: engine.Update,
			Diff:   []engine.FieldDiff{{Field: "rules", New: "tcp/4646 from 203.0.113.7/32"}},
		}},
	)
}

// waitOp is the operation id of the node that exampleNodes waits for.
const waitOp = "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70"

// exampleNodes returns node changes of every kind, in the order a plan gives them.
func exampleNodes() []app.NodeChange {
	return []app.NodeChange{
		{
			Action: app.NodeWait, Name: "prod-servers-1", Group: "servers", Role: v1alpha1.RoleServer, Zone: "ams",
			MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04", ID: "instance-2", Op: waitOp,
		},
		{
			Action: app.NodeCreate, Name: "prod-servers-2", Group: "servers", Role: v1alpha1.RoleServer, Zone: "ams",
			MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04",
		},
		{
			Action: app.NodeCreate, Name: "prod-workers-1", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
			MachineType: "vc2-4c-8gb", Image: "ubuntu-24.04",
		},
		{Action: app.NodeDelete, Name: "prod-old-0", ID: "instance-7", Reason: "not in the spec"},
		{Action: app.NodeDelete, Name: "prod-workers-0", ID: "instance-5", Reason: "duplicate"},
		{Action: app.NodeDelete, Name: "prod-workers-3", ID: "instance-8", Reason: "surplus"},
	}
}

// secretNames are the test cluster's secrets as a plan names them: relative to the cluster, in the order an update
// writes them.
var secretNames = []string{
	"pki/private/ca.key", "pki/ca-bundle.pem", "secrets/gossip.key", "secrets/acl-bootstrap-token",
}

// someNodes returns a create and a delete of nodes.
func someNodes() []app.NodeChange {
	nodes := exampleNodes()
	return []app.NodeChange{nodes[2], nodes[5]}
}

// exampleUpdate is an update plan with every part.
func exampleUpdate(t *testing.T) app.UpdatePlan {
	t.Helper()
	return app.UpdatePlan{Infra: exampleInfra(t), Nodes: exampleNodes(), Secrets: secretNames, Completed: true}
}

func TestUpdatePlanWriteTextGolden(t *testing.T) {
	got := planText(t, exampleUpdate(t))
	checkGolden(t, "update_plan.golden", got)
	if strings.Contains(got, waitOp) {
		t.Errorf("the text shows the operation id %s:\n%s", waitOp, got)
	}
}

func TestUpdatePlanJSONGolden(t *testing.T) {
	checkGolden(t, "update_plan.json.golden", encodeJSON(t, exampleUpdate(t), "  "))
}

func TestUpdatePlanWithoutChanges(t *testing.T) {
	p := app.UpdatePlan{Infra: infraPlan(t, nil)}
	checkGolden(t, "update_plan_empty.golden", planText(t, p))
	checkGolden(t, "update_plan_empty.json.golden", encodeJSON(t, p, "  "))
}

func TestUpdatePlanWriteText(t *testing.T) {
	const (
		nodesOnly = `+ node prod-workers-1 (client, vc2-4c-8gb, ams)
- node prod-workers-3 (ID instance-8, surplus)

Nodes: 1 to create, 0 to wait for, 1 to delete.
`
		completedOnly = "State: cluster.completed.yaml will be written.\n"
		secretsOnly   = "State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key and " +
			"secrets/acl-bootstrap-token will be written.\n"
	)
	var infraOnly strings.Builder
	if err := exampleInfra(t).WriteText(&infraOnly); err != nil {
		t.Fatalf("WriteText of the infrastructure: %v", err)
	}
	for _, tc := range []struct {
		name string
		plan app.UpdatePlan
		want string
	}{
		{"the infrastructure only, as the engine writes it", app.UpdatePlan{Infra: exampleInfra(t)}, infraOnly.String()},
		{"the nodes only", app.UpdatePlan{Infra: infraPlan(t, nil), Nodes: someNodes()}, nodesOnly},
		{"no infrastructure plan", app.UpdatePlan{Nodes: someNodes()}, nodesOnly},
		{"the completed spec only", app.UpdatePlan{Infra: infraPlan(t, nil), Completed: true}, completedOnly},
		{
			"the nodes and the completed spec",
			app.UpdatePlan{Nodes: someNodes(), Completed: true},
			nodesOnly + completedOnly,
		},
		{"the secrets only", app.UpdatePlan{Infra: infraPlan(t, nil), Secrets: secretNames}, secretsOnly},
		{
			"the nodes, the secrets and the completed spec",
			app.UpdatePlan{Nodes: someNodes(), Secrets: secretNames, Completed: true},
			nodesOnly + "State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, " +
				"secrets/acl-bootstrap-token and cluster.completed.yaml will be written.\n",
		},
		{"one secret", app.UpdatePlan{Secrets: secretNames[1:2]}, "State: pki/ca-bundle.pem will be written.\n"},
		{
			"one secret and the completed spec",
			app.UpdatePlan{Secrets: secretNames[2:3], Completed: true},
			"State: secrets/gossip.key and cluster.completed.yaml will be written.\n",
		},
		{"nothing", app.UpdatePlan{}, "No changes.\n"},
		{
			"a delete without a reason",
			app.UpdatePlan{Nodes: []app.NodeChange{{Action: app.NodeDelete, Name: "prod-x-0", ID: "i-1"}}},
			"- node prod-x-0 (ID i-1)\n\nNodes: 0 to create, 0 to wait for, 1 to delete.\n",
		},
		{
			"an unknown action",
			app.UpdatePlan{Nodes: []app.NodeChange{{Name: "prod-x-0", ID: "i-1"}}},
			"NodeAction(0) node prod-x-0\n\nNodes: 0 to create, 0 to wait for, 0 to delete.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, planText(t, tc.plan)); diff != "" {
				t.Errorf("WriteText (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdatePlanHasChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan app.UpdatePlan
		want bool
	}{
		{"nothing", app.UpdatePlan{}, false},
		{"an infrastructure plan without changes", app.UpdatePlan{Infra: infraPlan(t, nil)}, false},
		{"the infrastructure", app.UpdatePlan{Infra: exampleInfra(t)}, true},
		{"the nodes", app.UpdatePlan{Infra: infraPlan(t, nil), Nodes: someNodes()}, true},
		{"both", app.UpdatePlan{Infra: exampleInfra(t), Nodes: someNodes()}, true},
		{"the completed spec", app.UpdatePlan{Infra: infraPlan(t, nil), Completed: true}, true},
		{"a secret", app.UpdatePlan{Infra: infraPlan(t, nil), Secrets: secretNames[3:]}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.HasChanges(); got != tc.want {
				t.Errorf("HasChanges() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpdatePlanJSONWithoutInfrastructure(t *testing.T) {
	for _, tc := range []struct {
		plan app.UpdatePlan
		want string
	}{
		{app.UpdatePlan{}, `{"infrastructure":null,"nodes":[]}` + "\n"},
		{app.UpdatePlan{Completed: true}, `{"infrastructure":null,"nodes":[],"completedSpec":true}` + "\n"},
		{app.UpdatePlan{Applied: true}, `{"applied":true,"infrastructure":null,"nodes":[]}` + "\n"},
		{
			app.UpdatePlan{Secrets: secretNames[2:], Completed: true},
			`{"infrastructure":null,"nodes":[],"secrets":["secrets/gossip.key","secrets/acl-bootstrap-token"],` +
				`"completedSpec":true}` + "\n",
		},
	} {
		if got := encodeJSON(t, tc.plan, ""); got != tc.want {
			t.Errorf("JSON = %q, want %q", got, tc.want)
		}
	}
}

// TestUpdatePlanJSONLeavesEscapingToTheCaller checks that the plan's JSON escapes HTML characters such as < and &
// only when the caller's encoder does, in both parts.
func TestUpdatePlanJSONLeavesEscapingToTheCaller(t *testing.T) {
	p := app.UpdatePlan{
		Infra: infraPlan(t, nil, stubTask{key: engine.Key{Kind: kindVPC, Name: "prod"}, change: engine.Change{
			Action: engine.Create, Reason: "<new> & more",
		}}),
		Nodes: []app.NodeChange{{Action: app.NodeDelete, Name: "prod-x-0", ID: "i-1", Reason: "<old> & gone"}},
	}
	want := `{"infrastructure":{"changes":[{"kind":"vultr.VPC","name":"prod","action":"create",` +
		`"reason":"<new> & more"}],"summary":{"create":1,"update":0,"replace":0,"delete":0}},` +
		`"nodes":[{"action":"delete","name":"prod-x-0","id":"i-1","reason":"<old> & gone"}]}` + "\n"
	if got := encodeJSON(t, p, ""); got != want {
		t.Errorf("JSON without HTML escaping = %q, want %q", got, want)
	}
	escaped, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`\u003cnew\u003e \u0026 more`, `\u003cold\u003e \u0026 gone`} {
		if !strings.Contains(string(escaped), want) {
			t.Errorf("json.Marshal = %s, want it to hold %s", escaped, want)
		}
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

func TestUpdatePlanWriteTextWriteError(t *testing.T) {
	errBroken := errors.New("broken pipe")
	for _, tc := range []struct {
		name string
		plan app.UpdatePlan
	}{
		{"changes", app.UpdatePlan{Infra: exampleInfra(t), Nodes: exampleNodes()}},
		{"no changes", app.UpdatePlan{Infra: infraPlan(t, nil)}},
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

// appliedText returns what p.WriteApplied writes.
func appliedText(t *testing.T, p interface{ WriteApplied(io.Writer) error }) string {
	t.Helper()
	var b strings.Builder
	if err := p.WriteApplied(&b); err != nil {
		t.Fatalf("WriteApplied: %v", err)
	}
	return b.String()
}

func TestUpdatePlanWriteApplied(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan app.UpdatePlan
		want string
	}{
		{
			"every part",
			exampleUpdate(t),
			"Applied: 2 created, 1 updated, 0 replaced, 1 deleted. Nodes: 2 created, 1 waited for, 3 deleted. " +
				"Wrote pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token and " +
				"cluster.completed.yaml.\n",
		},
		{"the infrastructure only", app.UpdatePlan{Infra: exampleInfra(t)},
			"Applied: 2 created, 1 updated, 0 replaced, 1 deleted.\n"},
		{"the nodes only", app.UpdatePlan{Infra: infraPlan(t, nil), Nodes: someNodes()},
			"Nodes: 1 created, 0 waited for, 1 deleted.\n"},
		{"the completed spec only", app.UpdatePlan{Completed: true}, "Wrote cluster.completed.yaml.\n"},
		{"one secret only", app.UpdatePlan{Secrets: secretNames[1:2]}, "Wrote pki/ca-bundle.pem.\n"},
		{
			"the nodes and two secrets",
			app.UpdatePlan{Nodes: someNodes(), Secrets: secretNames[2:]},
			"Nodes: 1 created, 0 waited for, 1 deleted. Wrote secrets/gossip.key and secrets/acl-bootstrap-token.\n",
		},
		{"nothing", app.UpdatePlan{Infra: infraPlan(t, nil)}, "No changes.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, appliedText(t, tc.plan)); diff != "" {
				t.Errorf("WriteApplied (-want +got):\n%s", diff)
			}
		})
	}
	err := app.UpdatePlan{Completed: true}.WriteApplied(&failWriter{err: errors.New("broken pipe")})
	if want := "writing the plan: broken pipe"; err == nil || err.Error() != want {
		t.Errorf("WriteApplied error = %v, want %q", err, want)
	}
}
