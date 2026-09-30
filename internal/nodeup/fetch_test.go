package nodeup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/secrettest"
)

// Where tent-node keeps the assets that it downloaded, and the file of the cni-plugins asset there.
const (
	tentDir   = "/var/lib/tent"
	assetDir  = tentDir + "/assets"
	cachePath = assetDir + "/cni-plugins"
)

// payload is the content of an asset in the tests of fetch.
var payload = []byte("the CNI plugins, as a stand-in\n")

// signature is the signature in the query of a presigned URL, which neither errors nor logs may show.
const signature = "secret"

// sum returns the sha256 of data in hex.
func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// cniAsset returns version 1.9.1 of the cni-plugins asset with the sha256 of data, from urls.
func cniAsset(data []byte, urls ...string) nodeconfig.Asset {
	return nodeconfig.Asset{Name: nodeconfig.CNIPluginsAsset, Version: "1.9.1", URLs: urls, SHA256: sum(data)}
}

// serve starts a server of the handlers, by path, and gives h a transport that trusts it.
func serve(t *testing.T, h *nodeup.Host, handlers map[string]http.HandlerFunc) *nodeuptest.Server {
	t.Helper()
	srv := nodeuptest.Serve(t, handlers)
	h.Transport = srv.Client().Transport
	return srv
}

// file answers with data.
func file(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }
}

// status answers with the status alone.
func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// stream answers with data without a Content-Length, as a stream of chunks.
func stream(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(data)
	}
}

// entries returns a copy of the entries at the paths of fsys, by path; a missing one is left out.
func entries(fsys *nodeuptest.FS, paths ...string) map[string]nodeuptest.Entry {
	got := map[string]nodeuptest.Entry{}
	for _, p := range paths {
		if e, ok := fsys.Entry(p); ok {
			got[p] = e
		}
	}
	return got
}

// cached returns the entries of the cache when it holds data.
func cached(data []byte) map[string]nodeuptest.Entry {
	return map[string]nodeuptest.Entry{
		tentDir:   {Dir: true, Mode: 0o700, Owner: nodeconfig.Owner},
		assetDir:  {Dir: true, Mode: 0o700, Owner: nodeconfig.Owner},
		cachePath: {Data: data, Mode: 0o600, Owner: nodeconfig.Owner},
	}
}

func TestFetch(t *testing.T) {
	h, fsys, _ := machine(t)
	logs := captureLog(h)
	srv := serve(t, h, map[string]http.HandlerFunc{"/cni.tgz": file(payload)})
	a := cniAsset(payload, srv.URL+"/cni.tgz")
	data, changed, err := nodeup.Fetch(t.Context(), h, a)
	if err != nil || !changed || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %t, %v; want the payload and a change", data, changed, err)
	}
	if diff := cmp.Diff([]string{"/cni.tgz"}, srv.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(cached(payload), entries(fsys, tentDir, assetDir, cachePath)); diff != "" {
		t.Errorf("the cache (-want +got):\n%s", diff)
	}
	if !strings.Contains(logs.String(), ` level=INFO msg=download asset=cni-plugins version=1.9.1`) {
		t.Errorf("Fetch did not log the download:\n%s", logs)
	}

	// The second fetch takes the cached file.
	changes := len(fsys.Changes())
	data, changed, err = nodeup.Fetch(t.Context(), h, a)
	if err != nil || changed || !bytes.Equal(data, payload) {
		t.Fatalf("the second Fetch: %q, %t, %v; want the payload and no change", data, changed, err)
	}
	if got := srv.Requests(); len(got) != 1 {
		t.Errorf("the second Fetch asked for %q, want nothing", got[1:])
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second Fetch changed %q, want nothing", got)
	}
}

