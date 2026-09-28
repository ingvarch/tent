package vultr_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
)

// Markers of cluster prod's objects, as tent writes them.
const (
	sshKeyMarker  = "tent:cluster=prod;kind=ssh-key;fp=0a1b2c3d;op=" + opID
	vpcMarker     = "tent:cluster=prod;kind=vpc;op=" + opID
	serversMarker = "tent:cluster=prod;kind=firewall;role=server;op=" + opID
	clientsMarker = "tent:cluster=prod;kind=firewall;role=client;op=" + opID
)

// Keys of cluster prod's objects.
var (
	sshKeyKey  = engine.Key{Kind: "vultr.SSHKey", Name: "prod-0a1b2c3d"}
	vpcKey     = engine.Key{Kind: "vultr.VPC", Name: "prod"}
	serversKey = engine.Key{Kind: "vultr.FirewallGroup", Name: "prod-servers"}
	clientsKey = engine.Key{Kind: "vultr.FirewallGroup", Name: "prod-clients"}
)

// Dates of creation, oldest first.
const (
	sept20 = "2026-09-20T10:00:00+00:00"
	sept25 = "2026-09-25T10:00:00+00:00"
	sept27 = "2026-09-27T10:00:00+00:00"
)

// listCalls are the calls of an inventory of cluster prod before it lists the rules of firewall groups.
var listCalls = []vultrfake.Call{
	{Name: "ListSSHKeys"}, {Name: "ListVPCs"}, {Name: "ListFirewallGroups"},
	{Name: "ListInstances", Arg: "tent/cluster=prod"},
}

// newProvider returns a provider on api, such as a fake, with opts, that writes its log to the returned buffer as
// JSON.
func newProvider(api vultr.API, opts ...vultr.ProviderOption) (*vultr.Provider, *bytes.Buffer) {
	var log bytes.Buffer
	opts = append([]vultr.ProviderOption{vultr.WithLogger(slog.New(slog.NewJSONHandler(&log, nil)))}, opts...)
	return vultr.New(api, opts...), &log
}

// inventory takes the inventory of cluster prod with p. It stops the test when the inventory fails.
func inventory(t *testing.T, p *vultr.Provider) *vultr.Snapshot {
	t.Helper()
	snap, err := p.Inventory(t.Context(), "prod")
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	s, ok := snap.(*vultr.Snapshot)
	if !ok {
		t.Fatalf("Inventory returned a %T, want a *vultr.Snapshot", snap)
	}
	return s
}

// wantObjects checks the objects that s gives the engine.
func wantObjects(t *testing.T, s *vultr.Snapshot, want ...engine.Object) {
	t.Helper()
	if diff := cmp.Diff(want, s.Objects()); diff != "" {
		t.Errorf("Objects() (-want +got):\n%s", diff)
	}
}

