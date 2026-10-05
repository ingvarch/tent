package nodeup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// seedJoin puts 05-join.hcl for nc's seed onto fsys, as the join phase leaves it on a first boot. It records no
// change.
func seedJoin(t *testing.T, fsys *nodeuptest.FS, nc *nodeconfig.NodeConfig) {
	t.Helper()
	f, err := nodeconfig.RenderJoin(nc.Role, nc.Join.Servers)
	if err != nil {
		t.Fatal(err)
	}
	fsys.AddFile(t, f.Path, f.Content, fs.FileMode(f.Mode), f.Owner)
}

// infos returns the lines of logs at the level INFO.
func infos(logs *bytes.Buffer) []string { return logLines(logs, "INFO") }

// TestRefreshJoin checks that a refresh rewrites 05-join.hcl and the peers file once for each new peer set, logs one
// line for it, and never touches Nomad.
func TestRefreshJoin(t *testing.T) {
	ca := testCA(t)
	h, fsys, r, _ := ubuntu(t)
	logs := captureLog(h)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	nc := combined(t) // the seed is 10.64.0.5
	seedJoin(t, fsys, nc)
	two := serveNomad(t, ca, []string{"10.64.0.9:4647", "10.64.0.5:4647"})
	h.DialContext = two.Dial

	refresh := func(when string, wantChanges []string, wantInfo string) {
		t.Helper()
		changes, lines := len(fsys.Changes()), len(infos(logs))
		if err := nodeup.RefreshJoin(t.Context(), h, nc); err != nil {
			t.Fatalf("RefreshJoin %s: %v", when, err)
		}
		if diff := cmp.Diff(wantChanges, fsys.Changes()[changes:], cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("the changes %s (-want +got):\n%s", when, diff)
		}
		got := infos(logs)[lines:]
		switch {
		case wantInfo == "" && len(got) != 0:
			t.Errorf("RefreshJoin %s logged %q, want nothing", when, got)
		case wantInfo != "" && (len(got) != 1 || !strings.HasSuffix(got[0], wantInfo+"\n")):
			t.Errorf("RefreshJoin %s logged %q, want one line that ends with %q", when, got, wantInfo)
		}
	}

	refresh("of the first answer", []string{joinPath, "/var/lib/tent", peersPath},
		`msg="05-join.hcl joins the servers that answered" known=1 servers=2 peers="[10.64.0.5 10.64.0.9]"`)
	for _, want := range []string{`"10.64.0.5:4648"`, `"10.64.0.9:4648"`} {
		if !strings.Contains(joinFile(t, fsys), want) {
			t.Errorf("05-join.hcl has no %s:\n%s", want, joinFile(t, fsys))
		}
	}
	refresh("of the same answer", nil, "")

	three := serveNomad(t, ca, []string{"10.64.0.5:4647", "10.64.0.6:4647", "10.64.0.9:4647"})
	h.DialContext = three.Dial
	refresh("of a new answer", []string{joinPath, peersPath},
		`msg="05-join.hcl joins the servers that answered" known=2 servers=3 peers="[10.64.0.5 10.64.0.6 10.64.0.9]"`)

	// Nomad reads the file at its next start: the refresh never restarts it, nor marks a restart.
	if got := r.Commands(); len(got) != 0 {
		t.Errorf("RefreshJoin ran %q, want nothing", got)
	}
	if _, ok := fsys.Entry(restartFile); ok {
		t.Error("RefreshJoin marked a restart of Nomad")
	}
}

// asClient makes nc a client's config, with the seed and the system settings that it has, and gives it its hash.
func asClient(t *testing.T, nc *nodeconfig.NodeConfig) {
	t.Helper()
	nc.Role = v1alpha1.RoleClient
	rehash(t, nc)
}

// noDial is a dialer that fails t: the refresh must ask nobody.
func noDial(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) {
		t.Error("RefreshJoin dialed")
		return nil, errors.New("no dial")
	}
}

