package vultr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// testKey is the API key of the tests. No error may show it.
const testKey = "key-3c9f2b7e5a01"

const apiURL = "https://api.vultr.com/v2/instances"

// noKey fails the test when err's text shows the API key.
func noKey(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Errorf("the error shows the API key: %v", err)
	}
}

// send sends req through tr with a call record, as the client does. It checks that req keeps its header and that
// neither the returned error nor the record's shows the API key.
func send(t *testing.T, tr http.RoundTripper, req *http.Request) (*http.Response, *callRecord, error) {
	t.Helper()
	ctx, rec := withCallRecord(req.Context())
	req = req.WithContext(ctx)
	resp, err := tr.RoundTrip(req)
	noKey(t, err)
	noKey(t, newAPIError(rec))
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("RoundTrip set the request's own Authorization header")
	}
	return resp, rec, err
}

// newRequest makes a request as govultr does: its body is a bytes.Buffer, so it has GetBody. GET and HEAD have no
// body.
func newRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		body = bytes.NewBufferString("payload")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// readBody reads and closes a response body.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read the body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close the body: %v", err)
	}
	return string(data)
}

// reply is one answer of fakeAPI: a status, a header and a body, or an error.
type reply struct {
	status int
	header []string // keys and values in turn
	body   string
	err    error
}

// fakeAPI is a base transport that answers from a script and remembers the requests it got.
type fakeAPI struct {
	answer func(n int, req *http.Request) reply // answers request n, counting from 0

	mu    sync.Mutex
	calls []apiCall
	open  int // bodies of answers not closed yet
}

// apiCall is a request that fakeAPI got.
type apiCall struct {
	at                 time.Time
	method, auth, body string
}

func (f *fakeAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, apiCall{time.Now(), req.Method, req.Header.Get("Authorization"), string(body)})
	f.mu.Unlock()
	r := f.answer(n, req)
	if r.err != nil {
		return nil, r.err
	}
	f.mu.Lock()
	f.open++
	f.mu.Unlock()
	return &http.Response{
		StatusCode: r.status, Status: fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		Header: headers(r.header...), Body: &fakeBody{Reader: strings.NewReader(r.body), api: f}, Request: req,
	}, nil
}

// requests returns the requests fakeAPI got so far.
func (f *fakeAPI) requests() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// unclosed returns how many bodies of answers are not closed.
func (f *fakeAPI) unclosed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

// fakeBody is the body of a fakeAPI answer.
type fakeBody struct {
	*strings.Reader
	api    *fakeAPI
	closed bool
}

func (b *fakeBody) Close() error {
	b.api.mu.Lock()
	defer b.api.mu.Unlock()
	if !b.closed {
		b.closed = true
		b.api.open--
	}
	return nil
}

// status answers every request with status and header, and the body "attempt N".
func status(code int, header ...string) func(int, *http.Request) reply {
	return func(n int, _ *http.Request) reply {
		return reply{status: code, header: header, body: fmt.Sprintf("attempt %d", n+1)}
	}
}

// dropped answers every request with a failed connection.
func dropped(n int, _ *http.Request) reply {
	return reply{err: fmt.Errorf("read tcp: connection reset by peer (attempt %d)", n+1)}
}

// since returns the times of calls from start.
func since(start time.Time, calls []apiCall) []time.Duration {
	var ds []time.Duration
	for _, c := range calls {
		ds = append(ds, c.at.Sub(start))
	}
	return ds
}

// inAnHour is an HTTP date an hour after the start of a synctest bubble, which is midnight UTC on 2000-01-01.
var inAnHour = time.Date(2000, 1, 1, 1, 0, 0, 0, time.UTC).Format(http.TimeFormat)