// addCache puts data into the cache of the cni-plugins asset, with the mode, as a test's setup.
func addCache(t *testing.T, fsys *nodeuptest.FS, data []byte, mode fs.FileMode) {
	t.Helper()
	fsys.AddDir(t, tentDir, 0o700, nodeconfig.Owner)
	fsys.AddDir(t, assetDir, 0o700, nodeconfig.Owner)
	fsys.AddFile(t, cachePath, data, mode, nodeconfig.Owner)
}

func TestFetchReplacesAnotherCachedFile(t *testing.T) {
	corrupt := bytes.Clone(payload)
	corrupt[0] ^= 1
	for _, c := range []struct {
		name string
		old  []byte
	}{
		{"a corrupt file", corrupt},
		{"an older version", []byte("the CNI plugins 1.9.0\n")},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, _ := machine(t)
			addCache(t, fsys, c.old, 0o600)
			srv := serve(t, h, map[string]http.HandlerFunc{"/cni.tgz": file(payload)})
			data, changed, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/cni.tgz"))
			if err != nil || !changed || !bytes.Equal(data, payload) {
				t.Fatalf("Fetch: %q, %t, %v; want the payload and a change", data, changed, err)
			}
			if got := srv.Requests(); len(got) != 1 {
				t.Errorf("requests %q, want one", got)
			}
			// The file goes before the download, which writes it anew.
			if diff := cmp.Diff([]string{cachePath, cachePath}, fsys.Changes()); diff != "" {
				t.Errorf("changes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(cached(payload), entries(fsys, tentDir, assetDir, cachePath)); diff != "" {
				t.Errorf("the cache (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchRemovesAnotherCachedFileWhenTheDownloadFails(t *testing.T) {
	h, fsys, _ := machine(t)
	addCache(t, fsys, []byte("the CNI plugins 1.9.0\n"), 0o600)
	srv := serve(t, h, nil)
	if _, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/cni.tgz")); err == nil {
		t.Fatal("Fetch of a missing file succeeded")
	}
	if _, ok := fsys.Entry(cachePath); ok {
		t.Error("the older version stayed in the cache")
	}
}

func TestFetchMakesTheCachedFileRootOnly(t *testing.T) {
	h, fsys, _ := machine(t)
	addCache(t, fsys, payload, 0o644)
	srv := serve(t, h, nil)
	data, changed, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/cni.tgz"))
	if err != nil || !changed || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %t, %v; want the payload and a change", data, changed, err)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Errorf("Fetch asked for %q, want nothing", got)
	}
	if diff := cmp.Diff(cached(payload), entries(fsys, tentDir, assetDir, cachePath)); diff != "" {
		t.Errorf("the cache (-want +got):\n%s", diff)
	}
}

func TestFetchTriesTheNextURL(t *testing.T) {
	other := []byte("another file\n")
	for _, c := range []struct {
		name    string
		first   http.HandlerFunc // nil for none, which answers 404
		warning string
	}{
		{"a wrong sha256", file(other), "the sha256 is " + sum(other) + ", not " + sum(payload)},
		{"404", nil, "answered 404 Not Found"},
		{"403", status(http.StatusForbidden), "answered 403 Forbidden"},
		{"501", status(http.StatusNotImplemented), "answered 501 Not Implemented"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, _ := machine(t)
			logs := captureLog(h)
			handlers := map[string]http.HandlerFunc{"/b": file(payload)}
			if c.first != nil {
				handlers["/a"] = c.first
			}
			srv := serve(t, h, handlers)
			data, changed, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/a", srv.URL+"/b"))
			if err != nil || !changed || !bytes.Equal(data, payload) {
				t.Fatalf("Fetch: %q, %t, %v; want the payload and a change", data, changed, err)
			}
			// The first URL gets one try.
			if diff := cmp.Diff([]string{"/a", "/b"}, srv.Requests()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
			lines := warnings(logs)
			want := `msg="download failed" url=` + srv.URL + `/a try=1 error="` + c.warning + `"`
			if len(lines) != 1 || !strings.Contains(lines[0], want) {
				t.Errorf("warnings %q, want one with %q", lines, want)
			}
			if diff := cmp.Diff(cached(payload), entries(fsys, tentDir, assetDir, cachePath)); diff != "" {
				t.Errorf("the cache (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchFollowsRedirects(t *testing.T) {
	h, _, _ := machine(t)
	signed := "/objects/cni.tgz?X-Amz-Signature=" + signature
	srv := serve(t, h, map[string]http.HandlerFunc{
		"/releases/cni.tgz": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, signed, http.StatusFound) },
		"/objects/cni.tgz":  file(payload),
	})
	data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/releases/cni.tgz"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %v; want the payload", data, err)
	}
	if diff := cmp.Diff([]string{"/releases/cni.tgz", signed}, srv.Requests()); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

// hops answers /hop?n=k, for k from 0, with a redirect to /hop?n=k+1 while k is below n, and then with data. The
// redirects carry a signature.
func hops(n int, data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if k < n {
			http.Redirect(w, r, fmt.Sprintf("/hop?n=%d&X-Amz-Signature=%s", k+1, signature), http.StatusFound)
			return
		}
		_, _ = w.Write(data)
	}
}

func TestFetchFollowsTenRedirects(t *testing.T) {
	h, _, _ := machine(t)
	srv := serve(t, h, map[string]http.HandlerFunc{"/hop": hops(10, payload)})
	data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/hop"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %v; want the payload", data, err)
	}
	if got := srv.Requests(); len(got) != 11 {
		t.Errorf("%d requests, want 11", len(got))
	}
}

func TestFetchRefusesABadRedirect(t *testing.T) {
	redirect := func(location string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if location != "" {
				w.Header().Set("Location", location)
			}
			w.WriteHeader(http.StatusFound)
		}
	}
	for _, c := range []struct {
		name     string
		handler  http.HandlerFunc
		requests int // of the first URL
		want     string
	}{
		{"a Location that does not parse", redirect("https://objects.example.com/cni%zz.tgz?X-Amz-Signature=" +
			signature), 1, "answered 302 Found with a Location that does not parse"},
		{"no Location", redirect(""), 1, "answered 302 Found without a Location"},
		{"more than 10 redirects", hops(11, payload), 11, "more than 10 redirects"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := machine(t)
			logs := captureLog(h)
			srv := serve(t, h, map[string]http.HandlerFunc{"/hop": c.handler})
			_, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/hop", srv.URL+"/b"))
			// The first URL gets one try, and the next one its turn.
			want := "fetch cni-plugins 1.9.1: GET " + srv.URL + "/hop: " + c.want + "\nGET " + srv.URL +
				"/b: answered 404 Not Found"
			if errText(err) != want {
				t.Errorf("Fetch: %q, want %q", errText(err), want)
			}
			if got := srv.Requests(); len(got) != c.requests+1 || got[len(got)-1] != "/b" {
				t.Errorf("requests %q, want %d of /hop and then /b", got, c.requests)
			}
			secrettest.CheckHidden(t, map[string]string{"the error": errText(err), "the logs": logs.String()},
				map[string][]byte{"the URL's signature": []byte(signature)}, "")
		})
	}
}

