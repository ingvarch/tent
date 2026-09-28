package vultrfake_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// sshRule and pingRule are rules as tent sends them.
var (
	sshRule  = govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", Port: "22", Notes: "ssh"}
	pingRule = govultr.FirewallRuleReq{IPType: "v6", Protocol: "icmp", Subnet: "::"}
)

// mustCreateGroup creates a firewall group and fails the test on an error.
func mustCreateGroup(t *testing.T, f *vultrfake.Fake, description string) *govultr.FirewallGroup {
	t.Helper()
	g, err := f.CreateFirewallGroup(t.Context(), &govultr.FirewallGroupReq{Description: description})
	if err != nil {
		t.Fatalf("CreateFirewallGroup %q: %v", description, err)
	}
	return g
}

// mustCreateRule adds a rule to a firewall group and fails the test on an error.
func mustCreateRule(t *testing.T, f *vultrfake.Fake, groupID string, req govultr.FirewallRuleReq) *govultr.FirewallRule {
	t.Helper()
	r, err := f.CreateFirewallRule(t.Context(), groupID, &req)
	if err != nil {
		t.Fatalf("CreateFirewallRule in %s: %v", groupID, err)
	}
	return r
}

func TestFirewallGroups(t *testing.T) {
	const marker = "tent:cluster=prod;kind=firewall;role=server"
	f := newFake()
	ctx := t.Context()
	want := govultr.FirewallGroup{
		ID: "firewall-1", Description: marker, DateCreated: date, DateModified: date, MaxRuleCount: 50,
	}
	if diff := cmp.Diff(&want, mustCreateGroup(t, f, marker)); diff != "" {
		t.Errorf("CreateFirewallGroup (-want +got):\n%s", diff)
	}
	g, err := f.CreateFirewallGroup(ctx, nil) // a group needs no description
	if err != nil || g.ID != "firewall-2" || g.Description != "" {
		t.Errorf("CreateFirewallGroup(nil) = %+v, %v; want firewall-2 without a description", g, err)
	}

	ssh := mustCreateRule(t, f, "firewall-1", sshRule)
	wantSSH := govultr.FirewallRule{
		ID: 1, Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "0.0.0.0", Source: "0.0.0.0/0",
		Notes: "ssh",
	}
	if diff := cmp.Diff(&wantSSH, ssh); diff != "" {
		t.Errorf("CreateFirewallRule (-want +got):\n%s", diff)
	}
	ping := mustCreateRule(t, f, "firewall-1", pingRule)
	wantPing := govultr.FirewallRule{
		ID: 2, Action: "accept", IPType: "v6", Protocol: "icmp", Subnet: "::", Source: "::/0",
	}
	if diff := cmp.Diff(&wantPing, ping); diff != "" {
		t.Errorf("CreateFirewallRule (-want +got):\n%s", diff)
	}
	// Rule ids count from 1 in each group.
	if r := mustCreateRule(t, f, "firewall-2", sshRule); r.ID != 1 {
		t.Errorf("the first rule of firewall-2 has the id %d, want 1", r.ID)
	}

	want.RuleCount = 2
	wantGroups(t, f, want, govultr.FirewallGroup{
		ID: "firewall-2", DateCreated: date, DateModified: date, RuleCount: 1, MaxRuleCount: 50,
	})
	wantRules(t, f, "firewall-1", wantSSH, wantPing)

	if err := f.DeleteFirewallRule(ctx, "firewall-1", 1); err != nil {
		t.Fatalf("DeleteFirewallRule: %v", err)
	}
	wantRules(t, f, "firewall-1", wantPing)
	wantAPIError(t, f.DeleteFirewallRule(ctx, "firewall-1", 1), vultr.ErrNotFound,
		"vultr: DELETE /v2/firewalls/firewall-1/rules/1: 404 Not Found: Invalid firewall rule ID.")
	// A rule id is never given out again in its group.
	if r := mustCreateRule(t, f, "firewall-1", sshRule); r.ID != 3 {
		t.Errorf("the next rule of firewall-1 has the id %d, want 3", r.ID)
	}

	if err := f.DeleteFirewallGroup(ctx, "firewall-1"); err != nil {
		t.Fatalf("DeleteFirewallGroup: %v", err)
	}
	gs, err := f.ListFirewallGroups(ctx)
	if err != nil || len(gs) != 1 || gs[0].ID != "firewall-2" {
		t.Errorf("ListFirewallGroups = %+v, %v; want firewall-2 only", gs, err)
	}
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateFirewallGroup", Arg: marker},
		vultrfake.Call{Name: "CreateFirewallGroup"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v4 tcp 0.0.0.0/0 22"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v6 icmp ::/0"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-2 v4 tcp 0.0.0.0/0 22"},
		vultrfake.Call{Name: "ListFirewallGroups"},
		vultrfake.Call{Name: "ListFirewallRules", Arg: "firewall-1"},
		vultrfake.Call{Name: "DeleteFirewallRule", Arg: "firewall-1/1"},
		vultrfake.Call{Name: "ListFirewallRules", Arg: "firewall-1"},
		vultrfake.Call{Name: "DeleteFirewallRule", Arg: "firewall-1/1"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v4 tcp 0.0.0.0/0 22"},
		vultrfake.Call{Name: "DeleteFirewallGroup", Arg: "firewall-1"},
		vultrfake.Call{Name: "ListFirewallGroups"},
	)
}

func TestCreateFirewallRuleCall(t *testing.T) {
	f := newFake()
	mustCreateGroup(t, f, "g")
	for _, req := range []govultr.FirewallRuleReq{
		sshRule,
		pingRule,
		{IPType: "v4", Protocol: "udp", Subnet: "10.64.0.0", SubnetSize: 16, Port: "4646:4648"},
		{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", Port: "443", Source: "cloudflare"},
	} {
		mustCreateRule(t, f, "firewall-1", req)
	}
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateFirewallGroup", Arg: "g"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v4 tcp 0.0.0.0/0 22"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v6 icmp ::/0"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v4 udp 10.64.0.0/16 4646:4648"},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: "firewall-1 v4 tcp 0.0.0.0/0 443 source=cloudflare"},
	)
}

