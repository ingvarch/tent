package nodeup_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// agentServer writes the TLS files of a node of the role and a fresh CA into fsys, starts a fake Nomad agent API with
// the node's certificate, as Nomad serves it, and points the host's Nomad dials at it. unhealthy is how many first
// answers of /v1/agent/health are 500.
func agentServer(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, role v1alpha1.Role, unhealthy int) *nodeuptest.Agent {
	t.Helper()
	ca := testCA(t)
	srv := serveNomadWith(t, ca, tlsFiles(t, fsys, ca, role), nil)
	srv.SetUnhealthy(unhealthy)
	h.DialContext = srv.Dial
	return srv
}

// Errors of a lost connection, as Linux reports them: a dial that nothing listens to, and a connection that the peer
// reset.
var (
	refused = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	reset   = &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
)

// dialer dials the Nomad API, as Host.DialContext does.
type dialer = func(ctx context.Context, network, addr string) (net.Conn, error)

// failing returns a dialer whose first n dials fail with err and that passes the others to dial, and the count of its
// dials. n below 0 fails every dial.
func failing(n int, err error, dial dialer) (dialer, *atomic.Int32) {
	var dials atomic.Int32
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if d := dials.Add(1); n < 0 || int(d) <= n {
			return nil, err
		}
		return dial(ctx, network, addr)
	}, &dials
}

// counting returns a dialer that dials through dial, and the count of its connections that are not closed.
func counting(dial dialer) (dialer, *atomic.Int32) {
	var open atomic.Int32
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		open.Add(1)
		return &countedConn{Conn: c, open: &open}, nil
	}, &open
}

// countedConn is a connection that leaves the count open once it closes.
type countedConn struct {
	net.Conn
	open *atomic.Int32
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

func TestVerifyHealthy(t *testing.T) {
	// The fake server answers that there is no leader, which a server's check must pass.
	for role, want := range map[v1alpha1.Role][]string{
		v1alpha1.RoleServer:   {"/v1/status/leader?stale"},
		v1alpha1.RoleClient:   {"/v1/agent/health?type=client"},
		v1alpha1.RoleCombined: {"/v1/status/leader?stale", "/v1/agent/health?type=client"},
	} {
		t.Run(string(role), func(t *testing.T) {
			h, fsys, _, e := ubuntu(t)
			enableUnits(t, fsys, h.Runner.(*nodeuptest.Runner))
			logs := captureLog(h)
			nc := combined(t)
			nc.Role = role
			srv := agentServer(t, h, fsys, role, 0)
			dial, open := counting(srv.Dial)
			h.DialContext = dial
			res, err := runPhase(t, "verify", h, nc, e)
			if err != nil || res.Status != nodeup.Unchanged {
				t.Errorf("verify = %s %q, %v; want unchanged", res.Status, res.Reason, err)
			}
			if n := open.Load(); n != 0 {
				t.Errorf("verify left %d connections open, want none", n)
			}
			if diff := cmp.Diff(want, srv.Requests()); diff != "" {
				t.Errorf("the agent saw (-want +got):\n%s", diff)
			}
			dialed := srv.Dialed()
			if len(dialed) == 0 || slices.ContainsFunc(dialed, func(a string) bool { return a != "127.0.0.1:4646" }) {
				t.Errorf("verify dialed %q, want 127.0.0.1:4646 alone", dialed)
			}
			if len(fsys.Changes()) != 0 {
				t.Errorf("verify changed %q, want nothing", fsys.Changes())
			}
			if got := warnings(logs); len(got) != 0 {
				t.Errorf("a healthy agent logged warnings: %q", got)
			}
		})
	}
}

// TestVerifyWaitsForTheAgent checks that the check tries again while the agent answers 500, as it does until it has
// joined, passes when it turns healthy, and logs one warning for the tries.
func TestVerifyWaitsForTheAgent(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	logs := captureLog(h)
	srv := agentServer(t, h, fsys, v1alpha1.RoleClient, 2)
	if err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if got := srv.Requests(); len(got) != 3 {
		t.Errorf("the agent saw %q, want three tries", got)
	}
	got := warnings(logs)
	want := `msg="the Nomad agent is not healthy yet, trying again" error="GET /v1/agent/health?type=client ` +
		`answered 500 Internal Server Error: {\"client\":{\"ok\":false,\"message\":\"no known servers\"}}" ` +
		"every=0s tries=60\n"
	if len(got) != 1 || !strings.HasSuffix(got[0], want) {
		t.Errorf("warnings %q, want one that ends with %q", got, want)
	}
}

// TestVerifyUnhealthyStops checks that an agent that stays unhealthy fails the check after 60 tries, with its last
// answer, and that the tries log one warning.
func TestVerifyUnhealthyStops(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	logs := captureLog(h)
	srv := agentServer(t, h, fsys, v1alpha1.RoleClient, -1)
	err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0)
	want := "check the Nomad agent: not healthy after 60 tries: GET /v1/agent/health?type=client answered 500 " +
		`Internal Server Error: {"client":{"ok":false,"message":"no known servers"}}`
	if errText(err) != want {
		t.Errorf("CheckHealth = %q, want %q", errText(err), want)
	}
	if got := len(srv.Requests()); got != 60 {
		t.Errorf("the agent saw %d tries, want 60", got)
	}
	if got := warnings(logs); len(got) != 1 {
		t.Errorf("warnings %q, want one", got)
	}
}

