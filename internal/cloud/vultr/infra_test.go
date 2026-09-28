package vultr_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
	"github.com/ingvarch/tent/internal/model"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// checkGolden compares got with the file testdata/name. With -update it rewrites the file first.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
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

// newInfraFixture returns every infrastructure task of the example cluster, with its servers and workers, and every
// infrastructure kind, on an empty fake. The provider's operation ids are op-1, op-2 and so on.
func newInfraFixture(t *testing.T) *fixture {
	t.Helper()
	x := newFixture()
	x.tasks = buildInfra(t, x)
	x.kinds = x.p.InfraKinds()
	return x
}

// buildInfra returns the tasks that the fixture's provider builds for the example cluster. It stops the test when the
// build fails.
func buildInfra(t *testing.T, x *fixture) []engine.Task {
	t.Helper()
	tasks, err := x.p.BuildInfra(t.Context(), modelOf(t, exampleCluster(), exampleGroups()))
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	return tasks
}

func TestBuildInfra(t *testing.T) {
	x := newInfraFixture(t)
	want := []engine.Key{sshKeyOf(opsFP), vpcKey, serversKey, clientsKey}
	if diff := cmp.Diff(want, keysOf(x.tasks)); diff != "" {
		t.Errorf("keys (-want +got):\n%s", diff)
	}
}

func TestBuildInfraPlanText(t *testing.T) {
	checkGolden(t, "infra.plan.golden", planText(t, newInfraFixture(t)))
}

func TestBuildInfraApplyReplan(t *testing.T) {
	x := newInfraFixture(t)
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	// Each task has its own operation id.
	wantSSHKeys(t, x.f, govultr.SSHKey{Name: sshKeyName(opsFP, "op-1"), SSHKey: opsKey})
	wantVPCs(t, x.f, prodVPC("op-2"))
	wantFirewallGroups(t, x.f,
		firewallGroup{Description: firewallDescription("server", "op-3"), Rules: serverRules},
		firewallGroup{Description: firewallDescription("client", "op-4"), Rules: clientRules},
	)

	// Tasks built anew, with new operation ids, find the objects that the first ones made.
	x.tasks = buildInfra(t, x)
	if got := planText(t, x); got != "No changes.\n" {
		t.Errorf("the plan of new tasks after apply:\n%s\nwant no changes", got)
	}
}

func TestBuildInfraOtherProvider(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	tasks, err := p.BuildInfra(t.Context(), &model.Cluster{Name: "prod", Provider: v1alpha1.ProviderHetzner})
	const want = `cluster prod runs on "hetzner", not on vultr`
	if err == nil || err.Error() != want {
		t.Errorf("BuildInfra = %v, %v; want the error %q", keysOf(tasks), err, want)
	}
}

func TestInfraKinds(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	var names []string
	for _, k := range p.InfraKinds() {
		names = append(names, k.Name)
		if k.Deleter == nil {
			t.Errorf("the kind %s has no deleter", k.Name)
		}
	}
	// Firewall groups and the VPC go once the instances have left them; the SSH keys go last.
	want := []string{"vultr.FirewallGroup", "vultr.VPC", "vultr.SSHKey"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("kinds (-want +got):\n%s", diff)
	}
}

func TestDeleteInfra(t *testing.T) {
	x := newInfraFixture(t)
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	// Another cluster's objects.
	otherKey := x.f.AddSSHKey(t, govultr.SSHKey{
		Name: "tent:cluster=staging;kind=ssh-key;fp=" + opsFP + ";op=op-a", SSHKey: opsKey,
	})
	otherVPC := x.f.AddVPC(t, govultr.VPC{
		Region: "ams", Description: "tent:cluster=staging;kind=vpc;op=op-b", V4Subnet: "10.65.0.0", V4SubnetMask: 16,
	})
	otherGroup := x.f.AddFirewallGroup(t, govultr.FirewallGroup{
		Description: "tent:cluster=staging;kind=firewall;role=server;op=op-c",
	})
	x.f.AddFirewallRule(t, otherGroup.ID, ruleOf(t, "v4 tcp 0.0.0.0/0 22"))

	// Deleting a cluster plans no tasks: every object of the cluster goes.
	x.tasks = nil
	before := len(x.f.Calls())
	x.applyWithFaults(t, func(testing.TB) {})

	wantSSHKeys(t, x.f, otherKey)
	wantVPCs(t, x.f, otherVPC)
	wantFirewallGroups(t, x.f,
		firewallGroup{Description: otherGroup.Description, Rules: []string{"v4 tcp 0.0.0.0/0 22"}})
	var deletes []string
	for _, c := range x.f.Calls()[before:] {
		if strings.HasPrefix(c.Name, "Delete") {
			deletes = append(deletes, c.Name)
		}
	}
	// In the order of the kinds.
	want := []string{"DeleteFirewallGroup", "DeleteFirewallGroup", "DeleteVPC", "DeleteSSHKey"}
	if diff := cmp.Diff(want, deletes); diff != "" {
		t.Errorf("deletes (-want +got):\n%s", diff)
	}
}

// TestDeleteClusterWithNodes deletes a cluster in the order of delete cluster: the nodes that List gives, then every
// object of the infrastructure.
func TestDeleteClusterWithNodes(t *testing.T) {
	var x *fixture
	synctest.Test(t, func(t *testing.T) {
		x, _ = newNodesFixture(t, opsKey)
		createNode(t, x.p, serverRequest("op-a"))
		createNode(t, x.p, nodeRequest("prod-workers-0", "workers", v1alpha1.RoleClient, "op-b"))
	})
	nodes, err := x.p.List(t.Context(), "prod")
	if err != nil || len(nodes) != 2 {
		t.Fatalf("List = %+v, %v; want the two nodes", nodes, err)
	}
	for _, n := range nodes {
		if err := x.p.Delete(t.Context(), n); err != nil {
			t.Fatalf("Delete %s: %v", n.Name, err)
		}
	}

	// A plan without tasks deletes every object of the cluster.
	x.tasks = nil
	events := x.applyWithFaults(t, func(testing.TB) {})

	// The fake frees a VPC as soon as its instances are gone, so nothing is retried. Vultr refuses the VPC delete for
	// up to 20 s more, and the engine retries it.
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
	if ids := instanceIDs(x.f); len(ids) != 0 {
		t.Errorf("instances %v are left, want none", ids)
	}
	wantSSHKeys(t, x.f)
	wantVPCs(t, x.f)
	wantFirewallGroups(t, x.f)
}
