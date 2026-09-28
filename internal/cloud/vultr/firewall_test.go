package vultr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
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
// "<ip_type> <protocol> <subnet>/<subnet_size>", then the port when the rule has one and "source=<source>" when it has
// a source other than its own subnet.
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
		subnet := fmt.Sprintf("%s/%d", r.Subnet, r.SubnetSize)
		s := r.IPType + " " + r.Protocol + " " + subnet
		if r.Port != "" {
			s += " " + r.Port
		}
		if r.Source != "" && r.Source != subnet { // the fake lists a rule without a source with its subnet
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
	withSource := func(text, source string) govultr.FirewallRule {
		r := ruleOf(t, text)
		r.Source = source
		return r
	}
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
			// Vultr refuses a second copy of a rule, so only seeding makes one here. The copy with the lowest ID
			// stays, whatever the order of the list.
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
				withSource("v4 tcp 203.0.113.7/32 22", "cloudflare")),
			diff: "    + rule: v4 tcp 203.0.113.7/32 22\n" +
				"    - rule: v4 tcp 203.0.113.7/32 22 source=cloudflare\n",
			deleted: []string{"firewall-1/4"},
		},
		{
			name: "a wanted rule with a load balancer as its source",
			rules: append(rulesOf(t, "v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v6 icmp ::/0"),
				withSource("v4 tcp 203.0.113.7/32 22", "cb676a46-66fd-4dfb-b839-443f2e6c0b60")),
			diff: "    + rule: v4 tcp 203.0.113.7/32 22\n" +
				"    - rule: v4 tcp 203.0.113.7/32 22 source=cb676a46-66fd-4dfb-b839-443f2e6c0b60\n",
			deleted: []string{"firewall-1/4"},
		},
		{
			// Only a source equal to the rule's own subnet is no source.
			name: "a wanted rule with another network as its source",
			rules: append(rulesOf(t, "v4 icmp 0.0.0.0/0", "v4 tcp 0.0.0.0/0 4646", "v6 icmp ::/0"),
				withSource("v4 tcp 203.0.113.7/32 22", "203.0.113.0/24")),
			diff: "    + rule: v4 tcp 203.0.113.7/32 22\n" +
				"    - rule: v4 tcp 203.0.113.7/32 22 source=203.0.113.0/24\n",
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
		// A source that is the rule's own subnet, in any of its forms, is no source.
		govultr.FirewallRule{IPType: "v4", Protocol: "TCP", Subnet: "0.0.0.0", Port: "4646", Source: "0.0.0.0/0"},
		// A range of one port is that port.
		govultr.FirewallRule{IPType: "v4", Protocol: "tcp", Subnet: "203.0.113.7", SubnetSize: 32, Port: "22:22"},
		govultr.FirewallRule{IPType: "v6", Protocol: "icmp", Subnet: "0:0:0:0:0:0:0:0", Source: "::/0"},
	)
	x.wantAdopted(t, g.ID, "CreateFirewallGroup")
	wantNoRuleCalls(t, x.f)
}

// wantNoRuleCalls checks that no CreateFirewallRule or DeleteFirewallRule call reached f.
func wantNoRuleCalls(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	if calls := ruleCalls(f); len(calls) != 0 {
		t.Errorf("rule calls %v, want none", calls)
	}
}