func TestFirewallRuleSource(t *testing.T) {
	f := newFake()
	mustCreateGroup(t, f, "g")
	for _, tc := range []struct {
		name string
		req  govultr.FirewallRuleReq
		want string // the source that the create answer and the list give
	}{
		// Vultr lists a rule without a source with its own subnet as the source.
		{
			name: "no source",
			req:  govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "203.0.113.7", SubnetSize: 32, Port: "22"},
			want: "203.0.113.7/32",
		},
		{name: "no source, IPv6", req: pingRule, want: "::/0"},
		{
			name: "a source",
			req:  govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", Port: "443", Source: "cloudflare"},
			want: "cloudflare",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := mustCreateRule(t, f, "firewall-1", tc.req)
			if created.Source != tc.want {
				t.Errorf("CreateFirewallRule gives the source %q, want %q", created.Source, tc.want)
			}
			rules := f.FirewallRules("firewall-1")
			if i := slices.IndexFunc(rules, func(r govultr.FirewallRule) bool { return r.ID == created.ID }); i < 0 ||
				rules[i].Source != tc.want {
				t.Errorf("the rules of firewall-1 are %+v, want rule %d with the source %q", rules, created.ID, tc.want)
			}
		})
	}
}

func TestFirewallRuleAlreadyDefined(t *testing.T) {
	f := newFake()
	mustCreateGroup(t, f, "g")
	mustCreateRule(t, f, "firewall-1", sshRule)
	const defined = "vultr: POST /v2/firewalls/firewall-1/rules: 400 Bad Request: This rule is already defined"
	notes := sshRule
	notes.Notes = "" // notes do not tell rules apart
	for _, req := range []govultr.FirewallRuleReq{sshRule, notes} {
		rule, err := f.CreateFirewallRule(t.Context(), "firewall-1", &req)
		wantAPIError(t, err, vultr.ErrInvalid, defined)
		if rule != nil {
			t.Errorf("CreateFirewallRule returned %+v with the error", rule)
		}
	}
	// A rule that differs in ip_type, protocol, subnet, subnet_size, port or source is another rule.
	for _, edit := range []func(r *govultr.FirewallRuleReq){
		func(r *govultr.FirewallRuleReq) { r.IPType = "v6" },
		func(r *govultr.FirewallRuleReq) { r.Protocol = "udp" },
		func(r *govultr.FirewallRuleReq) { r.Subnet = "198.51.100.0" },
		func(r *govultr.FirewallRuleReq) { r.SubnetSize = 8 },
		func(r *govultr.FirewallRuleReq) { r.Port = "23" },
		func(r *govultr.FirewallRuleReq) { r.Source = "cloudflare" },
	} {
		req := sshRule
		edit(&req)
		mustCreateRule(t, f, "firewall-1", req)
	}
	if got := len(f.FirewallRules("firewall-1")); got != 7 {
		t.Errorf("firewall-1 holds %d rules, want 7", got)
	}
}