func TestFetchSendsTheUserOnlyToItsHost(t *testing.T) {
	const url = "https://tent:pw@a.example.com/x"
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tent:pw"))
	for _, c := range []struct {
		name, location, next string
		want                 []string // the Authorization of each request
	}{
		{"a relative redirect", "/y", "https://tent:pw@a.example.com/y", []string{auth, auth}},
		{"a redirect to another host", "https://objects.example.com/y", "https://objects.example.com/y",
			[]string{auth, ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, _ := machine(t)
				var got []string
				m := useMemory(h, func(req *http.Request) (*http.Response, error) {
					got = append(got, req.Header.Get("Authorization"))
					if req.URL.Path == "/x" {
						resp := reply(req, http.StatusFound, http.NoBody)
						resp.Header.Set("Location", c.location)
						return resp, nil
					}
					return reply(req, http.StatusOK, io.NopCloser(bytes.NewReader(payload))), nil
				})
				if _, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, url)); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff([]asked{{url, 0}, {c.next, 0}}, m.Asked()); diff != "" {
					t.Errorf("requests (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(c.want, got); diff != "" {
					t.Errorf("Authorization (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestFetchTakesTheFileAsItIs(t *testing.T) {
	// A server that gzips when asked marks the .tgz as gzip-encoded, and a transport that decodes it would give the
	// tar inside, whose sha256 is another.
	h, _, _ := machine(t)
	tgz := nodeuptest.Tgz(t, pluginFile("bridge", 0o755))
	var mu sync.Mutex
	var encodings []string
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		encodings = append(encodings, r.Header.Get("Accept-Encoding"))
	}
	srv := serve(t, h, map[string]http.HandlerFunc{
		"/releases/cni.tgz": func(w http.ResponseWriter, r *http.Request) {
			record(r)
			http.Redirect(w, r, "/objects/cni.tgz", http.StatusFound)
		},
		"/objects/cni.tgz": func(w http.ResponseWriter, r *http.Request) {
			record(r)
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
			}
			_, _ = w.Write(tgz)
		},
	})
	data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(tgz, srv.URL+"/releases/cni.tgz"))
	if err != nil || !bytes.Equal(data, tgz) {
		t.Fatalf("Fetch: %d bytes, %v; want the archive as it is", len(data), err)
	}
	if diff := cmp.Diff([]string{"identity", "identity"}, encodings); diff != "" {
		t.Errorf("Accept-Encoding of each request (-want +got):\n%s", diff)
	}
}

func TestFetchWithTheDefaultTransport(t *testing.T) {
	h, _, _ := machine(t)
	srv := httptest.NewServer(file(payload)) // on the loopback interface, which no proxy serves
	defer srv.Close()
	h.Transport = nil
	data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/cni.tgz"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %v; want the payload", data, err)
	}
}

