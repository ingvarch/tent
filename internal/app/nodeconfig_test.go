package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/spec"
)

// The specs of the test cluster prod: servers, workers and a combined group, which a cluster has instead of servers.
// Every setting that NodeConfig takes differs from its default.
const (
	nodeClusterYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: vultr
    region: ams
  networking:
    cidr: 10.10.0.0/16
  access:
    ssh: [203.0.113.7/32]
    api: [198.51.100.0/24]
  nomad:
    version: "2.0.7"
    region: eu
    clientIntroduction: warn
    tls:
      verifyHTTPSClient: false
    extraConfig:
      server: |
        server {
          raft_multiplier = 2
        }
      client: |
        client {
          gc_max_allocs = 100
        }
`
	nodeServersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  size: 3
`
	nodeWorkersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-4c-8gb
  size: 2
  nomad:
    nodePool: batch
    nodeClass: general
    drivers: [exec]
    meta:
      team: platform
      team.owner: ops
`
	nodeCombinedYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: dev
  cluster: prod
spec:
  role: combined
  machineType: vc2-2c-4gb
  size: 3
  nomad:
    nodeClass: small
`
)

// testNow is when the test CA and certificates are made, and when the signatures of releases are checked.
var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// at returns a clock that stands at t.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// equateNetip lets cmp compare netip values, whose fields are unexported.
var equateNetip = cmpopts.EquateComparable(netip.Prefix{}, netip.Addr{})