func TestTransportRetries(t *testing.T) {
	const s, ms, m = time.Second, time.Millisecond, time.Minute
	backoff := []time.Duration{0, 500 * ms, 1500 * ms, 3500 * ms} // waits of 0.5s, 1s and 2s
	capped := []time.Duration{0, m, 2 * m, 3 * m}                 // waits of maxRetryAfter
	for _, tc := range []struct {
		name   string
		method string
		answer func(int, *http.Request) reply
		at     []time.Duration // when each attempt starts
	}{
		{"GET 429", http.MethodGet, status(429), backoff},
		{
			"GET 429 with Retry-After", http.MethodGet, status(429, "Retry-After", "1"),
			[]time.Duration{0, s, 2 * s, 3 * s},
		},
		{"GET 429 with Retry-After: 3600", http.MethodGet, status(429, "Retry-After", "3600"), capped},
		{"GET 429 with Retry-After in an hour", http.MethodGet, status(429, "Retry-After", inAnHour), capped},
		{"GET 503", http.MethodGet, status(503), backoff},
		{
			"GET 503 with Retry-After", http.MethodGet, status(503, "Retry-After", "2"),
			[]time.Duration{0, 2 * s, 4 * s, 6 * s},
		},
		{"GET dropped", http.MethodGet, dropped, backoff},
		{"HEAD 500", http.MethodHead, status(500), backoff},
		{"PUT 502", http.MethodPut, status(502), backoff},
		{"PATCH 504", http.MethodPatch, status(504), backoff},
		{"DELETE dropped", http.MethodDelete, dropped, backoff},
		{"GET 503 then 200", http.MethodGet, func(n int, req *http.Request) reply {
			if n < 2 {
				return status(503)(n, req)
			}
			return status(200)(n, req)
		}, backoff[:3]},
		{"POST 429", http.MethodPost, status(429, "Retry-After", "1"), backoff[:1]},
		{"POST 500", http.MethodPost, status(500), backoff[:1]},
		{"POST 503", http.MethodPost, status(503), backoff[:1]},
		{"POST dropped", http.MethodPost, dropped, backoff[:1]},
		{"GET 501", http.MethodGet, status(501), backoff[:1]},
		{"GET 400", http.MethodGet, status(400), backoff[:1]},
		{"GET 404", http.MethodGet, status(404), backoff[:1]},
		{"GET 200", http.MethodGet, status(200), backoff[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &fakeAPI{answer: tc.answer}
				req := newRequest(t, tc.method, apiURL)
				start := time.Now()
				resp, rec, err := send(t, newTransport(testKey, api), req)

				calls := api.requests()
				if diff := cmp.Diff(tc.at, since(start, calls)); diff != "" {
					t.Errorf("attempts (-want +got):\n%s", diff)
				}
				body := ""
				if req.Body != nil {
					body = "payload"
				}
				for i, c := range calls {
					if c.method != tc.method || c.auth != "Bearer "+testKey || c.body != body {
						t.Errorf("attempt %d: %s with Authorization %q and body %q, want %s with the key and %q",
							i+1, c.method, c.auth, c.body, tc.method, body)
					}
				}
				if n := api.unclosed(); n != 0 {
					t.Errorf("%d bodies of answers not closed", n)
				}
				last := tc.answer(len(tc.at)-1, req)
				want := callRecord{method: tc.method, path: "/v2/instances"}
				if last.err != nil {
					if err == nil || resp != nil {
						t.Fatalf("RoundTrip = %v, %v; want the last error", resp, err)
					}
					if rec.cause == nil || rec.cause.Error() != last.err.Error() {
						t.Errorf("recorded error %v, want %v", rec.cause, last.err)
					}
					want.cause = rec.cause
				} else {
					if err != nil {
						t.Fatalf("RoundTrip: %v", err)
					}
					if resp.StatusCode != last.status || readBody(t, resp) != last.body {
						t.Errorf("RoundTrip returned %s, want the last answer: %d %s", resp.Status, last.status,
							last.body)
					}
					want.status, want.header, want.body = last.status, headers(last.header...), []byte(last.body)
				}
				opts := cmp.Options{cmp.AllowUnexported(callRecord{}), cmpopts.EquateErrors(), cmpopts.EquateEmpty()}
				if diff := cmp.Diff(want, *rec, opts); diff != "" {
					t.Errorf("record (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestTransportSpacesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &fakeAPI{answer: status(200)}
		tr := newTransport(testKey, api)
		start := time.Now()
		var wg sync.WaitGroup
		for i := range 4 {
			path := fmt.Sprintf("/v2/instances/%d", i)
			req := newRequest(t, http.MethodGet, "https://api.vultr.com"+path)
			wg.Go(func() {
				resp, rec, err := send(t, tr, req)
				if err != nil {
					t.Errorf("RoundTrip: %v", err)
					return
				}
				_ = readBody(t, resp)
				if rec.path != path || rec.status != 200 {
					t.Errorf("record of %s: %s with %d", path, rec.path, rec.status)
				}
			})
		}
		wg.Wait()
		got := since(start, api.requests())
		slices.Sort(got)
		want := []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("request starts (-want +got):\n%s", diff)
		}
	})
}

func TestTransportRetryAfterPausesAll(t *testing.T) {
	for _, tc := range []struct {
		retryAfter string
		pause      time.Duration
	}{
		{"2", 2 * time.Second},
		{"3600", time.Minute},
		{inAnHour, time.Minute},
	} {
		t.Run(tc.retryAfter, func(t *testing.T) { testPause(t, tc.retryAfter, tc.pause) })
	}
}

// testPause checks that a 429 with the header Retry-After: retryAfter holds every request of the transport for
// pause: one that waits for its turn when the 429 comes, and one that starts after it.
func testPause(t *testing.T, retryAfter string, pause time.Duration) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		api := &fakeAPI{answer: func(_ int, req *http.Request) reply {
			if req.Method == http.MethodPost {
				<-release
				return reply{status: 429, header: []string{"Retry-After", retryAfter}}
			}
			return reply{status: 200}
		}}
		tr := newTransport(testKey, api)
		start := time.Now()
		var wg sync.WaitGroup
		get := func() {
			req := newRequest(t, http.MethodGet, apiURL)
			wg.Go(func() {
				if _, _, err := send(t, tr, req); err != nil {
					t.Errorf("GET: %v", err)
				}
			})
		}
		post := newRequest(t, http.MethodPost, apiURL)
		wg.Go(func() {
			resp, _, err := send(t, tr, post)
			if err != nil || resp.StatusCode != 429 {
				t.Errorf("POST = %v, %v; want the 429", resp, err)
			}
		})
		synctest.Wait() // the POST is sent
		get()           // waits for its turn when the 429 comes
		synctest.Wait()
		close(release)
		synctest.Wait()
		get() // starts after the 429
		wg.Wait()

		want := []time.Duration{0, pause, pause + 100*time.Millisecond}
		if diff := cmp.Diff(want, since(start, api.requests())); diff != "" {
			t.Errorf("request starts (-want +got):\n%s", diff)
		}
	})
}

