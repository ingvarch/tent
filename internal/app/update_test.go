package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// Public keys of the update tests, each with the first 8 hex digits of its SHA-256 digest, which name its Vultr SSH
// key.
const (
	opsKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example"
	opsFP  = "8ba890ed"
	devKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINrldaS6ZmTkyJJJE5myvCxlWGxGxN3eRFaW+YJPfMqb dev@example"
	devFP  = "ae2316fa"
)

// keyedClusterYAML is the test cluster prod with the SSH keys ops and dev.
const keyedClusterYAML = clusterYAML + "  sshKeys:\n  - " + opsKey + "\n  - " + devKey + "\n"

// placeholderUserData is what the nodes boot with.
const placeholderUserData = "#cloud-config\npackage_update: false\npackage_upgrade: false\n"

// withCloud gives svc providers that reach a new Vultr fake, and returns the fake and the buffer that the provider
// logs to as JSON. Every provider other than vultr is unknown.
func withCloud(svc *app.Service) (*vultrfake.Fake, *bytes.Buffer) {
	f := vultrfake.New()
	return f, withAPI(svc, f)
}

// withAPI gives svc providers that reach Vultr through api, and returns the buffer that the provider logs to as JSON.
// Every provider other than vultr is unknown.
func withAPI(svc *app.Service, api vultr.API) *bytes.Buffer {
	var log bytes.Buffer
	p := vultr.New(api, vultr.WithLogger(slog.New(slog.NewJSONHandler(&log, nil))))
	svc.Providers = func(name v1alpha1.Provider) (cloud.Provider, error) {
		if name != v1alpha1.ProviderVultr {
			return nil, fmt.Errorf("unknown provider %q", name)
		}
		return p, nil
	}
	return &log
}

// newUpdate returns a service whose store holds the test cluster with the SSH keys ops and dev, its servers and its
// workers, and whose providers reach the returned Vultr fake. Outside a synctest bubble, the provider waits for new
// nodes in real time.
func newUpdate(t *testing.T) (*app.Service, *vultrfake.Fake) {
	t.Helper()
	svc, _ := newService(t)
	mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
	f, _ := withCloud(svc)
	return svc, f
}

// mustUpdate applies the update of the test cluster and stops the test when it fails.
func mustUpdate(t *testing.T, svc *app.Service) app.UpdatePlan {
	t.Helper()
	plan, err := svc.Update(t.Context(), "prod", true)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return plan
}

// wantConverged fails the test unless a plan of the test cluster's update has no changes.
func wantConverged(t *testing.T, svc *app.Service) {
	t.Helper()
	plan, err := svc.Update(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Update without apply: %v", err)
	}
	if plan.HasChanges() {
		t.Errorf("the plan after the update has changes:\n%s", planText(t, plan))
	}
}

// node is an instance of the fake as the tests see it: its name, its region, its tent tags without the operation id,
// and its user data.
type node struct {
	Name, Zone string
	Tags       []string
	UserData   string
}

// nodes returns the fake's instances in creation order. It fails the test unless each carries an operation id of its
// own.
func nodes(t *testing.T, f *vultrfake.Fake) []node {
	t.Helper()
	var out []node
	ops := map[string]bool{}
	for _, in := range f.Instances() {
		n := node{Name: in.Hostname, Zone: in.Region}
		op := opOf(in.Tags)
		for _, tag := range in.Tags {
			if !strings.HasPrefix(tag, cloud.LabelOp+"=") {
				n.Tags = append(n.Tags, tag)
			}
		}
		if !cloud.ValidOpID(op) || ops[op] {
			t.Errorf("instance %s has the operation id %q; want one of its own", in.Hostname, op)
		}
		ops[op] = true
		data, err := base64.StdEncoding.DecodeString(f.UserData(in.ID))
		if err != nil {
			t.Errorf("the user data of instance %s: %v", in.Hostname, err)
		}
		n.UserData = string(data)
		out = append(out, n)
	}
	return out
}

// wantNode is the node index of the test cluster's group, whose role is role.
func wantNode(group string, role v1alpha1.Role, index int) node {
	return node{
		Name: fmt.Sprintf("prod-%s-%d", group, index), Zone: "ams",
		Tags:     []string{"tent/cluster=prod", "tent/nodegroup=" + group, "tent/role=" + string(role)},
		UserData: placeholderUserData,
	}
}

