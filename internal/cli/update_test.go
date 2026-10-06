package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// updatePlan is the plan of the test cluster on an empty cloud, as update cluster prints it.
const updatePlan = `+ vultr.VPC/prod
    + cidr: 10.64.0.0/16
    + region: ams
+ vultr.FirewallGroup/prod-servers
    + rule: v4 icmp 0.0.0.0/0
    + rule: v4 tcp 0.0.0.0/0 4646
    + rule: v6 icmp ::/0
+ vultr.FirewallGroup/prod-clients
    + rule: v4 icmp 0.0.0.0/0
    + rule: v6 icmp ::/0
+ node prod-servers-0 (server, vc2-2c-4gb, ams)
+ node prod-servers-1 (server, vc2-2c-4gb, ams)
+ node prod-servers-2 (server, vc2-2c-4gb, ams)
+ node prod-workers-0 (client, vc2-2c-4gb, ams)
+ node prod-workers-1 (client, vc2-2c-4gb, ams)
+ node prod-workers-2 (client, vc2-2c-4gb, ams)

Plan: 3 to create, 0 to update, 0 to replace, 0 to delete.
Nodes: 6 to create, 0 to wait for, 0 to delete.
Nomad: bootstrap the ACL system and wait for 3 healthy servers.
State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, cluster.completed.yaml and nomad/bootstrapped will be written.
`

// applyHint is what update cluster prints on stderr after a plan with changes.
const applyHint = "run with --yes to apply the changes\n"

// built is what update cluster --yes prints on stdout when it builds the test cluster on an empty cloud: the plan,
// then a line that sums up what it did.
const built = updatePlan + "\n" +
	"Applied: 3 created, 0 updated, 0 replaced, 0 deleted. Nodes: 6 created, 0 waited for, 0 deleted. " +
	"Nomad: bootstrapped the ACL system; 3 servers are healthy. " +
	"Wrote pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, " +
	"cluster.completed.yaml and nomad/bootstrapped.\n"

// secretPaths are the test cluster's secrets in the store, in the order an update writes them.
var secretPaths = []string{
	"prod/pki/private/ca.key", "prod/pki/ca-bundle.pem", "prod/secrets/gossip.key", "prod/secrets/acl-bootstrap-token",
}

// nodeNames are the nodes of the test cluster in the order an update creates them.
var nodeNames = []string{
	"prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0", "prod-workers-1", "prod-workers-2",
}

// infraKeys are the infrastructure objects of the test cluster.
var infraKeys = []string{"vultr.VPC/prod", "vultr.FirewallGroup/prod-servers", "vultr.FirewallGroup/prod-clients"}

// update returns the arguments that update the test cluster in the store s, followed by more.
func update(s state, more ...string) []string {
	return append([]string{"update", "cluster", "prod", "--state", s.url}, more...)
}

// wantNoWrites fails the test unless every call that reached the fake reads.
func wantNoWrites(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	wantNoWritesIn(t, f.Calls())
}

// wantNoWritesIn fails the test unless every one of the calls reads.
func wantNoWritesIn(t *testing.T, calls []vultrfake.Call) {
	t.Helper()
	for _, c := range calls {
		if !strings.HasPrefix(c.Name, "List") && !strings.HasPrefix(c.Name, "Get") && c.Name != "AvailablePlans" {
			t.Errorf("a call that writes: %s %s", c.Name, c.Arg)
		}
	}
}

// wantInstances fails the test unless the fake holds instances with these names, in creation order.
func wantInstances(t *testing.T, f *vultrfake.Fake, names ...string) {
	t.Helper()
	var got []string
	for _, in := range f.Instances() {
		got = append(got, in.Hostname)
	}
	if diff := cmp.Diff(names, got); diff != "" {
		t.Errorf("the instances (-want +got):\n%s", diff)
	}
}

// buildLines returns the progress lines of an update that builds the test cluster on an empty cloud: the
// infrastructure's, which run in parallel and so come sorted, then the nodes' and Nomad's in order: the servers, the
// Nomad step, and each worker with the lines of its registration and its scrub.
func buildLines() (infra, nodes []string) {
	for _, k := range infraKeys {
		infra = append(infra, "creating "+k, "created "+k)
	}
	slices.Sort(infra)
	for i, n := range nodeNames {
		if n == "prod-workers-0" {
			nodes = append(nodes, nomadLines...)
		}
		nodes = append(nodes, "creating node "+n, fmt.Sprintf("created node %s (10.64.0.%d)", n, i+3))
		if strings.Contains(n, "workers") {
			nodes = append(nodes, "waiting for node "+n+" to register", "node "+n+" registered",
				"scrubbing the user data of node "+n, "scrubbed the user data of node "+n)
		}
	}
	return infra, nodes
}

