package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// stateOnly is the plan of the delete of the test cluster on a cloud that holds nothing of it.
const stateOnly = "- state prod/nodegroups/servers.yaml\n" +
	"- state prod/nodegroups/workers.yaml\n" +
	"- state prod/cluster.yaml\n" +
	"\n" +
	"State: 3 objects to delete.\n"

// deleteHint is what delete cluster prints on stderr after its preview.
const deleteHint = "run with --yes to delete them\n"

// stateDeleted is what delete cluster --yes prints on stdout for the test cluster on a cloud that holds nothing of
// it: the plan, then a line that sums up what it deleted.
const stateDeleted = stateOnly + "\nDeleted: 3 state objects.\n"

func TestDeleteClusterPreview(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	wantResult(t, runOn(t, f, "delete", "cluster", "prod", "--state", s.url), 0, stateOnly, deleteHint)
	wantResult(t, runOn(t, f, "delete", "cluster", "--name", "prod", "--state", s.url), 0, stateOnly, deleteHint)
	s.want(t, prodObjects)
	if len(f.Calls()) == 0 {
		t.Error("the preview did not look for the cluster's cloud objects")
	}
	wantNoWrites(t, f)
}

func TestDeleteCluster(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	wantOK(t, runOn(t, f, "delete", "cluster", "prod", "--yes", "--state", s.url), stateDeleted)
	s.want(t, map[string]string{})
	if len(f.Calls()) == 0 {
		t.Error("the delete did not look for the cluster's cloud objects")
	}
}

// TestDeleteClusterWithoutTheKey fails before it deletes anything: the cloud may hold objects of the cluster.
func TestDeleteClusterWithoutTheKey(t *testing.T) {
	s := withCluster(t)
	noKey := func(v1alpha1.Provider, *slog.Logger) (cloud.Provider, error) {
		return nil, errors.New("VULTR_API_KEY is not set")
	}
	for _, yes := range []string{"--yes=false", "--yes"} {
		wantError(t, runProviders(t, noKey, "delete", "cluster", "prod", yes, "--state", s.url),
			"Error: VULTR_API_KEY is not set\n")
	}
	s.want(t, prodObjects)
}

// TestDeleteClusterOnAnUnsupportedProvider deletes the state alone of a cluster on a provider that tent cannot manage
// yet, and warns.
func TestDeleteClusterOnAnUnsupportedProvider(t *testing.T) {
	s := hetznerCluster(t)
	f := vultrfake.New()
	const (
		warning = "WARNING: tent cannot manage clusters on hetzner yet, so it made no cloud objects for cluster " +
			"prod; deleting its state only\n"
		plan = "- state prod/nodegroups/servers.yaml\n" +
			"- state prod/cluster.yaml\n" +
			"\n" +
			"State: 2 objects to delete.\n"
	)
	wantResult(t, runOn(t, f, "delete", "cluster", "prod", "--state", s.url), 0, plan, warning+deleteHint)
	got := runOn(t, f, "delete", "cluster", "prod", "--yes", "-o", "json", "--state", s.url)
	const planJSON = `{
  "applied": true,
  "nodes": [],
  "infrastructure": null,
  "state": [
    "prod/nodegroups/servers.yaml",
    "prod/cluster.yaml"
  ],
  "unsupportedProvider": "hetzner"
}
`
	wantResult(t, got, 0, planJSON, warning)
	s.want(t, map[string]string{})
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}

// builtCluster returns a store that holds the test cluster and a fake that holds what update cluster --yes built
// for it. Call it in a synctest bubble.
func builtCluster(t *testing.T) (state, *vultrfake.Fake) {
	t.Helper()
	s := withCluster(t)
	return s, buildCluster(t, s)
}

// buildCluster runs update --yes for the test cluster of s on a new Vultr fake, which it returns.
func buildCluster(t *testing.T, s state) *vultrfake.Fake {
	t.Helper()
	f := vultrfake.New()
	if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
		t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
	}
	return f
}

// firewallID returns the id of the fake's firewall group for role, server or client.
func firewallID(t *testing.T, f *vultrfake.Fake, role string) string {
	t.Helper()
	for _, g := range f.FirewallGroups() {
		if strings.HasPrefix(g.Description, "tent:cluster=prod;kind=firewall;role="+role+";") {
			return g.ID
		}
	}
	t.Fatalf("no firewall group for role %s", role)
	return ""
}

