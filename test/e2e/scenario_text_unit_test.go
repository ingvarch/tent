package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

func scenarioSettings() settings {
	return settings{
		Region: "ams", Plan: "vc2-1c-1gb", Images: []string{"ubuntu-24.04", "ubuntu-26.04"}, UpgradeFrom: "2.0.6",
	}
}

func TestHACreateArgsBuildThreeServersAndTwoWorkersOnTheFirstImage(t *testing.T) {
	args := haCreateArgs(scenarioSettings(), "e2e-abc123-ha", "/k/id.pub", "203.0.113.7/32")
	want := []string{
		"create", "cluster", "e2e-abc123-ha",
		"--provider", "vultr", "--region", "ams", "--machine-type", "vc2-1c-1gb",
		"--servers", "3", "--workers", "2",
		"--image", "ubuntu-24.04", "--ssh-key", "/k/id.pub",
		"--ssh-access", "203.0.113.7/32", "--api-access", "203.0.113.7/32", "--yes",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%v\nwant\n%v", args, want)
	}
	if slices.Contains(args, "--allow-single-server") || slices.Contains(args, "--combined") {
		t.Errorf("args %v allow a single server or a combined group", args)
	}
}

func TestUpgradeCreateArgsBuildACombinedGroupOfThreeOnTheOldVersionAndTheLastImage(t *testing.T) {
	args := upgradeCreateArgs(scenarioSettings(), "e2e-abc123-upgrade", "/k/id.pub", "203.0.113.7/32")
	want := []string{
		"create", "cluster", "e2e-abc123-upgrade",
		"--provider", "vultr", "--region", "ams", "--machine-type", "vc2-1c-1gb",
		"--combined", "--servers", "3", "--nomad-version", "2.0.6",
		"--image", "ubuntu-26.04", "--ssh-key", "/k/id.pub",
		"--ssh-access", "203.0.113.7/32", "--api-access", "203.0.113.7/32", "--yes",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%v\nwant\n%v", args, want)
	}
	if slices.Contains(args, "--workers") {
		t.Errorf("args %v name workers, which a combined group refuses", args)
	}
}

func decodeCluster(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("the result is not one JSON object: %v\n%s", err, data)
	}
	return out
}

func readClusterJSON(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster.json")
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return data
}

func TestWithRollChangeSetsBothPartsOfTheExtraConfigAndKeepsTheRest(t *testing.T) {
	out, err := withRollChange(readClusterJSON(t), "abc123")
	if err != nil {
		t.Fatalf("withRollChange: %v", err)
	}
	got := decodeCluster(t, out)
	spec := got["spec"].(map[string]any)
	extra := spec["nomad"].(map[string]any)["extraConfig"].(map[string]any)
	if extra["server"] != "# e2e roll abc123\n" {
		t.Errorf("server = %q", extra["server"])
	}
	wantClient := "client {\n  meta {\n    e2e_roll = \"abc123\"\n  }\n}\n"
	if extra["client"] != wantClient {
		t.Errorf("client = %q, want %q", extra["client"], wantClient)
	}
	if len(extra) != 2 || len(spec["nomad"].(map[string]any)) != 1 {
		t.Errorf("spec.nomad = %v, want extraConfig with server and client alone", spec["nomad"])
	}
	if got["apiVersion"] != "tent/v1alpha1" || got["kind"] != "Cluster" ||
		got["metadata"].(map[string]any)["name"] != "e2e-abc123-ha" {
		t.Errorf("the header of the cluster changed: %v", got)
	}
	cloud := spec["cloud"].(map[string]any)
	if cloud["provider"] != "vultr" || cloud["region"] != "ams" || len(spec) != 2 {
		t.Errorf("the rest of the spec changed: %v", spec)
	}
}

func TestWithRollChangeKeepsOtherFieldsOfNomadAndNumbers(t *testing.T) {
	in := `[{"spec":{"nomad":{"version":"2.0.7"},"big":12345678901234567890}}]`
	out, err := withRollChange([]byte(in), "r")
	if err != nil {
		t.Fatalf("withRollChange: %v", err)
	}
	spec := decodeCluster(t, out)["spec"].(map[string]any)
	if spec["nomad"].(map[string]any)["version"] != "2.0.7" {
		t.Errorf("spec.nomad.version was lost: %v", spec["nomad"])
	}
	if _, ok := spec["nomad"].(map[string]any)["extraConfig"]; !ok {
		t.Errorf("spec.nomad has no extraConfig: %v", spec["nomad"])
	}
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("a number lost its digits:\n%s", out)
	}
}

