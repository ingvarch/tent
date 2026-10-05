package nodeuptest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/pki"
)

func TestServe(t *testing.T) {
	srv := nodeuptest.Serve(t, map[string]http.HandlerFunc{
		"/cni.tgz": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "the archive") },
	})
	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Over TLS, with the server's own client.
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	if code, body := get("/cni.tgz?X-Amz-Signature=1"); code != http.StatusOK || body != "the archive" {
		t.Errorf("GET /cni.tgz: %d %q, want 200 and the archive", code, body)
	}
	if code, _ := get("/missing"); code != http.StatusNotFound {
		t.Errorf("GET /missing: %d, want 404", code)
	}
	if diff := cmp.Diff([]string{"/cni.tgz?X-Amz-Signature=1", "/missing"}, srv.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// testPKI returns a CA of a test cluster, a server's certificate of it, and the key pair of a client's.
func testPKI(t *testing.T) (*pki.CA, pki.Certificate, tls.Certificate) {
	t.Helper()
	ca, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.IssueNode(v1alpha1.RoleServer, "global", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	node, err := ca.IssueNode(v1alpha1.RoleClient, "global", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(node.Cert, node.Key.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return ca, server, pair
}

// get calls GET path at 127.0.0.1:4646, which srv's Dial reaches, as a client of the CA bundle ca with certs, and
// returns the status and the body.
func get(t *testing.T, srv *nodeuptest.Server, ca []byte, path string, certs ...tls.Certificate) (int, string, error) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", Certificates: certs},
		DialContext:     srv.Dial,
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1:4646"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func TestServeMTLS(t *testing.T) {
	ca, server, pair := testPKI(t)
	srv := nodeuptest.ServeMTLS(t, ca.Bundle(), server.Cert, server.Key.Bytes(), map[string]http.HandlerFunc{
		"/v1/agent/health": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") },
	})
	if _, body, err := get(t, srv, ca.Bundle(), "/v1/agent/health", pair); err != nil || body != "ok" {
		t.Errorf("GET with the node's certificate: %q, %v; want ok", body, err)
	}
	// Without a certificate of the CA, the server answers nothing.
	if _, _, err := get(t, srv, ca.Bundle(), "/v1/agent/health"); err == nil {
		t.Error("GET without a client certificate passed")
	}
	if diff := cmp.Diff([]string{"/v1/agent/health"}, srv.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"127.0.0.1:4646", "127.0.0.1:4646"}, srv.Dialed()); diff != "" {
		t.Errorf("dialed (-want +got):\n%s", diff)
	}
}

func TestServeAgent(t *testing.T) {
	ca, server, pair := testPKI(t)
	a := nodeuptest.ServeAgent(t, ca.Bundle(), server.Cert, server.Key.Bytes())
	ask := func(path string) string {
		t.Helper()
		code, body, err := get(t, a.Server, ca.Bundle(), path, pair)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d %s", code, strings.TrimSpace(body))
	}
	var got []string
	got = append(got, ask("/v1/status/peers?stale"))
	a.SetPeers([]string{})
	got = append(got, ask("/v1/status/peers?stale"))
	a.SetPeers([]string{"10.64.0.5:4647"})
	got = append(got, ask("/v1/status/peers?stale"), ask("/v1/status/leader?stale"))
	a.SetUnhealthy(1)
	got = append(got, ask("/v1/agent/health?type=client"), ask("/v1/agent/health?type=client"))
	a.SetUnhealthy(-1)
	got = append(got, ask("/v1/agent/health?type=client"), ask("/v1/agent/health?type=client"))
	unhealthy := `500 {"client":{"ok":false,"message":"no known servers"}}`
	want := []string{
		"500 no peers", "200 []", `200 ["10.64.0.5:4647"]`, `200 ""`,
		unhealthy, `200 {"client":{"ok":true}}`, unhealthy, unhealthy,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the answers (-want +got):\n%s", diff)
	}
}

func TestHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := nodeuptest.Hang(ctx, "tcp", "10.64.0.5:4646")
		if conn != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 5*time.Second {
			t.Errorf("Hang = %v, %v after %s; want no connection and the context's end after 5s", conn, err,
				time.Since(start))
		}
	})
}