// nomadLines are the progress lines of the Nomad step of the build and the scrub of the servers, between the servers
// and the workers.
var nomadLines = []string{
	"waiting for a Nomad leader", "Nomad has a leader (10.64.0.3:4647)",
	"bootstrapping the ACL system", "bootstrapped the ACL system",
	"waiting for 3 healthy Nomad servers", "3 Nomad servers are healthy",
	"scrubbing the user data of node prod-servers-0", "scrubbed the user data of node prod-servers-0",
	"scrubbing the user data of node prod-servers-1", "scrubbed the user data of node prod-servers-1",
	"scrubbing the user data of node prod-servers-2", "scrubbed the user data of node prod-servers-2",
}

// wantBuildProgress fails the test unless errOut holds the progress lines of an update that builds the test cluster
// on an empty cloud, after the lines of before.
func wantBuildProgress(t *testing.T, errOut string, before ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")
	infra, nodes := buildLines()
	if len(lines) != len(before)+len(infra)+len(nodes) {
		t.Fatalf("stderr has %d lines, want %d:\n%s", len(lines), len(before)+len(infra)+len(nodes), errOut)
	}
	if !slices.Equal(before, lines[:len(before)]) {
		t.Errorf("stderr starts with\n%s\nwant\n%s", strings.Join(lines[:len(before)], "\n"),
			strings.Join(before, "\n"))
	}
	lines = lines[len(before):]
	if diff := cmp.Diff(infra, slices.Sorted(slices.Values(lines[:len(infra)]))); diff != "" {
		t.Errorf("the infrastructure's progress (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(nodes, lines[len(infra):]); diff != "" {
		t.Errorf("the nodes' progress (-want +got):\n%s", diff)
	}
}

func TestUpdateClusterPlan(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	wantResult(t, runOn(t, f, update(s)...), 0, updatePlan, applyHint)
	wantResult(t, runOn(t, f, "update", "cluster", "--name", "prod", "--state", s.url), 0, updatePlan, applyHint)
	wantNoWrites(t, f)
	s.want(t, prodObjects)
}

// planJSON is the plan as -o json prints it, with the parts that the tests check.
type planJSON struct {
	Applied        bool
	Infrastructure struct {
		Changes []struct{ Kind, Name, Action string }
	}
	Nodes []struct {
		Action, Name, Group, Role string
	}
	Nomad *struct {
		Bootstrap bool
		Servers   int
	}
	Secrets       []string
	CompletedSpec bool
}

// decodePlan decodes the plan that update cluster -o json printed.
func decodePlan(t *testing.T, out string) planJSON {
	t.Helper()
	var p planJSON
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("the plan is not the plan's JSON: %v\n%s", err, out)
	}
	return p
}

// wantPlanJSON fails the test unless p creates the test cluster on an empty cloud.
func wantPlanJSON(t *testing.T, p planJSON) {
	t.Helper()
	wantPlanJSONOf(t, p, nodeNames)
}

// wantPlanJSONOf fails the test unless p creates the nodes of the test cluster that are called names on an empty
// cloud, and bootstraps Nomad for its three servers.
func wantPlanJSONOf(t *testing.T, p planJSON, names []string) {
	t.Helper()
	var infra []string
	for _, c := range p.Infrastructure.Changes {
		infra = append(infra, c.Action+" "+c.Kind+"/"+c.Name)
	}
	var want []string
	for _, k := range infraKeys {
		want = append(want, "create "+k)
	}
	if diff := cmp.Diff(want, infra); diff != "" {
		t.Errorf("the infrastructure's changes (-want +got):\n%s", diff)
	}
	var nodes []string
	for _, n := range p.Nodes {
		nodes = append(nodes, n.Action+" "+n.Name+" "+n.Group+" "+n.Role)
	}
	want = nil
	for _, n := range names {
		group, role := "servers", "server"
		if strings.Contains(n, "workers") {
			group, role = "workers", "client"
		}
		want = append(want, "create "+n+" "+group+" "+role)
	}
	if diff := cmp.Diff(want, nodes); diff != "" {
		t.Errorf("the node changes (-want +got):\n%s", diff)
	}
	if p.Nomad == nil || !p.Nomad.Bootstrap || p.Nomad.Servers != 3 {
		t.Errorf("the Nomad step is %+v, want the bootstrap and 3 servers", p.Nomad)
	}
	want = nil
	for _, secret := range secretPaths {
		want = append(want, strings.TrimPrefix(secret, "prod/"))
	}
	if diff := cmp.Diff(want, p.Secrets); diff != "" {
		t.Errorf("the secrets (-want +got):\n%s", diff)
	}
	if !p.CompletedSpec {
		t.Error("the plan does not write the completed spec")
	}
}

