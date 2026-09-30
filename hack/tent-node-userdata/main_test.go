package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"net/netip"
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
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// The tent-node that the tests put into the user data. The URL carries a signature, as a presigned URL does.
const (
	testName    = "tent-spike-ab12cd-t"
	testVersion = "v0.1.0-rc.2-18-g4fc8be6-dirty"
	testSig     = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	testURL     = "https://tent-ci.acc.r2.cloudflarestorage.com/dev/tent-node/x/tent-node_linux_amd64?" +
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=" + testSig
)

var testSum = strings.Repeat("ab", 32)

// cloudConfig is the part of the cloud-config that the tests read: the files it writes.
type cloudConfig struct {
	WriteFiles []struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	} `json:"write_files"`
}

// runTool runs the tool with args and returns its exit code and output.
func runTool(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// setNode sets TENT_NODE_URL and TENT_NODE_SHA256 as the upload tool prints them.
func setNode(t *testing.T, nodeURL, sum string) {
	t.Helper()
	t.Setenv("TENT_NODE_URL", nodeURL)
	t.Setenv("TENT_NODE_SHA256", sum)
}

// decoded returns the NodeConfig in user data: the content of /etc/tent/node.json, through base64 and gzip, decoded
// and validated. It fails the test on any error.
func decoded(t *testing.T, userData string) *nodeconfig.NodeConfig {
	t.Helper()
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatalf("the user data does not start with #cloud-config: %.40q", userData)
	}
	var cc cloudConfig
	if err := yaml.Unmarshal([]byte(userData), &cc); err != nil {
		t.Fatalf("the user data is not YAML: %v", err)
	}
	if len(cc.WriteFiles) != 1 || cc.WriteFiles[0].Path != "/etc/tent/node.json" ||
		cc.WriteFiles[0].Encoding != "gz+b64" {
		t.Fatalf("the user data writes %+v, want only /etc/tent/node.json as gz+b64", cc.WriteFiles)
	}
	gz, err := base64.StdEncoding.DecodeString(cc.WriteFiles[0].Content)
	if err != nil {
		t.Fatalf("node.json is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("node.json is not gzip: %v", err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("node.json is not gzip: %v", err)
	}
	nc, err := nodeconfig.Decode(data)
	if err != nil {
		t.Fatalf("node.json does not decode: %v", err)
	}
	return nc
}

// ruleText writes a host firewall rule as the tests expect it: name, protocol, ports and sources.
func ruleText(r nodeconfig.Rule) string {
	from := make([]string, len(r.From))
	for i, p := range r.From {
		from[i] = p.String()
	}
	return fmt.Sprintf("%s %s %d-%d from %s", r.Name, r.Protocol, r.Ports.First, r.Ports.Last, strings.Join(from, " "))
}

// rulesText writes the rules of a host firewall as ruleText does, in their order.
func rulesText(f nodeconfig.HostFirewall) []string {
	texts := make([]string, len(f.Rules))
	for i, r := range f.Rules {
		texts[i] = ruleText(r)
	}
	return texts
}

// The host firewall rules of a node in the VPC 10.64.0.0/16, in the order tent writes them.
const (
	sshRule        = "ssh tcp 22-22 from 0.0.0.0/0 ::/0"
	icmpRule       = "icmp icmp 0-0 from 0.0.0.0/0 ::/0"
	apiRule        = "api tcp 4646-4646 from 0.0.0.0/0 ::/0"
	httpRule       = "nomad-http tcp 4646-4646 from 10.64.0.0/16"
	rpcRule        = "nomad-rpc tcp 4647-4647 from 10.64.0.0/16"
	serfTCP        = "serf tcp 4648-4648 from 10.64.0.0/16"
	serfUDP        = "serf udp 4648-4648 from 10.64.0.0/16"
	dynamicTCP     = "dynamic tcp 20000-32000 from 10.64.0.0/16"
	dynamicUDP     = "dynamic udp 20000-32000 from 10.64.0.0/16"
	bridgeHTTP     = "bridge-http tcp 4646-4646 from 172.26.64.0/20 172.17.0.0/16"
	bridgeDynamicT = "bridge-dynamic tcp 20000-32000 from 172.26.64.0/20 172.17.0.0/16"
	bridgeDynamicU = "bridge-dynamic udp 20000-32000 from 172.26.64.0/20 172.17.0.0/16"
)

// stableCNI returns the CNI plugins for amd64 that the embedded stable channel pins.
func stableCNI(t *testing.T) nodeconfig.Asset {
	t.Helper()
	ch, err := channels.Load("stable")
	if err != nil {
		t.Fatalf("load the stable channel: %v", err)
	}
	a, err := assets.CNI(ch, "amd64")
	if err != nil {
		t.Fatalf("CNI: %v", err)
	}
	return nodeconfig.Asset(a)
}

// TestUserDataByRole checks the NodeConfig of each role, client when -role is not given, in the VPC 10.64.0.0/16 when
// -cidr is not given: the tent-node under test; the CNI plugins, Docker and the bridge settings only on a node that
// runs a client; the host firewall that tent gives the role; and no files, so no secrets.
func TestUserDataByRole(t *testing.T) {
	tentNode := nodeconfig.Asset{Name: nodeconfig.TentNodeAsset, Version: testVersion, URLs: []string{testURL},
		SHA256: testSum}
	withDocker := nodeconfig.System{
		Sysctls: map[string]string{
			"net.bridge.bridge-nf-call-arptables": "1",
			"net.bridge.bridge-nf-call-ip6tables": "1",
			"net.bridge.bridge-nf-call-iptables":  "1",
		},
		KernelModules: []string{"br_netfilter", "overlay"},
		Docker:        true,
	}
	clientRules := []string{
		sshRule, icmpRule, httpRule, dynamicTCP, dynamicUDP, bridgeHTTP, bridgeDynamicT, bridgeDynamicU,
	}
	for _, tc := range []struct {
		name   string
		args   []string
		role   v1alpha1.Role
		assets []nodeconfig.Asset
		system nodeconfig.System
		rules  []string
	}{
		{
			name: "client by default", role: v1alpha1.RoleClient,
			assets: []nodeconfig.Asset{stableCNI(t), tentNode}, system: withDocker, rules: clientRules,
		},
		{
			name: "client", args: []string{"-role", "client"}, role: v1alpha1.RoleClient,
			assets: []nodeconfig.Asset{stableCNI(t), tentNode}, system: withDocker, rules: clientRules,
		},
		{
			name: "server", args: []string{"-role", "server"}, role: v1alpha1.RoleServer,
			assets: []nodeconfig.Asset{tentNode}, system: nodeconfig.System{},
			rules: []string{sshRule, icmpRule, apiRule, httpRule, rpcRule, serfTCP, serfUDP},
		},
		{
			name: "combined", args: []string{"-role", "combined"}, role: v1alpha1.RoleCombined,
			assets: []nodeconfig.Asset{stableCNI(t), tentNode}, system: withDocker,
			rules: []string{
				sshRule, icmpRule, apiRule, httpRule, rpcRule, serfTCP, serfUDP, dynamicTCP, dynamicUDP, bridgeHTTP,
				bridgeDynamicT, bridgeDynamicU,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setNode(t, testURL, testSum)
			code, out, errOut := runTool(t, append([]string{"-name", testName, "-version", testVersion}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if len(out) > nodeconfig.MaxUserDataBytes {
				t.Errorf("the user data is %d bytes, more than %d", len(out), nodeconfig.MaxUserDataBytes)
			}
			nc := decoded(t, out)
			if diff := cmp.Diff(tc.rules, rulesText(nc.Firewall)); diff != "" {
				t.Errorf("host firewall rules (-want +got):\n%s", diff)
			}
			want := &nodeconfig.NodeConfig{
				APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind, Cluster: "tent-node-check",
				Provider: v1alpha1.ProviderVultr, NodeGroup: "nodes", Name: testName, Role: tc.role, Assets: tc.assets,
				Join:   nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
				System: tc.system,
				Firewall: nodeconfig.HostFirewall{
					Rules: nc.Firewall.Rules, BlockMetadata: netip.MustParseAddr("169.254.169.254"),
				},
			}
			// The rules are the ones checked above by their text.
			want.SpecHash = nodeconfig.SpecHash(want)
			// Asset prints its URLs without their query: compare the URLs themselves.
			asset := cmp.Transformer("asset", func(a nodeconfig.Asset) []string {
				return append([]string{a.Name, a.Version, a.SHA256}, a.URLs...)
			})
			equate := cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})
			if diff := cmp.Diff(want, nc, equate, asset); diff != "" {
				t.Errorf("node.json (-want +got):\n%s", diff)
			}
			if len(nc.Files) != 0 {
				t.Errorf("node.json holds %d files, want none: no certificate, key or token", len(nc.Files))
			}
			if errOut != "" {
				t.Errorf("stderr is not empty:\n%s", errOut)
			}
		})
	}
}

// TestCIDRGivesTheRulesBetweenNodes checks that -cidr is the source of the rules between nodes.
func TestCIDRGivesTheRulesBetweenNodes(t *testing.T) {
	setNode(t, testURL, testSum)
	code, out, errOut := runTool(t, "-name", testName, "-version", testVersion, "-cidr", "10.20.0.0/20")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	want := []string{
		sshRule, icmpRule, "nomad-http tcp 4646-4646 from 10.20.0.0/20", "dynamic tcp 20000-32000 from 10.20.0.0/20",
		"dynamic udp 20000-32000 from 10.20.0.0/20", bridgeHTTP, bridgeDynamicT, bridgeDynamicU,
	}
	if diff := cmp.Diff(want, rulesText(decoded(t, out).Firewall)); diff != "" {
		t.Errorf("host firewall rules (-want +got):\n%s", diff)
	}
}

func TestFlagsWinOverTheEnvironment(t *testing.T) {
	setNode(t, "https://env.example.invalid/tent-node", strings.Repeat("0", 64))
	code, out, errOut := runTool(t, "-name", testName, "-version", testVersion, "-url", testURL, "-sha256", testSum)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	nc := decoded(t, out)
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.TentNodeAsset })
	if i < 0 {
		t.Fatalf("node.json has no tent-node asset")
	}
	if a := nc.Assets[i]; len(a.URLs) != 1 || a.URLs[0] != testURL || a.SHA256 != testSum {
		t.Errorf("the tent-node asset has the sha256 %s and %d URLs, want the flags' values", a.SHA256, len(a.URLs))
	}
}

func TestMissingValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url, sum string
		args     []string
		says     string
	}{
		{"no name", testURL, testSum, []string{"-version", testVersion}, "-name"},
		{"no version", testURL, testSum, []string{"-name", testName}, "-version"},
		{"no URL", "", testSum, []string{"-name", testName, "-version", testVersion}, "TENT_NODE_URL"},
		{"no sha256", testURL, "", []string{"-name", testName, "-version", testVersion}, "TENT_NODE_SHA256"},
		{"an argument", testURL, testSum, []string{"-name", testName, "-version", testVersion, "x"}, "no arguments"},
		{"an unknown flag", testURL, testSum, []string{"-cluster", "x"}, "-cluster"},
		{
			"a CIDR without a length", testURL, testSum,
			[]string{"-name", testName, "-version", testVersion, "-cidr", "10.64.0.0"}, "-cidr",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setNode(t, tc.url, tc.sum)
			code, out, errOut := runTool(t, tc.args...)
			if code != exitUsage || out != "" || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", code, out, errOut,
					exitUsage, tc.says)
			}
		})
	}
}

func TestInvalidValuesFail(t *testing.T) {
	for _, tc := range []struct {
		name, nodeName, url, sum string
		args                     []string
		says                     string
	}{
		{"a name that is no host name", "Tent_Spike", testURL, testSum, nil, "is not a host name"},
		{"a short sha256", testName, testURL, testSum[1:], nil, "is not 64 lower-case hex digits"},
		{"a URL that is not http", testName, "ftp://h/x?X-Amz-Signature=" + testSig, testSum, nil, "URLs[0]"},
		{"a URL with a space", testName, "https://h/x y?X-Amz-Signature=" + testSig, testSum, nil, "printable ASCII"},
		{
			"an unknown role", testName, testURL, testSum, []string{"-role", "worker"},
			`role "worker" is not server, client or combined`,
		},
		{
			"a CIDR that is no network", testName, testURL, testSum, []string{"-cidr", "10.64.0.5/16"},
			"10.64.0.5/16 has bits set after /16",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setNode(t, tc.url, tc.sum)
			code, out, errOut := runTool(t, append([]string{"-name", tc.nodeName, "-version", testVersion}, tc.args...)...)
			if code != exitError || out != "" || !strings.Contains(errOut, tc.says) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", code, out, errOut,
					exitError, tc.says)
			}
			if strings.Contains(errOut, testSig) {
				t.Errorf("the error shows the URL's signature:\n%s", errOut)
			}
		})
	}
}

func TestHelpHidesTheURL(t *testing.T) {
	setNode(t, testURL, testSum)
	code, out, errOut := runTool(t, "-h")
	if code != 0 || out != "" || !strings.Contains(errOut, "TENT_NODE_URL") {
		t.Errorf("-h: exit %d, stdout %q, stderr %q; want exit 0 and a usage that names TENT_NODE_URL", code, out,
			errOut)
	}
	if strings.Contains(errOut, testSig) {
		t.Errorf("the usage shows the URL's signature:\n%s", errOut)
	}
}