func server(index int) node { return wantNode("servers", v1alpha1.RoleServer, index) }

func worker(index int) node { return wantNode("workers", v1alpha1.RoleClient, index) }

// allNodes are the nodes of the test cluster in the order the update creates them.
var allNodes = []node{server(0), server(1), server(2), worker(0), worker(1)}

// wantNodes fails the test unless the fake holds these instances, in creation order.
func wantNodes(t *testing.T, f *vultrfake.Fake, want ...node) {
	t.Helper()
	if diff := cmp.Diff(want, nodes(t, f)); diff != "" {
		t.Errorf("the instances (-want +got):\n%s", diff)
	}
}

// createOf is the planned create of the node index of the test cluster's group, whose role is role.
func createOf(group string, role v1alpha1.Role, index int) app.NodeChange {
	return app.NodeChange{
		Action: app.NodeCreate, Name: fmt.Sprintf("prod-%s-%d", group, index), Group: group, Role: role, Zone: "ams",
		MachineType: "vc2-2c-4gb", Image: v1alpha1.DefaultImage,
	}
}

// deleteOf is the planned delete of the node name, the instance id, for reason.
func deleteOf(name, id, reason string) app.NodeChange {
	return app.NodeChange{Action: app.NodeDelete, Name: name, ID: id, Reason: reason}
}

func wantNodeChanges(t *testing.T, plan app.UpdatePlan, want ...app.NodeChange) {
	t.Helper()
	if diff := cmp.Diff(want, plan.Nodes); diff != "" {
		t.Errorf("the node changes (-want +got):\n%s", diff)
	}
}

// wantInfraChanges fails the test unless the plan's infrastructure changes are these, each "<action> <key>", in
// order.
func wantInfraChanges(t *testing.T, plan app.UpdatePlan, want ...string) {
	t.Helper()
	wantEngineChanges(t, plan.Infra, want...)
}

// wantEngineChanges fails the test unless the changes of the infrastructure plan p, nil for none, are these, each
// "<action> <key>", in order.
func wantEngineChanges(t *testing.T, p *engine.Plan, want ...string) {
	t.Helper()
	var got []string
	if p != nil {
		for _, c := range p.Changes() {
			got = append(got, c.Action.String()+" "+c.String()) // the key's String
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the infrastructure changes (-want +got):\n%s", diff)
	}
}

// progressLine returns a line for p: "infra <type> <key> <action>" for an infrastructure event, "going <n>" for the
// wait for n nodes to go, and "node <step> <action> <name>" for a node step, with the error of a failed step.
func progressLine(p app.Progress) string {
	switch {
	case p.Infra != nil:
		return fmt.Sprintf("infra %s %s %s", p.Infra.Type, p.Infra.Key, p.Infra.Action)
	case p.Going > 0:
		return fmt.Sprintf("going %d", p.Going)
	}
	line := fmt.Sprintf("node %s %s %s", p.Step, p.Node.Action, p.Node.Name)
	if p.Err != nil {
		line += ": " + p.Err.Error()
	}
	return line
}

// recordProgress makes svc record its progress as lines, and returns them.
func recordProgress(svc *app.Service) *[]string {
	var lines []string
	svc.OnProgress = func(p app.Progress) { lines = append(lines, progressLine(p)) }
	return &lines
}

// nodeSteps returns the progress lines of a node change that succeeded: started, then done.
func nodeSteps(action, name string) []string {
	return []string{"node started " + action + " " + name, "node done " + action + " " + name}
}

// fullSpec returns what the completed spec holds: the test cluster's specs with every default.
func fullSpec(t *testing.T, svc *app.Service) []byte {
	t.Helper()
	objs, err := svc.Get(t.Context(), "prod", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, err := spec.Encode(objs)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return data
}

// wantLockFree fails the test unless nobody holds the test cluster's lock.
func wantLockFree(t *testing.T, s statestore.Store) {
	t.Helper()
	if h := holder(t, s); h != nil {
		t.Errorf("Holder = %v; want nil: the lock is free", h)
	}
}

// holder returns the lease of the holder of the test cluster's lock, or nil when the lock is free.
func holder(t *testing.T, s statestore.Store) *statestore.Lease {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	lk, _, err := statestore.NewLocker(t.Context(), s, l, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := lk.Holder(t.Context())
	if err != nil {
		t.Fatalf("Holder: %v", err)
	}
	return h
}

// writeLog records the path of every write to the store, the lock's included.
type writeLog struct {
	statestore.Store
	mu     sync.Mutex
	writes []string
}

func (w *writeLog) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	w.record(p)
	return w.Store.Put(ctx, p, data, opts)
}

func (w *writeLog) Delete(ctx context.Context, p string) error {
	w.record(p)
	return w.Store.Delete(ctx, p)
}

func (w *writeLog) record(p string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, p)
}

// wantOnlyReads fails the test unless every call that reached the fake reads.
func wantOnlyReads(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	wantNoWrites(t, f.Calls())
}

// firewallGroupID returns the id of the test cluster's firewall group for role, server or client, in f. It stops the
// test when f has none.
func firewallGroupID(t *testing.T, f *vultrfake.Fake, role string) string {
	t.Helper()
	for _, g := range f.FirewallGroups() {
		if strings.HasPrefix(g.Description, "tent:cluster=prod;kind=firewall;role="+role+";") {
			return g.ID
		}
	}
	t.Fatalf("no firewall group for role %s", role)
	return ""
}

// opOf returns the operation id in the tags of an instance.
func opOf(tags []string) string {
	for _, tag := range tags {
		if op, ok := strings.CutPrefix(tag, cloud.LabelOp+"="); ok {
			return op
		}
	}
	return ""
}

// countCalls returns how many calls of the vultr.API method name reached f.
func countCalls(f *vultrfake.Fake, name string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Name == name {
			n++
		}
	}
	return n
}

