package nodeuptest_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
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
