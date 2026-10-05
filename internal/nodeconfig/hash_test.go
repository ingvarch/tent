package nodeconfig_test

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// hashPattern is the form of every spec hash: 16 lower-case hex digits, which the label codecs take.
var hashPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// hashed returns the sample with the files that a real group has besides the sample's: the CA bundle and the
// operator's extra client configuration, both group-level files.
func hashed() *nodeconfig.NodeConfig {
	c := sample()
	c.Files = append(c.Files,
		nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: "root:root",
			Content: []byte("-----BEGIN CERTIFICATE-----\nMIIBcanary\n-----END CERTIFICATE-----\n")},
		nodeconfig.File{Path: "/etc/nomad.d/99-user-client.hcl", Mode: 0o600, Owner: "root:root",
			Content: []byte("client {\n  max_kill_timeout = \"1m\"\n}\n")},
	)
	return c
}

// file returns the file of c at path, and fails the test when there is none.
func file(t *testing.T, c *nodeconfig.NodeConfig, path string) *nodeconfig.File {
	t.Helper()
	i := slices.IndexFunc(c.Files, func(f nodeconfig.File) bool { return f.Path == path })
	if i < 0 {
		t.Fatalf("the config has no file %s", path)
	}
	return &c.Files[i]
}

func TestSpecHashForm(t *testing.T) {
	if got := nodeconfig.SpecHash(hashed()); !hashPattern.MatchString(got) {
		t.Errorf("SpecHash() = %q, want 16 lower-case hex digits", got)
	}
	if got := nodeconfig.SpecHash(nil); got != "" {
		t.Errorf("SpecHash(nil) = %q, want none", got)
	}
}

// TestSpecHashCanary pins the hash of a fixed config. It changes only with the canonical form that SpecHash hashes,
// and every such change marks every node of every cluster out of date.
func TestSpecHashCanary(t *testing.T) {
	cidr := []netip.Prefix{netip.MustParsePrefix("10.64.0.0/16")}
	c := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind,
		Cluster: "canary", NodeGroup: "core", Name: "canary-core-0", Role: v1alpha1.RoleCombined,
		Assets: []nodeconfig.Asset{
			{Name: "tent-node", Version: "v0.3.0", SHA256: strings.Repeat("cd", 32),
				URLs: []string{"https://github.com/ingvarch/tent/releases/download/v0.3.0/tent-node_linux_amd64"}},
			{Name: "nomad", Version: "2.0.7", SHA256: strings.Repeat("ab", 32),
				URLs: []string{"https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip"}},
		},
		Files: []nodeconfig.File{
			{Path: "/etc/nomad.d/00-tent.hcl", Mode: 0o644, Owner: "root:root", Content: []byte("region = \"global\"\n")},
			{Path: nodeconfig.CAFile, Mode: 0o644, Owner: "root:root",
				Content: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")},
			{Path: "/etc/nomad.d/01-gossip.hcl", Mode: 0o600, Owner: "root:root", Secret: true,
				Content: []byte("server {\n  encrypt = \"key\"\n}\n")},
			{Path: "/etc/nomad.d/10-node.hcl", Mode: 0o644, Owner: "root:root", PerNode: true,
				Content: []byte("name = \"canary-core-0\"\n")},
		},
		Join: nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
		System: nodeconfig.System{
			Sysctls: map[string]string{"net.ipv4.ip_forward": "1"}, KernelModules: []string{"br_netfilter"}, Docker: true,
		},
		// The rules and the sources of icmp are out of order: the canonical form sorts them.
		Firewall: nodeconfig.HostFirewall{
			Rules: []nodeconfig.Rule{
				{Name: "nomad-rpc", Protocol: nodeconfig.ProtocolTCP,
					Ports: nodeconfig.PortRange{First: 4647, Last: 4647}, From: cidr},
				{Name: "icmp", Protocol: nodeconfig.ProtocolICMP,
					From: []netip.Prefix{netip.MustParsePrefix("::/0"), netip.MustParsePrefix("0.0.0.0/0")}},
				{Name: "dynamic", Protocol: nodeconfig.ProtocolUDP,
					Ports: nodeconfig.PortRange{First: 20000, Last: 32000}, From: cidr},
			},
			BlockMetadata: netip.MustParseAddr("169.254.169.254"),
		},
	}
	// The sha256 of the canonical JSON built by hand, with the rules sorted by name and the sources by address:
	// {"format":1,"files":[00-tent.hcl, tls/ca.pem],"assets":[nomad, tent-node],"system":{…},"firewall":{"rules":
	// [dynamic, icmp from ["0.0.0.0/0","::/0"], nomad-rpc],"blockMetadata":"169.254.169.254"}}.
	if got, want := nodeconfig.SpecHash(c), "50cf9d039b765cbd"; got != want {
		t.Errorf("SpecHash(canary) = %s, want %s", got, want)
	}
}