// The infrastructure changes of the test cluster on an empty cloud.
var infraCreates = []string{
	"create " + kindSSHKey + "/prod-" + opsFP, "create " + kindSSHKey + "/prod-" + devFP, "create " + kindVPC + "/prod",
	"create " + kindFirewall + "/prod-servers", "create " + kindFirewall + "/prod-clients",
}

// allCreates are the node changes of the test cluster on an empty cloud.
var allCreates = []app.NodeChange{
	createOf("servers", v1alpha1.RoleServer, 0), createOf("servers", v1alpha1.RoleServer, 1),
	createOf("servers", v1alpha1.RoleServer, 2),
	createOf("workers", v1alpha1.RoleClient, 0), createOf("workers", v1alpha1.RoleClient, 1),
}

func TestUpdatePlan(t *testing.T) {
	svc, f := newUpdate(t)
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec
	svc.OnProgress = func(p app.Progress) { t.Errorf("progress without apply: %s", progressLine(p)) }

	plan, err := svc.Update(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	wantInfraChanges(t, plan, infraCreates...)
	wantNodeChanges(t, plan, allCreates...)
	if plan.Applied {
		t.Error("the plan says it was applied")
	}
	if !plan.Completed {
		t.Error("the plan does not write the completed spec, which the store lacks")
	}
	if len(rec.writes) != 0 {
		t.Errorf("writes to the store: %v", rec.writes)
	}
	wantOnlyReads(t, f)
	wantNodes(t, f)
}

func TestUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		svc.Version = "v0.4.0"
		mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
		f, log := withCloud(svc)
		svc.Version = "v0.5.0"
		progress := recordProgress(svc)

		plan := mustUpdate(t, svc)

		wantInfraChanges(t, plan, infraCreates...)
		wantNodeChanges(t, plan, allCreates...)
		if !plan.Applied {
			t.Error("the plan does not say it was applied")
		}
		var keys []string
		for _, k := range f.SSHKeys() {
			keys = append(keys, k.SSHKey)
		}
		wantKeys := []string{opsKey, devKey}
		slices.Sort(keys)
		slices.Sort(wantKeys)
		if diff := cmp.Diff(wantKeys, keys); diff != "" {
			t.Errorf("the SSH keys (-want +got):\n%s", diff)
		}
		if vpcs := f.VPCs(); len(vpcs) != 1 || vpcs[0].Region != "ams" {
			t.Errorf("the VPCs are %+v, want one in ams", vpcs)
		}
		if groups := f.FirewallGroups(); len(groups) != 2 {
			t.Errorf("the firewall groups are %+v, want the servers' and the clients'", groups)
		}
		wantNodes(t, f, allNodes...)
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
		wantStored(t, svc.Store, versionPath, []byte("v0.5.0\n"))
		wantLockFree(t, svc.Store)
		if log.Len() != 0 {
			t.Errorf("the provider logged:\n%s", log)
		}

		// The infrastructure's events come first, in any order, then the node steps in order.
		split := slices.IndexFunc(*progress, func(line string) bool { return strings.HasPrefix(line, "node ") })
		if split < 0 {
			t.Fatalf("no node steps in the progress:\n%s", strings.Join(*progress, "\n"))
		}
		infra := slices.Sorted(slices.Values((*progress)[:split]))
		var wantInfra []string
		for _, c := range infraCreates {
			action, key, _ := strings.Cut(c, " ")
			wantInfra = append(wantInfra, "infra started "+key+" "+action, "infra succeeded "+key+" "+action)
		}
		slices.Sort(wantInfra)
		if diff := cmp.Diff(wantInfra, infra); diff != "" {
			t.Errorf("the infrastructure's progress (-want +got):\n%s", diff)
		}
		var wantSteps []string
		for _, n := range allNodes {
			wantSteps = append(wantSteps, nodeSteps("create", n.Name)...)
		}
		if diff := cmp.Diff(wantSteps, (*progress)[split:]); diff != "" {
			t.Errorf("the node steps (-want +got):\n%s", diff)
		}

		// A second update changes nothing, and needs no lock for that.
		calls := len(f.Calls())
		held := holdLock(t, svc.Store)
		plan = mustUpdate(t, svc)
		if plan.HasChanges() || !plan.Applied {
			t.Errorf("the second update has changes or was not applied (%t):\n%s", plan.Applied, planText(t, plan))
		}
		release(t, held)
		for _, c := range f.Calls()[calls:] {
			if !strings.HasPrefix(c.Name, "List") && c.Name != "AvailablePlans" {
				t.Errorf("the second update called %s %s", c.Name, c.Arg)
			}
		}
	})
}