// wantCalls checks the calls that reached f.
func wantCalls(t *testing.T, f *vultrfake.Fake, want ...vultrfake.Call) {
	t.Helper()
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// logRecords returns the records of a JSON log without their time.
func logRecords(t *testing.T, log *bytes.Buffer) []map[string]string {
	t.Helper()
	var records []map[string]string
	for line := range strings.Lines(log.String()) {
		var r map[string]string
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		delete(r, "time")
		records = append(records, r)
	}
	return records
}

func TestInventory(t *testing.T) {
	f := vultrfake.New()
	// Cluster prod's objects.
	key := f.AddSSHKey(t, govultr.SSHKey{ID: "key-prod", Name: sshKeyMarker, SSHKey: "ssh-ed25519 AAAAprod"})
	otherKey := f.AddSSHKey(t, govultr.SSHKey{
		ID: "key-prod-2", Name: "tent:cluster=prod;kind=ssh-key;fp=ffee0011", SSHKey: "ssh-ed25519 AAAAprod2",
	})
	vpc := f.AddVPC(t, govultr.VPC{
		ID: "vpc-prod", Region: "ams", Description: vpcMarker, V4Subnet: "10.64.0.0", V4SubnetMask: 16,
	})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-servers", Description: serversMarker, InstanceCount: 3})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-clients", Description: clientsMarker})
	for _, port := range []string{"22", "4646"} {
		f.AddFirewallRule(t, "fw-servers", govultr.FirewallRule{
			IPType: "v4", Protocol: "tcp", Subnet: "203.0.113.7", SubnetSize: 32, Port: port,
		})
	}
	f.AddFirewallRule(t, "fw-clients", govultr.FirewallRule{
		IPType: "v4", Protocol: "tcp", Subnet: "203.0.113.7", SubnetSize: 32, Port: "22",
	})
	// Objects that are not prod's: without a marker, or with another cluster's, whatever else it holds.
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-laptop", Name: "laptop"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-staging", Name: "tent:cluster=staging;kind=ssh-key;fp=0a1b2c3d"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-staging-no-fp", Name: "tent:cluster=staging;kind=ssh-key"})
	f.AddVPC(t, govultr.VPC{ID: "vpc-default", Region: "ams"})
	f.AddVPC(t, govultr.VPC{ID: "vpc-prod2", Region: "ams", Description: "tent:cluster=prod2;kind=vpc"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-web", Description: "web servers"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{
		ID: "fw-staging", Description: "tent:cluster=staging;kind=firewall;role=server",
	})
	f.AddFirewallRule(t, "fw-staging", govultr.FirewallRule{IPType: "v4", Protocol: "icmp", Subnet: "0.0.0.0"})

	p, log := newProvider(f)
	s := inventory(t, p)

	wantObjects(t, s,
		engine.Object{Key: clientsKey, ID: "fw-clients"},
		engine.Object{Key: serversKey, ID: "fw-servers"},
		engine.Object{Key: vpcKey, ID: "vpc-prod"},
		engine.Object{Key: sshKeyKey, ID: "key-prod"},
		engine.Object{Key: engine.Key{Kind: "vultr.SSHKey", Name: "prod-ffee0011"}, ID: "key-prod-2"},
	)
	for _, tc := range []struct {
		key  engine.Key
		want govultr.SSHKey
	}{
		{sshKeyKey, key},
		{engine.Key{Kind: "vultr.SSHKey", Name: "prod-ffee0011"}, otherKey},
	} {
		if got, ok := s.SSHKey(tc.key); !ok || !cmp.Equal(tc.want, got) {
			t.Errorf("SSHKey(%v) = %+v, %t; want %+v", tc.key, got, ok, tc.want)
		}
	}
	if got, ok := s.VPC(vpcKey); !ok || !cmp.Equal(vpc, got) {
		t.Errorf("VPC(%v) = %+v, %t; want %+v", vpcKey, got, ok, vpc)
	}
	groups := f.FirewallGroups() // as listed, with their rule counts
	for _, tc := range []struct {
		key engine.Key
		id  string
	}{{serversKey, "fw-servers"}, {clientsKey, "fw-clients"}} {
		want := groups[slices.IndexFunc(groups, func(g govultr.FirewallGroup) bool { return g.ID == tc.id })]
		if got, ok := s.FirewallGroup(tc.key); !ok || !cmp.Equal(want, got) {
			t.Errorf("FirewallGroup(%v) = %+v, %t; want %+v", tc.key, got, ok, want)
		}
		if diff := cmp.Diff(f.FirewallRules(tc.id), s.FirewallRules(tc.id)); diff != "" {
			t.Errorf("FirewallRules(%s) (-want +got):\n%s", tc.id, diff)
		}
	}

	// Nothing of another cluster.
	if got, ok := s.SSHKey(engine.Key{Kind: "vultr.SSHKey", Name: "staging-0a1b2c3d"}); ok {
		t.Errorf("SSHKey of cluster staging = %+v, want none", got)
	}
	if got, ok := s.VPC(engine.Key{Kind: "vultr.VPC", Name: "prod2"}); ok {
		t.Errorf("VPC of cluster prod2 = %+v, want none", got)
	}
	if got, ok := s.FirewallGroup(engine.Key{Kind: "vultr.FirewallGroup", Name: "staging-servers"}); ok {
		t.Errorf("FirewallGroup of cluster staging = %+v, want none", got)
	}
	if got := s.FirewallRules("fw-staging"); got != nil {
		t.Errorf("FirewallRules(fw-staging) = %+v, want none", got)
	}
	wantCalls(t, f, append(slices.Clone(listCalls),
		vultrfake.Call{Name: "ListFirewallRules", Arg: "fw-clients"},
		vultrfake.Call{Name: "ListFirewallRules", Arg: "fw-servers"},
	)...)
	if log.Len() != 0 {
		t.Errorf("the inventory logged:\n%s\nwant nothing", log)
	}
}

