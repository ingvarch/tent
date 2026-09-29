package nomadops_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// region is the Nomad region of the tests' cluster.
const region = "eu"

var _ nomadops.API = (*nomadops.Client)(nil)

// testPKI is the CA of a test cluster with the certificate of one server and an operator certificate.
type testPKI struct {
	ca       *pki.CA
	server   pki.Certificate
	operator pki.Certificate
}

// newPKI makes a CA, a certificate of a node of the role and an operator certificate, both for the region. Clients
// check certificates against the real clock, so they start now.
func newPKI(t *testing.T, role v1alpha1.Role, region string) testPKI {
	t.Helper()
	now := time.Now()
	ca, err := pki.NewCA("prod", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	operator, err := ca.IssueOperator(region, time.Hour, now)
	if err != nil {
		t.Fatalf("IssueOperator: %v", err)
	}
	return testPKI{ca: ca, server: issue(t, ca, role, region), operator: operator}
}

// issue returns a certificate that the CA issues now to a node of the role in the region.
func issue(t *testing.T, ca *pki.CA, role v1alpha1.Role, region string) pki.Certificate {
	t.Helper()
	c, err := ca.IssueNode(role, region, time.Now())
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	return c
}

// gotRequest is a request that a nomadServer got.
type gotRequest struct {
	Method, Path, Query string
	Token               string // the X-Nomad-Token header
	Peer                string // the common name of the client certificate
	Body                string
}

// nomadServer is a test server in the place of the HTTP API of a Nomad server. It speaks TLS with its certificate,
// requires a client certificate that its client CA signed, and logs the requests it gets.
type nomadServer struct {
	*httptest.Server

	mu   sync.Mutex
	reqs []gotRequest
}

// newNomadServer starts a nomadServer with the certificate that trusts the client certificates of clientCA and answers
// with h.
func newNomadServer(t *testing.T, cert pki.Certificate, clientCA []byte, h http.HandlerFunc) *nomadServer {
	t.Helper()
	pair, err := tls.X509KeyPair(cert.Cert, cert.Key)
	if err != nil {
		t.Fatalf("the server certificate: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientCA) {
		t.Fatal("the client CA holds no certificate")
	}
	s := &nomadServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // for h
		got := gotRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Token: r.Header.Get("X-Nomad-Token"),
			Body: string(body),
		}
		if len(r.TLS.PeerCertificates) > 0 {
			got.Peer = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, got)
		s.mu.Unlock()
		h(w, r)
	}))
	s.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
	}
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // the failed handshakes that the tests cause
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// addr returns the host and port of the server.
func (s *nomadServer) addr() string { return s.Listener.Addr().String() }

// requests returns the requests the server got so far.
func (s *nomadServer) requests() []gotRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// clientToken names the client's token in the requests that tests expect.
const clientToken = "the client's token"