// teardown returns the plan of the delete of the test cluster that builtCluster built. A development build of tent,
// as in tests, writes no tent version.
func teardown(t *testing.T, f *vultrfake.Fake) string {
	t.Helper()
	return teardownOf(t, f, len(nodeNames))
}

// teardownOf returns the plan of the delete of the test cluster that builtCluster built, with its first n nodes left.
func teardownOf(t *testing.T, f *vultrfake.Fake, n int) string {
	t.Helper()
	var b strings.Builder
	for i, name := range nodeNames[:n] {
		fmt.Fprintf(&b, "- node %s (ID instance-%d)\n", name, i+1)
	}
	fmt.Fprintf(&b, "- vultr.FirewallGroup/prod-clients (ID %s)\n", firewallID(t, f, "client"))
	fmt.Fprintf(&b, "- vultr.FirewallGroup/prod-servers (ID %s)\n", firewallID(t, f, "server"))
	b.WriteString("- vultr.VPC/prod (ID vpc-1)\n" +
		"- state prod/cluster.completed.yaml\n" +
		"- state prod/nodegroups/servers.yaml\n" +
		"- state prod/nodegroups/workers.yaml\n" +
		"- state prod/nomad/bootstrapped\n" +
		"- state prod/secrets/acl-bootstrap-token\n" +
		"- state prod/secrets/gossip.key\n" +
		"- state prod/pki/ca-bundle.pem\n" +
		"- state prod/pki/private/ca.key\n" +
		"- state prod/cluster.yaml\n" +
		"\n")
	fmt.Fprintf(&b, "Nodes: %d to delete.\n", n)
	b.WriteString("Plan: 0 to create, 0 to update, 0 to replace, 3 to delete.\n" +
		"State: 9 objects to delete.\n")
	return b.String()
}

// TestDeleteClusterPlansTwice plans once without the lock and once under it before it deletes the first node: each
// plan reads the inventory once.
func TestDeleteClusterPlansTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		want := teardown(t, f) + "\nDeleted: 6 nodes, 3 infrastructure objects, 9 state objects.\n"
		before := len(f.Calls())

		got := runOn(t, f, "delete", "cluster", "prod", "--yes", "--state", s.url)

		if got.code != 0 || got.out != want {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and\n%s", got.code, got.out, want)
		}
		if n := countBefore(f.Calls()[before:], "ListSSHKeys", "DeleteInstance"); n != 2 {
			t.Errorf("delete --yes read the inventory %d times before the first delete, want 2", n)
		}
	})
}

// TestDeleteClusterPrintsThePlanItApplies prints the plan made under the lock when the cloud changes after the plan
// without the lock: a node goes once the first plan has listed the nodes.
func TestDeleteClusterPrintsThePlanItApplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		want := teardownOf(t, f, 5) + "\nDeleted: 5 nodes, 3 infrastructure objects, 9 state objects.\n"
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "ListSSHKeys" { // the inventory, after the list of the nodes
				f.SetHook(nil)
				if err := f.DeleteInstance(ctx, "instance-6"); err != nil {
					t.Errorf("DeleteInstance: %v", err)
				}
			}
			return next(ctx)
		})

		got := runOn(t, f, "delete", "cluster", "prod", "--yes", "--state", s.url)

		if got.code != 0 || got.out != want {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and\n%s", got.code, got.out, want)
		}
		wantInstances(t, f)
		s.want(t, map[string]string{})
	})
}