// TestSpecHashKeeps checks what never marks a node out of date: where the assets come from, the order of the files
// and the assets, what is only this node's, and the secrets.
func TestSpecHashKeeps(t *testing.T) {
	type nc = nodeconfig.NodeConfig
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, c *nc)
	}{
		{"mirror URLs", func(_ *testing.T, c *nc) { c.Assets[0].URLs = []string{"https://other.example.com/nomad.zip"} }},
		{"mirror order", func(_ *testing.T, c *nc) { slices.Reverse(c.Assets[0].URLs) }},
		{"another mirror", func(_ *testing.T, c *nc) {
			c.Assets[2].URLs = append(c.Assets[2].URLs, "https://mirror.example.com/tent-node")
		}},
		{"file order", func(_ *testing.T, c *nc) { slices.Reverse(c.Files) }},
		{"asset order", func(_ *testing.T, c *nc) { slices.Reverse(c.Assets) }},
		{"a per-node file", func(t *testing.T, c *nc) {
			file(t, c, "/etc/nomad.d/10-node.hcl").Content = []byte("name = \"prod-core-1\"\n")
		}},
		{"a per-node secret", func(t *testing.T, c *nc) {
			file(t, c, nodeconfig.KeyFile).Content = []byte("another key\n")
		}},
		{"a per-node file added", func(_ *testing.T, c *nc) {
			c.Files = append(c.Files, nodeconfig.File{Path: nodeconfig.IntroTokenFile, Mode: 0o600,
				Owner: "root:root", Content: []byte("intro-token"), PerNode: true, Secret: true})
		}},
		{"the gossip key", func(t *testing.T, c *nc) {
			file(t, c, "/etc/nomad.d/01-gossip.hcl").Content = []byte("server {\n  encrypt = \"other\"\n}\n")
		}},
		{"join servers", func(_ *testing.T, c *nc) { c.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.7")} }},
		{"join refresh interval", func(_ *testing.T, c *nc) { c.Join.RefreshInterval = time.Hour }},
		{"name", func(_ *testing.T, c *nc) { c.Name = "prod-core-4" }},
		// A cluster never changes its provider, so the provider need not mark nodes out of date.
		{"provider", func(_ *testing.T, c *nc) { c.Provider = v1alpha1.ProviderHetzner }},
		// The region is in the hash through 00-tent.hcl already.
		{"region", func(_ *testing.T, c *nc) { c.Region = "europe" }},
		{"stored spec hash", func(_ *testing.T, c *nc) { c.SpecHash = "0123456789abcdef" }},
		{"rule order", func(_ *testing.T, c *nc) { slices.Reverse(c.Firewall.Rules) }},
		{"source order", func(t *testing.T, c *nc) {
			if len(c.Firewall.Rules[0].From) < 2 {
				t.Fatal("the first rule has fewer than two sources to reorder")
			}
			slices.Reverse(c.Firewall.Rules[0].From)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hashed()
			want := nodeconfig.SpecHash(c)
			tc.edit(t, c)
			if got := nodeconfig.SpecHash(c); got != want {
				t.Errorf("SpecHash() = %s after the edit, want %s as before", got, want)
			}
		})
	}
}

// TestSpecHashLeavesTheConfig checks that SpecHash sorts copies: the rules and their sources keep their order.
func TestSpecHashLeavesTheConfig(t *testing.T) {
	c := hashed()
	slices.Reverse(c.Firewall.Rules)
	slices.Reverse(c.Firewall.Rules[len(c.Firewall.Rules)-1].From)
	before := fmt.Sprint(c.Firewall)
	nodeconfig.SpecHash(c)
	if after := fmt.Sprint(c.Firewall); after != before {
		t.Errorf("SpecHash changed the firewall from\n%s\nto\n%s", before, after)
	}
}

// TestSpecHashEmptyAsLeftOut checks that empty lists and maps hash as left-out ones, as Encode writes them alike.
func TestSpecHashEmptyAsLeftOut(t *testing.T) {
	left, empty := hashed(), hashed()
	left.System.Sysctls, left.System.KernelModules, left.Firewall.Rules = nil, nil, nil
	empty.System.Sysctls, empty.System.KernelModules = map[string]string{}, []string{}
	empty.Firewall.Rules = []nodeconfig.Rule{}
	if a, b := nodeconfig.SpecHash(left), nodeconfig.SpecHash(empty); a != b {
		t.Errorf("SpecHash() = %s with left-out lists and %s with empty ones, want one hash", a, b)
	}
}

// TestSpecHashOrderOfDuplicates checks that the order of the files and assets leaves the hash as it is even in a config
// that Validate refuses, with two files at one path and two assets of one name.
func TestSpecHashOrderOfDuplicates(t *testing.T) {
	c := hashed()
	c.Files = append(c.Files, nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: "root:root",
		Content: []byte("another CA\n")})
	c.Assets = append(c.Assets, nodeconfig.Asset{Name: "nomad", Version: "2.0.8",
		URLs:   []string{"https://releases.hashicorp.com/nomad/2.0.8/nomad_2.0.8_linux_amd64.zip"},
		SHA256: strings.Repeat("2", 64)})
	want := nodeconfig.SpecHash(c)
	slices.Reverse(c.Files)
	slices.Reverse(c.Assets)
	if got := nodeconfig.SpecHash(c); got != want {
		t.Errorf("SpecHash() = %s with the files and assets reversed, want %s as before", got, want)
	}
}