// checkRequests fails t unless the server got the requests of want. The secrets in tokens are compared by their
// names, so that a message never shows one: a request's token is the name of its secret, or "another token", and a
// secret in a body reads <name>.
func checkRequests(t *testing.T, s *nomadServer, tokens map[string]secret.Secret, want ...gotRequest) {
	t.Helper()
	got := s.requests()
	for i, r := range got {
		if r.Token != "" {
			got[i].Token = "another token"
		}
		for name, token := range tokens {
			if len(token) == 0 {
				continue
			}
			if r.Token == string(token) {
				got[i].Token = name
			}
			got[i].Body = strings.ReplaceAll(got[i].Body, string(token), "<"+name+">")
		}
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// clientTokens names the client's token for checkRequests.
func clientTokens(token secret.Secret) map[string]secret.Secret {
	return map[string]secret.Secret{clientToken: token}
}

// answer returns a handler that answers with status and body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// config returns the config of a client of the server with the operator certificate of p and the token.
func config(s *nomadServer, p testPKI, token secret.Secret) nomadops.Config {
	return nomadops.Config{Address: s.addr(), Region: region, CA: p.ca.Bundle(), Cert: p.operator, Token: token}
}

// newClient returns a client of the server with the operator certificate of p and the token.
func newClient(t *testing.T, s *nomadServer, p testPKI, token secret.Secret) *nomadops.Client {
	t.Helper()
	c, err := nomadops.New(config(s, p, token))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// leaderRequest is the request of Leader.
var leaderRequest = gotRequest{Method: http.MethodGet, Path: "/v1/status/leader", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// show returns what a message may print of err: its text, unless it shows one of the secrets. Then it fails t and
// names the secret instead.
func show(t *testing.T, err error, secrets map[string]secret.Secret) string {
	t.Helper()
	if err == nil {
		return "no error"
	}
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if secrettest.Shows(err.Error(), secrets[name]) {
			t.Errorf("the error shows %s", name)
			return "an error that shows " + name
		}
	}
	return err.Error()
}

// checkCallErr fails t unless err is want, as text, and matches ErrNotReady exactly when notReady is set. It checks
// first that err shows none of the secrets, and never prints one.
func checkCallErr(t *testing.T, err error, want string, notReady bool, secrets map[string]secret.Secret) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %q", want)
	}
	text := show(t, err, secrets)
	if text != want {
		t.Errorf("error = %s, want %q", text, want)
	}
	if errors.Is(err, nomadops.ErrNotReady) != notReady {
		t.Errorf("errors.Is(%s, ErrNotReady) = %v, want %v", text, !notReady, notReady)
	}
}

// checkPermanent fails t unless err starts with prefix, says more, and does not match ErrNotReady.
func checkPermanent(t *testing.T, err error, prefix, says string, secrets map[string]secret.Secret) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	text := show(t, err, secrets)
	switch {
	case !strings.HasPrefix(text, prefix) || text == prefix || !strings.Contains(text[len(prefix):], says):
		t.Errorf("error = %s, want one that starts with %q and says %q", text, prefix, says)
	case errors.Is(err, nomadops.ErrNotReady):
		t.Errorf("error = %s matches ErrNotReady, want a permanent error", text)
	}
}

func TestNewRefusesABadConfig(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	good := nomadops.Config{Address: "203.0.113.5:4646", Region: region, CA: p.ca.Bundle(), Cert: p.operator,
		Token: token}
	notHostPort := func(addr string) string { return fmt.Sprintf("nomad: address %q is not host:port", addr) }
	cases := []struct {
		name   string
		change func(*nomadops.Config)
		want   string
	}{
		{"no address", func(c *nomadops.Config) { c.Address = "" }, notHostPort("")},
		{"no port", func(c *nomadops.Config) { c.Address = "203.0.113.5" }, notHostPort("203.0.113.5")},
		{"port 0", func(c *nomadops.Config) { c.Address = "203.0.113.5:0" }, notHostPort("203.0.113.5:0")},
		{"a port too high", func(c *nomadops.Config) { c.Address = "203.0.113.5:65536" },
			notHostPort("203.0.113.5:65536")},
		{"a named port", func(c *nomadops.Config) { c.Address = "203.0.113.5:https" },
			notHostPort("203.0.113.5:https")},
		{"no host", func(c *nomadops.Config) { c.Address = ":4646" }, notHostPort(":4646")},
		{"a URL", func(c *nomadops.Config) { c.Address = "https://203.0.113.5:4646" },
			notHostPort("https://203.0.113.5:4646")},
		{"a path", func(c *nomadops.Config) { c.Address = "203.0.113.5:4646/v1" }, notHostPort("203.0.113.5:4646/v1")},
		{"IPv6 without brackets", func(c *nomadops.Config) { c.Address = "2001:db8::1:4646" },
			notHostPort("2001:db8::1:4646")},
		{"no region", func(c *nomadops.Config) { c.Region = "" }, "nomad: no region"},
		{"no CA", func(c *nomadops.Config) { c.CA = nil }, "nomad: the CA bundle holds no certificate"},
		{"a key as the CA", func(c *nomadops.Config) { c.CA = p.operator.Key },
			"nomad: the CA bundle holds no certificate"},
		{"no certificate", func(c *nomadops.Config) { c.Cert.Cert = nil },
			"nomad: the operator certificate: tls: failed to find any PEM data in certificate input"},
		{"no key", func(c *nomadops.Config) { c.Cert.Key = nil },
			"nomad: the operator certificate: tls: failed to find any PEM data in key input"},
		{"another certificate's key", func(c *nomadops.Config) { c.Cert.Key = other.operator.Key },
			"nomad: the operator certificate: tls: private key does not match public key"},
		{"no token", func(c *nomadops.Config) { c.Token = nil }, "nomad: no ACL token"},
		{"a line end after the token", func(c *nomadops.Config) { c.Token = secret.Secret(string(token) + "\n") },
			"nomad: the ACL token has a control character"},
		{"a NUL in the token", func(c *nomadops.Config) { c.Token = secret.Secret(string(token[:8]) + "\x00") },
			"nomad: the ACL token has a control character"},
	}
	secrets := map[string]secret.Secret{
		"the token": token, "the operator key": p.operator.Key, "another key": other.operator.Key,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.change(&cfg)
			c, err := nomadops.New(cfg)
			if err == nil {
				t.Fatalf("New() = %v, want the error %q", c, tc.want)
			}
			if text := show(t, err, secrets); text != tc.want {
				t.Errorf("New() error = %s, want %q", text, tc.want)
			}
		})
	}
	if _, err := nomadops.New(good); err != nil {
		t.Errorf("New() of the good config: %v", err)
	}
	ipv6 := good
	ipv6.Address = "[2001:db8::1]:4646"
	if _, err := nomadops.New(ipv6); err != nil {
		t.Errorf("New() of an IPv6 address: %v", err)
	}
}

func TestLeaderOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `"10.0.0.5:4647"`))
	got, err := newClient(t, srv, p, token).Leader(t.Context())
	if err != nil {
		t.Fatalf("Leader: %s", show(t, err, clientTokens(token)))
	}
	if got != "10.0.0.5:4647" {
		t.Errorf("Leader() = %q, want 10.0.0.5:4647", got)
	}
	checkRequests(t, srv, clientTokens(token), leaderRequest)
}

// TestLeaderNotReady checks the answers after which the same call may succeed later.
func TestLeaderNotReady(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"no leader", answer(http.StatusOK, `""`), "nomad: GET /v1/status/leader: no leader"},
		{"500", answer(http.StatusInternalServerError, "No cluster leader"),
			"nomad: GET /v1/status/leader: 500: No cluster leader"},
		{"503", answer(http.StatusServiceUnavailable, "starting"), "nomad: GET /v1/status/leader: 503: starting"},
		{"429", answer(http.StatusTooManyRequests, "too many requests"),
			"nomad: GET /v1/status/leader: 429: too many requests"},
		{"a long page", answer(http.StatusBadGateway, "<html>\n<body>"+strings.Repeat("x", 600)+"</body>\n</html>"),
			"nomad: GET /v1/status/leader: 502: <html> <body>" + strings.Repeat("x", 499) + "…"},
		{"a character cut at the end", answer(http.StatusBadGateway, strings.Repeat("x", 511)+"éé"),
			"nomad: GET /v1/status/leader: 502: " + strings.Repeat("x", 511) + "…"},
		{"an answer cut short", cutAnswer(t), "nomad: GET /v1/status/leader: unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), tc.handler)
			_, err := newClient(t, srv, p, token).Leader(t.Context())
			checkCallErr(t, err, tc.want, true, clientTokens(token))
			checkRequests(t, srv, clientTokens(token), leaderRequest)
		})
	}
}

// cutAnswer returns a handler that sends a 200 whose body is shorter than its Content-Length says, and then closes
// the connection.
func cutAnswer(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n\"10.0.0."); err != nil {
			t.Errorf("write the answer: %v", err)
		}
		_ = conn.Close()
	}
}

func TestLeaderOfAClosedServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `"10.0.0.5:4647"`))
	c := newClient(t, srv, p, token)
	srv.Close()
	_, err := c.Leader(t.Context())
	text := show(t, err, clientTokens(token))
	if !errors.Is(err, nomadops.ErrNotReady) {
		t.Errorf("Leader() error = %s, want one that matches ErrNotReady", text)
	}
	if want := "nomad: GET /v1/status/leader: "; !strings.HasPrefix(text, want) {
		t.Errorf("Leader() error = %s, want one that starts with %q", text, want)
	}
}

// blockingHandler returns a handler that tells got when it has a request and then answers nothing until the request
// ends or the test does.
func blockingHandler(t *testing.T, got chan<- struct{}) http.HandlerFunc {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(_ http.ResponseWriter, r *http.Request) {
		if got != nil {
			got <- struct{}{}
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

func TestLeaderOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
	c := newClient(t, srv, p, token)
	c.SetTimeout(50 * time.Millisecond)
	_, err := c.Leader(t.Context())
	checkCallErr(t, err, "nomad: GET /v1/status/leader: no answer within 50ms", true, clientTokens(token))
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("the error matches context.DeadlineExceeded, which a caller would take for the end of its own context")
	}
}

func TestLeaderStopsWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	got := make(chan struct{})
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
	c := newClient(t, srv, p, token)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-got
		cancel()
	}()
	_, err := c.Leader(ctx)
	checkCallErr(t, err, "nomad: GET /v1/status/leader: context canceled", false, clientTokens(token))
	if !errors.Is(err, context.Canceled) {
		t.Error("the error does not match context.Canceled")
	}
}