func TestUpdateClusterPlanJSON(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	got := runOn(t, f, update(s, "-o", "json")...)
	if got.code != 0 || got.errOut != applyHint {
		t.Errorf("exit code = %d, stderr = %q; want 0 and %q", got.code, got.errOut, applyHint)
	}
	if !strings.HasPrefix(got.out, "{\n  \"infrastructure\": {\n") {
		t.Errorf("stdout is not indented JSON:\n%s", got.out)
	}
	wantPlanJSON(t, decodePlan(t, got.out))
	wantNoWrites(t, f)
}

func TestUpdateClusterPlanYAML(t *testing.T) {
	s := withCluster(t)
	got := runOnCloud(t, update(s, "-o", "yaml")...)
	if got.code != 0 || got.errOut != applyHint {
		t.Errorf("exit code = %d, stderr = %q; want 0 and %q", got.code, got.errOut, applyHint)
	}
	for _, want := range []string{"completedSpec: true\n", "infrastructure:\n  changes:\n", "- action: create\n"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("stdout\n%s\nwant it to hold %q", got.out, want)
		}
	}
}

// TestUpdateClusterExitCode exits with 2 through Execute when the plan has changes, and prints the plan without an
// error.
func TestUpdateClusterExitCode(t *testing.T) {
	s := withCluster(t)
	var out, errOut syncBuffer
	code := Execute(t.Context(), update(s, "--exit-code"), Streams{In: strings.NewReader(""), Out: &out, Err: &errOut},
		WithProviders(onVultr(vultrfake.New())), WithAssets(testAssets()), WithNomad(staticNomad()))
	wantResult(t, result{code, out.String(), errOut.String()}, 2, updatePlan, applyHint)
}

// TestUpdateClusterExitCodeWithoutChanges exits with 0 once the cluster is up to date.
func TestUpdateClusterExitCodeWithoutChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()
		if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
			t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
		}
		wantOK(t, runOn(t, f, update(s, "--exit-code")...), "No changes.\n")
	})
}

func TestUpdateClusterExitCodeNeedsAPlan(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	wantError(t, runOn(t, f, update(s, "--yes", "--exit-code")...),
		"Error: --exit-code works only without --yes\n")
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
	s.want(t, prodObjects)
}

func TestUpdateClusterApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()

		got := runOn(t, f, update(s, "--yes")...)

		if got.code != 0 || got.out != built {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and the plan and what it did\n%s", got.code, got.out, built)
		}
		wantBuildProgress(t, got.errOut, openAPILine)
		wantInstances(t, f, nodeNames...)
		if len(f.VPCs()) != 1 || len(f.FirewallGroups()) != 2 {
			t.Errorf("the VPCs are %+v and the firewall groups %+v, want one and two", f.VPCs(), f.FirewallGroups())
		}
		if _, ok := s.objects(t)["prod/cluster.completed.yaml"]; !ok {
			t.Error("the update did not write the completed spec")
		}

		wantOK(t, runOn(t, f, update(s, "--yes")...), "cluster prod is up to date\n")
	})
}

// wantNoSecrets fails the test unless the store s holds the test cluster's secrets and text shows none of them, in
// any of the forms that secrettest.Shows looks for. Its messages name the secret, never its content.
func wantNoSecrets(t *testing.T, s state, what, text string) {
	t.Helper()
	objs := s.objects(t)
	secrets := map[string][]byte{}
	for _, p := range secretPaths {
		secret, ok := objs[p]
		if !ok {
			t.Fatalf("the store holds no %s", p)
		}
		secrets[p] = []byte(secret)
	}
	secrettest.CheckHidden(t, map[string]string{what: text}, secrets, "")
}

// TestUpdateClusterShowsNoSecrets builds the test cluster in each output format with debug logs, then plans an update
// and a delete of it: neither stdout nor stderr shows a secret that the build wrote.
func TestUpdateClusterShowsNoSecrets(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := withCluster(t)
				f := vultrfake.New()
				for _, args := range [][]string{
					update(s, "--yes", "-o", format, "-vv"),
					update(s, "-o", format, "-vv", "--log-format", "json"),
					{"delete", "cluster", "prod", "-o", format, "-vv", "--state", s.url},
				} {
					got := runOn(t, f, args...)
					if got.code != 0 {
						t.Fatalf("%s: exit code %d\n%s", strings.Join(args, " "), got.code, got.errOut)
					}
					if !strings.Contains(got.errOut, "opened the state store") {
						t.Errorf("%s: stderr holds no debug log:\n%s", strings.Join(args, " "), got.errOut)
					}
					wantNoSecrets(t, s, strings.Join(args, " "), got.out+got.errOut)
				}
			})
		})
	}
}

