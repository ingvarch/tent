package nomadops_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// proxyToken names the proxy's token in the requests that tests expect.
const proxyToken = "the proxy's token"

// pageToken is a token that a page sends, which the proxy must replace.
const pageToken = "a token of the page"

// queryToken is a token in a query that escaping leaves as it is.
const queryToken = "a-token-in-the-query"

func TestRefusal(t *testing.T) {
	const (
		hostRefused  = "forbidden: unexpected Host"
		originRefuse = "forbidden: unexpected Origin"
		fetchRefused = "forbidden: cross-site request"
	)
	type hdr = map[string][]string
	cases := []struct {
		name   string
		listen string
		host   string
		header hdr
		want   string
	}{
		{"own address", "127.0.0.1:4646", "127.0.0.1:4646", nil, ""},
		{"localhost", "127.0.0.1:4646", "localhost:4646", nil, ""},
		{"upper-case localhost", "127.0.0.1:4646", "LOCALHOST:4646", nil, ""},
		{"localhost with a trailing dot", "127.0.0.1:4646", "localhost.:4646", nil, ""},
		{"IPv4 with a trailing dot", "127.0.0.1:4646", "127.0.0.1.:4646", nil, ""},
		{"another loopback address", "127.0.0.1:4646", "127.0.0.2:4646", nil, hostRefused},
		{"another port", "127.0.0.1:4646", "127.0.0.1:4647", nil, hostRefused},
		{"localhost on another port", "127.0.0.1:4646", "localhost:4647", nil, hostRefused},
		{"no port", "127.0.0.1:4646", "127.0.0.1", nil, hostRefused},
		{"localhost without a port", "127.0.0.1:4646", "localhost", nil, hostRefused},
		{"a name that points at 127.0.0.1", "127.0.0.1:4646", "evil.example:4646", nil, hostRefused},
		{"a name with localhost in it", "127.0.0.1:4646", "localhost.evil.example:4646", nil, hostRefused},
		{"a name that starts as localhost", "127.0.0.1:4646", "localhost@evil.example:4646", nil, hostRefused},
		{"no Host", "127.0.0.1:4646", "", nil, hostRefused},
		{"an IPv6 Host on an IPv4 listener", "127.0.0.1:4646", "[::1]:4646", nil, hostRefused},
		{"an IPv4-mapped Host", "127.0.0.1:4646", "[::ffff:127.0.0.1]:4646", nil, hostRefused},
		{"a port that is not a number", "127.0.0.1:4646", "127.0.0.1:http", nil, hostRefused},

		{"IPv6 own address", "[::1]:4646", "[::1]:4646", nil, ""},
		{"IPv6 written long", "[::1]:4646", "[0:0:0:0:0:0:0:1]:4646", nil, ""},
		{"localhost on an IPv6 listener", "[::1]:4646", "localhost:4646", nil, ""},
		{"IPv4 on an IPv6 listener", "[::1]:4646", "127.0.0.1:4646", nil, hostRefused},
		{"IPv6 without its brackets", "[::1]:4646", "::1:4646", nil, hostRefused},

		{"a listener on localhost", "localhost:4646", "localhost:4646", nil, ""},
		{"a listener on upper-case localhost", "LocalHost.:4646", "localhost:4646", nil, ""},
		{"an address on a listener on localhost", "localhost:4646", "127.0.0.1:4646", nil, hostRefused},
		{"a listener on port 80", "127.0.0.1:80", "127.0.0.1", nil, ""},
		{"a listener on port 80 and port 80 spelled", "127.0.0.1:80", "127.0.0.1:80", nil, ""},

		{"own Origin", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Origin": {"http://127.0.0.1:4646"}}, ""},
		{"Origin of localhost for localhost", "127.0.0.1:4646", "localhost:4646",
			hdr{"Origin": {"http://localhost:4646"}}, ""},
		{"Origin in upper case", "127.0.0.1:4646", "localhost:4646", hdr{"Origin": {"http://LocalHost:4646"}}, ""},
		{"Origin of localhost for the address", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://localhost:4646"}}, originRefuse},
		{"Origin of the address for localhost", "127.0.0.1:4646", "localhost:4646",
			hdr{"Origin": {"http://127.0.0.1:4646"}}, originRefuse},
		{"Origin of another site", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://evil.example"}}, originRefuse},
		{"Origin of another site on the port", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://evil.example:4646"}}, originRefuse},
		{"Origin on another port", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1:4647"}}, originRefuse},
		{"Origin without a port", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1"}}, originRefuse},
		{"Origin of port 80", "127.0.0.1:80", "127.0.0.1", hdr{"Origin": {"http://127.0.0.1"}}, ""},
		{"Origin over https", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"https://127.0.0.1:4646"}}, originRefuse},
		{"Origin null", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Origin": {"null"}}, originRefuse},
		{"Origin empty", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Origin": {""}}, originRefuse},
		{"Origin with a path", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1:4646/ui"}}, originRefuse},
		{"Origin with a user", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://evil.example@127.0.0.1:4646"}}, originRefuse},
		{"Origin that does not parse", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Origin": {"http://[::1"}}, originRefuse},
		{"two Origins", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1:4646", "http://127.0.0.1:4646"}}, originRefuse},
		{"IPv6 Origin", "[::1]:4646", "[::1]:4646", hdr{"Origin": {"http://[::1]:4646"}}, ""},
		{"IPv6 Origin written long", "[::1]:4646", "[::1]:4646", hdr{"Origin": {"http://[0:0:0:0:0:0:0:1]:4646"}}, ""},
		{"IPv4 Origin for an IPv6 Host", "[::1]:4646", "[::1]:4646", hdr{"Origin": {"http://127.0.0.1:4646"}},
			originRefuse},
		{"Origin of a foreign Host", "127.0.0.1:4646", "evil.example:4646",
			hdr{"Origin": {"http://evil.example:4646"}}, hostRefused},

		{"same-origin", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {"same-origin"}}, ""},
		{"none", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {"none"}}, ""},
		{"cross-site", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {"cross-site"}}, fetchRefused},
		{"same-site", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {"same-site"}}, fetchRefused},
		{"an unknown value", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {"elsewhere"}}, fetchRefused},
		{"an empty value", "127.0.0.1:4646", "127.0.0.1:4646", hdr{"Sec-Fetch-Site": {""}}, fetchRefused},
		{"two values", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Sec-Fetch-Site": {"same-origin", "cross-site"}}, fetchRefused},
		{"own Origin and same-origin", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1:4646"}, "Sec-Fetch-Site": {"same-origin"}}, ""},
		{"own Origin and cross-site", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://127.0.0.1:4646"}, "Sec-Fetch-Site": {"cross-site"}}, fetchRefused},
		{"a foreign Origin and same-origin", "127.0.0.1:4646", "127.0.0.1:4646",
			hdr{"Origin": {"http://evil.example"}, "Sec-Fetch-Site": {"same-origin"}}, originRefuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.header {
				h[k] = v // not Add: it would fold the empty value away
			}
			got, err := nomadops.Refusal(tc.listen, tc.host, h)
			if err != nil {
				t.Fatalf("Refusal(%q): %v", tc.listen, err)
			}
			if got != tc.want {
				t.Errorf("Refusal(%q, %q, %v) = %q, want %q", tc.listen, tc.host, tc.header, got, tc.want)
			}
		})
	}
}

func TestRefusalRefusesABadListener(t *testing.T) {
	for _, listen := range []string{"", "127.0.0.1", ":4646", "127.0.0.1:0", "127.0.0.1:http", "http://127.0.0.1:4646",
		"127.0.0.1:4646/ui", "::1:4646"} {
		if got, err := nomadops.Refusal(listen, "127.0.0.1:4646", http.Header{}); err == nil {
			t.Errorf("Refusal(%q) = %q, nil; want an error", listen, got)
		}
	}
}

// logBuffer is a log destination that handlers on several goroutines may write.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor waits until the log holds text, for 5 seconds at most. The proxy writes the line of a request after its
// answer went out, so a test that reads the log right after an answer may come before the line.
func (b *logBuffer) waitFor(text string) {
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), text) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// logger returns a logger that writes everything to the buffer.
func (b *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// proxyConfig returns the config of a proxy of the servers that is served on listen.
func proxyConfig(servers []string, p testPKI, token secret.Secret, listen string) nomadops.ProxyConfig {
	return nomadops.ProxyConfig{Servers: servers, Region: region, CA: p.ca.Bundle(), Cert: p.operator, Token: token,
		Listen: listen}
}

// startProxy serves a proxy of the servers on a loopback address and returns its server.
func startProxy(t *testing.T, servers []string, p testPKI, token secret.Secret, log *slog.Logger) *httptest.Server {
	t.Helper()
	front := httptest.NewUnstartedServer(nil)
	cfg := proxyConfig(servers, p, token, front.Listener.Addr().String())
	cfg.Log = log
	h, err := nomadops.NewProxy(cfg)
	if err != nil {
		_ = front.Listener.Close()
		t.Fatalf("NewProxy: %s", show(t, err, map[string]secret.Secret{"the token": token}))
	}
	front.Config.Handler = h
	front.Start()
	t.Cleanup(front.Close)
	return front
}

// reply is what a caller of the proxy got.
type reply struct {
	status int
	header http.Header
	body   string
}

// send sends the request to the proxy, with the headers, and never follows a redirect. A host that is not empty
// replaces the request's Host.
func send(t *testing.T, method, url, host, body string, header map[string]string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := noRedirectClient(t).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return reply{status: res.StatusCode, header: res.Header, body: string(b)}
}

// noRedirectClient returns a client that keeps no connection and follows no redirect.
func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// seenHeaders records the headers that a test server got of its requests.
type seenHeaders struct {
	mu   sync.Mutex
	rows []map[string]string
}

func (s *seenHeaders) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, map[string]string{
		"Origin": r.Header.Get("Origin"), "Authorization": r.Header.Get("Authorization"), "Host": r.Host,
	})
}

func (s *seenHeaders) all() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.rows...)
}

func proxyTokens(token secret.Secret) map[string]secret.Secret {
	return map[string]secret.Secret{proxyToken: token}
}

func TestProxyForwardsRequests(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	var seen seenHeaders
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)
		answer(http.StatusOK, `{"ok":true}`)(w, r)
	})
	front := startProxy(t, []string{srv.addr()}, p, token, nil)

	own := map[string]string{"X-Nomad-Token": pageToken, "Authorization": "Bearer " + pageToken, "Origin": front.URL}
	get := send(t, http.MethodGet, front.URL+"/v1/jobs?namespace=dev&prefix=a%2Fb", "", "", own)
	put := send(t, http.MethodPut, front.URL+"/v1/namespace/dev", "", `{"Name":"dev"}`, nil)
	for _, r := range []reply{get, put} {
		if r.status != http.StatusOK || r.body != `{"ok":true}` {
			t.Errorf("answer = %d %q, want 200 {\"ok\":true}", r.status, r.body)
		}
	}
	peer := "cli." + region + ".nomad"
	checkRequests(t, srv, proxyTokens(token),
		gotRequest{Method: http.MethodGet, Path: "/v1/jobs", Query: "namespace=dev&prefix=a%2Fb", Token: proxyToken,
			Peer: peer},
		gotRequest{Method: http.MethodPut, Path: "/v1/namespace/dev", Token: proxyToken, Peer: peer,
			Body: `{"Name":"dev"}`})
	wantSeen := map[string]string{"Origin": "", "Authorization": "", "Host": srv.addr()}
	for i, got := range seen.all() {
		if diff := cmp.Diff(wantSeen, got); diff != "" {
			t.Errorf("headers of request %d the server saw (-want +got):\n%s", i, diff)
		}
	}
}

