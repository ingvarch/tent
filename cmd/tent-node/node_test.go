package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/env/vultr"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
)

// Where the user data puts tent-node and its config.
const (
	configPath = "/etc/tent/node.json"
	binaryPath = "/usr/local/bin/tent-node"
)

// Stand-ins for the secrets of a node. They are not real keys, but the tests treat them as secrets.
var (
	gossipKey    = []byte(base64.StdEncoding.EncodeToString([]byte("secret-gossip-key-for-tent-node-tests")))
	nodeKey      = []byte("node-key-stand-in-5b1e8d3f9a2c4076\n")
	urlSignature = []byte("4e7a2c9d1f5b8e3a6c0d2f7b9e1a4c8d3f6b0e2a5c9d7f1b3e8a6c4d2f0b9e7a")
)

// secrets are the secrets of nodeConfig, by name.
var secrets = map[string][]byte{
	"the gossip key": gossipKey, "the node's key": nodeKey, "the URL's signature": urlSignature,
}

// nodeConfig returns a valid NodeConfig of prod-core-0, a combined node on Vultr with Docker that runs tent-node
// nodeuptest.Version from a presigned URL, with the node's key and the gossip key.
func nodeConfig(t *testing.T) *nodeconfig.NodeConfig {
	t.Helper()
	nc := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion, Kind: nodeconfig.Kind,
		Cluster: "prod", Provider: v1alpha1.ProviderVultr, NodeGroup: "core", Name: "prod-core-0",
		Role: v1alpha1.RoleCombined, Region: "global",
		Assets: []nodeconfig.Asset{{
			Name: nodeconfig.TentNodeAsset, Version: nodeuptest.Version,
			URLs: []string{"https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Expires=900&X-Amz-Signature=" +
				string(urlSignature)},
			SHA256: "3c7d1e9a5b2f8046c1e7a3d9b5f20864e1c7a3d9b5f20864e1c7a3d9b5f20864",
		}},
		Files: []nodeconfig.File{
			{Path: "/etc/nomad.d/01-gossip.hcl", Mode: 0o600, Owner: nodeconfig.Owner, Secret: true,
				Content: []byte("server {\n  encrypt = \"" + string(gossipKey) + "\"\n}\n")},
			{Path: nodeconfig.KeyFile, Mode: 0o600, Owner: nodeconfig.Owner, PerNode: true, Secret: true,
				Content: nodeKey},
			nodeconfig.RenderNomadService(),
		},
		Join: nodeconfig.Join{
			Strategy: nodeconfig.JoinSeedAndRefresh, Servers: []netip.Addr{netip.MustParseAddr("10.64.0.5")},
			RefreshInterval: time.Minute,
		},
		System: nodeconfig.System{
			Sysctls:       map[string]string{"net.bridge.bridge-nf-call-iptables": "1"},
			KernelModules: []string{"br_netfilter", "overlay"},
			Docker:        true,
		},
		Firewall: nodeconfig.HostFirewall{BlockMetadata: netip.MustParseAddr("169.254.169.254")},
	}
	nc.SpecHash = nodeconfig.SpecHash(nc)
	return nc
}

// machine is a fake Ubuntu machine named prod-core-0 on Vultr, and what the commands asked of it.
type machine struct {
	host    *nodeup.Host
	fs      *nodeuptest.FS
	runner  *nodeuptest.Runner
	env     *nodeuptest.Environment
	asked   []v1alpha1.Provider // the providers whose metadata service the commands asked for
	held    []progress          // the progress when the commands took the lock and when they left it, in turn
	lockErr error               // what taking the lock answers, when it fails
	// lockAfter is how long another process holds the lock: taking it waits that long, or until its context ends.
	lockAfter time.Duration
}

// progress is how many commands had run and how many changes the filesystem had recorded.
type progress struct{ commands, changes int }

// progress returns the machine's progress now.
func (m *machine) progress() progress { return progress{len(m.runner.Commands()), len(m.fs.Changes())} }

// newMachine returns a fake machine with nc, encoded, at configPath.
func newMachine(t *testing.T, nc *nodeconfig.NodeConfig) *machine {
	t.Helper()
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	nodeuptest.Ubuntu(t, fsys, r, "tent-node.service", "tent-node-join.service", "tent-node-join.timer", "nomad.service")
	data, err := nodeconfig.Encode(nc)
	if err != nil {
		t.Fatal(err)
	}
	fsys.AddFile(t, configPath, data, 0o600, nodeconfig.Owner)
	return &machine{
		host: nodeuptest.NewHost("prod-core-0", fsys, r), fs: fsys, runner: r,
		env: &nodeuptest.Environment{Instance: env.Instance{
			ID: "9d1c6f3e-2b7a-4e58-8c0d-5a4f1e7b3c92", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.7"),
		}},
	}
}