// TestUpdateClusterExitCodeSeesAMissingSecret exits with 2 when the plan writes only a secret that the store lacks.
func TestUpdateClusterExitCodeSeesAMissingSecret(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		if err := s.open(t).Delete(t.Context(), "prod/secrets/gossip.key"); err != nil {
			t.Fatal(err)
		}
		wantResult(t, runOn(t, f, update(s, "--exit-code")...), 2, "State: secrets/gossip.key will be written.\n",
			applyHint)
	})
}

// openAPILine is the warning that the Nomad API is open, without its line end.
var openAPILine = strings.TrimSuffix(openAPIWarning, "\n")

// progressEvent is a progress line of -o json.
type progressEvent struct {
	Type, Event, Step, Kind, Name, Action, ID, Address, Wait, Cause, Error, Leader string
	Voters                                                                         int
}

// decodeProgress decodes the progress lines of -o json, one JSON object each, and skips the other lines of stderr,
// which do not start with {.
func decodeProgress(t *testing.T, errOut string) []progressEvent {
	t.Helper()
	var events []progressEvent
	for line := range strings.Lines(errOut) {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e progressEvent
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("a progress line is not an event: %v\n%s", err, line)
		}
		events = append(events, e)
	}
	return events
}

func TestUpdateClusterApplyJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()

		got := runOn(t, f, update(s, "--yes", "-o", "json")...)

		if got.code != 0 {
			t.Fatalf("exit code = %d\n%s", got.code, got.errOut)
		}
		if first, _, _ := strings.Cut(got.errOut, "\n"); first != openAPILine {
			t.Errorf("stderr starts with %q, want the warning %q", first, openAPILine)
		}
		plan := decodePlan(t, got.out)
		wantPlanJSON(t, plan)
		if !plan.Applied {
			t.Error("the plan does not say it was applied")
		}
		events := decodeProgress(t, got.errOut)
		var infra, nodes, nomad []progressEvent
		for _, e := range events {
			switch e.Type {
			case "node":
				nodes = append(nodes, e)
			case "nomad":
				nomad = append(nomad, e)
			default:
				infra = append(infra, e)
			}
		}
		var wantInfra []progressEvent
		for _, k := range infraKeys {
			kind, name, _ := strings.Cut(k, "/")
			for _, event := range []string{"started", "succeeded"} {
				wantInfra = append(wantInfra, progressEvent{
					Type: "infrastructure", Event: event, Kind: kind, Name: name, Action: "create",
				})
			}
		}
		byKey := func(a, b progressEvent) int {
			return strings.Compare(a.Kind+a.Name+a.Event, b.Kind+b.Name+b.Event)
		}
		slices.SortFunc(wantInfra, byKey)
		slices.SortFunc(infra, byKey)
		if diff := cmp.Diff(wantInfra, infra); diff != "" {
			t.Errorf("the infrastructure's events (-want +got):\n%s", diff)
		}
		var wantNodes []progressEvent
		for i, n := range nodeNames {
			if n == "prod-workers-0" { // the servers are scrubbed before the first client is made
				for j, server := range nodeNames[:3] {
					id := fmt.Sprintf("instance-%d", j+1)
					wantNodes = append(wantNodes,
						progressEvent{Type: "node", Step: "started", Action: "scrub", Name: server, ID: id},
						progressEvent{Type: "node", Step: "done", Action: "scrub", Name: server, ID: id})
				}
			}
			wantNodes = append(wantNodes,
				progressEvent{Type: "node", Step: "started", Action: "create", Name: n},
				progressEvent{Type: "node", Step: "done", Action: "create", Name: n,
					ID: fmt.Sprintf("instance-%d", i+1), Address: fmt.Sprintf("10.64.0.%d", i+3)})
			if i >= 3 { // a client is scrubbed once it has registered
				id := fmt.Sprintf("instance-%d", i+1)
				wantNodes = append(wantNodes,
					progressEvent{Type: "node", Step: "started", Action: "scrub", Name: n, ID: id},
					progressEvent{Type: "node", Step: "done", Action: "scrub", Name: n, ID: id})
			}
		}
		if diff := cmp.Diff(wantNodes, nodes); diff != "" {
			t.Errorf("the nodes' events (-want +got):\n%s", diff)
		}
		wantNomad := []progressEvent{
			{Type: "nomad", Step: "started", Action: "leader"},
			{Type: "nomad", Step: "done", Action: "leader", Leader: "10.64.0.3:4647"},
			{Type: "nomad", Step: "started", Action: "bootstrap"},
			{Type: "nomad", Step: "done", Action: "bootstrap"},
			{Type: "nomad", Step: "started", Action: "healthy", Voters: 3},
			{Type: "nomad", Step: "done", Action: "healthy", Voters: 3},
		}
		for _, n := range nodeNames[3:] {
			wantNomad = append(wantNomad,
				progressEvent{Type: "nomad", Step: "started", Action: "register", Name: n},
				progressEvent{Type: "nomad", Step: "done", Action: "register", Name: n})
		}
		if diff := cmp.Diff(wantNomad, nomad); diff != "" {
			t.Errorf("the Nomad events (-want +got):\n%s", diff)
		}
		if i := slices.IndexFunc(events, func(e progressEvent) bool { return e.Type == "nomad" }); i != len(infra)+6 {
			t.Errorf("the first Nomad event is event %d, want %d: after the three servers", i, len(infra)+6)
		}
		if i := slices.IndexFunc(events, func(e progressEvent) bool { return e.Type == "node" }); i != len(infra) {
			t.Errorf("the first node event is event %d, want %d: after the infrastructure's", i, len(infra))
		}

		got = runOn(t, f, update(s, "--yes", "-o", "json")...)
		const upToDate = `{
  "applied": true,
  "infrastructure": {
    "changes": [],
    "summary": {
      "create": 0,
      "update": 0,
      "replace": 0,
      "delete": 0
    }
  },
  "nodes": []
}
`
		wantOK(t, got, upToDate)
	})
}