func TestProxyOfAServerOfAnotherCA(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, other.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	front := startProxy(t, []string{srv.addr()}, p, token, nil)
	r := send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil)
	if r.status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", r.status)
	}
	// The next request wraps round to the only server and fails the same way.
	if r := send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil); r.status != http.StatusBadGateway {
		t.Errorf("second status = %d, want 502", r.status)
	}
	checkRequests(t, srv, proxyTokens(token))
}

// TestProxyRefusesBeforeAnythingGoesUpstream checks that a request that a page of another site could send reaches no
// server, also a blind POST whose answer the page cannot read.
func TestProxyRefusesBeforeAnythingGoesUpstream(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	front := startProxy(t, []string{srv.addr()}, p, token, nil)
	_, port, err := net.SplitHostPort(front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name, method, host string
		header             map[string]string
	}{
		{"cross-site POST", http.MethodPost, "", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"POST with a foreign Origin", http.MethodPost, "", map[string]string{"Origin": "http://evil.example"}},
		{"POST with the Origin null", http.MethodPost, "", map[string]string{"Origin": "null"}},
		{"same-site GET", http.MethodGet, "", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"a foreign Host", http.MethodGet, "evil.example:" + port, nil},
		{"a foreign Host with the right port in the Origin", http.MethodPost, "evil.example:" + port,
			map[string]string{"Origin": "http://evil.example:" + port}},
		{"the right name on another port", http.MethodGet, "127.0.0.1:1", nil},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			r := send(t, tc.method, front.URL+"/v1/namespace/evil", tc.host, `{"Name":"evil"}`, tc.header)
			if r.status != http.StatusForbidden {
				t.Errorf("status = %d, want 403", r.status)
			}
			if strings.Count(strings.TrimSpace(r.body), "\n") != 0 || strings.Contains(r.body, string(token)) {
				t.Errorf("body = %q, want one line without the token", r.body)
			}
		})
	}
	checkRequests(t, srv, proxyTokens(token))

	passed := []struct {
		name, host string
		header     map[string]string
	}{
		{"no Origin and no Sec-Fetch-Site (the Nomad CLI)", "", nil},
		{"Sec-Fetch-Site none", "", map[string]string{"Sec-Fetch-Site": "none"}},
		{"its own Origin", "", map[string]string{"Origin": front.URL, "Sec-Fetch-Site": "same-origin"}},
		{"localhost", "localhost:" + port, map[string]string{"Origin": "http://localhost:" + port}},
	}
	for _, tc := range passed {
		t.Run(tc.name, func(t *testing.T) {
			if r := send(t, http.MethodGet, front.URL+"/v1/jobs", tc.host, "", tc.header); r.status != http.StatusOK {
				t.Errorf("status = %d, want 200", r.status)
			}
		})
	}
	if got := len(srv.requests()); got != len(passed) {
		t.Errorf("the server got %d requests, want %d", got, len(passed))
	}
}