// deps returns the deps that give the commands the machine, its metadata service for vultr, and binaryPath.
func (m *machine) deps() deps {
	return deps{
		host: func(log *slog.Logger) (*nodeup.Host, error) {
			m.host.Log = log
			return m.host, nil
		},
		environment: func(p v1alpha1.Provider) (env.Environment, error) {
			m.asked = append(m.asked, p)
			if p != v1alpha1.ProviderVultr {
				return nil, errors.New("tent-node does not support provider " + string(p) + " yet")
			}
			return m.env, nil
		},
		executable: func() (string, error) { return binaryPath, nil },
		lock: func(ctx context.Context) (func(), error) {
			if m.lockErr != nil {
				return nil, m.lockErr
			}
			select {
			case <-time.After(m.lockAfter):
			case <-ctx.Done(): // as nodeup.Lock says it
				return nil, fmt.Errorf("lock %s: %w", nodeup.LockPath, context.Cause(ctx))
			}
			m.held = append(m.held, m.progress())
			return func() { m.held = append(m.held, m.progress()) }, nil
		},
	}
}

// run runs tent-node with args on the machine, and returns its exit code, stdout and stderr.
func (m *machine) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return m.runWith(t.Context(), args...)
}

// runWith runs tent-node with args on the machine under ctx, and returns its exit code, stdout and stderr.
func (m *machine) runWith(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(ctx, args, &out, &errOut, m.deps())
	return code, out.String(), errOut.String()
}

// mustRun runs tent-node with args on the machine, and fails t unless it exits with 0 and prints nothing on stdout.
func (m *machine) mustRun(t *testing.T, args ...string) (stderr string) {
	t.Helper()
	code, stdout, stderr := m.run(t, args...)
	if code != 0 || stdout != "" {
		t.Fatalf("tent-node %q: code %d, stdout %q, stderr:\n%s", args, code, stdout, stderr)
	}
	return stderr
}

func TestInstallCommand(t *testing.T) {
	m := newMachine(t, nodeConfig(t))
	m.mustRun(t, "install", "--config", configPath)
	service, _ := m.fs.Entry("/etc/systemd/system/tent-node.service")
	if want := "\nExecStart=" + binaryPath + " up --config " + configPath + "\n"; !strings.Contains(string(service.Data),
		want) {
		t.Errorf("tent-node.service does not run %q:\n%s", want, service.Data)
	}
	timer, _ := m.fs.Entry("/etc/systemd/system/tent-node-join.timer")
	if !strings.Contains(string(timer.Data), "\nOnUnitActiveSec=60s\n") {
		t.Errorf("tent-node-join.timer does not run every minute, the config's refresh interval:\n%s", timer.Data)
	}
	needsReload := []string{
		"systemctl show -p NeedDaemonReload --value tent-node.service",
		"systemctl show -p NeedDaemonReload --value tent-node-join.service",
		"systemctl show -p NeedDaemonReload --value tent-node-join.timer",
	}
	// systemd reads a new unit when asked about it, so it needs no reload.
	want := append(slices.Clone(needsReload),
		"systemctl is-enabled tent-node.service", "systemctl enable tent-node.service",
		"systemctl is-enabled tent-node-join.timer", "systemctl enable tent-node-join.timer",
		"systemctl is-active tent-node.service", "systemctl start tent-node.service",
		"systemctl is-active tent-node-join.timer", "systemctl start tent-node-join.timer",
	)
	if diff := cmp.Diff(want, m.runner.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	if len(m.asked) != 0 {
		t.Errorf("install asked for the metadata service of %q, want none", m.asked)
	}

	changes, commands := len(m.fs.Changes()), len(m.runner.Commands())
	m.mustRun(t, "install", "--config", configPath)
	if got := m.fs.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second install changed %q, want nothing", got)
	}
	again := append(slices.Clone(needsReload),
		"systemctl is-enabled tent-node.service", "systemctl is-enabled tent-node-join.timer",
		"systemctl is-active tent-node.service", "systemctl is-active tent-node-join.timer",
	)
	if diff := cmp.Diff(again, m.runner.Commands()[commands:]); diff != "" {
		t.Errorf("the second install's commands, which must only read (-want +got):\n%s", diff)
	}
}