func TestInventoryEmpty(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	s := inventory(t, p)
	if got := s.Objects(); len(got) != 0 {
		t.Errorf("Objects() = %+v, want none", got)
	}
	wantCalls(t, f, listCalls...)
}

func TestInventorySkipsMarkersTentDoesNotWrite(t *testing.T) {
	f := vultrfake.New()
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-malformed", Name: "tent:cluster=prod;kind"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-no-fp", Name: "tent:cluster=prod;kind=ssh-key"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-short-fp", Name: "tent:cluster=prod;kind=ssh-key;fp=0a1b"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-of-kind-vpc", Name: "tent:cluster=prod;kind=vpc"})
	f.AddVPC(t, govultr.VPC{ID: "vpc-empty-marker", Description: "tent:"})
	f.AddVPC(t, govultr.VPC{ID: "vpc-of-kind-firewall", Description: "tent:cluster=prod;kind=firewall;role=server"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-no-role", Description: "tent:cluster=prod;kind=firewall"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{
		ID: "fw-combined", Description: "tent:cluster=prod;kind=firewall;role=combined",
	})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-upper", Description: "tent:cluster=Prod;kind=firewall"})

	p, log := newProvider(f)
	s := inventory(t, p)

	wantObjects(t, s)
	wantCalls(t, f, listCalls...)
	warning := func(typ, id, reason string) map[string]string {
		return map[string]string{
			"level": "WARN", "msg": "skipping a Vultr object with a marker that tent does not write",
			"type": typ, "id": id, "reason": reason,
		}
	}
	want := []map[string]string{
		warning("SSH key", "key-malformed", `marker "tent:cluster=prod;kind": no "=" in "kind"`),
		warning("SSH key", "key-no-fp", `marker "tent:cluster=prod;kind=ssh-key": fp is not 8 lower-case hex digits`),
		warning("SSH key", "key-short-fp",
			`marker "tent:cluster=prod;kind=ssh-key;fp=0a1b": fp is not 8 lower-case hex digits`),
		warning("SSH key", "key-of-kind-vpc", `marker "tent:cluster=prod;kind=vpc": kind is not ssh-key`),
		warning("VPC", "vpc-empty-marker", `marker "tent:": no cluster`),
		warning("VPC", "vpc-of-kind-firewall",
			`marker "tent:cluster=prod;kind=firewall;role=server": kind is not vpc`),
		warning("firewall group", "fw-no-role", `marker "tent:cluster=prod;kind=firewall": role is not server or client`),
		warning("firewall group", "fw-combined",
			`marker "tent:cluster=prod;kind=firewall;role=combined": role is not server or client`),
		warning("firewall group", "fw-upper", `marker "tent:cluster=Prod;kind=firewall": upper case in cluster`),
	}
	if diff := cmp.Diff(want, logRecords(t, log)); diff != "" {
		t.Errorf("warnings (-want +got):\n%s", diff)
	}
}

// objectCopy is one copy of an object in the dedupe tests.
type objectCopy struct {
	id        string
	created   string // date_created
	instances int    // how many nodes of cluster prod use a firewall group
	others    int    // how many instances of cluster staging use a firewall group
	apiCount  int    // a firewall group's instance_count, which the inventory does not read
}

// dedupeKind is a kind of object in the dedupe tests: the key of its copies, how to seed a copy, and the ID of the
// copy that a snapshot keeps.
type dedupeKind struct {
	name string
	key  engine.Key
	add  func(t *testing.T, f *vultrfake.Fake, c objectCopy)
	kept func(s *vultr.Snapshot) (string, bool)
}

var dedupeKinds = []dedupeKind{
	{
		name: "SSH key",
		key:  sshKeyKey,
		add: func(t *testing.T, f *vultrfake.Fake, c objectCopy) {
			f.AddSSHKey(t, govultr.SSHKey{ID: c.id, Name: sshKeyMarker, DateCreated: c.created})
		},
		kept: func(s *vultr.Snapshot) (string, bool) {
			k, ok := s.SSHKey(sshKeyKey)
			return k.ID, ok
		},
	},
	{
		name: "VPC",
		key:  vpcKey,
		add: func(t *testing.T, f *vultrfake.Fake, c objectCopy) {
			f.AddVPC(t, govultr.VPC{ID: c.id, Description: vpcMarker, DateCreated: c.created})
		},
		kept: func(s *vultr.Snapshot) (string, bool) {
			v, ok := s.VPC(vpcKey)
			return v.ID, ok
		},
	},
	{
		name: "firewall group",
		key:  serversKey,
		add: func(t *testing.T, f *vultrfake.Fake, c objectCopy) {
			f.AddFirewallGroup(t, govultr.FirewallGroup{
				ID: c.id, Description: serversMarker, DateCreated: c.created, InstanceCount: c.apiCount,
			})
			for range c.instances {
				f.AddInstance(t, govultr.Instance{FirewallGroupID: c.id, Tags: []string{"tent/cluster=prod"}})
			}
			for range c.others {
				f.AddInstance(t, govultr.Instance{FirewallGroupID: c.id, Tags: []string{"tent/cluster=staging"}})
			}
		},
		kept: func(s *vultr.Snapshot) (string, bool) {
			g, ok := s.FirewallGroup(serversKey)
			return g.ID, ok
		},
	},
}