// TestVerifyWithoutTLSFiles checks that the health check fails when the node has no certificate yet.
func TestVerifyWithoutTLSFiles(t *testing.T) {
	h, fsys, _, e := ubuntu(t)
	enableUnits(t, fsys, h.Runner.(*nodeuptest.Runner))
	_, err := runPhase(t, "verify", h, combined(t), e)
	if err == nil || !strings.Contains(err.Error(), "CA bundle") {
		t.Errorf("verify without the TLS files: %v, want the CA bundle's error", err)
	}
}

// TestVerifyRejectsAnotherCluster checks that a TLS failure does not wait: the certificate of another cluster fails
// the check at once, and the error shows no URL.
func TestVerifyRejectsAnotherCluster(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	other := testCA(t)
	srv := serveNomad(t, other, nil)
	ca := testCA(t)
	tlsFiles(t, fsys, ca, v1alpha1.RoleCombined)
	h.DialContext = srv.Dial
	err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0)
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatalf("CheckHealth of another cluster: %v, want the TLS failure", err)
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("the other cluster's agent saw %d answered requests, want none", got)
	}
	if got := len(srv.Dialed()); got != 1 {
		t.Errorf("CheckHealth dialed %d times, want once", got)
	}
	if strings.Contains(err.Error(), "https:") {
		t.Errorf("the error shows the URL: %v", err)
	}
}

// agentAnswering writes the TLS files of a node of the role and a fresh CA into fsys, starts a fake Nomad agent with
// the node's certificate that answers path with handler, and points the host's Nomad dials at it.
func agentAnswering(t *testing.T, h *nodeup.Host, fsys *nodeuptest.FS, role v1alpha1.Role, path string,
	handler http.HandlerFunc,
) *nodeuptest.Server {
	t.Helper()
	ca := testCA(t)
	cert := tlsFiles(t, fsys, ca, role)
	srv := nodeuptest.ServeMTLS(t, ca.Bundle(), cert.Cert, cert.Key.Bytes(), map[string]http.HandlerFunc{path: handler})
	h.DialContext = srv.Dial
	return srv
}