// TestDeleteClusterWithItsCloud previews the delete of a built cluster, then deletes its nodes, its infrastructure
// and its state, and prints each step on stderr and the plan it applied on stdout.
func TestDeleteClusterWithItsCloud(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		want := teardown(t, f)
		clients, servers := firewallID(t, f, "client"), firewallID(t, f, "server")
		wantResult(t, runOn(t, f, "delete", "cluster", "prod", "--state", s.url), 0, want, deleteHint)
		wantInstances(t, f, nodeNames...)

		got := runOn(t, f, "delete", "cluster", "prod", "--yes", "--state", s.url)

		deleted := want + "\nDeleted: 6 nodes, 3 infrastructure objects, 9 state objects.\n"
		if got.code != 0 || got.out != deleted {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and\n%s", got.code, got.out, deleted)
		}
		var wantNodes []string
		for i, n := range nodeNames {
			node := fmt.Sprintf("node %s (ID instance-%d)", n, i+1)
			wantNodes = append(wantNodes, "deleting "+node, "deleted "+node)
		}
		var wantInfra []string
		for _, obj := range []string{
			"vultr.FirewallGroup/prod-clients (ID " + clients + ")",
			"vultr.FirewallGroup/prod-servers (ID " + servers + ")",
			"vultr.VPC/prod (ID vpc-1)",
		} {
			wantInfra = append(wantInfra, "deleting "+obj, "deleted "+obj)
		}
		slices.Sort(wantInfra)
		lines := strings.Split(strings.TrimSuffix(got.errOut, "\n"), "\n")
		if len(lines) != len(wantNodes)+len(wantInfra) {
			t.Fatalf("stderr has %d lines, want %d:\n%s", len(lines), len(wantNodes)+len(wantInfra), got.errOut)
		}
		if diff := cmp.Diff(wantNodes, lines[:len(wantNodes)]); diff != "" {
			t.Errorf("the nodes' progress (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(wantInfra, slices.Sorted(slices.Values(lines[len(wantNodes):]))); diff != "" {
			t.Errorf("the infrastructure's progress (-want +got):\n%s", diff)
		}
		wantInstances(t, f)
		if len(f.VPCs()) != 0 || len(f.FirewallGroups()) != 0 {
			t.Errorf("the VPCs are %+v and the firewall groups %+v, want none", f.VPCs(), f.FirewallGroups())
		}
		s.want(t, map[string]string{})
	})
}

// TestDeleteClusterJSON prints the plan as JSON, and after the delete the plan it applied.
func TestDeleteClusterJSON(t *testing.T) {
	s := withCluster(t)
	const want = `{
  "nodes": [],
  "infrastructure": {
    "changes": [],
    "summary": {
      "create": 0,
      "update": 0,
      "replace": 0,
      "delete": 0
    }
  },
  "state": [
    "prod/nodegroups/servers.yaml",
    "prod/nodegroups/workers.yaml",
    "prod/cluster.yaml"
  ]
}
`
	wantResult(t, runOnCloud(t, "delete", "cluster", "prod", "-o", "json", "--state", s.url), 0, want, deleteHint)
	s.want(t, prodObjects)
	wantOK(t, runOnCloud(t, "delete", "cluster", "prod", "-o", "json", "--yes", "--state", s.url),
		"{\n  \"applied\": true,\n"+strings.TrimPrefix(want, "{\n"))
	s.want(t, map[string]string{})
}

// TestDeleteClusterProgressJSON prints the steps of the delete as JSON objects with -o json.
func TestDeleteClusterProgressJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		got := runOn(t, f, "delete", "cluster", "prod", "--yes", "-o", "json", "--state", s.url)
		if got.code != 0 {
			t.Fatalf("exit code = %d\n%s", got.code, got.errOut)
		}
		events := decodeProgress(t, got.errOut)
		if len(events) != 2*(len(nodeNames)+len(infraKeys)) {
			t.Fatalf("%d events, want %d:\n%s", len(events), 2*(len(nodeNames)+len(infraKeys)), got.errOut)
		}
		wantFirst := progressEvent{Type: "node", Step: "started", Action: "delete", Name: "prod-servers-0",
			ID: "instance-1"}
		if diff := cmp.Diff(wantFirst, events[0]); diff != "" {
			t.Errorf("the first event (-want +got):\n%s", diff)
		}
		wantLast := progressEvent{Type: "infrastructure", Event: "succeeded", Kind: "vultr.VPC", Name: "prod",
			Action: "delete", ID: "vpc-1"}
		if diff := cmp.Diff(wantLast, events[len(events)-1]); diff != "" {
			t.Errorf("the last event (-want +got):\n%s", diff)
		}
	})
}