// TestLeaderTLSFailuresArePermanent checks that a server or a client that the other side does not trust fails at
// once, whatever the server would answer.
func TestLeaderTLSFailuresArePermanent(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	cases := []struct {
		name     string
		server   pki.Certificate // the server's certificate
		clientCA []byte          // the CA whose client certificates the server trusts
		want     string          // what the error says after "nomad: GET /v1/status/leader: "
	}{
		{"a client's certificate", issue(t, p.ca, v1alpha1.RoleClient, region), p.ca.Bundle(),
			"certificate is valid for client.eu.nomad"},
		{"a server of another region", issue(t, p.ca, v1alpha1.RoleServer, "us"), p.ca.Bundle(),
			"certificate is valid for server.us.nomad"},
		{"a server of another CA", other.server, p.ca.Bundle(), "certificate signed by unknown authority"},
		{"a server that trusts another CA", p.server, other.ca.Bundle(), "remote error: tls:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := pki.NewBootstrapSecret()
			srv := newNomadServer(t, tc.server, tc.clientCA, answer(http.StatusOK, `"10.0.0.5:4647"`))
			_, err := newClient(t, srv, p, token).Leader(t.Context())
			checkPermanent(t, err, "nomad: GET /v1/status/leader: ", tc.want, clientTokens(token))
		})
	}
}

// newClientOf returns a client of a server at addr, which need not speak TLS, with the operator certificate of p.
func newClientOf(t *testing.T, addr string, p testPKI, token secret.Secret) *nomadops.Client {
	t.Helper()
	c, err := nomadops.New(nomadops.Config{Address: addr, Region: region, CA: p.ca.Bundle(), Cert: p.operator,
		Token: token})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestLeaderOfAServerWithoutTLS checks that a server that answers in plain HTTP fails at once.
func TestLeaderOfAServerWithoutTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := httptest.NewServer(answer(http.StatusOK, `"10.0.0.5:4647"`))
	t.Cleanup(srv.Close)
	_, err := newClientOf(t, srv.Listener.Addr().String(), p, token).Leader(t.Context())
	checkCallErr(t, err, "nomad: GET /v1/status/leader: http: server gave HTTP response to HTTPS client", false,
		clientTokens(token))
}

// TestLeaderOfAServerThatSpeaksSSH checks that a server that speaks neither TLS nor HTTP fails at once.
func TestLeaderOfAServerThatSpeaksSSH(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = l.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "SSH-2.0-OpenSSH_9.6\r\n")
			_, _ = io.Copy(io.Discard, conn) // until the client hangs up
			_ = conn.Close()
		}
	}()
	_, err = newClientOf(t, l.Addr().String(), p, token).Leader(t.Context())
	checkCallErr(t, err, "nomad: GET /v1/status/leader: tls: first record does not look like a TLS handshake", false,
		clientTokens(token))
}

// TestLeaderPermanentAnswers checks that a 4xx other than 429 is permanent.
func TestLeaderPermanentAnswers(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusForbidden, "Permission denied"))
	_, err := newClient(t, srv, p, token).Leader(t.Context())
	checkCallErr(t, err, "nomad: GET /v1/status/leader: 403: Permission denied", false, clientTokens(token))
}

// TestLeaderOfAnAnswerThatIsNotJSON checks that a success that the client cannot read is permanent.
func TestLeaderOfAnAnswerThatIsNotJSON(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "<html>"))
	_, err := newClient(t, srv, p, token).Leader(t.Context())
	checkPermanent(t, err, "nomad: GET /v1/status/leader: ", "", clientTokens(token))
}

// TestLeaderDoesNotFollowRedirects checks that the token never goes to where a server redirects, such as a plain
// HTTP address.
func TestLeaderDoesNotFollowRedirects(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	var mu sync.Mutex
	followed := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		followed++
		mu.Unlock()
		answer(http.StatusOK, `"10.9.9.9:4647"`)(w, nil)
	}))
	t.Cleanup(elsewhere.Close)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	_, err := newClient(t, srv, p, token).Leader(t.Context())
	checkPermanent(t, err, "nomad: GET /v1/status/leader: 307", "", clientTokens(token))
	mu.Lock()
	defer mu.Unlock()
	if followed > 0 {
		t.Errorf("the client followed the redirect %d times, want never", followed)
	}
}

