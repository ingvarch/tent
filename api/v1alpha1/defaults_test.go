package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSetDefaultsFillsEmptyFields(t *testing.T) {
	got := fixtures()
	SetDefaults(got.Cluster, got.NodeGroups)

	want := fixtures()
	spec := &want.Cluster.Spec
	spec.Channel = "stable"
	spec.Cloud.Zones = []string{"ams"}
	spec.Networking.CIDR = "10.64.0.0/16"
	spec.Access.API = []string{"0.0.0.0/0"}
	spec.Nomad.Region = "global"
	spec.Nomad.TLS.VerifyHTTPSClient = new(true)
	spec.Nomad.ClientIntroduction = ClientIntroductionStrict
	for _, g := range want.NodeGroups {
		g.Spec.Image = "ubuntu-24.04"
		g.Spec.Zones = []string{"ams"}
	}
	workers := want.NodeGroups[1]
	workers.Spec.Nomad.NodePool = "default"

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("SetDefaults (-want +got):\n%s", diff)
	}
}

func TestSetDefaultsKeepsSetFields(t *testing.T) {
	got := allSet()
	SetDefaults(got.Cluster, got.NodeGroups)
	if diff := cmp.Diff(allSet(), got); diff != "" {
		t.Errorf("SetDefaults changed fields the operator set (-want +got):\n%s", diff)
	}
}

func TestSetDefaultsKeepsVultrZones(t *testing.T) {
	// Vultr zones must equal [region]; a wrong list stays for Validate to report.
	c := cluster()
	c.Spec.Cloud.Zones = []string{"ewr"}
	SetDefaults(c, groups())
	if diff := cmp.Diff([]string{"ewr"}, c.Spec.Cloud.Zones); diff != "" {
		t.Errorf("spec.cloud.zones (-want +got):\n%s", diff)
	}
}

