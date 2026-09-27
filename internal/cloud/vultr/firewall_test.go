package vultr_test

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
	"github.com/ingvarch/tent/internal/model"
)

// Rules of the example cluster's firewall groups, as the tests write them: every node takes SSH from 203.0.113.7/32
// and ICMP from anywhere, and the servers take the Nomad API from anywhere too.
var (
	serverRules = []string{"v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v4 tcp 203.0.113.7/32 22", "v6 icmp ::/0"}
	clientRules = []string{"v4 icmp 0.0.0.0/0", "v4 tcp 203.0.113.7/32 22", "v6 icmp ::/0"}
)

// firewallDescription returns the description that tent gives cluster prod's firewall group for role, server or
// client, when the create carries the operation id op.
func firewallDescription(role, op string) string {
	return "tent:cluster=prod;kind=firewall;role=" + role + ";op=" + op
}

// exampleCluster returns the cluster prod of the architecture's example: in ams, SSH open to 203.0.113.7/32, the
// Nomad API left at its default, open to all, and one SSH key.
func exampleCluster() *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
		Metadata: v1alpha1.ClusterMeta{Name: "prod"},
		Spec: v1alpha1.ClusterSpec{
			Cloud:   v1alpha1.Cloud{Provider: v1alpha1.ProviderVultr, Region: "ams", Vultr: &v1alpha1.VultrCloud{}},
			Access:  v1alpha1.Access{SSH: []string{"203.0.113.7/32"}},
			SSHKeys: []string{opsKey},
			Nomad:   v1alpha1.ClusterNomad{Version: "2.0.7"},
		},
	}
}

// exampleClusterWith returns the example cluster changed by edit.
func exampleClusterWith(edit func(c *v1alpha1.Cluster)) *v1alpha1.Cluster {
	c := exampleCluster()
	edit(c)
	return c
}

// nodeGroup returns a node group of three machines of cluster prod.
func nodeGroup(name string, role v1alpha1.Role) *v1alpha1.NodeGroup {
	return &v1alpha1.NodeGroup{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
		Metadata: v1alpha1.NodeGroupMeta{Name: name, Cluster: "prod"},
		Spec:     v1alpha1.NodeGroupSpec{Role: role, MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04", Size: 3},
	}
}

// exampleGroups returns the node groups of the example: servers, and workers with the client role.
func exampleGroups() []*v1alpha1.NodeGroup {
	return []*v1alpha1.NodeGroup{nodeGroup("servers", v1alpha1.RoleServer), nodeGroup("workers", v1alpha1.RoleClient)}
}

// combinedGroups returns one node group with the combined role.
func combinedGroups() []*v1alpha1.NodeGroup {
	return []*v1alpha1.NodeGroup{nodeGroup("dev", v1alpha1.RoleCombined)}
}

// modelOf returns the model of the cluster c with groups. It stops the test when the model fails.
func modelOf(t *testing.T, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) *model.Cluster {
	t.Helper()
	m, err := model.New(c, groups)
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	return m
}

// newFirewallFixture returns the firewall group tasks of the cluster c with groups but without SSH keys, on an empty
// fake, and every infrastructure kind. The provider's operation ids are op-1, op-2 and so on; op-1 goes to the VPC
// task, which the fixture leaves out.
func newFirewallFixture(t *testing.T, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) *fixture {
	t.Helper()
	x := newFixture()
	x.tasks = firewallTasks(t, x, c, groups)
	x.kinds = x.p.InfraKinds()
	return x
}

// firewallTasks returns the firewall group tasks that the fixture's provider builds for the cluster c with groups but
// without SSH keys. It stops the test when the build fails.
func firewallTasks(t *testing.T, x *fixture, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []engine.Task {
	t.Helper()
	m := modelOf(t, c, groups)
	m.SSHKeys = nil
	tasks, err := infraTasks(t, x.p, *m, "vultr.FirewallGroup")
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	return tasks
}

// firewallGroup is a firewall group as the tests check it: its description and its rules, each as
// "<ip_type> <protocol> <subnet>/<subnet_size>", then the port and "source=<source>" when the rule has them.
type firewallGroup struct {
	Description string
	Rules       []string
}

// wantFirewallGroups checks the descriptions and rules of the fake's firewall groups, both in any order.
func wantFirewallGroups(t *testing.T, f *vultrfake.Fake, want ...firewallGroup) {
	t.Helper()
	var got []firewallGroup
	for _, g := range f.FirewallGroups() {
		got = append(got, firewallGroup{Description: g.Description, Rules: ruleTexts(f.FirewallRules(g.ID))})
	}
	opts := cmp.Options{
		cmpopts.SortSlices(func(a, b firewallGroup) bool { return a.Description < b.Description }),
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
		cmpopts.EquateEmpty(),
	}
	if diff := cmp.Diff(want, got, opts); diff != "" {
		t.Errorf("firewall groups (-want +got):\n%s", diff)
	}
}

// ruleTexts returns the text of each rule as firewallGroup has it, in the rules' order.
func ruleTexts(rules []govultr.FirewallRule) []string {
	texts := make([]string, 0, len(rules))
	for _, r := range rules {
		s := fmt.Sprintf("%s %s %s/%d", r.IPType, r.Protocol, r.Subnet, r.SubnetSize)
		if r.Port != "" {
			s += " " + r.Port
		}
		if r.Source != "" {
			s += " source=" + r.Source
		}
		texts = append(texts, s)
	}
	return texts
}

func TestFirewallTasks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups []*v1alpha1.NodeGroup
		want   []engine.Key
	}{
		// A combined group counts as servers.
		{name: "a combined group", groups: combinedGroups(), want: []engine.Key{serversKey}},
		{name: "servers only", groups: exampleGroups()[:1], want: []engine.Key{serversKey}},
		{name: "servers and clients", groups: exampleGroups(), want: []engine.Key{serversKey, clientsKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, exampleCluster(), tc.groups)
			if diff := cmp.Diff(tc.want, keysOf(x.tasks)); diff != "" {
				t.Errorf("keys (-want +got):\n%s", diff)
			}
			for _, task := range x.tasks {
				if deps := task.Deps(); len(deps) != 0 {
					t.Errorf("%s depends on %v, want nothing", task.Key(), deps)
				}
			}
		})
	}
}

