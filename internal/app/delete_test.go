package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

const completedPath = "prod/cluster.completed.yaml"

// newCluster returns a service over a store with the test cluster, its completed spec and its tent version, and the
// Vultr fake that its providers reach, which holds nothing.
func newCluster(t *testing.T) (*app.Service, *vultrfake.Fake) {
	t.Helper()
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	put(t, svc.Store, completedPath, []byte("completed"))
	put(t, svc.Store, versionPath, []byte("v0.4.0\n"))
	f, _ := withCloud(svc)
	return svc, f
}

// newBuilt returns a service whose store holds the test cluster with the SSH keys ops and dev, built on the returned
// Vultr fake by an update of tent v0.5.0. Call it in a synctest bubble, where the update does not wait in real time.
func newBuilt(t *testing.T) (*app.Service, *vultrfake.Fake) {
	t.Helper()
	svc, f := newUpdate(t)
	svc.Version = "v0.5.0"
	mustUpdate(t, svc)
	return svc, f
}

// allState is every object of the test cluster as newCluster stores it, in the order DeleteCluster deletes them.
var allState = []string{completedPath, serversPath, workersPath, clusterPath, versionPath}

// builtState is every object of the test cluster as newBuilt leaves it, with its secrets, in the order DeleteCluster
// deletes them.
var builtState = []string{
	completedPath, serversPath, workersPath, aclPath, gossipPath, caBundlePath, caKeyPath, clusterPath, versionPath,
}

// builtNodes are the node deletes of the test cluster as newBuilt builds it.
var builtNodes = []app.NodeChange{
	nodeDelete("prod-servers-0", "instance-1"), nodeDelete("prod-servers-1", "instance-2"),
	nodeDelete("prod-servers-2", "instance-3"), nodeDelete("prod-workers-0", "instance-4"),
	nodeDelete("prod-workers-1", "instance-5"),
}

// builtInfra are the infrastructure deletes of the test cluster as newBuilt builds it.
var builtInfra = []string{
	"delete " + kindFirewall + "/prod-clients", "delete " + kindFirewall + "/prod-servers", "delete " + kindVPC + "/prod",
	"delete " + kindSSHKey + "/prod-" + opsFP, "delete " + kindSSHKey + "/prod-" + devFP,
}

// mustDelete deletes the test cluster and stops the test when it fails.
func mustDelete(t *testing.T, svc *app.Service) app.DeletePlan {
	t.Helper()
	plan, err := svc.DeleteCluster(t.Context(), "prod", true, false)
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if !plan.Applied {
		t.Error("the plan does not say it was applied")
	}
	return plan
}

// wantDeletePlan fails the test unless err is nil and the plan deletes these nodes, infrastructure objects, each
// "<action> <key>", and state paths.
func wantDeletePlan(t *testing.T, plan app.DeletePlan, err error, nodes []app.NodeChange, infra, state []string) {
	t.Helper()
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if diff := cmp.Diff(nodes, plan.Nodes); diff != "" {
		t.Errorf("the node deletes (-want +got):\n%s", diff)
	}
	wantEngineChanges(t, plan.Infra, infra...)
	if diff := cmp.Diff(state, plan.State); diff != "" {
		t.Errorf("the state deletes (-want +got):\n%s", diff)
	}
	if plan.CloudUnknown || plan.Unsupported != "" {
		t.Errorf("the plan says the cloud is unknown (%t) or unsupported (%q)", plan.CloudUnknown, plan.Unsupported)
	}
}