// nodeSpecs decodes the docs, fills in the defaults, as the completed spec holds them, and returns the model with the
// specs.
func nodeSpecs(t *testing.T, docs ...string) (*model.Cluster, *v1alpha1.Cluster, []*v1alpha1.NodeGroup) {
	t.Helper()
	objs, err := spec.Decode([]byte(strings.Join(docs, "---\n")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v1alpha1.SetDefaults(objs.Cluster, objs.NodeGroups)
	if err := v1alpha1.Validate(objs.Cluster, objs.NodeGroups, v1alpha1.ValidateOptions{}); err != nil {
		t.Fatalf("the test specs are not valid: %v", err)
	}
	m, err := model.New(objs.Cluster, objs.NodeGroups)
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	return m, objs.Cluster, objs.NodeGroups
}

// The assets of the test nodes: Nomad and tent-node with made-up sums, the CNI plugins of stable.
var (
	testNomad    = nodeconfig.Asset{Name: "nomad", Version: "2.0.7", URLs: []string{nomadZipURL}, SHA256: sum1}
	testCNI      = nodeconfig.Asset{Name: "cni-plugins", Version: "1.9.1", URLs: []string{cniURL}, SHA256: stableCNI}
	testTentNode = nodeconfig.Asset{Name: "tent-node", Version: "v0.3.0", URLs: []string{tentNodeURL}, SHA256: sum2}
)

// testAssets returns the assets of the test nodes.
func testAssets() nodeAssets {
	return nodeAssets{nomad: testNomad, cni: testCNI, tentNode: testTentNode}
}

// Where the test assets come from, and the sha256 of the CNI plugins that stable pins for amd64.
const (
	nomadZipURL = "https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip"
	cniURL      = "https://github.com/containernetworking/plugins/releases/download/v1.9.1/" +
		"cni-plugins-linux-amd64-v1.9.1.tgz"
	tentNodeURL = "https://github.com/ingvarch/tent/releases/download/v0.3.0/tent-node_linux_amd64"
	stableCNI   = "b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303"
	sum1        = "1111111111111111111111111111111111111111111111111111111111111111"
	sum2        = "2222222222222222222222222222222222222222222222222222222222222222"
)

// testCA makes the CA of the test cluster.
func testCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA("prod", testNow)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return ca
}

// templates returns the templates of the test groups, and fails the test on an error.
func templates(t *testing.T, gossip pki.Secret, caBundle []byte, docs ...string) map[string]nodeconfig.NodeConfig {
	t.Helper()
	m, c, groups := nodeSpecs(t, docs...)
	tmpls, err := groupTemplates(m, c, groups, testAssets(), gossip, caBundle)
	if err != nil {
		t.Fatalf("groupTemplates: %v", err)
	}
	return tmpls
}

// fileView is what a test compares of a file: its content only by its size and digest, so that a failure message
// never shows a secret.
type fileView struct {
	Path            string
	Mode            uint32
	Owner           string
	PerNode, Secret bool
	Content         string
}

func viewOf(files []nodeconfig.File) []fileView {
	views := make([]fileView, len(files))
	for i, f := range files {
		views[i] = fileView{Path: f.Path, Mode: f.Mode, Owner: f.Owner, PerNode: f.PerNode, Secret: f.Secret,
			Content: fmt.Sprintf("%d bytes, sha256 %x", len(f.Content), sha256.Sum256(f.Content))}
	}
	return views
}

// fileAt returns the file of files at path, and fails the test when there is none.
func fileAt(t *testing.T, files []nodeconfig.File, path string) nodeconfig.File {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %s", path)
	return nodeconfig.File{}
}

// The host firewall rules of the test cluster, whose private network is 10.10.0.0/16.
var (
	anywhereFrom = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	clusterFrom  = []netip.Prefix{netip.MustParsePrefix("10.10.0.0/16")}

	sshRule  = nodeconfig.Rule{Name: "ssh", Protocol: "tcp", Ports: ports(22, 22), From: anywhereFrom}
	icmpRule = nodeconfig.Rule{Name: "icmp", Protocol: "icmp", From: anywhereFrom}
	apiRule  = nodeconfig.Rule{Name: "api", Protocol: "tcp", Ports: ports(4646, 4646), From: anywhereFrom}
	httpRule = nodeconfig.Rule{Name: "nomad-http", Protocol: "tcp", Ports: ports(4646, 4646), From: clusterFrom}
	rpcRule  = nodeconfig.Rule{Name: "nomad-rpc", Protocol: "tcp", Ports: ports(4647, 4647), From: clusterFrom}
	serfTCP  = nodeconfig.Rule{Name: "serf", Protocol: "tcp", Ports: ports(4648, 4648), From: clusterFrom}
	serfUDP  = nodeconfig.Rule{Name: "serf", Protocol: "udp", Ports: ports(4648, 4648), From: clusterFrom}
	dynTCP   = nodeconfig.Rule{Name: "dynamic", Protocol: "tcp", Ports: ports(20000, 32000), From: clusterFrom}
	dynUDP   = nodeconfig.Rule{Name: "dynamic", Protocol: "udp", Ports: ports(20000, 32000), From: clusterFrom}

	// Nomad's and Docker's default bridges, where workloads reach the node itself.
	bridgeFrom = []netip.Prefix{netip.MustParsePrefix("172.26.64.0/20"), netip.MustParsePrefix("172.17.0.0/16")}
	bridgeHTTP = nodeconfig.Rule{Name: "bridge-http", Protocol: "tcp", Ports: ports(4646, 4646), From: bridgeFrom}
	bridgeTCP  = nodeconfig.Rule{Name: "bridge-dynamic", Protocol: "tcp", Ports: ports(20000, 32000), From: bridgeFrom}
	bridgeUDP  = nodeconfig.Rule{Name: "bridge-dynamic", Protocol: "udp", Ports: ports(20000, 32000), From: bridgeFrom}

	// A server runs no workloads, so only nodes that run a client open the dynamic ports and the bridge rules.
	serverRules   = []nodeconfig.Rule{sshRule, icmpRule, apiRule, httpRule, rpcRule, serfTCP, serfUDP}
	clientRules   = []nodeconfig.Rule{sshRule, icmpRule, httpRule, dynTCP, dynUDP, bridgeHTTP, bridgeTCP, bridgeUDP}
	combinedRules = []nodeconfig.Rule{sshRule, icmpRule, apiRule, httpRule, rpcRule, serfTCP, serfUDP, dynTCP, dynUDP,
		bridgeHTTP, bridgeTCP, bridgeUDP}
)

func ports(first, last uint16) nodeconfig.PortRange {
	return nodeconfig.PortRange{First: first, Last: last}
}

// bridgeSysctls pass bridged traffic through the firewall on every node that runs a client.
var bridgeSysctls = map[string]string{
	"net.bridge.bridge-nf-call-arptables": "1",
	"net.bridge.bridge-nf-call-ip6tables": "1",
	"net.bridge.bridge-nf-call-iptables":  "1",
}

// TestGroupTemplates checks the template of each role: the Nomad agent that the specs describe, the CA bundle, the
// assets, the join strategy, the system and the host firewall, with the spec hash of it all.
func TestGroupTemplates(t *testing.T) {
	gossip, ca := pki.NewGossipKey(), testCA(t)
	caFile := nodeconfig.File{Path: "/etc/nomad.d/tls/ca.pem", Mode: 0o644, Owner: "root:root", Content: ca.Bundle()}
	agent := nodeconfig.Agent{
		Cluster: "prod", Region: "eu", CIDR: netip.MustParsePrefix("10.10.0.0/16"),
		ClientIntroduction: v1alpha1.ClientIntroductionWarn, Gossip: gossip, VerifyHTTPSClient: false,
		DynamicPorts: ports(20000, 32000),
		ExtraServer:  "server {\n  raft_multiplier = 2\n}\n", ExtraClient: "client {\n  gc_max_allocs = 100\n}\n",
	}
	with := func(edit func(a *nodeconfig.Agent)) nodeconfig.Agent {
		a := agent
		edit(&a)
		return a
	}
	for _, tc := range []struct {
		name     string
		docs     []string
		group    string
		agent    nodeconfig.Agent
		assets   []nodeconfig.Asset
		system   nodeconfig.System
		firewall []nodeconfig.Rule
	}{
		{
			name:     "server",
			docs:     []string{nodeClusterYAML, nodeServersYAML, nodeWorkersYAML},
			group:    "servers",
			agent:    with(func(a *nodeconfig.Agent) { a.Role, a.Group = v1alpha1.RoleServer, "servers" }),
			assets:   []nodeconfig.Asset{testNomad, testTentNode}, // servers run no workloads
			system:   nodeconfig.System{},
			firewall: serverRules,
		},
		{
			name:  "client",
			docs:  []string{nodeClusterYAML, nodeServersYAML, nodeWorkersYAML},
			group: "workers",
			agent: with(func(a *nodeconfig.Agent) {
				a.Role, a.Group, a.NodePool, a.NodeClass = v1alpha1.RoleClient, "workers", "batch", "general"
				a.Drivers, a.Meta = []string{"exec"}, map[string]string{"team": "platform", "team.owner": "ops"}
			}),
			assets: []nodeconfig.Asset{testNomad, testCNI, testTentNode},
			// Its drivers leave Docker out.
			system:   nodeconfig.System{Sysctls: bridgeSysctls, KernelModules: []string{"br_netfilter"}},
			firewall: clientRules,
		},
		{
			name:  "combined",
			docs:  []string{nodeClusterYAML, nodeCombinedYAML},
			group: "dev",
			agent: with(func(a *nodeconfig.Agent) {
				a.Role, a.Group, a.NodePool, a.NodeClass = v1alpha1.RoleCombined, "dev", "default", "small"
			}),
			assets: []nodeconfig.Asset{testNomad, testCNI, testTentNode},
			// No drivers keeps all of Nomad's built-in drivers, Docker among them.
			system: nodeconfig.System{
				Sysctls: bridgeSysctls, KernelModules: []string{"br_netfilter", "overlay"}, Docker: true,
			},
			firewall: combinedRules,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, ok := templates(t, gossip, ca.Bundle(), tc.docs...)[tc.group]
			if !ok {
				t.Fatalf("no template of node group %s", tc.group)
			}
			agentFiles, err := nodeconfig.RenderAgent(tc.agent)
			if err != nil {
				t.Fatalf("RenderAgent: %v", err)
			}
			// 00-tent.hcl holds no secret, so it shows how a setting of the specs went wrong.
			const tentFile = "/etc/nomad.d/00-tent.hcl"
			if diff := cmp.Diff(string(fileAt(t, agentFiles, tentFile).Content),
				string(fileAt(t, tmpl.Files, tentFile).Content)); diff != "" {
				t.Errorf("00-tent.hcl (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(viewOf(append(agentFiles, caFile)), viewOf(tmpl.Files)); diff != "" {
				t.Errorf("Files (-want +got):\n%s", diff)
			}
			tmpl.Files = nil
			want := nodeconfig.NodeConfig{
				APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind, Cluster: "prod",
				Provider: v1alpha1.ProviderVultr, NodeGroup: tc.group, Role: tc.agent.Role, Assets: tc.assets,
				Join:   nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
				System: tc.system,
				Firewall: nodeconfig.HostFirewall{
					Rules: tc.firewall, BlockMetadata: netip.MustParseAddr("169.254.169.254"),
				},
				SpecHash: tmpl.SpecHash,
			}
			if diff := cmp.Diff(want, tmpl, equateNetip); diff != "" {
				t.Errorf("template (-want +got):\n%s", diff)
			}
		})
	}
}

// TestGroupTemplatesSpecHash checks that a template carries the hash of its group-level configuration, that the
// groups of one cluster differ, and that spec.access leaves every hash as it is: the cloud firewall filters the
// sources.
func TestGroupTemplatesSpecHash(t *testing.T) {
	gossip, bundle := pki.NewGossipKey(), testCA(t).Bundle()
	tmpls := templates(t, gossip, bundle, nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	for name, tmpl := range tmpls {
		if want := nodeconfig.SpecHash(&tmpl); tmpl.SpecHash != want || want == "" {
			t.Errorf("the template of %s has the spec hash %q, want %q", name, tmpl.SpecHash, want)
		}
	}
	if tmpls["servers"].SpecHash == tmpls["workers"].SpecHash {
		t.Error("servers and workers have one spec hash")
	}
	access := strings.Replace(nodeClusterYAML, "ssh: [203.0.113.7/32]\n    api: [198.51.100.0/24]",
		"ssh: []\n    api: [0.0.0.0/0, 2001:db8::/32]", 1)
	if access == nodeClusterYAML {
		t.Fatal("the test did not change spec.access")
	}
	for name, tmpl := range templates(t, gossip, bundle, access, nodeServersYAML, nodeWorkersYAML) {
		if tmpl.SpecHash != tmpls[name].SpecHash {
			t.Errorf("a change of spec.access changes the spec hash of %s", name)
		}
	}
	// A new release of the CNI plugins marks only the nodes that run a client out of date.
	m, c, groups := nodeSpecs(t, nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	newCNI := testAssets()
	newCNI.cni.Version, newCNI.cni.SHA256 = "1.9.2", strings.Repeat("3", 64)
	withCNI, err := groupTemplates(m, c, groups, newCNI, gossip, bundle)
	if err != nil {
		t.Fatalf("groupTemplates: %v", err)
	}
	if withCNI["servers"].SpecHash != tmpls["servers"].SpecHash {
		t.Error("a new release of the CNI plugins changes the spec hash of the servers")
	}
	if withCNI["workers"].SpecHash == tmpls["workers"].SpecHash {
		t.Error("a new release of the CNI plugins leaves the spec hash of the workers as it is")
	}
	// A new CA bundle, as a rotation makes, marks the nodes out of date.
	for name, tmpl := range templates(t, gossip, testCA(t).Bundle(), nodeClusterYAML, nodeServersYAML,
		nodeWorkersYAML) {
		if tmpl.SpecHash == tmpls[name].SpecHash {
			t.Errorf("a new CA bundle leaves the spec hash of %s as it is", name)
		}
	}
}

// TestGroupTemplatesProvider checks that every template names the model's provider, which tells tent-node whose
// metadata service to read.
func TestGroupTemplatesProvider(t *testing.T) {
	m, c, groups := nodeSpecs(t, nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	m.Provider = v1alpha1.ProviderHetzner
	tmpls, err := groupTemplates(m, c, groups, testAssets(), pki.NewGossipKey(), testCA(t).Bundle())
	if err != nil {
		t.Fatalf("groupTemplates: %v", err)
	}
	for name, tmpl := range tmpls {
		if tmpl.Provider != v1alpha1.ProviderHetzner {
			t.Errorf("the template of %s names the provider %q, want hetzner", name, tmpl.Provider)
		}
	}
}

// TestHostFirewall checks the host firewall of each role in a cluster whose private network is 10.10.0.0/16: the
// rules that the templates of the test cluster carry, and the metadata service blocked.
func TestHostFirewall(t *testing.T) {
	cidr := netip.MustParsePrefix("10.10.0.0/16")
	for _, tc := range []struct {
		role  v1alpha1.Role
		rules []nodeconfig.Rule
	}{
		{v1alpha1.RoleServer, serverRules},
		{v1alpha1.RoleClient, clientRules},
		{v1alpha1.RoleCombined, combinedRules},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			want := nodeconfig.HostFirewall{Rules: tc.rules, BlockMetadata: netip.MustParseAddr("169.254.169.254")}
			if diff := cmp.Diff(want, HostFirewall(cidr, tc.role), equateNetip); diff != "" {
				t.Errorf("HostFirewall (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNodeSystem checks the system settings of each role: bridge networking on a node that runs a client, and Docker
// with its storage module unless the drivers leave the docker driver out.
func TestNodeSystem(t *testing.T) {
	withDocker := nodeconfig.System{
		Sysctls: bridgeSysctls, KernelModules: []string{"br_netfilter", "overlay"}, Docker: true,
	}
	for _, tc := range []struct {
		name    string
		role    v1alpha1.Role
		drivers []string
		want    nodeconfig.System
	}{
		{"server", v1alpha1.RoleServer, nil, nodeconfig.System{}},
		{"server with docker in its drivers", v1alpha1.RoleServer, []string{"docker"}, nodeconfig.System{}},
		{"client with every driver", v1alpha1.RoleClient, nil, withDocker},
		{"client with docker", v1alpha1.RoleClient, []string{"exec", "docker"}, withDocker},
		{
			"client without docker", v1alpha1.RoleClient, []string{"exec"},
			nodeconfig.System{Sysctls: bridgeSysctls, KernelModules: []string{"br_netfilter"}},
		},
		{"combined with every driver", v1alpha1.RoleCombined, nil, withDocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, NodeSystem(tc.role, tc.drivers)); diff != "" {
				t.Errorf("NodeSystem (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGroupTemplatesErrors(t *testing.T) {
	gossip, bundle := pki.NewGossipKey(), testCA(t).Bundle()
	for _, tc := range []struct {
		name   string
		edit   func(m *model.Cluster, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup
		gossip pki.Secret
		bundle []byte
		want   string
	}{
		{
			name: "a group without its spec",
			edit: func(_ *model.Cluster, _ *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup {
				return groups[:1]
			},
			gossip: gossip, bundle: bundle,
			want: "node group workers: no spec",
		},
		{
			name: "no defaults",
			edit: func(_ *model.Cluster, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup {
				c.Spec.Nomad.TLS.VerifyHTTPSClient = nil
				return groups
			},
			gossip: gossip, bundle: bundle,
			want: "cluster prod: spec.nomad.tls.verifyHTTPSClient is not set",
		},
		{
			name: "no join strategy",
			edit: func(m *model.Cluster, _ *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup {
				m.Join = 0
				return groups
			},
			gossip: gossip, bundle: bundle,
			want: "cluster prod: join strategy JoinStrategy(0) is not one that nodes know",
		},
		{
			name: "no gossip key",
			edit: func(_ *model.Cluster, _ *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup {
				return groups
			},
			bundle: bundle,
			want:   "node group servers: nomad agent: no gossip key",
		},
		{
			name: "no CA bundle",
			edit: func(_ *model.Cluster, _ *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) []*v1alpha1.NodeGroup {
				return groups
			},
			gossip: gossip,
			want:   "cluster prod: no CA bundle",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c, groups := nodeSpecs(t, nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
			groups = tc.edit(m, c, groups)
			tmpls, err := groupTemplates(m, c, groups, testAssets(), tc.gossip, tc.bundle)
			if err == nil || err.Error() != tc.want {
				t.Errorf("groupTemplates() error = %v, want %s", err, tc.want)
			}
			if tmpls != nil {
				t.Error("groupTemplates() returned templates with the error")
			}
		})
	}
}

// introToken returns a stand-in of an intro token of n bytes, three base64url parts as a JWT has.
func introToken(n int) pki.Secret {
	enc := noise(1, n-2)
	third := len(enc) / 3
	return pki.Secret(enc[:third] + "." + enc[third:2*third] + "." + enc[2*third:])
}

// noise returns n bytes of base64url, which is printable ASCII and safe in a URL, from ChaCha8 with the seed: the
// same text every time, which gzip shrinks no more than a real token or signature.
func noise(seed byte, n int) string {
	raw := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(raw) // never fails
	return base64.RawURLEncoding.EncodeToString(raw)[:n]
}

// seeds returns n private addresses of the test cluster from 10.10.0.10 on.
func seeds(n int) []netip.Addr {
	addrs := make([]netip.Addr, n)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{10, 10, 0, byte(10 + i)})
	}
	return addrs
}

// TestNodeConfig checks the config of one node of each role: its template with its name, its 10-node.hcl, its
// certificate and key, an intro token when it runs a client and has one, and the seed.
func TestNodeConfig(t *testing.T) {
	ca := testCA(t)
	tmpls := templates(t, pki.NewGossipKey(), ca.Bundle(), nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	combined := templates(t, pki.NewGossipKey(), ca.Bundle(), nodeClusterYAML, nodeCombinedYAML)["dev"]
	intro := introToken(2048)
	for _, tc := range []struct {
		name            string
		tmpl            nodeconfig.NodeConfig
		node            string
		bootstrapExpect int
		intro           pki.Secret
		wantIntro       bool
	}{
		{"server", tmpls["servers"], "prod-servers-1", 3, nil, false},
		{"client with an intro token", tmpls["workers"], "prod-workers-4", 3, intro, true},
		{"client without an intro token", tmpls["workers"], "prod-workers-4", 3, nil, false},
		{"combined with an intro token", combined, "prod-dev-0", 3, intro, true},
		{"combined without an intro token", combined, "prod-dev-0", 3, pki.Secret{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := ca.IssueNode(tc.tmpl.Role, "eu", testNow)
			if err != nil {
				t.Fatalf("IssueNode: %v", err)
			}
			before := viewOf(tc.tmpl.Files)
			nc, err := nodeConfig(tc.tmpl, tc.node, "ams", tc.bootstrapExpect, cert, seeds(3), tc.intro)
			if err != nil {
				t.Fatalf("nodeConfig: %v", err)
			}
			node, err := nodeconfig.RenderNode(tc.node, "ams", tc.tmpl.Role, tc.bootstrapExpect)
			if err != nil {
				t.Fatalf("RenderNode: %v", err)
			}
			wantFiles := append(slices.Clone(tc.tmpl.Files), node,
				nodeconfig.File{Path: "/etc/nomad.d/tls/agent.pem", Mode: 0o644, Owner: "root:root",
					Content: cert.Cert, PerNode: true},
				nodeconfig.File{Path: "/etc/nomad.d/tls/agent-key.pem", Mode: 0o600, Owner: "root:root",
					Content: cert.Key, PerNode: true, Secret: true})
			if tc.wantIntro {
				wantFiles = append(wantFiles, nodeconfig.File{Path: "/var/lib/nomad/client/intro_token.jwt",
					Mode: 0o600, Owner: "root:root", Content: tc.intro, PerNode: true, Secret: true})
			}
			if diff := cmp.Diff(viewOf(wantFiles), viewOf(nc.Files)); diff != "" {
				t.Errorf("Files (-want +got):\n%s", diff)
			}
			want := tc.tmpl
			want.Name, want.Files, want.Join.Servers = tc.node, nc.Files, seeds(3)
			if diff := cmp.Diff(want, *nc, equateNetip); diff != "" {
				t.Errorf("node config (-want +got):\n%s", diff)
			}
			if got := nodeconfig.SpecHash(nc); got != tc.tmpl.SpecHash {
				t.Errorf("the node's own parts change the spec hash from %s to %s", tc.tmpl.SpecHash, got)
			}
			if err := nc.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if diff := cmp.Diff(before, viewOf(tc.tmpl.Files)); diff != "" {
				t.Errorf("nodeConfig changed the template's files (-before +after):\n%s", diff)
			}
		})
	}
}

// TestNodeConfigsShareNothing checks that the configs of two nodes of one group share none of their own parts, with
// each other or with the caller, even when the template's files have room to grow.
func TestNodeConfigsShareNothing(t *testing.T) {
	ca := testCA(t)
	tmpl := templates(t, pki.NewGossipKey(), ca.Bundle(), nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)["workers"]
	tmpl.Files = append(make([]nodeconfig.File, 0, len(tmpl.Files)+8), tmpl.Files...)
	seed := seeds(2)
	var configs []*nodeconfig.NodeConfig
	for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
		cert, err := ca.IssueNode(v1alpha1.RoleClient, "eu", testNow)
		if err != nil {
			t.Fatalf("IssueNode: %v", err)
		}
		nc, err := nodeConfig(tmpl, name, "ams", 3, cert, seed, introToken(64))
		if err != nil {
			t.Fatalf("nodeConfig: %v", err)
		}
		configs = append(configs, nc)
	}
	seed[0] = netip.MustParseAddr("10.10.9.9")
	if n := string(fileAt(t, configs[0].Files, "/etc/nomad.d/10-node.hcl").Content); !strings.Contains(n,
		`"prod-workers-0"`) {
		t.Errorf("the first node's 10-node.hcl is that of another node:\n%s", n)
	}
	if got, want := configs[0].Join.Servers, seeds(2); !slices.Equal(got, want) {
		t.Errorf("the first node's seed is %v after the caller changed its own, want %v", got, want)
	}
}

func TestNodeConfigErrors(t *testing.T) {
	ca := testCA(t)
	tmpls := templates(t, pki.NewGossipKey(), ca.Bundle(), nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	cert, err := ca.IssueNode(v1alpha1.RoleServer, "eu", testNow)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	noCert, noKey := cert, cert
	noCert.Cert, noKey.Key = nil, pki.Secret{}
	for _, tc := range []struct {
		name  string
		node  string
		cert  pki.Certificate
		seed  []netip.Addr
		intro pki.Secret
		want  string
	}{
		{"a name that is no host name", "Prod_1", cert, seeds(1), nil, `node Prod_1: 10-node.hcl: name "Prod_1" is ` +
			"not a host name: 1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or " +
			"digit"},
		{"an intro token for a server", "prod-servers-0", cert, seeds(1), introToken(64),
			"node prod-servers-0: a server takes no intro token"},
		{"a repeated seed", "prod-servers-0", cert, append(seeds(2), seeds(1)...), nil,
			"node prod-servers-0: node config: join servers[2]: 10.10.0.10 is repeated"},
		{"no certificate", "prod-servers-0", noCert, seeds(1), nil, "node prod-servers-0: no certificate or key"},
		{"no key", "prod-servers-0", noKey, seeds(1), nil, "node prod-servers-0: no certificate or key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := nodeConfig(tmpls["servers"], tc.node, "ams", 3, tc.cert, tc.seed, tc.intro)
			if err == nil || err.Error() != tc.want {
				t.Errorf("nodeConfig() error = %v, want %s", err, tc.want)
			}
			if nc != nil {
				t.Error("nodeConfig() returned a config with the error")
			}
		})
	}
}

// TestNodeConfigHidesSecrets checks that printing a node's config shows none of its secrets: the gossip key, the
// node's key and intro token, and the signature of a development build's tent-node URL.
func TestNodeConfigHidesSecrets(t *testing.T) {
	ca, gossip, intro := testCA(t), pki.NewGossipKey(), introToken(512)
	m, c, groups := nodeSpecs(t, nodeClusterYAML, nodeCombinedYAML)
	const signature = "9f2c6e1ab47d03e58c1f6a2b7d4e90c3a5f81b6d2e7c409a3f5b8d1e6c2a7f40"
	withURL := testAssets()
	withURL.tentNode.URLs = []string{"https://tent-dev.s3.example.com/tent-node?X-Amz-Signature=" + signature}
	tmpls, err := groupTemplates(m, c, groups, withURL, gossip, ca.Bundle())
	if err != nil {
		t.Fatalf("groupTemplates: %v", err)
	}
	cert, err := ca.IssueNode(v1alpha1.RoleCombined, "eu", testNow)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	nc, err := nodeConfig(tmpls["dev"], "prod-dev-0", "ams", 3, cert, seeds(3), intro)
	if err != nil {
		t.Fatalf("nodeConfig: %v", err)
	}
	secrets := map[string][]byte{
		"the gossip key": gossip, "the node key": cert.Key, "the intro token": intro,
		"the URL's signature": []byte(signature),
	}
	secrettest.CheckHidden(t, secrettest.Printed(t, *nc), secrets, "[secret, ")
}

// sites answers requests from its files by full URL, without a network. Any other URL is 404.
type sites map[string]string

func (s sites) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := s[r.URL.String()]
	code := http.StatusOK
	if !ok {
		code = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// nomadReleases serves HashiCorp's own SHA256SUMS of Nomad 2.0.7 and its signature, as releases.hashicorp.com
// published them. They verify with HashiCorp's embedded key at testNow.
func nomadReleases(t *testing.T) sites {
	t.Helper()
	const dir = "https://releases.hashicorp.com/nomad/2.0.7/"
	s := sites{}
	for _, name := range []string{"nomad_2.0.7_SHA256SUMS", "nomad_2.0.7_SHA256SUMS.sig"} {
		data, err := os.ReadFile("../assets/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		s[dir+name] = string(data)
	}
	return s
}

func TestResolveAssets(t *testing.T) {
	const (
		release  = "https://github.com/ingvarch/tent/releases/download/v0.3.0/"
		nodeSum  = "3333333333333333333333333333333333333333333333333333333333333333"
		devURL   = "https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Signature=0123"
		devSum   = "4444444444444444444444444444444444444444444444444444444444444444"
		nomadSum = "4c9b8a0850d6fd9caadbbab09b3e6fdf8b77aa777729543c70c61b85acca68c1"
	)
	stable, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	withRelease := nomadReleases(t)
	withRelease[release+"checksums.txt"] = nodeSum + "  tent-node_linux_amd64\n"
	nomad := nodeconfig.Asset{Name: "nomad", Version: "2.0.7", URLs: []string{nomadZipURL}, SHA256: nomadSum}
	cni := nodeconfig.Asset{Name: "cni-plugins", Version: "1.9.1", URLs: []string{cniURL}, SHA256: stableCNI}
	for _, tc := range []struct {
		name    string
		version string
		opts    assets.Options
		want    []nodeconfig.Asset
	}{
		{
			name:    "a release of tent",
			version: "v0.3.0",
			// A release ignores the variables of a development build.
			opts: assets.Options{
				Client: &http.Client{Transport: withRelease}, DevURL: devURL, DevSHA256: devSum, Now: at(testNow),
			},
			want: []nodeconfig.Asset{nomad, cni, {Name: "tent-node", Version: "v0.3.0",
				URLs: []string{release + "tent-node_linux_amd64"}, SHA256: nodeSum}},
		},
		{
			name:    "a development build of tent",
			version: "v0.3.0-4-gabc1234",
			opts: assets.Options{
				Client: &http.Client{Transport: nomadReleases(t)}, DevURL: devURL, DevSHA256: devSum, Now: at(testNow),
			},
			want: []nodeconfig.Asset{nomad, cni, {Name: "tent-node", Version: "v0.3.0-4-gabc1234",
				URLs: []string{devURL}, SHA256: devSum}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAssets(t.Context(), tc.opts, stable, "2.0.7", tc.version, "amd64")
			if err != nil {
				t.Fatalf("resolveAssets: %v", err)
			}
			// Asset prints its URLs without their queries, so compare the fields.
			type view struct{ Name, Version, URLs, SHA256 string }
			views := func(as []nodeconfig.Asset) []view {
				vs := make([]view, len(as))
				for i, a := range as {
					vs[i] = view{a.Name, a.Version, strings.Join(a.URLs, " "), a.SHA256}
				}
				return vs
			}
			all := []nodeconfig.Asset{got.nomad, got.cni, got.tentNode}
			if diff := cmp.Diff(views(tc.want), views(all)); diff != "" {
				t.Errorf("resolveAssets (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveAssetsErrors(t *testing.T) {
	stable, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	amd64Only := &channels.Channel{Name: "edge", Nomad: stable.Nomad,
		CNI: channels.CNI{Version: "1.9.1", SHA256: map[string]string{"amd64": stableCNI}}}
	const sums = "https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_SHA256SUMS"
	for _, tc := range []struct {
		name    string
		ch      *channels.Channel
		sites   sites
		now     time.Time
		version string
		arch    string
		want    string // the start of the error
	}{
		{
			name: "no Nomad release", ch: stable, sites: sites{}, version: "v0.3.0", arch: "amd64",
			want: "find Nomad 2.0.7: get " + sums + ": 404 Not Found",
		},
		{
			name: "HashiCorp's key expired", ch: stable, sites: nomadReleases(t),
			now: time.Date(2030, 3, 2, 12, 0, 0, 0, time.UTC), version: "v0.3.0", arch: "amd64",
			want: "find Nomad 2.0.7: verify " + sums + " with " + sums + ".sig: HashiCorp's release key embedded in " +
				"this tent expired on 2030-03-01: a newer tent, with the renewed key, is needed: ",
		},
		{
			name: "no CNI plugins for the architecture", ch: amd64Only, sites: nomadReleases(t), version: "v0.3.0",
			arch: "arm64",
			want: "find the CNI plugins: channel edge has no CNI plugins for arm64",
		},
		{
			name: "a development build without its tent-node", ch: stable, sites: nomadReleases(t),
			version: "v0.3.0-4-gabc1234", arch: "amd64",
			want: "find tent-node: tent v0.3.0-4-gabc1234 is a development build, so no release holds its " +
				"tent-node: set TENT_NODE_URL and TENT_NODE_SHA256 to a tent-node built from the same commit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := testNow
			if !tc.now.IsZero() {
				now = tc.now
			}
			opts := assets.Options{Client: &http.Client{Transport: tc.sites}, Now: at(now)}
			got, err := resolveAssets(t.Context(), opts, tc.ch, "2.0.7", tc.version, tc.arch)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("resolveAssets() error = %v, want one that starts with %s", err, tc.want)
			}
			if !reflect.ValueOf(got).IsZero() {
				t.Error("resolveAssets() returned assets with the error")
			}
		})
	}
}

func TestDevVariablesWarning(t *testing.T) {
	const signature = "9f2c6e1ab47d03e58c1f6a2b7d4e90c3a5f81b6d2e7c409a3f5b8d1e6c2a7f40"
	url := "https://tent-dev.s3.example.com/tent-node?X-Amz-Signature=" + signature
	sum := strings.Repeat("a", 64)
	const warning = "TENT_NODE_URL and TENT_NODE_SHA256 are set, but tent v0.3.0 is a release build and ignores " +
		"them: its nodes download the tent-node of release v0.3.0"
	for _, tc := range []struct {
		name    string
		version string
		opts    assets.Options
		want    string
	}{
		{"a release with both", "v0.3.0", assets.Options{DevURL: url, DevSHA256: sum}, warning},
		{"a release with the URL", "v0.3.0", assets.Options{DevURL: url}, "TENT_NODE_URL is set, but tent v0.3.0 is " +
			"a release build and ignores it: its nodes download the tent-node of release v0.3.0"},
		{"a release with the sha256", "v0.3.0", assets.Options{DevSHA256: sum}, "TENT_NODE_SHA256 is set, but tent " +
			"v0.3.0 is a release build and ignores it: its nodes download the tent-node of release v0.3.0"},
		{"a release without them", "v0.3.0", assets.Options{}, ""},
		{"a pre-release with both", "v0.3.0-rc.1", assets.Options{DevURL: url, DevSHA256: sum},
			strings.ReplaceAll(warning, "v0.3.0", "v0.3.0-rc.1")},
		{"a development build with both", "v0.3.0-4-gabc1234", assets.Options{DevURL: url, DevSHA256: sum}, ""},
		{"dev with both", "dev", assets.Options{DevURL: url, DevSHA256: sum}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := devVariablesWarning(tc.version, tc.opts)
			if got != tc.want {
				t.Errorf("devVariablesWarning() = %q, want %q", got, tc.want)
			}
			if secrettest.Shows(got, []byte(signature)) {
				t.Error("the warning shows the URL's signature")
			}
		})
	}
}

// Extra configuration that an operator might give, 621 bytes in all.
const (
	statedExtraServer = `server {
  raft_multiplier = 1

  default_scheduler_config {
    scheduler_algorithm             = "spread"
    memory_oversubscription_enabled = true

    preemption_config {
      batch_scheduler_enabled   = true
      service_scheduler_enabled = true
    }
  }
}
`
	statedExtraClient = `client {
  reserved {
    cpu            = 500
    memory         = 512
    reserved_ports = "22"
  }

  host_volume "data" {
    path      = "/srv/data"
    read_only = false
  }
}

plugin "docker" {
  config {
    allow_privileged = false

    volumes {
      enabled = true
    }

    gc {
      image       = true
      image_delay = "3m"
    }
  }
}
`
)

// TestNodeConfigUserDataFits builds the user data of a combined node from real data and worst-case inputs, and checks
// that it fits and that its payload decodes back to node.json: a real CA and node certificate, the CNI plugins of the
// embedded stable channel, a Nomad release URL, a development build's presigned tent-node URL of 1.5 KiB, an intro
// token of 2 KiB, 5 seeds, 8 meta keys and the extraConfig above.
func TestNodeConfigUserDataFits(t *testing.T) {
	ca := testCA(t)
	cert, err := ca.IssueNode(v1alpha1.RoleCombined, "eu", testNow)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	stable, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	cni, err := assets.CNI(stable, "amd64")
	if err != nil {
		t.Fatalf("CNI: %v", err)
	}
	presigned := "https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Algorithm=AWS4-HMAC-SHA256&" +
		"X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=" +
		fmt.Sprintf("%x", sha256.Sum256([]byte(noise(2, 64)))) + "&X-Amz-Security-Token="
	presigned += noise(3, 1536-len(presigned))
	tentNode, err := assets.TentNode(t.Context(), assets.Options{DevURL: presigned, DevSHA256: strings.Repeat("e", 64)},
		"v0.3.0-4-gabc1234-dirty", "amd64")
	if err != nil {
		t.Fatalf("TentNode: %v", err)
	}
	intro := introToken(2048)
	wantIncompressible(t, "the intro token", intro)
	wantIncompressible(t, "the presigned URL", []byte(presigned))
	nomad := assets.Asset{Name: "nomad", Version: "2.0.7", URLs: []string{nomadZipURL},
		SHA256: "4c9b8a0850d6fd9caadbbab09b3e6fdf8b77aa777729543c70c61b85acca68c1"}
	all := nodeAssets{nomad: nodeconfig.Asset(nomad), cni: nodeconfig.Asset(cni), tentNode: nodeconfig.Asset(tentNode)}

	m, c, groups := nodeSpecs(t, nodeClusterYAML, nodeCombinedYAML)
	c.Spec.Nomad.ExtraConfig = v1alpha1.ExtraConfig{Server: statedExtraServer, Client: statedExtraClient}
	groups[0].Spec.Nomad.Drivers = []string{"docker", "exec"}
	groups[0].Spec.Nomad.Meta = map[string]string{}
	for i := range 8 {
		groups[0].Spec.Nomad.Meta[fmt.Sprintf("team-%d.owner", i)] = fmt.Sprintf("platform-team-%d@example.com", i)
	}
	tmpls, err := groupTemplates(m, c, groups, all, pki.NewGossipKey(), ca.Bundle())
	if err != nil {
		t.Fatalf("groupTemplates: %v", err)
	}
	nc, err := nodeConfig(tmpls["dev"], "prod-dev-17", "ams", 5, cert, seeds(5), intro)
	if err != nil {
		t.Fatalf("nodeConfig: %v", err)
	}
	data, err := nodeconfig.UserData(nc)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	node, err := nodeconfig.Encode(nc)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := payloadOf(t, data); !bytes.Equal(got, node) {
		t.Errorf("the payload decodes to %d bytes that differ from the %d of node.json", len(got), len(node))
	}
	t.Logf("combined node: extraConfig %d bytes, node.json %d bytes, user data %d bytes, %d of %d left",
		len(statedExtraServer)+len(statedExtraClient), len(node), len(data), nodeconfig.MaxUserDataBytes-len(data),
		nodeconfig.MaxUserDataBytes)
}

// wantIncompressible fails the test when gzip shrinks data below 70%, as it shrinks a repeated pattern: base64 of
// random bytes, which a real token or signature is, keeps about 75%.
func wantIncompressible(t *testing.T, what string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len()*10 < len(data)*7 {
		t.Errorf("gzip shrinks %s from %d to %d bytes: a worst case must not compress", what, len(data), buf.Len())
	}
}

// payloadOf returns node.json as cloud-init writes it from the user data: the content of its only file, decoded
// through base64 and gzip.
func payloadOf(t *testing.T, userData []byte) []byte {
	t.Helper()
	var cc struct {
		WriteFiles []struct {
			Path     string `json:"path"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		} `json:"write_files"`
	}
	if err := yaml.Unmarshal(userData, &cc); err != nil {
		t.Fatalf("the user data is not YAML: %v", err)
	}
	if len(cc.WriteFiles) != 1 || cc.WriteFiles[0].Path != "/etc/tent/node.json" ||
		cc.WriteFiles[0].Encoding != "gz+b64" {
		t.Fatalf("the user data writes %d files, want /etc/tent/node.json in gz+b64", len(cc.WriteFiles))
	}
	zipped, err := base64.StdEncoding.DecodeString(cc.WriteFiles[0].Content)
	if err != nil {
		t.Fatalf("the payload is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatalf("the payload is not gzip: %v", err)
	}
	node, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	return node
}
