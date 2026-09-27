package vultr

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// maxRulesPerGroup is the most rules that a new firewall group holds. A group reports its own most as max_rule_count.
const maxRulesPerGroup = 50

// firewallTask makes one firewall group of the cluster, the servers' or the clients', hold the rules that let traffic
// from the internet reach its nodes. The group's description is its marker.
type firewallTask struct {
	api      API
	cluster  string
	role     string                          // roleServer or roleClient
	rules    map[string]govultr.FirewallRule // the rules the group must hold, by their text
	op       string                          // the operation id that the create puts into the marker
	attempts opState                         // what the attempts of the create share
}

// firewallTasks returns the task of the servers' firewall group, which server and combined node groups use, and the
// task of the clients' group when m has a client node group.
func (p *Provider) firewallTasks(m *model.Cluster) []engine.Task {
	roles := []string{roleServer}
	if m.HasClients() {
		roles = append(roles, roleClient)
	}
	tasks := make([]engine.Task, 0, len(roles))
	for _, role := range roles {
		tasks = append(tasks, &firewallTask{
			api: p.api, cluster: m.Name, role: role, rules: wantedRules(m.Access, role), op: p.opID(),
		})
	}
	return tasks
}

// wantedRules returns the rules of the firewall group for role, by their text: one rule for each source of each
// access rule that reaches the group's nodes.
func wantedRules(access []model.AccessRule, role string) map[string]govultr.FirewallRule {
	rules := map[string]govultr.FirewallRule{}
	for _, a := range access {
		if !reaches(a.To, role) {
			continue
		}
		for _, src := range a.From {
			r := govultr.FirewallRule{IPType: "v4", Protocol: a.Protocol, Subnet: src.Addr().String(),
				SubnetSize: src.Bits()}
			if src.Addr().Is6() {
				r.IPType = "v6"
			}
			if a.Protocol != model.ProtocolICMP {
				r.Port = strconv.Itoa(int(a.Port))
			}
			rules[ruleText(r)] = r
		}
	}
	return rules
}

// reaches reports whether an access rule to target reaches the nodes of the firewall group for role: the servers'
// group takes the rules to every node and to the servers, the clients' group the rules to every node.
func reaches(target model.Target, role string) bool {
	return target == model.AllNodes || (target == model.Servers && role == roleServer)
}

// ruleText returns the text by which tent compares rules: the IP type, the protocol and the subnet, then the port
// when the rule has one and "source=<source>" when it has a source, such as "v4 tcp 203.0.113.7/32 22" or
// "v6 icmp ::/0". It reads a rule as Vultr may write it: the IP type and the protocol in any case, an address in any
// of its forms, a range of one port, such as 22:22, as that port, and an ICMP rule with a port, which ICMP does not
// have.
func ruleText(r govultr.FirewallRule) string {
	protocol := strings.ToLower(r.Protocol)
	subnet := r.Subnet
	if a, err := netip.ParseAddr(r.Subnet); err == nil {
		subnet = a.String()
	}
	port := r.Port
	if first, last, ok := strings.Cut(port, ":"); ok && first == last {
		port = first
	}
	s := strings.ToLower(r.IPType) + " " + protocol + " " + subnet + "/" + strconv.Itoa(r.SubnetSize)
	if port != "" && protocol != model.ProtocolICMP {
		s += " " + port
	}
	if r.Source != "" {
		s += " source=" + r.Source
	}
	return s
}

// Key returns vultr.FirewallGroup/<cluster>-servers or vultr.FirewallGroup/<cluster>-clients.
func (t *firewallTask) Key() engine.Key { return firewallGroupKey(t.cluster, t.role) }

// Deps returns nothing: a firewall group needs no other object.
func (t *firewallTask) Deps() []engine.Key { return nil }

