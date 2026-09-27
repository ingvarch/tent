package vultr

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Defaults of the transport. Vultr answers more than 30 requests per second from one address with 429.
const (
	defaultInterval = 100 * time.Millisecond // 10 requests per second
	firstBackoff    = 500 * time.Millisecond // the wait before the first retry, doubled before each next one
	maxAttempts     = 4
	attemptTimeout  = 2 * time.Minute // the longest an attempt takes, its answer's body included
	maxRetryAfter   = time.Minute     // the longest wait the transport takes from Retry-After
)

// maxRecordedBody is the most of an answer's body that a callRecord keeps.
const maxRecordedBody = 64 << 10

// transport sends govultr's requests to the Vultr API. The client turns govultr's own retries off, since govultr
// would send a POST again, which may create a second object. The transport instead:
//   - sets the Authorization header;
//   - starts at most one request per interval, and after a 429 with Retry-After starts none until that time;
//   - gives each attempt at most 2 minutes, its answer's body included, so an answer that stalls fails the attempt;
//   - sends a GET, HEAD, PUT, PATCH or DELETE up to 4 times in all, after a failed attempt, a 429 or a 5xx other
//     than 501, waiting Retry-After or else a backoff;
//   - never sends a POST again, and hands it to net/http without GetBody, so net/http cannot send it again either;
//   - waits at most a minute for a Retry-After, both before a retry and in the pause of the other requests, so one
//     odd header cannot hold every request for an hour; APIError.RetryAfter keeps the server's value;
//   - records the last answer or error of each call in the callRecord of the request's context, so the client builds
//     its errors from the record instead of from govultr's text.
type transport struct {
	// Key is exported only so that fmt calls apiKey's methods: it prints an unexported field without them.
	Key            apiKey
	base           http.RoundTripper
	limit          limiter
	firstBackoff   time.Duration
	attemptTimeout time.Duration
}

// apiKey is an API key that fmt prints as [redacted] with every verb.
type apiKey string

const redacted = "[redacted]"

func (apiKey) String() string   { return redacted }
func (apiKey) GoString() string { return redacted }