// TestUpdateClusterFails stops at a node that fails to create: the progress says so, the error follows, and stdout
// holds the plan alone.
func TestUpdateClusterFails(t *testing.T) {
	const cause = "create node prod-servers-0 of cluster prod: vultr: POST /v2/instances: 400 Bad Request: " +
		"Invalid plan."
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()
		f.Fail(t, "CreateInstance",
			vultr.NewAPIError(http.MethodPost, "/v2/instances", http.StatusBadRequest, "Invalid plan.", 0), 1)

		got := runOn(t, f, update(s, "--yes")...)

		if got.code != 1 || got.out != updatePlan {
			t.Errorf("exit code = %d, stdout\n%s\nwant 1 and the plan alone", got.code, got.out)
		}
		infra, _ := buildLines()
		want := slices.Concat([]string{openAPILine}, infra, []string{
			"creating node prod-servers-0",
			"failed to create node prod-servers-0: " + cause,
			"Error: " + cause,
		})
		lines := strings.Split(strings.TrimSuffix(got.errOut, "\n"), "\n")
		if len(lines) == len(want) {
			slices.Sort(lines[1 : 1+len(infra)])
		}
		if diff := cmp.Diff(want, lines); diff != "" {
			t.Errorf("stderr (-want +got):\n%s", diff)
		}
		wantInstances(t, f)
	})
}

// pendingWorker leaves the test cluster built on f, in the store s, with its worker prod-workers-2 created anew as
// instance-7 but not ready yet, as an update that stopped while the node booted leaves it. Call it in a synctest
// bubble.
func pendingWorker(t *testing.T, s state, f *vultrfake.Fake) {
	t.Helper()
	if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
		t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
	}
	if err := f.DeleteInstance(t.Context(), "instance-6"); err != nil {
		t.Fatal(err)
	}
	f.SetBootReads(t, 100, 100)
	f.Fail(t, "GetInstance", instanceLost("instance-7"), 1)
	if got := runOn(t, f, update(s, "--yes")...); got.code != 1 {
		t.Fatalf("update --yes: exit code %d, want 1: the read fails\n%s", got.code, got.errOut)
	}
}

// instanceLost is the error of a read of the instance id that Vultr refuses.
func instanceLost(id string) error {
	return vultr.NewAPIError(http.MethodGet, "/v2/instances/"+id, http.StatusBadRequest, "Invalid instance.", 0)
}

// waitPlan is the plan of an update that waits for the worker that pendingWorker left.
const waitPlan = "~ node prod-workers-2 (ID instance-7, wait until it joins, scrub its user data)\n" +
	"\n" +
	"Nodes: 0 to create, 1 to wait for, 0 to delete.\n"

