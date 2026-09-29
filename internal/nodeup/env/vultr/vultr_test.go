package vultr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/env/vultr"
	"github.com/ingvarch/tent/internal/secrettest"
)

// metadataAddr is the address that the environment must dial: the metadata service's.
const metadataAddr = "169.254.169.254:80"

// deadProxy is a proxy that nobody answers at. Every test puts it into the environment, so a read through a proxy
// fails.
const deadProxy = "http://127.0.0.1:9"

// recorded is the machine that testdata/v1.json describes. The file has the structure of the document that a Vultr
// instance in ams served on 2026-09-25, with documentation values in place of its public address, MAC addresses, ids,
// host name and SSH key.
var recorded = env.Instance{
	ID: "0b8f5c3e-1d2a-4c6e-9f7b-2a4d6e8c0f13", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.3"),
}

// answer is what the fake service answers to one request.
type answer struct {
	status    int
	body      string
	location  string // the Location header, if any
	stall     bool   // answer nothing until the client gives up
	stallBody bool   // send the header and half the body, then nothing until the client gives up
	cut       bool   // send a 200 whose Content-Length counts the whole body, then half of it, and close
	then      func() // called before the answer goes out
}

// ok is the answer with the recorded document.
func ok(t *testing.T) answer {
	return answer{status: http.StatusOK, body: recordedDoc(t)}
}

// service is a fake Vultr metadata service. It gives its answers in turn, and the last one again once they run out.
// Its dial connects to it when asked for the metadata service's address, after failing the first failDials times,
// and refuses any other address.
type service struct {
	t         *testing.T
	srv       *httptest.Server
	answers   []answer
	failDials int
	done      chan struct{} // closed when the test ends, which ends every stall

	mu     sync.Mutex
	reqs   []string // the method and path of each request
	dials  int
	strays []string // the addresses asked for other than the metadata service's
}

// newService starts a fake service with the answers, and puts deadProxy into the environment for the test. The test
// fails when anything dials another address than the metadata service's.
func newService(t *testing.T, answers ...answer) *service {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, deadProxy)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	s := &service{t: t, answers: answers, done: make(chan struct{})}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { close(s.done) }) // before Close, which waits for the handlers
	t.Cleanup(func() {
		if strays := s.strayDials(); len(strays) > 0 {
			t.Errorf("dialed %v, not only the metadata service at %s", strays, metadataAddr)
		}
	})
	return s
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
	a := s.answers[min(len(s.reqs), len(s.answers))-1]
	s.mu.Unlock()
	if a.then != nil {
		a.then()
	}
	switch {
	case a.stall:
		s.stall(r)
	case a.cut:
		s.cutShort(w, a.body)
	case a.stallBody:
		w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body[:len(a.body)/2])
		_ = http.NewResponseController(w).Flush()
		s.stall(r)
	default:
		if a.location != "" {
			w.Header().Set("Location", a.location)
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}
}

// stall returns when the client gives up the request or the test ends.
func (s *service) stall(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-s.done:
	}
}

// cutShort sends a 200 whose Content-Length counts the whole body, then half of the body, and closes the connection.
func (s *service) cutShort(w http.ResponseWriter, body string) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.t.Errorf("hijack the connection: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:len(body)/2])
	_ = buf.Flush()
}

// dial connects to the fake service in place of the metadata service.
func (s *service) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s.mu.Lock()
	s.dials++
	fail := s.dials <= s.failDials
	stray := addr != metadataAddr
	if stray {
		s.strays = append(s.strays, addr)
	}
	s.mu.Unlock()
	switch {
	case stray:
		return nil, fmt.Errorf("the test refuses to dial %s", addr)
	case fail:
		return nil, errors.New("the test refuses this connection")
	}
	var d net.Dialer
	return d.DialContext(ctx, network, s.srv.Listener.Addr().String())
}

// requests returns the method and path of each request so far.
func (s *service) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *service) strayDials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.strays)
}

func (s *service) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// environment returns an Environment that reads the fake service, gives each try at most try, and waits wait(n)
// after failed try n.
func (s *service) environment(try time.Duration, wait func(n int) time.Duration) *vultr.Environment {
	return vultr.NewForTest(s.dial, try, wait)
}

