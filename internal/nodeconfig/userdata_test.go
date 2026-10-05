package nodeconfig_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/secrettest"
)

// cloudConfig is the cloud-config that UserData writes, as cloud-init reads it.
type cloudConfig struct {
	PackageUpdate  *bool `json:"package_update"`
	PackageUpgrade *bool `json:"package_upgrade"`
	WriteFiles     []struct {
		Path        string `json:"path"`
		Encoding    string `json:"encoding"`
		Owner       string `json:"owner"`
		Permissions string `json:"permissions"`
		Content     string `json:"content"`
	} `json:"write_files"`
	RunCmd [][]string `json:"runcmd"`
}

// userData returns the user data of c and what cloud-init reads from it, and fails the test on an error.
func userData(t *testing.T, c *nodeconfig.NodeConfig) ([]byte, cloudConfig) {
	t.Helper()
	data, err := nodeconfig.UserData(c)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	var cc cloudConfig
	if err := yaml.UnmarshalStrict(data, &cc); err != nil {
		t.Fatalf("the user data is not the cloud-config the test knows: %v", err)
	}
	return data, cc
}

// script returns the shell script that cloud-init runs, and fails the test when runcmd is not one sh -c command.
func script(t *testing.T, cc cloudConfig) string {
	t.Helper()
	if len(cc.RunCmd) != 1 || len(cc.RunCmd[0]) != 3 || cc.RunCmd[0][0] != "/bin/sh" || cc.RunCmd[0][1] != "-c" {
		t.Fatalf("runcmd = %d commands, want one /bin/sh -c <script>", len(cc.RunCmd))
	}
	return cc.RunCmd[0][2]
}

// payload is the line of the user data that holds node.json, which the golden masks: its bytes follow the
// compressor's version.
var payload = regexp.MustCompile(`(?m)^(    content: ).+$`)

// TestUserDataGolden checks the user data of the sample against its golden, with the payload masked. The script
// shows how the URLs are quoted: the development build's URL has "?" and "&".
func TestUserDataGolden(t *testing.T) {
	data, _ := userData(t, sample())
	checkGolden(t, "user-data.yaml.golden", payload.ReplaceAllString(string(data), "${1}[gz+b64 of node.json]"))
}

// TestUserDataPayload checks the cloud-config: package updates off, and one file, /etc/tent/node.json, which only root
// reads and whose content decodes through gzip and base64 to exactly what Encode writes.
func TestUserDataPayload(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		t.Run(string(role), func(t *testing.T) {
			c := sample()
			c.Role = role
			data, cc := userData(t, c)
			if !bytes.HasPrefix(data, []byte("#cloud-config\n")) {
				t.Error("the user data does not start with #cloud-config")
			}
			if cc.PackageUpdate == nil || *cc.PackageUpdate || cc.PackageUpgrade == nil || *cc.PackageUpgrade {
				t.Errorf("package_update, package_upgrade = %v, %v; want false, false",
					cc.PackageUpdate, cc.PackageUpgrade)
			}
			if len(cc.WriteFiles) != 1 {
				t.Fatalf("write_files has %d entries, want 1", len(cc.WriteFiles))
			}
			f := cc.WriteFiles[0]
			if got, want := fmt.Sprintf("%s %s %s %s", f.Path, f.Encoding, f.Owner, f.Permissions),
				"/etc/tent/node.json gz+b64 root:root 0600"; got != want {
				t.Errorf("write_files[0] = %s, want %s", got, want)
			}
			want, err := nodeconfig.Encode(c)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if got := gunzipBase64(t, f.Content); !bytes.Equal(got, want) {
				t.Error("the payload does not decode to what Encode writes")
			}
		})
	}
}

// gunzipBase64 decodes content as cloud-init decodes gz+b64, and fails the test when it cannot.
func gunzipBase64(t *testing.T, content string) []byte {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		t.Fatalf("the payload is not base64: %v", err)
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("the payload is not gzip: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("the payload is not gzip: %v", err)
	}
	return data
}

