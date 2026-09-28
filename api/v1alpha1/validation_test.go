package v1alpha1

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Details that several cases expect.
const (
	nameRule = "must be 2 to 20 lowercase letters, digits or dashes, starting with a letter and ending with " +
		"a letter or digit"
	regionRule     = "must match ^[a-z0-9][a-z0-9-]*$"
	cidrRule       = "must be a CIDR in address/bits form"
	privateRule    = "must be a private range inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16"
	whitespaceRule = "must not contain whitespace"
	serverNomad    = "must be empty for role=server: client settings do not apply"
	keyTypeRule    = "key type must be one of ssh-ed25519, ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, " +
		"ecdsa-sha2-nistp521, ssh-rsa, sk-ssh-ed25519@openssh.com, sk-ecdsa-sha2-nistp256@openssh.com"
)

// validateCase is a spec for Validate: a base, an edit before SetDefaults and an edit after it.
type validateCase struct {
	name      string
	base      func() objects   // fixtures when nil
	spec      func(o *objects) // what the operator wrote, before SetDefaults
	defaulted func(o *objects) // after SetDefaults, to clear a field it fills
	opts      ValidateOptions
	want      Errors // nil when the spec is valid
}

func (tc validateCase) build() objects {
	base := tc.base
	if base == nil {
		base = fixtures
	}
	o := base()
	if tc.spec != nil {
		tc.spec(&o)
	}
	SetDefaults(o.Cluster, o.NodeGroups)
	if tc.defaulted != nil {
		tc.defaulted(&o)
	}
	return o
}

func (tc validateCase) run(t *testing.T) {
	t.Helper()
	o := tc.build()
	err := Validate(o.Cluster, o.NodeGroups, tc.opts)
	if tc.want == nil {
		if err != nil {
			t.Errorf("Validate:\n%v\nwant nil", err)
		}
		return
	}
	var got Errors
	if !errors.As(err, &got) {
		t.Fatalf("Validate = %v, want Errors", err)
	}
	if diff := cmp.Diff(tc.want, got); diff != "" {
		t.Errorf("Validate (-want +got):\n%s", diff)
	}
}

// renamed returns an edit that renames the cluster and points its groups at the new name.
func renamed(name string) func(o *objects) {
	return func(o *objects) {
		o.Cluster.Metadata.Name = name
		for _, g := range o.NodeGroups {
			g.Metadata.Cluster = name
		}
	}
}

// fakeSSHKey returns a public key line of type typ whose key blob is made up but well formed.
func fakeSSHKey(typ string) string {
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = append(blob, "key"...)
	return typ + " " + base64.StdEncoding.EncodeToString(blob) + " test"
}

