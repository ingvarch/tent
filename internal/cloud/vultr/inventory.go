package vultr

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
)

// snapshot is what Inventory found of one cluster. The engine reads Objects; each task reads the copy of its object
// that the snapshot keeps. Nothing changes it once Inventory returns it.
type snapshot struct {
	objects []engine.Object                      // every owned object, duplicates included, in the order of Objects
	sshKeys map[engine.Key]govultr.SSHKey        // the kept SSH key of each key
	vpcs    map[engine.Key]govultr.VPC           // the kept VPC of each key
	groups  map[engine.Key]govultr.FirewallGroup // the kept firewall group of each key
	rules   map[string][]govultr.FirewallRule    // the rules of each kept firewall group, by the group's ID
}

// Objects returns every object the cluster owns, with the duplicates marked: the firewall groups, then the VPCs, then
// the SSH keys, the order the engine deletes them in; then by name and ID.
func (s *snapshot) Objects() []engine.Object { return slices.Clone(s.objects) }

// sshKey returns the SSH key that the snapshot keeps for k.
func (s *snapshot) sshKey(k engine.Key) (govultr.SSHKey, bool) {
	key, ok := s.sshKeys[k]
	return key, ok
}

// vpc returns the VPC that the snapshot keeps for k.
func (s *snapshot) vpc(k engine.Key) (govultr.VPC, bool) {
	v, ok := s.vpcs[k]
	return v, ok
}

// firewallGroup returns the firewall group that the snapshot keeps for k.
func (s *snapshot) firewallGroup(k engine.Key) (govultr.FirewallGroup, bool) {
	g, ok := s.groups[k]
	return g, ok
}

// firewallRules returns the rules of a firewall group that the snapshot keeps, and nil for any other group.
func (s *snapshot) firewallRules(groupID string) []govultr.FirewallRule {
	return slices.Clone(s.rules[groupID])
}

// Inventory lists the SSH keys, VPCs and firewall groups that the cluster owns, with one list call for each, and the
// rules of each firewall group it keeps. An object is the cluster's when the marker in its name (an SSH key) or its
// description (a VPC or a firewall group) names the cluster.
//
// Of the owned objects with one key it keeps one, and marks the others as duplicates for the engine to delete: the
// firewall group with the most instances; else, and for SSH keys and VPCs, the oldest; else the one with the lowest
// ID. A creation date that does not parse counts as newer than any that does.
//
// It skips these objects with a warning, so tent neither adopts nor deletes them:
//   - a text that starts with "tent:" and does not parse, whatever cluster it names;
//   - a marker of the cluster whose kind does not match the list it came from, such as kind=vpc on an SSH key;
//   - an SSH key marker of the cluster without an fp of 8 lower-case hex digits;
//   - a firewall group marker of the cluster without the role server or client.
func (p *Provider) Inventory(ctx context.Context, cluster string) (engine.Snapshot, error) {
	s, err := p.inventory(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("inventory of cluster %s: %w", cluster, err)
	}
	return s, nil
}

