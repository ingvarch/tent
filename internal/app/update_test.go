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
	"github.com/ingvarch/tent/internal/channels"
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

// withCloud gives svc providers that reach a new Vultr fake, the release files of assetstest and a Nomad that follows
// the fake, and returns the fake and the buffer that the provider logs to as JSON. Every provider other than vultr is
// unknown.
func withCloud(svc *app.Service) (*vultrfake.Fake, *bytes.Buffer) {
	f := vultrfake.New()
	withNomad(svc, f)
	return f, withAPI(svc, f)
}

// withAPI gives svc providers that reach Vultr through api and the release files of assetstest, and returns the buffer
// that the provider logs to as JSON. Every provider other than vultr is unknown.
func withAPI(svc *app.Service, api vultr.API) *bytes.Buffer {
	withAssets(svc)
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

// specHashTag is the tag of a node's spec hash as the tests see it, with the hash masked.
const specHashTag = cloud.LabelSpecHash + "=<hash>"

// scrubbedStub is the user data that a joined node keeps, as the Vultr provider leaves it.
const scrubbedStub = "#cloud-config\n# tent removed this node's user data after the node joined the cluster\n{}\n"

// isScrubbed reports whether the user data of the instance id in f is the stub.
func isScrubbed(f *vultrfake.Fake, id string) bool {
	return f.UserData(id) == base64.StdEncoding.EncodeToString([]byte(scrubbedStub))
}

// isJoined reports whether the tags carry the joined label with the value true.
func isJoined(tags []string) bool { return tagOf(tags, cloud.LabelJoined) == "true" }

// unscrubbedJoined returns the names of the instances of f that carry the joined label and hold other user data than
// the stub.
func unscrubbedJoined(f *vultrfake.Fake) []string {
	var out []string
	for _, in := range f.Instances() {
		if isJoined(in.Tags) && !isScrubbed(f, in.ID) {
			out = append(out, in.Hostname)
		}
	}
	return out
}

// joinedMismatches returns what differs from the state in which exactly the instances called names carry the joined
// label and the stub, in the order of the instances.
func joinedMismatches(f *vultrfake.Fake, names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, in := range f.Instances() {
		want := slices.Contains(names, in.Hostname)
		seen[in.Hostname] = true
		joined, scrubbed := isJoined(in.Tags), isScrubbed(f, in.ID)
		switch {
		case want && !joined:
			out = append(out, in.Hostname+" lacks "+cloud.LabelJoined)
		case !want && joined:
			out = append(out, in.Hostname+" carries "+cloud.LabelJoined)
		}
		switch {
		case want && !scrubbed:
			out = append(out, in.Hostname+" holds user data other than the stub")
		case !want && scrubbed:
			out = append(out, in.Hostname+" holds the stub")
		}
	}
	for _, name := range names {
		if !seen[name] {
			out = append(out, "no instance "+name)
		}
	}
	return out
}

// wantJoined fails the test unless exactly the instances called names carry the joined label and the stub.
func wantJoined(t *testing.T, f *vultrfake.Fake, names ...string) {
	t.Helper()
	if got := joinedMismatches(f, names); len(got) > 0 {
		t.Errorf("the joined instances differ from %q:\n%s", names, strings.Join(got, "\n"))
	}
}

// node is an instance of the fake as the tests see it: its name, its region and its tent tags without the operation
// id and the joined tag, the spec hash masked.
type node struct {
	Name, Zone string
	Tags       []string
}

// nodes returns the fake's instances in creation order, without the joined tag. It fails the test for an instance that
// carries that tag and still holds other user data than the stub, and unless each carries an operation id of its own
// and a spec hash of 16 hex digits that is the same in its node group.
func nodes(t *testing.T, f *vultrfake.Fake) []node {
	t.Helper()
	var out []node
	for _, name := range unscrubbedJoined(f) {
		t.Errorf("instance %s carries %s and still holds its user data", name, cloud.LabelJoined)
	}
	ops := map[string]bool{}
	hashes := map[string]string{} // by node group
	for _, in := range f.Instances() {
		n := node{Name: in.Hostname, Zone: in.Region}
		op := opOf(in.Tags)
		group := tagOf(in.Tags, cloud.LabelNodeGroup)
		for _, tag := range in.Tags {
			switch {
			case strings.HasPrefix(tag, cloud.LabelOp+"="), strings.HasPrefix(tag, cloud.LabelJoined+"="):
			case strings.HasPrefix(tag, cloud.LabelSpecHash+"="):
				hash := strings.TrimPrefix(tag, cloud.LabelSpecHash+"=")
				if !specHashPattern.MatchString(tag) {
					t.Errorf("instance %s has the spec hash %q, want 16 hex digits", in.Hostname, hash)
				}
				if hashes[group] != "" && hashes[group] != hash {
					t.Errorf("instance %s has the spec hash %s, another node of group %s has %s", in.Hostname, hash, group,
						hashes[group])
				}
				hashes[group] = hash
				n.Tags = append(n.Tags, specHashTag)
			default:
				n.Tags = append(n.Tags, tag)
			}
		}
		if !cloud.ValidOpID(op) || ops[op] {
			t.Errorf("instance %s has the operation id %q; want one of its own", in.Hostname, op)
		}
		ops[op] = true
		out = append(out, n)
	}
	return out
}

// wantNode is the node index of the test cluster's group, whose role is role.
func wantNode(group string, role v1alpha1.Role, index int) node {
	return node{
		Name: fmt.Sprintf("prod-%s-%d", group, index), Zone: "ams",
		Tags: []string{"tent/cluster=prod", "tent/nodegroup=" + group, "tent/role=" + string(role), specHashTag},
	}
}

func server(index int) node { return wantNode("servers", v1alpha1.RoleServer, index) }

func worker(index int) node { return wantNode("workers", v1alpha1.RoleClient, index) }

// allNodes are the nodes of the test cluster in the order the update creates them.
var allNodes = []node{server(0), server(1), server(2), worker(0), worker(1)}

// allNames are the names of allNodes.
var allNames = namesOf(allNodes)

func namesOf(nodes []node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

// wantNodes fails the test unless the fake holds these instances, in creation order.
func wantNodes(t *testing.T, f *vultrfake.Fake, want ...node) {
	t.Helper()
	if diff := cmp.Diff(want, nodes(t, f)); diff != "" {
		t.Errorf("the instances (-want +got):\n%s", diff)
	}
}

// hashMask stands for a spec hash in the node changes that the tests compare.
const hashMask = "<hash>"

// createOf is the planned create of the node index of the test cluster's group, whose role is role, with the spec hash
// masked.
func createOf(group string, role v1alpha1.Role, index int) app.NodeChange {
	return app.NodeChange{
		Action: app.NodeCreate, Name: fmt.Sprintf("prod-%s-%d", group, index), Group: group, Role: role, Zone: "ams",
		MachineType: "vc2-2c-4gb", Image: v1alpha1.DefaultImage, SpecHash: hashMask,
	}
}

// deleteOf is the planned delete of the node name, the instance id, for reason.
func deleteOf(name, id, reason string) app.NodeChange {
	return app.NodeChange{Action: app.NodeDelete, Name: name, ID: id, Reason: reason}
}

// wantNodeChanges fails the test unless the plan's node changes are these. A spec hash must be 16 hex digits and the
// same for the changes of a node group, and shows as hashMask.
func wantNodeChanges(t *testing.T, plan app.UpdatePlan, want ...app.NodeChange) {
	t.Helper()
	got := slices.Clone(plan.Nodes)
	hashes := map[string]string{} // by node group
	for i, c := range got {
		if c.SpecHash == "" {
			continue
		}
		if !specHashPattern.MatchString("tent/spec-hash=" + c.SpecHash) {
			t.Errorf("the change of %s has the spec hash %q, want 16 hex digits", c.Name, c.SpecHash)
		}
		if hashes[c.Group] != "" && hashes[c.Group] != c.SpecHash {
			t.Errorf("the change of %s has the spec hash %s, another change of group %s has %s", c.Name, c.SpecHash,
				c.Group, hashes[c.Group])
		}
		hashes[c.Group], got[i].SpecHash = c.SpecHash, hashMask
	}
	if diff := cmp.Diff(want, got); diff != "" {
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
// wait for n nodes to go, "nomad <step> <action> [<node>]" for a step of the Nomad step and "node <step> <action>
// <name>" for a node step, with the error of a failed step.
func progressLine(p app.Progress) string {
	switch {
	case p.Infra != nil:
		return fmt.Sprintf("infra %s %s %s", p.Infra.Type, p.Infra.Key, p.Infra.Action)
	case p.Going > 0:
		return fmt.Sprintf("going %d", p.Going)
	case p.Nomad != nil:
		line := strings.TrimSpace(fmt.Sprintf("nomad %s %s %s", p.Step, p.Nomad.Action, p.Nomad.Node))
		if p.Err != nil {
			line += ": " + p.Err.Error()
		}
		return line
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

// registerSteps returns the progress lines of the wait for the node called name to register, which succeeded.
func registerSteps(name string) []string {
	return []string{"nomad started register " + name, "nomad done register " + name}
}

// fullSpec returns what the completed spec holds: the test cluster's specs with every default and, unless the spec
// sets one, the Nomad version that the embedded channel stable recommends.
func fullSpec(t *testing.T, svc *app.Service) []byte {
	t.Helper()
	stable, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	return completedSpec(t, svc, stable.Nomad.Recommended)
}

// completedSpec returns the test cluster's specs with every default and, unless the spec sets one, the Nomad version
// v. With v empty, it is the completed spec as a tent without channels wrote it.
func completedSpec(t *testing.T, svc *app.Service, v string) []byte {
	t.Helper()
	objs, err := svc.Get(t.Context(), "prod", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := &objs.Cluster.Spec.Nomad; n.Version == "" {
		n.Version = v
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

// writeLog records the path of every write to the store, the lock's included, and tells onWrite when it is set.
type writeLog struct {
	statestore.Store
	onWrite func(path string)
	mu      sync.Mutex
	writes  []string
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
	if w.onWrite != nil {
		w.onWrite(p)
	}
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
func opOf(tags []string) string { return tagOf(tags, cloud.LabelOp) }

// tagOf returns the value of the tag called label in the tags of an instance, "" when it has none.
func tagOf(tags []string, label string) string {
	for _, tag := range tags {
		if v, ok := strings.CutPrefix(tag, label+"="); ok {
			return v
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
	sites := withAssets(svc)
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec
	svc.OnProgress = func(p app.Progress) { t.Errorf("progress without apply: %s", progressLine(p)) }

	plan, err := svc.Update(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	wantInfraChanges(t, plan, infraCreates...)
	wantNodeChanges(t, plan, allCreates...)
	if want := (&app.NomadStep{Bootstrap: true, Servers: 3}); !cmp.Equal(plan.Nomad, want) {
		t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
	}
	if plan.Applied {
		t.Error("the plan says it was applied")
	}
	if !plan.Completed {
		t.Error("the plan does not write the completed spec, which the store lacks")
	}
	if len(rec.writes) != 0 {
		t.Errorf("writes to the store: %v", rec.writes)
	}
	if got := len(sites.URLs()); got != 2 {
		t.Errorf("the plan read %d release files %v, want Nomad's SHA256SUMS and its signature", got, sites.URLs())
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

		// The infrastructure's events come first, in any order, then the node steps and the Nomad step in order.
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
		wantSteps := slices.Concat(
			nodeSteps("create", "prod-servers-0"), nodeSteps("create", "prod-servers-1"),
			nodeSteps("create", "prod-servers-2"),
			[]string{
				"nomad started leader", "nomad done leader", "nomad started bootstrap", "nomad done bootstrap",
				"nomad started healthy", "nomad done healthy", "nomad started keyring", "nomad done keyring",
			},
			nodeSteps("scrub", "prod-servers-0"), nodeSteps("scrub", "prod-servers-1"),
			nodeSteps("scrub", "prod-servers-2"),
			nodeSteps("create", "prod-workers-0"), registerSteps("prod-workers-0"), nodeSteps("scrub", "prod-workers-0"),
			nodeSteps("create", "prod-workers-1"), registerSteps("prod-workers-1"), nodeSteps("scrub", "prod-workers-1"),
		)
		if diff := cmp.Diff(wantSteps, (*progress)[split:]); diff != "" {
			t.Errorf("the node steps and the Nomad step (-want +got):\n%s", diff)
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
			mustReplace(t, svc, keyedClusterYAML+"  nomad:\n    region: eu\n")
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

// joinedRefusal is the error of a plan that deletes the nodes of names, which joined Nomad: each name is
// "<node> (ID <id>, <reason>)". The advice names tent rolling-update cluster when a node is surplus.
func joinedRefusal(surplus bool, names ...string) string {
	tail := "; update deletes only nodes that never joined: "
	if surplus {
		tail += "run tent rolling-update cluster to finish a rolling update that stopped, "
	}
	if len(names) == 1 {
		return "update would delete a node that joined Nomad: " + names[0] + tail +
			"keep this node in the specs, or delete the whole cluster with tent delete cluster"
	}
	return "update would delete nodes that joined Nomad: " + strings.Join(names[:len(names)-1], ", ") + " and " +
		names[len(names)-1] + tail + "keep these nodes in the specs, or delete the whole cluster with tent delete cluster"
}

// duplicateRefusal is the error of a plan that deletes the machine id of the node name, a duplicate of the machine
// stayer, which joined Nomad.
func duplicateRefusal(name, id, stayer string) string {
	return "update would delete a node that joined Nomad: " + name + " (ID " + id + ", duplicate of ID " + stayer + "); " +
		"update deletes only nodes that never joined: remove one of the two machines called " + name + " from Nomad " +
		"and delete it in the cloud, or delete the whole cluster with tent delete cluster"
}

// wantRefused fails the test unless the update of the test cluster, with and without apply, fails with the error
// want and leaves the cloud and the store as they are.
func wantRefused(t *testing.T, svc *app.Service, f *vultrfake.Fake, want string) {
	t.Helper()
	cloudBefore, storeBefore := cloudView(f), snapshot(t, svc.Store)
	calls := len(f.Calls())
	for _, apply := range []bool{false, true} {
		_, err := svc.Update(t.Context(), "prod", apply)
		wantError(t, err, want)
	}
	wantNoWrites(t, f.Calls()[calls:])
	if diff := cmp.Diff(cloudBefore, cloudView(f)); diff != "" {
		t.Errorf("the cloud changed (-before +after):\n%s", diff)
	}
	wantSnapshot(t, svc.Store, storeBefore)
}

func TestUpdateScales(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)

		// Down: the newest worker joined, so the plan refuses to delete it.
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 1"))
		wantRefused(t, svc, f, joinedRefusal(true, "prod-workers-1 (ID instance-5, surplus)"))
		wantNodes(t, f, allNodes...)

		// Up: the new node takes the next index.
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))
		plan := mustUpdate(t, svc)
		wantNodeChanges(t, plan, createOf("workers", v1alpha1.RoleClient, 2))
		wantNodes(t, f, server(0), server(1), server(2), worker(0), worker(1), worker(2))
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
		wantConverged(t, svc)
	})
}

// TestUpdateRemovesTheLastClientGroup removes the only client group. One update deletes a worker that never joined,
// then the clients' firewall group, which Vultr's guard keeps while nodes use it. When the workers joined, the plan
// refuses.
func TestUpdateRemovesTheLastClientGroup(t *testing.T) {
	t.Run("a worker that never joined", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			w.Withhold("prod-workers-0") // the build stops at its registration
			if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the build: %v, want a deadline", err)
			}
			clients := firewallGroupID(t, f, "client")
			if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
				t.Fatal(err)
			}
			before, asked := len(f.Calls()), len(w.Log())
			progress := recordProgress(svc)

			plan := mustUpdate(t, svc)

			wantInfraChanges(t, plan, "delete "+kindFirewall+"/prod-clients")
			wantNodeChanges(t, plan, deleteOf("prod-workers-0", "instance-4", "not in the spec"))
			wantLines(t, *progress, slices.Concat(nodeSteps("delete", "prod-workers-0"), []string{
				"infra started " + kindFirewall + "/prod-clients delete",
				"infra succeeded " + kindFirewall + "/prod-clients delete",
			}))
			if diff := cmp.Diff([]string{"Nodes", "Peers"}, nomadNames(w, asked)); diff != "" {
				t.Errorf("the Nomad calls before the delete (-want +got):\n%s", diff)
			}
			var deletes []vultrfake.Call
			for _, c := range f.Calls()[before:] {
				if strings.HasPrefix(c.Name, "Delete") {
					deletes = append(deletes, c)
				}
			}
			wantDeletes := []vultrfake.Call{
				{Name: "DeleteInstance", Arg: "instance-4"}, {Name: "DeleteFirewallGroup", Arg: clients},
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
	})
	t.Run("workers that joined", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f := newUpdate(t)
			mustUpdate(t, svc)
			if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
				t.Fatal(err)
			}

			wantRefused(t, svc, f, joinedRefusal(false,
				"prod-workers-0 (ID instance-4, not in the spec)", "prod-workers-1 (ID instance-5, not in the spec)"))
			wantNodes(t, f, allNodes...)
		})
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
		if bytes.Equal(get(t, svc.Store, completedPath), completed) {
			t.Error("the completed spec is the old one after the failed run, want the new one")
		}
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
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

// TestUpdateReportsTheMachines reports the machine of each node it created: its ID and its private address. A scrub
// step names no machine but the node's.
func TestUpdateReportsTheMachines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		var steps []app.Progress
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Nomad == nil {
				steps = append(steps, p)
			}
		}

		mustUpdate(t, svc)

		var got, want []string
		for _, p := range steps {
			got = append(got, fmt.Sprintf("%s %s %s %s %s", p.Step, p.Node.Action, p.Node.Name, p.Instance.ID,
				p.Instance.PrivateIP))
		}
		for i, n := range allNodes {
			want = append(want, "started create "+n.Name+"  invalid IP",
				fmt.Sprintf("done create %s instance-%d 10.64.0.%d", n.Name, i+1, i+3))
			if i == 2 { // the servers are scrubbed once they are healthy, before the clients are made
				for _, s := range allNodes[:3] {
					want = append(want, "started scrub "+s.Name+"  invalid IP", "done scrub "+s.Name+"  invalid IP")
				}
			}
			if i > 2 { // a client is scrubbed once it has registered
				want = append(want, "started scrub "+n.Name+"  invalid IP", "done scrub "+n.Name+"  invalid IP")
			}
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
			if p.Infra == nil && p.Nomad == nil && p.Step == app.NodeDone {
				done = append(done, fmt.Sprintf("%s %s %s %s", p.Node.Action, p.Node.Name, p.Instance.ID,
					p.Instance.PrivateIP))
			}
		}

		mustUpdate(t, svc)

		want := []string{"wait prod-workers-1 instance-6 10.64.0.7", "scrub prod-workers-1  invalid IP"}
		if diff := cmp.Diff(want, done); diff != "" {
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
		wantNodeChanges(t, plan, slices.Concat([]app.NodeChange{wait}, allCreates[1:])...)

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
			if p.Infra == nil && p.Nomad == nil && p.Step == app.NodeStarted {
				errs = append(errs, replaceWorkers(t, other))
			}
		}
		mustUpdate(t, svc)
		if want := 2 * len(allNodes); len(errs) != want { // the nodes, and the scrub of each
			t.Fatalf("%d node steps started, want %d", len(errs), want)
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

// TestUpdateInterrupted interrupts the update as it starts the first node, as Ctrl-C would. The completed spec is
// stored by then, and no node exists.
func TestUpdateInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		ctx, interrupt := context.WithCancel(t.Context())
		svc.OnProgress = func(p app.Progress) {
			if p.Infra == nil && p.Nomad == nil && p.Step == app.NodeStarted {
				interrupt()
			}
		}
		_, err := svc.Update(ctx, "prod", true)
		wantError(t, err, "interrupted")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false", err)
		}
		wantLockFree(t, svc.Store)
		wantStored(t, svc.Store, completedPath, fullSpec(t, svc))
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

// dropJoinedTag takes the joined label off the instance id of f, as a change by hand could, and leaves its user data.
func dropJoinedTag(t *testing.T, f *vultrfake.Fake, id string) {
	t.Helper()
	for _, in := range f.Instances() {
		if in.ID == id {
			f.SetInstanceTags(t, id, slices.DeleteFunc(in.Tags, func(tag string) bool {
				return strings.HasPrefix(tag, cloud.LabelJoined+"=")
			})...)
		}
	}
}

// unmark takes the joined label off the instance id of f and gives it the user data of its create request back.
func unmark(t *testing.T, f *vultrfake.Fake, id string) {
	t.Helper()
	req, ok := f.CreateRequest(id)
	if !ok {
		t.Fatalf("the fake has no create request for %s", id)
	}
	dropJoinedTag(t, f, id)
	f.SetInstanceUserData(t, id, req.UserData)
}

// markedBuild is a fake with the five instances of a first build, in which only prod-servers-0 is marked as joined:
// the other four have lost the label and hold their first user data again.
func markedBuild(t *testing.T) *vultrfake.Fake {
	t.Helper()
	svc, f, _ := newRelease(t)
	mustUpdate(t, svc)
	wantJoined(t, f, allNames...)
	for _, in := range f.Instances()[1:] {
		unmark(t, f, in.ID)
	}
	return f
}

// joinedFixture is markedBuild with one more instance, no-stub, a seeded instance that carries the joined label and
// has no stub (its user data is empty).
func joinedFixture(t *testing.T) *vultrfake.Fake {
	t.Helper()
	f := markedBuild(t)
	in := f.Instances()[0] // prod-servers-0, whose tags hold the joined label
	in.ID, in.Hostname = "", "no-stub"
	f.AddInstance(t, in)
	return f
}

func TestJoinedMismatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := joinedFixture(t)
		for _, tc := range []struct {
			name  string
			names []string
			want  []string
		}{
			{"the marked one", []string{"prod-servers-0", "no-stub"}, []string{
				"no-stub holds user data other than the stub"}},
			{"none expected", nil, []string{"prod-servers-0 carries tent/joined", "prod-servers-0 holds the stub",
				"no-stub carries tent/joined"}},
			{"a node that is not marked", []string{"prod-servers-0", "prod-servers-1"}, []string{
				"prod-servers-1 lacks tent/joined", "prod-servers-1 holds user data other than the stub",
				"no-stub carries tent/joined"}},
			{"a client that is not marked", []string{"prod-servers-0", "prod-workers-0"}, []string{
				"prod-workers-0 lacks tent/joined", "prod-workers-0 holds user data other than the stub",
				"no-stub carries tent/joined"}},
			{"an unknown node", []string{"prod-servers-0", "x"}, []string{"no-stub carries tent/joined",
				"no instance x"}},
		} {
			if diff := cmp.Diff(tc.want, joinedMismatches(f, tc.names)); diff != "" {
				t.Errorf("%s: the mismatches (-want +got):\n%s", tc.name, diff)
			}
		}
	})
}

func TestJoinedTagStaysOutOfTheNodeView(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := markedBuild(t)
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, "prod-servers-0")
		if nc := configOf(t, f, "prod-servers-0"); nc.Name != "prod-servers-0" {
			t.Errorf("the NodeConfig is of %q, want prod-servers-0", nc.Name)
		}
	})
}

func TestUnscrubbedJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if diff := cmp.Diff([]string{"no-stub"}, unscrubbedJoined(joinedFixture(t))); diff != "" {
			t.Errorf("the joined instances without the stub (-want +got):\n%s", diff)
		}
	})
}

func TestCloudViewShowsTheUserData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { testCloudViewShowsTheUserData(t, joinedFixture(t)) })
}

func testCloudViewShowsTheUserData(t *testing.T, f *vultrfake.Fake) {
	got := map[string]string{}
	for _, line := range cloudView(f) {
		if fields := strings.Fields(line); fields[0] == "instance" {
			i := strings.LastIndex(line, "user data")
			if i < 0 {
				t.Errorf("the line %q does not tell the user data", line)
				continue
			}
			got[fields[1]] = line[i:]
		}
	}
	want := map[string]string{"prod-servers-0": "user data scrubbed", "prod-servers-1": "user data kept",
		"prod-servers-2": "user data kept", "prod-workers-0": "user data kept", "prod-workers-1": "user data kept",
		"no-stub": "user data kept"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the user data of the instance lines (-want +got):\n%s", diff)
	}
}