// owned returns the objects of f that belong to cluster by their tags or markers, each as "<kind> <id>".
func owned(f *vultrfake.Fake, cluster string) []string {
	marker := "tent:cluster=" + cluster + ";"
	var objs []string
	for _, in := range f.Instances() {
		if slices.Contains(in.Tags, cloud.LabelCluster+"="+cluster) {
			objs = append(objs, "instance "+in.ID)
		}
	}
	for _, k := range f.SSHKeys() {
		if strings.HasPrefix(k.Name, marker) {
			objs = append(objs, "ssh key "+k.ID)
		}
	}
	for _, v := range f.VPCs() {
		if strings.HasPrefix(v.Description, marker) {
			objs = append(objs, "vpc "+v.ID)
		}
	}
	for _, g := range f.FirewallGroups() {
		if strings.HasPrefix(g.Description, marker) {
			objs = append(objs, "firewall group "+g.ID)
		}
	}
	return objs
}

// callsSince returns the calls that reached f after the first n.
func callsSince(f *vultrfake.Fake, n int) []vultrfake.Call { return f.Calls()[n:] }

// deletesIn returns the deletes among calls, with the calls of one method in a row sorted by their argument: the
// engine deletes the objects of one kind in parallel.
func deletesIn(calls []vultrfake.Call) []vultrfake.Call {
	var deletes []vultrfake.Call
	for _, c := range calls {
		if strings.HasPrefix(c.Name, "Delete") {
			deletes = append(deletes, c)
		}
	}
	for start := 0; start < len(deletes); {
		end := start + 1
		for end < len(deletes) && deletes[end].Name == deletes[start].Name {
			end++
		}
		slices.SortFunc(deletes[start:end], func(a, b vultrfake.Call) int { return strings.Compare(a.Arg, b.Arg) })
		start = end
	}
	return deletes
}

// wantNoWrites fails the test unless every call of calls reads.
func wantNoWrites(t *testing.T, calls []vultrfake.Call) {
	t.Helper()
	for _, c := range calls {
		if !strings.HasPrefix(c.Name, "List") && !strings.HasPrefix(c.Name, "Get") && c.Name != "AvailablePlans" {
			t.Errorf("a call that writes: %s %s", c.Name, c.Arg)
		}
	}
}

func TestDeleteClusterStatePlan(t *testing.T) {
	svc, f := newCluster(t)
	put(t, svc.Store, lockPath, []byte("{}")) // the lock's lease goes when the lock is released
	plan, err := svc.DeleteCluster(t.Context(), "prod", false, false)
	wantDeletePlan(t, plan, err, nil, nil, allState)
	wantPaths(t, svc.Store, completedPath, clusterPath, lockPath, serversPath, workersPath, versionPath)
	wantNoWrites(t, f.Calls())
}

func TestDeleteClusterState(t *testing.T) {
	svc, _ := newCluster(t)
	plan, err := svc.DeleteCluster(t.Context(), "prod", true, false)
	wantDeletePlan(t, plan, err, nil, nil, allState)
	wantPaths(t, svc.Store)
}