func (apiKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// transportOption changes a default of newTransport.
type transportOption func(*transport)

// withInterval sets the time between the starts of two requests.
func withInterval(d time.Duration) transportOption {
	return func(t *transport) { t.limit.interval = d }
}

// withBackoff sets the wait before the first retry.
func withBackoff(first time.Duration) transportOption {
	return func(t *transport) { t.firstBackoff = first }
}

// withAttemptTimeout sets the longest an attempt takes, its answer's body included.
func withAttemptTimeout(d time.Duration) transportOption {
	return func(t *transport) { t.attemptTimeout = d }
}

// newTransport returns a transport that sends requests with the API key through base, or through
// http.DefaultTransport when base is nil.
func newTransport(key string, base http.RoundTripper, opts ...transportOption) *transport {
	t := &transport{
		Key:            apiKey(key),
		base:           cmp.Or(base, http.DefaultTransport),
		limit:          limiter{interval: defaultInterval},
		firstBackoff:   firstBackoff,
		attemptTimeout: attemptTimeout,
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// callRecord is what the transport saw of one call: its last attempt's answer, or why that attempt got none. The
// transport writes it once, before RoundTrip returns.
type callRecord struct {
	method string
	path   string
	status int         // 0 when the call got no answer
	header http.Header // of the answer
	body   []byte      // the start of the answer's body, at most maxRecordedBody bytes
	cause  error       // why the call got no answer, such as a failed connection or the end of the context
}

// callRecordKey is the context key of a *callRecord.
type callRecordKey struct{}

// withCallRecord returns a context that makes the transport record a call in the returned record. Each call needs
// its own record.
func withCallRecord(ctx context.Context) (context.Context, *callRecord) {
	rec := &callRecord{}
	return context.WithValue(ctx, callRecordKey{}, rec), rec
}

// RoundTrip sends req and returns the last answer, with its whole body, or the last error. It does not change req.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, body, err := t.send(req)
	if rec, ok := req.Context().Value(callRecordKey{}).(*callRecord); ok {
		*rec = callRecord{method: cmp.Or(req.Method, http.MethodGet), path: req.URL.Path, cause: err}
		if resp != nil {
			rec.status, rec.header = resp.StatusCode, resp.Header.Clone()
			rec.body = bytes.Clone(body[:min(len(body), maxRecordedBody)])
		}
	}
	return resp, err
}

// send sends req until an attempt's outcome is final, and returns the answer and its body, or the error. When the
// context ends before that, the error is the context's.
func (t *transport) send(req *http.Request) (*http.Response, []byte, error) {
	ctx := req.Context()
	retry := mayRetry(req)
	body := req.Body
	for n := 1; ; n++ {
		resp, data, err := t.attempt(req, body, retry)
		if err != nil && ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		var wait time.Duration
		if resp != nil {
			wait = min(retryAfter(resp.Header, time.Now()), maxRetryAfter)
			if resp.StatusCode == http.StatusTooManyRequests && wait > 0 {
				t.limit.pause(time.Now().Add(wait))
			}
		}
		if !retry || n == maxAttempts || !retryable(resp, err) {
			return resp, data, err
		}
		if wait == 0 {
			wait = t.backoff(n)
		}
		if serr := sleep(ctx, wait); serr != nil {
			return nil, nil, serr
		}
		if req.GetBody != nil {
			b, gerr := req.GetBody()
			if gerr != nil {
				return resp, data, err // the last outcome stands when the body cannot be had again
			}
			body = b
		}
	}
}

// attempt sends req once with body when its turn comes, and reads the answer's whole body within the attempt
// timeout. It puts the body back on the answer and returns it too. It closes body when it does not send it. retry
// says whether the transport may send req again.
func (t *transport) attempt(req *http.Request, body io.ReadCloser, retry bool) (*http.Response, []byte, error) {
	if err := t.limit.wait(req.Context()); err != nil {
		if body != nil {
			_ = body.Close() // nothing was sent
		}
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(req.Context(), t.attemptTimeout)
	defer cancel()
	out := req.Clone(ctx)
	out.Body = body
	if !retry {
		// Go 1.26's HTTP/2 client sends a request again through GetBody when the server resets the stream with
		// PROTOCOL_ERROR, even after the server read it.
		out.GetBody = nil
	}
	out.Header.Set("Authorization", "Bearer "+string(t.Key))
	resp, data, err := t.roundTrip(out)
	if err != nil && ctx.Err() != nil && req.Context().Err() == nil {
		return nil, nil, fmt.Errorf("no answer within %v", t.attemptTimeout)
	}
	return resp, data, err
}

// roundTrip sends req through the base transport and reads the answer's whole body. It puts the body back on the
// answer and returns it too.
func (t *transport) roundTrip(req *http.Request) (*http.Response, []byte, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // read to the end or failed: either way nothing is left to lose
	if err != nil {
		return nil, nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, data, nil
}

// backoff returns the wait after attempt n, counting from 1: firstBackoff doubled n-1 times.
func (t *transport) backoff(n int) time.Duration {
	return t.firstBackoff << (n - 1)
}

// mayRetry reports whether the transport may send req again: its method is idempotent, and its body, if any, can be
// had again. A POST is never sent again: Vultr may have carried it out.
func mayRetry(req *http.Request) bool {
	switch cmp.Or(req.Method, http.MethodGet) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	}
	return false
}

// retryable reports whether an attempt's outcome is worth another attempt: a failed connection, 429, or a 5xx other
// than 501.
func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	s := resp.StatusCode
	return s == http.StatusTooManyRequests || (s >= 500 && s <= 599 && s != http.StatusNotImplemented)
}

// limiter spaces the starts of requests by an interval, and during a pause starts none. It is safe for concurrent
// use.
type limiter struct {
	interval time.Duration

	mu    sync.Mutex
	next  time.Time // the earliest start of the next request
	until time.Time // the end of the pause
}

// wait returns when a request may start, or with the context's error when ctx ends first.
func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		at := latest(time.Now(), l.next, l.until)
		l.next = at.Add(l.interval)
		l.mu.Unlock()
		if err := sleep(ctx, time.Until(at)); err != nil {
			return err
		}
		l.mu.Lock()
		paused := time.Now().Before(l.until) // a pause began during the wait
		l.mu.Unlock()
		if !paused {
			return nil
		}
	}
}

// pause starts no request before until.
func (l *limiter) pause(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.until = latest(l.until, until)
}

// latest returns the latest of times.
func latest(first time.Time, rest ...time.Time) time.Time {
	for _, t := range rest {
		if t.After(first) {
			first = t
		}
	}
	return first
}

// sleep waits for d, or until ctx ends and then returns ctx's error. It returns at once when ctx has ended.
func sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
