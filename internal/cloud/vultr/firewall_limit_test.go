package vultr

import (
	"net/netip"
	"testing"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// TestFirewallTaskPlanUnknownLimit checks that a group that reports no max_rule_count gets the limit of a new group
// instead of failing every plan.
func TestFirewallTaskPlanUnknownLimit(t *testing.T) {
	access := []model.AccessRule{{
		Name: "ssh", To: model.AllNodes, Protocol: model.ProtocolTCP, Port: 22,
		From: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
	}}
	task := &firewallTask{cluster: "prod", role: roleServer, rules: wantedRules(access, roleServer), op: "op-1"}
	key := firewallGroupKey("prod", roleServer)
	snap := &snapshot{
		groups: map[engine.Key]govultr.FirewallGroup{key: {ID: "firewall-1", MaxRuleCount: 0}},
		rules:  map[string][]govultr.FirewallRule{},
	}
	ch, err := task.Plan(t.Context(), &engine.Env{Snapshot: snap, Outputs: &engine.Outputs{}})
	if err != nil {
		t.Fatalf("Plan: %v, want success", err)
	}
	if ch.Action != engine.Update {
		t.Errorf("Plan action = %v, want update", ch.Action)
	}
}

// TestReaches checks which firewall groups an access rule to each target reaches: the servers' group holds server and
// combined nodes, the clients' group client nodes.
func TestReaches(t *testing.T) {
	for _, tc := range []struct {
		target         model.Target
		server, client bool
	}{
		{model.AllNodes, true, true},
		{model.Servers, true, false},
		{model.Clients, true, true}, // the combined nodes of the servers' group run clients
		{model.Target(0), false, false},
	} {
		for role, want := range map[string]bool{roleServer: tc.server, roleClient: tc.client} {
			if got := reaches(tc.target, role); got != want {
				t.Errorf("reaches(%v, %s) = %t, want %t", tc.target, role, got, want)
			}
		}
	}
}