func TestFetchSendsTheUserOfAURL(t *testing.T) {
	h, _, _ := machine(t)
	srv := serve(t, h, map[string]http.HandlerFunc{"/cni.tgz": func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "tent" || password != signature {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(payload)
	}})
	host := strings.TrimPrefix(srv.URL, "https://")
	data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, "https://tent:"+signature+"@"+host+"/cni.tgz"))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch: %q, %v; want the payload", data, err)
	}
}

func TestFetchNeedsAURL(t *testing.T) {
	h, fsys, _ := machine(t)
	addCache(t, fsys, []byte("the CNI plugins 1.9.0\n"), 0o600)
	_, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload))
	if want := "fetch cni-plugins 1.9.1: the asset has no URLs"; errText(err) != want {
		t.Errorf("Fetch: %q, want %q", errText(err), want)
	}
	if got := fsys.Changes(); len(got) != 0 {
		t.Errorf("Fetch changed %q, want nothing", got)
	}
}

func TestFetchFailsOnEveryURL(t *testing.T) {
	h, fsys, _ := machine(t)
	logs := captureLog(h)
	other := []byte("another file\n")
	srv := serve(t, h, map[string]http.HandlerFunc{"/b": file(other)})
	host := strings.TrimPrefix(srv.URL, "https://")
	a := cniAsset(payload, srv.URL+"/a?X-Amz-Expires=900&X-Amz-Signature="+signature,
		"https://tent:"+signature+"@"+host+"/b")
	_, _, err := nodeup.Fetch(t.Context(), h, a)
	want := "fetch cni-plugins 1.9.1: GET " + srv.URL + "/a?[query hidden]: answered 404 Not Found\n" +
		"GET https://tent:xxxxx@" + host + "/b: the sha256 is " + sum(other) + ", not " + sum(payload)
	if errText(err) != want {
		t.Errorf("Fetch: %q, want %q", errText(err), want)
	}
	secrettest.CheckHidden(t, map[string]string{"the error": errText(err), "the logs": logs.String()},
		map[string][]byte{"the URL's signature": []byte(signature)}, "?[query hidden]")
	if _, ok := fsys.Entry(cachePath); ok {
		t.Error("a failed Fetch wrote the cache")
	}
}