// TestDeleteClusterAgain runs a delete again that stopped before the Cluster's spec and the tent version.
func TestDeleteClusterAgain(t *testing.T) {
	svc, _ := newCluster(t)
	for _, p := range []string{completedPath, serversPath, workersPath} {
		if err := svc.Store.Delete(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := svc.DeleteCluster(t.Context(), "prod", true, false)
	wantDeletePlan(t, plan, err, nil, nil, []string{clusterPath, versionPath})
	wantPaths(t, svc.Store)
}

// TestDeleteClusterUnknownState refuses objects in the state that tent does not know before it calls the cloud, and
// with force deletes them with the rest of the state.
func TestDeleteClusterUnknownState(t *testing.T) {
	unknown := []string{"prod/nodegroups/old/x.yaml", "prod/notes.txt", "prod/pki/ca.pem"}
	svc, f := newCluster(t)
	for _, p := range unknown {
		put(t, svc.Store, p, []byte("?"))
	}
	stored := list(t, svc.Store, "")
	for _, apply := range []bool{false, true} {
		_, err := svc.DeleteCluster(t.Context(), "prod", apply, false)
		wantError(t, err, "cluster prod holds objects tent does not know: prod/nodegroups/old/x.yaml, prod/notes.txt, "+
			"prod/pki/ca.pem; delete them yourself or use --force")
		wantPaths(t, svc.Store, stored...)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
	want := []string{
		completedPath, unknown[0], serversPath, workersPath, unknown[1], unknown[2], clusterPath, versionPath,
	}
	plan, err := svc.DeleteCluster(t.Context(), "prod", false, true)
	wantDeletePlan(t, plan, err, nil, nil, want)
	plan, err = svc.DeleteCluster(t.Context(), "prod", true, true)
	wantDeletePlan(t, plan, err, nil, nil, want)
	wantPaths(t, svc.Store)
}

// Store paths of the test cluster's secrets.
const (
	caKeyPath    = "prod/pki/private/ca.key"
	caBundlePath = "prod/pki/ca-bundle.pem"
	gossipPath   = "prod/secrets/gossip.key"
	aclPath      = "prod/secrets/acl-bootstrap-token"
)

// secretPaths are the test cluster's secrets in the order an update writes them.
var secretPaths = []string{caKeyPath, caBundlePath, gossipPath, aclPath}

// secretDeletes are the test cluster's secrets in the order DeleteCluster deletes them: the reverse of their writes,
// so that the CA bundle goes before its key.
var secretDeletes = []string{aclPath, gossipPath, caBundlePath, caKeyPath}

// TestDeleteClusterSecrets deletes the cluster's CA, gossip key and ACL bootstrap secret without force, after the
// node groups and the completed spec and before cluster.yaml and the tent version.
func TestDeleteClusterSecrets(t *testing.T) {
	svc, _ := newCluster(t)
	for _, p := range secretPaths {
		put(t, svc.Store, p, []byte("?"))
	}
	want := slices.Concat([]string{completedPath, serversPath, workersPath}, secretDeletes,
		[]string{clusterPath, versionPath})
	for _, apply := range []bool{false, true} {
		plan, err := svc.DeleteCluster(t.Context(), "prod", apply, false)
		wantDeletePlan(t, plan, err, nil, nil, want)
	}
	wantPaths(t, svc.Store)
}

// TestDeleteClusterUnknownObjectNextToTheSecrets refuses an object under pki/ that is not one of the secrets.
func TestDeleteClusterUnknownObjectNextToTheSecrets(t *testing.T) {
	const unknown = "prod/pki/private/old.key"
	svc, _ := newCluster(t)
	for _, p := range append(slices.Clone(secretPaths), unknown) {
		put(t, svc.Store, p, []byte("?"))
	}
	_, err := svc.DeleteCluster(t.Context(), "prod", true, false)
	wantError(t, err, "cluster prod holds objects tent does not know: "+unknown+"; delete them yourself or use --force")
	plan, err := svc.DeleteCluster(t.Context(), "prod", true, true)
	wantDeletePlan(t, plan, err, nil, nil, slices.Concat([]string{completedPath, serversPath, workersPath, unknown},
		secretDeletes, []string{clusterPath, versionPath}))
	wantPaths(t, svc.Store)
}

func TestDeleteClusterMissing(t *testing.T) {
	svc, f := newCluster(t)
	for _, apply := range []bool{false, true} {
		_, err := svc.DeleteCluster(t.Context(), "dev", apply, false)
		wantError(t, err, notFound(svc, "cluster dev"))
	}
	wantPaths(t, svc.Store, completedPath, clusterPath, serversPath, workersPath, versionPath)
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}

// TestDeleteClusterWithoutClusterSpec deletes the state of a cluster without cluster.yaml, such as one left from an
// interrupted create: tent cannot tell its cloud, so it deletes the state alone.
func TestDeleteClusterWithoutClusterSpec(t *testing.T) {
	svc, _ := newService(t)
	put(t, svc.Store, serversPath, encode(t, serversYAML))
	put(t, svc.Store, workersPath, encode(t, workersYAML))
	put(t, svc.Store, versionPath, []byte("v0.4.0\n"))
	svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
		t.Error("DeleteCluster looked up a provider")
		return nil, errors.New("no provider")
	}
	state := []string{serversPath, workersPath, versionPath}
	for _, apply := range []bool{false, true} {
		plan, err := svc.DeleteCluster(t.Context(), "prod", apply, false)
		if err != nil {
			t.Fatalf("DeleteCluster: %v", err)
		}
		if !plan.CloudUnknown || plan.Nodes != nil || plan.Infra != nil {
			t.Errorf("the plan has the cloud unknown %t, the nodes %v and the infrastructure %v; want true and none",
				plan.CloudUnknown, plan.Nodes, plan.Infra)
		}
		if diff := cmp.Diff(state, plan.State); diff != "" {
			t.Errorf("the state deletes (-want +got):\n%s", diff)
		}
	}
	wantPaths(t, svc.Store)
}

// TestDeleteClusterOnAnUnsupportedProvider deletes the state alone of a cluster on a provider that tent cannot manage
// yet: tent made no cloud objects there.
func TestDeleteClusterOnAnUnsupportedProvider(t *testing.T) {
	svc, _ := newService(t)
	hetzner := edit(t, edit(t, clusterYAML, "provider: vultr", "provider: hetzner"), "region: ams",
		"region: eu-central\n    zones: [fsn1, nbg1, hel1]")
	put(t, svc.Store, clusterPath, encode(t, hetzner))
	put(t, svc.Store, serversPath, encode(t, edit(t, serversYAML, "vc2-2c-4gb", "cx23")))
	var asked []v1alpha1.Provider
	svc.Providers = func(name v1alpha1.Provider) (cloud.Provider, error) {
		asked = append(asked, name)
		return nil, fmt.Errorf("look up the provider: %w", cloud.UnsupportedProvider(name))
	}
	state := []string{serversPath, clusterPath}
	for _, apply := range []bool{false, true} {
		plan, err := svc.DeleteCluster(t.Context(), "prod", apply, false)
		if err != nil {
			t.Fatalf("DeleteCluster: %v", err)
		}
		if plan.Unsupported != v1alpha1.ProviderHetzner || plan.CloudUnknown || plan.Nodes != nil ||
			plan.Infra != nil {
			t.Errorf("the plan has the unsupported provider %q, the cloud unknown %t, the nodes %v and the "+
				"infrastructure %v; want hetzner, false and none", plan.Unsupported, plan.CloudUnknown, plan.Nodes,
				plan.Infra)
		}
		if diff := cmp.Diff(state, plan.State); diff != "" {
			t.Errorf("the state deletes (-want +got):\n%s", diff)
		}
		if plan.Applied != apply {
			t.Errorf("Applied = %t, want %t", plan.Applied, apply)
		}
	}
	if len(asked) == 0 || slices.ContainsFunc(asked, func(p v1alpha1.Provider) bool { return p != "hetzner" }) {
		t.Errorf("looked up the providers %v, want hetzner", asked)
	}
	wantPaths(t, svc.Store)
}

func TestDeleteClusterFailsBeforeTheCloud(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers func(v1alpha1.Provider) (cloud.Provider, error)
		want      string
	}{
		{
			name: "a provider that tent cannot reach",
			providers: func(v1alpha1.Provider) (cloud.Provider, error) {
				return nil, errors.New("VULTR_API_KEY is not set")
			},
			want: "VULTR_API_KEY is not set",
		},
		{name: "no providers", want: "no cloud providers are set up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newCluster(t)
			svc.Providers = tc.providers
			stored := snapshot(t, svc.Store)
			for _, apply := range []bool{false, true} {
				_, err := svc.DeleteCluster(t.Context(), "prod", apply, false)
				wantError(t, err, tc.want)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

// TestDeleteClusterPlan plans the delete of a built cluster: every node, every infrastructure object and the state,
// with only reads.
func TestDeleteClusterPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		before := len(f.Calls())
		stored := snapshot(t, svc.Store)
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec
		svc.OnProgress = func(p app.Progress) { t.Errorf("progress without apply: %s", progressLine(p)) }

		plan, err := svc.DeleteCluster(t.Context(), "prod", false, false)

		wantDeletePlan(t, plan, err, builtNodes, builtInfra, builtState)
		if plan.Applied {
			t.Error("the plan says it was applied")
		}
		if len(rec.writes) != 0 {
			t.Errorf("writes to the store: %v", rec.writes)
		}
		wantSnapshot(t, svc.Store, stored)
		wantNoWrites(t, callsSince(f, before))
		wantNodes(t, f, allNodes...)
	})
}

// TestDeleteCluster deletes a built cluster next to another one: the nodes, then the firewall groups, the VPC and
// the SSH keys, then the state. The other cluster keeps everything.
func TestDeleteCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		devSpecs := make([]string, 0, 3)
		for _, doc := range []string{keyedClusterYAML, serversYAML, workersYAML} {
			devSpecs = append(devSpecs, strings.ReplaceAll(doc, "prod", "dev"))
		}
		mustCreate(t, svc, devSpecs...)
		if _, err := svc.Update(t.Context(), "dev", true); err != nil {
			t.Fatalf("Update dev: %v", err)
		}
		devObjects, devState := owned(f, "dev"), list(t, svc.Store, "dev/")
		if len(devObjects) != 10 {
			t.Fatalf("cluster dev owns %v, want 5 nodes, a VPC, 2 firewall groups and 2 SSH keys", devObjects)
		}
		clients, servers := firewallGroupID(t, f, "client"), firewallGroupID(t, f, "server")
		var vpcs, keys []string
		for _, v := range f.VPCs() {
			if strings.HasPrefix(v.Description, "tent:cluster=prod;") {
				vpcs = append(vpcs, v.ID)
			}
		}
		for _, k := range f.SSHKeys() {
			if strings.HasPrefix(k.Name, "tent:cluster=prod;") {
				keys = append(keys, k.ID)
			}
		}
		if len(vpcs) != 1 {
			t.Fatalf("cluster prod owns the VPCs %v, want one", vpcs)
		}
		slices.Sort(keys)
		log := withAPI(svc, f)
		before := len(f.Calls())
		var progress []string
		svc.OnProgress = func(p app.Progress) {
			progress = append(progress, progressLine(p))
			if n := len(list(t, svc.Store, "prod/")); n != len(builtState) {
				t.Errorf("%d objects of the state are left during the cloud's deletes, want all %d", n, len(builtState))
			}
		}

		plan := mustDelete(t, svc)

		wantDeletePlan(t, plan, nil, builtNodes, builtInfra, builtState)
		want := []vultrfake.Call{
			{Name: "DeleteInstance", Arg: "instance-1"}, {Name: "DeleteInstance", Arg: "instance-2"},
			{Name: "DeleteInstance", Arg: "instance-3"}, {Name: "DeleteInstance", Arg: "instance-4"},
			{Name: "DeleteInstance", Arg: "instance-5"},
		}
		fw := []string{clients, servers}
		slices.Sort(fw)
		for _, id := range fw {
			want = append(want, vultrfake.Call{Name: "DeleteFirewallGroup", Arg: id})
		}
		want = append(want, vultrfake.Call{Name: "DeleteVPC", Arg: vpcs[0]})
		for _, id := range keys {
			want = append(want, vultrfake.Call{Name: "DeleteSSHKey", Arg: id})
		}
		if diff := cmp.Diff(want, deletesIn(callsSince(f, before))); diff != "" {
			t.Errorf("the deletes (-want +got):\n%s", diff)
		}
		if left := owned(f, "prod"); len(left) != 0 {
			t.Errorf("the cloud still holds objects of cluster prod: %v", left)
		}
		if diff := cmp.Diff(devObjects, owned(f, "dev")); diff != "" {
			t.Errorf("the objects of cluster dev (-before +after):\n%s", diff)
		}
		if log.Len() != 0 {
			t.Errorf("the provider logged:\n%s", log)
		}
		if paths := list(t, svc.Store, "prod/"); len(paths) != 0 {
			t.Errorf("the state of cluster prod is left: %v", paths)
		}
		if diff := cmp.Diff(devState, list(t, svc.Store, "dev/")); diff != "" {
			t.Errorf("the state of cluster dev (-before +after):\n%s", diff)
		}
		wantLockFree(t, svc.Store)

		// The node steps come first, in order, then the infrastructure's events, in any order.
		var wantSteps []string
		for _, c := range builtNodes {
			wantSteps = append(wantSteps, nodeSteps("delete", c.Name)...)
		}
		split := min(len(wantSteps), len(progress))
		if diff := cmp.Diff(wantSteps, progress[:split]); diff != "" {
			t.Errorf("the node steps (-want +got):\n%s", diff)
		}
		var wantInfra []string
		for _, c := range builtInfra {
			action, key, _ := strings.Cut(c, " ")
			wantInfra = append(wantInfra, "infra started "+key+" "+action, "infra succeeded "+key+" "+action)
		}
		slices.Sort(wantInfra)
		if diff := cmp.Diff(wantInfra, slices.Sorted(slices.Values(progress[split:]))); diff != "" {
			t.Errorf("the infrastructure's progress (-want +got):\n%s", diff)
		}
	})
}

