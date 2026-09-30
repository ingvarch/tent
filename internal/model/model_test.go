package model_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/model"
)

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example"

// equatePrefixes lets cmp compare netip.Prefix, whose fields are unexported.
var equatePrefixes = cmpopts.EquateComparable(netip.Prefix{})

// cluster returns the Vultr cluster of the architecture's example with every defaulted field left empty.
func cluster() *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
		Metadata: v1alpha1.ClusterMeta{Name: "prod"},
		Spec: v1alpha1.ClusterSpec{
			Cloud:   v1alpha1.Cloud{Provider: v1alpha1.ProviderVultr, Region: "ams", Vultr: &v1alpha1.VultrCloud{}},
			Access:  v1alpha1.Access{SSH: []string{"203.0.113.7/32"}},
			SSHKeys: []string{sshKey},
			Nomad:   v1alpha1.ClusterNomad{Version: "2.0.7"},
		},
	}
}

// clusterWith returns the example cluster changed by edit.
func clusterWith(edit func(c *v1alpha1.Cluster)) *v1alpha1.Cluster {
	c := cluster()
	edit(c)
	return c
}

// group returns a node group of the prod cluster with every defaulted field left empty.
func group(name string, role v1alpha1.Role, size int) *v1alpha1.NodeGroup {
	return &v1alpha1.NodeGroup{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
		Metadata: v1alpha1.NodeGroupMeta{Name: name, Cluster: "prod"},
		Spec:     v1alpha1.NodeGroupSpec{Role: role, MachineType: "vc2-2c-4gb", Size: size},
	}
}

// groups returns the servers and workers groups of the example.
func groups() []*v1alpha1.NodeGroup {
	workers := group("workers", v1alpha1.RoleClient, 3)
	workers.Spec.Nomad = v1alpha1.NodeGroupNomad{
		NodeClass: "general",
		Drivers:   []string{"docker", "exec"},
		Meta:      map[string]string{"team": "platform"},
	}
	return []*v1alpha1.NodeGroup{group("servers", v1alpha1.RoleServer, 3), workers}
}

// hetzner returns the example moved to Hetzner with every defaulted field given a value other than its default.
func hetzner() (*v1alpha1.Cluster, []*v1alpha1.NodeGroup) {
	c := clusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Cloud = v1alpha1.Cloud{
			Provider: v1alpha1.ProviderHetzner,
			Region:   "eu-central",
			Zones:    []string{"fsn1", "nbg1", "hel1"},
			Hetzner:  &v1alpha1.HetznerCloud{},
		}
		c.Spec.Networking.CIDR = "172.16.0.0/20"
		c.Spec.Access.API = []string{"198.51.100.0/24"}
		c.Spec.Nomad.TLS.VerifyHTTPSClient = new(false)
	})
	gs := groups()
	for _, g := range gs {
		g.Spec.MachineType = "cx23"
		g.Spec.Image = "ubuntu-26.04"
	}
	gs[1].Spec.Zones = []string{"nbg1"}
	return c, gs
}

func prefixes(cidrs ...string) []netip.Prefix {
	ps := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		ps[i] = netip.MustParsePrefix(c)
	}
	return ps
}

// icmp is the ICMP rule of every cluster.
func icmp() model.AccessRule {
	return model.AccessRule{
		Name: "icmp", To: model.AllNodes, Protocol: "icmp", From: prefixes("0.0.0.0/0", "::/0"),
	}
}

// intra is the rules between the nodes of every cluster whose private network is cidr.
func intra(cidr string) []model.IntraRule {
	from := prefixes(cidr)
	ports := func(first, last uint16) model.PortRange { return model.PortRange{First: first, Last: last} }
	return []model.IntraRule{
		{Name: "nomad-http", To: model.AllNodes, Protocol: "tcp", Ports: ports(4646, 4646), From: from},
		{Name: "nomad-rpc", To: model.Servers, Protocol: "tcp", Ports: ports(4647, 4647), From: from},
		{Name: "serf", To: model.Servers, Protocol: "tcp", Ports: ports(4648, 4648), From: from},
		{Name: "serf", To: model.Servers, Protocol: "udp", Ports: ports(4648, 4648), From: from},
		{Name: "dynamic", To: model.Clients, Protocol: "tcp", Ports: ports(20000, 32000), From: from},
		{Name: "dynamic", To: model.Clients, Protocol: "udp", Ports: ports(20000, 32000), From: from},
	}
}