// TestUpdateWritesTheCompletedSpec runs an update whose only change is the completed spec: it takes the lock, raises
// the tent version and writes the spec, and only reads the cloud.
func TestUpdateWritesTheCompletedSpec(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, svc *app.Service)
	}{
		{"a spec change that touches no cloud object", func(t *testing.T, svc *app.Service) {
			mustReplace(t, svc, keyedClusterYAML+"  nomad:\n    version: 2.0.7\n")
		}},
		{"an update that stopped before it wrote the completed spec", func(t *testing.T, svc *app.Service) {
			if err := svc.Store.Delete(t.Context(), completedPath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f := newUpdate(t)
				mustUpdate(t, svc)
				tc.change(t, svc)
				before := len(f.Calls())
				rec := &writeLog{Store: svc.Store}
				svc.Store, svc.Version = rec, "v0.5.0"
				progress := recordProgress(svc)

				plan := mustUpdate(t, svc)

				if got := planText(t, plan); got != "State: cluster.completed.yaml will be written.\n" {
					t.Errorf("the plan is\n%s\nwant the completed spec alone", got)
				}
				if diff := cmp.Diff([]string{lockPath, versionPath, completedPath, lockPath}, rec.writes); diff != "" {
					t.Errorf("writes to the store (-want +got):\n%s", diff)
				}
				for _, c := range f.Calls()[before:] {
					if !strings.HasPrefix(c.Name, "List") && c.Name != "AvailablePlans" {
						t.Errorf("the update called %s %s", c.Name, c.Arg)
					}
				}
				if len(*progress) != 0 {
					t.Errorf("progress: %v", *progress)
				}
				wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
				wantConverged(t, svc)
			})
		})
	}
}

// TestUpdateRepairsANode recreates a node that was deleted by hand. The specs are as last applied, so the completed
// spec is not written again.
func TestUpdateRepairsANode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		if err := f.DeleteInstance(t.Context(), "instance-5"); err != nil {
			t.Fatal(err)
		}
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec

		plan := mustUpdate(t, svc)

		wantNodeChanges(t, plan, createOf("workers", v1alpha1.RoleClient, 1))
		if plan.Completed {
			t.Error("the plan writes the completed spec, which has not changed")
		}
		if slices.Contains(rec.writes, completedPath) {
			t.Errorf("the update wrote the completed spec: %v", rec.writes)
		}
		wantNodes(t, f, allNodes...)
		wantConverged(t, svc)
	})
}