// listedRules are the rules of the example cluster's servers as Vultr lists them after tent created them: each has
// its own subnet as its source, and a type, which govultr does not read.
const listedRules = `[
  {"id": 1, "type": "v4", "ip_type": "v4", "action": "accept", "protocol": "tcp", "port": "22",
   "subnet": "203.0.113.7", "subnet_size": 32, "source": "203.0.113.7/32", "notes": "", "direction": "in",
   "loadbalancer_id": ""},
  {"id": 3, "type": "v4", "ip_type": "v4", "action": "accept", "protocol": "icmp", "port": "",
   "subnet": "0.0.0.0", "subnet_size": 0, "source": "0.0.0.0/0", "notes": "", "direction": "in",
   "loadbalancer_id": ""},
  {"id": 4, "type": "v6", "ip_type": "v6", "action": "accept", "protocol": "icmp", "port": "",
   "subnet": "::", "subnet_size": 0, "source": "::/0", "notes": "", "direction": "in", "loadbalancer_id": ""},
  {"id": 5, "type": "v4", "ip_type": "v4", "action": "accept", "protocol": "tcp", "port": "4646",
   "subnet": "0.0.0.0", "subnet_size": 0, "source": "0.0.0.0/0", "notes": "", "direction": "in",
   "loadbalancer_id": ""}
]`

func TestFirewallTaskAdoptsRulesAsVultrListsThem(t *testing.T) {
	var rules []govultr.FirewallRule
	if err := json.Unmarshal([]byte(listedRules), &rules); err != nil {
		t.Fatalf("decode the listed rules: %v", err)
	}
	x := newFirewallFixture(t, exampleCluster(), exampleGroups())
	seedServersGroup(t, x.f, rules...)
	clients := x.f.AddFirewallGroup(t, govultr.FirewallGroup{Description: firewallDescription("client", "op-earlier")})
	for _, r := range rules {
		if r.Port != "4646" { // the Nomad API is open on the servers only
			x.f.AddFirewallRule(t, clients.ID, r)
		}
	}
	if got := planText(t, x); got != "No changes.\n" {
		t.Errorf("the plan of the rules as Vultr lists them:\n%s\nwant no changes", got)
	}
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	wantNoRuleCalls(t, x.f)
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

// laggingRules is a fake whose first list of a group's rules after a rule create without an answer does not show
// the created rule yet, as a list that lags behind the create.
type laggingRules struct {
	*vultrfake.Fake
	mu    sync.Mutex
	stale map[string][]govultr.FirewallRule // by group id: the rules that the next list returns
}

func (a *laggingRules) CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (
	*govultr.FirewallRule, error) {
	before := a.FirewallRules(groupID)
	r, err := a.Fake.CreateFirewallRule(ctx, groupID, req)
	if errors.Is(err, vultr.ErrUnavailable) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stale[groupID] = before
	}
	return r, err
}