// TestSpecHashDeterministic checks that one config always hashes alike, whatever the order Go gives a map's keys in.
func TestSpecHashDeterministic(t *testing.T) {
	c := hashed()
	for i := range 20 {
		c.System.Sysctls[strings.Repeat("k", i+1)+".x"] = "1"
	}
	want := nodeconfig.SpecHash(c)
	for range 20 {
		if got := nodeconfig.SpecHash(c); got != want {
			t.Fatalf("SpecHash() = %s, then %s for the same config", want, got)
		}
	}
}

// TestSpecHashChanges checks that each input of the hash changes it: the group-level files that are not secret, the
// CA bundle among them, the assets' names, versions and sha256s, the system settings and the host firewall.
func TestSpecHashChanges(t *testing.T) {
	type nc = nodeconfig.NodeConfig
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, c *nc)
	}{
		{"00-tent.hcl", func(t *testing.T, c *nc) {
			file(t, c, "/etc/nomad.d/00-tent.hcl").Content = []byte("region = \"europe\"\n")
		}},
		// File.String and the JSON of a File show only the path and the size.
		{"00-tent.hcl of the same length", func(t *testing.T, c *nc) {
			f := file(t, c, "/etc/nomad.d/00-tent.hcl")
			f.Content = []byte(strings.Replace(string(f.Content), "global", "globe!", 1))
		}},
		{"99-user-client.hcl", func(t *testing.T, c *nc) {
			file(t, c, "/etc/nomad.d/99-user-client.hcl").Content = []byte("client {}\n")
		}},
		{"the CA bundle", func(t *testing.T, c *nc) {
			file(t, c, nodeconfig.CAFile).Content =
				[]byte("-----BEGIN CERTIFICATE-----\nMIIBnext\n-----END CERTIFICATE-----\n")
		}},
		{"a group-level file added", func(_ *testing.T, c *nc) {
			c.Files = append(c.Files, nodeconfig.File{Path: "/etc/nomad.d/98-user-server.hcl", Mode: 0o600,
				Owner: "root:root", Content: []byte("server {}\n")})
		}},
		{"a group-level file removed", func(_ *testing.T, c *nc) {
			c.Files = slices.DeleteFunc(c.Files, func(f nodeconfig.File) bool { return f.Path == nodeconfig.CAFile })
		}},
		{"a group-level file made per-node", func(t *testing.T, c *nc) { file(t, c, nodeconfig.CAFile).PerNode = true }},
		{"a file's path", func(t *testing.T, c *nc) {
			file(t, c, nodeconfig.CAFile).Path = "/etc/nomad.d/tls/ca-bundle.pem"
		}},
		{"a file's mode", func(t *testing.T, c *nc) { file(t, c, nodeconfig.CAFile).Mode = 0o640 }},
		{"a file's owner", func(t *testing.T, c *nc) { file(t, c, nodeconfig.CAFile).Owner = "root:nomad" }},
		{"an asset's version", func(_ *testing.T, c *nc) { c.Assets[0].Version = "2.0.8" }},
		{"an asset's sha256", func(_ *testing.T, c *nc) { c.Assets[1].SHA256 = strings.Repeat("0", 64) }},
		{"the tent-node version", func(_ *testing.T, c *nc) { c.Assets[2].Version = "v0.3.1" }},
		{"an asset's name", func(_ *testing.T, c *nc) { c.Assets[1].Name = "cni" }},
		{"an asset added", func(_ *testing.T, c *nc) {
			c.Assets = append(c.Assets, nodeconfig.Asset{Name: "docker", Version: "29.0.0",
				URLs: []string{"https://download.example.com/docker.tgz"}, SHA256: strings.Repeat("1", 64)})
		}},
		{"an asset removed", func(_ *testing.T, c *nc) { c.Assets = c.Assets[1:] }},
		{"a sysctl's value", func(_ *testing.T, c *nc) { c.System.Sysctls["vm.max_map_count"] = "524288" }},
		{"a sysctl added", func(_ *testing.T, c *nc) { c.System.Sysctls["net.core.somaxconn"] = "4096" }},
		{"a kernel module added", func(_ *testing.T, c *nc) {
			c.System.KernelModules = append(c.System.KernelModules, "nf_conntrack")
		}},
		{"docker", func(_ *testing.T, c *nc) { c.System.Docker = false }},
		{"a firewall rule's ports", func(_ *testing.T, c *nc) { c.Firewall.Rules[2].Ports.Last = 32001 }},
		{"a firewall rule's sources", func(_ *testing.T, c *nc) {
			c.Firewall.Rules[2].From = []netip.Prefix{netip.MustParsePrefix("10.65.0.0/16")}
		}},
		{"a firewall rule's protocol", func(_ *testing.T, c *nc) {
			c.Firewall.Rules[2].Protocol = nodeconfig.ProtocolTCP
		}},
		{"a firewall rule added", func(_ *testing.T, c *nc) {
			c.Firewall.Rules = append(c.Firewall.Rules, nodeconfig.Rule{Name: "nomad-http",
				Protocol: nodeconfig.ProtocolTCP, Ports: nodeconfig.PortRange{First: 4646, Last: 4646},
				From: []netip.Prefix{netip.MustParsePrefix("10.64.0.0/16")}})
		}},
		{"the metadata address", func(_ *testing.T, c *nc) {
			c.Firewall.BlockMetadata = netip.MustParseAddr("169.254.169.253")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hashed()
			before := nodeconfig.SpecHash(c)
			tc.edit(t, c)
			if got := nodeconfig.SpecHash(c); got == before {
				t.Errorf("SpecHash() = %s after the edit, as before", got)
			}
		})
	}
}

