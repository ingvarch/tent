package nomadops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
)

// ProxyConfig is how NewProxy reaches the cluster's servers and who it is there.
type ProxyConfig struct {
	// Servers are the host and port of each server's HTTP API, such as 203.0.113.5:4646. The proxy uses the first
	// one until a request reaches no server, and then the next one.
	Servers []string
	// Region is the Nomad region. Each server must show a certificate for server.<region>.nomad.
	Region string
	// CA is the cluster's CA bundle, PEM, which the servers' certificates must chain to.
	CA []byte
	// Cert is the operator certificate, for cli.<region>.nomad, and its key. The proxy shows it to the servers.
	Cert pki.Certificate
	// Token is the secret of the ACL token that the proxy sends with every request.
	Token secret.Secret
	// Listen is the host and port that the handler is served on, as callers reach it, such as 127.0.0.1:4646.
	Listen string
	// Log gets one line for each request and one for each request that reached no server. It is nil for no log.
	Log *slog.Logger
}

// Format prints the config as its servers, whatever the verb.
func (c ProxyConfig) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "nomadops.ProxyConfig(servers=%v)", c.Servers)
}

// proxy is the handler of NewProxy.
type proxy struct {
	guard   hostGuard
	servers []*url.URL
	current atomic.Int64 // the index of the server that requests go to
	rp      *httputil.ReverseProxy
	log     *slog.Logger
}

// serverIndexKey is the context key of the index of the server that a request goes to.
type serverIndexKey struct{}

// NewProxy returns a handler that passes each request to one of the cluster's servers over mutual TLS, with the token
// of cfg in place of any token that the request carries. Its callers need no certificate and no token, so it must be
// served on a loopback address only.
//
// A request whose Host is not the address that Listen names, or localhost with its port, is refused with 403, and so
// is one whose Origin is not that Host or whose Sec-Fetch-Site is neither same-origin nor none, before anything goes
// to a server: a page of another site cannot use the handler. A request with neither header, such as the Nomad CLI's,
// passes.
//
// The request's Origin and Authorization are not passed on, the answer is streamed as it comes and an upgraded
// connection, such as the exec of a task, carries bytes both ways. A redirect goes back to the caller as it is. A
// request that reaches no server gets 502, and the next request goes to the next server. NewProxy fails for the
// reasons that New does, with no servers and with a Listen that is not host:port. The NOMAD_* variables have no effect
// on the handler, and its errors, its own answers (403 and 502) and its log never show the key or a token.
func NewProxy(cfg ProxyConfig) (http.Handler, error) {
	if len(cfg.Servers) == 0 {
		return nil, errors.New("nomad: no servers")
	}
	servers := make([]*url.URL, len(cfg.Servers))
	for i, addr := range cfg.Servers {
		u, ok := hostPortURL(addr)
		if !ok {
			return nil, fmt.Errorf("nomad: address %q is not host:port", addr)
		}
		servers[i] = u
	}
	if err := checkCluster(cfg.Region, cfg.CA, cfg.Cert, cfg.Token); err != nil {
		return nil, err
	}
	guard, err := newHostGuard(cfg.Listen)
	if err != nil {
		return nil, err
	}
	hc, err := clusterHTTPClient(cfg.Region, cfg.CA, cfg.Cert)
	if err != nil {
		return nil, err
	}
	p := &proxy{guard: guard, servers: servers, log: cfg.Log}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	token := string(cfg.Token)
	p.rp = &httputil.ReverseProxy{
		Transport:     hc.Transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(servers[serverIndex(pr.In.Context())])
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("X-Nomad-Token", token)
		},
		ErrorHandler: p.noServer,
	}
	return p, nil
}

// serverIndex returns the index of the server that the request of ctx goes to.
func serverIndex(ctx context.Context) int64 {
	i, _ := ctx.Value(serverIndexKey{}).(int64)
	return i
}

