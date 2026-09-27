package vultrfake_test

import (
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
		ID: 1, Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "0.0.0.0", Notes: "ssh",
	}
	if diff := cmp.Diff(&wantSSH, ssh); diff != "" {
		t.Errorf("CreateFirewallRule (-want +got):\n%s", diff)
	}
	ping := mustCreateRule(t, f, "firewall-1", pingRule)
	wantPing := govultr.FirewallRule{ID: 2, Action: "accept", IPType: "v6", Protocol: "icmp", Subnet: "::"}
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

func TestFirewallRuleLimit(t *testing.T) {
	f := newFake()
	mustCreateGroup(t, f, "g")
	for range 50 {
		mustCreateRule(t, f, "firewall-1", sshRule)
	}
	rule, err := f.CreateFirewallRule(t.Context(), "firewall-1", &sshRule)
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
	if r := mustCreateRule(t, f, "firewall-1", sshRule); r.ID != 51 {
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