// group returns the config of the node name of a group of role, as the app makes it: the group's agent files and CA
// bundle, and the node's own 10-node.hcl, key and seeds.
func group(
	t *testing.T, role v1alpha1.Role, name, datacenter string, key []byte, servers ...netip.Addr,
) *nodeconfig.NodeConfig {
	t.Helper()
	c := hashed()
	c.Name, c.Role, c.Join.Servers = name, role, servers
	c.Files = render(t, role)
	node, err := nodeconfig.RenderNode(name, datacenter, role, 3)
	if err != nil {
		t.Fatalf("RenderNode: %v", err)
	}
	c.Files = append(c.Files, node,
		nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: "root:root", Content: []byte("ca\n")},
		nodeconfig.File{Path: nodeconfig.KeyFile, Mode: 0o600, Owner: "root:root", Content: key,
			PerNode: true, Secret: true})
	return c
}

// TestSpecHashAlikeInGroup checks that two nodes of one group hash alike: their names, datacenters, keys and seeds
// differ.
func TestSpecHashAlikeInGroup(t *testing.T) {
	a := group(t, v1alpha1.RoleCombined, "prod-core-0", "ams", []byte("key a\n"))
	b := group(t, v1alpha1.RoleCombined, "prod-core-1", "fra", []byte("key b\n"), netip.MustParseAddr("10.64.0.5"))
	if ha, hb := nodeconfig.SpecHash(a), nodeconfig.SpecHash(b); ha != hb {
		t.Errorf("two nodes of one group hash to %s and %s, want one hash", ha, hb)
	}
}