// TestVerifyFailsOnAnotherStatus checks that an answer other than 200 or 500, which waiting does not change, fails the
// check at once.
func TestVerifyFailsOnAnotherStatus(t *testing.T) {
	cases := []struct {
		code       int
		body, want string
	}{
		{http.StatusForbidden, "Permission denied", "answered 403 Forbidden: Permission denied"},
		{http.StatusServiceUnavailable, "no healthy upstream", "answered 503 Service Unavailable: no healthy upstream"},
		{http.StatusNotFound, "", "answered 404 Not Found"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			h, fsys, _, _ := ubuntu(t)
			srv := agentAnswering(t, h, fsys, v1alpha1.RoleClient, "/v1/agent/health",
				func(w http.ResponseWriter, _ *http.Request) { http.Error(w, c.body, c.code) })
			err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0)
			if want := "check the Nomad agent: GET /v1/agent/health?type=client " + c.want; errText(err) != want {
				t.Errorf("CheckHealth = %q, want %q", errText(err), want)
			}
			if got := len(srv.Requests()); got != 1 {
				t.Errorf("the agent saw %d tries, want one", got)
			}
		})
	}
}

// TestVerifyQuotesTheAnswerShortly checks that an error quotes the start of the agent's answer, 200 characters at
// most, on one line and without the characters that a terminal acts on, with "..." where the answer goes on, and
// leaves out a quote with nothing to show.
func TestVerifyQuotesTheAnswerShortly(t *testing.T) {
	long := "rpc error: no [31mpath [0m to region "
	long += strings.Repeat("x", 200-len(long)) + "..."
	cases := []struct {
		name, body, quote string
	}{
		{"long", "rpc error:\n\tno \x1b[31mpath\x1b[0m to region\n" + strings.Repeat("x", 64<<10), ": " + long},
		{"white space after the text", "no leader" + strings.Repeat(" ", 1000) + "x", ": no leader..."},
		{"invalid UTF-8", strings.Repeat("\xff", 1000) + "no leader", ": ..."},
		{"white space alone", " \n\t ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, _, _ := ubuntu(t)
			agentAnswering(t, h, fsys, v1alpha1.RoleServer, "/v1/status/leader",
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, c.body)
				})
			err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleServer, 0)
			want := "check the Nomad agent: GET /v1/status/leader?stale answered 503 Service Unavailable" + c.quote
			if errText(err) != want {
				t.Errorf("CheckHealth = %q, want %q", errText(err), want)
			}
		})
	}
}

// TestVerifyRetriesALostConnection checks that a connection that is refused or reset, as while systemd restarts the
// agent, is tried again.
func TestVerifyRetriesALostConnection(t *testing.T) {
	for name, lost := range map[string]error{"refused": refused, "reset": reset} {
		t.Run(name, func(t *testing.T) {
			h, fsys, _, _ := ubuntu(t)
			srv := agentServer(t, h, fsys, v1alpha1.RoleClient, 0)
			dial, dials := failing(2, lost, srv.Dial)
			h.DialContext = dial
			if err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0); err != nil {
				t.Fatalf("CheckHealth: %v", err)
			}
			if dials.Load() != 3 || len(srv.Requests()) != 1 {
				t.Errorf("CheckHealth dialed %d times and the agent saw %q; want 3 dials and one request",
					dials.Load(), srv.Requests())
			}
			// An agent that never comes back fails the check with the last failure.
			h.DialContext, _ = failing(-1, lost, nil)
			err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0)
			want := "check the Nomad agent: not healthy after 60 tries: GET /v1/agent/health?type=client: " +
				lost.Error()
			if !errors.Is(err, lost) || errText(err) != want {
				t.Errorf("CheckHealth = %q, want %q, which matches the lost connection's error", errText(err), want)
			}
		})
	}
}

