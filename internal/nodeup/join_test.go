package nodeup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/pki"
)

// testCA returns a CA of the test cluster, valid from now.
func testCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// tlsFiles writes the CA bundle, the node's certificate and its key of ca into fsys, as the nomad phase's files, and
// returns the certificate.
func tlsFiles(t *testing.T, fsys *nodeuptest.FS, ca *pki.CA, role v1alpha1.Role) pki.Certificate {
	t.Helper()
	cert, err := ca.IssueNode(role, "global", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fsys.AddDir(t, "/etc/nomad.d/tls", 0o755, nodeconfig.Owner)
	fsys.AddFile(t, nodeconfig.CAFile, ca.Bundle(), 0o644, nodeconfig.Owner)
	fsys.AddFile(t, nodeconfig.CertFile, cert.Cert, 0o644, nodeconfig.Owner)
	fsys.AddFile(t, nodeconfig.KeyFile, cert.Key.Bytes(), 0o600, nodeconfig.Owner)
	return cert
}

// Paths that join and the refresh write.
const (
	joinPath  = "/etc/nomad.d/05-join.hcl"
	peersPath = "/var/lib/tent/peers.json"
)

// serveNomad starts a fake Nomad server, a nodeuptest.Agent with a server certificate of ca for the region global,
// whose /v1/status/peers answers peers, or 500 when peers is nil. It requires the node's certificate, which a Nomad
// agent does only with verify_https_client, so that a test proves that the node sends it.
func serveNomad(t *testing.T, ca *pki.CA, peers []string) *nodeuptest.Agent {
	t.Helper()
	cert, err := ca.IssueNode(v1alpha1.RoleServer, "global", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return serveNomadWith(t, ca, cert, peers)
}

// serveNomadWith starts a fake Nomad server as serveNomad does, with the certificate cert of ca.
func serveNomadWith(t *testing.T, ca *pki.CA, cert pki.Certificate, peers []string) *nodeuptest.Agent {
	t.Helper()
	a := nodeuptest.ServeAgent(t, ca.Bundle(), cert.Cert, cert.Key.Bytes())
	a.SetPeers(peers)
	return a
}

// joinFile returns the content of 05-join.hcl on fsys, or "".
func joinFile(t *testing.T, fsys *nodeuptest.FS) string {
	t.Helper()
	e, ok := fsys.Entry(joinPath)
	if !ok {
		return ""
	}
	return string(e.Data)
}

func TestJoinSeedsWithoutTLSFiles(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	logs := captureLog(h)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	h.DialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("join asked a server without TLS files")
		return nil, nil
	}
	res, err := runPhase(t, "join", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Errorf("join = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	// A first boot has no TLS files and no peers file yet: nothing to warn about.
	if got := warnings(logs); len(got) != 0 {
		t.Errorf("a first boot logged warnings: %q", got)
	}
	content := joinFile(t, fsys)
	if !strings.Contains(content, `"10.64.0.5:4648"`) || !strings.HasPrefix(content, nodeconfig.NodeHeader+"server {") {
		t.Errorf("05-join.hcl does not join the seed's gossip port:\n%s", content)
	}
	if e, ok := fsys.Entry(joinPath); !ok || e.Mode != 0o644 {
		t.Errorf("05-join.hcl: found %v, mode %#o; want 0644", ok, e.Mode)
	}
}

func TestJoinRefreshesFromThePeers(t *testing.T) {
	ca := testCA(t)
	// Unsorted and with a duplicate: the answer's order and repeats change nothing.
	srv := serveNomad(t, ca, []string{"10.64.0.9:4647", "10.64.0.5:4647", "10.64.0.9:4647"})
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	dial, open := counting(srv.Dial)
	h.DialContext = dial

	res, err := runPhase(t, "join", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("join = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if n := open.Load(); n != 0 {
		t.Errorf("join left %d connections open, want none", n)
	}
	if got := srv.Dialed(); !cmp.Equal(got, []string{"10.64.0.5:4646"}) {
		t.Errorf("join dialed %q, want 10.64.0.5:4646", got)
	}
	if got := srv.Requests(); !cmp.Equal(got, []string{"/v1/status/peers?stale"}) {
		t.Errorf("the server saw %q, want /v1/status/peers?stale", got)
	}
	content := joinFile(t, fsys)
	for _, want := range []string{`"10.64.0.5:4648"`, `"10.64.0.9:4648"`} {
		if !strings.Contains(content, want) {
			t.Errorf("05-join.hcl has no %s:\n%s", want, content)
		}
	}
	peers, ok := fsys.Entry(peersPath)
	if !ok || peers.Mode != 0o600 {
		t.Fatalf("peers.json: found %v, mode %#o; want 0600", ok, peers.Mode)
	}
	if dir, ok := fsys.Entry("/var/lib/tent"); !ok || dir.Mode != 0o700 {
		t.Errorf("/var/lib/tent: found %v, mode %#o; want 0700", ok, dir.Mode)
	}
	var stored []string
	if err := json.Unmarshal(peers.Data, &stored); err != nil ||
		!cmp.Equal(stored, []string{"10.64.0.5", "10.64.0.9"}) {
		t.Errorf("peers.json = %q, %v; want the two addresses", peers.Data, err)
	}

	// A second run with the same answer changes nothing.
	changes := len(fsys.Changes())
	res, err = runPhase(t, "join", h, nc, nil)
	if err != nil || res.Status != nodeup.Unchanged {
		t.Errorf("the second join = %s %q, %v; want unchanged", res.Status, res.Reason, err)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second join changed %q, want nothing", got)
	}
}

func TestJoinFallsBackToTheSeed(t *testing.T) {
	ca := testCA(t)
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	h.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") }

	res, err := runPhase(t, "join", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Errorf("join = %s %q, %v; want done (the seed is rendered anyway)", res.Status, res.Reason, err)
	}
	if content := joinFile(t, fsys); !strings.Contains(content, `"10.64.0.5:4648"`) {
		t.Errorf("05-join.hcl does not join the seed:\n%s", content)
	}
}

// TestJoinFallsBackOnATLSFailure checks that a server whose certificate fails the handshake, such as another
// cluster's, is not joined, and that the logged error shows no URL.
func TestJoinFallsBackOnATLSFailure(t *testing.T) {
	other := testCA(t) // another cluster's CA
	srv := serveNomad(t, other, []string{"10.64.0.5:4647"})
	ca := testCA(t)
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	var logs bytes.Buffer
	h.Log = slog.New(slog.NewTextHandler(&logs, nil))
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	h.DialContext = srv.Dial

	res, err := runPhase(t, "join", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Errorf("join = %s %q, %v; want done (the seed is rendered anyway)", res.Status, res.Reason, err)
	}
	if content := joinFile(t, fsys); !strings.Contains(content, `"10.64.0.5:4648"`) {
		t.Errorf("05-join.hcl does not join the seed:\n%s", content)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Errorf("the other cluster's server saw %q, want no answered request", got)
	}
	if out := logs.String(); strings.Contains(out, "http") || strings.Contains(out, "/v1/") {
		t.Errorf("the log shows a URL's internals:\n%s", out)
	}
}

// TestJoinKeepsTheLastKnownPeers checks that with every server down the node keeps the last known set beside the
// seed, the last known first: a replaced server group leaves the seed dead.
func TestJoinKeepsTheLastKnownPeers(t *testing.T) {
	ca := testCA(t)
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
	fsys.AddFile(t, peersPath, []byte(`["10.64.0.9"]`), 0o600, nodeconfig.Owner)
	h.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no route") }

	if _, err := runPhase(t, "join", h, nc, nil); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	content := joinFile(t, fsys)
	if !strings.Contains(content, `retry_join = ["10.64.0.9:4648", "10.64.0.5:4648"]`) {
		t.Errorf("05-join.hcl does not join the last known peer first, then the seed:\n%s", content)
	}
}

// TestJoinIgnoresAnEmptyAnswer checks that a server that answers with no peers, as one does before the bootstrap, does
// not erase the known servers.
func TestJoinIgnoresAnEmptyAnswer(t *testing.T) {
	ca := testCA(t)
	srv := serveNomad(t, ca, []string{})
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
	fsys.AddFile(t, peersPath, []byte(`["10.64.0.9"]`), 0o600, nodeconfig.Owner)
	h.DialContext = srv.Dial

	if _, err := runPhase(t, "join", h, nc, nil); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	content := joinFile(t, fsys)
	if !strings.Contains(content, `"10.64.0.9:4648"`) {
		t.Errorf("05-join.hcl lost the last known peer to an empty answer:\n%s", content)
	}
	if e, _ := fsys.Entry(peersPath); string(e.Data) != `["10.64.0.9"]` {
		t.Errorf("peers.json = %q, want the last known peer untouched", e.Data)
	}
}

// TestJoinIPv6 checks a seed and an answer with IPv6 addresses.
func TestJoinIPv6(t *testing.T) {
	ca := testCA(t)
	srv := serveNomad(t, ca, []string{"[fd00::1]:4647"})
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("fd00::5")}
	h.DialContext = srv.Dial

	if _, err := runPhase(t, "join", h, nc, nil); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	if got := srv.Dialed(); !cmp.Equal(got, []string{"[fd00::5]:4646"}) {
		t.Errorf("join dialed %q, want [fd00::5]:4646", got)
	}
	if content := joinFile(t, fsys); !strings.Contains(content, `"[fd00::1]:4648"`) {
		t.Errorf("05-join.hcl does not join the IPv6 peer:\n%s", content)
	}
}

func TestJoinAsksTheLastKnownPeersFirst(t *testing.T) {
	ca := testCA(t)
	srv := serveNomad(t, ca, []string{"10.64.0.9:4647"})
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t)
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
	fsys.AddFile(t, peersPath, []byte(`["10.64.0.9"]`), 0o600, nodeconfig.Owner)
	h.DialContext = srv.Dial

	if _, err := runPhase(t, "join", h, nc, nil); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	if got := srv.Dialed(); len(got) == 0 || got[0] != "10.64.0.9:4646" {
		t.Errorf("join dialed %q, want the last known peer 10.64.0.9:4646 first", got)
	}
}

func TestJoinIgnoresACorruptPeersFile(t *testing.T) {
	for name, data := range map[string]string{
		"cut short":     `["10.64.0`,
		"one bad entry": `["bad-entry", "10.64.0.9"]`,
	} {
		t.Run(name, func(t *testing.T) {
			h, fsys, _, _ := ubuntu(t)
			logs := captureLog(h)
			nc := combined(t)
			nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
			fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
			fsys.AddFile(t, peersPath, []byte(data), 0o600, nodeconfig.Owner)
			if _, err := runPhase(t, "join", h, nc, nil); err != nil {
				t.Fatalf("join failed on a corrupt peers file: %v", err)
			}
			content := joinFile(t, fsys)
			if !strings.Contains(content, `"10.64.0.5:4648"`) {
				t.Errorf("05-join.hcl does not join the seed:\n%s", content)
			}
			// A bad entry is left out; the good one joins.
			if name == "one bad entry" && !strings.Contains(content, `"10.64.0.9:4648"`) {
				t.Errorf("05-join.hcl left out the file's good entry:\n%s", content)
			}
			// Either way a warning names the file.
			if got := warnings(logs); len(got) != 1 || !strings.Contains(got[0], " file=/var/lib/tent/peers.json") {
				t.Errorf("warnings %q, want one that names the peers file", got)
			}
		})
	}
}

func TestJoinClient(t *testing.T) {
	ca := testCA(t)
	srv := serveNomad(t, ca, []string{"10.64.0.5:4647"})
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleClient)
	nc := combined(t)
	nc.Role = v1alpha1.RoleClient
	nc.Join.Servers = []netip.Addr{netip.MustParseAddr("10.64.0.5")}
	h.DialContext = srv.Dial

	if _, err := runPhase(t, "join", h, nc, nil); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	content := joinFile(t, fsys)
	if !strings.HasPrefix(content, nodeconfig.NodeHeader+"client {") || !strings.Contains(content, `"10.64.0.5:4647"`) {
		t.Errorf("a client does not join the RPC port in its client block:\n%s", content)
	}
}