// exampleModel is the model of the example cluster.
func exampleModel() *model.Cluster {
	return &model.Cluster{
		Name:     "prod",
		Provider: v1alpha1.ProviderVultr,
		Region:   "ams",
		Zones:    []string{"ams"},
		CIDR:     netip.MustParsePrefix("10.64.0.0/16"),
		SSHKeys:  []string{sshKey},
		Access: []model.AccessRule{
			{Name: "ssh", To: model.AllNodes, Protocol: "tcp", Port: 22, From: prefixes("203.0.113.7/32")},
			icmp(),
			{Name: "api", To: model.Servers, Protocol: "tcp", Port: 4646, From: prefixes("0.0.0.0/0")},
		},
		Intra: intra("10.64.0.0/16"),
		Join:  model.JoinSeedAndRefresh,
		Groups: []model.NodeGroup{
			{
				Name: "servers", Role: v1alpha1.RoleServer, MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04",
				Zones: []string{"ams"}, Size: 3,
			},
			{
				Name: "workers", Role: v1alpha1.RoleClient, MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04",
				Zones: []string{"ams"}, Size: 3,
			},
		},
	}
}

// modelWith returns the example model changed by edit.
func modelWith(edit func(m *model.Cluster)) *model.Cluster {
	m := exampleModel()
	edit(m)
	return m
}

func TestNew(t *testing.T) {
	hc, hgs := hetzner()
	for _, tc := range []struct {
		name    string
		cluster *v1alpha1.Cluster
		groups  []*v1alpha1.NodeGroup
		want    *model.Cluster
	}{
		{
			name:    "example with the defaults left out",
			cluster: cluster(),
			groups:  groups(),
			want:    exampleModel(),
		},
		{
			name:    "given values kept",
			cluster: hc,
			groups:  hgs,
			want: modelWith(func(m *model.Cluster) {
				m.Provider = v1alpha1.ProviderHetzner
				m.Region = "eu-central"
				m.Zones = []string{"fsn1", "nbg1", "hel1"}
				m.CIDR = netip.MustParsePrefix("172.16.0.0/20")
				m.Access[2].From = prefixes("198.51.100.0/24")
				m.Intra = intra("172.16.0.0/20")
				for i := range m.Groups {
					m.Groups[i].MachineType = "cx23"
					m.Groups[i].Image = "ubuntu-26.04"
				}
				m.Groups[0].Zones = []string{"fsn1", "nbg1", "hel1"}
				m.Groups[1].Zones = []string{"nbg1"}
			}),
		},
		{
			name:    "groups sorted by name, nil groups skipped",
			cluster: cluster(),
			groups:  []*v1alpha1.NodeGroup{groups()[1], nil, groups()[0]},
			want:    exampleModel(),
		},
		{
			name:    "combined group only",
			cluster: cluster(),
			groups:  []*v1alpha1.NodeGroup{group("dev", v1alpha1.RoleCombined, 1)},
			want: modelWith(func(m *model.Cluster) {
				m.Groups = []model.NodeGroup{{
					Name: "dev", Role: v1alpha1.RoleCombined, MachineType: "vc2-2c-4gb", Image: "ubuntu-24.04",
					Zones: []string{"ams"}, Size: 1,
				}}
			}),
		},
		{
			name:    "empty access.ssh gives no ssh rule",
			cluster: clusterWith(func(c *v1alpha1.Cluster) { c.Spec.Access.SSH = []string{} }),
			groups:  groups(),
			want:    modelWith(func(m *model.Cluster) { m.Access = m.Access[1:] }),
		},
		{
			name: "sources sorted and deduplicated, IPv4 before IPv6",
			cluster: clusterWith(func(c *v1alpha1.Cluster) {
				c.Spec.Access.SSH = []string{
					"2001:db8::/32", "203.0.113.7/32", "10.0.0.0/16", "10.0.0.0/8", "203.0.113.7/32", "2001:db8::/32",
				}
				c.Spec.Access.API = []string{"::/0", "0.0.0.0/0", "::/0"}
			}),
			groups: groups(),
			want: modelWith(func(m *model.Cluster) {
				m.Access[0].From = prefixes("10.0.0.0/8", "10.0.0.0/16", "203.0.113.7/32", "2001:db8::/32")
				m.Access[2].From = prefixes("0.0.0.0/0", "::/0")
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := model.New(tc.cluster, tc.groups)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, equatePrefixes); diff != "" {
				t.Errorf("New (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewNetworkIntents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups []*v1alpha1.NodeGroup
	}{
		{"servers without clients", []*v1alpha1.NodeGroup{group("servers", v1alpha1.RoleServer, 3)}},
		{"combined group only", []*v1alpha1.NodeGroup{group("dev", v1alpha1.RoleCombined, 1)}},
		{
			name: "combined group and clients",
			groups: []*v1alpha1.NodeGroup{
				group("dev", v1alpha1.RoleCombined, 3), group("workers", v1alpha1.RoleClient, 2),
			},
		},
		{"no groups", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := model.New(cluster(), tc.groups)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if diff := cmp.Diff(intra("10.64.0.0/16"), got.Intra, equatePrefixes); diff != "" {
				t.Errorf("Intra (-want +got):\n%s", diff)
			}
			if got.Join != model.JoinSeedAndRefresh {
				t.Errorf("Join = %v, want %v", got.Join, model.JoinSeedAndRefresh)
			}
		})
	}
}

func TestNewKeepsInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		specs func() (*v1alpha1.Cluster, []*v1alpha1.NodeGroup)
	}{
		{"defaults left out", func() (*v1alpha1.Cluster, []*v1alpha1.NodeGroup) { return cluster(), groups() }},
		{"values given", hetzner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, gs := tc.specs()
			m, err := model.New(c, gs)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Changing the model must not reach the specs either.
			m.Zones[0] = "changed"
			m.SSHKeys[0] = "changed"
			m.Access[0].From[0] = netip.Prefix{}
			m.Groups[1].Zones[0] = "changed"
			// A change to one rule between nodes must not reach the others.
			m.Intra[0].From[0] = netip.Prefix{}
			for i, r := range m.Intra[1:] {
				if len(r.From) != 1 || r.From[0] != m.CIDR {
					t.Errorf("Intra[%d].From = %v after a change to Intra[0].From, want [%v]", i+1, r.From, m.CIDR)
				}
			}

			wantCluster, wantGroups := tc.specs()
			if diff := cmp.Diff(wantCluster, c); diff != "" {
				t.Errorf("cluster changed (-before +after):\n%s", diff)
			}
			if diff := cmp.Diff(wantGroups, gs); diff != "" {
				t.Errorf("groups changed (-before +after):\n%s", diff)
			}
		})
	}
}

func TestNewErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster *v1alpha1.Cluster
		want    string // part of the error message
	}{
		{"no cluster", nil, "no cluster"},
		{
			name:    "unparsable CIDR",
			cluster: clusterWith(func(c *v1alpha1.Cluster) { c.Spec.Networking.CIDR = "10.64.0.0" }),
			want:    "cluster prod: spec.networking.cidr: ",
		},
		{
			name:    "unparsable ssh source",
			cluster: clusterWith(func(c *v1alpha1.Cluster) { c.Spec.Access.SSH = []string{"203.0.113.7/32", "nope"} }),
			want:    "cluster prod: spec.access.ssh[1]: ",
		},
		{
			name:    "unparsable api source",
			cluster: clusterWith(func(c *v1alpha1.Cluster) { c.Spec.Access.API = []string{"300.0.0.0/8"} }),
			want:    "cluster prod: spec.access.api[0]: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := model.New(tc.cluster, groups())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want one containing %q", err, tc.want)
			}
			if m != nil {
				t.Errorf("New = %+v with an error, want nil", m)
			}
		})
	}
}

func TestHasClientGroup(t *testing.T) {
	groupsOf := func(roles ...v1alpha1.Role) []model.NodeGroup {
		gs := make([]model.NodeGroup, len(roles))
		for i, r := range roles {
			gs[i] = model.NodeGroup{Role: r, Size: 3}
		}
		return gs
	}
	for _, tc := range []struct {
		name   string
		groups []model.NodeGroup
		want   bool
	}{
		{"servers and clients", groupsOf(v1alpha1.RoleServer, v1alpha1.RoleClient), true},
		{"combined and clients", groupsOf(v1alpha1.RoleCombined, v1alpha1.RoleClient), true},
		{"servers only", groupsOf(v1alpha1.RoleServer), false},
		{"combined only", groupsOf(v1alpha1.RoleCombined), false},
		{
			name:   "client group of size 0",
			groups: []model.NodeGroup{{Role: v1alpha1.RoleServer, Size: 3}, {Role: v1alpha1.RoleClient}},
			want:   true,
		},
		{"no groups", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Cluster{Groups: tc.groups}
			if got := m.HasClientGroup(); got != tc.want {
				t.Errorf("HasClientGroup() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTargetString(t *testing.T) {
	for _, tc := range []struct {
		target model.Target
		want   string
	}{
		{model.AllNodes, "all-nodes"},
		{model.Servers, "servers"},
		{model.Clients, "clients"},
		{model.Target(0), "Target(0)"}, // the zero Target opens nothing
		{model.Target(7), "Target(7)"},
		{model.Target(-1), "Target(-1)"},
	} {
		if got := tc.target.String(); got != tc.want {
			t.Errorf("Target(%d).String() = %q, want %q", int(tc.target), got, tc.want)
		}
	}
}

func TestTargetIncludes(t *testing.T) {
	for _, tc := range []struct {
		target model.Target
		role   v1alpha1.Role
		want   bool
	}{
		{model.AllNodes, v1alpha1.RoleServer, true},
		{model.AllNodes, v1alpha1.RoleClient, true},
		{model.AllNodes, v1alpha1.RoleCombined, true},
		{model.AllNodes, "", false},
		{model.AllNodes, "worker", false},
		{model.Servers, v1alpha1.RoleServer, true},
		{model.Servers, v1alpha1.RoleClient, false},
		{model.Servers, v1alpha1.RoleCombined, true}, // combined nodes run a server
		{model.Servers, "", false},
		{model.Clients, v1alpha1.RoleServer, false},
		{model.Clients, v1alpha1.RoleClient, true},
		{model.Clients, v1alpha1.RoleCombined, true}, // combined nodes run a client
		{model.Clients, "", false},
		{model.Target(0), v1alpha1.RoleServer, false}, // the zero Target opens nothing
		{model.Target(0), v1alpha1.RoleClient, false},
		{model.Target(7), v1alpha1.RoleCombined, false},
	} {
		if got := tc.target.Includes(tc.role); got != tc.want {
			t.Errorf("%v.Includes(%q) = %t, want %t", tc.target, tc.role, got, tc.want)
		}
	}
}

// TestIntraRules checks the rules between the nodes of a cluster whose private network is the given one, which New
// gives the model, and that no two rules share their sources.
func TestIntraRules(t *testing.T) {
	cidr := netip.MustParsePrefix("172.16.0.0/20")
	got := model.IntraRules(cidr)
	if diff := cmp.Diff(intra("172.16.0.0/20"), got, equatePrefixes); diff != "" {
		t.Errorf("IntraRules (-want +got):\n%s", diff)
	}
	got[0].From[0] = netip.Prefix{}
	for i, r := range got[1:] {
		if len(r.From) != 1 || r.From[0] != cidr {
			t.Errorf("rule %d: From = %v after a change to rule 0, want [%v]", i+1, r.From, cidr)
		}
	}
}

// TestDynamicPorts checks that the dynamic ports are the ports that the dynamic rules open.
func TestDynamicPorts(t *testing.T) {
	want := model.PortRange{First: 20000, Last: 32000}
	if got := model.DynamicPorts(); got != want {
		t.Errorf("DynamicPorts() = %v, want %v", got, want)
	}
	m, err := model.New(cluster(), groups())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, r := range m.Intra {
		if r.Name == "dynamic" && r.Ports != model.DynamicPorts() {
			t.Errorf("the dynamic rule over %s opens %v, want DynamicPorts() %v", r.Protocol, r.Ports,
				model.DynamicPorts())
		}
	}
}

func TestJoinStrategyString(t *testing.T) {
	for _, tc := range []struct {
		strategy model.JoinStrategy
		want     string
	}{
		{model.JoinSeedAndRefresh, "seed-and-refresh"},
		{model.JoinStrategy(0), "JoinStrategy(0)"}, // the zero JoinStrategy finds no servers
		{model.JoinStrategy(7), "JoinStrategy(7)"},
		{model.JoinStrategy(-1), "JoinStrategy(-1)"},
	} {
		if got := tc.strategy.String(); got != tc.want {
			t.Errorf("JoinStrategy(%d).String() = %q, want %q", int(tc.strategy), got, tc.want)
		}
	}
}