func TestFirewallTasksApplyReplan(t *testing.T) {
	ipv6 := exampleClusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Access.SSH = []string{"2001:db8::/48", "203.0.113.7/32"}
		c.Spec.Access.API = []string{"198.51.100.0/24", "2001:db8:1::/48"}
	})
	for _, tc := range []struct {
		name    string
		cluster *v1alpha1.Cluster
		groups  []*v1alpha1.NodeGroup
		want    []firewallGroup
	}{
		{
			name:    "a combined group",
			cluster: exampleCluster(),
			groups:  combinedGroups(),
			want:    []firewallGroup{{Description: firewallDescription("server", "op-2"), Rules: serverRules}},
		},
		{
			name:    "servers and clients",
			cluster: exampleCluster(),
			groups:  exampleGroups(),
			want: []firewallGroup{
				{Description: firewallDescription("server", "op-2"), Rules: serverRules},
				{Description: firewallDescription("client", "op-3"), Rules: clientRules},
			},
		},
		{
			name:    "IPv6 sources",
			cluster: ipv6,
			groups:  exampleGroups(),
			want: []firewallGroup{
				{Description: firewallDescription("server", "op-2"), Rules: []string{
					"v4 icmp 0.0.0.0/0", "v6 icmp ::/0",
					"v4 tcp 203.0.113.7/32 22", "v6 tcp 2001:db8::/48 22",
					"v4 tcp 198.51.100.0/24 4646", "v6 tcp 2001:db8:1::/48 4646",
				}},
				{Description: firewallDescription("client", "op-3"), Rules: []string{
					"v4 icmp 0.0.0.0/0", "v6 icmp ::/0", "v4 tcp 203.0.113.7/32 22", "v6 tcp 2001:db8::/48 22",
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, tc.cluster, tc.groups)
			enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
			wantFirewallGroups(t, x.f, tc.want...)
			if got := countCalls(x.f, "CreateFirewallGroup"); got != len(tc.want) {
				t.Errorf("%d CreateFirewallGroup calls, want %d", got, len(tc.want))
			}
		})
	}
}