// TestSpecHashRoles checks that the groups of different roles hash apart: their agent files differ.
func TestSpecHashRoles(t *testing.T) {
	seen := map[string]v1alpha1.Role{}
	for _, role := range v1alpha1.Roles() {
		h := nodeconfig.SpecHash(group(t, role, "prod-a-0", "ams", nodeKey))
		if other, ok := seen[h]; ok {
			t.Errorf("the %s and %s groups both hash to %s", other, role, h)
		}
		seen[h] = role
	}
}

// TestSpecHashRoundTrip checks that a config hashes alike before Encode and after Decode.
func TestSpecHashRoundTrip(t *testing.T) {
	c := hashed()
	data, err := nodeconfig.Encode(c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := nodeconfig.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if a, b := nodeconfig.SpecHash(c), nodeconfig.SpecHash(got); a != b {
		t.Errorf("SpecHash() = %s before Encode and %s after Decode", a, b)
	}
}

// TestValidateSpecHash checks that Validate, and so Encode and Decode, take a config whose stored hash is its own or
// empty, and refuse any other.
func TestValidateSpecHash(t *testing.T) {
	c := hashed()
	own := nodeconfig.SpecHash(c)
	c.SpecHash = own
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with its own hash = %v, want no error", err)
	}
	file(t, c, "/etc/nomad.d/00-tent.hcl").Content = []byte("region = \"europe\"\n")
	want := `node config: spec hash "` + own + `" is not the hash of the configuration, ` + nodeconfig.SpecHash(c)
	if got := errText(c.Validate()); got != want {
		t.Errorf("Validate() after a change = %q, want %q", got, want)
	}
	if _, err := nodeconfig.Encode(c); errText(err) != want {
		t.Errorf("Encode() after a change = %v, want %q", err, want)
	}
	c.SpecHash = ""
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() without a hash = %v, want no error", err)
	}
}