func TestFetchLimitsTheSize(t *testing.T) {
	const limit = 256 << 20
	t.Run("a Content-Length above 256 MiB", func(t *testing.T) {
		h, _, _ := machine(t)
		srv := serve(t, h, map[string]http.HandlerFunc{"/a": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(limit+1))
			w.WriteHeader(http.StatusOK)
		}})
		_, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, srv.URL+"/a"))
		want := fmt.Sprintf("fetch cni-plugins 1.9.1: GET %s/a: the answer has %d bytes, more than %d", srv.URL,
			limit+1, limit)
		if errText(err) != want {
			t.Errorf("Fetch: %q, want %q", errText(err), want)
		}
		if got := srv.Requests(); len(got) != 1 {
			t.Errorf("requests %q, want one", got)
		}
	})
	t.Run("a longer stream", func(t *testing.T) {
		h, _, _ := machine(t)
		long := []byte("0123456789a")
		srv := serve(t, h, map[string]http.HandlerFunc{"/a": stream(long)})
		_, _, err := nodeup.FetchUpTo(t.Context(), h, cniAsset(long, srv.URL+"/a"), 10)
		if want := "fetch cni-plugins 1.9.1: GET " + srv.URL + "/a: the answer has more than 10 bytes"; errText(
			err) != want {
			t.Errorf("Fetch: %q, want %q", errText(err), want)
		}
		if got := srv.Requests(); len(got) != 1 {
			t.Errorf("requests %q, want one", got)
		}
	})
	t.Run("a stream of the limit", func(t *testing.T) {
		h, _, _ := machine(t)
		exact := []byte("0123456789")
		srv := serve(t, h, map[string]http.HandlerFunc{"/a": stream(exact)})
		data, _, err := nodeup.FetchUpTo(t.Context(), h, cniAsset(exact, srv.URL+"/a"), 10)
		if err != nil || !bytes.Equal(data, exact) {
			t.Errorf("Fetch: %q, %v; want %q", data, err, exact)
		}
	})
}

// memoryTransport answers requests in memory, and records them. A synctest bubble's clock stands still while a
// goroutine waits for the network, so the tests that need that clock use it instead of an httptest server.
type memoryTransport struct {
	answer func(req *http.Request) (*http.Response, error)
	start  time.Time
	mu     sync.Mutex
	asked  []asked
}

// asked is a request that a memoryTransport answered: its URL, and how long after the transport's start it came.
type asked struct {
	URL   string
	After time.Duration
}

// useMemory gives h a memoryTransport that answers with answer, from now on.
func useMemory(h *nodeup.Host, answer func(req *http.Request) (*http.Response, error)) *memoryTransport {
	m := &memoryTransport{answer: answer, start: time.Now()}
	h.Transport = m
	return m
}

func (m *memoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	m.asked = append(m.asked, asked{req.URL.String(), time.Since(m.start)})
	m.mu.Unlock()
	return m.answer(req)
}

// Asked returns the requests, in order.
func (m *memoryTransport) Asked() []asked {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]asked(nil), m.asked...)
}

// reply returns the answer to req with the status and the body, whose length is unknown.
func reply(req *http.Request, code int, body io.ReadCloser) *http.Response {
	return &http.Response{
		Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), StatusCode: code,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: body, ContentLength: -1,
		Request: req,
	}
}