func TestValidateAccepts(t *testing.T) {
	tests := []validateCase{
		{name: "fixtures"},
		{name: "hetzner", base: hetzner},
		{
			name: "combined group of size 3 with clientIntroduction warn",
			base: combined,
			spec: func(o *objects) { o.Cluster.Spec.Nomad.ClientIntroduction = ClientIntroductionWarn },
		},
		{name: "server group of size 5", spec: func(o *objects) { o.NodeGroups[0].Spec.Size = 5 }},
		{
			name: "single server with --allow-single-server",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Size = 1 },
			opts: ValidateOptions{AllowSingleServer: true},
		},
		{
			name: "names of 20 and 2 characters",
			spec: func(o *objects) {
				renamed(strings.Repeat("a", 20))(o)
				o.NodeGroups[1].Metadata.Name = "ab"
			},
		},
		{
			name: "names starting with ones Windows reserves",
			spec: func(o *objects) {
				renamed("console")(o)
				o.NodeGroups[0].Metadata.Name = "com10"
				o.NodeGroups[1].Metadata.Name = "nul-1"
			},
		},
		{
			name: "names ending with ones Windows reserves",
			spec: func(o *objects) {
				renamed("falcon")(o)
				o.NodeGroups[0].Metadata.Name = "x-aux"
				o.NodeGroups[1].Metadata.Name = "x-lpt1"
			},
		},
		{
			name: "IPv6 sources",
			spec: func(o *objects) {
				o.Cluster.Spec.Access = Access{
					SSH: []string{"2001:db8::/32"},
					API: []string{"2001:db8::/32", "0.0.0.0/0"},
				}
			},
		},
		{
			name: "every supported SSH key type",
			spec: func(o *objects) {
				o.Cluster.Spec.SSHKeys = nil
				for _, typ := range []string{
					"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa",
					"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
				} {
					o.Cluster.Spec.SSHKeys = append(o.Cluster.Spec.SSHKeys, fakeSSHKey(typ))
				}
			},
		},
		{name: "no Nomad version", spec: func(o *objects) { o.Cluster.Spec.Nomad.Version = "" }},
		// The channel, which Validate does not know, sets the versions a cluster may run.
		{name: "any Nomad version", spec: func(o *objects) { o.Cluster.Spec.Nomad.Version = "1.10.3" }},
		{name: "empty client group", spec: func(o *objects) { o.NodeGroups[1].Spec.Size = 0 }},
		{
			// drivers: [] decodes as an empty, non-nil slice, and it is still empty.
			name: "server group with drivers: []",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Nomad.Drivers = []string{} },
		},
		{
			name: "nil node group entry",
			spec: func(o *objects) { o.NodeGroups = append([]*NodeGroup{nil}, o.NodeGroups...) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}

func TestValidateReportsFieldPaths(t *testing.T) {
	long := strings.Repeat("a", 21)
	tests := []validateCase{
		// Cluster metadata.
		{
			name: "cluster apiVersion",
			spec: func(o *objects) { o.Cluster.APIVersion = "tent/v1" },
			want: Errors{{"Cluster prod", "apiVersion", "must be tent/v1alpha1"}},
		},
		{
			name: "cluster kind",
			spec: func(o *objects) { o.Cluster.Kind = KindNodeGroup },
			want: Errors{{"Cluster prod", "kind", "must be Cluster"}},
		},
		{
			name: "cluster name too short",
			spec: renamed("p"),
			want: Errors{{"Cluster p", "metadata.name", nameRule}},
		},
		{
			name: "cluster name too long",
			spec: renamed(long),
			want: Errors{{"Cluster " + long, "metadata.name", nameRule}},
		},
		{
			name: "cluster name ends with a dash",
			spec: renamed("prod-"),
			want: Errors{{"Cluster prod-", "metadata.name", nameRule}},
		},
		{
			// The groups still name prod; without a cluster name there is nothing to compare them with.
			name: "cluster name missing",
			spec: func(o *objects) { o.Cluster.Metadata.Name = "" },
			want: Errors{{"Cluster (no name)", "metadata.name", "required"}},
		},
		{
			// The groups still name prod; the invalid name is not worth copying into them.
			name: "cluster name invalid",
			spec: func(o *objects) { o.Cluster.Metadata.Name = "Prod" },
			want: Errors{{"Cluster Prod", "metadata.name", nameRule}},
		},
		{
			name: "cluster name reserved by Windows",
			spec: renamed("con"),
			want: Errors{{"Cluster con", "metadata.name", "must not be con: Windows reserves that name"}},
		},
		{
			// The groups still name prod; the reserved name is not worth copying into them.
			name: "cluster name reserved by Windows, groups name another",
			spec: func(o *objects) { o.Cluster.Metadata.Name = "nul" },
			want: Errors{{"Cluster nul", "metadata.name", "must not be nul: Windows reserves that name"}},
		},
		// Cluster spec.
		{
			name:      "channel missing",
			defaulted: func(o *objects) { o.Cluster.Spec.Channel = "" },
			want:      Errors{{"Cluster prod", "spec.channel", "required"}},
		},
		{
			name: "channel pattern",
			spec: func(o *objects) { o.Cluster.Spec.Channel = "Beta" },
			want: Errors{{"Cluster prod", "spec.channel", "must match ^[a-z][a-z0-9-]*$"}},
		},
		{
			name: "unknown provider",
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Provider = "aws" },
			want: Errors{{"Cluster prod", "spec.cloud.provider", "must be one of vultr, hetzner"}},
		},
		{
			name: "hetzner block on vultr",
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Hetzner = &HetznerCloud{} },
			want: Errors{{"Cluster prod", "spec.cloud.hetzner", "must not be set when the provider is vultr"}},
		},
		{
			name: "vultr block on hetzner",
			base: hetzner,
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Vultr = &VultrCloud{} },
			want: Errors{{"Cluster prod", "spec.cloud.vultr", "must not be set when the provider is hetzner"}},
		},
		{
			// SetDefaults sets zones to [""]; the operator sees the missing region only.
			name: "region missing on vultr",
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Region = "" },
			want: Errors{{"Cluster prod", "spec.cloud.region", "required"}},
		},
		{
			name: "zones without a region on vultr",
			spec: func(o *objects) {
				o.Cluster.Spec.Cloud.Region = ""
				o.Cluster.Spec.Cloud.Zones = []string{"ams"}
			},
			want: Errors{{"Cluster prod", "spec.cloud.region", "required"}},
		},
		{
			name: "region pattern",
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Region = "AMS" },
			want: Errors{{"Cluster prod", "spec.cloud.region", regionRule}},
		},
		{
			name: "vultr zones other than the region",
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Zones = []string{"ams", "ewr"} },
			want: Errors{{"Cluster prod", "spec.cloud.zones", "must be [ams]: Vultr has no zones"}},
		},
		{
			name: "hetzner zones missing",
			base: hetzner,
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Zones = nil },
			want: Errors{{"Cluster prod", "spec.cloud.zones", "required"}},
		},
		{
			name: "hetzner zone pattern",
			base: hetzner,
			spec: func(o *objects) { o.Cluster.Spec.Cloud.Zones = []string{"fsn1", "NBG1"} },
			want: Errors{{"Cluster prod", "spec.cloud.zones[1]", regionRule}},
		},
		{
			// After SetDefaults, so the groups keep three distinct zones.
			name: "hetzner duplicate zone",
			base: hetzner,
			defaulted: func(o *objects) {
				o.Cluster.Spec.Cloud.Zones = append(o.Cluster.Spec.Cloud.Zones, "fsn1")
			},
			want: Errors{{"Cluster prod", "spec.cloud.zones[3]", `duplicate zone "fsn1"`}},
		},
		{
			name: "network not a CIDR",
			spec: func(o *objects) { o.Cluster.Spec.Networking.CIDR = "10.64.0.0" },
			want: Errors{{"Cluster prod", "spec.networking.cidr", cidrRule}},
		},
		{
			name: "network with host bits",
			spec: func(o *objects) { o.Cluster.Spec.Networking.CIDR = "10.64.1.0/16" },
			want: Errors{{"Cluster prod", "spec.networking.cidr",
				"must be a network address: 10.64.1.0 has bits set after /16"}},
		},
		{
			name: "network IPv6",
			spec: func(o *objects) { o.Cluster.Spec.Networking.CIDR = "fd00::/64" },
			want: Errors{{"Cluster prod", "spec.networking.cidr", "must be an IPv4 CIDR"}},
		},
		{
			name: "network public",
			spec: func(o *objects) { o.Cluster.Spec.Networking.CIDR = "198.51.100.0/24" },
			want: Errors{{"Cluster prod", "spec.networking.cidr", privateRule}},
		},
		{
			// 10.0.0.0 is private, but the prefix also covers the public 11.0.0.0/8.
			name: "network wider than a private range",
			spec: func(o *objects) { o.Cluster.Spec.Networking.CIDR = "10.0.0.0/7" },
			want: Errors{{"Cluster prod", "spec.networking.cidr", privateRule}},
		},
		{
			name: "ssh source not a CIDR",
			spec: func(o *objects) { o.Cluster.Spec.Access.SSH = []string{"203.0.113.7"} },
			want: Errors{{"Cluster prod", "spec.access.ssh[0]", cidrRule}},
		},
		{
			name: "ssh source with host bits",
			spec: func(o *objects) { o.Cluster.Spec.Access.SSH = []string{"203.0.113.7/24"} },
			want: Errors{{"Cluster prod", "spec.access.ssh[0]",
				"must be a network address: 203.0.113.7 has bits set after /24"}},
		},
		{
			name: "api source not a CIDR",
			spec: func(o *objects) { o.Cluster.Spec.Access.API = []string{"0.0.0.0/0", "office"} },
			want: Errors{{"Cluster prod", "spec.access.api[1]", cidrRule}},
		},
		{
			name: "api IPv6 source with host bits",
			spec: func(o *objects) { o.Cluster.Spec.Access.API = []string{"2001:db8::1/64"} },
			want: Errors{{"Cluster prod", "spec.access.api[0]",
				"must be a network address: 2001:db8::1 has bits set after /64"}},
		},
		{
			// api: [] survives SetDefaults, which fills only a left-out list.
			name: "api empty",
			spec: func(o *objects) { o.Cluster.Spec.Access.API = []string{} },
			want: Errors{{"Cluster prod", "spec.access.api",
				"must not be empty: tent needs the Nomad API; list the addresses that may reach it"}},
		},
		{
			name: "ssh key without data",
			spec: func(o *objects) { o.Cluster.Spec.SSHKeys = []string{"ssh-ed25519"} },
			want: Errors{{"Cluster prod", "spec.sshKeys[0]",
				"must be an OpenSSH public key: <type> <base64> [comment]"}},
		},
		{
			name: "ssh key type",
			spec: func(o *objects) { o.Cluster.Spec.SSHKeys = []string{fakeSSHKey("ssh-dss")} },
			want: Errors{{"Cluster prod", "spec.sshKeys[0]", keyTypeRule}},
		},
		{
			name: "ssh key data not base64",
			spec: func(o *objects) { o.Cluster.Spec.SSHKeys = []string{"ssh-ed25519 not-base64! ops@example"} },
			want: Errors{{"Cluster prod", "spec.sshKeys[0]", "key data is not valid base64"}},
		},
		{
			name: "ssh key data of another type",
			spec: func(o *objects) {
				o.Cluster.Spec.SSHKeys = []string{strings.Replace(testSSHKey, "ssh-ed25519", "ssh-rsa", 1)}
			},
			want: Errors{{"Cluster prod", "spec.sshKeys[0]", "key data does not match the type ssh-rsa"}},
		},
		{
			name: "ssh key data too short",
			spec: func(o *objects) { o.Cluster.Spec.SSHKeys = []string{"ssh-ed25519 AAAA"} },
			want: Errors{{"Cluster prod", "spec.sshKeys[0]", "key data does not match the type ssh-ed25519"}},
		},
		{
			// Two keys in one string read as one key with the second as its comment.
			name: "ssh key of two lines",
			spec: func(o *objects) {
				o.Cluster.Spec.SSHKeys = []string{testSSHKey + "\n" + fakeSSHKey("ssh-rsa"), testSSHKey + "\r"}
			},
			want: Errors{
				{"Cluster prod", "spec.sshKeys[0]", "must be one line"},
				{"Cluster prod", "spec.sshKeys[1]", "must be one line"},
			},
		},
		{
			// The same key with another comment is still the same key.
			name: "duplicate ssh key",
			spec: func(o *objects) {
				other := strings.TrimSuffix(testSSHKey, "ops@example") + "me"
				o.Cluster.Spec.SSHKeys = append(o.Cluster.Spec.SSHKeys, other)
			},
			want: Errors{{"Cluster prod", "spec.sshKeys[1]", "duplicate key"}},
		},
		{
			name:      "nomad region missing",
			defaulted: func(o *objects) { o.Cluster.Spec.Nomad.Region = "" },
			want:      Errors{{"Cluster prod", "spec.nomad.region", "required"}},
		},
		{
			name: "nomad region pattern",
			spec: func(o *objects) { o.Cluster.Spec.Nomad.Region = "Global" },
			want: Errors{{"Cluster prod", "spec.nomad.region", regionRule}},
		},
		{
			name: "client introduction",
			spec: func(o *objects) { o.Cluster.Spec.Nomad.ClientIntroduction = "loose" },
			want: Errors{{"Cluster prod", "spec.nomad.clientIntroduction", "must be one of strict, warn, none"}},
		},
		// Node group metadata.
		{
			name: "group apiVersion",
			spec: func(o *objects) { o.NodeGroups[1].APIVersion = "tent/v1" },
			want: Errors{{"NodeGroup workers", "apiVersion", "must be tent/v1alpha1"}},
		},
		{
			name: "group kind",
			spec: func(o *objects) { o.NodeGroups[1].Kind = KindCluster },
			want: Errors{{"NodeGroup workers", "kind", "must be NodeGroup"}},
		},
		{
			name: "group name pattern",
			spec: func(o *objects) { o.NodeGroups[1].Metadata.Name = "Workers" },
			want: Errors{{"NodeGroup Workers", "metadata.name", nameRule}},
		},
		{
			name: "group name missing",
			spec: func(o *objects) { o.NodeGroups[1].Metadata.Name = "" },
			want: Errors{{"NodeGroup (no name)", "metadata.name", "required"}},
		},
		{
			name: "group name reserved by Windows",
			spec: func(o *objects) { o.NodeGroups[1].Metadata.Name = "lpt9" },
			want: Errors{{"NodeGroup lpt9", "metadata.name", "must not be lpt9: Windows reserves that name"}},
		},
		{
			// A missing name is reported as required, not as shared.
			name: "two groups without a name",
			spec: func(o *objects) {
				for _, g := range o.NodeGroups {
					g.Metadata.Name = ""
				}
			},
			want: Errors{
				{"NodeGroup (no name)", "metadata.name", "required"},
				{"NodeGroup (no name)", "metadata.name", "required"},
			},
		},
		{
			name: "group of another cluster",
			spec: func(o *objects) { o.NodeGroups[1].Metadata.Cluster = "staging" },
			want: Errors{{"NodeGroup workers", "metadata.cluster", "must be prod, the cluster's name"}},
		},
		{
			name: "three groups share a name",
			spec: func(o *objects) {
				extra := groups()[1]
				extra.Metadata.Name = "servers"
				o.NodeGroups[1].Metadata.Name = "servers"
				o.NodeGroups = append(o.NodeGroups, extra)
			},
			want: Errors{
				{"NodeGroup servers", "metadata.name", "duplicate node group name: 3 groups are named servers"},
			},
		},
		// Node group spec.
		{
			name: "group role",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Role = "worker" },
			want: Errors{{"NodeGroup workers", "spec.role", "must be one of server, client, combined"}},
		},
		{
			name: "machine type missing",
			spec: func(o *objects) { o.NodeGroups[0].Spec.MachineType = "" },
			want: Errors{{"NodeGroup servers", "spec.machineType", "required"}},
		},
		{
			name: "machine type with whitespace",
			spec: func(o *objects) { o.NodeGroups[0].Spec.MachineType = "vc2 2c 4gb" },
			want: Errors{{"NodeGroup servers", "spec.machineType", whitespaceRule}},
		},
		{
			name:      "image missing",
			defaulted: func(o *objects) { o.NodeGroups[0].Spec.Image = "" },
			want:      Errors{{"NodeGroup servers", "spec.image", "required"}},
		},
		{
			name: "image with whitespace",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Image = "ubuntu 24.04" },
			want: Errors{{"NodeGroup servers", "spec.image", whitespaceRule}},
		},
		{
			name: "server group size",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Size = 2 },
			want: Errors{{"NodeGroup servers", "spec.size", "must be 1, 3 or 5 for role=server"}},
		},
		{
			name: "combined group size",
			base: combined,
			spec: func(o *objects) { o.NodeGroups[0].Spec.Size = 4 },
			want: Errors{{"NodeGroup servers", "spec.size", "must be 1, 3 or 5 for role=combined"}},
		},
		{
			name: "single server without --allow-single-server",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Size = 1 },
			want: Errors{{"NodeGroup servers", "spec.size", "size 1 needs --allow-single-server"}},
		},
		{
			name: "negative client size",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Size = -1 },
			want: Errors{{"NodeGroup workers", "spec.size", "must not be negative"}},
		},
		{
			name: "group zone outside the cluster",
			base: hetzner,
			spec: func(o *objects) { o.NodeGroups[0].Spec.Zones = []string{"fsn1", "fsn2"} },
			want: Errors{{"NodeGroup servers", "spec.zones[1]", `"fsn2" is not a cluster zone`}},
		},
		{
			name: "duplicate group zone",
			base: hetzner,
			spec: func(o *objects) { o.NodeGroups[1].Spec.Zones = []string{"nbg1", "nbg1"} },
			want: Errors{{"NodeGroup workers", "spec.zones[1]", `duplicate zone "nbg1"`}},
		},
		{
			name: "server group with a node class",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Nomad.NodeClass = "general" },
			want: Errors{{"NodeGroup servers", "spec.nomad", serverNomad}},
		},
		{
			name: "server group with a node pool",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Nomad.NodePool = "default" },
			want: Errors{{"NodeGroup servers", "spec.nomad", serverNomad}},
		},
		{
			name: "server group with meta",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Nomad.Meta = map[string]string{"team": "platform"} },
			want: Errors{{"NodeGroup servers", "spec.nomad", serverNomad}},
		},
		{
			name: "server group with drivers",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Nomad.Drivers = []string{"docker"} },
			want: Errors{{"NodeGroup servers", "spec.nomad", serverNomad}},
		},
		{
			name: "node pool too long",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Nomad.NodePool = strings.Repeat("a", 129) },
			want: Errors{{"NodeGroup workers", "spec.nomad.nodePool", "must match ^[A-Za-z0-9_-]{1,128}$"}},
		},
		{
			name: "node class pattern",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Nomad.NodeClass = "general purpose" },
			want: Errors{{"NodeGroup workers", "spec.nomad.nodeClass", "must match ^[A-Za-z0-9._-]+$"}},
		},
		{
			name: "driver pattern",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Nomad.Drivers = []string{"docker", "Exec"} },
			want: Errors{{"NodeGroup workers", "spec.nomad.drivers[1]", "must match ^[a-z0-9_-]+$"}},
		},
		{
			name: "duplicate driver",
			spec: func(o *objects) { o.NodeGroups[1].Spec.Nomad.Drivers = []string{"docker", "exec", "docker"} },
			want: Errors{{"NodeGroup workers", "spec.nomad.drivers[2]", `duplicate driver "docker"`}},
		},
		{
			name: "meta keys, in sorted order",
			spec: func(o *objects) {
				o.NodeGroups[1].Spec.Nomad.Meta = map[string]string{"team": "a", "zz top": "b", "cost center": "c"}
			},
			want: Errors{
				{"NodeGroup workers", `spec.nomad.meta["cost center"]`, "key must match ^[A-Za-z0-9_.-]+$"},
				{"NodeGroup workers", `spec.nomad.meta["zz top"]`, "key must match ^[A-Za-z0-9_.-]+$"},
			},
		},
		// Across objects.
		{
			name: "no server group",
			spec: func(o *objects) { o.NodeGroups[0].Spec.Role = RoleClient },
			want: Errors{{"Cluster prod", "nodeGroups", "need exactly one server or combined group, found 0"}},
		},
		{
			name: "two server groups",
			spec: func(o *objects) {
				all := groups()[0]
				all.Metadata.Name = "all"
				o.NodeGroups = append(o.NodeGroups, all)
			},
			want: Errors{{"Cluster prod", "nodeGroups",
				"need exactly one server or combined group, found 2: all, servers"}},
		},
		{
			name: "strict client introduction with a combined group",
			base: combined,
			spec: func(o *objects) { o.Cluster.Spec.Nomad.ClientIntroduction = ClientIntroductionStrict },
			want: Errors{{"Cluster prod", "spec.nomad.clientIntroduction",
				"must not be strict: a combined group registers its client before intro tokens exist"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}

func TestValidateRejectsWindowsNames(t *testing.T) {
	names := []string{"con", "prn", "aux", "nul"}
	for d := '1'; d <= '9'; d++ {
		names = append(names, "com"+string(d), "lpt"+string(d))
	}
	for _, name := range names {
		reserved := "must not be " + name + ": Windows reserves that name"
		for _, tc := range []validateCase{
			{
				name: "cluster " + name,
				spec: renamed(name),
				want: Errors{{"Cluster " + name, "metadata.name", reserved}},
			},
			{
				name: "group " + name,
				spec: func(o *objects) { o.NodeGroups[1].Metadata.Name = name },
				want: Errors{{"NodeGroup " + name, "metadata.name", reserved}},
			},
		} {
			t.Run(tc.name, tc.run)
		}
	}
}

func TestValidateCollectsEveryProblem(t *testing.T) {
	// Groups come in reverse order; problems still come cluster first, then groups by name, then cross-object checks.
	validateCase{
		spec: func(o *objects) {
			servers, workers := o.NodeGroups[0], o.NodeGroups[1]
			o.NodeGroups = []*NodeGroup{workers, servers}
			workers.Spec.Size = -1
			servers.Spec.Role = RoleClient
			servers.Spec.MachineType = ""
			o.Cluster.Spec.Channel = "Beta"
		},
		want: Errors{
			{"Cluster prod", "spec.channel", "must match ^[a-z][a-z0-9-]*$"},
			{"NodeGroup servers", "spec.machineType", "required"},
			{"NodeGroup workers", "spec.size", "must not be negative"},
			{"Cluster prod", "nodeGroups", "need exactly one server or combined group, found 0"},
		},
	}.run(t)
}

func TestErrorsFormat(t *testing.T) {
	o := validateCase{spec: func(o *objects) {
		o.Cluster.Spec.Channel = "Beta"
		o.NodeGroups[1].Spec.Size = -1
	}}.build()
	err := fmt.Errorf("apply: %w", Validate(o.Cluster, o.NodeGroups, ValidateOptions{}))

	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("errors.As found no Errors in %v", err)
	}
	want := "Cluster prod: spec.channel: must match ^[a-z][a-z0-9-]*$\n" +
		"NodeGroup workers: spec.size: must not be negative"
	if got := errs.Error(); got != want {
		t.Errorf("Errors.Error() =\n%s\nwant\n%s", got, want)
	}
}

func TestValidateRejectsNilCluster(t *testing.T) {
	err := Validate(nil, groups(), ValidateOptions{})
	if err == nil {
		t.Fatal("Validate(nil, ...) = nil, want an error")
	}
	// A missing cluster is not a problem with a field, so it stays a plain error.
	var errs Errors
	if errors.As(err, &errs) {
		t.Errorf("Validate(nil, ...) returned Errors %v, want a plain error", errs)
	}
}

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		want       string // "" when the name is valid
	}{
		{KindCluster, "prod", ""},
		{KindNodeGroup, "web-2", ""},
		{KindCluster, "PROD", `invalid cluster name "PROD": ` + nameRule},
		{KindNodeGroup, "Web", `invalid node group name "Web": ` + nameRule},
		{KindCluster, "", `invalid cluster name "": ` + nameRule},
		{KindCluster, "../prod", `invalid cluster name "../prod": ` + nameRule},
		{KindNodeGroup, "com1", `invalid node group name "com1": must not be com1: Windows reserves that name`},
	} {
		err := ValidateName(tc.kind, tc.name)
		if got := fmt.Sprint(err); (tc.want == "" && err != nil) || (tc.want != "" && got != tc.want) {
			t.Errorf("ValidateName(%s, %q) = %v, want %q", tc.kind, tc.name, err, tc.want)
		}
	}
}