// trackedBody is a request body that knows whether it was closed.
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestTransportCancel(t *testing.T) {
	blocked := func(_ int, req *http.Request) reply {
		<-req.Context().Done()
		return reply{err: errors.New("net/http: request canceled")}
	}
	for _, tc := range []struct {
		name   string
		pause  bool // a 429 with Retry-After: 10 comes first
		answer func(int, *http.Request) reply
		after  time.Duration // when the context ends; before the call when 0
		calls  int
	}{
		{"before the call", false, status(200), 0, 0},
		{"waiting for a turn", true, status(200), time.Second, 0},
		{"during an attempt", false, blocked, time.Second, 1},
		{"waiting for a retry", false, status(503), 200 * time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &fakeAPI{answer: tc.answer}
				tr := newTransport(testKey, api)
				if tc.pause {
					api.answer = status(429, "Retry-After", "10")
					if _, _, err := send(t, tr, newRequest(t, http.MethodPost, apiURL)); err != nil {
						t.Fatalf("POST: %v", err)
					}
					api.answer = tc.answer
				}
				before := len(api.requests())

				ctx, cancel := context.WithCancel(t.Context())
				if tc.after == 0 {
					cancel()
				} else {
					time.AfterFunc(tc.after, cancel)
				}
				var bodies []*trackedBody
				newBody := func() (io.ReadCloser, error) {
					b := &trackedBody{Reader: strings.NewReader("payload")}
					bodies = append(bodies, b)
					return b, nil
				}
				req := newRequest(t, http.MethodPut, apiURL).WithContext(ctx)
				req.Body, _ = newBody()
				req.GetBody = newBody
				start := time.Now()
				resp, rec, err := send(t, tr, req)

				if !errors.Is(err, context.Canceled) || resp != nil {
					t.Errorf("RoundTrip = %v, %v; want context.Canceled", resp, err)
				}
				if waited := time.Since(start); waited != tc.after {
					t.Errorf("RoundTrip returned after %v, want %v", waited, tc.after)
				}
				if n := len(api.requests()) - before; n != tc.calls {
					t.Errorf("%d calls, want %d", n, tc.calls)
				}
				for i, b := range bodies {
					if !b.closed.Load() {
						t.Errorf("request body %d is not closed", i)
					}
				}
				if rec.status != 0 || !errors.Is(rec.cause, context.Canceled) {
					t.Errorf("record: status %d, error %v; want context.Canceled", rec.status, rec.cause)
				}
				if e := newAPIError(rec); !errors.Is(e, context.Canceled) || !errors.Is(e, ErrUnavailable) {
					t.Errorf("newAPIError = %v, want ErrUnavailable and context.Canceled", e)
				}
			})
		})
	}
}

func TestTransportNeedsGetBodyToRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &fakeAPI{answer: status(503)}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, apiURL,
			io.NopCloser(strings.NewReader("payload")))
		if err != nil {
			t.Fatal(err)
		}
		if req.GetBody != nil {
			t.Fatal("the request has GetBody")
		}
		resp, _, err := send(t, newTransport(testKey, api), req)
		if err != nil || resp.StatusCode != 503 {
			t.Fatalf("RoundTrip = %v, %v; want the 503", resp, err)
		}
		if n := len(api.requests()); n != 1 {
			t.Errorf("%d attempts, want 1", n)
		}
	})
}

func TestTransportGetBodyFails(t *testing.T) {
	for _, answer := range []func(int, *http.Request) reply{status(503), dropped} {
		synctest.Test(t, func(t *testing.T) {
			api := &fakeAPI{answer: answer}
			req := newRequest(t, http.MethodPut, apiURL)
			req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("no body") }
			resp, rec, err := send(t, newTransport(testKey, api), req)
			if n := len(api.requests()); n != 1 {
				t.Errorf("%d attempts, want 1", n)
			}
			last := answer(0, req)
			if last.err != nil {
				if err == nil || resp != nil || rec.cause == nil {
					t.Errorf("RoundTrip = %v, %v, recorded %v; want the error", resp, err, rec.cause)
				}
				return
			}
			if err != nil || resp.StatusCode != last.status || rec.status != last.status {
				t.Errorf("RoundTrip = %v, %v, recorded %d; want the %d", resp, err, rec.status, last.status)
			}
		})
	}
}

func TestTransportGetBodyOnlyForRetries(t *testing.T) {
	for _, tc := range []struct {
		method  string
		getBody bool // whether the base gets a request with GetBody
	}{
		{http.MethodPost, false},
		{http.MethodGet, true},
		{http.MethodPut, true},
		{http.MethodDelete, true},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var got []bool
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				got = append(got, r.GetBody != nil)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
			})
			req, err := http.NewRequestWithContext(t.Context(), tc.method, apiURL, bytes.NewBufferString("payload"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := send(t, newTransport(testKey, base), req); err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if diff := cmp.Diff([]bool{tc.getBody}, got); diff != "" {
				t.Errorf("the base got GetBody (-want +got):\n%s", diff)
			}
			if req.GetBody == nil {
				t.Error("RoundTrip removed the request's own GetBody")
			}
		})
	}
}

func TestTransportHidesTheKey(t *testing.T) {
	tr := newTransport(testKey, nil)
	hexKey := fmt.Sprintf("%x", []byte(testKey))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		got := fmt.Sprintf(verb, tr)
		if strings.Contains(got, testKey) || strings.Contains(got, hexKey) || !strings.Contains(got, "[redacted]") {
			t.Errorf("%s of the transport = %s, want [redacted] in place of the key", verb, got)
		}
	}
}

// stallingAPI is a base transport whose answers stall: a read of the body, or RoundTrip itself when inHeader is set,
// blocks until the request's context ends. The requests after the first stalled ones get a whole answer.
type stallingAPI struct {
	stalled  int // how many requests stall
	inHeader bool

	mu     sync.Mutex
	starts []time.Time
}