func TestInstallCommandTakesTheConfigsPath(t *testing.T) {
	nc := nodeConfig(t)
	m := newMachine(t, nc)
	data, err := nodeconfig.Encode(nc)
	if err != nil {
		t.Fatal(err)
	}
	m.fs.AddFile(t, "/etc/tent/other.json", data, 0o600, nodeconfig.Owner)
	m.mustRun(t, "install", "-config", "/etc/tent/other.json")
	join, _ := m.fs.Entry("/etc/systemd/system/tent-node-join.service")
	if want := "\nExecStart=" + binaryPath + " refresh-join --config /etc/tent/other.json\n"; !strings.Contains(
		string(join.Data), want) {
		t.Errorf("tent-node-join.service does not run %q:\n%s", want, join.Data)
	}
}

// cniPath is where serveAssets serves the archive of the CNI plugins.
const cniPath = "/cni-plugins-linux-amd64-v1.9.1.tgz"

// serveAssets serves the archive of the CNI plugins and the Nomad zip over HTTPS, and adds them to nc as its
// assets.
func serveAssets(t *testing.T, nc *nodeconfig.NodeConfig) *nodeuptest.Server {
	t.Helper()
	archive := nodeuptest.Tgz(t, nodeuptest.TarFile{
		Header: tar.Header{Name: "./bridge", Mode: 0o755}, Content: []byte("bridge plugin\n"),
	})
	zip := nodeuptest.NomadZip(t, []byte("the nomad binary of the tests\n"))
	srv := nodeuptest.Serve(t, map[string]http.HandlerFunc{
		cniPath: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) },
		zipPath: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(zip) },
	})
	sumCNI, sumZip := sha256.Sum256(archive), sha256.Sum256(zip)
	nc.Assets = append(nc.Assets,
		nodeconfig.Asset{
			Name: nodeconfig.CNIPluginsAsset, Version: "1.9.1", URLs: []string{srv.URL + cniPath},
			SHA256: hex.EncodeToString(sumCNI[:]),
		},
		nodeconfig.Asset{
			Name: nodeconfig.NomadAsset, Version: "2.0.7", URLs: []string{srv.URL + zipPath},
			SHA256: hex.EncodeToString(sumZip[:]),
		},
	)
	nc.SpecHash = nodeconfig.SpecHash(nc)
	return srv
}

// serveAgent starts a fake Nomad agent, a nodeuptest.Agent, of a new cluster CA, and gives nc the CA bundle, the
// node's certificate and its key of that CA, as withNodeTLS does; the agent serves with that certificate, as Nomad
// does. Until SetPeers, it answers 500 to /v1/status/peers, so join keeps the seed.
func serveAgent(t *testing.T, nc *nodeconfig.NodeConfig) *nodeuptest.Agent {
	t.Helper()
	ca, node := withNodeTLS(t, nc)
	return nodeuptest.ServeAgent(t, ca.Bundle(), node.Cert, node.Key.Bytes())
}