// TestVerifyRetriesAClosedConnection checks that an agent that closes the connection before the whole answer, as one
// that systemd stops does, is asked again.
func TestVerifyRetriesAClosedConnection(t *testing.T) {
	for name, lose := range map[string]http.HandlerFunc{
		"before the answer": func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		},
		// The server closes a connection whose answer is shorter than its length.
		"in the answer": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, `{"client"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, fsys, _, _ := ubuntu(t)
			var answers atomic.Int32
			srv := agentAnswering(t, h, fsys, v1alpha1.RoleClient, "/v1/agent/health",
				func(w http.ResponseWriter, r *http.Request) {
					if answers.Add(1) <= 2 {
						lose(w, r)
						return
					}
					_, _ = io.WriteString(w, `{"client":{"ok":true}}`)
				})
			if err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0); err != nil {
				t.Fatalf("CheckHealth: %v", err)
			}
			if got := len(srv.Requests()); got != 3 {
				t.Errorf("the agent saw %d tries, want 3", got)
			}
		})
	}
}

// TestVerifyFailsAtOnceWithoutTheAgent checks that a dial that fails for another reason than a lost connection fails
// the check at once: a test machine without a fake agent fails clearly instead of waiting.
func TestVerifyFailsAtOnceWithoutTheAgent(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	tlsFiles(t, fsys, testCA(t), v1alpha1.RoleCombined)
	dial, dials := failing(0, nil, h.DialContext)
	h.DialContext = dial
	err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleServer, 0)
	want := "check the Nomad agent: GET /v1/status/leader?stale: nodeuptest: the machine has no network: dial the " +
		"servers with a stub"
	if errText(err) != want || dials.Load() != 1 {
		t.Errorf("CheckHealth = %q after %d dials, want %q after one", errText(err), dials.Load(), want)
	}
}

// TestVerifyGivesUpAfterTwoMinutes checks the phase's budget: 60 tries, two seconds apart.
func TestVerifyGivesUpAfterTwoMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fsys, r, e := ubuntu(t)
		enableUnits(t, fsys, r)
		tlsFiles(t, fsys, testCA(t), v1alpha1.RoleCombined)
		logs := captureLog(h)
		dial, dials := failing(-1, refused, nil)
		h.DialContext = dial
		start := time.Now()
		_, err := runPhase(t, "verify", h, combined(t), e)
		if waited := time.Since(start); waited != 59*2*time.Second {
			t.Errorf("verify gave up after %s, want 1m58s", waited)
		}
		want := "check the Nomad agent: not healthy after 60 tries: GET /v1/status/leader?stale: " + refused.Error()
		if !errors.Is(err, syscall.ECONNREFUSED) || errText(err) != want {
			t.Errorf("verify = %q, want %q, which matches ECONNREFUSED", errText(err), want)
		}
		if dials.Load() != 60 {
			t.Errorf("verify dialed %d times, want 60", dials.Load())
		}
		if got := warnings(logs); len(got) != 1 || !strings.HasSuffix(got[0], " every=2s tries=60\n") {
			t.Errorf("warnings %q, want one that says every=2s tries=60", got)
		}
	})
}

// TestVerifyStopsWaitingWhenTheContextEnds checks that the check stops at once when its context ends during a wait,
// and keeps the last failure.
func TestVerifyStopsWaitingWhenTheContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, fsys, _, _ := ubuntu(t)
		tlsFiles(t, fsys, testCA(t), v1alpha1.RoleCombined)
		dial, dials := failing(-1, refused, nil)
		h.DialContext = dial
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		start := time.Now()
		err := nodeup.CheckHealth(ctx, h, v1alpha1.RoleClient, 2*time.Second)
		if waited := time.Since(start); waited != 3*time.Second {
			t.Errorf("CheckHealth stopped after %s, want 3s", waited)
		}
		want := "check the Nomad agent: GET /v1/agent/health?type=client: " + refused.Error() + "; stopped waiting: " +
			context.DeadlineExceeded.Error()
		if !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, context.DeadlineExceeded) || errText(err) != want {
			t.Errorf("CheckHealth = %q, want %q, which matches both errors", errText(err), want)
		}
		if dials.Load() != 2 {
			t.Errorf("CheckHealth dialed %d times, want 2", dials.Load())
		}
	})
}