// TestUpdateClusterWaits waits for a node that an interrupted update created, and prints its address once it is
// ready.
func TestUpdateClusterWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()
		pendingWorker(t, s, f)
		wantResult(t, runOn(t, f, update(s, "--yes")...), 0,
			waitPlan+"\nNodes: 0 created, 1 waited for, 0 deleted.\n",
			openAPIWarning+"waiting for node prod-workers-2\nnode prod-workers-2 (10.64.0.8) is ready\n"+
				"waiting for node prod-workers-2 to register\nnode prod-workers-2 registered\n"+
				"scrubbing the user data of node prod-workers-2\nscrubbed the user data of node prod-workers-2\n")
	})
}

// TestUpdateClusterFailedWait prints the provider's error in the progress, and the error with the node it waited for
// last.
func TestUpdateClusterFailedWait(t *testing.T) {
	const cause = "create node prod-workers-2 of cluster prod: wait for instance instance-7: vultr: GET " +
		"/v2/instances/instance-7: 400 Bad Request: Invalid instance."
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		f := vultrfake.New()
		pendingWorker(t, s, f)
		f.Fail(t, "GetInstance", instanceLost("instance-7"), 1)
		wantResult(t, runOn(t, f, update(s, "--yes")...), 1, waitPlan,
			openAPIWarning+"waiting for node prod-workers-2\n"+
				"failed to wait for node prod-workers-2: "+cause+"\n"+
				"Error: wait for node prod-workers-2: "+cause+"\n")
	})
}

// countBefore returns how many of calls are calls of the vultr.API method name before the first call of the method
// first, or in all of calls when none is.
func countBefore(calls []vultrfake.Call, name, first string) int {
	n := 0
	for _, c := range calls {
		if c.Name == first {
			break
		}
		if c.Name == name {
			n++
		}
	}
	return n
}

// TestUpdateClusterPlansTwice plans once without the lock and once under it, and prints the plan made under the
// lock: each plan checks the specs against the cloud once.
func TestUpdateClusterPlansTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		if err := f.DeleteInstance(t.Context(), "instance-6"); err != nil {
			t.Fatal(err)
		}
		before := len(f.Calls())
		got := runOn(t, f, update(s, "--yes")...)
		if got.code != 0 {
			t.Fatalf("exit code = %d\n%s", got.code, got.errOut)
		}
		if n := countBefore(f.Calls()[before:], "AvailablePlans", ""); n != 2 {
			t.Errorf("update --yes checked the specs against the cloud %d times, want 2", n)
		}
	})
}

// TestUpdateClusterPrintsThePlanItApplies prints the plan made under the lock when the cloud changes after the plan
// without the lock: another node goes as the second plan starts.
func TestUpdateClusterPrintsThePlanItApplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		if err := f.DeleteInstance(t.Context(), "instance-6"); err != nil {
			t.Fatal(err)
		}
		plans := 0
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "AvailablePlans" { // the first call of a plan
				if plans++; plans == 2 {
					f.SetHook(nil)
					if err := f.DeleteInstance(ctx, "instance-5"); err != nil {
						t.Errorf("DeleteInstance: %v", err)
					}
				}
			}
			return next(ctx)
		})

		got := runOn(t, f, update(s, "--yes")...)

		const want = "+ node prod-workers-1 (client, vc2-2c-4gb, ams)\n" +
			"+ node prod-workers-2 (client, vc2-2c-4gb, ams)\n" +
			"\n" +
			"Nodes: 2 to create, 0 to wait for, 0 to delete.\n" +
			"\n" +
			"Nodes: 2 created, 0 waited for, 0 deleted.\n"
		if got.code != 0 || got.out != want {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and\n%s", got.code, got.out, want)
		}
		wantInstances(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0", "prod-workers-1",
			"prod-workers-2")
	})
}

// TestUpdateClusterRefusesToDeleteAJoinedNode fails a scale down of workers that joined, with and without --yes and
// with --exit-code, as an error with exit code 1, and changes nothing.
func TestUpdateClusterRefusesToDeleteAJoinedNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		s.put(t, workersPath, strings.Replace(workersYAML, "size: 3", "size: 2", 1))
		before := s.objects(t)
		calls := len(f.Calls())
		const want = "Error: update would delete a node that joined Nomad: prod-workers-2 (ID instance-6, surplus); " +
			"tent cannot drain a node or remove a server yet, so update deletes only nodes that never joined; " +
			"keep this node in the specs, or delete the whole cluster with tent delete cluster\n"

		for _, more := range [][]string{nil, {"--yes"}, {"--exit-code"}} {
			wantError(t, runOn(t, f, update(s, more...)...), want)
		}

		wantNoWritesIn(t, f.Calls()[calls:])
		wantInstances(t, f, nodeNames...)
		s.want(t, before)
	})
}

// lockLost is the error of a change that was saved although its lock was lost at the end.
const lockLost = "Error: the change is saved, but the lock of cluster prod was lost before tent released it"