// withNodeTLS gives nc the CA bundle, the node's certificate and its key of a new cluster CA, and returns the CA and
// the certificate.
func withNodeTLS(t *testing.T, nc *nodeconfig.NodeConfig) (*pki.CA, pki.Certificate) {
	t.Helper()
	ca, err := pki.NewCA(nc.Cluster, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	node, err := ca.IssueNode(nc.Role, nc.Region, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	nc.Files = slices.DeleteFunc(nc.Files, func(f nodeconfig.File) bool { return f.Path == nodeconfig.KeyFile })
	nc.Files = append(nc.Files,
		nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: nodeconfig.Owner, Content: ca.Bundle()},
		nodeconfig.File{
			Path: nodeconfig.CertFile, Mode: 0o644, Owner: nodeconfig.Owner, Content: node.Cert, PerNode: true,
		},
		nodeconfig.File{
			Path: nodeconfig.KeyFile, Mode: 0o600, Owner: nodeconfig.Owner, Content: node.Key.Bytes(), PerNode: true,
			Secret: true,
		},
	)
	nc.SpecHash = nodeconfig.SpecHash(nc)
	return ca, node
}

// zipPath is where serveAssets serves the Nomad zip.
const zipPath = "/nomad_2.0.7_linux_amd64.zip"

func TestUpCommand(t *testing.T) {
	nc := nodeConfig(t)
	srv := serveAssets(t, nc)
	agent := serveAgent(t, nc)
	m := newMachine(t, nc)
	m.host.Transport = srv.Client().Transport
	m.host.DialContext = agent.Dial
	// A clock that moves, as a real one does, so that the status changes on the second run.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	m.host.Now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	m.mustRun(t, "install")
	// The fake systemd does not run up when install starts tent-node.service, so it runs here, as systemd would.
	stderr := m.mustRun(t, "up", "--config", configPath)
	if !strings.Contains(stderr, "level=INFO") {
		t.Errorf("up logged nothing on stderr:\n%s", stderr)
	}
	if !cmp.Equal(m.asked, []v1alpha1.Provider{v1alpha1.ProviderVultr}) {
		t.Errorf("up asked for the metadata service of %q, want vultr", m.asked)
	}
	var report nodeup.Report
	status, _ := m.fs.Entry(nodeup.StatusFile)
	if err := json.Unmarshal(status.Data, &report); err != nil {
		t.Fatalf("status.json: %v", err)
	}
	var phases []string
	for _, p := range report.Phases {
		phases = append(phases, p.Name+" "+string(p.Status))
	}
	want := []string{
		"preflight unchanged", "system done", "hostfirewall done", "runtime done", "cni done",
		"join done", "nomad done", "verify unchanged",
	}
	if diff := cmp.Diff(want, phases); diff != "" {
		t.Errorf("phases in status.json (-want +got):\n%s", diff)
	}

	changes, commands := len(m.fs.Changes()), len(m.runner.Commands())
	m.mustRun(t, "up")
	if diff := cmp.Diff([]string{nodeup.StatusFile}, m.fs.Changes()[changes:]); diff != "" {
		t.Errorf("the second up changed files (-want +got):\n%s", diff)
	}
	reads := []string{
		"timedatectl show -p CanNTP -p NTP",
		"systemctl is-active firewalld.service", "systemctl is-enabled firewalld.service",
		"nft -j list tables", "systemctl is-enabled ufw.service",
		"dpkg-query -W -f=${Status} docker.io", "systemctl is-enabled docker.service",
		"systemctl is-active docker.service",
		"systemctl show -p NeedDaemonReload --value nomad.service", "systemctl is-active nomad.service",
		"systemctl is-enabled tent-node.service", "systemctl is-enabled tent-node-join.timer",
	}
	if diff := cmp.Diff(reads, m.runner.Commands()[commands:]); diff != "" {
		t.Errorf("the second up's commands, which must only read (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{cniPath, zipPath}, srv.Requests()); diff != "" {
		t.Errorf("the two runs' downloads (-want +got):\n%s", diff)
	}
	// Each up checks the agent's health; the second one's join asks it too, once the node's certificate is there.
	checks := []string{"/v1/status/leader?stale", "/v1/agent/health?type=client"}
	asked := slices.Concat(checks, []string{"/v1/status/peers?stale"}, checks)
	if diff := cmp.Diff(asked, agent.Requests()); diff != "" {
		t.Errorf("the agent saw (-want +got):\n%s", diff)
	}
}

func TestUpCommandFails(t *testing.T) {
	m := newMachine(t, nodeConfig(t))
	m.host.HostName = "prod-core-9"
	code, stdout, stderr := m.run(t, "up")
	want := "Error: phase preflight: the host name is prod-core-9, not prod-core-0: the node config is another node's\n"
	if code != 1 || stdout != "" || !strings.HasSuffix(stderr, want) {
		t.Errorf("tent-node up: code %d, stdout %q, stderr:\n%s\nwant 1 and stderr ending with %q", code, stdout,
			stderr, want)
	}
	if _, ok := m.fs.Entry(nodeup.StatusFile); !ok {
		t.Error("a failed up wrote no status")
	}
}

func TestUpCommandRefusesAnUnsupportedProvider(t *testing.T) {
	nc := nodeConfig(t)
	nc.Provider = v1alpha1.ProviderHetzner
	m := newMachine(t, nc)
	code, stdout, stderr := m.run(t, "up")
	if want := "Error: tent-node does not support provider hetzner yet\n"; code != 1 || stdout != "" || stderr != want {
		t.Errorf("tent-node up: code %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if len(m.runner.Commands()) != 0 || len(m.fs.Changes()) != 0 {
		t.Errorf("up ran %q and changed %q, want nothing", m.runner.Commands(), m.fs.Changes())
	}
}

// TestUpTakesTheLock checks that up runs under the lock that serializes it with a refresh-join run by hand, and that
// a lock that cannot be taken fails the command before anything runs.
func TestUpTakesTheLock(t *testing.T) {
	nc := nodeConfig(t)
	srv := serveAssets(t, nc)
	agent := serveAgent(t, nc)
	m := newMachine(t, nc)
	m.host.Transport = srv.Client().Transport
	m.host.DialContext = agent.Dial
	m.mustRun(t, "install")
	before := m.progress()
	m.mustRun(t, "up", "--config", configPath)
	// Every command and change of up, status.json included, comes while it holds the lock.
	if want := []progress{before, m.progress()}; !slices.Equal(m.held, want) {
		t.Errorf("up held the lock from and to %+v, want %+v: from before its first command to after its last change",
			m.held, want)
	}

	stuck := newMachine(t, nodeConfig(t))
	stuck.lockErr = errors.New("the lock is stuck")
	code, _, stderr := stuck.run(t, "up")
	if code != 1 || !strings.Contains(stderr, "the lock is stuck") {
		t.Errorf("tent-node up with a stuck lock: code %d, stderr %q; want 1 and the lock's error", code, stderr)
	}
	if len(stuck.runner.Commands()) != 0 {
		t.Errorf("up with a stuck lock ran %q, want nothing", stuck.runner.Commands())
	}
}

func TestEnvironmentByProvider(t *testing.T) {
	e, err := environment(v1alpha1.ProviderVultr)
	if _, ok := e.(*vultr.Environment); !ok || err != nil {
		t.Errorf("environment(vultr) = %T, %v; want Vultr's", e, err)
	}
	for _, p := range []v1alpha1.Provider{v1alpha1.ProviderHetzner, "aws"} {
		e, err := environment(p)
		if want := "tent-node does not support provider " + string(p) + " yet"; e != nil || errText(err) != want {
			t.Errorf("environment(%s) = %v, %q; want nothing and %q", p, e, errText(err), want)
		}
	}
}

// addFiles puts nc's files onto the machine, as up writes them, with no change recorded.
func (m *machine) addFiles(t *testing.T, nc *nodeconfig.NodeConfig) {
	t.Helper()
	for _, f := range nc.Files {
		m.fs.AddFile(t, f.Path, f.Content, fs.FileMode(f.Mode), f.Owner)
	}
}

// TestRefreshJoinCommand checks that refresh-join rewrites 05-join.hcl once for a new peer set, under the lock, and
// never touches Nomad.
func TestRefreshJoinCommand(t *testing.T) {
	nc := nodeConfig(t)
	agent := serveAgent(t, nc)
	agent.SetPeers([]string{"10.64.0.9:4647", "10.64.0.5:4647"})
	m := newMachine(t, nc)
	m.host.DialContext = agent.Dial
	m.addFiles(t, nc)

	before := m.progress()
	stderr := m.mustRun(t, "refresh-join", "--config", configPath)
	want := `level=INFO msg="05-join.hcl joins the servers that answered" known=1 servers=2 peers="[10.64.0.5 10.64.0.9]"`
	if !strings.HasSuffix(stderr, want+"\n") || strings.Count(stderr, "\n") != 1 {
		t.Errorf("refresh-join logged:\n%s\nwant one line that ends with %s", stderr, want)
	}
	wantChanges := []string{"/etc/nomad.d/05-join.hcl", "/var/lib/tent", "/var/lib/tent/peers.json"}
	if diff := cmp.Diff(wantChanges, m.fs.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	// Every change comes while it holds the lock.
	if want := []progress{before, m.progress()}; !slices.Equal(m.held, want) {
		t.Errorf("refresh-join held the lock from and to %+v, want %+v", m.held, want)
	}

	// The same answer changes nothing and logs nothing.
	changes := len(m.fs.Changes())
	if stderr := m.mustRun(t, "refresh-join"); stderr != "" {
		t.Errorf("the second refresh-join logged:\n%s", stderr)
	}
	if got := m.fs.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second refresh-join changed %q, want nothing", got)
	}
	// Nomad reads 05-join.hcl at its next start: refresh-join runs no command at all.
	if got := m.runner.Commands(); len(got) != 0 {
		t.Errorf("refresh-join ran %q, want nothing", got)
	}
	if got := agent.Requests(); !cmp.Equal(got, []string{"/v1/status/peers?stale", "/v1/status/peers?stale"}) {
		t.Errorf("the agent saw %q, want the peers asked twice", got)
	}
}

// TestRefreshJoinCommandWithoutAnAnswer checks that refresh-join exits with 0 and changes nothing when it hears from no
// server: the timer tries again in a minute.
func TestRefreshJoinCommandWithoutAnAnswer(t *testing.T) {
	cases := []struct {
		name  string
		files bool // whether up has written the node's files, the TLS files among them
		want  string
	}{
		{"no TLS files", false, `level=INFO msg="no TLS files yet; 05-join.hcl stays until the next refresh"`},
		// A combined node asks its own agent and the seed.
		{"no server answers", true, `level=INFO msg="no server answered; 05-join.hcl stays until the next refresh" ` +
			"asked=2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nc := nodeConfig(t)
			agent := serveAgent(t, nc)
			m := newMachine(t, nc)
			m.host.DialContext = agent.Dial
			if c.files {
				m.addFiles(t, nc)
			}
			stderr := m.mustRun(t, "refresh-join")
			if !strings.Contains(stderr, c.want+"\n") {
				t.Errorf("refresh-join logged:\n%s\nwant a line that ends with %s", stderr, c.want)
			}
			if len(m.runner.Commands()) != 0 || len(m.fs.Changes()) != 0 {
				t.Errorf("refresh-join ran %q and changed %q, want nothing", m.runner.Commands(), m.fs.Changes())
			}
			if !c.files && len(agent.Requests()) != 0 {
				t.Errorf("refresh-join without TLS files asked %q", agent.Requests())
			}
		})
	}
}

// TestRefreshJoinWaitsForTheLock checks that refresh-join waits for the lock while another tent-node run holds it, and
// gives up a minute before systemd would stop it, with exit status 0: the next refresh tries again.
func TestRefreshJoinWaitsForTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMachine(t, nodeConfig(t))
		m.lockAfter = time.Hour
		start := time.Now()
		stderr := m.mustRun(t, "refresh-join")
		if waited := time.Since(start); waited != 4*time.Minute {
			t.Errorf("refresh-join gave up after %s, want 4m0s", waited)
		}
		want := `level=INFO msg="another tent-node run holds the lock; the next refresh tries again" waited=4m0s` + "\n"
		if !strings.HasSuffix(stderr, want) || strings.Count(stderr, "\n") != 1 {
			t.Errorf("refresh-join logged:\n%s\nwant one line that ends with %s", stderr, want)
		}
		if len(m.runner.Commands()) != 0 || len(m.fs.Changes()) != 0 || len(m.held) != 0 {
			t.Errorf("refresh-join ran %q, changed %q and held the lock %v; want nothing", m.runner.Commands(),
				m.fs.Changes(), m.held)
		}
	})
}