func (s *stallingAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	n := len(s.starts)
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	body := io.NopCloser(strings.NewReader("ok"))
	if n < s.stalled {
		if s.inHeader {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body = io.NopCloser(stalledBody{req.Context()})
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: req}, nil
}

// stalledBody is a body that sends nothing until its context ends.
type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func TestTransportBoundsEachAttempt(t *testing.T) {
	const ms, m = time.Millisecond, time.Minute
	stalls := []time.Duration{0, 2*m + 500*ms, 4*m + 1500*ms, 6*m + 3500*ms} // 2 minutes each, then the backoff
	for _, tc := range []struct {
		name     string
		method   string
		api      *stallingAPI
		opts     []transportOption
		starts   []time.Duration // when each attempt starts
		took     time.Duration   // when RoundTrip returns
		answered bool            // the last attempt gets an answer
	}{
		{"a GET whose body stalls", http.MethodGet, &stallingAPI{stalled: 4}, nil, stalls, 8*m + 3500*ms, false},
		{
			"a GET whose header stalls", http.MethodGet, &stallingAPI{stalled: 4, inHeader: true}, nil, stalls,
			8*m + 3500*ms, false,
		},
		{"a GET whose body stalls once", http.MethodGet, &stallingAPI{stalled: 1}, nil, stalls[:2], 2*m + 500*ms, true},
		{
			"a POST whose body stalls", http.MethodPost, &stallingAPI{stalled: 1},
			[]transportOption{withAttemptTimeout(10 * time.Second)}, stalls[:1], 10 * time.Second, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				resp, rec, err := send(t, newTransport(testKey, tc.api, tc.opts...), newRequest(t, tc.method, apiURL))
				if took := time.Since(start); took != tc.took {
					t.Errorf("RoundTrip returned after %v, want %v", took, tc.took)
				}
				var starts []time.Duration
				for _, at := range tc.api.starts {
					starts = append(starts, at.Sub(start))
				}
				if diff := cmp.Diff(tc.starts, starts); diff != "" {
					t.Errorf("attempts (-want +got):\n%s", diff)
				}
				if tc.answered {
					if err != nil || readBody(t, resp) != "ok" {
						t.Errorf("RoundTrip = %v, %v; want the answer", resp, err)
					}
					return
				}
				e := newAPIError(rec)
				if err == nil || resp != nil || !errors.Is(e, ErrUnavailable) || errors.Is(e, context.DeadlineExceeded) {
					t.Errorf("RoundTrip = %v, %v, error %v; want ErrUnavailable without the context's error", resp, err, e)
				}
				timeout := tc.took - tc.starts[len(tc.starts)-1]
				if want := fmt.Sprintf("no answer within %v", timeout); rec.cause == nil || rec.cause.Error() != want {
					t.Errorf("recorded %v, want %q", rec.cause, want)
				}
			})
		})
	}
}

func TestTransportDefaultBase(t *testing.T) {
	if tr := newTransport(testKey, nil); tr.base != http.DefaultTransport {
		t.Errorf("base = %v, want http.DefaultTransport", tr.base)
	}
}

// server starts a test server that counts its requests. It runs in real time, so the transport it returns waits 1ms.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *transport, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(testKey, srv.Client().Transport, withInterval(time.Millisecond), withBackoff(time.Millisecond))
	return srv, tr, &hits
}

func TestTransportOverHTTP(t *testing.T) {
	t.Run("authorization", func(t *testing.T) {
		var auth atomic.Value
		srv, tr, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
			auth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Invalid API token.","status":401}`)
		})
		_, rec, err := send(t, tr, newRequest(t, http.MethodGet, srv.URL+"/v2/account"))
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if got := auth.Load(); got != "Bearer "+testKey {
			t.Errorf("the server got Authorization %q, want the key", got)
		}
		if e := newAPIError(rec); !errors.Is(e, ErrForbidden) {
			t.Errorf("newAPIError = %v, want ErrForbidden", e)
		}
	})

	t.Run("body", func(t *testing.T) {
		for _, size := range []int{100, maxRecordedBody, 100 << 10} {
			full := strings.Repeat("0123456789", size/10+1)[:size]
			srv, tr, _ := server(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, full)
			})
			ctx, rec := withCallRecord(t.Context())
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v2/instances", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if got := readBody(t, resp); got != full {
				t.Errorf("%d bytes: the caller read %d bytes, want all", size, len(got))
			}
			if got, want := string(rec.body), full[:min(size, maxRecordedBody)]; got != want {
				t.Errorf("%d bytes: recorded %d bytes, want %d", size, len(got), len(want))
			}
			if rec.status != 200 || rec.header.Get("Content-Type") != "application/json" {
				t.Errorf("%d bytes: recorded %d, %v; want 200 and the header", size, rec.status, rec.header)
			}
		}
	})

	t.Run("dropped connection", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			hits   int32
		}{{http.MethodGet, 4}, {http.MethodPost, 1}} {
			srv, tr, hits := server(t, func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			})
			resp, rec, err := send(t, tr, newRequest(t, tc.method, srv.URL+"/v2/instances"))
			if err == nil || resp != nil || rec.cause == nil {
				t.Errorf("%s: RoundTrip = %v, %v, recorded %v; want an error", tc.method, resp, err, rec.cause)
			}
			if n := hits.Load(); n != tc.hits {
				t.Errorf("%s: the server got %d requests, want %d", tc.method, n, tc.hits)
			}
		}
	})

	t.Run("no server", func(t *testing.T) {
		srv, tr, _ := server(t, func(http.ResponseWriter, *http.Request) {})
		srv.Close()
		if _, _, err := send(t, tr, newRequest(t, http.MethodPost, srv.URL+"/v2/instances")); err == nil {
			t.Error("RoundTrip to a closed server succeeded")
		}
	})
}

// roundTripFunc is a function as an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