// TestProxyStreamsAnAnswer checks that a chunk arrives before the answer ends, also when the server announces the
// length of the whole answer, as it does not for a stream.
func TestProxyStreamsAnAnswer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	release := make(chan struct{})
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = io.WriteString(w, "one")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "two")
		case <-time.After(5 * time.Second):
			_, _ = io.WriteString(w, "???")
		}
	})
	front := startProxy(t, []string{srv.addr()}, p, pki.NewBootstrapSecret(), nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/v1/event/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := noRedirectClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	first := make([]byte, 3)
	if _, err := io.ReadFull(res.Body, first); err != nil || string(first) != "one" {
		t.Fatalf("first chunk = %q, %v; want one", first, err)
	}
	close(release)
	rest, err := io.ReadAll(res.Body)
	if err != nil || string(rest) != "two" {
		t.Errorf("second chunk = %q, %v; want two", rest, err)
	}
}

// TestProxyPassesAnUpgrade checks that a connection that the server upgrades carries bytes both ways.
func TestProxyPassesAnUpgrade(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if r.Header.Get("Origin") != "" {
			t.Error("the upgrade carried an Origin")
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Errorf("flush the 101: %v", err)
			return
		}
		_, _ = io.Copy(conn, rw.Reader)
	})
	var logs logBuffer
	front := startProxy(t, []string{srv.addr()}, p, token, logs.logger())

	conn, err := net.DialTimeout("tcp", front.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	host := front.Listener.Addr().String()
	_, err = fmt.Fprintf(conn, "GET /v1/client/allocation/x/exec HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\n"+
		"Connection: Upgrade\r\nUpgrade: echo\r\nX-Nomad-Token: %s\r\n\r\n", host, host, pageToken)
	if err != nil {
		t.Fatalf("write the request: %v", err)
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", res.StatusCode)
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write through the upgrade: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Errorf("echo = %q, %v; want ping", line, err)
	}
	checkRequests(t, srv, proxyTokens(token), gotRequest{Method: http.MethodGet,
		Path: "/v1/client/allocation/x/exec", Token: proxyToken, Peer: "cli." + region + ".nomad"})
	// The line is written when the upgraded connection ends.
	_ = conn.Close()
	logs.waitFor("proxy request")
	if !strings.Contains(logs.String(), "status=101") {
		t.Errorf("the log should hold status=101 for the upgrade:\n%s", logs.String())
	}
}

func TestProxyMovesToTheNextServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	front := startProxy(t, []string{dead, srv.addr()}, p, token, nil)

	statuses := make([]int, 3)
	for i := range statuses {
		statuses[i] = send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil).status
	}
	if diff := cmp.Diff([]int{502, 200, 200}, statuses); diff != "" {
		t.Errorf("statuses (-want +got):\n%s", diff)
	}
	if got := len(srv.requests()); got != 2 {
		t.Errorf("the second server got %d requests, want 2", got)
	}
}

// TestProxyKeepsTheServerAfterAnAnswer checks that an error status of a server does not move the proxy on.
func TestProxyKeepsTheServerAfterAnAnswer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	first := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusInternalServerError, "no"))
	second := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	front := startProxy(t, []string{first.addr(), second.addr()}, p, pki.NewBootstrapSecret(), nil)
	for range 2 {
		if r := send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil); r.status != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", r.status)
		}
	}
	if len(first.requests()) != 2 || len(second.requests()) != 0 {
		t.Errorf("requests: first %d, second %d; want 2 and 0", len(first.requests()), len(second.requests()))
	}
}

// TestProxyLogsTheFinalStatus checks that a 100 Continue before the answer is not the logged status.
func TestProxyLogsTheFinalStatus(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	var logs logBuffer
	front := startProxy(t, []string{srv.addr()}, p, pki.NewBootstrapSecret(), logs.logger())
	conn, err := net.DialTimeout("tcp", front.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "PUT /v1/namespace/dev HTTP/1.1\r\nHost: %s\r\nExpect: 100-continue\r\n"+
		"Content-Length: 2\r\nConnection: close\r\n\r\n", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("write the request: %v", err)
	}
	br := bufio.NewReader(conn)
	for wrote := false; ; {
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read the answer: %v", err)
		}
		if res.StatusCode == http.StatusContinue {
			if !wrote { // a second 100 must not get the body again
				if _, err := io.WriteString(conn, "{}"); err != nil {
					t.Fatalf("write the body: %v", err)
				}
				wrote = true
			}
			continue
		}
		_, _ = io.Copy(io.Discard, res.Body)
		break
	}
	logs.waitFor("proxy request")
	if !strings.Contains(logs.String(), "status=200") || strings.Contains(logs.String(), "status=100") {
		t.Errorf("the log should hold status=200 and not status=100:\n%s", logs.String())
	}
}