// planText returns the text of the fixture's plan against a fresh inventory. It stops the test when the plan fails.
func planText(t *testing.T, x *fixture) string {
	t.Helper()
	p, err := x.plan(t)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var b strings.Builder
	if err := p.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestFirewallTaskAccessChange(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), exampleGroups())
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)

	// SSH moves from 203.0.113.7/32 to a v4 and a v6 network.
	changed := exampleClusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Access.SSH = []string{"198.51.100.0/24", "2001:db8::/48"}
	})
	x.tasks = firewallTasks(t, x, changed, exampleGroups())
	const want = "~ vultr.FirewallGroup/prod-servers\n" +
		"    + rule: v4 tcp 198.51.100.0/24 22\n" +
		"    - rule: v4 tcp 203.0.113.7/32 22\n" +
		"    + rule: v6 tcp 2001:db8::/48 22\n" +
		"~ vultr.FirewallGroup/prod-clients\n" +
		"    + rule: v4 tcp 198.51.100.0/24 22\n" +
		"    - rule: v4 tcp 203.0.113.7/32 22\n" +
		"    + rule: v6 tcp 2001:db8::/48 22\n" +
		"\n" +
		"Plan: 0 to create, 2 to update, 0 to replace, 0 to delete.\n"
	if diff := cmp.Diff(want, planText(t, x)); diff != "" {
		t.Errorf("plan text (-want +got):\n%s", diff)
	}

	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	ssh := []string{"v4 tcp 198.51.100.0/24 22", "v6 tcp 2001:db8::/48 22"}
	wantFirewallGroups(t, x.f,
		firewallGroup{
			Description: firewallDescription("server", "op-2"),
			Rules:       append([]string{"v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v6 icmp ::/0"}, ssh...),
		},
		firewallGroup{
			Description: firewallDescription("client", "op-3"),
			Rules:       append([]string{"v4 icmp 0.0.0.0/0", "v6 icmp ::/0"}, ssh...),
		},
	)
	if got := countCalls(x.f, "CreateFirewallGroup"); got != 2 {
		t.Errorf("%d CreateFirewallGroup calls, want the first apply's 2", got)
	}
}

// ruleOf returns the rule that text gives in the form of ruleTexts, without a source.
func ruleOf(t *testing.T, text string) govultr.FirewallRule {
	t.Helper()
	f := strings.Fields(text)
	if len(f) < 3 {
		t.Fatalf("rule %q: want <ip_type> <protocol> <subnet>/<subnet_size> [<port>]", text)
	}
	subnet, size, _ := strings.Cut(f[2], "/")
	n, err := strconv.Atoi(size)
	if err != nil {
		t.Fatalf("rule %q: %v", text, err)
	}
	r := govultr.FirewallRule{IPType: f[0], Protocol: f[1], Subnet: subnet, SubnetSize: n}
	if len(f) > 3 {
		r.Port = f[3]
	}
	return r
}

// rulesOf returns the rules that texts give, as ruleOf reads them.
func rulesOf(t *testing.T, texts ...string) []govultr.FirewallRule {
	t.Helper()
	rules := make([]govultr.FirewallRule, 0, len(texts))
	for _, text := range texts {
		rules = append(rules, ruleOf(t, text))
	}
	return rules
}

// seedServersGroup stores cluster prod's firewall group for its servers with rules, as an earlier run left it, and
// returns the group.
func seedServersGroup(t *testing.T, f *vultrfake.Fake, rules ...govultr.FirewallRule) govultr.FirewallGroup {
	t.Helper()
	g := f.AddFirewallGroup(t, govultr.FirewallGroup{Description: firewallDescription("server", "op-earlier")})
	for _, r := range rules {
		f.AddFirewallRule(t, g.ID, r)
	}
	return g
}