// dedupeCase is copies of one object and the ID of the copy to keep.
type dedupeCase struct {
	name   string
	copies []objectCopy
	keep   string
}

func TestInventoryKeepsTheOldestCopy(t *testing.T) {
	for _, k := range dedupeKinds {
		for _, tc := range []dedupeCase{
			{
				name:   "the oldest",
				copies: []objectCopy{{id: "a", created: sept27}, {id: "b", created: sept20}, {id: "c", created: sept25}},
				keep:   "b",
			},
			{
				name: "the oldest in time, whatever the offset",
				copies: []objectCopy{
					{id: "a", created: "2026-09-20T11:00:00+00:00"}, {id: "b", created: "2026-09-20T12:00:00+02:00"},
				},
				keep: "b",
			},
			{
				name:   "the lowest ID of the oldest",
				copies: []objectCopy{{id: "c", created: sept20}, {id: "b", created: sept25}, {id: "a", created: sept20}},
				keep:   "a",
			},
			{
				name:   "a date that does not parse comes last",
				copies: []objectCopy{{id: "a", created: "yesterday"}, {id: "b", created: sept27}},
				keep:   "b",
			},
			{
				name:   "the lowest ID when no date parses",
				copies: []objectCopy{{id: "b", created: "yesterday"}, {id: "a", created: "2026-09-20"}},
				keep:   "a",
			},
		} {
			t.Run(k.name+"/"+tc.name, func(t *testing.T) { testDedupe(t, k, tc) })
		}
	}
}