// TestDeleteClusterTellsThePlan tells OnDeletePlan the plan made under the lock, once, before the first change.
func TestDeleteClusterTellsThePlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newBuilt(t)
		var events []string
		var told []app.DeletePlan
		svc.OnDeletePlan = func(p app.DeletePlan) error {
			if h := holder(t, svc.Store); h == nil || h.Operation != "delete" {
				t.Errorf("the lock is held by %v while OnDeletePlan runs, want a delete", h)
			}
			told = append(told, p)
			events = append(events, "plan")
			return nil
		}
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }

		if _, err := svc.DeleteCluster(t.Context(), "prod", false, false); err != nil {
			t.Fatalf("DeleteCluster without apply: %v", err)
		}
		if len(told) != 0 {
			t.Errorf("OnDeletePlan was called %d times without apply, want none", len(told))
		}
		mustDelete(t, svc)
		if len(told) != 1 || len(events) < 2 || events[0] != "plan" {
			t.Fatalf("OnDeletePlan was called %d times, and the events start with %q; want once, first", len(told),
				events[:min(2, len(events))])
		}
		wantDeletePlan(t, told[0], nil, builtNodes, builtInfra, builtState)
		if told[0].Applied {
			t.Error("the plan told before the changes says it was applied")
		}
	})
}