// TestRefreshJoinWithoutAnAnswer checks that a refresh that hears from no server changes nothing and passes, and says
// why: the next refresh tries again.
func TestRefreshJoinWithoutAnAnswer(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, nc *nodeconfig.NodeConfig)
		info  string // the one INFO line, or "" for none
		warn  string // the start of a WARN line, or "" for none
	}{
		{"no TLS files", func(t *testing.T, h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig) {
			h.DialContext = noDial(t)
		}, `msg="no TLS files yet; 05-join.hcl stays until the next refresh"`, ""},
		{"no server is known", func(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, nc *nodeconfig.NodeConfig) {
			tlsFiles(t, fsys, testCA(t), v1alpha1.RoleClient)
			nc.Join.Servers = nil
			asClient(t, nc)
			h.DialContext = noDial(t)
		}, `msg="no server is known; 05-join.hcl stays until the next refresh"`, ""},
		{"the TLS files do not load", func(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, _ *nodeconfig.NodeConfig) {
			tlsFiles(t, fsys, testCA(t), v1alpha1.RoleCombined)
			fsys.AddFile(t, nodeconfig.CertFile, []byte("not a certificate\n"), 0o644, nodeconfig.Owner)
			h.DialContext = noDial(t)
		}, "", `msg="no mTLS client of the Nomad API; the known servers stay"`},
		{"no server answers", func(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, _ *nodeconfig.NodeConfig) {
			ca := testCA(t)
			tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
			h.DialContext = serveNomad(t, ca, nil).Dial
		}, `msg="no server answered; 05-join.hcl stays until the next refresh" asked=2`,
			`msg="a server did not answer"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _ := ubuntu(t)
			logs := captureLog(h)
			nc := combined(t)
			seedJoin(t, fsys, nc)
			c.setup(t, h, fsys, nc)
			if err := nodeup.RefreshJoin(t.Context(), h, nc); err != nil {
				t.Fatalf("RefreshJoin: %v", err)
			}
			if got := fsys.Changes(); len(got) != 0 {
				t.Errorf("RefreshJoin changed %q, want nothing", got)
			}
			if got := r.Commands(); len(got) != 0 {
				t.Errorf("RefreshJoin ran %q, want nothing", got)
			}
			got := infos(logs)
			switch {
			case c.info == "" && len(got) != 0:
				t.Errorf("RefreshJoin logged %q, want no INFO line", got)
			case c.info != "" && (len(got) != 1 || !strings.HasSuffix(got[0], c.info+"\n")):
				t.Errorf("RefreshJoin logged %q, want one line that ends with %q", got, c.info)
			}
			if w := warnings(logs); c.warn != "" && (len(w) == 0 || !strings.Contains(w[0], c.warn)) {
				t.Errorf("RefreshJoin warned %q, want %q", w, c.warn)
			}
		})
	}
}

// TestRefreshJoinOnAServerAsksItsOwnAgent checks that a server with no seed and no peers file, as the cluster's first
// server is for its whole life, learns the servers from its own agent.
func TestRefreshJoinOnAServerAsksItsOwnAgent(t *testing.T) {
	ca := testCA(t)
	h, fsys, _, _ := ubuntu(t)
	logs := captureLog(h)
	nc := server(t)
	nc.Join.Servers = nil
	rehash(t, nc)
	// The agent serves with the node's certificate, which covers server.global.nomad.
	srv := serveNomadWith(t, ca, tlsFiles(t, fsys, ca, v1alpha1.RoleServer), []string{"10.64.0.7:4647"})
	h.DialContext = srv.Dial
	if err := nodeup.RefreshJoin(t.Context(), h, nc); err != nil {
		t.Fatalf("RefreshJoin: %v", err)
	}
	if got := srv.Dialed(); !cmp.Equal(got, []string{"127.0.0.1:4646"}) {
		t.Errorf("RefreshJoin dialed %q, want the node's own agent", got)
	}
	if content := joinFile(t, fsys); !strings.Contains(content, `retry_join = ["10.64.0.7:4648"]`) {
		t.Errorf("05-join.hcl does not join the agent's peer:\n%s", content)
	}
	// The answer's addresses are stored, not the address that was asked.
	if e, _ := fsys.Entry(peersPath); string(e.Data) != `["10.64.0.7"]` {
		t.Errorf("peers.json = %q, want the answer's address", e.Data)
	}
	want := `msg="05-join.hcl joins the servers that answered" known=0 servers=1 peers=[10.64.0.7]` + "\n"
	if got := infos(logs); len(got) != 1 || !strings.HasSuffix(got[0], want) {
		t.Errorf("RefreshJoin logged %q, want one line that ends with %q", got, want)
	}
}

// TestRefreshJoinAsks checks whom a refresh asks, in order: on a role that runs a server the node's own agent, then
// the last known servers and the seed; on a client the known servers alone.
func TestRefreshJoinAsks(t *testing.T) {
	known := []string{"10.64.0.9:4646", "10.64.0.5:4646"}
	for role, want := range map[v1alpha1.Role][]string{
		v1alpha1.RoleServer:   append([]string{"127.0.0.1:4646"}, known...),
		v1alpha1.RoleCombined: append([]string{"127.0.0.1:4646"}, known...),
		v1alpha1.RoleClient:   known,
	} {
		t.Run(string(role), func(t *testing.T) {
			ca := testCA(t)
			h, fsys, _, _ := ubuntu(t)
			logs := captureLog(h)
			tlsFiles(t, fsys, ca, role)
			nc := combined(t)
			switch role {
			case v1alpha1.RoleServer:
				nc = server(t)
			case v1alpha1.RoleClient:
				asClient(t, nc)
			}
			fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
			fsys.AddFile(t, peersPath, []byte(`["10.64.0.9"]`), 0o600, nodeconfig.Owner)
			// Every server answers 500, so the refresh asks each.
			srv := serveNomad(t, ca, nil)
			h.DialContext = srv.Dial
			if err := nodeup.RefreshJoin(t.Context(), h, nc); err != nil {
				t.Fatalf("RefreshJoin: %v", err)
			}
			if diff := cmp.Diff(want, srv.Dialed()); diff != "" {
				t.Errorf("the dials (-want +got):\n%s", diff)
			}
			info := fmt.Sprintf(`msg="no server answered; 05-join.hcl stays until the next refresh" asked=%d`, len(want))
			if got := infos(logs); len(got) != 1 || !strings.HasSuffix(got[0], info+"\n") {
				t.Errorf("RefreshJoin logged %q, want one line that ends with %q", got, info)
			}
		})
	}
}

// TestRefreshJoinFailsOnAWrite checks that a refresh that cannot write 05-join.hcl or the peers file fails, and says
// which file; no peers are stored for a file that was not written.
func TestRefreshJoinFailsOnAWrite(t *testing.T) {
	for _, path := range []string{joinPath, peersPath} {
		t.Run(path, func(t *testing.T) {
			ca := testCA(t)
			h, fsys, _, _ := ubuntu(t)
			tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
			nc := combined(t)
			h.DialContext = serveNomad(t, ca, []string{"10.64.0.9:4647"}).Dial
			fsys.Fail(path, errors.New("read-only file system"))
			want := "refresh 05-join.hcl: write " + path + ": read-only file system"
			if err := nodeup.RefreshJoin(t.Context(), h, nc); errText(err) != want {
				t.Errorf("RefreshJoin = %q, want %q", errText(err), want)
			}
			if _, ok := fsys.Entry(peersPath); ok {
				t.Error("the peers file was written")
			}
		})
	}
}

// statFails is a filesystem whose Stat of path fails with err.
type statFails struct {
	nodeup.FS
	path string
	err  error
}

func (s statFails) Stat(p string) (fs.FileInfo, error) {
	if p == s.path {
		return nil, s.err
	}
	return s.FS.Stat(p)
}

// TestRefreshJoinFailsOnAStat checks that a TLS file that cannot be looked at fails the refresh: it is not a missing
// file, after which the next refresh would try again.
func TestRefreshJoinFailsOnAStat(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, testCA(t), v1alpha1.RoleCombined)
	h.FS = statFails{fsys, nodeconfig.CertFile, errors.New("permission denied")}
	h.DialContext = noDial(t)
	want := "refresh 05-join.hcl: stat " + nodeconfig.CertFile + ": permission denied"
	if err := nodeup.RefreshJoin(t.Context(), h, combined(t)); errText(err) != want {
		t.Errorf("RefreshJoin = %q, want %q", errText(err), want)
	}
}

// TestRefreshJoinStopsAtItsDeadline checks that a refresh whose context ends stops asking at once, warns of no
// server that it did not finish asking, and fails with the context's error.
func TestRefreshJoinStopsAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fsys, _, _ := ubuntu(t)
		logs := captureLog(h)
		tlsFiles(t, fsys, testCA(t), v1alpha1.RoleClient)
		nc := combined(t)
		asClient(t, nc)
		fsys.AddDir(t, "/var/lib/tent", 0o700, nodeconfig.Owner)
		fsys.AddFile(t, peersPath, []byte(`["10.64.0.1", "10.64.0.2", "10.64.0.3"]`), 0o600, nodeconfig.Owner)
		var dials atomic.Int32
		h.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return nodeuptest.Hang(ctx, network, addr)
		}
		// Each call gives up after 5 s: the third is cut at 12 s, and the fourth server is never asked.
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		start := time.Now()
		err := nodeup.RefreshJoin(ctx, h, nc)
		if want := "refresh 05-join.hcl: context deadline exceeded"; !errors.Is(err, context.DeadlineExceeded) ||
			errText(err) != want {
			t.Errorf("RefreshJoin = %q, want %q, which matches context.DeadlineExceeded", errText(err), want)
		}
		if waited := time.Since(start); waited != 12*time.Second {
			t.Errorf("RefreshJoin stopped after %s, want 12s", waited)
		}
		if dials.Load() != 3 || len(warnings(logs)) != 2 {
			t.Errorf("RefreshJoin dialed %d times and warned %q; want 3 dials and 2 warnings", dials.Load(),
				warnings(logs))
		}
		if got := fsys.Changes(); len(got) != 0 {
			t.Errorf("RefreshJoin changed %q, want nothing", got)
		}
	})
}