// TestUserDataScript checks the script that installs tent-node: sh reads it; it makes the directory first, so a
// missing one does not read as a bad download; it gives up on a mirror that stalls or trickles, so the next one gets
// its turn; it checks the tent-node asset's sha256 and tries the asset's URLs in order.
func TestUserDataScript(t *testing.T) {
	c := sample()
	mirror := "https://mirror.example.com/tent-node_linux_amd64"
	c.Assets[2].URLs = append(c.Assets[2].URLs, mirror)
	_, cc := userData(t, c)
	s := script(t, cc)
	mkdir, loop := strings.Index(s, "mkdir -p /usr/local/bin\n"), strings.Index(s, "for url in ")
	if mkdir < 0 || loop < mkdir {
		t.Errorf("the script makes /usr/local/bin at %d and starts the downloads at %d, want the directory first",
			mkdir, loop)
	}
	curl := "curl -fsSL --connect-timeout 10 --speed-limit 1024 --speed-time 30 --max-time 600 --retry 5 " +
		`--retry-all-errors --retry-max-time 600 -o /usr/local/bin/tent-node.download "$url"`
	// sh joins a line that ends in a backslash with the next one.
	if joined := regexp.MustCompile(`\s*\\\n\s*`).ReplaceAllString(s, " "); !strings.Contains(joined, curl) {
		t.Errorf("the script does not download with %s", curl)
	}
	if !strings.Contains(s, "echo '"+c.Assets[2].SHA256+"  /usr/local/bin/tent-node.download' | sha256sum -c -") {
		t.Error("the script does not check the tent-node asset's sha256")
	}
	first, second := strings.Index(s, "'"+presignedURL+"'"), strings.Index(s, "'"+mirror+"'")
	if first < 0 || second < first {
		t.Errorf("the script has the URLs at %d and %d, want both in the asset's order", first, second)
	}
	if !strings.Contains(s, "exec /usr/local/bin/tent-node install --config /etc/tent/node.json\n") {
		t.Error("the script does not run tent-node install")
	}
	sh := shell(t)
	if out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput(); err != nil {
		t.Errorf("sh cannot read the script: %v\n%s", err, out)
	}
}

// shell returns the path of sh, which every Unix has. It skips the test on Windows: the script runs on Linux nodes,
// and the sh of Git for Windows takes its arguments from a Windows command line and may expand globs in them, so a
// result there says nothing about the nodes.
func shell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs under a Unix sh; Windows has none of its own")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to run the script with: %v", err)
	}
	return sh
}

// TestUserDataURLsStayInert runs the script's loop over hostile URLs with sh: each URL reaches curl as one word, as it
// is, and nothing in it runs.
func TestUserDataURLsStayInert(t *testing.T) {
	hostile := []string{
		"https://mirror.example.com/tent-node?a=1&b=2;echo${IFS}INJECTED;#x",
		"https://mirror.example.com/$(echo${IFS}INJECTED)/`echo${IFS}INJECTED`/${HOME}",
		`https://mirror.example.com/'quoted'/"double"/\back|pipe>out<in*glob?one`,
	}
	c := sample()
	c.Assets[2].URLs = hostile
	_, cc := userData(t, c)
	var loop string
	for line := range strings.Lines(script(t, cc)) {
		if strings.HasPrefix(line, "for url in ") {
			loop = line
		}
	}
	if loop == "" {
		t.Fatal("the script has no loop over the URLs")
	}
	out, err := exec.Command(shell(t), "-c", loop+`printf '%s\n' "$url"; done`).CombinedOutput()
	if err != nil {
		t.Fatalf("sh cannot run the loop: %v\n%s", err, out)
	}
	if got, want := string(out), strings.Join(hostile, "\n")+"\n"; got != want {
		t.Errorf("the loop gives the URLs\n%s\nwant\n%s", got, want)
	}
}