// TestFirewallRuleAlreadyDefinedWithoutSource checks that a rule without a source is the same rule as one whose source
// is its own subnet, as Vultr lists it: a seeded rule keeps an empty source.
func TestFirewallRuleAlreadyDefinedWithoutSource(t *testing.T) {
	f := newFake()
	g := f.AddFirewallGroup(t, govultr.FirewallGroup{})
	f.AddFirewallRule(t, g.ID, govultr.FirewallRule{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", Port: "22"})
	f.AddFirewallRule(t, g.ID, govultr.FirewallRule{
		IPType: "v6", Protocol: "icmp", Subnet: "::", Source: "::/0",
	})
	withSource := sshRule
	withSource.Source = "0.0.0.0/0"
	for _, req := range []govultr.FirewallRuleReq{sshRule, withSource, pingRule} {
		rule, err := f.CreateFirewallRule(t.Context(), g.ID, &req)
		wantAPIError(t, err, vultr.ErrInvalid,
			"vultr: POST /v2/firewalls/firewall-1/rules: 400 Bad Request: This rule is already defined")
		if rule != nil {
			t.Errorf("CreateFirewallRule returned %+v with the error", rule)
		}
	}
	if got := len(f.FirewallRules(g.ID)); got != 2 {
		t.Errorf("firewall-1 holds %d rules, want the 2 seeded", got)
	}
}

func TestMissingFirewallGroup(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	mustCreateRule(t, f, mustCreateGroup(t, f, "g").ID, sshRule)
	if err := f.DeleteFirewallGroup(ctx, "firewall-1"); err != nil { // its rules go with it
		t.Fatalf("DeleteFirewallGroup: %v", err)
	}
	const text = ": 404 Not Found: Invalid firewall group ID."
	rules, err := f.ListFirewallRules(ctx, "firewall-1")
	wantAPIError(t, err, vultr.ErrNotFound, "vultr: GET /v2/firewalls/firewall-1/rules"+text)
	if rules != nil {
		t.Errorf("ListFirewallRules returned %v with the error", rules)
	}
	rule, err := f.CreateFirewallRule(ctx, "firewall-1", &sshRule)
	wantAPIError(t, err, vultr.ErrNotFound, "vultr: POST /v2/firewalls/firewall-1/rules"+text)
	if rule != nil {
		t.Errorf("CreateFirewallRule returned %v with the error", rule)
	}
	wantAPIError(t, f.DeleteFirewallRule(ctx, "firewall-1", 1), vultr.ErrNotFound,
		"vultr: DELETE /v2/firewalls/firewall-1/rules/1"+text)
	wantAPIError(t, f.DeleteFirewallGroup(ctx, "firewall-1"), vultr.ErrNotFound,
		"vultr: DELETE /v2/firewalls/firewall-1"+text)
}

func TestCreateFirewallRuleInvalid(t *testing.T) {
	const prefix = "vultr: POST /v2/firewalls/firewall-1/rules: 400 Bad Request: "
	for _, tc := range []struct {
		name string
		req  *govultr.FirewallRuleReq
		text string
	}{
		{"no request", nil, prefix + "Invalid ip_type."},
		{"no ip_type", &govultr.FirewallRuleReq{Protocol: "tcp", Port: "22"}, prefix + "Invalid ip_type."},
		{"no protocol", &govultr.FirewallRuleReq{IPType: "v4", Port: "22"}, prefix + "Invalid protocol."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			mustCreateGroup(t, f, "g")
			rule, err := f.CreateFirewallRule(t.Context(), "firewall-1", tc.req)
			wantAPIError(t, err, vultr.ErrInvalid, tc.text)
			if rule != nil {
				t.Errorf("CreateFirewallRule returned %+v with the error", rule)
			}
			wantRules(t, f, "firewall-1")
		})
	}
}

// portRule returns sshRule with the port n, so that each n gives another rule.
func portRule(n int) govultr.FirewallRuleReq {
	r := sshRule
	r.Port = strconv.Itoa(n)
	return r
}

func TestFirewallRuleLimit(t *testing.T) {
	f := newFake()
	mustCreateGroup(t, f, "g")
	for n := range 50 {
		mustCreateRule(t, f, "firewall-1", portRule(n+1))
	}
	last := portRule(51)
	rule, err := f.CreateFirewallRule(t.Context(), "firewall-1", &last)
	wantAPIError(t, err, vultr.ErrLimitReached, "vultr: POST /v2/firewalls/firewall-1/rules: 400 Bad Request: "+
		"You have reached the maximum number of rules for this firewall group.")
	if rule != nil {
		t.Errorf("the 51st rule is %+v, want none", rule)
	}
	gs, err := f.ListFirewallGroups(t.Context())
	if err != nil || len(gs) != 1 || gs[0].RuleCount != 50 {
		t.Errorf("ListFirewallGroups = %+v, %v; want one group with 50 rules", gs, err)
	}
	if err := f.DeleteFirewallRule(t.Context(), "firewall-1", 7); err != nil {
		t.Fatalf("DeleteFirewallRule: %v", err)
	}
	if r := mustCreateRule(t, f, "firewall-1", last); r.ID != 51 {
		t.Errorf("the rule after a delete has the id %d, want 51", r.ID)
	}
}

// wantGroups checks the fake's firewall groups.
func wantGroups(t *testing.T, f *vultrfake.Fake, want ...govultr.FirewallGroup) {
	t.Helper()
	got, err := f.ListFirewallGroups(t.Context())
	if err != nil {
		t.Fatalf("ListFirewallGroups: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("firewall groups (-want +got):\n%s", diff)
	}
}

// wantRules checks the rules of a firewall group.
func wantRules(t *testing.T, f *vultrfake.Fake, groupID string, want ...govultr.FirewallRule) {
	t.Helper()
	got, err := f.ListFirewallRules(t.Context(), groupID)
	if err != nil {
		t.Fatalf("ListFirewallRules %s: %v", groupID, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rules of %s (-want +got):\n%s", groupID, diff)
	}
}