// TestProxyStaysOnTheServerWhenTheCallerGivesUp checks that a caller that cancels does not move the proxy on.
func TestProxyStaysOnTheServerWhenTheCallerGivesUp(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	started := make(chan struct{})
	first := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/slow" {
			answer(http.StatusOK, "{}")(w, r)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	second := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	var logs logBuffer
	front := startProxy(t, []string{first.addr(), second.addr()}, p, pki.NewBootstrapSecret(), logs.logger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/v1/slow", nil)
		if err != nil {
			return
		}
		if res, err := noRedirectClient(t).Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-done:
		t.Fatal("the request never reached the first server")
	}
	cancel()
	<-done
	// The proxy has handled the request once it logs it.
	logs.waitFor("path=/v1/slow")
	if r := send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil); r.status != http.StatusOK {
		t.Errorf("status = %d, want 200", r.status)
	}
	if got := len(second.requests()); got != 0 {
		t.Errorf("the second server got %d requests, want 0", got)
	}
}

func TestProxyDoesNotFollowARedirect(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/v1/other")
		w.WriteHeader(http.StatusFound)
	})
	front := startProxy(t, []string{srv.addr()}, p, pki.NewBootstrapSecret(), nil)
	r := send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", nil)
	if r.status != http.StatusFound || r.header.Get("Location") != "/v1/other" {
		t.Errorf("answer = %d, Location %q; want 302, /v1/other", r.status, r.header.Get("Location"))
	}
	if got := len(srv.requests()); got != 1 {
		t.Errorf("the server got %d requests, want 1", got)
	}
}