// Plan plans a create, with a "rule" diff line for each rule, when the snapshot has no firewall group for the task.
// When it has one, Plan sets the output id and plans an update when the group's rules differ from the task's: a
// "+ rule" line for each rule the group lacks and a "- rule" line for each rule to delete. It fails when the task
// has more rules than the group may hold: its max_rule_count, or 50 for a new group or one that reports none.
func (t *firewallTask) Plan(_ context.Context, env *engine.Env) (engine.Change, error) {
	s, err := snapshotOf(env)
	if err != nil {
		return engine.Change{}, err
	}
	g, ok := s.firewallGroup(t.Key())
	if limit := ruleLimit(g); len(t.rules) > limit {
		return engine.Change{}, fmt.Errorf("firewall group %s needs %d rules; Vultr allows %d", t.Key().Name,
			len(t.rules), limit)
	}
	if !ok {
		return engine.Change{Action: engine.Create, Diff: ruleDiff(t.ruleChanges(nil))}, nil
	}
	env.Outputs.Set(t.Key(), outputID, g.ID)
	diff := ruleDiff(t.ruleChanges(s.firewallRules(g.ID)))
	if len(diff) == 0 {
		return engine.Change{Action: engine.Noop}, nil
	}
	return engine.Change{Action: engine.Update, Diff: diff}, nil
}

// ruleLimit returns the most rules that the firewall group g holds: its max_rule_count, or 50 for a group that
// reports none, such as the zero group that stands for a new one.
func ruleLimit(g govultr.FirewallGroup) int {
	if g.MaxRuleCount > 0 {
		return g.MaxRuleCount
	}
	return maxRulesPerGroup
}

// ruleChanges compares a group's rules, have, with the task's. It returns the texts of the task's rules that have
// lacks, sorted, and the rules of have to delete, by ID: the ones the task does not want, and each copy of a wanted
// rule but the one with the lowest ID.
func (t *firewallTask) ruleChanges(have []govultr.FirewallRule) (missing []string, extra []govultr.FirewallRule) {
	have = slices.Clone(have)
	slices.SortFunc(have, func(a, b govultr.FirewallRule) int { return cmp.Compare(a.ID, b.ID) })
	held := map[string]bool{}
	for _, r := range have {
		text := ruleText(r)
		if _, ok := t.rules[text]; ok && !held[text] {
			held[text] = true
			continue
		}
		extra = append(extra, r)
	}
	for _, text := range slices.Sorted(maps.Keys(t.rules)) {
		if !held[text] {
			missing = append(missing, text)
		}
	}
	return missing, extra
}

// ruleDiff returns a "rule" diff line for each missing rule and each extra rule, sorted by the rules' texts.
func ruleDiff(missing []string, extra []govultr.FirewallRule) []engine.FieldDiff {
	var diff []engine.FieldDiff
	for _, text := range missing {
		diff = append(diff, engine.FieldDiff{Field: "rule", New: text})
	}
	for _, r := range extra {
		diff = append(diff, engine.FieldDiff{Field: "rule", Old: ruleText(r)})
	}
	slices.SortStableFunc(diff, func(a, b engine.FieldDiff) int {
		return strings.Compare(cmp.Or(a.New, a.Old), cmp.Or(b.New, b.Old))
	})
	return diff
}

// Apply creates the firewall group unless an earlier attempt did, sets the output id, and then makes the group hold
// the task's rules.
func (t *firewallTask) Apply(ctx context.Context, env *engine.Env, _ engine.Change) error {
	s, err := snapshotOf(env)
	if err != nil {
		return err
	}
	g, _ := s.firewallGroup(t.Key()) // the zero group when the task creates one
	id, ok := env.Outputs.Get(t.Key(), outputID)
	if !ok {
		id, err = createWithOp(ctx, &t.attempts,
			opFinder(t.api.ListFirewallGroups, firewallGroupType, t.cluster, t.op), t.createGroup)
		if err != nil {
			return err
		}
		env.Outputs.Set(t.Key(), outputID, id)
	}
	return t.reconcile(ctx, id, ruleLimit(g))
}