func TestUserDataErrors(t *testing.T) {
	type nc = nodeconfig.NodeConfig
	for _, tc := range []struct {
		name string
		edit func(c *nc)
		want string
	}{
		{"invalid", func(c *nc) { c.Kind = "Node" }, `user data: node config: kind "Node" is not NodeConfig`},
		{"no tent-node", func(c *nc) { c.Assets = c.Assets[:2] }, "user data: no tent-node asset"},
		{"URL with a space", func(c *nc) {
			c.Assets[2].URLs = []string{"https://a.example.com/x", "https://b.example.com/x y"}
		},
			"user data: tent-node URLs[1] has a character that is not printable ASCII"},
		{"URL with Unicode", func(c *nc) { c.Assets[2].URLs = []string{"https://a.example.com/tént-node"} },
			"user data: tent-node URLs[0] has a character that is not printable ASCII"},
		// YAML 1.1, which cloud-init reads, ends a line at U+2028.
		{"URL with a line separator", func(c *nc) { c.Assets[2].URLs = []string{"https://a.example.com/x - y"} },
			"user data: tent-node URLs[0] has a character that is not printable ASCII"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sample()
			tc.edit(c)
			data, err := nodeconfig.UserData(c)
			if got := errText(err); got != tc.want {
				t.Errorf("UserData() error = %q, want %q", got, tc.want)
			}
			if data != nil {
				t.Error("UserData() returned user data with the error")
			}
		})
	}
	if _, err := nodeconfig.UserData(nil); errText(err) != "user data: no node config" {
		t.Errorf("UserData(nil) error = %v, want no node config", err)
	}
}

// noise returns n bytes of printable text that compresses as badly as base64 does, the same for the same seed.
func noise(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return []byte(base64.StdEncoding.EncodeToString(raw)[:n])
}

// pem returns a PEM block of the type with a body of n noise bytes, as a certificate or a key of that size looks.
func pem(seed uint64, kind string, n int) []byte {
	body := noise(seed, n)
	var b strings.Builder
	b.WriteString("-----BEGIN " + kind + "-----\n")
	for len(body) > 64 {
		b.Write(body[:64])
		b.WriteByte('\n')
		body = body[64:]
	}
	b.Write(body)
	b.WriteString("\n-----END " + kind + "-----\n")
	return []byte(b.String())
}

// worstCase returns the largest config that tent makes for a node of the role, without the operator's extra
// configuration: nomad.service, two CAs in the bundle during a rotation, a certificate with every name, a 2 KiB intro
// token, tent-node from a short presigned URL, a mirror and a development build's 1.5 KiB presigned URL, a mirror for
// each other asset, 5 seeds, 12 meta keys and the host firewall of every rule.
func worstCase(t *testing.T, role v1alpha1.Role) *nodeconfig.NodeConfig {
	t.Helper()
	a := agents()[role]
	a.ExtraServer, a.ExtraClient = "", ""
	a.Drivers = []string{"docker", "exec", "raw_exec", "java", "qemu", "podman"}
	a.Meta = map[string]string{}
	for i := range 12 {
		a.Meta[fmt.Sprintf("team-%02d.example.owner", i)] = fmt.Sprintf("platform-team-%02d@example.com", i)
	}
	files, err := nodeconfig.RenderAgent(a)
	if err != nil {
		t.Fatalf("RenderAgent: %v", err)
	}
	c := sample()
	c.Role, c.NodeGroup = role, a.Group
	c.Name = "prod-" + a.Group + "-17"
	node, err := nodeconfig.RenderNode(c.Name, "ams", role, 5)
	if err != nil {
		t.Fatalf("RenderNode: %v", err)
	}
	c.Files = files
	c.Files = append(c.Files, node, nodeconfig.RenderNomadService(),
		nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: "root:root",
			Content: append(pem(1, "CERTIFICATE", 600), pem(2, "CERTIFICATE", 600)...)},
		nodeconfig.File{Path: nodeconfig.CertFile, Mode: 0o644, Owner: "root:root", PerNode: true,
			Content: pem(3, "CERTIFICATE", 800)},
		nodeconfig.File{Path: nodeconfig.KeyFile, Mode: 0o600, Owner: "root:root", PerNode: true, Secret: true,
			Content: pem(4, "PRIVATE KEY", 190)},
	)
	if role.RunsClient() {
		c.Files = append(c.Files, nodeconfig.File{Path: nodeconfig.IntroTokenFile, Mode: 0o600,
			Owner: "root:root", PerNode: true, Secret: true, Content: noise(5, 2048)})
	}
	for i := range c.Assets {
		c.Assets[i].URLs = append(c.Assets[i].URLs, "https://mirror.example.com/tent/assets/"+c.Assets[i].Name)
	}
	c.Assets[2].URLs = append(c.Assets[2].URLs,
		"https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Signature="+string(noise(6, 1500)))
	c.Join.Servers = nil
	for i := range 5 {
		c.Join.Servers = append(c.Join.Servers, netip.AddrFrom4([4]byte{10, 64, 0, byte(100 + i)}))
	}
	cidr := []netip.Prefix{netip.MustParsePrefix("10.64.0.0/16")}
	bridges := []netip.Prefix{netip.MustParsePrefix("172.26.64.0/20"), netip.MustParsePrefix("172.17.0.0/16")}
	for _, r := range []struct {
		name        string
		first, last uint16
		from        []netip.Prefix
	}{
		{"nomad-http", 4646, 4646, cidr}, {"nomad-rpc", 4647, 4647, cidr}, {"serf", 4648, 4648, cidr},
		{"dynamic", 20000, 32000, cidr}, {"bridge-http", 4646, 4646, bridges}, {"bridge-dynamic", 20000, 32000, bridges},
	} {
		for _, proto := range []string{nodeconfig.ProtocolTCP, nodeconfig.ProtocolUDP} {
			c.Firewall.Rules = append(c.Firewall.Rules, nodeconfig.Rule{Name: r.name, Protocol: proto,
				Ports: nodeconfig.PortRange{First: r.first, Last: r.last}, From: r.from})
		}
	}
	c.SpecHash = nodeconfig.SpecHash(c)
	return c
}