func TestNewProxyRefusesABadConfig(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	good := proxyConfig([]string{"203.0.113.5:4646", "203.0.113.6:4646"}, p, token, "127.0.0.1:4646")
	notHostPort := func(addr string) string { return fmt.Sprintf("nomad: address %q is not host:port", addr) }
	badListen := func(addr string) string { return fmt.Sprintf("nomad: listen address %q is not host:port", addr) }
	cases := []struct {
		name   string
		change func(*nomadops.ProxyConfig)
		want   string
	}{
		{"no servers", func(c *nomadops.ProxyConfig) { c.Servers = nil }, "nomad: no servers"},
		{"a bad second server", func(c *nomadops.ProxyConfig) { c.Servers[1] = "203.0.113.6" },
			notHostPort("203.0.113.6")},
		{"a URL as a server", func(c *nomadops.ProxyConfig) { c.Servers = []string{"https://203.0.113.5:4646"} },
			notHostPort("https://203.0.113.5:4646")},
		{"no region", func(c *nomadops.ProxyConfig) { c.Region = "" }, "nomad: no region"},
		{"no CA", func(c *nomadops.ProxyConfig) { c.CA = nil }, "nomad: the CA bundle holds no certificate"},
		{"another certificate's key", func(c *nomadops.ProxyConfig) { c.Cert.Key = other.operator.Key },
			"nomad: the operator certificate: tls: private key does not match public key"},
		{"no token", func(c *nomadops.ProxyConfig) { c.Token = nil }, "nomad: no ACL token"},
		{"a line end after the token", func(c *nomadops.ProxyConfig) { c.Token = secret.Secret(string(token) + "\n") },
			"nomad: the ACL token has a control character"},
		{"no listen address", func(c *nomadops.ProxyConfig) { c.Listen = "" }, badListen("")},
		{"no port to listen on", func(c *nomadops.ProxyConfig) { c.Listen = "127.0.0.1" }, badListen("127.0.0.1")},
		{"a URL to listen on", func(c *nomadops.ProxyConfig) { c.Listen = "http://127.0.0.1:4646" },
			badListen("http://127.0.0.1:4646")},
		{"port 0", func(c *nomadops.ProxyConfig) { c.Listen = "127.0.0.1:0" }, badListen("127.0.0.1:0")},
	}
	secrets := map[string]secret.Secret{
		"the token": token, "the operator key": p.operator.Key, "another key": other.operator.Key,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			cfg.Servers = append([]string(nil), good.Servers...)
			tc.change(&cfg)
			h, err := nomadops.NewProxy(cfg)
			if err == nil {
				t.Fatalf("NewProxy() = %v, want the error %q", h, tc.want)
			}
			if text := show(t, err, secrets); text != tc.want {
				t.Errorf("NewProxy() error = %s, want %q", text, tc.want)
			}
		})
	}
	if _, err := nomadops.NewProxy(good); err != nil {
		t.Errorf("NewProxy() of the good config: %v", err)
	}
	ipv6 := good
	ipv6.Listen = "[::1]:4646"
	if _, err := nomadops.NewProxy(ipv6); err != nil {
		t.Errorf("NewProxy() on an IPv6 address: %v", err)
	}
}

func TestProxyConfigPrintsItsServers(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cfg := proxyConfig([]string{"203.0.113.5:4646", "203.0.113.6:4646"}, p, token, "127.0.0.1:4646")
	var logs logBuffer
	cfg.Log = logs.logger()
	secrets := map[string][]byte{"the token": token, "the operator key": p.operator.Key}
	secrettest.CheckHidden(t, secrettest.Printed(t, cfg), secrets, "")
	want := "nomadops.ProxyConfig(servers=[203.0.113.5:4646 203.0.113.6:4646])"
	if got := fmt.Sprintf("%v", cfg); got != want {
		t.Errorf("config prints as %q, want %q", got, want)
	}
}

// TestProxyShowsNoSecret checks the answers and the log lines of every kind of request.
func TestProxyShowsNoSecret(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	other := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	good := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	bad := newNomadServer(t, other.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	var logs logBuffer
	front := startProxy(t, []string{bad.addr(), good.addr()}, p, token, logs.logger())
	header := map[string]string{"X-Nomad-Token": pageToken, "Authorization": "Bearer " + pageToken}
	forbidden := map[string]string{"X-Nomad-Token": pageToken, "Origin": "http://evil.example"}
	replies := []reply{
		send(t, http.MethodGet, front.URL+"/v1/jobs?token="+queryToken, "", "", header),
		send(t, http.MethodGet, front.URL+"/v1/jobs", "", "", header),
		send(t, http.MethodPost, front.URL+"/v1/jobs", "", "", forbidden),
	}
	if got := []int{replies[0].status, replies[1].status, replies[2].status}; !cmp.Equal(got, []int{502, 200, 403}) {
		t.Errorf("statuses = %v, want [502 200 403]", got)
	}
	secrets := map[string][]byte{"the token": token, "the operator key": p.operator.Key,
		"the token of the page":  []byte(pageToken),
		"the token in the query": []byte(queryToken)}
	for _, status := range []string{"status=502", "status=200", "status=403"} {
		logs.waitFor(status) // the secrets are looked for in the lines of all three requests
	}
	outputs := map[string]string{"the log": logs.String()}
	for i, r := range replies {
		outputs[fmt.Sprintf("the answer %d", i)] = r.body + fmt.Sprint(r.header)
	}
	secrettest.CheckHidden(t, outputs, secrets, "")
	for _, want := range []string{"method=GET", "path=/v1/jobs", "status=200", "status=403", "status=502"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}
}