// TestDeleteClusterStopsWhenThePlanCannotBeTold deletes nothing when OnDeletePlan fails, and returns its error.
func TestDeleteClusterStopsWhenThePlanCannotBeTold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		errClosed := errors.New("stdout is closed")
		svc.OnDeletePlan = func(app.DeletePlan) error { return errClosed }
		svc.OnProgress = func(p app.Progress) { t.Errorf("progress: %s", progressLine(p)) }
		stored := snapshot(t, svc.Store)
		before := len(f.Calls())

		plan, err := svc.DeleteCluster(t.Context(), "prod", true, false)

		if !errors.Is(err, errClosed) {
			t.Errorf("DeleteCluster = %v, want %v", err, errClosed)
		}
		if plan.Applied {
			t.Error("the plan says it was applied")
		}
		wantNoWrites(t, callsSince(f, before))
		wantSnapshot(t, svc.Store, stored)
		wantLockFree(t, svc.Store)
	})
}

// TestDeleteClusterRetriesTheVPC deletes a VPC that Vultr still counts attached to the deleted nodes at first.
func TestDeleteClusterRetriesTheVPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		vpc := f.VPCs()[0].ID
		f.Fail(t, "DeleteVPC", vultr.NewAPIError(http.MethodDelete, "/v2/vpcs/"+vpc, http.StatusBadRequest,
			"The following servers are attached to this VPC network: 10.64.0.3", 0), 1)
		progress := recordProgress(svc)

		mustDelete(t, svc)

		if n := countCalls(f, "DeleteVPC"); n != 2 {
			t.Errorf("%d VPC deletes, want 2", n)
		}
		if !slices.Contains(*progress, "infra retrying "+kindVPC+"/prod delete") {
			t.Errorf("the progress does not retry the VPC:\n%s", strings.Join(*progress, "\n"))
		}
		if left := owned(f, "prod"); len(left) != 0 {
			t.Errorf("the cloud still holds objects of cluster prod: %v", left)
		}
		wantPaths(t, svc.Store)
	})
}