func TestWithRollChangeRefusesWhatTheSuiteDidNotMake(t *testing.T) {
	for name, in := range map[string]string{
		"an empty array":                   `[]`,
		"two clusters":                     `[{"spec":{}},{"spec":{}}]`,
		"not an array":                     `{"spec":{}}`,
		"not JSON":                         `spec: {}`,
		"an element that is not an object": `[1]`,
		"no spec":                          `[{"metadata":{}}]`,
		"a spec that is a list":            `[{"spec":[]}]`,
		"a nomad that is a string":         `[{"spec":{"nomad":"x"}}]`,
		"an extraConfig":                   `[{"spec":{"nomad":{"extraConfig":{"server":"x"}}}}]`,
		"an empty extraConfig":             `[{"spec":{"nomad":{"extraConfig":{}}}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := withRollChange([]byte(in), "r"); err == nil {
				t.Errorf("withRollChange accepted %s and gave\n%s", in, out)
			}
		})
	}
}

func TestScenarioScriptsAreTheOnesTheRestartStepRuns(t *testing.T) {
	if got := bootIDScript(); got != "cat /proc/sys/kernel/random/boot_id" {
		t.Errorf("bootIDScript = %q", got)
	}
	if got := rebootScript(); got != "systemd-run --on-active=2 --unit=e2e-reboot systemctl reboot" {
		t.Errorf("rebootScript = %q", got)
	}
	if got := joinFileScript(); got != "cat /etc/nomad.d/05-join.hcl" {
		t.Errorf("joinFileScript = %q", got)
	}
	want := "journalctl -b -u nomad --no-pager -o cat | grep 'node registration complete' || true"
	if got := registrationScript(); got != want {
		t.Errorf("registrationScript = %q, want %q", got, want)
	}
}

func TestParseBootID(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	got, err := parseBootID(" " + id + "\n")
	if err != nil || got != id {
		t.Errorf("parseBootID = %q, %v; want %q", got, err, id)
	}
	for name, in := range map[string]string{
		"empty":       "",
		"blank":       " \n",
		"two lines":   id + "\n" + id + "\n",
		"not a UUID":  "boot-1",
		"too long":    id + "0",
		"upper case":  strings.ToUpper(id),
		"a banner":    "Warning: Permanently added\n" + id,
		"no dashes":   strings.ReplaceAll(id, "-", ""),
		"bad letters": "0f8fad5b-d9cb-469f-a165-70867728950g",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := parseBootID(in); err == nil {
				t.Errorf("parseBootID(%q) = %q, want an error", in, got)
			}
		})
	}
}

func TestRegistrationLinesKeepsOnlyTheRegistrationLines(t *testing.T) {
	out := "\nclient: node registration complete\n  \nsomething else\nclient: node registration complete\n"
	got := registrationLines(out)
	want := []string{"client: node registration complete", "client: node registration complete"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registrationLines = %q, want %q", got, want)
	}
	if got := registrationLines(""); len(got) != 0 {
		t.Errorf("registrationLines of nothing = %q", got)
	}
}

const (
	nodesAnswer = `[{"ID":"n1","Name":"c-1","Status":"ready","SchedulingEligibility":"eligible",` +
		`"Version":"2.0.7","Address":"10.0.0.5","Drain":false},` +
		`{"ID":"n2","Name":"c-2","Status":"down","SchedulingEligibility":"ineligible","Version":"2.0.6",` +
		`"Address":"10.0.0.6"}]`
	membersAnswer = `{"ServerName":"s.global","Members":[` +
		`{"Name":"s-1.global","Addr":"10.0.0.1","Port":4648,"Status":"alive",` +
		`"Tags":{"build":"2.0.7","role":"nomad"}},` +
		`{"Name":"s-2.global","Addr":"10.0.0.2","Port":4648,"Status":"left","Tags":null}]}`
)

func TestListNodesReadsTheFieldsTheScenariosUse(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/nodes": fixed(200, nodesAnswer)})
	got, err := listNodes(context.Background(), n)
	if err != nil {
		t.Fatalf("listNodes: %v", err)
	}
	want := []nomadNode{
		{ID: "n1", Name: "c-1", Status: "ready", SchedulingEligibility: "eligible", Version: "2.0.7",
			Address: "10.0.0.5"},
		{ID: "n2", Name: "c-2", Status: "down", SchedulingEligibility: "ineligible", Version: "2.0.6",
			Address: "10.0.0.6"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listNodes = %+v, want %+v", got, want)
	}
}

func TestListNodesNamesTheRequestOnAnError(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/nodes": fixed(500, "boom")})
	if _, err := listNodes(context.Background(), n); err == nil || !strings.Contains(err.Error(), "GET /v1/nodes") {
		t.Errorf("listNodes error = %v, want one that names the request", err)
	}
}

func TestNodeMetaReadsTheMetaOfOneNode(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/node/n1":    fixed(200, `{"ID":"n1","Meta":{"e2e_roll":"abc123","connect.sidecar_image":"x"}}`),
		"GET /v1/node/n2":    fixed(200, `{"ID":"n2","Meta":null}`),
		"GET /v1/node/a%2Fb": fixed(200, `{"Meta":{"k":"v"}}`),
	})
	got, err := nodeMeta(context.Background(), n, "n1")
	if err != nil || got["e2e_roll"] != "abc123" || len(got) != 2 {
		t.Errorf("nodeMeta(n1) = %v, %v", got, err)
	}
	if got, err := nodeMeta(context.Background(), n, "n2"); err != nil || len(got) != 0 {
		t.Errorf("nodeMeta(n2) = %v, %v; want no meta and no error", got, err)
	}
	if got, err := nodeMeta(context.Background(), n, "a/b"); err != nil || got["k"] != "v" {
		t.Errorf("nodeMeta(a/b) = %v, %v; want the ID escaped in the path", got, err)
	}
	if _, err := nodeMeta(context.Background(), n, "missing"); err == nil {
		t.Errorf("nodeMeta(missing) succeeded on a 404")
	}
}

func TestListMembersReadsTheMembersOfTheAnswer(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/agent/members": fixed(200, membersAnswer)})
	got, err := listMembers(context.Background(), n)
	if err != nil {
		t.Fatalf("listMembers: %v", err)
	}
	want := []agentMember{
		{Name: "s-1.global", Addr: "10.0.0.1", Status: "alive",
			Tags: map[string]string{"build": "2.0.7", "role": "nomad"}},
		{Name: "s-2.global", Addr: "10.0.0.2", Status: "left"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listMembers = %+v, want %+v", got, want)
	}
}

func TestListMembersNamesTheRequestOnAnError(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/agent/members": fixed(403, "denied")})
	if _, err := listMembers(context.Background(), n); err == nil ||
		!strings.Contains(err.Error(), "GET /v1/agent/members") {
		t.Errorf("listMembers error = %v, want one that names the request", err)
	}
}

func TestRunningAllocationsCountsTheRunningOnesOfTheJob(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations": fixed(200, `[{"ID":"a","ClientStatus":"running"},`+
			`{"ID":"b","ClientStatus":"pending"},{"ID":"c","ClientStatus":"running"},`+
			`{"ID":"d","ClientStatus":"complete"}]`),
		"GET /v1/job/none/allocations":  fixed(200, `[]`),
		"GET /v1/job/a%2Fb/allocations": fixed(200, `[{"ID":"a","ClientStatus":"running"}]`),
	})
	if got, err := runningAllocations(context.Background(), n, "e2e-web"); err != nil || got != 2 {
		t.Errorf("runningAllocations(e2e-web) = %d, %v; want 2", got, err)
	}
	if got, err := runningAllocations(context.Background(), n, "none"); err != nil || got != 0 {
		t.Errorf("runningAllocations(none) = %d, %v; want 0", got, err)
	}
	if got, err := runningAllocations(context.Background(), n, "a/b"); err != nil || got != 1 {
		t.Errorf("runningAllocations(a/b) = %d, %v; want 1 with the job escaped in the path", got, err)
	}
	if _, err := runningAllocations(context.Background(), n, "missing"); err == nil {
		t.Errorf("runningAllocations(missing) succeeded on a 404")
	}
}

func machine(id, label, role, cluster string) vultrapi.Instance {
	return vultrapi.Instance{
		ID: id, Label: label, MainIP: "192.0.2.1",
		Tags: []string{"tent/cluster=" + cluster, "tent/role=" + role},
	}
}

func upgradeMachines() []vultrapi.Instance {
	return []vultrapi.Instance{
		machine("i1", "up-nodes-4", "combined", "up"),
		machine("i2", "up-nodes-5", "combined", "up"),
		machine("i3", "up-nodes-6", "combined", "up"),
	}
}

func readyNodes(version string) []nomadNode {
	return []nomadNode{
		{ID: "n4", Name: "up-nodes-4", Status: "ready", SchedulingEligibility: "eligible", Version: version},
		{ID: "n5", Name: "up-nodes-5", Status: "ready", SchedulingEligibility: "eligible", Version: version},
		{ID: "n6", Name: "up-nodes-6", Status: "ready", SchedulingEligibility: "eligible", Version: version},
	}
}

func aliveMembers(version string) []agentMember {
	var out []agentMember
	for _, name := range []string{"up-nodes-4", "up-nodes-5", "up-nodes-6"} {
		out = append(out, agentMember{
			Name: name + ".global", Status: "alive", Tags: map[string]string{"build": version},
		})
	}
	return out
}

func TestCheckVersionsPassesWhenEveryNodeAndMemberIsOnTheVersion(t *testing.T) {
	if err := checkVersions(readyNodes("2.0.7"), aliveMembers("2.0.7"), upgradeMachines(), "2.0.7"); err != nil {
		t.Errorf("checkVersions: %v", err)
	}
}

func TestCheckVersionsIgnoresMembersThatAreNotAlive(t *testing.T) {
	members := append(aliveMembers("2.0.7"),
		agentMember{Name: "up-nodes-1.global", Status: "left", Tags: map[string]string{"build": "2.0.6"}},
		agentMember{Name: "up-nodes-2.global", Status: "failed"})
	if err := checkVersions(readyNodes("2.0.7"), members, upgradeMachines(), "2.0.7"); err != nil {
		t.Errorf("checkVersions: %v", err)
	}
}

func TestCheckVersionsNamesEveryMismatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   func([]nomadNode) []nomadNode
		members func([]agentMember) []agentMember
		want    []string
	}{
		{"a node on the old version", func(n []nomadNode) []nomadNode { n[1].Version = "2.0.6"; return n }, nil,
			[]string{"up-nodes-5", "version 2.0.6"}},
		{"a node that is down", func(n []nomadNode) []nomadNode { n[0].Status = "down"; return n }, nil,
			[]string{"up-nodes-4", "status down"}},
		{"a node that is ineligible", func(n []nomadNode) []nomadNode {
			n[2].SchedulingEligibility = "ineligible"
			return n
		}, nil, []string{"up-nodes-6", "ineligible"}},
		{"a node of a machine that is gone", func(n []nomadNode) []nomadNode {
			n[0].Name = "up-nodes-1"
			return n
		}, nil, []string{"up-nodes-1", "up-nodes-4"}},
		{"a missing node", func(n []nomadNode) []nomadNode { return n[:2] }, nil,
			[]string{"2 nodes", "3 machines"}},
		{"a member on the old version", nil, func(m []agentMember) []agentMember {
			m[1].Tags = map[string]string{"build": "2.0.6"}
			return m
		}, []string{"up-nodes-5", "build 2.0.6"}},
		{"a member without a build tag", nil, func(m []agentMember) []agentMember { m[0].Tags = nil; return m },
			[]string{"up-nodes-4", "build none"}},
		{"a missing member", nil, func(m []agentMember) []agentMember { return m[:2] },
			[]string{"2 alive members", "3 machines", "up-nodes-6"}},
		{"a member of no machine", nil, func(m []agentMember) []agentMember {
			m[2].Name = "old-9.global"
			return m
		}, []string{"old-9.global", "up-nodes-6"}},
		{"a member named without a region", nil, func(m []agentMember) []agentMember {
			m[0].Name = "up-nodes-4"
			return m
		}, []string{"up-nodes-4", "no region"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, members, machines := readyNodes("2.0.7"), aliveMembers("2.0.7"), upgradeMachines()
			if tc.nodes != nil {
				nodes = tc.nodes(nodes)
			}
			if tc.members != nil {
				members = tc.members(members)
			}
			err := checkVersions(nodes, members, machines, "2.0.7")
			if err == nil {
				t.Fatalf("checkVersions passed")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestCheckVersionsReportsAllProblemsAndTheOtherMembersApart(t *testing.T) {
	nodes := readyNodes("2.0.6")
	members := append(aliveMembers("2.0.6"), agentMember{Name: "up-nodes-1.global", Status: "left"})
	err := checkVersions(nodes, members, upgradeMachines(), "2.0.7")
	if err == nil {
		t.Fatal("checkVersions passed")
	}
	msg := err.Error()
	if strings.Count(msg, "version 2.0.6") != 3 || strings.Count(msg, "build 2.0.6") != 3 {
		t.Errorf("error %q does not name all six mismatches", msg)
	}
	if !strings.Contains(msg, "other members: up-nodes-1.global (left)") {
		t.Errorf("error %q does not list the other members apart", msg)
	}
}

func TestCheckVersionsWithoutMachinesFails(t *testing.T) {
	if err := checkVersions(nil, nil, nil, "2.0.7"); err == nil {
		t.Error("checkVersions passed with no machine")
	}
}

func TestCheckRolledNamesTheMachinesThatAreStillListed(t *testing.T) {
	before := []vultrapi.Instance{{ID: "a", Label: "x-1"}, {ID: "b", Label: "x-2"}}
	if err := checkRolled(before, []vultrapi.Instance{{ID: "c", Label: "x-3"}, {ID: "d", Label: "x-4"}}); err != nil {
		t.Errorf("checkRolled: %v", err)
	}
	err := checkRolled(before, []vultrapi.Instance{{ID: "b", Label: "x-2"}, {ID: "c", Label: "x-3"}})
	if err == nil || !strings.Contains(err.Error(), "x-2") || strings.Contains(err.Error(), "x-1") {
		t.Errorf("checkRolled = %v, want an error that names x-2 alone", err)
	}
	if err := checkRolled(nil, nil); err == nil {
		t.Error("checkRolled passed with nothing before; the machines to roll were never recorded")
	}
}

func TestCheckKeptNamesTheMachinesThatAreGone(t *testing.T) {
	kept := []vultrapi.Instance{{ID: "a", Label: "w-1"}, {ID: "b", Label: "w-2"}}
	if err := checkKept(kept, []vultrapi.Instance{{ID: "b"}, {ID: "a"}, {ID: "z"}}); err != nil {
		t.Errorf("checkKept: %v", err)
	}
	err := checkKept(kept, []vultrapi.Instance{{ID: "a"}})
	if err == nil || !strings.Contains(err.Error(), "w-2") || strings.Contains(err.Error(), "w-1") {
		t.Errorf("checkKept = %v, want an error that names w-2 alone", err)
	}
	if err := checkKept(nil, nil); err == nil {
		t.Error("checkKept passed with nothing kept; the machines were never recorded")
	}
}

func haMachines() []vultrapi.Instance {
	return []vultrapi.Instance{
		machine("s1", "ha-servers-1", "server", "ha"),
		machine("s2", "ha-servers-2", "server", "ha"),
		machine("s3", "ha-servers-3", "server", "ha"),
		machine("w1", "ha-workers-1", "client", "ha"),
		machine("w2", "ha-workers-2", "client", "ha"),
		machine("o1", "other-servers-1", "server", "other"),
	}
}

func TestCheckCountsCountsTheServersAndWorkersOfTheCluster(t *testing.T) {
	if err := checkCounts(haMachines(), "ha", 3, 2); err != nil {
		t.Errorf("checkCounts: %v", err)
	}
	for name, tc := range map[string]struct{ servers, workers int }{
		"too few servers":  {4, 2},
		"too many servers": {2, 2},
		"too few workers":  {3, 3},
		"too many workers": {3, 1},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkCounts(haMachines(), "ha", tc.servers, tc.workers)
			if err == nil || !strings.Contains(err.Error(), "ha") {
				t.Errorf("checkCounts = %v, want an error that names the cluster", err)
			}
		})
	}
}

func TestCheckCountsNamesBothCountsWhenBothAreWrong(t *testing.T) {
	err := checkCounts(nil, "ha", 3, 2)
	if err == nil {
		t.Fatal("checkCounts passed with no machine")
	}
	for _, want := range []string{"0 servers, want 3", "0 workers, want 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkCounts = %v, want %q named", err, want)
		}
	}
}

func TestServersToDeleteReturnsOnlyTheServersOfTheCluster(t *testing.T) {
	machines := []vultrapi.Instance{
		machine("s1", "a-1", "server", "e2e-abc123-ha"),
		machine("s2", "a-2", "server", "e2e-abc123-ha"),
		machine("w1", "a-w", "client", "e2e-abc123-ha"),
		machine("c1", "a-c", "combined", "e2e-abc123-ha"),
		machine("x1", "b-1", "server", "e2e-abc123-ha2"),
		machine("x2", "b-2", "server", "e2e-abc123"),
		machine("x3", "b-3", "server", "prod"),
		{ID: "u1", Label: "untagged"},
		{ID: "u2", Label: "role-only", Tags: []string{"tent/role=server"}},
		{ID: "u3", Label: "cluster-only", Tags: []string{"tent/cluster=e2e-abc123-ha"}},
	}
	got, err := serversToDelete(machines, "e2e-abc123-ha")
	if err != nil {
		t.Fatalf("serversToDelete: %v", err)
	}
	var ids []string
	for _, in := range got {
		ids = append(ids, in.ID)
	}
	if !reflect.DeepEqual(ids, []string{"s1", "s2"}) {
		t.Errorf("serversToDelete returned %v, want s1 and s2", ids)
	}
}

func TestServersToDeleteRefusesWhatIsNotAnE2ECluster(t *testing.T) {
	machines := []vultrapi.Instance{machine("p1", "p-1", "server", "prod"), machine("e1", "e-1", "server", ""),
		machine("e2", "e-2", "server", "e2e-"), machine("e3", "e-3", "server", "e2e")}
	for _, name := range []string{"", "prod", "e2e-", "e2e"} {
		if got, err := serversToDelete(machines, name); err == nil {
			t.Errorf("serversToDelete(%q) = %v, want an error", name, got)
		}
	}
}

func TestServersToDeleteRefusesAnEmptyResult(t *testing.T) {
	machines := []vultrapi.Instance{machine("w1", "w", "client", "e2e-abc123-ha")}
	got, err := serversToDelete(machines, "e2e-abc123-ha")
	if err == nil || !strings.Contains(err.Error(), "e2e-abc123-ha") {
		t.Errorf("serversToDelete = %v, %v, want an error that names the cluster", got, err)
	}
}

func TestServerAddressesListsTheUsableAddressesOfTheRoleInTheCluster(t *testing.T) {
	all := []vultrapi.Instance{
		instance("s1", "server", "192.0.2.1", "e2e-a"),
		instance("s2", "server", "0.0.0.0", "e2e-a"),
		instance("s3", "server", "", "e2e-a"),
		instance("s4", "server", "192.0.2.4", "e2e-a"),
		instance("c1", "client", "192.0.2.5", "e2e-a"),
		instance("s5", "server", "192.0.2.6", "e2e-b"),
	}
	want := []string{"https://192.0.2.1:4646", "https://192.0.2.4:4646"}
	if got := serverAddresses(all, "e2e-a", "server"); !slices.Equal(got, want) {
		t.Errorf("serverAddresses = %v, want %v", got, want)
	}
	if got := serverAddresses(all, "e2e-c", "server"); len(got) != 0 {
		t.Errorf("serverAddresses of a cluster without machines = %v, want none", got)
	}
}

func TestLowestLabelPicksTheMachineThatSortsFirst(t *testing.T) {
	got, err := lowestLabel([]vultrapi.Instance{{Label: "ha-workers-2"}, {Label: "ha-workers-1"}, {Label: "ha-workers-3"}})
	if err != nil || got.Label != "ha-workers-1" {
		t.Errorf("lowestLabel = %q, %v; want ha-workers-1", got.Label, err)
	}
	if _, err := lowestLabel(nil); err == nil {
		t.Error("lowestLabel passed with no machine")
	}
}

func TestNodeNamedFindsTheNodeOfAMachine(t *testing.T) {
	nodes := []nomadNode{{ID: "n1", Name: "ha-workers-1"}, {ID: "n2", Name: "ha-workers-2"}}
	got, err := nodeNamed(nodes, "ha-workers-2")
	if err != nil || got.ID != "n2" {
		t.Errorf("nodeNamed = %+v, %v; want n2", got, err)
	}
	if _, err := nodeNamed(nodes, "ha-workers-3"); err == nil || !strings.Contains(err.Error(), "ha-workers-3") {
		t.Errorf("nodeNamed of a missing node = %v, want an error that names it", err)
	}
	twin := append(slices.Clone(nodes), nomadNode{ID: "n3", Name: "ha-workers-1"})
	if _, err := nodeNamed(twin, "ha-workers-1"); err == nil {
		t.Error("nodeNamed passed with two nodes of one name")
	}
}

func TestCheckRollMetaNamesTheNodesWithoutTheRunsValue(t *testing.T) {
	good := map[string]map[string]string{"w-1": {"e2e_roll": "abc123"}, "w-2": {"e2e_roll": "abc123", "x": "y"}}
	if err := checkRollMeta(good, "abc123", 2); err != nil {
		t.Errorf("checkRollMeta: %v", err)
	}
	bad := map[string]map[string]string{"w-1": {"e2e_roll": "old"}, "w-2": {}, "w-3": {"e2e_roll": "abc123"}}
	err := checkRollMeta(bad, "abc123", 3)
	if err == nil || !strings.Contains(err.Error(), "w-1") || !strings.Contains(err.Error(), "w-2") ||
		strings.Contains(err.Error(), "w-3") {
		t.Errorf("checkRollMeta = %v, want an error that names w-1 and w-2 alone", err)
	}
	if err := checkRollMeta(good, "abc123", 3); err == nil {
		t.Error("checkRollMeta passed with fewer nodes than wanted")
	}
}

func TestBootProblemWaitsForAnotherBootID(t *testing.T) {
	const before = "0f8fad5b-d9cb-469f-a165-70867728950e"
	const after = "1f8fad5b-d9cb-469f-a165-70867728950e"
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		{"another boot", after + "\n", nil, false},
		{"the same boot", before + "\n", nil, true},
		{"ssh fails", "", errors.New("connection refused"), true},
		{"garbage", "hello", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bootProblem(tc.out, tc.err, before)
			if (got != "") != tc.wantErr {
				t.Errorf("bootProblem = %q, want a problem: %t", got, tc.wantErr)
			}
		})
	}
}

func TestRegisteredProblemNeedsALineAndTheSameNodeReadyAndEligible(t *testing.T) {
	lines := []string{"2026-10-13T10:00:00Z client: node registration complete"}
	ready := []nomadNode{{ID: "n1", Status: "ready", SchedulingEligibility: "eligible"}}
	if got := registeredProblem(lines, ready, "n1"); got != "" {
		t.Errorf("registeredProblem = %q, want none", got)
	}
	for _, tc := range []struct {
		name  string
		lines []string
		nodes []nomadNode
		want  string
	}{
		{"no line", nil, ready, "registration"},
		{"another node ID", lines, []nomadNode{{ID: "n2", Status: "ready", SchedulingEligibility: "eligible"}}, "n1"},
		{"down", lines, []nomadNode{{ID: "n1", Status: "down", SchedulingEligibility: "eligible"}}, "down"},
		{"ineligible", lines, []nomadNode{{ID: "n1", Status: "ready", SchedulingEligibility: "ineligible"}}, "ineligible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registeredProblem(tc.lines, tc.nodes, "n1"); !strings.Contains(got, tc.want) {
				t.Errorf("registeredProblem = %q, want it to hold %q", got, tc.want)
			}
		})
	}
}

func TestRestartRecordHoldsTheEvidenceOfTheRestart(t *testing.T) {
	got := restartRecord(restartEvidence{
		Machine: "ha-workers-1", NodeID: "n1", BootBefore: "b1", BootAfter: "b2",
		BootWait: 90 * time.Second, RegisterWait: 30 * time.Second,
		Registrations: []string{"line one", "line two"}, JoinFile: "retry_join = [\"10.0.0.9\"]\n",
	})
	for _, want := range []string{
		"machine: ha-workers-1", "node ID: n1", "boot ID before: b1", "boot ID after: b2",
		"wait for the new boot: 1m30s", "wait for the registration: 30s",
		"line one\nline two\n", "retry_join = [\"10.0.0.9\"]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("record does not hold %q:\n%s", want, got)
		}
	}
}

func TestAllocationsProblemNeedsExactlyTheWantedRunningAllocations(t *testing.T) {
	if got := allocationsProblem(2, "e2e-web", 2); got != "" {
		t.Errorf("allocationsProblem(2, 2) = %q, want none", got)
	}
	for _, running := range []int{0, 1, 3} {
		got := allocationsProblem(running, "e2e-web", 2)
		if !strings.Contains(got, "e2e-web") || !strings.Contains(got, "want 2") {
			t.Errorf("allocationsProblem(%d, 2) = %q, want a problem that names the job and the count", running, got)
		}
	}
}
