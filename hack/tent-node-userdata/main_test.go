package main

import (
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
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

// testNow is when the tests run the tool: HashiCorp's release key is valid then.
var testNow = assetstest.Now()

// The Nomad that the stable channel recommends, and its zip's sha256 in HashiCorp's signed SHA256SUMS.
const (
	nomadVersion = assetstest.NomadVersion
	nomadDir     = "https://releases.hashicorp.com/nomad/" + nomadVersion + "/"
	nomadSum     = assetstest.NomadSHA256
)

// noReleases answers every request with 404.
type noReleases struct{}

func (noReleases) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Header:     http.Header{},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// runTool runs the tool with args at testNow, with HashiCorp's release files, and returns its exit code and output.
func runTool(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runWith(t, assetstest.New(), args...)
}

// runWith is runTool with the release files that rt serves.
func runWith(t *testing.T, rt http.RoundTripper, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, &out, &errOut, &http.Client{Transport: rt}, testNow)
	return code, out.String(), errOut.String()
}

// setNode sets TENT_NODE_URL and TENT_NODE_SHA256 as the upload tool prints them.
func setNode(t *testing.T, nodeURL, sum string) {
	t.Helper()
	t.Setenv("TENT_NODE_URL", nodeURL)
	t.Setenv("TENT_NODE_SHA256", sum)
}

// cloudConfig is the part of the cloud-config that the tests read: the files it writes.
type cloudConfig struct {
	WriteFiles []struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	} `json:"write_files"`
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
		t.Fatalf("the user data writes %d files, want only /etc/tent/node.json as gz+b64", len(cc.WriteFiles))
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

