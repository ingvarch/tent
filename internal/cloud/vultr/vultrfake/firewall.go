package vultrfake

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// maxRuleCount is the most rules a new firewall group holds.
const maxRuleCount = 50

// noGroup is the message of an answer about a missing firewall group.
const noGroup = "Invalid firewall group ID."

// firewallGroup is a firewall group with its rules. Its RuleCount is not kept: it is the number of rules.
type firewallGroup struct {
	govultr.FirewallGroup
	rules    []govultr.FirewallRule
	lastRule int // the id of the last rule added
}

// group returns the firewall group with id, or nil. The caller holds the lock.
func (f *Fake) group(id string) *firewallGroup {
	i := slices.IndexFunc(f.groups, func(g *firewallGroup) bool { return g.ID == id })
	if i < 0 {
		return nil
	}
	return f.groups[i]
}

// ListFirewallGroups returns every firewall group.
func (f *Fake) ListFirewallGroups(ctx context.Context) ([]govultr.FirewallGroup, error) {
	var out []govultr.FirewallGroup
	err := f.run(ctx, request{name: "ListFirewallGroups", method: http.MethodGet, path: "/v2/firewalls"}, func() error {
		out = f.firewallGroups()
		return nil
	})
	return result(out, err)
}

// firewallGroups returns a copy of every firewall group with its rule count, or nil when there is none. The caller
// holds the lock.
func (f *Fake) firewallGroups() []govultr.FirewallGroup {
	var out []govultr.FirewallGroup
	for _, g := range f.groups {
		fg := g.FirewallGroup
		fg.RuleCount = len(g.rules)
		out = append(out, fg)
	}
	return out
}

// CreateFirewallGroup creates a firewall group without rules, which may hold 50.
func (f *Fake) CreateFirewallGroup(ctx context.Context, req *govultr.FirewallGroupReq) (*govultr.FirewallGroup,
	error) {
	in := deref(req)
	r := request{name: "CreateFirewallGroup", arg: in.Description, method: http.MethodPost, path: "/v2/firewalls"}
	var out *govultr.FirewallGroup
	err := f.run(ctx, r, func() error {
		now := f.date()
		g := govultr.FirewallGroup{
			ID: f.newID("firewall"), Description: in.Description, DateCreated: now, DateModified: now,
			MaxRuleCount: maxRuleCount,
		}
		f.groups = append(f.groups, &firewallGroup{FirewallGroup: g})
		out = &g
		return nil
	})
	return result(out, err)
}

// DeleteFirewallGroup deletes a firewall group and its rules. Like Vultr, it deletes a group that instances use, and
// they are left without a firewall group.
func (f *Fake) DeleteFirewallGroup(ctx context.Context, id string) error {
	if err := vultr.CheckID("DELETE /v2/firewalls/{id}", "id", id); err != nil {
		return err
	}
	r := request{name: "DeleteFirewallGroup", arg: id, method: http.MethodDelete, path: "/v2/firewalls/" + id}
	return f.run(ctx, r, func() error {
		if !remove(&f.groups, id, func(g *firewallGroup) string { return g.ID }) {
			return r.fail(http.StatusNotFound, noGroup)
		}
		for _, in := range f.instances {
			if in.FirewallGroupID == id {
				in.FirewallGroupID = ""
			}
		}
		return nil
	})
}

// ListFirewallRules returns every rule of a firewall group.
func (f *Fake) ListFirewallRules(ctx context.Context, groupID string) ([]govultr.FirewallRule, error) {
	if err := vultr.CheckID("GET /v2/firewalls/{groupID}/rules", "groupID", groupID); err != nil {
		return nil, err
	}
	r := request{
		name: "ListFirewallRules", arg: groupID, method: http.MethodGet, path: "/v2/firewalls/" + groupID + "/rules",
	}
	var out []govultr.FirewallRule
	err := f.run(ctx, r, func() error {
		g := f.group(groupID)
		if g == nil {
			return r.fail(http.StatusNotFound, noGroup)
		}
		out = clone(g.rules)
		return nil
	})
	return result(out, err)
}

