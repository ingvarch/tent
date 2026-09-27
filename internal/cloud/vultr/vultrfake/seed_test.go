package vultrfake_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// fatalTB is a testing.TB that records Fatalf instead of ending the test.
type fatalTB struct {
	testing.TB
	msgs []string
}

func (tb *fatalTB) Helper() {}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.msgs = append(tb.msgs, fmt.Sprintf(format, args...))
}

// wantFatal checks that tb recorded exactly one Fatalf, with the message want.
func wantFatal(t *testing.T, tb *fatalTB, want string) {
	t.Helper()
	if diff := cmp.Diff([]string{want}, tb.msgs); diff != "" {
		t.Errorf("Fatalf messages (-want +got):\n%s", diff)
	}
}

const uuid = "cb676a46-66fd-4dfb-b839-443f2e6c0b60"

func TestAddSSHKey(t *testing.T) {
	f := newFake()
	const old = "2026-01-02T03:04:05+00:00"
	k1 := f.AddSSHKey(t, govultr.SSHKey{Name: "tent:cluster=prod;kind=ssh-key", SSHKey: "ssh-ed25519 AAAA"})
	k2 := f.AddSSHKey(t, govultr.SSHKey{ID: uuid, Name: "tent:cluster=prod;kind=ssh-key", DateCreated: old})
	want := []govultr.SSHKey{
		{ID: "ssh-key-1", Name: "tent:cluster=prod;kind=ssh-key", SSHKey: "ssh-ed25519 AAAA", DateCreated: date},
		{ID: uuid, Name: "tent:cluster=prod;kind=ssh-key", DateCreated: old},
	}
	if diff := cmp.Diff(want, []govultr.SSHKey{k1, k2}); diff != "" {
		t.Errorf("AddSSHKey (-want +got):\n%s", diff)
	}
	wantCalls(t, f) // seeding is no call
	wantSSHKeys(t, f, want...)
	if k := mustCreateSSHKey(t, f, "c"); k.ID != "ssh-key-2" {
		t.Errorf("the created key's id is %q, want ssh-key-2", k.ID)
	}
}

func TestAddVPC(t *testing.T) {
	f := newFake()
	// Two VPCs with one description, as a lost create answer may leave behind.
	const marker = "tent:cluster=prod;kind=vpc"
	v2 := f.AddVPC(t, govultr.VPC{ID: "vpc-2", Region: "ams", Description: marker, V4Subnet: "10.64.0.0",
		V4SubnetMask: 16})
	v1 := f.AddVPC(t, govultr.VPC{Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16})
	if v2.ID != "vpc-2" || v1.ID != "vpc-1" || v1.DateCreated != date || v2.DateCreated != date {
		t.Errorf("AddVPC = %+v and %+v, want vpc-2 and vpc-1 created at %s", v2, v1, date)
	}
	wantVPCs(t, f, v2, v1)
	// Created ids skip the seeded ones.
	v, err := f.CreateVPC(t.Context(), &govultr.VPCReq{Region: "fra"})
	if err != nil || v.ID != "vpc-3" {
		t.Errorf("CreateVPC = %+v, %v; want vpc-3", v, err)
	}
}