func TestInventoryKeepsTheFirewallGroupThatTheMostNodesUse(t *testing.T) {
	k := dedupeKinds[slices.IndexFunc(dedupeKinds, func(k dedupeKind) bool { return k.key == serversKey })]
	for _, tc := range []dedupeCase{
		{
			name: "the most instances",
			copies: []objectCopy{
				{id: "a", created: sept20}, {id: "b", created: sept27, instances: 3}, {id: "c", instances: 1},
			},
			keep: "b",
		},
		{
			name: "the oldest of the most instances",
			copies: []objectCopy{
				{id: "a", created: sept27, instances: 2}, {id: "b", created: sept25, instances: 2},
				{id: "c", created: sept20, instances: 1},
			},
			keep: "b",
		},
		{
			name: "the lowest ID of the oldest with the most instances",
			copies: []objectCopy{
				{id: "c", created: sept25, instances: 2}, {id: "b", created: sept25, instances: 2},
				{id: "a", created: sept20},
			},
			keep: "b",
		},
		{
			name: "a date that does not parse comes last",
			copies: []objectCopy{
				{id: "a", created: "yesterday", instances: 2}, {id: "b", created: sept27, instances: 2},
			},
			keep: "b",
		},
		{
			name:   "the newer copy that a node uses",
			copies: []objectCopy{{id: "a", created: sept20}, {id: "b", created: sept27, instances: 1}},
			keep:   "b",
		},
		{
			// Vultr gave no instance_count on GET /v2/firewalls/{id}, and it would count other clusters' instances.
			name: "the API's instance_count does not count",
			copies: []objectCopy{
				{id: "a", created: sept20, apiCount: 5}, {id: "b", created: sept27, instances: 1},
			},
			keep: "b",
		},
		{
			name: "instances of another cluster do not count",
			copies: []objectCopy{
				{id: "a", created: sept27, others: 3}, {id: "b", created: sept20}, {id: "c", created: sept25},
			},
			keep: "b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) { testDedupe(t, k, tc) })
	}
}

// testDedupe seeds the copies of tc and checks that the snapshot keeps tc.keep and marks every other copy a
// duplicate. For firewall groups it checks that only the kept one's rules are listed.
func testDedupe(t *testing.T, k dedupeKind, tc dedupeCase) {
	f := vultrfake.New()
	var want []engine.Object
	for _, c := range tc.copies {
		k.add(t, f, c)
		want = append(want, engine.Object{Key: k.key, ID: c.id, Duplicate: c.id != tc.keep})
	}
	slices.SortFunc(want, func(a, b engine.Object) int { return strings.Compare(a.ID, b.ID) })
	p, _ := newProvider(f)
	s := inventory(t, p)

	wantObjects(t, s, want...)
	if got, ok := k.kept(s); !ok || got != tc.keep {
		t.Errorf("the snapshot keeps %q (found: %t), want %q", got, ok, tc.keep)
	}
	calls := slices.Clone(listCalls)
	if k.key.Kind == "vultr.FirewallGroup" {
		calls = append(calls, vultrfake.Call{Name: "ListFirewallRules", Arg: tc.keep})
	}
	wantCalls(t, f, calls...)
}

func TestInventoryObjectsOrder(t *testing.T) {
	f := vultrfake.New()
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-1", Name: "tent:cluster=prod;kind=ssh-key;fp=ffffffff"})
	f.AddSSHKey(t, govultr.SSHKey{ID: "key-2", Name: "tent:cluster=prod;kind=ssh-key;fp=00000000"})
	f.AddVPC(t, govultr.VPC{ID: "vpc-1", Description: vpcMarker, DateCreated: sept27})
	f.AddVPC(t, govultr.VPC{ID: "vpc-2", Description: vpcMarker, DateCreated: sept20})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-s2", Description: serversMarker, DateCreated: sept20})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-s1", Description: serversMarker, DateCreated: sept27})
	f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-c", Description: clientsMarker})

	p, _ := newProvider(f)
	s := inventory(t, p)

	// Firewall groups, then the VPC, then SSH keys, in the order the engine deletes them; then by name and ID.
	wantObjects(t, s,
		engine.Object{Key: clientsKey, ID: "fw-c"},
		engine.Object{Key: serversKey, ID: "fw-s1", Duplicate: true},
		engine.Object{Key: serversKey, ID: "fw-s2"},
		engine.Object{Key: vpcKey, ID: "vpc-1", Duplicate: true},
		engine.Object{Key: vpcKey, ID: "vpc-2"},
		engine.Object{Key: engine.Key{Kind: "vultr.SSHKey", Name: "prod-00000000"}, ID: "key-2"},
		engine.Object{Key: engine.Key{Kind: "vultr.SSHKey", Name: "prod-ffffffff"}, ID: "key-1"},
	)
	// The rules of the kept groups, in the same order.
	wantCalls(t, f, append(slices.Clone(listCalls),
		vultrfake.Call{Name: "ListFirewallRules", Arg: "fw-c"},
		vultrfake.Call{Name: "ListFirewallRules", Arg: "fw-s2"},
	)...)
}

func TestInventoryListErrors(t *testing.T) {
	for _, tc := range []struct{ call, path string }{
		{"ListSSHKeys", "/v2/ssh-keys"},
		{"ListVPCs", "/v2/vpcs"},
		{"ListFirewallGroups", "/v2/firewalls"},
		{"ListInstances", "/v2/instances"},
		{"ListFirewallRules", "/v2/firewalls/fw-servers/rules"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			f := vultrfake.New()
			f.AddFirewallGroup(t, govultr.FirewallGroup{ID: "fw-servers", Description: serversMarker})
			apiErr := vultr.NewAPIError(http.MethodGet, tc.path, http.StatusInternalServerError, "Internal error", 0)
			f.Fail(t, tc.call, apiErr, 1)
			p, _ := newProvider(f)

			snap, err := p.Inventory(t.Context(), "prod")
			if snap != nil {
				t.Errorf("Inventory returned the snapshot %+v with the error", snap)
			}
			if want := "inventory of cluster prod: " + apiErr.Error(); err == nil || err.Error() != want {
				t.Errorf("Inventory error = %v, want %q", err, want)
			}
			if e := (*vultr.APIError)(nil); !errors.As(err, &e) || e != apiErr {
				t.Errorf("errors.As(%v, *vultr.APIError) = %v, want the API error", err, e)
			}
		})
	}
}