// TestDeleteClusterWithoutItsSpec deletes the state alone of a cluster without cluster.yaml, and warns.
func TestDeleteClusterWithoutItsSpec(t *testing.T) {
	s := newState(t)
	s.put(t, serversPath, serversYAML)
	s.put(t, workersPath, workersYAML)
	f := vultrfake.New()
	const (
		warning = "WARNING: cluster prod has no cluster.yaml, so tent cannot tell its cloud; deleting its state only\n"
		plan    = "- state prod/nodegroups/servers.yaml\n" +
			"- state prod/nodegroups/workers.yaml\n" +
			"\n" +
			"State: 2 objects to delete.\n"
	)
	wantResult(t, runOn(t, f, "delete", "cluster", "prod", "--state", s.url), 0, plan, warning+deleteHint)
	wantResult(t, runOn(t, f, "delete", "cluster", "prod", "--yes", "--state", s.url), 0,
		plan+"\nDeleted: 2 state objects.\n", warning)
	s.want(t, map[string]string{})
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}

func TestDeleteClusterUnknownObjects(t *testing.T) {
	s := withCluster(t)
	s.put(t, "prod/notes.txt", "mine")
	const refused = "Error: cluster prod holds objects tent does not know: prod/notes.txt; delete them yourself or " +
		"use --force\n"
	wantError(t, runOnCloud(t, "delete", "cluster", "prod", "--state", s.url), refused)
	wantError(t, runOnCloud(t, "delete", "cluster", "prod", "--yes", "--state", s.url), refused)
	const plan = "- state prod/nodegroups/servers.yaml\n" +
		"- state prod/nodegroups/workers.yaml\n" +
		"- state prod/notes.txt\n" +
		"- state prod/cluster.yaml\n" +
		"\n" +
		"State: 4 objects to delete.\n"
	wantResult(t, runOnCloud(t, "delete", "cluster", "prod", "--force", "--state", s.url), 0, plan,
		"run with --yes --force to delete them\n")
	s.want(t, map[string]string{
		clusterPath: clusterYAML, serversPath: serversYAML, workersPath: workersYAML, "prod/notes.txt": "mine",
	})
	wantOK(t, runOnCloud(t, "delete", "cluster", "prod", "--yes", "--force", "--state", s.url),
		plan+"\nDeleted: 4 state objects.\n")
	s.want(t, map[string]string{})
}

func TestDeleteClusterMissing(t *testing.T) {
	s := withCluster(t)
	wantError(t, runOnCloud(t, "delete", "cluster", "dev", "--yes", "--state", s.url),
		"Error: cluster dev not found in "+s.url+"\n")
	s.want(t, prodObjects)
}

func TestDeleteClusterHelpSaysItDeletesTheCloud(t *testing.T) {
	got := runOnCloud(t, "delete", "cluster", "--help")
	const want = "Delete the cloud objects of the cluster named by NAME or --name, then its state"
	if got.code != 0 || !strings.Contains(got.out, want) {
		t.Errorf("exit code = %d, stdout\n%s\nwant 0 and it to hold %q", got.code, got.out, want)
	}
}

// losingStore removes the lock's lease after it writes or deletes one path, as state unlock --force by someone else
// would.
type losingStore struct {
	statestore.Store
	after string
}

func (s losingStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	v, err := s.Store.Put(ctx, p, data, opts)
	return v, s.lose(ctx, p, err)
}

func (s losingStore) Delete(ctx context.Context, p string) error {
	return s.lose(ctx, p, s.Store.Delete(ctx, p))
}

// lose removes the lock's lease when the change of p succeeded and p is the path after which the lock goes, and
// returns err otherwise.
func (s losingStore) lose(ctx context.Context, p string, err error) error {
	if err == nil && p == s.after {
		err = s.Store.Delete(ctx, "prod/lock")
	}
	return err
}

// TestDeleteClusterLosesItsLock prints what it deleted, and fails with an error that says the delete is saved.
func TestDeleteClusterLosesItsLock(t *testing.T) {
	s := withCluster(t)
	losing := func(st statestore.Store) statestore.Store { return losingStore{Store: st, after: clusterPath} }
	opts := &globalOptions{openStore: wrapped(losing), providers: onVultr(vultrfake.New())}
	wantResult(t, runWith(t, opts, "delete", "cluster", "prod", "--yes", "--state", s.url), 1, stateDeleted,
		"Error: the change is saved, but the lock of cluster prod was lost before tent released it\n")
	s.want(t, map[string]string{})
}