func TestUpdateScales(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)

		// Down: the newest worker goes.
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 1"))
		plan := mustUpdate(t, svc)
		wantInfraChanges(t, plan)
		wantNodeChanges(t, plan, deleteOf("prod-workers-1", "instance-5", "surplus"))
		wantNodes(t, f, server(0), server(1), server(2), worker(0))
		wantConverged(t, svc)

		// Up: the lowest free index comes back.
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))
		plan = mustUpdate(t, svc)
		wantNodeChanges(t, plan, createOf("workers", v1alpha1.RoleClient, 1), createOf("workers", v1alpha1.RoleClient, 2))
		wantNodes(t, f, server(0), server(1), server(2), worker(0), worker(1), worker(2))
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
		wantConverged(t, svc)
	})
}

// TestUpdateRemovesTheLastClientGroup removes the only client group: one update deletes its nodes, then the clients'
// firewall group, which Vultr's guard keeps while nodes use it.
func TestUpdateRemovesTheLastClientGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		clients := firewallGroupID(t, f, "client")
		if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
			t.Fatal(err)
		}
		before := len(f.Calls())
		progress := recordProgress(svc)

		plan := mustUpdate(t, svc)

		wantInfraChanges(t, plan, "delete "+kindFirewall+"/prod-clients")
		wantNodeChanges(t, plan,
			deleteOf("prod-workers-0", "instance-4", "not in the spec"),
			deleteOf("prod-workers-1", "instance-5", "not in the spec"))
		want := slices.Concat(nodeSteps("delete", "prod-workers-0"), nodeSteps("delete", "prod-workers-1"), []string{
			"infra started " + kindFirewall + "/prod-clients delete",
			"infra succeeded " + kindFirewall + "/prod-clients delete",
		})
		if diff := cmp.Diff(want, *progress); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
		var deletes []vultrfake.Call
		for _, c := range f.Calls()[before:] {
			if strings.HasPrefix(c.Name, "Delete") {
				deletes = append(deletes, c)
			}
		}
		wantDeletes := []vultrfake.Call{
			{Name: "DeleteInstance", Arg: "instance-4"}, {Name: "DeleteInstance", Arg: "instance-5"},
			{Name: "DeleteFirewallGroup", Arg: clients},
		}
		if diff := cmp.Diff(wantDeletes, deletes); diff != "" {
			t.Errorf("the deletes (-want +got):\n%s", diff)
		}
		wantNodes(t, f, server(0), server(1), server(2))
		if groups := f.FirewallGroups(); len(groups) != 1 || groups[0].ID == clients {
			t.Errorf("the firewall groups are %+v, want the servers' alone", groups)
		}
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
		wantConverged(t, svc)
	})
}

// TestUpdateStopsAtAFailedCreate fails a node's create while the plan also deletes an SSH key: the update stops, and
// the next one finishes the job.
func TestUpdateStopsAtAFailedCreate(t *testing.T) {
	const wantErr = "create node prod-workers-2 of cluster prod: vultr: POST /v2/instances: 400 Bad Request: " +
		"Invalid plan."
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		completed := get(t, svc.Store, completedPath)
		mustReplace(t, svc, edit(t, keyedClusterYAML, "  - "+devKey+"\n", ""), edit(t, workersYAML, "size: 2", "size: 3"))
		f.Fail(t, "CreateInstance",
			vultr.NewAPIError(http.MethodPost, "/v2/instances", http.StatusBadRequest, "Invalid plan.", 0), 1)
		progress := recordProgress(svc)

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, wantErr)
		if plan.Applied {
			t.Error("the plan of the failed update says it was applied")
		}
		wantInfraChanges(t, plan, "delete "+kindSSHKey+"/prod-"+devFP)
		wantNodeChanges(t, plan, createOf("workers", v1alpha1.RoleClient, 2))
		want := []string{"node started create prod-workers-2", "node failed create prod-workers-2: " + wantErr}
		if diff := cmp.Diff(want, *progress); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
		if keys := f.SSHKeys(); len(keys) != 2 {
			t.Errorf("%d SSH keys, want both: the delete waits for the next update", len(keys))
		}
		wantNodes(t, f, allNodes...)
		wantStored(t, svc.Store, completedPath, completed)
		wantLockFree(t, svc.Store)

		plan = mustUpdate(t, svc)
		wantInfraChanges(t, plan, "delete "+kindSSHKey+"/prod-"+devFP)
		wantNodeChanges(t, plan, createOf("workers", v1alpha1.RoleClient, 2))
		if keys := f.SSHKeys(); len(keys) != 1 || keys[0].SSHKey != opsKey {
			t.Errorf("the SSH keys are %+v, want ops alone", keys)
		}
		wantNodes(t, f, slices.Concat(allNodes, []node{worker(2)})...)
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
		wantConverged(t, svc)
	})
}