// undeletable is a Vultr API on which the delete of one instance succeeds and leaves the instance listed.
type undeletable struct {
	*vultrfake.Fake
	id string
}

func (u undeletable) DeleteInstance(ctx context.Context, id string) error {
	if id == u.id {
		return nil
	}
	return u.Fake.DeleteInstance(ctx, id)
}

// TestDeleteClusterWaitsForTheNodes lists the nodes every 5 seconds until none is left, for up to 5 minutes, before
// it deletes the infrastructure.
func TestDeleteClusterWaitsForTheNodes(t *testing.T) {
	t.Run("a node that goes after 12 seconds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f := newBuilt(t)
			withAPI(svc, undeletable{Fake: f, id: "instance-1"})
			start := time.Now()
			gone := make(chan struct{})
			defer func() { <-gone }()
			go func() {
				defer close(gone)
				time.Sleep(12 * time.Second)
				if err := f.DeleteInstance(context.Background(), "instance-1"); err != nil {
					t.Errorf("DeleteInstance: %v", err)
				}
			}()
			progress := recordProgress(svc)

			mustDelete(t, svc)

			// One event when the wait starts, after the node deletes and before the infrastructure's, and none for
			// each list.
			var waits []int
			for i, line := range *progress {
				if strings.HasPrefix(line, "going ") {
					waits = append(waits, i)
				}
			}
			if want := 2 * len(builtNodes); len(waits) != 1 || waits[0] != want || (*progress)[want] != "going 1" {
				t.Errorf("the progress\n%s\nwant going 1 once, after the node deletes", strings.Join(*progress, "\n"))
			}

			// The list after 15 s is the first without the node. The store's own locks may add a few milliseconds.
			if d := time.Since(start); d < 15*time.Second || d >= 16*time.Second {
				t.Errorf("the delete took %v, want 15s", d)
			}
			if left := owned(f, "prod"); len(left) != 0 {
				t.Errorf("the cloud still holds objects of cluster prod: %v", left)
			}
			wantPaths(t, svc.Store)
		})
	})
	t.Run("a node that stays", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f := newBuilt(t)
			withAPI(svc, undeletable{Fake: f, id: "instance-1"})
			stored := snapshot(t, svc.Store)
			before := len(f.Calls())
			start := time.Now()

			_, err := svc.DeleteCluster(t.Context(), "prod", true, false)

			wantError(t, err, "wait for the nodes to go: still listed after 5m0s: prod-servers-0 (instance-1)")
			if d := time.Since(start); d < 5*time.Minute || d >= 5*time.Minute+time.Second {
				t.Errorf("the delete took %v, want 5m0s", d)
			}
			for _, c := range callsSince(f, before) {
				if strings.HasPrefix(c.Name, "Delete") && c.Name != "DeleteInstance" {
					t.Errorf("the delete went on to %s %s", c.Name, c.Arg)
				}
			}
			wantNodes(t, f, server(0))
			wantSnapshot(t, svc.Store, stored)
			wantLockFree(t, svc.Store)
		})
	})
}