// withoutWorkers returns the plan or the summary text of the test cluster's build as it reads for the servers alone:
// no line of a worker, and three nodes instead of six.
func withoutWorkers(text string) string {
	var lines []string
	for line := range strings.Lines(text) {
		if !strings.Contains(line, "prod-workers-") {
			lines = append(lines, line)
		}
	}
	return strings.NewReplacer("Nodes: 6 to create", "Nodes: 3 to create", "Nodes: 6 created", "Nodes: 3 created").
		Replace(strings.Join(lines, ""))
}

// TestBuildLosesItsLock prints what update --yes and create --yes did, and fails with the error that says the change
// is saved, when the lock is lost after the update has written the mark of the Nomad bootstrap. The cluster has no
// workers: the mark is then the last write, and a node created after the lock was lost would stop the run.
func TestBuildLosesItsLock(t *testing.T) {
	serversOnly := nodeNames[:3]
	builtServers := withoutWorkers(built)
	for _, tc := range []struct {
		name   string
		stored bool // the store holds the test cluster, with no worker, before the run
		args   func(s state) []string
		check  func(t *testing.T, out string)
	}{
		{"update", true, func(s state) []string { return update(s, "--yes") }, func(t *testing.T, out string) {
			if out != builtServers {
				t.Errorf("stdout\n%s\nwant the plan and what it did\n%s", out, builtServers)
			}
		}},
		{"update -o json", true, func(s state) []string { return update(s, "--yes", "-o", "json") },
			func(t *testing.T, out string) {
				plan := decodePlan(t, out)
				wantPlanJSONOf(t, plan, serversOnly)
				if !plan.Applied {
					t.Error("the plan does not say it was applied")
				}
			}},
		{"create", false, func(s state) []string { return createProd(s, "--yes", "--workers", "0") },
			func(t *testing.T, out string) {
				if want := createdProd + builtServers; out != want {
					t.Errorf("stdout\n%s\nwant the create's lines, the plan and what it did\n%s", out, want)
				}
			}},
		{"create -o json", false, func(s state) []string { return createProd(s, "--yes", "--workers", "0", "-o", "json") },
			func(t *testing.T, out string) {
				var got struct {
					Changes []changeOutput  `json:"changes"`
					Update  json.RawMessage `json:"update"`
				}
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("stdout is not the create's JSON: %v\n%s", err, out)
				}
				if len(got.Changes) != 3 {
					t.Errorf("the changes are %v, want the Cluster and two node groups", got.Changes)
				}
				if plan := decodePlan(t, string(got.Update)); !plan.Applied {
					t.Error("the update's plan does not say it was applied")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newState(t)
				if tc.stored {
					s.put(t, clusterPath, clusterYAML)
					s.put(t, serversPath, serversYAML)
					s.put(t, workersPath, strings.Replace(workersYAML, "size: 3", "size: 0", 1))
				}
				f := vultrfake.New()
				losing := func(st statestore.Store) statestore.Store {
					return losingStore{Store: st, after: "prod/nomad/bootstrapped"}
				}
				opts := &globalOptions{
					openStore: wrapped(losing), providers: onVultr(f), assets: testAssets(), nomad: staticNomad(),
				}
				got := runWith(t, opts, tc.args(s)...)

				if got.code != 1 || !strings.HasSuffix(got.errOut, "\n"+lockLost+"\n") {
					t.Errorf("exit code = %d, stderr\n%s\nwant 1 and the error\n%s", got.code, got.errOut, lockLost)
				}
				tc.check(t, got.out)
				wantInstances(t, f, serversOnly...)
				if _, ok := s.objects(t)["prod/nomad/bootstrapped"]; !ok {
					t.Error("the update did not write the mark of the Nomad bootstrap")
				}
			})
		})
	}
}

// counting returns providers that reach the Vultr fake f and count the calls of each provider name in n.
func counting(f *vultrfake.Fake, n map[v1alpha1.Provider]int) Providers {
	p := onVultr(f)
	return func(name v1alpha1.Provider, log *slog.Logger) (cloud.Provider, error) {
		n[name]++
		return p(name, log)
	}
}

// TestCommandsAskForTheProviderOnce builds one provider for each command, however often it plans.
func TestCommandsAskForTheProviderOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newState(t)
		f := vultrfake.New()
		for _, args := range [][]string{
			createProd(s, "--yes"),
			update(s),
			update(s, "--yes"),
			{"delete", "cluster", "prod", "--state", s.url},
			{"delete", "cluster", "prod", "--yes", "--state", s.url},
		} {
			n := map[v1alpha1.Provider]int{}
			if got := runProviders(t, counting(f, n), args...); got.code != 0 {
				t.Fatalf("%s: exit code %d\n%s", strings.Join(args, " "), got.code, got.errOut)
			}
			if want := (map[v1alpha1.Provider]int{v1alpha1.ProviderVultr: 1}); !cmp.Equal(want, n) {
				t.Errorf("%s asked for the providers %v, want vultr once", strings.Join(args, " "), n)
			}
		}
	})
}