func TestFirewallTaskDeletesRulesItDoesNotWant(t *testing.T) {
	withID := func(r govultr.FirewallRule, id int) govultr.FirewallRule {
		r.ID = id
		return r
	}
	fromCloudflare := ruleOf(t, "v4 tcp 203.0.113.7/32 22")
	fromCloudflare.Source = "cloudflare"
	for _, tc := range []struct {
		name    string
		rules   []govultr.FirewallRule // the group's rules in Vultr, in the order Vultr lists them
		diff    string                 // the plan's diff lines
		deleted []string               // the Arg of each DeleteFirewallRule call
	}{
		{
			name:    "a rule added by hand",
			rules:   append(rulesOf(t, serverRules...), ruleOf(t, "v4 tcp 0.0.0.0/0 80")),
			diff:    "    - rule: v4 tcp 0.0.0.0/0 80\n",
			deleted: []string{"firewall-1/5"},
		},
		{
			// The copy with the lowest ID stays, whatever the order of the list.
			name: "a second copy of a wanted rule",
			rules: []govultr.FirewallRule{
				withID(ruleOf(t, "v4 icmp 0.0.0.0/0"), 2), withID(ruleOf(t, "v4 tcp 0.0.0.0/0 4646"), 3),
				withID(ruleOf(t, "v4 tcp 203.0.113.7/32 22"), 4), withID(ruleOf(t, "v6 icmp ::/0"), 5),
				withID(ruleOf(t, "v4 tcp 203.0.113.7/32 22"), 1),
			},
			diff:    "    - rule: v4 tcp 203.0.113.7/32 22\n",
			deleted: []string{"firewall-1/4"},
		},
		{
			// A range of two ports is not the rule of one of them.
			name: "a port range",
			rules: append(rulesOf(t, "v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v6 icmp ::/0"),
				ruleOf(t, "v4 tcp 203.0.113.7/32 22:23")),
			diff: "    + rule: v4 tcp 203.0.113.7/32 22\n" +
				"    - rule: v4 tcp 203.0.113.7/32 22:23\n",
			deleted: []string{"firewall-1/4"},
		},
		{
			// A rule with a source opens the port to that source only, such as Cloudflare's addresses.
			name: "a wanted rule with a source",
			rules: append(rulesOf(t, "v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v6 icmp ::/0"),
				fromCloudflare),
			diff: "    + rule: v4 tcp 203.0.113.7/32 22\n" +
				"    - rule: v4 tcp 203.0.113.7/32 22 source=cloudflare\n",
			deleted: []string{"firewall-1/4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, exampleCluster(), combinedGroups())
			seedServersGroup(t, x.f, tc.rules...)
			want := "~ vultr.FirewallGroup/prod-servers\n" + tc.diff +
				"\nPlan: 0 to create, 1 to update, 0 to replace, 0 to delete.\n"
			if diff := cmp.Diff(want, planText(t, x)); diff != "" {
				t.Errorf("plan text (-want +got):\n%s", diff)
			}
			enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
			wantFirewallGroups(t, x.f,
				firewallGroup{Description: firewallDescription("server", "op-earlier"), Rules: serverRules})
			var deleted []string
			for _, c := range x.f.Calls() {
				if c.Name == "DeleteFirewallRule" {
					deleted = append(deleted, c.Arg)
				}
			}
			if diff := cmp.Diff(tc.deleted, deleted); diff != "" {
				t.Errorf("deleted rules (-want +got):\n%s", diff)
			}
		})
	}
}

// ruleCalls returns the CreateFirewallRule and DeleteFirewallRule calls that reached f, in order, each as its name
// and Arg.
func ruleCalls(f *vultrfake.Fake) []string {
	var calls []string
	for _, c := range f.Calls() {
		if c.Name == "CreateFirewallRule" || c.Name == "DeleteFirewallRule" {
			calls = append(calls, c.Name+" "+c.Arg)
		}
	}
	return calls
}