func TestSetDefaultsFillsEmptyVultrZones(t *testing.T) {
	// zones: [] reads as an empty, non-nil list; like a left-out one, it becomes [region].
	tests := []struct {
		name  string
		zones []string
	}{
		{"left out", nil},
		{"empty", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cluster()
			c.Spec.Cloud.Zones = tt.zones
			SetDefaults(c, groups())
			if diff := cmp.Diff([]string{"ams"}, c.Spec.Cloud.Zones); diff != "" {
				t.Errorf("spec.cloud.zones (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetDefaultsIsIdempotent(t *testing.T) {
	inputs := []struct {
		name  string
		build func() objects
	}{
		{"fixtures", fixtures},
		{"all fields set", allSet},
		{"combined", combined},
		{"hetzner", hetzner},
		// drivers: [] in YAML decodes as an empty, non-nil slice.
		{"empty drivers", func() objects {
			o := fixtures()
			o.NodeGroups[1].Spec.Nomad.Drivers = []string{}
			return o
		}},
	}
	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			once, twice := in.build(), in.build()
			SetDefaults(once.Cluster, once.NodeGroups)
			SetDefaults(twice.Cluster, twice.NodeGroups)
			SetDefaults(twice.Cluster, twice.NodeGroups)
			if diff := cmp.Diff(once, twice); diff != "" {
				t.Errorf("second SetDefaults changed the spec (-once +twice):\n%s", diff)
			}
		})
	}
}

func TestSetDefaultsClientIntroductionFollowsTheCombinedRole(t *testing.T) {
	last := combined()
	last.NodeGroups = []*NodeGroup{nil, last.NodeGroups[1], last.NodeGroups[0]}
	tests := []struct {
		name string
		in   objects
		want ClientIntroduction
	}{
		{"combined group", combined(), ClientIntroductionWarn},
		{"combined group last, after a nil entry", last, ClientIntroductionWarn},
		{"no combined group", fixtures(), ClientIntroductionStrict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetDefaults(tt.in.Cluster, tt.in.NodeGroups)
			if got := tt.in.Cluster.Spec.Nomad.ClientIntroduction; got != tt.want {
				t.Errorf("clientIntroduction = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetDefaultsNodePoolFollowsTheRole(t *testing.T) {
	tests := []struct {
		role Role
		want string
	}{
		{RoleServer, ""},
		{RoleClient, "default"},
		{RoleCombined, "default"},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			g := groups()[1]
			g.Spec.Role = tt.role
			SetDefaults(cluster(), []*NodeGroup{g})
			if got := g.Spec.Nomad.NodePool; got != tt.want {
				t.Errorf("nodePool = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetDefaultsKeepsAnEmptyAPIList(t *testing.T) {
	// An operator who writes api: [] must not get port 4646 opened to everyone; Validate rejects the empty list.
	tests := []struct {
		name      string
		api, want []string
	}{
		{"left out", nil, []string{"0.0.0.0/0"}},
		{"empty", []string{}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cluster()
			c.Spec.Access.API = tt.api
			SetDefaults(c, groups())
			if diff := cmp.Diff(tt.want, c.Spec.Access.API); diff != "" {
				t.Errorf("spec.access.api (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetDefaultsLeavesFieldsWithoutADefault(t *testing.T) {
	// Hetzner zones must be given, empty ssh closes SSH, and the Nomad version comes from the channel later.
	o := hetzner()
	spec := &o.Cluster.Spec
	spec.Cloud.Zones = nil
	spec.Access.SSH = nil
	spec.Nomad.Version = ""
	SetDefaults(o.Cluster, o.NodeGroups)
	if spec.Cloud.Zones != nil || spec.Access.SSH != nil || spec.Nomad.Version != "" {
		t.Errorf("zones %q, ssh %q, nomad.version %q, want all left empty",
			spec.Cloud.Zones, spec.Access.SSH, spec.Nomad.Version)
	}
}

func TestSetDefaultsGroupZonesAreCopies(t *testing.T) {
	o := fixtures()
	SetDefaults(o.Cluster, o.NodeGroups)
	for _, g := range o.NodeGroups {
		if len(g.Spec.Zones) == 0 {
			t.Fatalf("group %s has no zones after SetDefaults", g.Metadata.Name)
		}
		g.Spec.Zones[0] = g.Metadata.Name
	}
	if diff := cmp.Diff([]string{"ams"}, o.Cluster.Spec.Cloud.Zones); diff != "" {
		t.Errorf("changing a group's zones changed the cluster's (-want +got):\n%s", diff)
	}
	for _, g := range o.NodeGroups {
		if got := g.Spec.Zones[0]; got != g.Metadata.Name {
			t.Errorf("group %s zones[0] = %q, want %q: another group shares the slice", g.Metadata.Name, got,
				g.Metadata.Name)
		}
	}
}

func TestSetDefaultsToleratesNil(t *testing.T) {
	SetDefaults(nil, nil)

	gs := groups()
	SetDefaults(nil, gs)
	if diff := cmp.Diff(groups(), gs); diff != "" {
		t.Errorf("SetDefaults without a cluster changed the groups (-want +got):\n%s", diff)
	}

	workers := groups()[1]
	SetDefaults(cluster(), []*NodeGroup{nil, workers})
	if got := workers.Spec.Image; got != "ubuntu-24.04" {
		t.Errorf("image of the group after a nil entry = %q, want ubuntu-24.04", got)
	}
}

// allSet returns the Hetzner fixtures with every defaulted field set to something other than its default. Hetzner
// has several zones, so group zones can differ from the default.
func allSet() objects {
	o := hetzner()
	spec := &o.Cluster.Spec
	spec.Channel = "beta"
	spec.Networking.CIDR = "10.50.0.0/16"
	spec.Access.API = []string{"198.51.100.0/24"}
	spec.Nomad.Region = "eu"
	spec.Nomad.TLS.VerifyHTTPSClient = new(false)
	spec.Nomad.ClientIntroduction = ClientIntroductionNone
	for _, g := range o.NodeGroups {
		g.Spec.Image = "ubuntu-26.04"
		g.Spec.Zones = []string{"fsn1", "nbg1"}
	}
	workers := o.NodeGroups[1]
	workers.Spec.Nomad.NodePool = "batch"
	return o
}