// streamBody returns a body that write fills until the context ends, and that fails with the cause of req's context
// once it ends, as the bodies of net/http do.
func streamBody(req *http.Request, write func(ctx context.Context, w io.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	ctx := req.Context()
	context.AfterFunc(ctx, func() { pw.CloseWithError(context.Cause(ctx)) })
	go func() { pw.CloseWithError(write(ctx, pw)) }()
	return pr
}

// slowly writes data a byte at a time, each after the pause.
func slowly(data []byte, pause time.Duration) func(ctx context.Context, w io.Writer) error {
	return func(ctx context.Context, w io.Writer) error {
		for _, b := range data {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(pause):
			}
			if _, err := w.Write([]byte{b}); err != nil {
				return err
			}
		}
		return nil
	}
}

// The URLs of the in-memory tests. The first carries a signature.
const (
	urlA       = "https://a.example.com/cni.tgz?X-Amz-Signature=" + signature
	shownA     = "https://a.example.com/cni.tgz?[query hidden]"
	urlB       = "https://b.example.com/cni.tgz"
	redirectTo = "https://objects.example.com/cni.tgz?X-Amz-Signature=" + signature
)

func TestFetchRetries(t *testing.T) {
	refused := errors.New("dial tcp 192.0.2.1:443: connect: connection refused")
	for _, c := range []struct {
		name    string
		answer  func(req *http.Request) (*http.Response, error) // of a.example.com
		asked   []string                                        // the URLs of a try
		warning string
	}{
		{"429", answerStatus(http.StatusTooManyRequests), []string{urlA}, "answered 429 Too Many Requests"},
		{"500", answerStatus(http.StatusInternalServerError), []string{urlA}, "answered 500 Internal Server Error"},
		{"503", answerStatus(http.StatusServiceUnavailable), []string{urlA}, "answered 503 Service Unavailable"},
		{"504", answerStatus(http.StatusGatewayTimeout), []string{urlA}, "answered 504 Gateway Timeout"},
		{"a failed connection", func(*http.Request) (*http.Response, error) { return nil, refused }, []string{urlA},
			refused.Error()},
		{"a cut body", func(req *http.Request) (*http.Response, error) {
			cut := io.MultiReader(strings.NewReader("the s"), iotest.ErrReader(io.ErrUnexpectedEOF))
			return reply(req, http.StatusOK, io.NopCloser(cut)), nil
		}, []string{urlA}, "read the answer: unexpected EOF"},
		// A failed connection to the redirect's target retries from the first URL; the logs show no signature.
		{"a failed connection after a redirect", func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "objects.example.com" {
				return nil, refused
			}
			resp := reply(req, http.StatusFound, http.NoBody)
			resp.Header.Set("Location", redirectTo)
			return resp, nil
		}, []string{urlA, redirectTo}, refused.Error()},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, _ := machine(t)
				logs := captureLog(h)
				m := useMemory(h, func(req *http.Request) (*http.Response, error) {
					if req.URL.Host == "b.example.com" {
						return reply(req, http.StatusOK, io.NopCloser(bytes.NewReader(payload))), nil
					}
					return c.answer(req)
				})
				data, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, urlA, urlB))
				if err != nil || !bytes.Equal(data, payload) {
					t.Fatalf("Fetch: %q, %v; want the payload", data, err)
				}
				// Three tries 2 seconds apart, then the next URL at once.
				var want []asked
				for try := range 3 {
					for _, u := range c.asked {
						want = append(want, asked{u, time.Duration(try) * 2 * time.Second})
					}
				}
				want = append(want, asked{urlB, 4 * time.Second})
				if diff := cmp.Diff(want, m.Asked()); diff != "" {
					t.Errorf("requests (-want +got):\n%s", diff)
				}
				lines := warnings(logs)
				for i, line := range lines {
					if w := fmt.Sprintf(`msg="download failed" url="%s" try=%d error="%s"`, shownA, i+1,
						c.warning); !strings.Contains(line, w) {
						t.Errorf("warning %q, want %q", line, w)
					}
				}
				if len(lines) != 3 {
					t.Errorf("%d warnings, want 3", len(lines))
				}
				secrettest.CheckHidden(t, map[string]string{"the logs": logs.String()},
					map[string][]byte{"the URL's signature": []byte(signature)}, "")
			})
		})
	}
}