func TestValidateNames(t *testing.T) {
	alone := func(edit func(o *objects)) func(o *objects) {
		return func(o *objects) {
			o.Cluster = nil
			edit(o)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(o *objects)
		want Errors // nil when the names are valid
	}{
		{"valid", func(*objects) {}, nil},
		{"node groups alone", alone(func(*objects) {}), nil},
		{"other fields", func(o *objects) {
			o.Cluster.Spec.Channel = "Beta"
			o.NodeGroups[0].Spec.Size = 2
		}, nil},
		{"cluster name", renamed("PROD"), Errors{{"Cluster PROD", "metadata.name", nameRule}}},
		{"no cluster name", renamed(""), Errors{{"Cluster (no name)", "metadata.name", "required"}}},
		{"group name", func(o *objects) { o.NodeGroups[1].Metadata.Name = "../y" },
			Errors{{"NodeGroup ../y", "metadata.name", nameRule}}},
		{"group of another cluster", func(o *objects) { o.NodeGroups[1].Metadata.Cluster = "../x" },
			Errors{{"NodeGroup workers", "metadata.cluster", "must be prod, the cluster's name"}}},
		{"node groups alone of an invalid cluster", alone(func(o *objects) { o.NodeGroups[1].Metadata.Cluster = "../x" }),
			Errors{{"NodeGroup workers", "metadata.cluster", nameRule}}},
		{"node groups alone without a cluster", alone(func(o *objects) { o.NodeGroups[0].Metadata.Cluster = "" }),
			Errors{{"NodeGroup servers", "metadata.cluster", "required"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fixtures()
			tc.edit(&o)
			if diff := cmp.Diff(tc.want, ValidateNames(o.Cluster, o.NodeGroups)); diff != "" {
				t.Errorf("ValidateNames (-want +got):\n%s", diff)
			}
		})
	}
}