// CreateFirewallRule adds an accept rule to a firewall group, with the next id of the group, counting from 1. A rule
// without a source gets its own subnet as the source, such as 203.0.113.7/32, as Vultr lists it. The create fails
// with vultr.ErrInvalid without an ip_type or a protocol, and with a 400 "This rule is already defined", also
// vultr.ErrInvalid, when the group holds a rule with the same ip_type, protocol, subnet, subnet_size, port and
// source; an empty source counts as the rule's own subnet. It fails with vultr.ErrLimitReached when the group holds
// its most rules.
func (f *Fake) CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (
	*govultr.FirewallRule, error) {
	if err := vultr.CheckID("POST /v2/firewalls/{groupID}/rules", "groupID", groupID); err != nil {
		return nil, err
	}
	in := deref(req)
	r := request{
		name: "CreateFirewallRule", arg: ruleArg(groupID, in), method: http.MethodPost,
		path: "/v2/firewalls/" + groupID + "/rules",
	}
	rule := govultr.FirewallRule{
		IPType: in.IPType, Protocol: in.Protocol, Port: in.Port, Subnet: in.Subnet, SubnetSize: in.SubnetSize,
		Source: in.Source, Notes: in.Notes,
	}
	rule.Source = source(rule)
	var out *govultr.FirewallRule
	err := f.run(ctx, r, func() error {
		g := f.group(groupID)
		switch {
		case g == nil:
			return r.fail(http.StatusNotFound, noGroup)
		case in.IPType == "":
			return r.fail(http.StatusBadRequest, "Invalid ip_type.")
		case in.Protocol == "":
			return r.fail(http.StatusBadRequest, "Invalid protocol.")
		case slices.ContainsFunc(g.rules, func(o govultr.FirewallRule) bool { return sameRule(o, rule) }):
			return r.fail(http.StatusBadRequest, "This rule is already defined")
		case len(g.rules) >= g.MaxRuleCount:
			return r.fail(http.StatusBadRequest, "You have reached the maximum number of rules for this firewall group.")
		}
		added := g.addRule(rule)
		out = &added
		return nil
	})
	return result(out, err)
}

// sameRule reports whether a and b have the same ip_type, protocol, subnet, subnet_size, port and source. An empty
// source counts as the rule's own subnet, as Vultr lists it, so a seeded rule without one matches a created rule.
func sameRule(a, b govultr.FirewallRule) bool {
	return a.IPType == b.IPType && a.Protocol == b.Protocol && a.Subnet == b.Subnet && a.SubnetSize == b.SubnetSize &&
		a.Port == b.Port && source(a) == source(b)
}

// source returns the source of r as Vultr lists it: r's own subnet, such as 203.0.113.7/32, when r has none.
func source(r govultr.FirewallRule) string {
	return cmp.Or(r.Source, r.Subnet+"/"+strconv.Itoa(r.SubnetSize))
}

// ruleArg returns the Call.Arg of a CreateFirewallRule call, such as "firewall-1 v4 tcp 0.0.0.0/0 22".
func ruleArg(groupID string, r govultr.FirewallRuleReq) string {
	arg := fmt.Sprintf("%s %s %s %s/%d", groupID, r.IPType, r.Protocol, r.Subnet, r.SubnetSize)
	if r.Port != "" {
		arg += " " + r.Port
	}
	if r.Source != "" {
		arg += " source=" + r.Source
	}
	return arg
}

// addRule adds r to the group and returns it. An id of 0 gets the group's next one, and an empty action "accept".
// The caller holds the lock.
func (g *firewallGroup) addRule(r govultr.FirewallRule) govultr.FirewallRule {
	if r.ID == 0 {
		r.ID = g.lastRule + 1
	}
	g.lastRule = max(g.lastRule, r.ID)
	r.Action = cmp.Or(r.Action, "accept")
	g.rules = append(g.rules, r)
	return r
}

// DeleteFirewallRule removes a rule from a firewall group.
func (f *Fake) DeleteFirewallRule(ctx context.Context, groupID string, ruleID int) error {
	const route = "DELETE /v2/firewalls/{groupID}/rules/{ruleID}"
	if err := vultr.CheckID(route, "groupID", groupID); err != nil {
		return err
	}
	if err := vultr.CheckRuleID(route, ruleID); err != nil {
		return err
	}
	rule := strconv.Itoa(ruleID)
	r := request{
		name: "DeleteFirewallRule", arg: groupID + "/" + rule, method: http.MethodDelete,
		path: "/v2/firewalls/" + groupID + "/rules/" + rule,
	}
	return f.run(ctx, r, func() error {
		g := f.group(groupID)
		switch {
		case g == nil:
			return r.fail(http.StatusNotFound, noGroup)
		case !remove(&g.rules, ruleID, func(r govultr.FirewallRule) int { return r.ID }):
			return r.fail(http.StatusNotFound, "Invalid firewall rule ID.")
		}
		return nil
	})
}
