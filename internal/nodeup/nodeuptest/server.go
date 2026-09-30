package nodeuptest

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// Server is an HTTPS server of what a machine downloads in a test. It answers each path with its handler, and 404
// where there is none, and records the path and the query of every request. Its Client trusts it.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

// Serve starts a Server of the handlers, by path, which closes when t ends.
func Serve(t testing.TB, handlers map[string]http.HandlerFunc) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(s.Close)
	return s
}

// Requests returns the path and the query of every request, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}