// TestUpdateReportsTheMachines reports the machine of each node it created: its ID and its private address.
func TestUpdateReportsTheMachines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		var steps []app.Progress
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil {
				steps = append(steps, p)
			}
		}

		mustUpdate(t, svc)

		var got, want []string
		for _, p := range steps {
			got = append(got, fmt.Sprintf("%s %s %s %s", p.Step, p.Node.Name, p.Instance.ID, p.Instance.PrivateIP))
		}
		for i, n := range allNodes {
			want = append(want, "started "+n.Name+"  invalid IP",
				fmt.Sprintf("done %s instance-%d 10.64.0.%d", n.Name, i+1, i+3))
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("the node steps (-want +got):\n%s", diff)
		}
	})
}

// pendingWorker leaves the test cluster built on f with its worker prod-workers-1 created anew but not ready yet, as
// an update that stopped while the node booted leaves it. The next update waits for it and changes nothing else.
func pendingWorker(t *testing.T, svc *app.Service, f *vultrfake.Fake) {
	t.Helper()
	mustUpdate(t, svc)
	if err := f.DeleteInstance(t.Context(), "instance-5"); err != nil {
		t.Fatal(err)
	}
	f.SetBootReads(t, 100, 100)
	f.Fail(t, "GetInstance", instanceLost("instance-6"), 1)
	if _, err := svc.Update(t.Context(), "prod", true); err == nil {
		t.Fatal("Update succeeded, want the failed read")
	}
}

// instanceLost is the error of a read of the instance id that Vultr refuses.
func instanceLost(id string) error {
	return vultr.NewAPIError(http.MethodGet, "/v2/instances/"+id, http.StatusBadRequest, "Invalid instance.", 0)
}

// TestUpdateReportsTheMachineItWaitedFor reports the machine of a node that it waited for, which has the address that
// the deleted worker left free.
func TestUpdateReportsTheMachineItWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		pendingWorker(t, svc, f)
		var done []string
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Step == app.NodeDone {
				done = append(done, fmt.Sprintf("%s %s %s %s", p.Node.Action, p.Node.Name, p.Instance.ID,
					p.Instance.PrivateIP))
			}
		}

		mustUpdate(t, svc)

		if diff := cmp.Diff([]string{"wait prod-workers-1 instance-6 10.64.0.7"}, done); diff != "" {
			t.Errorf("the done steps (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateReportsTheProvidersErrorOfAFailedWait reports the provider's own error for a failed wait, and returns it
// with the node that the update waited for.
func TestUpdateReportsTheProvidersErrorOfAFailedWait(t *testing.T) {
	const cause = "create node prod-workers-1 of cluster prod: wait for instance instance-6: vultr: GET " +
		"/v2/instances/instance-6: 400 Bad Request: Invalid instance."
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		pendingWorker(t, svc, f)
		f.Fail(t, "GetInstance", instanceLost("instance-6"), 1)
		progress := recordProgress(svc)

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "wait for node prod-workers-1: "+cause)
		want := []string{"node started wait prod-workers-1", "node failed wait prod-workers-1: " + cause}
		if diff := cmp.Diff(want, *progress); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateLostCreateAnswer loses the answer to a node's create: the provider finds the node by its operation id,
// and no node is created twice.
func TestUpdateLostCreateAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		f.LoseResponse(t, "CreateInstance", 1)
		mustUpdate(t, svc)
		wantNodes(t, f, allNodes...)
		if n := countCalls(f, "CreateInstance"); n != len(allNodes) {
			t.Errorf("%d creates, want %d", n, len(allNodes))
		}
		wantConverged(t, svc)
	})
}

