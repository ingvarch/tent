package nodeuptest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// Server is an HTTPS server of what a machine downloads or asks in a test. It answers each path with its handler, and
// 404 where there is none, and records the path and the query of every request. Its Client trusts it.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	dialed   []string
}

// Serve starts a Server of the handlers, by path, which closes when t ends.
func Serve(t testing.TB, handlers map[string]http.HandlerFunc) *Server {
	t.Helper()
	s := newServer(handlers)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// ServeMTLS starts a Server of the handlers, as Serve does, with the certificate cert and its key, both PEM, that
// answers only clients with a certificate of the CA bundle ca. A Nomad agent asks for one only with
// verify_https_client; the Server always does, so that a test proves that the client sends its certificate. Its
// Client has none.
func ServeMTLS(t testing.TB, ca, cert, key []byte, handlers map[string]http.HandlerFunc) *Server {
	t.Helper()
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("nodeuptest: the CA bundle has no certificates")
	}
	s := newServer(handlers)
	s.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// newServer returns a Server of the handlers that is not started yet.
func newServer(handlers map[string]http.HandlerFunc) *Server {
	s := &Server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.RequestURI())
		s.mu.Unlock()
		handler, ok := handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	return s
}

// Requests returns the path and the query of every request, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Dial connects to the server whatever address it is asked for, and records the address: it stands in for a
// machine's Host.DialContext, as a test cannot bind the addresses that the machine dials.
func (s *Server) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s.mu.Lock()
	s.dialed = append(s.dialed, addr)
	s.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, s.Listener.Addr().String())
}

// Agent is a fake Nomad agent's HTTP API, served as ServeMTLS serves it. /v1/status/peers answers the peers of
// SetPeers, or 500 until then; /v1/status/leader answers that there is no leader yet, as a server does with ?stale
// before the cluster has one; /v1/agent/health answers 500 to the first answers that SetUnhealthy counts, then 200.
type Agent struct {
	*Server
	mu        sync.Mutex
	peers     []string
	unhealthy int // the answers of /v1/agent/health that are 500; below 0 all are
}

// ServeAgent starts an Agent with the certificate cert and its key of the CA bundle ca, all PEM.
func ServeAgent(t testing.TB, ca, cert, key []byte) *Agent {
	t.Helper()
	a := &Agent{}
	a.Server = ServeMTLS(t, ca, cert, key, map[string]http.HandlerFunc{
		"/v1/status/peers": a.servePeers,
		"/v1/status/leader": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`""`))
		},
		"/v1/agent/health": a.serveHealth,
	})
	return a
}

// SetPeers sets the answer of /v1/status/peers; nil answers 500.
func (a *Agent) SetPeers(peers []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.peers = peers
}

// SetUnhealthy sets how many next answers of /v1/agent/health are 500; below 0, all are.
func (a *Agent) SetUnhealthy(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unhealthy = n
}

func (a *Agent) servePeers(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	peers := a.peers
	a.mu.Unlock()
	if peers == nil {
		http.Error(w, "no peers", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(peers)
}

func (a *Agent) serveHealth(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	unhealthy := a.unhealthy
	if unhealthy > 0 {
		a.unhealthy--
	}
	a.mu.Unlock()
	if unhealthy != 0 {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"client":{"ok":false,"message":"no known servers"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"client":{"ok":true}}`))
}

// Hang is a Host.DialContext that answers nothing until its context ends, as a server behind a broken network: a
// call gives up at its own deadline.
func Hang(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Dialed returns the addresses that Dial was asked for, in order.
func (s *Server) Dialed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.dialed)
}