func TestFirewallTaskAddsBeforeItDeletes(t *testing.T) {
	const (
		add    = "CreateFirewallRule firewall-1 v4 tcp 198.51.100.0/24 22"
		remove = "DeleteFirewallRule firewall-1/3" // v4 tcp 203.0.113.7/32 22
	)
	for _, tc := range []struct {
		name string
		max  int // the group's max_rule_count; it holds 4 rules
		want []string
	}{
		// SSH stays open while its source changes.
		{name: "room for the additions", max: 50, want: []string{add, remove}},
		{name: "room for exactly the additions", max: 5, want: []string{add, remove}},
		// A full group has no room for the new rule until the old one is gone.
		{name: "no room for the additions", max: 4, want: []string{remove, add}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sshFromNetwork := exampleClusterWith(func(c *v1alpha1.Cluster) {
				c.Spec.Access.SSH = []string{"198.51.100.0/24"}
			})
			x := newFirewallFixture(t, sshFromNetwork, combinedGroups())
			g := x.f.AddFirewallGroup(t, govultr.FirewallGroup{
				Description: firewallDescription("server", "op-earlier"), MaxRuleCount: tc.max,
			})
			for _, r := range rulesOf(t, serverRules...) {
				x.f.AddFirewallRule(t, g.ID, r)
			}
			enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
			if diff := cmp.Diff(tc.want, ruleCalls(x.f)); diff != "" {
				t.Errorf("rule calls (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFirewallTaskMakesRoomWhenVultrRefuses checks that a group whose real limit is lower than the one it reports
// still gets the task's rules: when Vultr refuses an addition as over the limit, the task deletes the rules it does
// not want and adds again.
func TestFirewallTaskMakesRoomWhenVultrRefuses(t *testing.T) {
	const (
		add    = "CreateFirewallRule firewall-1 v4 tcp 198.51.100.0/24 22"
		remove = "DeleteFirewallRule firewall-1/3" // v4 tcp 203.0.113.7/32 22
	)
	sshFromNetwork := exampleClusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Access.SSH = []string{"198.51.100.0/24"}
	})
	x := newFirewallFixture(t, sshFromNetwork, combinedGroups())
	g := x.f.AddFirewallGroup(t, govultr.FirewallGroup{Description: firewallDescription("server", "op-earlier")})
	for _, r := range rulesOf(t, serverRules...) {
		x.f.AddFirewallRule(t, g.ID, r)
	}
	full := vultr.NewAPIError(http.MethodPost, "/v2/firewalls/"+g.ID+"/rules", http.StatusBadRequest,
		"You have reached the maximum number of rules for this firewall group", 0)
	x.f.Fail(t, "CreateFirewallRule", full, 1)
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	if diff := cmp.Diff([]string{add, remove, add}, ruleCalls(x.f)); diff != "" {
		t.Errorf("rule calls (-want +got):\n%s", diff)
	}
}

func TestFirewallTaskReadsRulesAsVultrWritesThem(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	g := seedServersGroup(t, x.f,
		govultr.FirewallRule{IPType: "V4", Protocol: "ICMP", Subnet: "0.0.0.0", Port: "0"},
		govultr.FirewallRule{IPType: "v4", Protocol: "TCP", Subnet: "0.0.0.0", Port: "4646"},
		// A range of one port is that port.
		govultr.FirewallRule{IPType: "v4", Protocol: "tcp", Subnet: "203.0.113.7", SubnetSize: 32, Port: "22:22"},
		govultr.FirewallRule{IPType: "v6", Protocol: "icmp", Subnet: "0:0:0:0:0:0:0:0"},
	)
	x.wantAdopted(t, g.ID, "CreateFirewallGroup")
	for _, call := range []string{"CreateFirewallRule", "DeleteFirewallRule"} {
		if got := countCalls(x.f, call); got != 0 {
			t.Errorf("%d %s calls, want none", got, call)
		}
	}
}

// sshFrom returns the example cluster with SSH open to n networks, 198.51.100.0/32 and so on.
func sshFrom(n int) *v1alpha1.Cluster {
	return exampleClusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Access.SSH = nil
		for i := range n {
			c.Spec.Access.SSH = append(c.Spec.Access.SSH, fmt.Sprintf("198.51.100.%d/32", i))
		}
	})
}

func TestFirewallTaskRuleLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster *v1alpha1.Cluster
		max     int // the group's max_rule_count in Vultr; 0 when the cluster has no group yet
		want    string
	}{
		{
			// 50 SSH rules, the two ICMP rules and the API rule.
			name: "a new group", cluster: sshFrom(50),
			want: "plan vultr.FirewallGroup/prod-servers: firewall group prod-servers needs 53 rules; Vultr allows 50",
		},
		{name: "a new group with as many rules as Vultr allows", cluster: sshFrom(47)},
		{
			name: "a group that holds fewer", cluster: exampleCluster(), max: 3,
			want: "plan vultr.FirewallGroup/prod-servers: firewall group prod-servers needs 4 rules; Vultr allows 3",
		},
		{name: "a group that holds as many", cluster: exampleCluster(), max: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, tc.cluster, combinedGroups())
			if tc.max > 0 {
				x.f.AddFirewallGroup(t, govultr.FirewallGroup{
					Description: firewallDescription("server", "op-earlier"), MaxRuleCount: tc.max,
				})
			}
			_, err := x.plan(t)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("NewPlan: %v, want success", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("NewPlan error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFirewallTaskLostGroupCreate(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	x.f.LoseResponse(t, "CreateFirewallGroup", 1)
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	wantFirewallGroups(t, x.f, firewallGroup{Description: firewallDescription("server", "op-2"), Rules: serverRules})
	if got := countCalls(x.f, "CreateFirewallGroup"); got != 1 {
		t.Errorf("%d CreateFirewallGroup calls, want 1", got)
	}
}

func TestFirewallTaskLostCreateAndFailedSearch(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	// The second attempt searches, finds the group and does not create it again.
	description := firewallDescription("server", "op-2")
	x.wantSearchAfterLostCreate(t, vultrfake.Call{Name: "CreateFirewallGroup", Arg: description},
		"ListFirewallGroups", "/v2/firewalls")
	wantFirewallGroups(t, x.f, firewallGroup{Description: description, Rules: serverRules})
}

func TestFirewallTaskRetries(t *testing.T) {
	serverError := func(method, path string) error {
		return vultr.NewAPIError(method, path, http.StatusInternalServerError, "Internal error", 0)
	}
	for _, tc := range []struct {
		name    string
		extra   bool // the cluster's servers' group exists and holds a rule that tent does not want
		faults  func(tb testing.TB, f *vultrfake.Fake)
		wait    time.Duration // the wait before the retry; 0 for a backoff
		creates int           // the CreateFirewallRule calls
		deletes int           // the DeleteFirewallRule calls
	}{
		{
			// A rule create without an answer is sent again only after the next attempt lists the rules.
			name:    "a lost rule create",
			faults:  func(tb testing.TB, f *vultrfake.Fake) { f.LoseResponse(tb, "CreateFirewallRule", 1) },
			creates: 4,
		},
		{
			// Vultr did not carry out the throttled create, so the retry sends it again.
			name:    "a throttled rule create",
			faults:  func(tb testing.TB, f *vultrfake.Fake) { f.Throttle(tb, "CreateFirewallRule", 7*time.Second, 1) },
			wait:    7 * time.Second,
			creates: 5,
		},
		{
			name: "a failed rule list",
			faults: func(tb testing.TB, f *vultrfake.Fake) {
				f.Fail(tb, "ListFirewallRules", serverError(http.MethodGet, "/v2/firewalls/firewall-1/rules"), 1)
			},
			creates: 4,
		},
		{
			name:    "a lost rule delete",
			extra:   true,
			faults:  func(tb testing.TB, f *vultrfake.Fake) { f.LoseResponse(tb, "DeleteFirewallRule", 1) },
			deletes: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, exampleCluster(), combinedGroups())
			description := firewallDescription("server", "op-2")
			if tc.extra {
				g := seedServersGroup(t, x.f, rulesOf(t, append(serverRules, "v4 tcp 0.0.0.0/0 80")...)...)
				description = g.Description
			}
			events := x.applyWithFaults(t, func(tb testing.TB) { tc.faults(tb, x.f) })
			var retries []time.Duration
			for _, e := range events {
				if e.Type == engine.Retrying {
					retries = append(retries, e.Wait)
				}
			}
			switch {
			case len(retries) != 1:
				t.Errorf("the engine retried %d times, want once", len(retries))
			case tc.wait > 0 && retries[0] != tc.wait:
				t.Errorf("the engine waited %v before the retry, want %v", retries[0], tc.wait)
			}
			wantFirewallGroups(t, x.f, firewallGroup{Description: description, Rules: serverRules})
			groupCreates := 1 // a retry does not create the group again
			if tc.extra {
				groupCreates = 0 // the group exists
			}
			if got := countCalls(x.f, "CreateFirewallGroup"); got != groupCreates {
				t.Errorf("%d CreateFirewallGroup calls, want %d", got, groupCreates)
			}
			if got := countCalls(x.f, "CreateFirewallRule"); got != tc.creates {
				t.Errorf("%d CreateFirewallRule calls, want %d", got, tc.creates)
			}
			if got := countCalls(x.f, "DeleteFirewallRule"); got != tc.deletes {
				t.Errorf("%d DeleteFirewallRule calls, want %d", got, tc.deletes)
			}
		})
	}
}

func TestFirewallTaskOutputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose bool // the create's answer is lost
	}{
		{name: "created"},
		{name: "adopted after a lost answer", lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, exampleCluster(), combinedGroups())
			if tc.lose {
				x.f.LoseResponse(t, "CreateFirewallGroup", 1)
			}
			id := x.createdID(t)
			if groups := x.f.FirewallGroups(); len(groups) != 1 || id != groups[0].ID {
				t.Errorf("output id = %q, and the fake holds %+v; want the id of its one firewall group", id, groups)
			}
		})
	}
}

func TestFirewallTaskDeleteOfAGoneRule(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	seedServersGroup(t, x.f, rulesOf(t, append(serverRules, "v4 tcp 0.0.0.0/0 80")...)...)
	events, err := x.applyInBubble(t, func(tb testing.TB) {
		gone := vultr.NewAPIError(http.MethodDelete, "/v2/firewalls/firewall-1/rules/5", http.StatusNotFound,
			"Invalid firewall rule ID.", 0)
		x.f.Fail(tb, "DeleteFirewallRule", gone, 1)
	})
	if err != nil {
		t.Errorf("apply: %v, want success", err)
	}
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
}

func TestFirewallTaskGroupHoldsItsMostRules(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	events, err := x.applyInBubble(t, func(tb testing.TB) {
		full := vultr.NewAPIError(http.MethodPost, "/v2/firewalls/firewall-1/rules", http.StatusBadRequest,
			"You have reached the maximum number of rules for this firewall group.", 0)
		// Refused in both orders: before and after the deletions, of which a new group has none.
		x.f.Fail(tb, "CreateFirewallRule", full, 2)
	})
	// Vultr does not raise the most rules of a group on request, so the error does not say how to raise a limit.
	const want = "vultr.FirewallGroup/prod-servers: add rule v4 icmp 0.0.0.0/0: firewall group prod-servers may " +
		"already hold the most rules Vultr allows in a group: vultr: POST /v2/firewalls/firewall-1/rules: " +
		"400 Bad Request: You have reached the maximum number of rules for this firewall group."
	if err == nil || err.Error() != want {
		t.Errorf("apply error = %v, want %q", err, want)
	}
	if !errors.Is(err, vultr.ErrLimitReached) {
		t.Errorf("errors.Is(%v, vultr.ErrLimitReached) = false", err)
	}
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
}