// answerStatus answers with the status alone.
func answerStatus(code int) func(req *http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) { return reply(req, code, http.NoBody), nil }
}

func TestFetchLimitsTheTime(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(req *http.Request) (*http.Response, error)
		want   string
		every  time.Duration // from the start of a try to the start of the next
	}{
		{"no answer", func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, context.Cause(req.Context())
		}, "no answer within 1m0s", 62 * time.Second},
		{"a stalled body", func(req *http.Request) (*http.Response, error) {
			return reply(req, http.StatusOK, streamBody(req, func(ctx context.Context, w io.Writer) error {
				if _, err := w.Write([]byte("the s")); err != nil {
					return err
				}
				<-ctx.Done()
				return context.Cause(ctx)
			})), nil
		}, "no data for 1m0s", 62 * time.Second},
		{"a slow body", func(req *http.Request) (*http.Response, error) {
			return reply(req, http.StatusOK, streamBody(req, slowly(bytes.Repeat([]byte("x"), 1000), 50*time.Second))),
				nil
		}, "no whole answer within 10m0s", 602 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, _ := machine(t)
				m := useMemory(h, c.answer)
				_, _, err := nodeup.Fetch(t.Context(), h, cniAsset(payload, urlB))
				if want := "fetch cni-plugins 1.9.1: GET " + urlB + ": " + c.want; errText(err) != want {
					t.Errorf("Fetch: %q, want %q", errText(err), want)
				}
				want := []asked{{urlB, 0}, {urlB, c.every}, {urlB, 2 * c.every}}
				if diff := cmp.Diff(want, m.Asked()); diff != "" {
					t.Errorf("requests (-want +got):\n%s", diff)
				}
				if took, want := time.Since(m.start), 3*c.every-2*time.Second; took != want {
					t.Errorf("Fetch took %s, want %s", took, want)
				}
			})
		})
	}
}

func TestFetchWaitsForASlowBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, _ := machine(t)
		data := []byte("abc")
		// A byte every 50 seconds: never a minute without one.
		m := useMemory(h, func(req *http.Request) (*http.Response, error) {
			return reply(req, http.StatusOK, streamBody(req, slowly(data, 50*time.Second))), nil
		})
		got, _, err := nodeup.Fetch(t.Context(), h, cniAsset(data, urlB))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Fetch: %q, %v; want %q", got, err, data)
		}
		if took := time.Since(m.start); took != 150*time.Second {
			t.Errorf("Fetch took %s, want 2m30s", took)
		}
	})
}

func TestFetchStopsWhenTheContextEnds(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(req *http.Request) (*http.Response, error)
		want   string
	}{
		// The error keeps the last try's.
		{"during a wait", answerStatus(http.StatusServiceUnavailable), "fetch cni-plugins 1.9.1: GET " + shownA +
			": answered 503 Service Unavailable\nstopped: context canceled"},
		{"during a try", func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, context.Cause(req.Context())
		}, "fetch cni-plugins 1.9.1: stopped: context canceled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, _ := machine(t)
				m := useMemory(h, c.answer)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				time.AfterFunc(time.Second, cancel)
				_, _, err := nodeup.Fetch(ctx, h, cniAsset(payload, urlA, urlB))
				if !errors.Is(err, context.Canceled) || errText(err) != c.want {
					t.Errorf("Fetch: %q, want %q", errText(err), c.want)
				}
				if took := time.Since(m.start); took != time.Second {
					t.Errorf("Fetch took %s, want 1s", took)
				}
				if diff := cmp.Diff([]asked{{urlA, 0}}, m.Asked()); diff != "" {
					t.Errorf("requests (-want +got):\n%s", diff)
				}
			})
		})
	}
}