// TestRefreshJoinCommandFailsOnTheLock checks that a lock that fails for another reason than the end of its wait
// fails refresh-join.
func TestRefreshJoinCommandFailsOnTheLock(t *testing.T) {
	m := newMachine(t, nodeConfig(t))
	m.lockErr = errors.New("lock /run/tent-node.lock: permission denied")
	code, stdout, stderr := m.run(t, "refresh-join")
	if want := "Error: lock /run/tent-node.lock: permission denied\n"; code != 1 || stdout != "" || stderr != want {
		t.Errorf("tent-node refresh-join: code %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
}

// hangingMachine returns a machine with the node's TLS files and a peers file of n servers that answer nothing, as
// servers behind a broken network do: each call gives up after 5 seconds.
func hangingMachine(t *testing.T, n int) *machine {
	t.Helper()
	nc := nodeConfig(t)
	withNodeTLS(t, nc)
	m := newMachine(t, nc)
	m.addFiles(t, nc)
	peers := make([]string, n)
	for i := range peers {
		peers[i] = netip.AddrFrom4([4]byte{10, 64, 1, byte(i + 1)}).String()
	}
	data, err := json.Marshal(peers)
	if err != nil {
		t.Fatal(err)
	}
	m.fs.AddFile(t, "/var/lib/tent/peers.json", data, 0o600, nodeconfig.Owner)
	m.host.DialContext = nodeuptest.Hang
	return m
}

// TestRefreshJoinCommandEnds checks how refresh-join ends when it cannot finish: with 0 and one line when its own
// deadline ends the run, 15 seconds before systemd would stop it, as the next refresh tries again; and with 1 when
// systemd stops it.
func TestRefreshJoinCommandEnds(t *testing.T) {
	cases := []struct {
		name      string
		lockAfter time.Duration // how long another run holds the lock
		sigterm   time.Duration // when SIGTERM ends the command's context; 0 for never
		code      int
		after     time.Duration
		warnings  int    // the servers that did not answer in time; the others are not asked
		last      string // the end of stderr's last line
	}{
		{"its deadline while servers hang", 2 * time.Second, 0, 0, 4*time.Minute + 45*time.Second, 56,
			`level=INFO msg="the refresh ran out of time; the next refresh tries again" within=4m45s`},
		// Both paths say why, as signal.NotifyContext in main words it.
		{"SIGTERM while servers hang", 2 * time.Second, time.Minute, 1, time.Minute, 11,
			"Error: refresh 05-join.hcl: terminated signal received"},
		{"SIGTERM while it waits for the lock", time.Hour, time.Minute, 1, time.Minute, 0,
			"Error: lock /run/tent-node.lock: terminated signal received"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The node's own agent and 60 known servers answer nothing: 61 calls of 5 seconds.
				m := hangingMachine(t, 60)
				m.lockAfter = c.lockAfter
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if c.sigterm > 0 {
					time.AfterFunc(c.sigterm, func() { cancel(nodeuptest.Terminated) })
				}
				start := time.Now()
				code, stdout, stderr := m.runWith(ctx, "refresh-join")
				if waited := time.Since(start); code != c.code || stdout != "" || waited != c.after {
					t.Errorf("refresh-join: code %d, stdout %q after %s; want %d after %s", code, stdout, waited, c.code,
						c.after)
				}
				lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
				if !strings.HasSuffix(lines[len(lines)-1], c.last) {
					t.Errorf("refresh-join's last line is %q, want one that ends with %q", lines[len(lines)-1], c.last)
				}
				// Only the end through the deadline logs, as the end of the lock's wait does.
				want := 0
				if c.code == 0 {
					want = 1
				}
				if infos := strings.Count(stderr, " level=INFO "); infos != want {
					t.Errorf("refresh-join logged %d INFO lines, want %d:\n%s", infos, want, stderr)
				}
				if got := strings.Count(stderr, " level=WARN "); got != c.warnings {
					t.Errorf("refresh-join logged %d warnings, want %d", got, c.warnings)
				}
				if len(m.fs.Changes()) != 0 || len(m.runner.Commands()) != 0 {
					t.Errorf("refresh-join changed %q and ran %q, want nothing", m.fs.Changes(), m.runner.Commands())
				}
			})
		})
	}
}