// TestDeleteClusterDoesNotWaitForNodesThatAreGone tells nothing of a wait when the cloud lists no node after the
// deletes.
func TestDeleteClusterDoesNotWaitForNodesThatAreGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newBuilt(t)
		progress := recordProgress(svc)
		mustDelete(t, svc)
		going := func(line string) bool { return strings.HasPrefix(line, "going ") }
		if i := slices.IndexFunc(*progress, going); i >= 0 {
			t.Errorf("the progress tells of a wait: %s", (*progress)[i])
		}
	})
}

// TestDeleteClusterStopsAtAFailedNodeDelete keeps the rest of the cluster and its state, and the next delete
// finishes the job.
func TestDeleteClusterStopsAtAFailedNodeDelete(t *testing.T) {
	const wantErr = "delete node prod-servers-0 (instance-1): vultr: DELETE /v2/instances/instance-1: " +
		"500 Internal Server Error: Internal error."
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		f.Fail(t, "DeleteInstance", vultr.NewAPIError(http.MethodDelete, "/v2/instances/instance-1",
			http.StatusInternalServerError, "Internal error.", 0), 1)
		stored := snapshot(t, svc.Store)
		objects := owned(f, "prod")
		before := len(f.Calls())
		progress := recordProgress(svc)

		plan, err := svc.DeleteCluster(t.Context(), "prod", true, false)

		wantError(t, err, wantErr)
		if plan.Applied {
			t.Error("the plan of the failed delete says it was applied")
		}
		want := []string{"node started delete prod-servers-0", "node failed delete prod-servers-0: " + wantErr}
		if diff := cmp.Diff(want, *progress); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
		wantDeletes := []vultrfake.Call{{Name: "DeleteInstance", Arg: "instance-1"}}
		if diff := cmp.Diff(wantDeletes, deletesIn(callsSince(f, before))); diff != "" {
			t.Errorf("the deletes (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(objects, owned(f, "prod")); diff != "" {
			t.Errorf("the objects of cluster prod (-before +after):\n%s", diff)
		}
		wantSnapshot(t, svc.Store, stored)
		wantLockFree(t, svc.Store)

		plan = mustDelete(t, svc)
		wantDeletePlan(t, plan, nil, builtNodes, builtInfra, builtState)
		if left := owned(f, "prod"); len(left) != 0 {
			t.Errorf("the cloud still holds objects of cluster prod: %v", left)
		}
		wantPaths(t, svc.Store)
	})
}

// TestDeleteClusterWithInvalidSpecs deletes a cluster whose specs are invalid and whose machine type is not
// available: the delete checks neither.
func TestDeleteClusterWithInvalidSpecs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		put(t, svc.Store, serversPath, encode(t, edit(t, serversYAML, "size: 3", "size: 2")))
		f.SetAvailability("ams", "vc2-1c-1gb")

		mustDelete(t, svc)

		if left := owned(f, "prod"); len(left) != 0 {
			t.Errorf("the cloud still holds objects of cluster prod: %v", left)
		}
		wantPaths(t, svc.Store)
	})
}

// TestDeleteClusterInterrupted interrupts the delete as it starts the first node's, as Ctrl-C would.
func TestDeleteClusterInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newBuilt(t)
		stored := snapshot(t, svc.Store)
		ctx, interrupt := context.WithCancel(t.Context())
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Step == app.NodeStarted {
				interrupt()
			}
		}

		_, err := svc.DeleteCluster(ctx, "prod", true, false)

		wantError(t, err, "interrupted")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false", err)
		}
		wantNodes(t, f, allNodes...)
		wantSnapshot(t, svc.Store, stored)
		wantLockFree(t, svc.Store)
	})
}