// extraRoom is the least room that the largest config of a role must leave for the operator's extra configuration.
const extraRoom = 8 << 10

// TestUserDataWorstCases checks that the largest config of each role leaves at least 8 KiB of the user data for the
// operator's extra configuration, and logs the sizes.
func TestUserDataWorstCases(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		c := worstCase(t, role)
		data, _ := userData(t, c)
		encoded, err := nodeconfig.Encode(c)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		left := nodeconfig.MaxUserDataBytes - len(data)
		t.Logf("%s: node.json %d bytes, user data %d bytes, %d of %d left", role, len(encoded), len(data), left,
			nodeconfig.MaxUserDataBytes)
		if left < extraRoom {
			t.Errorf("%s: the user data leaves %d bytes for extra configuration, want at least %d", role, left,
				extraRoom)
		}
	}
}

// oversize matches the error of user data above the limit, and the size it names.
var oversize = regexp.MustCompile(`^user data: node group core needs (\d+) bytes, more than the 24576 that fit$`)

// TestUserDataOversize checks that extra configuration that does not fit fails with the node group and the size, and
// that the error shows no secret.
func TestUserDataOversize(t *testing.T) {
	c := worstCase(t, v1alpha1.RoleCombined)
	c.SpecHash = ""
	c.Files = append(c.Files, nodeconfig.File{Path: "/etc/nomad.d/98-user-server.hcl", Mode: 0o600, Owner: "root:root",
		Content: append([]byte("# "), noise(7, 16<<10)...)})
	data, err := nodeconfig.UserData(c)
	m := oversize.FindStringSubmatch(errText(err))
	if m == nil {
		t.Fatalf("UserData() error = %v, want one that names the node group and the size", err)
	}
	if n, _ := strconv.Atoi(m[1]); n <= nodeconfig.MaxUserDataBytes {
		t.Errorf("the error names %d bytes, which fit", n)
	}
	if data != nil {
		t.Error("UserData() returned user data with the error")
	}
	for name, s := range map[string][]byte{"the gossip key": gossipKey, "the URL's signature": urlSignature} {
		if secrettest.Shows(errText(err), s) {
			t.Errorf("the error shows %s", name)
		}
	}
}

// TestAssetNames checks the names that node.json gives the assets: tent-node finds each asset by its name, and the
// names are part of the NodeConfig contract.
func TestAssetNames(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{nodeconfig.NomadAsset, "nomad"},
		{nodeconfig.CNIPluginsAsset, "cni-plugins"},
		{nodeconfig.TentNodeAsset, "tent-node"},
	} {
		if tc.got != tc.want {
			t.Errorf("an asset is named %q, want %q", tc.got, tc.want)
		}
	}
}

// TestUserDataDeterministic checks that one config always gives the same user data.
func TestUserDataDeterministic(t *testing.T) {
	first, _ := userData(t, sample())
	for range 5 {
		if again, _ := userData(t, sample()); !bytes.Equal(again, first) {
			t.Fatal("UserData gave other bytes for the same config")
		}
	}
}