func TestAddFirewallGroupAndRules(t *testing.T) {
	f := newFake()
	g := f.AddFirewallGroup(t, govultr.FirewallGroup{Description: "g", RuleCount: 9})
	want := govultr.FirewallGroup{ID: "firewall-1", Description: "g", DateCreated: date, DateModified: date,
		MaxRuleCount: 50}
	if diff := cmp.Diff(want, g); diff != "" {
		t.Errorf("AddFirewallGroup (-want +got):\n%s", diff)
	}
	small := f.AddFirewallGroup(t, govultr.FirewallGroup{ID: uuid, DateCreated: "2026-01-02T03:04:05+00:00",
		MaxRuleCount: 2, InstanceCount: 3})
	if small.DateModified != small.DateCreated || small.MaxRuleCount != 2 || small.InstanceCount != 3 {
		t.Errorf("AddFirewallGroup = %+v, want the given fields and date_modified = date_created", small)
	}

	r5 := f.AddFirewallRule(t, uuid, govultr.FirewallRule{ID: 5, IPType: "v4", Protocol: "tcp", Port: "22"})
	r6 := f.AddFirewallRule(t, uuid, govultr.FirewallRule{IPType: "v4", Protocol: "icmp", Action: "accept"})
	wantRules(t, f, uuid,
		govultr.FirewallRule{ID: 5, Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22"},
		govultr.FirewallRule{ID: 6, Action: "accept", IPType: "v4", Protocol: "icmp"},
	)
	if r5.ID != 5 || r6.ID != 6 || r5.Action != "accept" {
		t.Errorf("AddFirewallRule = %+v and %+v, want the rules 5 and 6", r5, r6)
	}
	// The group's own limit holds for creates.
	rule, err := f.CreateFirewallRule(t.Context(), uuid, &sshRule)
	wantAPIError(t, err, vultr.ErrLimitReached, "vultr: POST /v2/firewalls/"+uuid+"/rules: 400 Bad Request: "+
		"You have reached the maximum number of rules for this firewall group.")
	if rule != nil {
		t.Errorf("CreateFirewallRule returned %+v with the error", rule)
	}
	want.RuleCount = 0
	small.RuleCount = 2
	wantGroups(t, f, want, small)
	wantCalls(t, f,
		vultrfake.Call{Name: "ListFirewallRules", Arg: uuid},
		vultrfake.Call{Name: "CreateFirewallRule", Arg: uuid + " v4 tcp 0.0.0.0/0 22"},
		vultrfake.Call{Name: "ListFirewallGroups"},
	)
}

func TestSeedMisuse(t *testing.T) {
	f := newFake()
	f.AddVPC(t, govultr.VPC{ID: "taken", Region: "ams"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{})
	f.AddFirewallRule(t, "firewall-1", govultr.FirewallRule{ID: 1, IPType: "v4", Protocol: "icmp"})
	for _, tc := range []struct {
		name string
		seed func(testing.TB)
		want string
	}{
		{
			"a bad id", func(tb testing.TB) { f.AddSSHKey(tb, govultr.SSHKey{ID: "a/b"}) },
			`vultrfake: AddSSHKey: the id "a/b" holds more than ASCII letters, digits and "-"`,
		},
		{
			"a taken id", func(tb testing.TB) { f.AddVPC(tb, govultr.VPC{ID: "taken"}) },
			`vultrfake: AddVPC: the id "taken" is taken`,
		},
		{
			"a group id taken by a VPC", func(tb testing.TB) { f.AddFirewallGroup(tb, govultr.FirewallGroup{ID: "taken"}) },
			`vultrfake: AddFirewallGroup: the id "taken" is taken`,
		},
		{
			"a rule of a missing group",
			func(tb testing.TB) { f.AddFirewallRule(tb, "firewall-9", govultr.FirewallRule{}) },
			`vultrfake: AddFirewallRule: no firewall group "firewall-9"`,
		},
		{
			"a negative rule id",
			func(tb testing.TB) { f.AddFirewallRule(tb, "firewall-1", govultr.FirewallRule{ID: -1}) },
			`vultrfake: AddFirewallRule: the rule id -1 is negative`,
		},
		{
			"a taken rule id", func(tb testing.TB) { f.AddFirewallRule(tb, "firewall-1", govultr.FirewallRule{ID: 1}) },
			`vultrfake: AddFirewallRule: firewall-1 has a rule 1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			tc.seed(tb)
			wantFatal(t, tb, tc.want)
		})
	}
	// Nothing was stored.
	wantSSHKeys(t, f)
	wantVPCs(t, f, govultr.VPC{ID: "taken", Region: "ams", DateCreated: date})
	wantGroups(t, f, govultr.FirewallGroup{ID: "firewall-1", DateCreated: date, DateModified: date, RuleCount: 1,
		MaxRuleCount: 50})
}