// TestUpdateWaitsForNodes runs an update after one that stopped while its nodes booted: it waits for them, and creates
// none again.
func TestUpdateWaitsForNodes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		f.SetBootReads(t, 100, 100)
		f.Fail(t, "GetInstance", vultr.NewAPIError(http.MethodGet, "/v2/instances/instance-1",
			http.StatusBadRequest, "Invalid instance.", 0), 1)
		if _, err := svc.Update(t.Context(), "prod", true); err == nil {
			t.Fatal("Update succeeded, want the failed read")
		}
		wantNodes(t, f, server(0))

		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wait := createOf("servers", v1alpha1.RoleServer, 0)
		wait.Action, wait.ID, wait.Op = app.NodeWait, "instance-1", opOf(f.Instances()[0].Tags)
		wantNodeChanges(t, plan, slices.Concat(allCreates[1:], []app.NodeChange{wait})...)

		f.SetBootReads(t, 1, 2)
		mustUpdate(t, svc)
		wantNodes(t, f, allNodes...)
		if n := countCalls(f, "CreateInstance"); n != len(allNodes) {
			t.Errorf("%d creates, want %d", n, len(allNodes))
		}
		wantConverged(t, svc)
	})
}

// TestUpdateGivesEachNodeTenMinutes has the provider read a new node every 5 seconds until it is ready.
func TestUpdateGivesEachNodeTenMinutes(t *testing.T) {
	t.Run("nodes that take 8 minutes each", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f := newUpdate(t)
			f.SetBootReads(t, 96, 96) // ready at the read after 480 s
			start := time.Now()
			mustUpdate(t, svc)
			if d, want := time.Since(start), time.Duration(len(allNodes))*8*time.Minute; d < want {
				t.Errorf("the update took %v, want %v or more", d, want)
			}
			wantNodes(t, f, allNodes...)
		})
	})
	t.Run("a node that takes longer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f := newUpdate(t)
			f.SetBootReads(t, 200, 200) // ready after 1000 s
			start := time.Now()
			_, err := svc.Update(t.Context(), "prod", true)
			// The deadline and a read are due at once, so the error names the read or not.
			const prefix = "create node prod-servers-0 of cluster prod: wait for instance instance-1: "
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want one that starts with %q and matches context.DeadlineExceeded", err, prefix)
			}
			// The store's own locks may add a few milliseconds.
			if d := time.Since(start); d < 10*time.Minute || d >= 10*time.Minute+time.Second {
				t.Errorf("the update took %v, want 10m0s", d)
			}
			wantNodes(t, f, server(0))
			wantLockFree(t, svc.Store)
		})
	})
}

func TestUpdateFailsBeforeTheCloud(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, svc *app.Service, f *vultrfake.Fake)
		check   func(t *testing.T, err error)
	}{
		{
			name: "an invalid spec",
			prepare: func(t *testing.T, svc *app.Service, _ *vultrfake.Fake) {
				put(t, svc.Store, serversPath, encode(t, edit(t, serversYAML, "size: 3", "size: 2")))
			},
			check: func(t *testing.T, err error) {
				wantFieldErrors(t, err, v1alpha1.FieldError{
					Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server",
				})
			},
		},
		{
			name: "a provider that tent cannot reach",
			prepare: func(_ *testing.T, svc *app.Service, _ *vultrfake.Fake) {
				svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
					return nil, errors.New("VULTR_API_KEY is not set")
				}
			},
			check: func(t *testing.T, err error) { wantError(t, err, "VULTR_API_KEY is not set") },
		},
		{
			name:    "no providers",
			prepare: func(_ *testing.T, svc *app.Service, _ *vultrfake.Fake) { svc.Providers = nil },
			check:   func(t *testing.T, err error) { wantError(t, err, "no cloud providers are set up") },
		},
		{
			name: "a failed preflight",
			prepare: func(_ *testing.T, _ *app.Service, f *vultrfake.Fake) {
				f.SetAvailability("ams", "vc2-1c-1gb")
			},
			check: func(t *testing.T, err error) {
				const detail = `plan "vc2-2c-4gb" is not available in ams now`
				wantFieldErrors(t, err,
					v1alpha1.FieldError{Object: "NodeGroup servers", Path: "spec.machineType", Detail: detail},
					v1alpha1.FieldError{Object: "NodeGroup workers", Path: "spec.machineType", Detail: detail})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, f := newUpdate(t)
			tc.prepare(t, svc, f)
			rec := &writeLog{Store: svc.Store}
			svc.Store = rec
			_, err := svc.Update(t.Context(), "prod", true)
			tc.check(t, err)
			if len(rec.writes) != 0 {
				t.Errorf("writes to the store: %v", rec.writes)
			}
			wantOnlyReads(t, f)
			wantNodes(t, f)
		})
	}
}

func TestUpdateMissingCluster(t *testing.T) {
	svc, f := newUpdate(t)
	_, err := svc.Update(t.Context(), "dev", true)
	wantError(t, err, notFound(svc, "cluster dev"))
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}