func (a *laggingRules) ListFirewallRules(ctx context.Context, groupID string) ([]govultr.FirewallRule, error) {
	rules, err := a.Fake.ListFirewallRules(ctx, groupID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if stale, ok := a.stale[groupID]; ok && err == nil {
		delete(a.stale, groupID)
		return stale, nil
	}
	return rules, err
}

// TestFirewallTaskLostRuleCreateAndLaggingList checks that a retried create of a rule that exists counts as done:
// Vultr refuses it as already defined.
func TestFirewallTaskLostRuleCreateAndLaggingList(t *testing.T) {
	x := newFixture()
	x.p = opProvider(&laggingRules{Fake: x.f, stale: map[string][]govultr.FirewallRule{}})
	x.tasks = firewallTasks(t, x, exampleCluster(), combinedGroups())
	x.kinds = x.p.InfraKinds()
	events := x.applyWithFaults(t, func(tb testing.TB) { x.f.LoseResponse(tb, "CreateFirewallRule", 1) })
	if n := countEvents(events, engine.Retrying); n != 1 {
		t.Errorf("the engine retried %d times, want once", n)
	}
	const icmp = "CreateFirewallRule firewall-1 v4 icmp 0.0.0.0/0"
	if calls := ruleCalls(x.f); len(calls) < 2 || !slices.Equal(calls[:2], []string{icmp, icmp}) {
		t.Errorf("the rule calls are %v, want the ICMP rule's create twice first", calls)
	}
	wantFirewallGroups(t, x.f, firewallGroup{Description: firewallDescription("server", "op-2"), Rules: serverRules})
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

func TestFirewallTaskRuleAlreadyDefined(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string // Vultr's answer to the first rule create, with the status 400
		creates int    // the CreateFirewallRule calls
		want    string // the apply's error; empty for success
	}{
		// The rule exists, so the task goes on with the next one.
		{name: "Vultr's answer as it came", message: "This rule is already defined ", creates: 4},
		{name: "in another case", message: "this rule is ALREADY defined", creates: 4},
		{
			name: "another answer", message: "Invalid port.", creates: 1,
			want: "vultr.FirewallGroup/prod-servers: add rule v4 icmp 0.0.0.0/0: " +
				"vultr: POST /v2/firewalls/firewall-1/rules: 400 Bad Request: Invalid port.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFirewallFixture(t, exampleCluster(), combinedGroups())
			events, err := x.applyInBubble(t, func(tb testing.TB) {
				answer := vultr.NewAPIError(http.MethodPost, "/v2/firewalls/firewall-1/rules", http.StatusBadRequest,
					tc.message, 0)
				x.f.Fail(tb, "CreateFirewallRule", answer, 1)
			})
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("apply: %v, want success", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("apply error = %v, want %q", err, tc.want)
			}
			if n := countEvents(events, engine.Retrying); n != 0 {
				t.Errorf("the engine retried %d times, want never", n)
			}
			if got := countCalls(x.f, "CreateFirewallRule"); got != tc.creates {
				t.Errorf("%d CreateFirewallRule calls, want %d", got, tc.creates)
			}
		})
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

func TestFirewallGroupDeleteInUse(t *testing.T) {
	x := newFixture()
	x.kinds = x.p.InfraKinds()
	g := seedServersGroup(t, x.f, rulesOf(t, serverRules...)...)
	// Vultr deletes a group that instances use, so this answer is not one it is known to give; any answer that
	// matches vultr.ErrInUse is retried.
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
	env := &engine.Env{Snapshot: inventory(t, p)}
	before := len(f.Calls())
	obj := engine.Object{Key: serversKey, ID: "firewall-9"}
	if err := deleterOf(t, p, "vultr.FirewallGroup").Delete(t.Context(), env, obj); err != nil {
		t.Errorf("Delete of a firewall group that is gone: %v, want success", err)
	}
	wantCallsSince(t, f, before, vultrfake.Call{Name: "ListInstances", Arg: "tent/cluster=prod"},
		vultrfake.Call{Name: "DeleteFirewallGroup", Arg: "firewall-9"})
}

// seedNode stores a node of cluster prod with the label name in the firewall group groupID, without a call, and
// returns it.
func seedNode(t *testing.T, f *vultrfake.Fake, name, groupID string) govultr.Instance {
	t.Helper()
	return f.AddInstance(t, govultr.Instance{
		Label: name, FirewallGroupID: groupID,
		Tags: []string{"tent/cluster=prod", "tent/nodegroup=servers", "tent/role=server"},
	})
}

// wantPlannedChanges plans the fixture's tasks against a fresh inventory and checks the plan's changes.
func wantPlannedChanges(t *testing.T, x *fixture, want ...engine.PlannedChange) {
	t.Helper()
	p, err := x.plan(t)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if diff := cmp.Diff(want, p.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
}

// TestFirewallGroupDeleteReportsADuplicateThatNodesUse checks that tent never deletes a firewall group that a node
// uses: Vultr would delete it and leave the node without a firewall, and its image allows root login with a password.
// A duplicate that nodes use is reported with a warning, the apply goes on, and every run reports it again until
// those nodes are replaced.
func TestFirewallGroupDeleteReportsADuplicateThatNodesUse(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	// Two copies of the servers' group, each with a node: the older one is kept, and the newer one is a duplicate.
	dup := x.f.AddFirewallGroup(t, govultr.FirewallGroup{
		Description: firewallDescription("server", "op-b"), DateCreated: sept25,
	})
	kept := x.f.AddFirewallGroup(t, govultr.FirewallGroup{
		Description: firewallDescription("server", "op-a"), DateCreated: sept20,
	})
	for _, r := range rulesOf(t, serverRules...) {
		x.f.AddFirewallRule(t, kept.ID, r)
	}
	node := seedNode(t, x.f, "prod-servers-0", dup.ID)
	other := seedNode(t, x.f, "prod-servers-1", kept.ID)
	deleteDup := engine.PlannedChange{
		Key: serversKey, ID: dup.ID, Change: engine.Change{Action: engine.Delete, Reason: "duplicate"},
	}
	wantPlannedChanges(t, x, deleteDup)

	events, err := x.applyInBubble(t, func(testing.TB) {})

	if err != nil {
		t.Errorf("apply: %v, want success", err)
	}
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
	if n := countCalls(x.f, "DeleteFirewallGroup"); n != 0 {
		t.Errorf("%d DeleteFirewallGroup calls, want none", n)
	}
	wantFirewallGroups(t, x.f, firewallGroup{Description: dup.Description},
		firewallGroup{Description: kept.Description, Rules: serverRules})
	if got := instanceOf(t, x.f, node.ID).FirewallGroupID; got != dup.ID {
		t.Errorf("the node's firewall group is %q, want %q", got, dup.ID)
	}
	want := []map[string]string{{
		"level": "WARN", "msg": "keeping a duplicate Vultr firewall group that nodes use; replace those nodes, " +
			"and a later run deletes the group",
		"cluster": "prod", "group": "prod-servers", "id": dup.ID, "nodes": "prod-servers-0 (instance-1)",
	}}
	if diff := cmp.Diff(want, logRecords(t, x.log)); diff != "" {
		t.Errorf("log (-want +got):\n%s", diff)
	}
	// The next run plans the delete again.
	wantPlannedChanges(t, x, deleteDup)

	// Once the node is gone, the same prune deletes the duplicate, and the other node keeps its group.
	if err := x.p.Delete(t.Context(), cloud.Instance{ID: node.ID, Name: node.Label}); err != nil {
		t.Fatalf("Delete %s: %v", node.ID, err)
	}
	x.applyWithFaults(t, func(testing.TB) {})
	wantFirewallGroups(t, x.f, firewallGroup{Description: kept.Description, Rules: serverRules})
	if got := instanceOf(t, x.f, other.ID).FirewallGroupID; got != kept.ID {
		t.Errorf("the other node's firewall group is %q, want %q", got, kept.ID)
	}
}

// TestFirewallGroupDeleteRefusesAnUnwantedGroupThatNodesUse checks that the delete of a firewall group that the
// cluster no longer wants fails while nodes use it, such as the clients' group while the last clients still run.
// The engine does not retry the refusal: the nodes must go first.
func TestFirewallGroupDeleteRefusesAnUnwantedGroupThatNodesUse(t *testing.T) {
	x := newFirewallFixture(t, exampleCluster(), combinedGroups())
	servers := seedServersGroup(t, x.f, rulesOf(t, serverRules...)...)
	clients := x.f.AddFirewallGroup(t, govultr.FirewallGroup{Description: firewallDescription("client", "op-a")})
	node := seedNode(t, x.f, "prod-workers-0", clients.ID)
	wantPlannedChanges(t, x, engine.PlannedChange{
		Key: clientsKey, ID: clients.ID, Change: engine.Change{Action: engine.Delete},
	})

	events, err := x.applyInBubble(t, func(testing.TB) {})

	const wantErr = "vultr.FirewallGroup/prod-clients (ID firewall-2): firewall group prod-clients (ID firewall-2) " +
		"still protects nodes prod-workers-0 (instance-1), and tent does not delete a firewall group that nodes " +
		"use; delete those nodes first"
	if errText(err) != wantErr {
		t.Errorf("apply error = %v, want %q", err, wantErr)
	}
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
	if n := countCalls(x.f, "DeleteFirewallGroup"); n != 0 {
		t.Errorf("%d DeleteFirewallGroup calls, want none", n)
	}
	wantFirewallGroups(t, x.f, firewallGroup{Description: servers.Description, Rules: serverRules},
		firewallGroup{Description: clients.Description})
	if got := instanceOf(t, x.f, node.ID).FirewallGroupID; got != clients.ID {
		t.Errorf("the node's firewall group is %q, want %q", got, clients.ID)
	}
	if got := logRecords(t, x.log); len(got) != 0 {
		t.Errorf("log = %v, want none", got)
	}
}

func TestFirewallGroupDeleteNamesEveryNodeInTheGroup(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	g := seedServersGroup(t, f)
	clients := f.AddFirewallGroup(t, govultr.FirewallGroup{Description: firewallDescription("client", "op-earlier")})
	seedNode(t, f, "prod-servers-1", g.ID)
	seedNode(t, f, "prod-workers-0", clients.ID)
	seedNode(t, f, "prod-servers-0", g.ID)
	// A node whose label was changed in the console: its name is its hostname.
	f.AddInstance(t, govultr.Instance{Hostname: "prod-servers-2", Label: "db primary", FirewallGroupID: g.ID,
		Tags: []string{"tent/cluster=prod", "tent/nodegroup=servers", "tent/role=server"}})
	// An instance of another cluster in the group: tent looks for the cluster's nodes only.
	f.AddInstance(t, govultr.Instance{Label: "staging-servers-0", FirewallGroupID: g.ID,
		Tags: []string{"tent/cluster=staging"}})
	env := &engine.Env{Snapshot: inventory(t, p)}

	err := deleterOf(t, p, "vultr.FirewallGroup").Delete(t.Context(), env, engine.Object{Key: serversKey, ID: g.ID})

	// By name.
	const want = "firewall group prod-servers (ID firewall-1) still protects nodes prod-servers-0 (instance-3), " +
		"prod-servers-1 (instance-1), prod-servers-2 (instance-4), and tent does not delete a firewall group that " +
		"nodes use; delete those nodes first"
	if errText(err) != want {
		t.Errorf("Delete = %v, want %q", err, want)
	}
}

// TestFirewallGroupDeleteSeesNodesThatListSkips checks that a node whose tent tags do not decode, which Nodes.List
// skips, still keeps its firewall group: delete cluster stops at the group and names the node.
func TestFirewallGroupDeleteSeesNodesThatListSkips(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	g := seedServersGroup(t, f)
	f.AddInstance(t, govultr.Instance{ID: "upper", Hostname: "prod-servers-0", FirewallGroupID: g.ID,
		Tags: []string{"tent/cluster=PROD", "tent/nodegroup=servers", "tent/role=server"}})
	if nodes, err := p.List(t.Context(), "prod"); err != nil || len(nodes) != 0 {
		t.Fatalf("List = %+v, %v; want no node", nodes, err)
	}
	env := &engine.Env{Snapshot: inventory(t, p)}

	err := deleterOf(t, p, "vultr.FirewallGroup").Delete(t.Context(), env, engine.Object{Key: serversKey, ID: g.ID})

	const want = "firewall group prod-servers (ID firewall-1) still protects nodes prod-servers-0 (upper), and tent " +
		"does not delete a firewall group that nodes use; delete those nodes first"
	if errText(err) != want {
		t.Errorf("Delete = %v, want %q", err, want)
	}
	if n := countCalls(f, "DeleteFirewallGroup"); n != 0 {
		t.Errorf("%d DeleteFirewallGroup calls, want none", n)
	}
}

// TestFirewallGroupDeleteNodeListFails checks which failures of the node list the engine retries: a rate limit, a 5xx
// and no answer, but not a refused API key.
func TestFirewallGroupDeleteNodeListFails(t *testing.T) {
	fail := func(err error) func(tb testing.TB, f *vultrfake.Fake) {
		return func(tb testing.TB, f *vultrfake.Fake) { f.Fail(tb, "ListInstances", err, 1) }
	}
	answer := func(status int, msg string) error {
		return vultr.NewAPIError(http.MethodGet, "/v2/instances", status, msg, 0)
	}
	for _, tc := range []struct {
		name  string
		fault func(tb testing.TB, f *vultrfake.Fake)
		want  string // the apply's error; "" when the engine retries and the delete succeeds
	}{
		{"429", func(tb testing.TB, f *vultrfake.Fake) { f.Throttle(tb, "ListInstances", time.Second, 1) }, ""},
		{"503", fail(answer(http.StatusServiceUnavailable, "Try again later")), ""},
		{"no answer", fail(vultr.NewNoAnswerError(http.MethodGet, "/v2/instances", nil)), ""},
		{"401", fail(answer(http.StatusUnauthorized, "Invalid API token.")),
			"401 Unauthorized: Invalid API token."},
		{"403", fail(answer(http.StatusForbidden, "Unauthorized IP address")),
			"403 Forbidden: Unauthorized IP address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newFixture()
			x.kinds = x.p.InfraKinds()
			seedServersGroup(t, x.f, rulesOf(t, serverRules...)...)
			if tc.want == "" {
				events := x.applyWithFaults(t, func(tb testing.TB) { tc.fault(tb, x.f) })
				if n := countEvents(events, engine.Retrying); n != 1 {
					t.Errorf("the engine retried %d times, want once", n)
				}
				wantFirewallGroups(t, x.f)
				return
			}
			events, err := x.applyInBubble(t, func(tb testing.TB) { tc.fault(tb, x.f) })
			want := "vultr.FirewallGroup/prod-servers (ID firewall-1): firewall group prod-servers (ID firewall-1): " +
				"list the nodes of cluster prod: vultr: GET /v2/instances: " + tc.want
			if errText(err) != want {
				t.Errorf("apply error = %v, want %q", err, want)
			}
			if n := countEvents(events, engine.Retrying); n != 0 {
				t.Errorf("the engine retried %d times, want never", n)
			}
			wantFirewallGroups(t, x.f, firewallGroup{Description: firewallDescription("server", "op-earlier"),
				Rules: serverRules})
		})
	}
}

func TestFirewallGroupDeleteRetriesAFailedNodeList(t *testing.T) {
	x := newFixture()
	x.kinds = x.p.InfraKinds()
	seedServersGroup(t, x.f, rulesOf(t, serverRules...)...)
	events := x.applyWithFaults(t, func(tb testing.TB) {
		x.f.Fail(tb, "ListInstances", vultr.NewAPIError(http.MethodGet, "/v2/instances",
			http.StatusInternalServerError, "Internal error", 0), 1)
	})
	var retries []string
	for _, e := range events {
		if e.Type == engine.Retrying {
			retries = append(retries, errText(e.Err))
		}
	}
	want := []string{"firewall group prod-servers (ID firewall-1): list the nodes of cluster prod: " +
		"vultr: GET /v2/instances: 500 Internal Server Error: Internal error"}
	if diff := cmp.Diff(want, retries); diff != "" {
		t.Errorf("the errors the engine retried after (-want +got):\n%s", diff)
	}
	wantFirewallGroups(t, x.f)
}

func TestFirewallGroupDeleteWithoutInventory(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	obj := engine.Object{Key: serversKey, ID: "firewall-9"}

	err := deleterOf(t, p, "vultr.FirewallGroup").Delete(t.Context(), &engine.Env{}, obj)

	const want = "firewall group prod-servers (ID firewall-9): the snapshot names no cluster, so tent cannot tell " +
		"which nodes use the group"
	if errText(err) != want {
		t.Errorf("Delete = %v, want %q", err, want)
	}
	wantCalls(t, f)
}