// inventory does the work of Inventory.
func (p *Provider) inventory(ctx context.Context, cluster string) (*snapshot, error) {
	keys, err := p.api.ListSSHKeys(ctx)
	if err != nil {
		return nil, err
	}
	vpcs, err := p.api.ListVPCs(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := p.api.ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	s := &snapshot{rules: map[string][]govultr.FirewallRule{}}
	s.sshKeys = keepOne(claim(ctx, p.log, cluster, keys, sshKeyType), sshKeyType, &s.objects)
	s.vpcs = keepOne(claim(ctx, p.log, cluster, vpcs, vpcType), vpcType, &s.objects)
	s.groups = keepOne(claim(ctx, p.log, cluster, groups, firewallGroupType), firewallGroupType, &s.objects)
	slices.SortFunc(s.objects, compareObjects)
	for _, o := range s.objects {
		if o.Key.Kind != engineKindFirewallGroup || o.Duplicate {
			continue
		}
		if s.rules[o.ID], err = p.api.ListFirewallRules(ctx, o.ID); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// objectType tells the inventory how to read one type of Vultr object.
type objectType[T any] struct {
	name string         // for warnings, such as "SSH key"
	kind string         // the kind that the object's marker must give, such as KindSSHKey
	text func(T) string // the free-text field that holds the object's marker
	id   func(T) string
	// key returns the key of an object of the cluster with the marker m, or why m lacks what the type needs.
	key func(cluster string, m Marker) (engine.Key, string)
	// keep returns a negative number when a rather than b is the copy to keep, and a positive one for b.
	keep func(a, b T) int
}

var sshKeyType = objectType[govultr.SSHKey]{
	name: "SSH key",
	kind: KindSSHKey,
	text: func(k govultr.SSHKey) string { return k.Name },
	id:   func(k govultr.SSHKey) string { return k.ID },
	key: func(cluster string, m Marker) (engine.Key, string) {
		if !fingerprintPattern.MatchString(m.Fingerprint) {
			return engine.Key{}, "fp is not 8 lower-case hex digits"
		}
		return sshKeyKey(cluster, m.Fingerprint), ""
	},
	keep: func(a, b govultr.SSHKey) int {
		return cmp.Or(compareCreated(a.DateCreated, b.DateCreated), strings.Compare(a.ID, b.ID))
	},
}

var vpcType = objectType[govultr.VPC]{
	name: "VPC",
	kind: KindVPC,
	text: func(v govultr.VPC) string { return v.Description },
	id:   func(v govultr.VPC) string { return v.ID },
	key:  func(cluster string, _ Marker) (engine.Key, string) { return vpcKey(cluster), "" },
	keep: func(a, b govultr.VPC) int {
		return cmp.Or(compareCreated(a.DateCreated, b.DateCreated), strings.Compare(a.ID, b.ID))
	},
}

var firewallGroupType = objectType[govultr.FirewallGroup]{
	name: "firewall group",
	kind: KindFirewall,
	text: func(g govultr.FirewallGroup) string { return g.Description },
	id:   func(g govultr.FirewallGroup) string { return g.ID },
	key: func(cluster string, m Marker) (engine.Key, string) {
		if _, ok := firewallGroupNames[m.Role]; !ok {
			return engine.Key{}, "role is not server or client"
		}
		return firewallGroupKey(cluster, m.Role), ""
	},
	keep: func(a, b govultr.FirewallGroup) int {
		return cmp.Or(cmp.Compare(b.InstanceCount, a.InstanceCount), compareCreated(a.DateCreated, b.DateCreated),
			strings.Compare(a.ID, b.ID))
	},
}

// keyOf reads the marker in an object's text, and returns it with the object's key when the cluster owns the object:
// the marker names the cluster and is one that tent writes on objects of the type. For a text that tent does not
// write, why says what is wrong with it: a text that starts with "tent:" and does not parse, or a marker of the
// cluster whose kind is not the type's, or that lacks what the type needs, such as an SSH key's fp.
func (t objectType[T]) keyOf(cluster, text string) (m Marker, k engine.Key, owned bool, why string) {
	m, ok, err := ParseMarker(text)
	switch {
	case !ok:
		return Marker{}, engine.Key{}, false, ""
	case err != nil:
		return Marker{}, engine.Key{}, false, err.Error()
	case m.Cluster != cluster:
		return Marker{}, engine.Key{}, false, ""
	case m.Kind != t.kind:
		return Marker{}, engine.Key{}, false, fmt.Sprintf("marker %q: kind is not %s", text, t.kind)
	}
	if k, why = t.key(cluster, m); why != "" {
		return Marker{}, engine.Key{}, false, fmt.Sprintf("marker %q: %s", text, why)
	}
	return m, k, true, ""
}

// claim returns the objects that the cluster owns, by key. It logs a warning for each object that keyOf finds wrong.
func claim[T any](ctx context.Context, log *slog.Logger, cluster string, objs []T, t objectType[T]) map[engine.Key][]T {
	owned := map[engine.Key][]T{}
	for _, o := range objs {
		_, k, ok, why := t.keyOf(cluster, t.text(o))
		if why != "" {
			log.WarnContext(ctx, "skipping a Vultr object with a marker that tent does not write",
				"type", t.name, "id", t.id(o), "reason", why)
		}
		if ok {
			owned[k] = append(owned[k], o)
		}
	}
	return owned
}

// keepOne returns the copy to keep of each key of owned, and appends every copy to objects, the others marked as
// duplicates.
func keepOne[T any](owned map[engine.Key][]T, t objectType[T], objects *[]engine.Object) map[engine.Key]T {
	kept := make(map[engine.Key]T, len(owned))
	for k, copies := range owned {
		slices.SortFunc(copies, t.keep)
		kept[k] = copies[0]
		for i, c := range copies {
			*objects = append(*objects, engine.Object{Key: k, ID: t.id(c), Duplicate: i > 0})
		}
	}
	return kept
}

// compareCreated compares two creation dates, RFC 3339 as Vultr gives them, in time. A date that does not parse comes
// after every one that does.
func compareCreated(a, b string) int {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return 1
	case errB != nil:
		return -1
	}
	return ta.Compare(tb)
}

// compareObjects orders objects by their kind in infraKindOrder, then by name, then by ID.
func compareObjects(a, b engine.Object) int {
	return cmp.Or(cmp.Compare(slices.Index(infraKindOrder, a.Key.Kind), slices.Index(infraKindOrder, b.Key.Kind)),
		strings.Compare(a.Key.Name, b.Key.Name), strings.Compare(a.ID, b.ID))
}