// TestUpdateHoldsTheLock runs another change while the update creates a node: it finds the cluster locked for update.
func TestUpdateHoldsTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		other := &app.Service{Store: svc.Store, Version: "dev"}
		var errs []error
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Step == app.NodeStarted {
				errs = append(errs, replaceWorkers(t, other))
			}
		}
		mustUpdate(t, svc)
		if len(errs) != len(allNodes) {
			t.Fatalf("%d node steps started, want %d", len(errs), len(allNodes))
		}
		for _, err := range errs {
			locked, ok := errors.AsType[*statestore.LockedError](err)
			if !ok || locked.Holder.Operation != "update" {
				t.Errorf("another change failed with %v, want the lock held for update", err)
			}
		}
		wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
	})
}

// TestUpdateInterrupted interrupts the update as it starts the first node, as Ctrl-C would.
func TestUpdateInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		ctx, interrupt := context.WithCancel(t.Context())
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Step == app.NodeStarted {
				interrupt()
			}
		}
		_, err := svc.Update(ctx, "prod", true)
		wantError(t, err, "interrupted")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false", err)
		}
		wantLockFree(t, svc.Store)
		if paths := list(t, svc.Store, completedPath); len(paths) != 0 {
			t.Errorf("the completed spec is written: %v", paths)
		}
		wantNodes(t, f)
	})
}

// TestUpdateTellsThePlan tells OnUpdatePlan the plan made under the lock, once, before the first change, and returns
// that plan applied.
func TestUpdateTellsThePlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		var events []string
		var told []app.UpdatePlan
		svc.OnUpdatePlan = func(p app.UpdatePlan) error {
			if h := holder(t, svc.Store); h == nil || h.Operation != "update" {
				t.Errorf("the lock is held by %v while OnUpdatePlan runs, want an update", h)
			}
			told = append(told, p)
			events = append(events, "plan")
			return nil
		}
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }

		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if len(told) != 0 {
			t.Errorf("OnUpdatePlan was called %d times without apply, want none", len(told))
		}
		plan := mustUpdate(t, svc)
		if len(told) != 1 || len(events) < 2 || events[0] != "plan" {
			t.Fatalf("OnUpdatePlan was called %d times, and the events start with %q; want once, first", len(told),
				events[:min(2, len(events))])
		}
		if told[0].Applied {
			t.Error("the plan told before the changes says it was applied")
		}
		if got, want := planText(t, told[0]), planText(t, plan); got != want {
			t.Errorf("the plan told is\n%s\nwant the plan applied\n%s", got, want)
		}

		mustUpdate(t, svc)
		if len(told) != 1 {
			t.Errorf("OnUpdatePlan was called for an update without changes")
		}
	})
}

// TestUpdateStopsWhenThePlanCannotBeTold changes nothing when OnUpdatePlan fails, and returns its error.
func TestUpdateStopsWhenThePlanCannotBeTold(t *testing.T) {
	svc, f := newUpdate(t)
	errClosed := errors.New("stdout is closed")
	svc.OnUpdatePlan = func(app.UpdatePlan) error { return errClosed }
	svc.OnProgress = func(p app.Progress) { t.Errorf("progress: %s", progressLine(p)) }
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec

	plan, err := svc.Update(t.Context(), "prod", true)

	if !errors.Is(err, errClosed) {
		t.Errorf("Update = %v, want %v", err, errClosed)
	}
	if plan.Applied {
		t.Error("the plan says it was applied")
	}
	if diff := cmp.Diff([]string{lockPath, lockPath}, rec.writes); diff != "" {
		t.Errorf("writes to the store other than the lock's (-want +got):\n%s", diff)
	}
	wantOnlyReads(t, f)
	wantLockFree(t, svc.Store)
}

func TestNodeStepString(t *testing.T) {
	for _, tc := range []struct {
		step app.NodeStep
		want string
	}{
		{app.NodeStarted, "started"},
		{app.NodeDone, "done"},
		{app.NodeFailed, "failed"},
		{0, "NodeStep(0)"},
		{app.NodeFailed + 1, "NodeStep(4)"},
	} {
		if got := tc.step.String(); got != tc.want {
			t.Errorf("NodeStep(%d).String() = %q, want %q", int(tc.step), got, tc.want)
		}
	}
}
