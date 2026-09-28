package app_test

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/statestore"
)

// The integration tests run whole flows of the use cases with the Vultr provider on its fake: golden files of the
// calls that reach Vultr, runs cut at every call, and lost answers at every create.

// The example cluster of the architecture, section 3.1, with 2 workers.
const (
	exampleClusterYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  channel: stable
  cloud:
    provider: vultr
    region: ams
    zones: [ams]
    vultr: {}
  networking:
    cidr: 10.64.0.0/16
  access:
    ssh: [203.0.113.7/32]
    api: [0.0.0.0/0]
  sshKeys:
  - ` + opsKey + `
  nomad:
    version: 2.0.7
    region: global
    tls: {verifyHTTPSClient: true}
    clientIntroduction: strict
    extraConfig:
      server: ""
      client: ""
`
	exampleServersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 3
`
	exampleWorkersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 2
  nomad:
    nodePool: default
    nodeClass: general
    drivers: [docker, exec]
    meta: {team: platform}
`
)

// exampleStore returns a service over a new file store that holds the example cluster, and the store's root.
func exampleStore(t *testing.T) (*app.Service, string) {
	t.Helper()
	svc, root := newService(t)
	mustCreate(t, svc, exampleClusterYAML, exampleServersYAML, exampleWorkersYAML)
	return svc, root
}

// newExample returns a service whose store holds the example cluster, and the empty Vultr fake that its providers
// reach. Call it in a synctest bubble, where an update does not wait in real time.
func newExample(t *testing.T) (*app.Service, *vultrfake.Fake) {
	t.Helper()
	svc, _ := exampleStore(t)
	f, _ := withCloud(svc)
	return svc, f
}

// newExampleFrom is newExample over a copy of the store at the root template, from exampleStore. The store syncs each
// write to disk, which makes writing the specs anew take most of a short test's time; the copy does not sync.
func newExampleFrom(t *testing.T, template string) (*app.Service, *vultrfake.Fake) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.CopyFS(root, os.DirFS(template)); err != nil {
		t.Fatalf("copy the store: %v", err)
	}
	s, err := statestore.Open(t.Context(), fileURL(root))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	svc := &app.Service{Store: s, Version: "dev"}
	f, _ := withCloud(svc)
	return svc, f
}

// buildExample builds the example cluster on an empty fake, in a bubble of its own, and returns the plan and the
// calls of the build, as flowCalls gives them, and the cloud it leaves, as cloudView gives it.
func buildExample(t *testing.T) (plan app.UpdatePlan, calls, view []string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		svc, f := newExample(t)
		plan, calls = updateFlow(t, svc, f)
		view = cloudView(f)
	})
	return plan, calls, view
}

// deleteExample builds the example cluster on an empty fake and deletes it, in a bubble of its own, and returns the
// calls of the delete, as flowCalls gives them. It fails the test unless the delete leaves nothing in the store.
func deleteExample(t *testing.T) (calls []string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		svc, f := newExample(t)
		mustUpdate(t, svc)
		calls = deleteFlow(t, svc, f)
		wantPaths(t, svc.Store)
	})
	return calls
}

var (
	// idPattern matches the ids that the fake gives out.
	idPattern = regexp.MustCompile(`\b(?:ssh-key|vpc|firewall|instance)-[0-9]+\b`)
	// opPattern matches an operation id.
	opPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// firewallNames maps the role in a firewall group's marker to the end of its engine key's name.
var firewallNames = map[string]string{"server": "servers", "client": "clients"}

// markedKey returns the engine key of the object that the marker text marks, such as vultr.SSHKey/prod-8ba890ed, or ""
// when the text is not a marker of an infrastructure object.
func markedKey(text string) string {
	m, ok, err := vultr.ParseMarker(text)
	switch {
	case !ok || err != nil:
		return ""
	case m.Kind == vultr.KindSSHKey:
		return kindSSHKey + "/" + m.Cluster + "-" + m.Fingerprint
	case m.Kind == vultr.KindVPC:
		return kindVPC + "/" + m.Cluster
	case m.Kind == vultr.KindFirewall:
		return kindFirewall + "/" + m.Cluster + "-" + firewallNames[m.Role]
	}
	return ""
}

// names maps the id of each object of f to a name that does not depend on the order the fake made the objects in:
// the engine key of an SSH key, a VPC or a firewall group, and the hostname of an instance.
func names(f *vultrfake.Fake) map[string]string {
	m := map[string]string{}
	for _, k := range f.SSHKeys() {
		m[k.ID] = markedKey(k.Name)
	}
	for _, v := range f.VPCs() {
		m[v.ID] = markedKey(v.Description)
	}
	for _, g := range f.FirewallGroups() {
		m[g.ID] = markedKey(g.Description)
	}
	for _, in := range f.Instances() {
		m[in.ID] = in.Hostname
	}
	return m
}

// line returns the call as "<method> <argument>", each id in the argument replaced by the object's name in angle
// brackets, such as <vultr.VPC/prod>. An id without a name stays.
func line(c vultrfake.Call, names map[string]string) string {
	return idPattern.ReplaceAllStringFunc(strings.TrimSpace(c.Name+" "+c.Arg), func(id string) string {
		if n := names[id]; n != "" {
			return "<" + n + ">"
		}
		return id
	})
}

// callKey returns the line of a call with every operation id masked as <op>, so that it names the same call in
// every run.
func callKey(line string) string { return opPattern.ReplaceAllString(line, "<op>") }

// subject returns the engine key of the object that the call acts on: the object that the marker in its argument
// marks, or the one that the first id in its argument names. It is "" for a call without an object.
func subject(c vultrfake.Call, names map[string]string) string {
	if k := markedKey(c.Arg); k != "" {
		return k
	}
	return names[idPattern.FindString(c.Arg)]
}

// flowCalls runs a use case with svc and returns the calls that reached f, each as line gives it. run carries out the
// use case and returns the infrastructure plan it applied.
//
// The engine applies the changes of a plan in parallel, so the order of their calls differs from run to run. So the
// calls of each run of the engine, from its first event to its last with no node step in between, come grouped by the
// object they act on, in the order of the plan's changes; the calls on one object keep their order, since one change
// makes them one after another. The calls without an object, such as the list of the nodes before a firewall group's
// delete, come first. Every other call keeps its place.
func flowCalls(t *testing.T, svc *app.Service, f *vultrfake.Fake, run func() *engine.Plan) []string {
	t.Helper()
	nm := names(f)
	start := len(f.Calls())
	var runs [][2]int // the engine's runs, as [from, to) in the calls of the flow
	open := false
	svc.OnProgress = func(p app.Progress) {
		n := len(f.Calls()) - start
		switch {
		case p.Infra == nil:
			open = false
		case !open:
			open = true
			runs = append(runs, [2]int{n, n})
		default:
			runs[len(runs)-1][1] = n
		}
	}
	infra := run()
	svc.OnProgress = nil
	maps.Copy(nm, names(f))
	var order []string
	if infra != nil {
		for _, c := range infra.Changes() {
			order = append(order, c.String()) // the key's String
		}
	}
	rank := func(c vultrfake.Call) int {
		s := subject(c, nm)
		if s == "" {
			return -1
		}
		i := slices.Index(order, s)
		if i < 0 {
			t.Errorf("the engine's call %s acts on %s, which its plan does not change", line(c, nm), s)
		}
		return i
	}
	calls := f.Calls()[start:]
	for _, r := range runs {
		slices.SortStableFunc(calls[r[0]:r[1]], func(a, b vultrfake.Call) int { return cmp.Compare(rank(a), rank(b)) })
	}
	lines := make([]string, len(calls))
	for i, c := range calls {
		lines[i] = line(c, nm)
	}
	return lines
}

// updateFlow applies the update of the test cluster and returns its plan and its calls, as flowCalls gives them.
func updateFlow(t *testing.T, svc *app.Service, f *vultrfake.Fake) (app.UpdatePlan, []string) {
	t.Helper()
	var plan app.UpdatePlan
	calls := flowCalls(t, svc, f, func() *engine.Plan {
		plan = mustUpdate(t, svc)
		return plan.Infra
	})
	return plan, calls
}

// deleteFlow deletes the test cluster and returns its calls, as flowCalls gives them.
func deleteFlow(t *testing.T, svc *app.Service, f *vultrfake.Fake) []string {
	t.Helper()
	return flowCalls(t, svc, f, func() *engine.Plan { return mustDelete(t, svc).Infra })
}

// callsText returns the lines of calls, one per line, with the operation ids numbered in the order they first show:
// <op1>, <op2> and so on. A call that repeats one keeps its number.
func callsText(lines []string) string {
	ops := map[string]string{}
	return opPattern.ReplaceAllStringFunc(strings.Join(lines, "\n")+"\n", func(op string) string {
		if _, ok := ops[op]; !ok {
			ops[op] = fmt.Sprintf("<op%d>", len(ops)+1)
		}
		return ops[op]
	})
}

// TestFlowGoldens builds the example cluster on an empty cloud, scales the workers of the built cluster from 2 to 3,
// and deletes the built cluster. The plans of the two updates and the calls that each of the three runs makes to
// Vultr match golden files.
func TestFlowGoldens(t *testing.T) {
	plan, calls, _ := buildExample(t)
	checkGolden(t, "flow_build.plan.golden", planText(t, plan))
	checkGolden(t, "flow_build.calls.golden", callsText(calls))

	synctest.Test(t, func(t *testing.T) {
		svc, f := newExample(t)
		mustUpdate(t, svc)
		mustReplace(t, svc, edit(t, exampleWorkersYAML, "size: 2", "size: 3"))
		plan, calls := updateFlow(t, svc, f)
		checkGolden(t, "flow_scale.plan.golden", planText(t, plan))
		checkGolden(t, "flow_scale.calls.golden", callsText(calls))
	})

	checkGolden(t, "flow_delete.calls.golden", callsText(deleteExample(t)))
}
