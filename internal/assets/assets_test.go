package assets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// serve starts a release server with the files, by path. A path in cut sends half of its file and then drops the
// connection; any other path is 404.
func serve(t *testing.T, files map[string]string, cut ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		switch {
		case !ok:
			http.NotFound(w, r)
		case slices.Contains(cut, r.URL.Path):
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write([]byte(body[:len(body)/2]))
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler) // the server drops the connection
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sites answers requests from its files by full URL, without a network, for tests of the default URLs. Any other URL
// is 404.
type sites map[string]string

func (s sites) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := s[r.URL.String()]
	code := http.StatusOK
	if !ok {
		code = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// wantErr fails the test unless err's text holds every part.
func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one that says %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not say %q", err, p)
		}
	}
}

func TestGet(t *testing.T) {
	largest := strings.Repeat("x", maxFileSize)
	srv := serve(t, map[string]string{"/ok": "hello", "/largest": largest, "/big": largest + "x", "/cut": "0123456789"},
		"/cut")
	opts := Options{Client: srv.Client()}

	for path, want := range map[string]string{"/ok": "hello", "/largest": largest} {
		data, err := opts.get(t.Context(), srv.URL+path)
		if err != nil || string(data) != want {
			t.Errorf("get %s = %d bytes, %v; want %d bytes", path, len(data), err, len(want))
		}
	}

	for _, tc := range []struct{ name, path, want string }{
		{"missing", "/missing", "404 Not Found"},
		{"too large", "/big", "larger than 1048576 bytes"},
		{"cut connection", "/cut", "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := opts.get(t.Context(), srv.URL+tc.path)
			wantErr(t, err, srv.URL+tc.path, tc.want)
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := opts.get(ctx, srv.URL+"/ok")
		wantErr(t, err, srv.URL+"/ok")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("get with a cancelled context = %v, want context.Canceled", err)
		}
	})
}

func TestGetFollowsRedirectsOnlyToHTTPS(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("get followed a redirect to plain http: %s", r.URL)
	}))
	t.Cleanup(plain.Close)
	var tlsSrv *httptest.Server
	tlsSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("hello"))
		case "/to-https":
			http.Redirect(w, r, tlsSrv.URL+"/ok", http.StatusFound)
		case "/to-http":
			http.Redirect(w, r, plain.URL+"/ok", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, tlsSrv.URL+"/loop", http.StatusFound)
		}
	}))
	t.Cleanup(tlsSrv.Close)
	opts := Options{Client: tlsSrv.Client()}

	data, err := opts.get(t.Context(), tlsSrv.URL+"/to-https")
	if err != nil || string(data) != "hello" {
		t.Errorf("get /to-https = %q, %v; want hello", data, err)
	}
	_, err = opts.get(t.Context(), tlsSrv.URL+"/to-http")
	wantErr(t, err, tlsSrv.URL+"/to-http", "redirected to "+plain.URL+"/ok, which is not https")
	_, err = opts.get(t.Context(), tlsSrv.URL+"/loop")
	wantErr(t, err, tlsSrv.URL+"/loop", "stopped after 10 redirects")

	// A client with its own redirect policy keeps it.
	own := *tlsSrv.Client()
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, err = Options{Client: &own}.get(t.Context(), tlsSrv.URL+"/to-https")
	wantErr(t, err, tlsSrv.URL+"/to-https", "302 Found")
}