// ServeHTTP refuses the request or passes it to a server.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		// Never a header, and never the query: a token may travel in it.
		p.log.Info("proxy request", "method", r.Method, "path", r.URL.Path, "status", sw.statusOrOK())
	}()
	if reason := p.guard.refusal(r.Host, r.Header); reason != "" {
		http.Error(sw, reason, http.StatusForbidden)
		return
	}
	ctx := context.WithValue(r.Context(), serverIndexKey{}, p.current.Load())
	p.rp.ServeHTTP(sw, r.WithContext(ctx))
}

// noServer answers a request that reached no server and moves the next request on to the next server.
func (p *proxy) noServer(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		i := serverIndex(r.Context())
		p.current.CompareAndSwap(i, (i+1)%int64(len(p.servers)))
		p.log.Warn("proxy: a request failed; the next goes to the next server", "server", p.servers[i].Host,
			"error", err)
	}
	http.Error(w, "bad gateway: no Nomad server answered", http.StatusBadGateway)
}

// statusWriter remembers the status that a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status < http.StatusOK {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the flusher of the writer underneath.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack takes over the connection, as an upgrade does, and remembers the upgrade's status: the reverse proxy writes
// that status on the connection itself.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (w *statusWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// hostGuard decides which requests may use the proxy. It holds the Host values that are the proxy's own.
type hostGuard struct {
	own map[string]bool // canonical host:port
}

// newHostGuard returns the guard of a proxy that is served on listen, which must be host:port.
func newHostGuard(listen string) (hostGuard, error) {
	u, ok := hostPortURL(listen)
	if !ok {
		return hostGuard{}, fmt.Errorf("nomad: listen address %q is not host:port", listen)
	}
	own, _ := canonicalHostPort(u.Host)
	local, _ := canonicalHostPort("localhost:" + u.Port())
	return hostGuard{own: map[string]bool{own: true, local: true}}, nil
}

// refusal returns why a request with the Host and the headers must not be served, or "" when it may be. It refuses
// a Host that is not the proxy's own, an Origin that is not that Host, and a Sec-Fetch-Site other than same-origin
// and none. A header that is set twice, or set empty, counts as set to a value that never matches. A request with
// neither Origin nor Sec-Fetch-Site passes: such a request is not a browser's, or not a cross-site one.
func (g hostGuard) refusal(host string, h http.Header) string {
	reqHost, ok := canonicalHostPort(host)
	if !ok || !g.own[reqHost] {
		return "forbidden: unexpected Host"
	}
	if origins, set := h["Origin"]; set && !originIs(origins, reqHost) {
		return "forbidden: unexpected Origin"
	}
	if sites, set := h["Sec-Fetch-Site"]; set && (len(sites) != 1 || (sites[0] != "same-origin" && sites[0] != "none")) {
		return "forbidden: cross-site request"
	}
	return ""
}

// originIs reports whether the Origin headers are one http origin, the host and port of which are hostPort.
func originIs(origins []string, hostPort string) bool {
	if len(origins) != 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		u.Opaque != "" {
		return false
	}
	got, ok := canonicalHostPort(u.Host)
	return ok && got == hostPort
}

// canonicalHostPort returns hostPort in the form that two spellings of one address share: a lower-case host without
// a trailing dot, an IP address in its short form and a decimal port. A missing port is port 80. It fails when the
// text is not a host with an optional port.
func canonicalHostPort(hostPort string) (string, bool) {
	host, port := hostPort, "80"
	switch {
	case strings.HasPrefix(hostPort, "[") && strings.HasSuffix(hostPort, "]"):
		host = hostPort[1 : len(hostPort)-1]
	case strings.Contains(hostPort, ":"):
		var err error
		if host, port, err = net.SplitHostPort(hostPort); err != nil {
			return "", false
		}
	}
	n, err := strconv.Atoi(port)
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if err != nil || n < 1 || n > 65535 || host == "" {
		return "", false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String() // an IPv4-mapped IPv6 address stays what it is: it is not the IPv4 listener's own
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), true
}