// TestCallErrorClasses checks the class of the errors that the Nomad API module gives without an answer's status.
func TestCallErrorClasses(t *testing.T) {
	var syntax *json.SyntaxError
	if err := json.Unmarshal([]byte("<html>"), new(string)); !errors.As(err, &syntax) {
		t.Fatalf("json.Unmarshal of <html>: %v, want a *json.SyntaxError", err)
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	cases := []struct {
		name     string
		err      error
		notReady bool
	}{
		{"a body cut short", io.ErrUnexpectedEOF, true},
		{"a reset while reading the body", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"a refused connection", &url.Error{Op: "Get", URL: "https://203.0.113.5:4646/v1/nodes", Err: refused}, true},
		{"a body that is not JSON", syntax, false},
		{"an alert from the server", &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nomadops.NewCallError(http.MethodGet, "/v1/nodes", tc.err)
			if got := errors.Is(err, nomadops.ErrNotReady); got != tc.notReady {
				t.Errorf("errors.Is(%v, ErrNotReady) = %v, want %v", err, got, tc.notReady)
			}
		})
	}
}

// TestNewIgnoresTheNomadEnvironment checks that the NOMAD_* variables of the Nomad CLI change nothing: not the
// server, the region, the token, the certificates, nor the checks of the server's certificate.
func TestNewIgnoresTheNomadEnvironment(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	decoy := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `"10.9.9.9:4647"`))
	for name, value := range map[string]string{
		"NOMAD_ADDR":            decoy.URL,
		"NOMAD_REGION":          "us",
		"NOMAD_NAMESPACE":       "other",
		"NOMAD_TOKEN":           "00000000-0000-4000-8000-000000000000",
		"NOMAD_HTTP_AUTH":       "user:password",
		"NOMAD_CACERT":          "/does/not/exist/ca.pem",
		"NOMAD_CAPATH":          "/does/not/exist",
		"NOMAD_CLIENT_CERT":     "/does/not/exist/cli.pem",
		"NOMAD_CLIENT_KEY":      "/does/not/exist/cli-key.pem",
		"NOMAD_TLS_SERVER_NAME": "client.eu.nomad",
		"NOMAD_SKIP_VERIFY":     "true",
	} {
		t.Setenv(name, value)
	}

	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `"10.0.0.5:4647"`))
	got, err := newClient(t, srv, p, token).Leader(t.Context())
	if err != nil || got != "10.0.0.5:4647" {
		t.Errorf("Leader() = %q, %s; want 10.0.0.5:4647", got, show(t, err, clientTokens(token)))
	}
	checkRequests(t, srv, clientTokens(token), leaderRequest)
	if n := len(decoy.requests()); n > 0 {
		t.Errorf("the server of NOMAD_ADDR got %d requests, want none", n)
	}

	untrusted := newNomadServer(t, other.server, p.ca.Bundle(), answer(http.StatusOK, `"10.0.0.5:4647"`))
	if _, err := newClient(t, untrusted, p, token).Leader(t.Context()); err == nil {
		t.Error("Leader() of a server of another CA succeeded with NOMAD_SKIP_VERIFY=true, want an error")
	}
}

func TestConfigAndClientPrintNoSecret(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `"10.0.0.5:4647"`))
	cfg := config(srv, p, token)
	secrets := map[string][]byte{"the token": token, "the operator key": p.operator.Key}
	secrettest.CheckHidden(t, secrettest.Printed(t, cfg), secrets, fmt.Sprintf("[secret, %d bytes]", len(token)))
	c := newClient(t, srv, p, token)
	secrettest.CheckHidden(t, secrettest.Printed(t, c), secrets, "")
	secrettest.CheckHidden(t, secrettest.Printed(t, *c), secrets, "")
	for _, tc := range []struct {
		client any
		want   string
	}{
		{c, "nomadops.Client(" + srv.URL + ")"},
		{nomadops.Client{}, "nomadops.Client()"},
	} {
		if got := fmt.Sprintf("%s", tc.client); got != tc.want {
			t.Errorf("Sprintf(%%s) = %q, want %q", got, tc.want)
		}
	}
}