// slowStat is a filesystem whose Stat of path takes d, as on a disk that hangs, and then fails with err.
type slowStat struct {
	nodeup.FS
	path string
	d    time.Duration
	err  error
}

func (s slowStat) Stat(p string) (fs.FileInfo, error) {
	if p == s.path {
		time.Sleep(s.d)
		return nil, s.err
	}
	return s.FS.Stat(p)
}

// TestRefreshJoinCommandFailsPastItsDeadline checks that an error of the refresh fails refresh-join even when it comes
// after the deadline: only the deadline's own end leaves the work to the next refresh.
func TestRefreshJoinCommandFailsPastItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMachine(t, nodeConfig(t))
		m.host.FS = slowStat{m.fs, nodeconfig.CAFile, 5 * time.Minute, errors.New("input/output error")}
		code, stdout, stderr := m.run(t, "refresh-join")
		want := "Error: refresh 05-join.hcl: stat " + nodeconfig.CAFile + ": input/output error\n"
		if code != 1 || stdout != "" || stderr != want {
			t.Errorf("tent-node refresh-join: code %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr,
				want)
		}
	})
}

func TestNodeCommandsReportABadConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, m *machine)
		want string
	}{
		{"missing", func(t *testing.T, m *machine) {
			if _, err := m.fs.Remove(configPath); err != nil {
				t.Fatal(err)
			}
		}, "Error: read the node config: open /etc/tent/node.json: file does not exist\n"},
		{"not JSON", func(t *testing.T, m *machine) {
			m.fs.AddFile(t, configPath, []byte("#cloud-config\n"), 0o600, nodeconfig.Owner)
		}, "Error: decode node config: invalid character '#' looking for beginning of value\n"},
		{"invalid", func(t *testing.T, m *machine) {
			nc := nodeConfig(t)
			data, err := nodeconfig.Encode(nc)
			if err != nil {
				t.Fatal(err)
			}
			m.fs.AddFile(t, configPath, bytes.Replace(data, []byte(`"combined"`), []byte(`"worker"`), 1), 0o600,
				nodeconfig.Owner)
		}, "Error: node config: role \"worker\" is not server, client or combined\n"},
	}
	for _, c := range cases {
		for _, command := range []string{"install", "up", "refresh-join"} {
			t.Run(c.name+" "+command, func(t *testing.T) {
				m := newMachine(t, nodeConfig(t))
				c.edit(t, m)
				changes := len(m.fs.Changes())
				code, stdout, stderr := m.run(t, command)
				if code != 1 || stdout != "" || stderr != c.want {
					t.Errorf("tent-node %s: code %d, stdout %q, stderr %q; want 1 and %q", command, code, stdout,
						stderr, c.want)
				}
				// The config fails before the lock: nothing waits on a node whose config is broken.
				if len(m.runner.Commands()) != 0 || len(m.fs.Changes()) != changes || len(m.held) != 0 {
					t.Errorf("%s ran %q, changed %q and took the lock %v; want nothing", command, m.runner.Commands(),
						m.fs.Changes()[changes:], m.held)
				}
			})
		}
	}
}

func TestNodeCommandUsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		says string
	}{
		{[]string{"install", "extra"}, "tent-node install takes no arguments, only flags"},
		{[]string{"up", "-frobnicate"}, "flag provided but not defined: -frobnicate; run tent-node up -h for its flags"},
		{[]string{"refresh-join", "--config"}, "flag needs an argument: -config"},
		{[]string{"up", "--config", "etc/tent/node.json"}, `--config: path "etc/tent/node.json" is not absolute and clean`},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			m := newMachine(t, nodeConfig(t))
			code, stdout, stderr := m.run(t, c.args...)
			if want := "Error: " + c.says; code != 2 || stdout != "" || !strings.HasPrefix(stderr, want) {
				t.Errorf("tent-node %q: code %d, stdout %q, stderr %q; want 2 and an error that starts with %q", c.args,
					code, stdout, stderr, want)
			}
			if len(m.runner.Commands()) != 0 || len(m.fs.Changes()) != 0 {
				t.Errorf("tent-node %q ran %q and changed %q, want nothing", c.args, m.runner.Commands(),
					m.fs.Changes())
			}
		})
	}
}

func TestNodeCommandHelp(t *testing.T) {
	for _, command := range []string{"install", "up", "refresh-join"} {
		m := newMachine(t, nodeConfig(t))
		code, stdout, stderr := m.run(t, command, "-h")
		if code != 0 || stdout != "" || !strings.Contains(stderr, "Usage of tent-node "+command+":") ||
			!strings.Contains(stderr, "-config path") || !strings.Contains(stderr, configPath) {
			t.Errorf("tent-node %s -h: code %d, stdout %q, stderr %q; want 0 and the usage with --config", command,
				code, stdout, stderr)
		}
		if len(m.runner.Commands()) != 0 {
			t.Errorf("tent-node %s -h ran %q", command, m.runner.Commands())
		}
	}
}

