package app_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/engine"
)

// deletePlanText returns what p.WriteText writes.
func deletePlanText(t *testing.T, p app.DeletePlan) string {
	t.Helper()
	var b strings.Builder
	if err := p.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

// nodeDelete is the planned delete of the node name, the instance id.
func nodeDelete(name, id string) app.NodeChange {
	return app.NodeChange{Action: app.NodeDelete, Name: name, ID: id}
}

// teardown is an infrastructure plan that deletes a firewall group, a VPC and an SSH key.
func teardown(t *testing.T) *engine.Plan {
	t.Helper()
	return infraPlan(t, cloudObjects{
		{Key: engine.Key{Kind: kindSSHKey, Name: "prod-99aabbcc"}, ID: "ssh-9"},
		{Key: engine.Key{Kind: kindVPC, Name: "prod"}, ID: "vpc-1"},
		{Key: engine.Key{Kind: kindFirewall, Name: "prod-servers"}, ID: "fw-1"},
	})
}

// exampleDelete is a delete plan with every part.
func exampleDelete(t *testing.T) app.DeletePlan {
	t.Helper()
	return app.DeletePlan{
		Nodes: []app.NodeChange{nodeDelete("prod-servers-0", "instance-1"), nodeDelete("prod-workers-0", "instance-4")},
		Infra: teardown(t),
		State: []string{completedPath, serversPath, clusterPath, versionPath},
	}
}

func TestDeletePlanWriteTextGolden(t *testing.T) {
	checkGolden(t, "delete_plan.golden", deletePlanText(t, exampleDelete(t)))
}

func TestDeletePlanJSONGolden(t *testing.T) {
	checkGolden(t, "delete_plan.json.golden", encodeJSON(t, exampleDelete(t), "  "))
}

func TestDeletePlanWriteText(t *testing.T) {
	const (
		nodesOnly = "- node prod-servers-0 (ID instance-1)\n\nNodes: 1 to delete.\n"
		stateOnly = "- state prod/cluster.yaml\n\nState: 1 object to delete.\n"
	)
	var infraOnly strings.Builder
	if err := teardown(t).WriteText(&infraOnly); err != nil {
		t.Fatalf("WriteText of the infrastructure: %v", err)
	}
	one := []app.NodeChange{nodeDelete("prod-servers-0", "instance-1")}
	for _, tc := range []struct {
		name string
		plan app.DeletePlan
		want string
	}{
		{"the infrastructure only, as the engine writes it", app.DeletePlan{Infra: teardown(t)}, infraOnly.String()},
		{"the nodes only", app.DeletePlan{Nodes: one, Infra: infraPlan(t, nil)}, nodesOnly},
		{"the state only", app.DeletePlan{State: []string{clusterPath}}, stateOnly},
		{
			"the state of a cluster whose cloud is unknown",
			app.DeletePlan{State: []string{clusterPath}, CloudUnknown: true},
			stateOnly,
		},
		{
			"the nodes and the state, without infrastructure to delete",
			app.DeletePlan{Nodes: one, Infra: infraPlan(t, nil), State: []string{serversPath, clusterPath}},
			"- node prod-servers-0 (ID instance-1)\n- state prod/nodegroups/servers.yaml\n- state prod/cluster.yaml\n" +
				"\nNodes: 1 to delete.\nState: 2 objects to delete.\n",
		},
		{"nothing", app.DeletePlan{}, "No changes.\n"},
		{"an infrastructure plan without changes", app.DeletePlan{Infra: infraPlan(t, nil)}, "No changes.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, deletePlanText(t, tc.plan)); diff != "" {
				t.Errorf("WriteText (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDeletePlanHasChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan app.DeletePlan
		want bool
	}{
		{"nothing", app.DeletePlan{}, false},
		{"an infrastructure plan without changes", app.DeletePlan{Infra: infraPlan(t, nil)}, false},
		{"the cloud is unknown and there is no state", app.DeletePlan{CloudUnknown: true}, false},
		{"the nodes", app.DeletePlan{Nodes: exampleDelete(t).Nodes}, true},
		{"the infrastructure", app.DeletePlan{Infra: teardown(t)}, true},
		{"the state", app.DeletePlan{State: []string{clusterPath}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.HasChanges(); got != tc.want {
				t.Errorf("HasChanges() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeletePlanJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan app.DeletePlan
		want string
	}{
		{"nothing", app.DeletePlan{}, `{"nodes":[],"infrastructure":null,"state":[]}` + "\n"},
		{
			"the state of a cluster whose cloud is unknown",
			app.DeletePlan{State: []string{clusterPath}, CloudUnknown: true},
			`{"nodes":[],"infrastructure":null,"state":["prod/cluster.yaml"],"cloudUnknown":true}` + "\n",
		},
		{
			"the state of a cluster on a provider that tent cannot manage yet",
			app.DeletePlan{State: []string{clusterPath}, Unsupported: "hetzner"},
			`{"nodes":[],"infrastructure":null,"state":["prod/cluster.yaml"],"unsupportedProvider":"hetzner"}` + "\n",
		},
		{
			"an applied plan",
			app.DeletePlan{State: []string{clusterPath}, Applied: true},
			`{"applied":true,"nodes":[],"infrastructure":null,"state":["prod/cluster.yaml"]}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeJSON(t, tc.plan, ""); got != tc.want {
				t.Errorf("JSON = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeletePlanJSONLeavesEscapingToTheCaller checks that the plan's JSON escapes HTML characters such as < and &
// only when the caller's encoder does.
func TestDeletePlanJSONLeavesEscapingToTheCaller(t *testing.T) {
	p := app.DeletePlan{State: []string{"prod/<notes>&more"}}
	want := `{"nodes":[],"infrastructure":null,"state":["prod/<notes>&more"]}` + "\n"
	if got := encodeJSON(t, p, ""); got != want {
		t.Errorf("JSON without HTML escaping = %q, want %q", got, want)
	}
	escaped, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `\u003cnotes\u003e\u0026more`; !strings.Contains(string(escaped), want) {
		t.Errorf("json.Marshal = %s, want it to hold %s", escaped, want)
	}
}

func TestDeletePlanWriteTextWriteError(t *testing.T) {
	errBroken := errors.New("broken pipe")
	for _, tc := range []struct {
		name string
		plan app.DeletePlan
	}{
		{"changes", exampleDelete(t)},
		{"no changes", app.DeletePlan{}},
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

func TestDeletePlanWriteApplied(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan app.DeletePlan
		want string
	}{
		{"every part", exampleDelete(t), "Deleted: 2 nodes, 3 infrastructure objects, 4 state objects.\n"},
		{
			"one of each",
			app.DeletePlan{
				Nodes: exampleDelete(t).Nodes[:1],
				Infra: infraPlan(t, cloudObjects{{Key: engine.Key{Kind: kindVPC, Name: "prod"}, ID: "vpc-1"}}),
				State: []string{clusterPath},
			},
			"Deleted: 1 node, 1 infrastructure object, 1 state object.\n",
		},
		{"the state only", app.DeletePlan{Infra: infraPlan(t, nil), State: []string{serversPath, clusterPath}},
			"Deleted: 2 state objects.\n"},
		{"nothing", app.DeletePlan{}, "No changes.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, appliedText(t, tc.plan)); diff != "" {
				t.Errorf("WriteApplied (-want +got):\n%s", diff)
			}
		})
	}
}