func TestUpdateClusterInvalidSpec(t *testing.T) {
	s := newState(t)
	s.put(t, clusterPath, clusterYAML)
	s.put(t, serversPath, replaced(t, serversYAML, "size: 3", "size: 1"))
	f := vultrfake.New()
	wantError(t, runOn(t, f, update(s)...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: size 1 needs --allow-single-server\n")
	got := runOn(t, f, update(s, "--allow-single-server")...)
	if got.code != 0 || !strings.Contains(got.out, "+ node prod-servers-0 (server, vc2-2c-4gb, ams)\n") {
		t.Errorf("exit code = %d, stdout\n%s\nstderr %q; want 0 and the plan", got.code, got.out, got.errOut)
	}
}

// TestUpdateClusterPreflight prints the problems that the provider finds as an invalid spec.
func TestUpdateClusterPreflight(t *testing.T) {
	s := newState(t)
	s.put(t, clusterPath, clusterYAML)
	s.put(t, serversPath, replaced(t, serversYAML, "vc2-2c-4gb", "vc2-99c-1tb"))
	got := runOnCloud(t, update(s)...)
	if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, "Error: invalid spec:\n  NodeGroup servers: ") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 1, nothing and the invalid spec", got.code, got.out,
			got.errOut)
	}
}

func TestUpdateClusterProviderErrors(t *testing.T) {
	s := withCluster(t)
	noKey := func(v1alpha1.Provider, *slog.Logger) (cloud.Provider, error) {
		return nil, errors.New("VULTR_API_KEY is not set")
	}
	wantError(t, runProviders(t, noKey, update(s)...), "Error: VULTR_API_KEY is not set\n")
	wantError(t, runProviders(t, noKey, update(s, "--yes")...), "Error: VULTR_API_KEY is not set\n")

	wantError(t, runOnCloud(t, "update", "cluster", "prod", "--state", hetznerCluster(t).url),
		"Error: tent cannot manage clusters on hetzner yet\n")
}

// hetznerCluster returns a store that holds the test cluster on Hetzner, with its servers.
func hetznerCluster(t *testing.T) state {
	t.Helper()
	s := newState(t)
	s.put(t, clusterPath, replaced(t, replaced(t, clusterYAML, "provider: vultr", "provider: hetzner"),
		"region: ams\n    vultr: {}", "region: eu-central\n    zones: [fsn1, nbg1, hel1]\n    hetzner: {}"))
	s.put(t, serversPath, replaced(t, serversYAML, "vc2-2c-4gb", "cx23"))
	return s
}

// TestUpdateClusterLogsToTheCommandsLogger gives the providers the logger of -v and --log-format.
func TestUpdateClusterLogsToTheCommandsLogger(t *testing.T) {
	s := withCluster(t)
	probe := func(name v1alpha1.Provider, log *slog.Logger) (cloud.Provider, error) {
		log.Info("probe", "provider", string(name))
		return nil, errors.New("stop")
	}
	got := runProviders(t, probe, update(s, "-v", "--log-format", "json")...)
	if got.code != 1 || !strings.Contains(got.errOut, `"msg":"probe","provider":"vultr"}`) {
		t.Errorf("exit code = %d, stderr\n%s\nwant 1 and the probe's log in JSON", got.code, got.errOut)
	}
}

func TestUpdateClusterHelp(t *testing.T) {
	got := runOnCloud(t, "update", "cluster", "--help")
	for _, want := range []string{
		"Usage:\n  tent update cluster [NAME] [flags]\n",
		"--yes", "--exit-code", "--allow-single-server", "VULTR_API_KEY",
		"It also makes the cluster's missing CA, gossip key and ACL bootstrap secret in the state store, and never " +
			"replaces them.",
		"It starts Nomad on the nodes, bootstraps its ACL system and waits for the servers to be healthy and the " +
			"clients to register.",
		"TENT_NODE_URL and TENT_NODE_SHA256",
	} {
		if got.code != 0 || !strings.Contains(got.out, want) {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and it to hold %q", got.code, got.out, want)
		}
	}
	if strings.Contains(got.out, "without Nomad") {
		t.Errorf("stdout\n%s\nwant no claim that the nodes run without Nomad", got.out)
	}
}