func TestNodeCommandsShowNoSecrets(t *testing.T) {
	m := newMachine(t, nodeConfig(t))
	var outputs = map[string]string{}
	record := func(name string, args ...string) {
		code, stdout, stderr := m.run(t, args...)
		outputs[name] = stdout + stderr
		if code == 2 {
			t.Errorf("tent-node %q: a usage error:\n%s", args, stderr)
		}
	}
	record("install", "install")
	record("up", "up")
	record("refresh-join", "refresh-join")
	// A config that decodes but does not validate, and a phase that fails.
	data, _ := m.fs.ReadFile(configPath)
	m.fs.AddFile(t, "/etc/tent/bad.json", bytes.Replace(data, []byte(`"mode": 384`), []byte(`"mode": 420`), 1), 0o600,
		nodeconfig.Owner)
	record("an invalid config", "up", "--config", "/etc/tent/bad.json")
	m.fs.Fail("/etc/modules-load.d/tent.conf", errors.New("read-only file system"))
	record("a failed up", "up")
	for name, out := range outputs {
		if out == "" {
			t.Errorf("%s printed nothing", name)
		}
	}
	secrettest.CheckHidden(t, outputs, secrets, "")
}

func TestUsageListsTheCommands(t *testing.T) {
	code, _, stderr := runArgs(t, "-h")
	for _, c := range []string{"install", "up", "refresh-join", "version"} {
		if code != 0 || !strings.Contains(stderr, "\n  "+c+" ") {
			t.Errorf("the usage does not list %s:\n%s", c, stderr)
		}
	}
}

func TestUpOnThisMachineWithoutAConfig(t *testing.T) {
	// The deps of main read the machine itself; a config that is not there stops up before it touches anything.
	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"up", "--config", "/nonexistent/tent-node-test/node.json"}, &out, &errOut,
		machineDeps())
	if code != 1 || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "Error: read the node config: ") {
		t.Errorf("tent-node up with a missing config: code %d, stdout %q, stderr %q; want 1 and a read error", code,
			out.String(), errOut.String())
	}
}

// errText returns the error's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The commands are in the order the usage lists them.
func TestCommandsOrder(t *testing.T) {
	var names []string
	for _, c := range commands {
		names = append(names, c.name)
	}
	if want := []string{"install", "up", "refresh-join", "version"}; !slices.Equal(names, want) {
		t.Errorf("commands %q, want %q", names, want)
	}
}