// createGroup sends the create of the firewall group and returns its id.
func (t *firewallTask) createGroup(ctx context.Context) (string, error) {
	g, err := t.api.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{
		Description: Marker{Cluster: t.cluster, Kind: KindFirewall, Role: t.role, Op: t.op}.String(),
	})
	if err != nil {
		return "", err
	}
	return g.ID, nil
}

// reconcile makes the firewall group id, which holds at most limit rules, hold the task's rules. It adds the missing
// rules before it deletes the ones the task does not want, so that a rule whose source changes never leaves a moment
// with neither rule. When the group lacks room for the additions, or Vultr refuses one as over the group's limit, it
// deletes first.
func (t *firewallTask) reconcile(ctx context.Context, id string, limit int) error {
	err := t.reconcileInOrder(ctx, id, limit, false)
	if errors.Is(err, ErrLimitReached) {
		return t.reconcileInOrder(ctx, id, limit, true)
	}
	return err
}

// reconcileInOrder lists the rules of the firewall group id afresh, so that it sees what an earlier attempt did, then
// adds the missing rules and deletes the ones the task does not want: the deletions first when deleteFirst is set or
// the group, which holds at most limit rules, lacks room for the additions.
func (t *firewallTask) reconcileInOrder(ctx context.Context, id string, limit int, deleteFirst bool) error {
	have, err := t.api.ListFirewallRules(ctx, id)
	if err != nil {
		return fmt.Errorf("list the rules of firewall group %s: %w", id, markRetryable(err, true))
	}
	missing, extra := t.ruleChanges(have)
	if deleteFirst || len(have)+len(missing) > limit {
		if err := t.deleteRules(ctx, id, extra); err != nil {
			return err
		}
		return t.addRules(ctx, id, missing)
	}
	if err := t.addRules(ctx, id, missing); err != nil {
		return err
	}
	return t.deleteRules(ctx, id, extra)
}

// addRules adds the task's rules whose texts are missing to the firewall group id.
func (t *firewallTask) addRules(ctx context.Context, id string, missing []string) error {
	for _, text := range missing {
		if err := t.addRule(ctx, id, t.rules[text]); err != nil {
			return fmt.Errorf("add rule %s: %w", text, err)
		}
	}
	return nil
}

// deleteRules deletes the rules extra from the firewall group id. A rule that is gone counts as deleted.
func (t *firewallTask) deleteRules(ctx context.Context, id string, extra []govultr.FirewallRule) error {
	for _, r := range extra {
		if err := deleted(t.api.DeleteFirewallRule(ctx, id, r.ID)); err != nil {
			return fmt.Errorf("delete rule %d (%s): %w", r.ID, ruleText(r), err)
		}
	}
	return nil
}

// addRule adds the rule r to the firewall group id. Vultr refuses the create with ErrLimitReached when the group holds
// its most rules, a limit that it does not raise on request.
func (t *firewallTask) addRule(ctx context.Context, id string, r govultr.FirewallRule) error {
	_, err := t.api.CreateFirewallRule(ctx, id, &govultr.FirewallRuleReq{
		IPType: r.IPType, Protocol: r.Protocol, Subnet: r.Subnet, SubnetSize: r.SubnetSize, Port: r.Port,
	})
	if errors.Is(err, ErrLimitReached) {
		limit := fmt.Sprintf("firewall group %s may already hold the most rules Vultr allows in a group", t.Key().Name)
		return &objectLimitError{limit: limit, err: err}
	}
	// A rule create is not idempotent, but it is retried as if it were: a second copy of a rule is harmless, and
	// reconcile lists the rules before it adds one, so it does not add a listed rule again.
	return markRetryable(err, true)
}

// Delete deletes the firewall group obj. A group that is gone counts as deleted. While instances still use the group,
// Vultr may refuse the delete with ErrInUse, and the engine tries it again.
func (t *firewallTask) Delete(ctx context.Context, _ *engine.Env, obj engine.Object) error {
	return deleted(t.api.DeleteFirewallGroup(ctx, obj.ID))
}
