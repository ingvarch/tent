package nodeconfig_test

import (
	"encoding/base64"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// checkGolden compares got with the file testdata/name. With -update it rewrites the file first.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("output differs from %s (-file +got):\n%s", path, diff)
	}
}

// errText returns the error's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Stand-ins for the secrets of a node. They are not real keys, but the tests treat them as secrets.
var (
	gossipKey = []byte(base64.StdEncoding.EncodeToString([]byte("secret-gossip-key-for-tests-only")))
	nodeKey   = []byte("node-key-stand-in-4f1d9c27b8e3a605\n")
	// urlSignature signs the presigned URL of a development build's tent-node.
	urlSignature = []byte("9f2c6e1ab47d03e58c1f6a2b7d4e90c3a5f81b6d2e7c409a3f5b8d1e6c2a7f40")
)

// presignedURL is where a development build's nodes download tent-node.
var presignedURL = "https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Expires=900&X-Amz-Signature=" +
	string(urlSignature)

// sample returns a valid NodeConfig of the first node of a combined group, with every part set.
func sample() *nodeconfig.NodeConfig {
	cidr := netip.MustParsePrefix("10.64.0.0/16")
	anywhere := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	return &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion,
		Kind:       nodeconfig.Kind,
		Cluster:    "prod",
		NodeGroup:  "core",
		Name:       "prod-core-0",
		Role:       v1alpha1.RoleCombined,
		Assets: []nodeconfig.Asset{
			{
				Name: "nomad", Version: "2.0.7",
				URLs: []string{
					"https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip",
					"https://mirror.example.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip",
				},
				SHA256: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
			},
			{
				Name: "cni-plugins", Version: "1.9.1",
				URLs: []string{
					"https://github.com/containernetworking/plugins/releases/download/v1.9.1/" +
						"cni-plugins-linux-amd64-v1.9.1.tgz",
				},
				SHA256: "b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303",
			},
			{
				Name: "tent-node", Version: "v0.3.0-4-gabc1234", URLs: []string{presignedURL},
				SHA256: "3c7d1e9a5b2f8046c1e7a3d9b5f20864e1c7a3d9b5f20864e1c7a3d9b5f20864",
			},
		},
		Files: []nodeconfig.File{
			{
				Path: "/etc/nomad.d/00-tent.hcl", Mode: 0o644, Owner: "root:root",
				Content: []byte("# group configuration\nregion = \"global\"\n"),
			},
			{
				Path: "/etc/nomad.d/01-gossip.hcl", Mode: 0o600, Owner: "root:root",
				Content: []byte("server {\n  encrypt = \"" + string(gossipKey) + "\"\n}\n"), Secret: true,
			},
			{
				Path: "/etc/nomad.d/tls/agent-key.pem", Mode: 0o600, Owner: "root:root",
				Content: nodeKey, PerNode: true, Secret: true,
			},
			{
				Path: "/etc/nomad.d/10-node.hcl", Mode: 0o644, Owner: "root:root",
				Content: []byte("name = \"prod-core-0\" # <&> é\n"), PerNode: true,
			},
		},
		Join: nodeconfig.Join{
			Strategy:        nodeconfig.JoinSeedAndRefresh,
			Servers:         []netip.Addr{netip.MustParseAddr("10.64.0.5"), netip.MustParseAddr("10.64.0.9")},
			RefreshInterval: time.Minute,
		},
		System: nodeconfig.System{
			Sysctls:       map[string]string{"vm.max_map_count": "262144", "net.ipv4.ip_forward": "1"},
			KernelModules: []string{"br_netfilter", "overlay"},
			Docker:        true,
		},
		Firewall: nodeconfig.HostFirewall{
			Rules: []nodeconfig.Rule{
				{Name: "ssh", Protocol: nodeconfig.ProtocolTCP, Ports: nodeconfig.PortRange{First: 22, Last: 22},
					From: anywhere},
				{Name: "icmp", Protocol: nodeconfig.ProtocolICMP, From: anywhere},
				{Name: "dynamic", Protocol: nodeconfig.ProtocolUDP,
					Ports: nodeconfig.PortRange{First: 20000, Last: 32000}, From: []netip.Prefix{cidr}},
			},
			BlockMetadata: netip.MustParseAddr("169.254.169.254"),
		},
	}
}