// withoutPayload returns the user data without the line that carries node.json.
func withoutPayload(userData string) string {
	lines := strings.Split(userData, "\n")
	return strings.Join(slices.DeleteFunc(lines, func(l string) bool {
		return strings.HasPrefix(strings.TrimSpace(l), "content: ")
	}), "\n")
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

// fileView is what a test compares of a file: everything but its content.
type fileView struct {
	Path            string
	Mode            uint32
	Owner           string
	PerNode, Secret bool
}

func viewOf(files []nodeconfig.File) []fileView {
	views := make([]fileView, len(files))
	for i, f := range files {
		views[i] = fileView{Path: f.Path, Mode: f.Mode, Owner: f.Owner, PerNode: f.PerNode, Secret: f.Secret}
	}
	return views
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

// combinedRules are the host firewall rules of a combined node in a cluster whose private network is cidr, in the
// order tent writes them.
func combinedRules(cidr string) []string {
	const bridges = "from 172.26.64.0/20 172.17.0.0/16"
	return []string{
		"ssh tcp 22-22 from 0.0.0.0/0 ::/0",
		"icmp icmp 0-0 from 0.0.0.0/0 ::/0",
		"api tcp 4646-4646 from 0.0.0.0/0 ::/0",
		"nomad-http tcp 4646-4646 from " + cidr,
		"nomad-rpc tcp 4647-4647 from " + cidr,
		"serf tcp 4648-4648 from " + cidr,
		"serf udp 4648-4648 from " + cidr,
		"dynamic tcp 20000-32000 from " + cidr,
		"dynamic udp 20000-32000 from " + cidr,
		"bridge-http tcp 4646-4646 " + bridges,
		"bridge-dynamic tcp 20000-32000 " + bridges,
		"bridge-dynamic udp 20000-32000 " + bridges,
	}
}

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

// gossipPattern finds the gossip key in 01-gossip.hcl.
var gossipPattern = regexp.MustCompile(`encrypt = "([^"]+)"`)

// gossipKey returns the gossip key in the files, and fails the test when there is none.
func gossipKey(t *testing.T, files []nodeconfig.File) pki.Secret {
	t.Helper()
	m := gossipPattern.FindSubmatch(fileAt(t, files, "/etc/nomad.d/01-gossip.hcl").Content)
	if m == nil {
		t.Fatal("01-gossip.hcl holds no encrypt key")
	}
	return pki.Secret(m[1])
}

// pemBlocks returns the PEM blocks in data and fails the test when anything else is there.
func pemBlocks(t *testing.T, path string, data []byte) []*pem.Block {
	t.Helper()
	var blocks []*pem.Block
	for rest := data; len(bytes.TrimSpace(rest)) > 0; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			t.Fatalf("%s holds something other than PEM blocks", path)
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// checkPKI checks the TLS files: the CA bundle holds certificates alone; the node's certificate is the CA's, for both
// roles of a combined node in the region global and for localhost; its key is the only private key that the node
// gets, so the CA's key stays out; and the gossip key is one that tent makes.
func checkPKI(t *testing.T, files []nodeconfig.File) {
	t.Helper()
	keys := 0
	for _, f := range files {
		if bytes.Contains(f.Content, []byte("PRIVATE KEY")) {
			keys++
		}
	}
	if keys != 1 {
		t.Errorf("%d files hold a private key, want only the node's", keys)
	}
	roots := x509.NewCertPool()
	for _, b := range pemBlocks(t, "ca.pem", fileAt(t, files, "/etc/nomad.d/tls/ca.pem").Content) {
		if b.Type != "CERTIFICATE" {
			t.Fatalf("ca.pem holds a %s", b.Type)
		}
		ca, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatalf("ca.pem: %v", err)
		}
		roots.AddCert(ca)
	}
	certs := pemBlocks(t, "agent.pem", fileAt(t, files, "/etc/nomad.d/tls/agent.pem").Content)
	if len(certs) != 1 || certs[0].Type != "CERTIFICATE" {
		t.Fatalf("agent.pem holds %d blocks, want one certificate", len(certs))
	}
	cert, err := x509.ParseCertificate(certs[0].Bytes)
	if err != nil {
		t.Fatalf("agent.pem: %v", err)
	}
	for _, name := range []string{"server.global.nomad", "client.global.nomad", "localhost"} {
		_, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, CurrentTime: testNow,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
		if err != nil {
			t.Errorf("agent.pem for %s: %v", name, err)
		}
	}
	keyBlocks := pemBlocks(t, "agent-key.pem", fileAt(t, files, "/etc/nomad.d/tls/agent-key.pem").Content)
	if len(keyBlocks) != 1 || keyBlocks[0].Type != "PRIVATE KEY" {
		t.Fatalf("agent-key.pem holds %d blocks, want one private key", len(keyBlocks))
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlocks[0].Bytes)
	if err != nil {
		t.Fatalf("agent-key.pem: %v", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		t.Fatalf("agent-key.pem holds a %T, which signs nothing", key)
	}
	if pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(cert.PublicKey) {
		t.Error("agent-key.pem is not the key of agent.pem")
	}
	if err := pki.CheckGossipKey(gossipKey(t, files)); err != nil {
		t.Errorf("01-gossip.hcl: %v", err)
	}
}

// TestUserData checks the user data of the node, in the VPC 10.64.0.0/16 and the zone ams when -cidr and -zone are
// not given: the NodeConfig that tent gives the combined node of a cluster of one node, with the stable channel's Nomad
// and CNI plugins and the tent-node under test, the system settings and the host firewall, and every file: the agent
// configuration, the unit, and a new CA bundle, certificate, key and gossip key. Nothing but the user data shows a
// secret.
func TestUserData(t *testing.T) {
	withDocker := nodeconfig.System{
		Sysctls: map[string]string{
			"net.bridge.bridge-nf-call-arptables": "1",
			"net.bridge.bridge-nf-call-ip6tables": "1",
			"net.bridge.bridge-nf-call-iptables":  "1",
		},
		KernelModules: []string{"br_netfilter", "overlay"},
		Docker:        true,
	}
	nomad := nodeconfig.Asset{Name: "nomad", Version: nomadVersion,
		URLs: []string{nomadDir + "nomad_" + nomadVersion + "_linux_amd64.zip"}, SHA256: nomadSum}
	tentNode := nodeconfig.Asset{Name: "tent-node", Version: testVersion, URLs: []string{testURL}, SHA256: testSum}
	// The files in the order tent writes them: the group's, then the node's. No intro token: servers take any client.
	wantFiles := []fileView{
		{Path: "/etc/nomad.d/00-tent.hcl", Mode: 0o644, Owner: "root:root"},
		{Path: "/etc/nomad.d/01-gossip.hcl", Mode: 0o600, Owner: "root:root", Secret: true},
		{Path: "/etc/nomad.d/tls/ca.pem", Mode: 0o644, Owner: "root:root"},
		{Path: "/etc/systemd/system/nomad.service", Mode: 0o644, Owner: "root:root"},
		{Path: "/etc/nomad.d/10-node.hcl", Mode: 0o644, Owner: "root:root", PerNode: true},
		{Path: "/etc/nomad.d/tls/agent.pem", Mode: 0o644, Owner: "root:root", PerNode: true},
		{Path: "/etc/nomad.d/tls/agent-key.pem", Mode: 0o600, Owner: "root:root", PerNode: true, Secret: true},
	}
	for _, tc := range []struct {
		name       string
		args       []string
		cidr, zone string
	}{
		{"defaults", nil, "10.64.0.0/16", "ams"},
		{"a CIDR and a zone", []string{"-cidr", "10.20.0.0/20", "-zone", "fra"}, "10.20.0.0/20", "fra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setNode(t, testURL, testSum)
			code, out, errOut := runTool(t, append([]string{"-name", testName, "-version", testVersion}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if errOut != "" {
				t.Errorf("stderr is not empty:\n%s", errOut)
			}
			if len(out) > nodeconfig.MaxUserDataBytes {
				t.Errorf("the user data is %d bytes, more than %d", len(out), nodeconfig.MaxUserDataBytes)
			}
			nc := decoded(t, out)
			if diff := cmp.Diff(combinedRules(tc.cidr), rulesText(nc.Firewall)); diff != "" {
				t.Errorf("host firewall rules (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantFiles, viewOf(nc.Files)); diff != "" {
				t.Errorf("files (-want +got):\n%s", diff)
			}
			gossip := gossipKey(t, nc.Files)
			agent, err := nodeconfig.RenderAgent(nodeconfig.Agent{
				Role: v1alpha1.RoleCombined, Cluster: "tent-node-check", Group: "nodes", Region: "global",
				CIDR: netip.MustParsePrefix(tc.cidr), ClientIntroduction: v1alpha1.ClientIntroductionWarn,
				Gossip: gossip, VerifyHTTPSClient: true, NodePool: "default",
				DynamicPorts: nodeconfig.PortRange{First: 20000, Last: 32000},
			})
			if err != nil {
				t.Fatalf("RenderAgent: %v", err)
			}
			node, err := nodeconfig.RenderNode(testName, tc.zone, v1alpha1.RoleCombined, 1)
			if err != nil {
				t.Fatalf("RenderNode: %v", err)
			}
			// They hold no secret, so a failure may show them.
			for _, f := range []nodeconfig.File{agent[0], nodeconfig.RenderNomadService(), node} {
				if diff := cmp.Diff(string(f.Content), string(fileAt(t, nc.Files, f.Path).Content)); diff != "" {
					t.Errorf("%s (-want +got):\n%s", f.Path, diff)
				}
			}
			if !bytes.Equal(agent[1].Content, fileAt(t, nc.Files, agent[1].Path).Content) {
				t.Errorf("%s is not the one that tent renders for the gossip key", agent[1].Path)
			}
			checkPKI(t, nc.Files)
			secrettest.CheckHidden(t, map[string]string{"stdout without node.json": withoutPayload(out),
				"stderr": errOut}, map[string][]byte{
				"the gossip key": gossip, "the node key": fileAt(t, nc.Files, "/etc/nomad.d/tls/agent-key.pem").Content,
			}, "")

			if nc.SpecHash == "" {
				t.Error("node.json has no spec hash")
			}
			want := &nodeconfig.NodeConfig{
				APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind, Cluster: "tent-node-check",
				Provider: v1alpha1.ProviderVultr, NodeGroup: "nodes", Name: testName, Role: v1alpha1.RoleCombined,
				Region: "global", Assets: []nodeconfig.Asset{nomad, stableCNI(t), tentNode},
				Join:   nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
				System: withDocker,
				// The rules and the files are the ones checked above.
				Firewall: nodeconfig.HostFirewall{
					Rules: nc.Firewall.Rules, BlockMetadata: netip.MustParseAddr("169.254.169.254"),
				},
				Files: nc.Files, SpecHash: nc.SpecHash,
			}
			// Asset prints its URLs without their query: compare the URLs themselves.
			asset := cmp.Transformer("asset", func(a nodeconfig.Asset) []string {
				return append([]string{a.Name, a.Version, a.SHA256}, a.URLs...)
			})
			file := cmp.Transformer("file", func(f nodeconfig.File) string { return f.Path })
			equate := cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})
			if diff := cmp.Diff(want, nc, equate, asset, file); diff != "" {
				t.Errorf("node.json (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEachRunMakesNewSecrets checks that every run makes its own CA, node key and gossip key.
func TestEachRunMakesNewSecrets(t *testing.T) {
	setNode(t, testURL, testSum)
	var runs [2][]nodeconfig.File
	for i := range runs {
		code, out, errOut := runTool(t, "-name", testName, "-version", testVersion)
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		runs[i] = decoded(t, out).Files
	}
	for _, path := range []string{"/etc/nomad.d/01-gossip.hcl", "/etc/nomad.d/tls/ca.pem",
		"/etc/nomad.d/tls/agent-key.pem"} {
		if bytes.Equal(fileAt(t, runs[0], path).Content, fileAt(t, runs[1], path).Content) {
			t.Errorf("two runs give the same %s", path)
		}
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
		// A release's nodes take the release's tent-node and leave the one under test.
		{
			"a release", testURL, testSum, []string{"-name", testName, "-version", "v0.3.0"},
			"-version v0.3.0 is a release",
		},
		{
			"a pre-release", testURL, testSum, []string{"-name", testName, "-version", "v0.3.0-rc.1"},
			"-version v0.3.0-rc.1 is a release",
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
		{"a short sha256", testName, testURL, testSum[1:], nil, "want 64 lower-case hex digits"},
		{"a URL that is not http", testName, "ftp://h/x?X-Amz-Signature=" + testSig, testSum, nil, "URLs[0]"},
		{"a URL with a space", testName, "https://h/x y?X-Amz-Signature=" + testSig, testSum, nil, "printable ASCII"},
		{
			"a CIDR that is no network", testName, testURL, testSum, []string{"-cidr", "10.64.0.5/16"},
			"spec.networking.cidr: must be a network address: 10.64.0.5 has bits set after /16",
		},
		{
			"a public CIDR", testName, testURL, testSum, []string{"-cidr", "8.8.0.0/16"},
			"spec.networking.cidr: must be a private range",
		},
		{"a zone that is no region", testName, testURL, testSum, []string{"-zone", "Ams"}, "spec.cloud.region"},
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

// TestNoNomadRelease checks that the tool fails when Nomad's signed release files are not there.
func TestNoNomadRelease(t *testing.T) {
	setNode(t, testURL, testSum)
	code, out, errOut := runWith(t, noReleases{}, "-name", testName, "-version", testVersion)
	const want = "find Nomad " + nomadVersion + ": get " + nomadDir + "nomad_" + nomadVersion + "_SHA256SUMS: 404 Not Found"
	if code != exitError || out != "" || !strings.Contains(errOut, want) {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and an error that says %q", code, out, errOut, exitError,
			want)
	}
	if strings.Contains(errOut, testSig) {
		t.Errorf("the error shows the URL's signature:\n%s", errOut)
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
