package nodeconfig_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/secrettest"
)

func TestValidate(t *testing.T) {
	type nc = nodeconfig.NodeConfig
	for _, tc := range []struct {
		name string
		edit func(c *nc)
		want string // the error's text; "" for none
	}{
		{"sample", func(*nc) {}, ""},
		{"server", func(c *nc) { c.Role = v1alpha1.RoleServer }, ""},
		{"client", func(c *nc) { c.Role = v1alpha1.RoleClient }, ""},
		{"no seeds", func(c *nc) { c.Join.Servers = nil }, ""},
		{"nothing optional", func(c *nc) {
			c.Assets, c.Files, c.Join.Servers, c.System, c.Firewall.Rules = nil, nil, nil, nodeconfig.System{}, nil
		}, ""},
		{"spec hash", func(c *nc) { c.SpecHash = nodeconfig.SpecHash(c) }, ""},

		{"api version", func(c *nc) { c.APIVersion = "tent/v1" },
			`node config: apiVersion "tent/v1" is not tent/v1alpha1`},
		{"kind", func(c *nc) { c.Kind = "Cluster" }, `node config: kind "Cluster" is not NodeConfig`},
		{"cluster", func(c *nc) { c.Cluster = "Prod" }, `node config: invalid cluster name "Prod": must be 2 to 20 ` +
			`lowercase letters, digits or dashes, starting with a letter and ending with a letter or digit`},
		{"hetzner", func(c *nc) { c.Provider = v1alpha1.ProviderHetzner }, ""},
		{"no provider", func(c *nc) { c.Provider = "" }, `node config: provider "" is not one of vultr, hetzner`},
		{"unknown provider", func(c *nc) { c.Provider = "aws" },
			`node config: provider "aws" is not one of vultr, hetzner`},
		{"provider in upper case", func(c *nc) { c.Provider = "Vultr" },
			`node config: provider "Vultr" is not one of vultr, hetzner`},
		{"node group", func(c *nc) { c.NodeGroup = "" }, `node config: invalid node group name "": must be 2 to 20 ` +
			`lowercase letters, digits or dashes, starting with a letter and ending with a letter or digit`},
		{"name", func(c *nc) { c.Name = "prod_core_0" }, `node config: name "prod_core_0" is not a host name: ` +
			`1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or digit`},
		{"no name", func(c *nc) { c.Name = "" }, `node config: name "" is not a host name: ` +
			`1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or digit`},
		{"role", func(c *nc) { c.Role = "worker" }, `node config: role "worker" is not server, client or combined`},
		{"no region", func(c *nc) { c.Region = "" }, "node config: no region"},
		{"region", func(c *nc) { c.Region = "Global" },
			`node config: region "Global" is not lower-case letters, digits and dashes, ` +
				"starting with a letter or digit"},

		{"asset without a name", func(c *nc) { c.Assets[1].Name = "" }, "node config: assets[1]: no name"},
		// The name is the name of the asset's file in tent-node's cache.
		{"asset name with a slash", func(c *nc) { c.Assets[1].Name = "cni/plugins" },
			`node config: assets[1]: name "cni/plugins" is not 1 to 63 lower-case letters, digits and dashes, ` +
				"starting and ending with a letter or digit"},
		{"asset name in upper case", func(c *nc) { c.Assets[0].Name = "Nomad" },
			`node config: assets[0]: name "Nomad" is not 1 to 63 lower-case letters, digits and dashes, ` +
				"starting and ending with a letter or digit"},
		{"asset without a version", func(c *nc) { c.Assets[0].Version = "" }, "node config: asset nomad: no version"},
		{"asset without URLs", func(c *nc) { c.Assets[0].URLs = nil }, "node config: asset nomad: no URLs"},
		{"asset URL not http", func(c *nc) { c.Assets[0].URLs[1] = "ftp://mirror.example.com/nomad.zip" },
			"node config: asset nomad: URLs[1] is not an absolute http or https URL"},
		{"asset URL relative", func(c *nc) { c.Assets[0].URLs[0] = "/nomad.zip" },
			"node config: asset nomad: URLs[0] is not an absolute http or https URL"},
		{"asset URL without a host", func(c *nc) { c.Assets[0].URLs[0] = "https:///nomad.zip" },
			"node config: asset nomad: URLs[0] is not an absolute http or https URL"},
		{"asset URL does not parse", func(c *nc) { c.Assets[0].URLs[0] = "https://mirror.example.com/%zz" },
			"node config: asset nomad: URLs[0] is not an absolute http or https URL"},
		{"asset sha256 in upper case", func(c *nc) { c.Assets[1].SHA256 = "B98F74A0" },
			`node config: asset cni-plugins: sha256 "B98F74A0" is not 64 lower-case hex digits`},
		{"two assets with one name", func(c *nc) { c.Assets[1].Name = "nomad" },
			"node config: two assets are named nomad"},

		{"relative path", func(c *nc) { c.Files[0].Path = "etc/nomad.d/00-tent.hcl" },
			`node config: files[0]: path "etc/nomad.d/00-tent.hcl" is not absolute and clean`},
		{"unclean path", func(c *nc) { c.Files[0].Path = "/etc/nomad.d/../00-tent.hcl" },
			`node config: files[0]: path "/etc/nomad.d/../00-tent.hcl" is not absolute and clean`},
		{"root path", func(c *nc) { c.Files[0].Path = "/" }, `node config: files[0]: path "/" is not absolute and clean`},
		{"no mode", func(c *nc) { c.Files[1].Mode = 0 }, "node config: file /etc/nomad.d/01-gossip.hcl: no mode"},
		{"mode beyond the permissions", func(c *nc) { c.Files[1].Mode = 0o4755 },
			"node config: file /etc/nomad.d/01-gossip.hcl: mode 04755 has bits beyond the permissions 0777"},
		{"secret file only its owner reads", func(c *nc) { c.Files[1].Mode = 0o400 }, ""},
		{"secret file the group reads", func(c *nc) { c.Files[1].Mode = 0o640 },
			"node config: file /etc/nomad.d/01-gossip.hcl: mode 0640 gives other users access to a secret file"},
		{"secret file others read", func(c *nc) { c.Files[1].Mode = 0o604 },
			"node config: file /etc/nomad.d/01-gossip.hcl: mode 0604 gives other users access to a secret file"},
		{"secret file the group writes", func(c *nc) { c.Files[1].Mode = 0o620 },
			"node config: file /etc/nomad.d/01-gossip.hcl: mode 0620 gives other users access to a secret file"},
		{"owner without a group", func(c *nc) { c.Files[0].Owner = "root" },
			`node config: file /etc/nomad.d/00-tent.hcl: owner "root" is not user:group`},
		{"content not UTF-8", func(c *nc) { c.Files[1].Content = []byte{'k', 0xff, 'y'} },
			"node config: file /etc/nomad.d/01-gossip.hcl: content is not valid UTF-8"},
		{"two files with one path", func(c *nc) { c.Files[3].Path = c.Files[0].Path },
			"node config: two files have the path /etc/nomad.d/00-tent.hcl"},

		{"join strategy", func(c *nc) { c.Join.Strategy = "seed-refresh" },
			`node config: join strategy "seed-refresh" is not seed-and-refresh`},
		{"invalid seed", func(c *nc) { c.Join.Servers[1] = netip.Addr{} },
			"node config: join servers[1]: not an address"},
		{"repeated seed", func(c *nc) { c.Join.Servers[1] = c.Join.Servers[0] },
			"node config: join servers[1]: 10.64.0.5 is repeated"},
		{"no refresh interval", func(c *nc) { c.Join.RefreshInterval = 0 },
			"node config: join refresh interval 0s is less than 1s"},
		{"refresh interval below a second", func(c *nc) { c.Join.RefreshInterval = 999 * time.Millisecond },
			"node config: join refresh interval 999ms is less than 1s"},
		{"refresh interval of a second", func(c *nc) { c.Join.RefreshInterval = time.Second }, ""},

		{"sysctl key", func(c *nc) { c.System.Sysctls["net ipv4"] = "1" },
			`node config: sysctl "net ipv4" is not a sysctl key`},
		// A leading dash in sysctl.d makes errors on the line ignored.
		{"sysctl key with a leading dash", func(c *nc) { c.System.Sysctls["-net.ipv4.ip_forward"] = "1" },
			`node config: sysctl "-net.ipv4.ip_forward" is not a sysctl key`},
		{"sysctl key with a leading underscore", func(c *nc) { c.System.Sysctls["_net.ipv4.ip_forward"] = "1" },
			`node config: sysctl "_net.ipv4.ip_forward" is not a sysctl key`},
		{"empty sysctl value", func(c *nc) { c.System.Sysctls["net.ipv4.ip_forward"] = "" },
			"node config: sysctl net.ipv4.ip_forward: the value is empty"},
		{"sysctl value with a line end", func(c *nc) { c.System.Sysctls["vm.max_map_count"] = "1\nkernel.x = 1" },
			"node config: sysctl vm.max_map_count: the value has a control character or invalid UTF-8"},
		{"kernel module", func(c *nc) { c.System.KernelModules[1] = "overlay; reboot" },
			`node config: kernel module "overlay; reboot" is not a module name`},
		// modprobe would read it as an option.
		{"kernel module like an option", func(c *nc) { c.System.KernelModules[1] = "--dry-run" },
			`node config: kernel module "--dry-run" is not a module name`},
		{"kernel module starting with an underscore", func(c *nc) { c.System.KernelModules[1] = "_overlay" },
			`node config: kernel module "_overlay" is not a module name`},
		{"repeated kernel module", func(c *nc) { c.System.KernelModules[1] = "br_netfilter" },
			"node config: kernel module br_netfilter is repeated"},

		{"rule without a name", func(c *nc) { c.Firewall.Rules[2].Name = "" },
			"node config: firewall rules[2]: no name"},
		// tent-node puts a rule's name into the host firewall's ruleset, inside quotes.
		{"rule name with a quote", func(c *nc) { c.Firewall.Rules[0].Name = `ssh" accept` },
			`node config: firewall rules[0]: name "ssh\" accept" is not 1 to 63 lower-case letters, digits and ` +
				"dashes, starting and ending with a letter or digit"},
		{"rule name with a space", func(c *nc) { c.Firewall.Rules[0].Name = "nomad http" },
			`node config: firewall rules[0]: name "nomad http" is not 1 to 63 lower-case letters, digits and ` +
				"dashes, starting and ending with a letter or digit"},
		{"rule name in upper case", func(c *nc) { c.Firewall.Rules[0].Name = "SSH" },
			`node config: firewall rules[0]: name "SSH" is not 1 to 63 lower-case letters, digits and dashes, ` +
				"starting and ending with a letter or digit"},
		{"rule name with a leading dash", func(c *nc) { c.Firewall.Rules[0].Name = "-ssh" },
			`node config: firewall rules[0]: name "-ssh" is not 1 to 63 lower-case letters, digits and dashes, ` +
				"starting and ending with a letter or digit"},
		{"rule name of 63 characters", func(c *nc) { c.Firewall.Rules[0].Name = strings.Repeat("a", 63) }, ""},
		{"rule name of 64 characters", func(c *nc) { c.Firewall.Rules[0].Name = strings.Repeat("a", 64) },
			`node config: firewall rules[0]: name "` + strings.Repeat("a", 64) + `" is not 1 to 63 lower-case ` +
				"letters, digits and dashes, starting and ending with a letter or digit"},
		{"rule protocol", func(c *nc) { c.Firewall.Rules[0].Protocol = "sctp" },
			`node config: firewall rule ssh/sctp: protocol "sctp" is not tcp, udp or icmp`},
		{"rule without ports", func(c *nc) { c.Firewall.Rules[0].Ports = nodeconfig.PortRange{} },
			"node config: firewall rule ssh/tcp: ports 0-0 are not a range of ports from 1 to 65535"},
		{"rule with reversed ports", func(c *nc) { c.Firewall.Rules[2].Ports = nodeconfig.PortRange{First: 9, Last: 8} },
			"node config: firewall rule dynamic/udp: ports 9-8 are not a range of ports from 1 to 65535"},
		{"ICMP rule with ports", func(c *nc) { c.Firewall.Rules[1].Ports = nodeconfig.PortRange{First: 8, Last: 8} },
			"node config: firewall rule icmp/icmp: ICMP has no ports"},
		{"rule without sources", func(c *nc) { c.Firewall.Rules[2].From = nil },
			"node config: firewall rule dynamic/udp: no sources"},
		{"invalid source", func(c *nc) { c.Firewall.Rules[2].From = []netip.Prefix{{}} },
			"node config: firewall rule dynamic/udp: from[0] is not a prefix"},
		{"source with host bits", func(c *nc) {
			c.Firewall.Rules[2].From = []netip.Prefix{netip.MustParsePrefix("10.64.0.1/16")}
		}, "node config: firewall rule dynamic/udp: from[0] 10.64.0.1/16 has bits set after /16"},
		{"one of two rules with one name", func(c *nc) {
			serf := nodeconfig.Rule{Name: "serf", Protocol: nodeconfig.ProtocolTCP,
				Ports: nodeconfig.PortRange{First: 4648, Last: 4648}, From: c.Firewall.Rules[2].From}
			serfUDP := serf
			serfUDP.Protocol, serfUDP.From = nodeconfig.ProtocolUDP, nil
			c.Firewall.Rules = append(c.Firewall.Rules, serf, serfUDP)
		}, "node config: firewall rule serf/udp: no sources"},
		{"no metadata address", func(c *nc) { c.Firewall.BlockMetadata = netip.Addr{} },
			"node config: firewall: no metadata address to block"},
		// tent-node writes the addresses into the host firewall's ruleset, which a zone could break out of.
		{"metadata address with a zone", func(c *nc) {
			c.Firewall.BlockMetadata = netip.MustParseAddr("fe80::1%x counter accept\n}\nflush ruleset\n#")
		}, `node config: firewall: metadata address "fe80::1%x counter accept\n}\nflush ruleset\n#" has an IPv6 zone`},
		{"metadata address with a zone of letters", func(c *nc) {
			c.Firewall.BlockMetadata = netip.MustParseAddr("fe80::1%eth0")
		}, `node config: firewall: metadata address "fe80::1%eth0" has an IPv6 zone`},
		// The ruleset would match it as IPv6, which no IPv4 packet is.
		{"metadata address in IPv6 form", func(c *nc) {
			c.Firewall.BlockMetadata = netip.MustParseAddr("::ffff:169.254.169.254")
		}, "node config: firewall: metadata address ::ffff:169.254.169.254 is an IPv4 address in IPv6 form; " +
			"write 169.254.169.254"},
		{"source in IPv6 form", func(c *nc) {
			c.Firewall.Rules[2].From = []netip.Prefix{netip.MustParsePrefix("::ffff:10.64.0.0/112")}
		}, "node config: firewall rule dynamic/udp: from[0] ::ffff:10.64.0.0/112 is an IPv4 network in IPv6 form; " +
			"write 10.64.0.0/16"},

		{"the first problem", func(c *nc) { c.Kind, c.Role = "", "" }, `node config: kind "" is not NodeConfig`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sample()
			tc.edit(c)
			if got := errText(c.Validate()); got != tc.want {
				t.Errorf("Validate() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateNil(t *testing.T) {
	var c *nodeconfig.NodeConfig
	if got, want := errText(c.Validate()), "no node config"; got != want {
		t.Errorf("Validate() = %q, want %q", got, want)
	}
}

func TestFileString(t *testing.T) {
	for _, tc := range []struct {
		file nodeconfig.File
		want string
	}{
		{nodeconfig.File{Path: "/etc/nomad.d/00-tent.hcl", Content: []byte("region = \"global\"\n")},
			"/etc/nomad.d/00-tent.hcl [18 bytes]"},
		{nodeconfig.File{Path: "/etc/nomad.d/01-gossip.hcl", Content: []byte("0123456789"), Secret: true},
			"/etc/nomad.d/01-gossip.hcl [secret, 10 bytes]"},
	} {
		if got := tc.file.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
		if got := tc.file.GoString(); got != tc.want {
			t.Errorf("GoString() = %q, want %q", got, tc.want)
		}
	}
}

func TestAssetString(t *testing.T) {
	const sum = "3c7d1e9a5b2f8046c1e7a3d9b5f20864e1c7a3d9b5f20864e1c7a3d9b5f20864"
	for _, tc := range []struct {
		urls []string
		want string
	}{
		{[]string{presignedURL}, "tent-node v1 sha256 " + sum + " from " +
			"https://tent-dev.s3.example.com/tent-node_linux_amd64?[query hidden]"},
		{[]string{"https://a.example.com/x?sig=1", "https://b.example.com/y#part", "https://ops:pw@c.example.com/z"},
			"tent-node v1 sha256 " + sum + " from https://a.example.com/x?[query hidden], " +
				"https://b.example.com/y#part, https://ops:xxxxx@c.example.com/z"},
		{[]string{"https://mirror.example.com/%zz?sig=1"},
			"tent-node v1 sha256 " + sum + " from [a URL that does not parse]"},
	} {
		a := nodeconfig.Asset{Name: "tent-node", Version: "v1", URLs: tc.urls, SHA256: sum}
		if got := a.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
		if got := a.GoString(); got != tc.want {
			t.Errorf("GoString() = %q, want %q", got, tc.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{presignedURL, "https://tent-dev.s3.example.com/tent-node_linux_amd64?[query hidden]"},
		{"https://b.example.com/y#part", "https://b.example.com/y#part"},
		{"https://ops:pw@c.example.com/z?sig=1", "https://ops:xxxxx@c.example.com/z?[query hidden]"},
		{"https://mirror.example.com/%zz?sig=1", "[a URL that does not parse]"},
	} {
		if got := nodeconfig.RedactURL(tc.url); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// TestAssetHidesURLQueries checks that no way of printing or logging an asset shows the query of its URLs, which may
// carry a signature, while Encode keeps the URLs as they are.
func TestAssetHidesURLQueries(t *testing.T) {
	c := sample()
	secrets := map[string][]byte{"the URL's signature": urlSignature}
	secrettest.CheckHidden(t, secrettest.Printed(t, c.Assets[2]), secrets, "?[query hidden]")
	data, err := nodeconfig.Encode(c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(data), `"`+presignedURL+`"`) {
		t.Error("Encode does not keep the presigned URL as it is")
	}
}

// TestFileNeverPrintsContent checks that no way of printing or logging a file, alone or in a NodeConfig, shows its
// content: secret files show that they are secret and their size, other files their size.
func TestFileNeverPrintsContent(t *testing.T) {
	c := sample()
	gossip, key, tent := c.Files[1], c.Files[2], c.Files[0]
	secrets := map[string][]byte{"the gossip key": gossipKey, "the node key": nodeKey}
	secrettest.CheckHidden(t, secrettest.Printed(t, gossip), secrets,
		fmt.Sprintf("/etc/nomad.d/01-gossip.hcl [secret, %d bytes]", len(gossip.Content)))
	secrettest.CheckHidden(t, secrettest.Printed(t, key), secrets,
		fmt.Sprintf("/etc/nomad.d/tls/agent-key.pem [secret, %d bytes]", len(key.Content)))
	secrettest.CheckHidden(t, secrettest.Printed(t, *c),
		map[string][]byte{"the gossip key": gossipKey, "the node key": nodeKey, "the URL's signature": urlSignature},
		"[secret, ")
	secrettest.CheckHidden(t, secrettest.Printed(t, tent), map[string][]byte{"the content": tent.Content},
		fmt.Sprintf("/etc/nomad.d/00-tent.hcl [%d bytes]", len(tent.Content)))
}