// noWait is the wait between tries of tests that do not count the waits.
func noWait(int) time.Duration { return 0 }

// testTry is how long a try takes at most in tests whose service answers at once.
const testTry = 5 * time.Second

// testContext returns the context of a read, which ends after 10 seconds, so that a read that retries forever fails
// the test.
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// recordedDoc returns testdata/v1.json.
func recordedDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// editedDoc returns testdata/v1.json after edit.
func editedDoc(t *testing.T, edit func(doc map[string]any)) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(recordedDoc(t)), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// interfaces returns the interfaces of the document: the public one, then the private one.
func interfaces(doc map[string]any) []any { return doc["interfaces"].([]any) }

// privateIPv4 returns the ipv4 object of the document's private interface.
func privateIPv4(doc map[string]any) map[string]any {
	return interfaces(doc)[1].(map[string]any)["ipv4"].(map[string]any)
}

// errText returns the error's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestReadRecordedDocument checks that Read takes the instance id, the zone in lower case and the private address
// from a document as Vultr serves it, with one request.
func TestReadRecordedDocument(t *testing.T) {
	s := newService(t, ok(t))
	got, err := s.environment(testTry, noWait).Read(testContext(t))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != recorded {
		t.Errorf("Read() = %+v, want %+v", got, recorded)
	}
	if got, want := s.requests(), []string{"GET /v1.json"}; !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// TestReadDocument checks what Read takes from a document and what it refuses. It never tries again for a document
// that it refuses.
func TestReadDocument(t *testing.T) {
	type doc = map[string]any
	withPrivate := func(addr string) func(doc) {
		return func(d doc) { privateIPv4(d)["address"] = addr }
	}
	for _, tc := range []struct {
		name string
		edit func(d doc)
		want env.Instance // when there is no error
		err  string
	}{
		{name: "zone in mixed case", edit: func(d doc) { d["region"].(doc)["regioncode"] = "Fra" },
			want: env.Instance{ID: recorded.ID, Zone: "fra", PrivateIP: recorded.PrivateIP}},
		{name: "private interface first", edit: func(d doc) { slices.Reverse(interfaces(d)) }, want: recorded},
		{name: "user data and vendor data", edit: func(d doc) {
			d["user-data"], d["vendor-data"], d["startup-script"] = "#cloud-config\n", "Content-Type: multipart", ""
		}, want: recorded},

		{name: "no instance-v2-id", edit: func(d doc) { delete(d, "instance-v2-id") },
			err: "vultr metadata: no instance-v2-id"},
		{name: "empty instance-v2-id", edit: func(d doc) { d["instance-v2-id"] = "" },
			err: "vultr metadata: no instance-v2-id"},
		{name: "no region", edit: func(d doc) { delete(d, "region") }, err: "vultr metadata: no region.regioncode"},
		{name: "empty region code", edit: func(d doc) { d["region"].(doc)["regioncode"] = "" },
			err: "vultr metadata: no region.regioncode"},
		{name: "no private interface", edit: func(d doc) { d["interfaces"] = interfaces(d)[:1] },
			err: "vultr metadata: no interface with network-type private"},
		{name: "no interfaces", edit: func(d doc) { delete(d, "interfaces") },
			err: "vultr metadata: no interface with network-type private"},
		{name: "two private interfaces", edit: func(d doc) { interfaces(d)[0].(doc)["network-type"] = "private" },
			err: "vultr metadata: 2 interfaces with network-type private, want one"},
		{name: "no private address", edit: func(d doc) { delete(privateIPv4(d), "address") },
			err: "vultr metadata: the private interface has no IPv4 address"},
		{name: "empty private address", edit: withPrivate(""),
			err: "vultr metadata: the private interface has no IPv4 address"},
		{name: "private address 0.0.0.0", edit: withPrivate("0.0.0.0"),
			err: "vultr metadata: the private interface has no IPv4 address: it reports 0.0.0.0"},
		{name: "private address IPv6", edit: withPrivate("fd00::3"),
			err: `vultr metadata: the private interface's IPv4 address "fd00::3" is not an IPv4 address`},
		{name: "private address not an address", edit: withPrivate("10.64.0"),
			err: `vultr metadata: the private interface's IPv4 address "10.64.0" is not an IPv4 address`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, answer{status: http.StatusOK, body: editedDoc(t, tc.edit)})
			got, err := s.environment(testTry, noWait).Read(testContext(t))
			if errText(err) != tc.err {
				t.Fatalf("Read() error = %q, want %q", errText(err), tc.err)
			}
			if got != tc.want {
				t.Errorf("Read() = %+v, want %+v", got, tc.want)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

// TestReadNotJSON checks that a document that does not parse fails at once.
func TestReadNotJSON(t *testing.T) {
	s := newService(t, answer{status: http.StatusOK, body: "<html>maintenance</html>"})
	_, err := s.environment(testTry, noWait).Read(testContext(t))
	want := "vultr metadata: the document does not parse: invalid character '<' looking for beginning of value"
	if errText(err) != want {
		t.Errorf("Read() error = %q, want %q", errText(err), want)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// TestReadTypeErrors checks that a field of another type fails the read at once, with an error that names the type
// Read takes. The words before the type differ between Go versions.
func TestReadTypeErrors(t *testing.T) {
	const prefix = "vultr metadata: the document does not parse: json: cannot unmarshal string into Go struct field "
	for _, tc := range []struct {
		name, want string
		edit       func(d map[string]any)
	}{
		{"region", "vultr.region", func(d map[string]any) { d["region"] = "AMS" }},
		{"interfaces", "[]vultr.networkInterface", func(d map[string]any) { d["interfaces"] = "enp8s0" }},
		{"ipv4", "vultr.ipv4", func(d map[string]any) { interfaces(d)[1].(map[string]any)["ipv4"] = "10.64.0.3" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, answer{status: http.StatusOK, body: editedDoc(t, tc.edit)})
			_, err := s.environment(testTry, noWait).Read(testContext(t))
			if got := errText(err); !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, " of type "+tc.want) {
				t.Errorf("Read() error = %q, want %q… of type %s", got, prefix, tc.want)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

// TestReadRetries checks that Read tries again after a failed connection, a try without an answer in time, a 429 or a
// 5xx other than 501, a body cut short and a body that stalls, waiting after each failed try.
func TestReadRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answers   func(t *testing.T) []answer
		failDials int
		try       time.Duration
		requests  int
		waits     []int
	}{
		{name: "500 then 200", answers: func(t *testing.T) []answer {
			return []answer{{status: http.StatusInternalServerError}, ok(t)}
		}, try: testTry, requests: 2, waits: []int{1}},
		{name: "503, 502 then 200", answers: func(t *testing.T) []answer {
			return []answer{{status: http.StatusServiceUnavailable}, {status: http.StatusBadGateway}, ok(t)}
		}, try: testTry, requests: 3, waits: []int{1, 2}},
		{name: "429 then 200", answers: func(t *testing.T) []answer {
			return []answer{{status: http.StatusTooManyRequests}, ok(t)}
		}, try: testTry, requests: 2, waits: []int{1}},
		{name: "failed connections", answers: func(t *testing.T) []answer { return []answer{ok(t)} },
			failDials: 2, try: testTry, requests: 1, waits: []int{1, 2}},
		{name: "no answer in time", answers: func(t *testing.T) []answer {
			return []answer{{stall: true}, ok(t)}
		}, try: 50 * time.Millisecond, requests: 2, waits: []int{1}},
		{name: "a body cut short", answers: func(t *testing.T) []answer {
			return []answer{{body: recordedDoc(t), cut: true}, ok(t)}
		}, try: testTry, requests: 2, waits: []int{1}},
		{name: "a body that stalls", answers: func(t *testing.T) []answer {
			return []answer{{status: http.StatusOK, body: recordedDoc(t), stallBody: true}, ok(t)}
		}, try: 50 * time.Millisecond, requests: 2, waits: []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, tc.answers(t)...)
			s.failDials = tc.failDials
			var waits []int
			e := s.environment(tc.try, func(n int) time.Duration {
				waits = append(waits, n)
				return time.Millisecond
			})
			got, err := e.Read(testContext(t))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != recorded {
				t.Errorf("Read() = %+v, want %+v", got, recorded)
			}
			if n := len(s.requests()); n != tc.requests {
				t.Errorf("%d requests, want %d", n, tc.requests)
			}
			if !slices.Equal(waits, tc.waits) {
				t.Errorf("waited after the tries %v, want %v", waits, tc.waits)
			}
		})
	}
}

// TestReadFailsAtOnce checks that an answer other than 200, 429 and a 5xx other than 501 fails the read without
// another try, and that Read follows no redirect.
func TestReadFailsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		answer answer
		want   string
	}{
		{answer{status: http.StatusNotFound},
			"vultr metadata: GET http://169.254.169.254/v1.json answered 404 Not Found"},
		{answer{status: http.StatusForbidden},
			"vultr metadata: GET http://169.254.169.254/v1.json answered 403 Forbidden"},
		{answer{status: http.StatusNotImplemented},
			"vultr metadata: GET http://169.254.169.254/v1.json answered 501 Not Implemented"},
		{answer{status: http.StatusFound, location: "http://198.51.100.7/v1.json"},
			"vultr metadata: GET http://169.254.169.254/v1.json answered 302 Found"},
		{answer{status: http.StatusNoContent},
			"vultr metadata: GET http://169.254.169.254/v1.json answered 204 No Content"},
	} {
		t.Run(http.StatusText(tc.answer.status), func(t *testing.T) {
			s := newService(t, tc.answer, ok(t))
			waited := false
			_, err := s.environment(testTry, func(int) time.Duration {
				waited = true
				return 0
			}).Read(testContext(t))
			if errText(err) != tc.want {
				t.Errorf("Read() error = %q, want %q", errText(err), tc.want)
			}
			if n := len(s.requests()); n != 1 || waited {
				t.Errorf("%d requests and waited %v, want 1 request and no wait", n, waited)
			}
		})
	}
}

// TestReadStopsWhenContextEnds checks that Read tries no more once its context ends: before the first try, during a
// try and during a wait. The error is the context's, with the last failed try.
func TestReadStopsWhenContextEnds(t *testing.T) {
	const last = "; the last try: GET http://169.254.169.254/v1.json answered 500 Internal Server Error"
	failed := answer{status: http.StatusInternalServerError}

	t.Run("before the first try", func(t *testing.T) {
		s := newService(t, ok(t))
		ctx, cancel := context.WithCancel(testContext(t))
		cancel()
		_, err := s.environment(testTry, noWait).Read(ctx)
		if !errors.Is(err, context.Canceled) || errText(err) != "vultr metadata: context canceled" {
			t.Errorf("Read() error = %v, want vultr metadata: context canceled", err)
		}
		if n, d := len(s.requests()), s.dialCount(); n != 0 || d != 0 {
			t.Errorf("%d requests and %d dials, want none", n, d)
		}
	})

	t.Run("during the tries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testContext(t))
		cancelled := failed
		cancelled.then = cancel
		s := newService(t, failed, failed, cancelled)
		_, err := s.environment(testTry, noWait).Read(ctx)
		if !errors.Is(err, context.Canceled) || errText(err) != "vultr metadata: context canceled"+last {
			t.Errorf("Read() error = %v, want vultr metadata: context canceled%s", err, last)
		}
		if n := len(s.requests()); n != 3 {
			t.Errorf("%d requests, want 3", n)
		}
	})

	t.Run("while connections fail", func(t *testing.T) {
		s := newService(t, ok(t))
		s.failDials = 1000
		ctx, cancel := context.WithCancel(testContext(t))
		_, err := s.environment(testTry, func(n int) time.Duration {
			if n == 2 {
				cancel()
			}
			return 0
		}).Read(ctx)
		want := `vultr metadata: context canceled; the last try: Get "http://169.254.169.254/v1.json": ` +
			"the test refuses this connection"
		if !errors.Is(err, context.Canceled) || errText(err) != want {
			t.Errorf("Read() error = %v, want %s", err, want)
		}
		if d := s.dialCount(); d != 2 {
			t.Errorf("%d dials, want 2", d)
		}
	})

	t.Run("during a wait", func(t *testing.T) {
		s := newService(t, failed)
		ctx, cancel := context.WithCancel(testContext(t))
		start := time.Now()
		_, err := s.environment(testTry, func(int) time.Duration {
			cancel()
			return time.Minute
		}).Read(ctx)
		if !errors.Is(err, context.Canceled) || errText(err) != "vultr metadata: context canceled"+last {
			t.Errorf("Read() error = %v, want vultr metadata: context canceled%s", err, last)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Read returned after %v, want at once when the context ends", d)
		}
		if n := len(s.requests()); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
	})
}

// TestReadIgnoresProxy checks that Read goes to the metadata service itself even when the environment names a proxy:
// through a proxy, 169.254.169.254 would be the proxy's own metadata service.
func TestReadIgnoresProxy(t *testing.T) {
	s := newService(t, ok(t))
	if os.Getenv("HTTP_PROXY") != deadProxy {
		t.Fatal("the test did not set HTTP_PROXY")
	}
	if _, err := s.environment(testTry, noWait).Read(testContext(t)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strays := s.strayDials(); len(strays) > 0 {
		t.Errorf("dialed %v, want only the metadata service at %s", strays, metadataAddr)
	}
}

// TestReadErrorsHideTheDocument checks that no error shows the document, which carries the user data and its secrets:
// not for a document that Read refuses, that does not parse, or that comes with a status Read does not take.
func TestReadErrorsHideTheDocument(t *testing.T) {
	marker := "tent-node-secret-marker-5c1f0e9b7a2d4836"
	withSecret := func(t *testing.T, edit func(d map[string]any)) string {
		return editedDoc(t, func(d map[string]any) {
			d["user-data"] = marker
			edit(d)
		})
	}
	whole := withSecret(t, func(map[string]any) {})
	secrets := map[string][]byte{
		"the user data":      []byte(marker),
		"the document's key": []byte("AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
	}
	for _, tc := range []struct {
		name    string
		answers func(cancel func()) []answer
	}{
		{"refused document", func(func()) []answer {
			return []answer{{status: http.StatusOK, body: withSecret(t, func(d map[string]any) {
				delete(d, "instance-v2-id")
			})}}
		}},
		{"no private address", func(func()) []answer {
			return []answer{{status: http.StatusOK, body: withSecret(t, func(d map[string]any) {
				privateIPv4(d)["address"] = "0.0.0.0"
			})}}
		}},
		{"not JSON after the user data", func(func()) []answer {
			return []answer{{status: http.StatusOK, body: `{"user-data": "` + marker + `", "instance-v2-id": }`}}
		}},
		{"a field of another type", func(func()) []answer {
			return []answer{{status: http.StatusOK, body: `{"user-data": "` + marker + `", "region": "AMS"}`}}
		}},
		{"404 with the document", func(func()) []answer {
			return []answer{{status: http.StatusNotFound, body: whole}}
		}},
		{"500 with the document until the context ends", func(cancel func()) []answer {
			return []answer{{status: http.StatusInternalServerError, body: whole},
				{status: http.StatusInternalServerError, body: whole, then: cancel}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(testContext(t))
			s := newService(t, tc.answers(cancel)...)
			_, err := s.environment(testTry, noWait).Read(ctx)
			if err == nil {
				t.Fatal("Read() succeeded, want an error")
			}
			secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, secrets, "")
			if strings.Contains(err.Error(), `"interfaces"`) {
				t.Errorf("the error shows the document: %q", err)
			}
		})
	}
}

// TestDefaults checks that New gives each try 5 seconds and waits a second between tries.
func TestDefaults(t *testing.T) {
	try, wait := vultr.Defaults()
	if try != 5*time.Second || wait != time.Second {
		t.Errorf("each try takes at most %v and the wait is %v, want 5s and 1s", try, wait)
	}
}
