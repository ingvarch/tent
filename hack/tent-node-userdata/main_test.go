package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
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

func TestUserDataHoldsAClientConfigWithTentNodeOnly(t *testing.T) {
	setNode(t, testURL, testSum)
	code, out, errOut := runTool(t, "-name", testName, "-version", testVersion)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	nc := decoded(t, out)

	want := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind, Cluster: "tent-node-check",
		Provider: v1alpha1.ProviderVultr, NodeGroup: "clients", Name: testName, Role: v1alpha1.RoleClient,
		Assets: []nodeconfig.Asset{
			{Name: nodeconfig.TentNodeAsset, Version: testVersion, URLs: []string{testURL}, SHA256: testSum},
		},
		Join: nodeconfig.Join{Strategy: nodeconfig.JoinSeedAndRefresh, RefreshInterval: time.Minute},
		// As tent gives a client whose node group keeps the docker driver.
		System: nodeconfig.System{
			Sysctls: map[string]string{
				"net.bridge.bridge-nf-call-arptables": "1",
				"net.bridge.bridge-nf-call-ip6tables": "1",
				"net.bridge.bridge-nf-call-iptables":  "1",
			},
			KernelModules: []string{"br_netfilter", "overlay"},
			Docker:        true,
		},
		Firewall: nodeconfig.HostFirewall{BlockMetadata: netip.MustParseAddr("169.254.169.254")},
	}
	want.SpecHash = nodeconfig.SpecHash(want)
	// Asset prints its URLs without their query: compare the URLs themselves.
	asset := cmp.Transformer("asset", func(a nodeconfig.Asset) []string {
		return append([]string{a.Name, a.Version, a.SHA256}, a.URLs...)
	})
	if diff := cmp.Diff(want, nc, cmpopts.EquateComparable(netip.Addr{}), asset); diff != "" {
		t.Errorf("node.json (-want +got):\n%s", diff)
	}
	if len(nc.Files) != 0 {
		t.Errorf("node.json holds %d files, want none: no certificate, key or token", len(nc.Files))
	}
	if errOut != "" {
		t.Errorf("stderr is not empty:\n%s", errOut)
	}
}

func TestFlagsWinOverTheEnvironment(t *testing.T) {
	setNode(t, "https://env.example.invalid/tent-node", strings.Repeat("0", 64))
	code, out, errOut := runTool(t, "-name", testName, "-version", testVersion, "-url", testURL, "-sha256", testSum)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	a := decoded(t, out).Assets[0]
	if len(a.URLs) != 1 || a.URLs[0] != testURL || a.SHA256 != testSum {
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
		says                     string
	}{
		{"a name that is no host name", "Tent_Spike", testURL, testSum, "is not a host name"},
		{"a short sha256", testName, testURL, testSum[1:], "is not 64 lower-case hex digits"},
		{"a URL that is not http", testName, "ftp://h/x?X-Amz-Signature=" + testSig, testSum, "URLs[0]"},
		{"a URL with a space", testName, "https://h/x y?X-Amz-Signature=" + testSig, testSum, "printable ASCII"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setNode(t, tc.url, tc.sum)
			code, out, errOut := runTool(t, "-name", tc.nodeName, "-version", testVersion)
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