func TestFirewallGroupDeleteWhileInstancesAttached(t *testing.T) {
	x := newFixture()
	x.kinds = x.p.InfraKinds()
	g := seedServersGroup(t, x.f, rulesOf(t, serverRules...)...)
	// What Vultr answers here is not verified; any answer that matches vultr.ErrInUse is retried.
	attached := vultr.NewAPIError(http.MethodDelete, "/v2/firewalls/"+g.ID, http.StatusConflict,
		"Firewall group has attached instances", 0)
	events := x.applyWithFaults(t, func(tb testing.TB) { x.f.Fail(tb, "DeleteFirewallGroup", attached, 2) })
	var retries int
	for _, e := range events {
		if e.Type != engine.Retrying {
			continue
		}
		retries++
		if !errors.Is(e.Err, vultr.ErrInUse) {
			t.Errorf("the engine retried after %v, want an error that matches vultr.ErrInUse", e.Err)
		}
	}
	if retries != 2 {
		t.Errorf("the engine retried %d times, want twice", retries)
	}
	if got := countCalls(x.f, "DeleteFirewallGroup"); got != 3 {
		t.Errorf("%d DeleteFirewallGroup calls, want 3", got)
	}
	wantFirewallGroups(t, x.f)
}

func TestFirewallGroupDeleteOfAGoneGroup(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	obj := engine.Object{Key: serversKey, ID: "firewall-9"}
	if err := deleterOf(t, p, "vultr.FirewallGroup").Delete(t.Context(), &engine.Env{}, obj); err != nil {
		t.Errorf("Delete of a firewall group that is gone: %v, want success", err)
	}
	wantCalls(t, f, vultrfake.Call{Name: "DeleteFirewallGroup", Arg: "firewall-9"})
}
