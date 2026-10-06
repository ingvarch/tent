package vultrfake

import (
	"cmp"
	"slices"
	"testing"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// AddSSHKey stores an SSH key without a call, and returns it as stored. It fails the test when the id holds more
// than ASCII letters, digits and "-", or is taken.
func (f *Fake) AddSSHKey(tb testing.TB, k govultr.SSHKey) govultr.SSHKey {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.seedID(tb, "AddSSHKey", "ssh-key", k.ID)
	if !ok {
		return govultr.SSHKey{}
	}
	k.ID, k.DateCreated = id, f.dateOr(k.DateCreated)
	f.sshKeys = append(f.sshKeys, k)
	return k
}

// AddVPC stores a VPC without a call, and returns it as stored. It fails the test when the id holds more than ASCII
// letters, digits and "-", or is taken.
func (f *Fake) AddVPC(tb testing.TB, v govultr.VPC) govultr.VPC {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.seedID(tb, "AddVPC", "vpc", v.ID)
	if !ok {
		return govultr.VPC{}
	}
	v.ID, v.DateCreated = id, f.dateOr(v.DateCreated)
	f.vpcs = append(f.vpcs, v)
	return v
}

// AddFirewallGroup stores a firewall group without rules and without a call, and returns it as stored. An empty
// date_modified gets the date_created, and an empty max_rule_count 50. RuleCount is not kept: it is the number of the
// group's rules. It fails the test when the id holds more than ASCII letters, digits and "-", or is taken.
func (f *Fake) AddFirewallGroup(tb testing.TB, g govultr.FirewallGroup) govultr.FirewallGroup {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.seedID(tb, "AddFirewallGroup", "firewall", g.ID)
	if !ok {
		return govultr.FirewallGroup{}
	}
	g.ID, g.DateCreated = id, f.dateOr(g.DateCreated)
	g.DateModified = cmp.Or(g.DateModified, g.DateCreated)
	g.MaxRuleCount = cmp.Or(g.MaxRuleCount, maxRuleCount)
	g.RuleCount = 0
	f.groups = append(f.groups, &firewallGroup{FirewallGroup: g})
	return g
}

// AddFirewallRule stores a rule of the firewall group groupID without a call, and returns it as stored. An id of 0
// gets the group's next one, and an empty action "accept"; the source stays as given, even when empty. It fails the
// test when the group does not exist, or the id is negative or taken in the group.
func (f *Fake) AddFirewallRule(tb testing.TB, groupID string, r govultr.FirewallRule) govultr.FirewallRule {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.group(groupID)
	switch {
	case g == nil:
		tb.Fatalf("vultrfake: AddFirewallRule: no firewall group %q", groupID)
	case r.ID < 0:
		tb.Fatalf("vultrfake: AddFirewallRule: the rule id %d is negative", r.ID)
	case slices.ContainsFunc(g.rules, func(o govultr.FirewallRule) bool { return o.ID == r.ID }):
		tb.Fatalf("vultrfake: AddFirewallRule: %s has a rule %d", groupID, r.ID)
	default:
		return g.addRule(r)
	}
	return govultr.FirewallRule{}
}

// AddInstance stores an instance without a call, attached to the VPCs with vpcIDs in order, and returns it as stored.
// Empty status fields read active, running and ok, and the instance keeps its status fields however often it is read.
// In each VPC it gets an address and a MAC as a created instance does, and ListInstanceVPCs lists them at once. It
// fails the test when a VPC does not exist, or the id holds more than ASCII letters, digits and "-", or is taken.
func (f *Fake) AddInstance(tb testing.TB, in govultr.Instance, vpcIDs ...string) govultr.Instance {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range vpcIDs {
		if _, ok := f.findVPC(id); !ok {
			tb.Fatalf("vultrfake: AddInstance: no VPC %q", id)
			return govultr.Instance{}
		}
	}
	id, ok := f.seedID(tb, "AddInstance", "instance", in.ID)
	if !ok {
		return govultr.Instance{}
	}
	in.ID, in.DateCreated = id, f.dateOr(in.DateCreated)
	in.Status = cmp.Or(in.Status, "active")
	in.PowerStatus = cmp.Or(in.PowerStatus, "running")
	in.ServerStatus = cmp.Or(in.ServerStatus, "ok")
	in.Tags, in.Features = slices.Clone(in.Tags), slices.Clone(in.Features)
	inst := &instance{Instance: in}
	f.instances = append(f.instances, inst)
	f.attach(inst, vpcIDs)
	return inst.view()
}

// SetInstanceTags replaces the tags of an instance without a call, as a change by hand does. It fails the test for an
// unknown instance.
func (f *Fake) SetInstanceTags(tb testing.TB, id string, tags ...string) {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instance(id); in != nil {
		in.Tags = slices.Clone(tags)
	} else {
		tb.Fatalf("vultrfake: SetInstanceTags: no instance %q", id)
	}
}

// SetInstanceUserData replaces the user data of an instance, base64 as sent, without a call, as a change by hand does.
// It fails the test for an unknown instance.
func (f *Fake) SetInstanceUserData(tb testing.TB, id, userData string) {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instance(id); in != nil {
		in.userData = userData
	} else {
		tb.Fatalf("vultrfake: SetInstanceUserData: no instance %q", id)
	}
}

// seedID returns the id of an object to seed: id, or a new id of kind when id is empty. It fails the test and returns
// false when id holds more than ASCII letters, digits and "-", or is taken. method names the seeding method, for the
// message. The caller holds the lock.
func (f *Fake) seedID(tb testing.TB, method, kind, id string) (string, bool) {
	tb.Helper()
	switch {
	case id == "":
		return f.newID(kind), true
	case vultr.CheckID(method, "id", id) != nil:
		tb.Fatalf(`vultrfake: %s: the id %q holds more than ASCII letters, digits and "-"`, method, id)
	case f.ids[id]:
		tb.Fatalf("vultrfake: %s: the id %q is taken", method, id)
	default:
		f.ids[id] = true
		return id, true
	}
	return "", false
}

// dateOr returns d, or the clock's time as a date_created when d is empty.
func (f *Fake) dateOr(d string) string {
	if d != "" {
		return d
	}
	return f.date()
}
