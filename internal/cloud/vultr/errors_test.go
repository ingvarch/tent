package vultr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// sentinels are the classes of an *APIError.
var sentinels = []error{
	ErrNotFound, ErrRateLimited, ErrLimitReached, ErrInUse, ErrInvalid, ErrForbidden, ErrUnavailable,
}

// headers makes a header from keys and values in turn.
func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// answered is the record of a call that got an answer. header holds keys and values in turn.
func answered(method, path string, status int, body string, header ...string) callRecord {
	return callRecord{method: method, path: path, status: status, header: headers(header...), body: []byte(body)}
}

// wantClass checks that err matches kind and no other class; kind nil means none.
func wantClass(t *testing.T, err, kind error) {
	t.Helper()
	for _, s := range sentinels {
		if got, want := errors.Is(err, s), errors.Is(s, kind); got != want {
			t.Errorf("errors.Is(%v, %v) = %v, want %v", err, s, got, want)
		}
	}
}

// apiError builds the error of rec and checks that it is an *APIError.
func apiError(t *testing.T, rec *callRecord) *APIError {
	t.Helper()
	err := newAPIError(rec)
	e, ok := errors.AsType[*APIError](fmt.Errorf("call: %w", err))
	if !ok {
		t.Fatalf("newAPIError(%+v) = %v, want an *APIError", rec, err)
	}
	return e
}

func TestNewAPIError(t *testing.T) {
	const (
		feeLimit = "Server add failed: You have reached the maximum monthly fee limit for this account. To deploy " +
			"additional services, please request a limit increase."
		// What a VPC delete gets for 14-20 s after its instances are gone.
		attached = "The following servers are attached to this VPC network: 10.64.0.3, 10.64.0.4"
	)
	for _, tc := range []struct {
		name  string
		rec   callRecord
		kind  error         // the one class it matches; nil for none
		text  string        // Error()
		msg   string        // Message
		after time.Duration // RetryAfter
	}{
		{
			name: "invalid",
			rec:  answered("POST", "/v2/vpcs", 400, `{"error":"Invalid subnet","status":400}`),
			kind: ErrInvalid, text: "vultr: POST /v2/vpcs: 400 Bad Request: Invalid subnet", msg: "Invalid subnet",
		},
		{
			name: "unprocessable",
			rec:  answered("POST", "/v2/instances", 422, `{"error":"Invalid plan","status":422}`),
			kind: ErrInvalid, text: "vultr: POST /v2/instances: 422 Unprocessable Entity: Invalid plan",
			msg: "Invalid plan",
		},
		{
			name: "method not allowed",
			rec:  answered("PUT", "/v2/vpcs/1", 405, ""),
			kind: ErrInvalid, text: "vultr: PUT /v2/vpcs/1: 405 Method Not Allowed",
		},
		{
			name: "instance limit",
			rec: answered("POST", "/v2/instances", 400, `{"error":"You have reached the maximum number of instances `+
				`allowed on your account","status":400}`),
			kind: ErrLimitReached,
			text: "vultr: POST /v2/instances: 400 Bad Request: You have reached the maximum number of instances " +
				"allowed on your account",
			msg: "You have reached the maximum number of instances allowed on your account",
		},
		{
			name: "fee limit",
			rec:  answered("POST", "/v2/instances", 400, `{"error":"`+feeLimit+`","status":400}`),
			kind: ErrLimitReached, text: "vultr: POST /v2/instances: 400 Bad Request: " + feeLimit, msg: feeLimit,
		},
		{
			name: "object limit",
			rec:  answered("POST", "/v2/vpcs", 400, `{"error":"VPC limit reached for this region","status":400}`),
			kind: ErrLimitReached, text: "vultr: POST /v2/vpcs: 400 Bad Request: VPC limit reached for this region",
			msg: "VPC limit reached for this region",
		},
		{
			name: "limit on a 402",
			rec:  answered("POST", "/v2/instances", 402, `{"error":"Limit reached","status":402}`),
			kind: ErrLimitReached, text: "vultr: POST /v2/instances: 402 Payment Required: Limit reached",
			msg: "Limit reached",
		},
		{
			name: "limit on a 403",
			rec:  answered("POST", "/v2/vpcs", 403, `{"error":"VPC limit reached for this region","status":403}`),
			kind: ErrForbidden, text: "vultr: POST /v2/vpcs: 403 Forbidden: VPC limit reached for this region",
			msg: "VPC limit reached for this region",
		},
		{
			name: "limit on a 401",
			rec:  answered("GET", "/v2/instances", 401, `{"error":"API key limit reached","status":401}`),
			kind: ErrForbidden, text: "vultr: GET /v2/instances: 401 Unauthorized: API key limit reached",
			msg: "API key limit reached",
		},
		{
			name: "rate limit on a 400",
			rec:  answered("POST", "/v2/vpcs", 400, `{"error":"Rate-limit exceeded","status":400}`),
			kind: ErrInvalid, text: "vultr: POST /v2/vpcs: 400 Bad Request: Rate-limit exceeded",
			msg: "Rate-limit exceeded",
		},
		{
			name: "limits is a limit",
			rec:  answered("POST", "/v2/instances", 400, `{"error":"This exceeds your account limits","status":400}`),
			kind: ErrLimitReached, text: "vultr: POST /v2/instances: 400 Bad Request: This exceeds your account limits",
			msg: "This exceeds your account limits",
		},
		{
			name: "limited is not a limit",
			rec:  answered("POST", "/v2/vpcs", 400, `{"error":"Access is limited to the owner","status":400}`),
			kind: ErrInvalid, text: "vultr: POST /v2/vpcs: 400 Bad Request: Access is limited to the owner",
			msg: "Access is limited to the owner",
		},
		{
			name: "limitation is not a limit",
			rec:  answered("POST", "/v2/vpcs", 400, `{"error":"A limitation of the region","status":400}`),
			kind: ErrInvalid, text: "vultr: POST /v2/vpcs: 400 Bad Request: A limitation of the region",
			msg: "A limitation of the region",
		},
		{
			name: "a limit of objects in use",
			rec: answered("POST", "/v2/vpcs", 400,
				`{"error":"You have reached the limit of VPC networks in use","status":400}`),
			kind: ErrLimitReached,
			text: "vultr: POST /v2/vpcs: 400 Bad Request: You have reached the limit of VPC networks in use",
			msg:  "You have reached the limit of VPC networks in use",
		},
		{
			name: "rate limit on a 403",
			rec:  answered("GET", "/v2/instances", 403, `{"error":"Rate limit exceeded","status":403}`),
			kind: ErrForbidden, text: "vultr: GET /v2/instances: 403 Forbidden: Rate limit exceeded",
			msg: "Rate limit exceeded",
		},
		{
			name: "unauthorized",
			rec:  answered("GET", "/v2/instances", 401, `{"error":"Invalid API token.","status":401}`),
			kind: ErrForbidden, text: "vultr: GET /v2/instances: 401 Unauthorized: Invalid API token.",
			msg: "Invalid API token.",
		},
		{
			name: "forbidden",
			rec:  answered("GET", "/v2/instances", 403, `{"error":"Unauthorized IP address","status":403}`),
			kind: ErrForbidden, text: "vultr: GET /v2/instances: 403 Forbidden: Unauthorized IP address",
			msg: "Unauthorized IP address",
		},
		{
			name: "not found",
			rec:  answered("DELETE", "/v2/vpcs/1", 404, `{"error":"Invalid VPC ID","status":404}`),
			kind: ErrNotFound, text: "vultr: DELETE /v2/vpcs/1: 404 Not Found: Invalid VPC ID", msg: "Invalid VPC ID",
		},
		{
			name: "not found mentioning a limit",
			rec:  answered("GET", "/v2/limits", 404, `{"error":"No limits here","status":404}`),
			kind: ErrNotFound, text: "vultr: GET /v2/limits: 404 Not Found: No limits here", msg: "No limits here",
		},
		{
			name: "rate limited",
			rec: answered("GET", "/v2/instances", 429, `{"error":"Rate limit exceeded","status":429}`,
				"Retry-After", "3"),
			kind: ErrRateLimited, text: "vultr: GET /v2/instances: 429 Too Many Requests: Rate limit exceeded",
			msg: "Rate limit exceeded", after: 3 * time.Second,
		},
		{
			name: "server error without JSON",
			rec:  answered("GET", "/v2/instances", 500, "<html>\n  <body>Internal error</body>\n</html>\n"),
			kind: ErrUnavailable,
			text: "vultr: GET /v2/instances: 500 Internal Server Error: <html> <body>Internal error</body> </html>",
			msg:  "<html> <body>Internal error</body> </html>",
		},
		{
			name: "empty body",
			rec:  answered("GET", "/v2/instances", 502, ""),
			kind: ErrUnavailable, text: "vultr: GET /v2/instances: 502 Bad Gateway",
		},
		{
			name: "empty error field",
			rec:  answered("GET", "/v2/instances", 503, `{"error":"","status":503}`, "Retry-After", "7"),
			kind: ErrUnavailable,
			text: `vultr: GET /v2/instances: 503 Service Unavailable: {"error":"","status":503}`,
			msg:  `{"error":"","status":503}`, after: 7 * time.Second,
		},
		{
			name: "not implemented",
			rec:  answered("GET", "/v2/instances", 501, "no"),
			kind: ErrInvalid, text: "vultr: GET /v2/instances: 501 Not Implemented: no", msg: "no",
		},
		{
			name: "status without text",
			rec:  answered("GET", "/v2/instances", 520, "origin error"),
			kind: ErrUnavailable, text: "vultr: GET /v2/instances: 520: origin error", msg: "origin error",
		},
		{
			name: "conflict",
			rec:  answered("PUT", "/v2/vpcs/1", 409, `{"error":"Conflict","status":409}`),
			kind: ErrInUse, text: "vultr: PUT /v2/vpcs/1: 409 Conflict: Conflict", msg: "Conflict",
		},
		{
			name: "limit on a conflict",
			rec:  answered("PUT", "/v2/vpcs/1", 409, `{"error":"Limit reached","status":409}`),
			kind: ErrInUse, text: "vultr: PUT /v2/vpcs/1: 409 Conflict: Limit reached", msg: "Limit reached",
		},
		{
			name: "locked",
			rec:  answered("DELETE", "/v2/vpcs/1", 423, `{"error":"Locked","status":423}`),
			kind: ErrInUse, text: "vultr: DELETE /v2/vpcs/1: 423 Locked: Locked", msg: "Locked",
		},
		{
			name: "servers attached to a VPC",
			rec:  answered("DELETE", "/v2/vpcs/1", 400, `{"error":"`+attached+`","status":400}`),
			kind: ErrInUse, text: "vultr: DELETE /v2/vpcs/1: 400 Bad Request: " + attached, msg: attached,
		},
		{
			name: "in use",
			rec:  answered("DELETE", "/v2/firewalls/1", 400, `{"error":"The firewall group is IN USE","status":400}`),
			kind: ErrInUse, text: "vultr: DELETE /v2/firewalls/1: 400 Bad Request: The firewall group is IN USE",
			msg: "The firewall group is IN USE",
		},
		{
			name: "in use on a create is invalid",
			rec:  answered("POST", "/v2/ssh-keys", 400, `{"error":"The name is already in use","status":400}`),
			kind: ErrInvalid, text: "vultr: POST /v2/ssh-keys: 400 Bad Request: The name is already in use",
			msg: "The name is already in use",
		},
		{
			name: "in use on a 403",
			rec:  answered("DELETE", "/v2/firewalls/1", 403, `{"error":"The key is in use","status":403}`),
			kind: ErrForbidden, text: "vultr: DELETE /v2/firewalls/1: 403 Forbidden: The key is in use",
			msg: "The key is in use",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := apiError(t, &tc.rec)
			want := &APIError{
				Method: tc.rec.method, Path: tc.rec.path, Status: tc.rec.status, Message: tc.msg, RetryAfter: tc.after,
			}
			if diff := cmp.Diff(want, e, cmpopts.IgnoreUnexported(APIError{})); diff != "" {
				t.Errorf("fields (-want +got):\n%s", diff)
			}
			if got := e.Error(); got != tc.text {
				t.Errorf("Error() = %q, want %q", got, tc.text)
			}
			if e.Unwrap() != nil {
				t.Errorf("Unwrap() = %v, want nil for an answer", e.Unwrap())
			}
			wantClass(t, e, tc.kind)

			// NewAPIError builds the same error from the parts.
			built := NewAPIError(tc.rec.method, tc.rec.path, tc.rec.status, tc.msg, tc.after)
			if diff := cmp.Diff(want, built, cmpopts.IgnoreUnexported(APIError{})); diff != "" {
				t.Errorf("NewAPIError fields (-want +got):\n%s", diff)
			}
			if got := built.Error(); got != tc.text {
				t.Errorf("NewAPIError: Error() = %q, want %q", got, tc.text)
			}
			if built.Unwrap() != nil {
				t.Errorf("NewAPIError: Unwrap() = %v, want nil", built.Unwrap())
			}
			wantClass(t, built, tc.kind)
		})
	}
}

func TestNewAPIErrorWithoutErrorStatus(t *testing.T) {
	// A status below 400 matches no class, even with a message about a limit.
	for _, status := range []int{200, 302} {
		e := NewAPIError("GET", "/v2/vpcs", status, "limit reached", 0)
		wantClass(t, e, nil)
		want := fmt.Sprintf("vultr: GET /v2/vpcs: %d %s: limit reached", status, http.StatusText(status))
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
}

func TestNewNoAnswerError(t *testing.T) {
	cause := fmt.Errorf("read tcp 192.0.2.1:443: %w", context.DeadlineExceeded)
	e := NewNoAnswerError("POST", "/v2/instances", cause)
	if got, want := e.Error(), "vultr: POST /v2/instances: read tcp 192.0.2.1:443: context deadline exceeded"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	want := &APIError{Method: "POST", Path: "/v2/instances"}
	if diff := cmp.Diff(want, e, cmpopts.IgnoreUnexported(APIError{})); diff != "" {
		t.Errorf("fields (-want +got):\n%s", diff)
	}
	if !errors.Is(e, context.DeadlineExceeded) || !errors.Is(e.Unwrap(), cause) {
		t.Errorf("%v does not unwrap to its cause", e)
	}
	wantClass(t, e, ErrUnavailable)

	// The client builds the same error for a call without an answer.
	rec := callRecord{method: "POST", path: "/v2/instances", cause: cause}
	if c := apiError(t, &rec); c.Error() != e.Error() || !errors.Is(c, cause) {
		t.Errorf("NewNoAnswerError = %v, want the client's %v", e, c)
	}
}

func TestNewNoAnswerErrorWithoutCause(t *testing.T) {
	e := NewNoAnswerError("GET", "/v2/vpcs", nil)
	if got, want := e.Error(), "vultr: GET /v2/vpcs: no answer"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	wantClass(t, e, ErrUnavailable)
}

func TestNewAPIErrorTransport(t *testing.T) {
	cause := fmt.Errorf("read tcp 192.0.2.1:443: %w", context.DeadlineExceeded)
	rec := callRecord{method: "POST", path: "/v2/instances", cause: cause}
	e := apiError(t, &rec)
	if got, want := e.Error(), "vultr: POST /v2/instances: read tcp 192.0.2.1:443: context deadline exceeded"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if e.Method != "POST" || e.Path != "/v2/instances" || e.Status != 0 || e.Message != "" {
		t.Errorf("fields %+v, want the method and path only", e)
	}
	if !errors.Is(e, context.DeadlineExceeded) || !errors.Is(e.Unwrap(), cause) {
		t.Errorf("%v does not unwrap to its cause", e)
	}
	wantClass(t, e, ErrUnavailable)
}

func TestNewAPIErrorRetryAfterDate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { // a fixed clock
		for _, tc := range []struct {
			at   time.Time
			want time.Duration
		}{
			{time.Now().Add(5 * time.Second), 5 * time.Second},
			{time.Now().Add(-5 * time.Second), 0},
		} {
			rec := answered("GET", "/v2/instances", 429, "", "Retry-After", tc.at.UTC().Format(http.TimeFormat))
			if got := apiError(t, &rec).RetryAfter; got != tc.want {
				t.Errorf("RetryAfter for %s = %v, want %v", rec.header.Get("Retry-After"), got, tc.want)
			}
		}
	})
}

func TestNewAPIErrorNone(t *testing.T) {
	for _, rec := range []callRecord{
		{},
		answered("GET", "/v2/instances", 200, `{"instances":[]}`),
		answered("DELETE", "/v2/vpcs/1", 204, ""),
	} {
		if err := newAPIError(&rec); err != nil {
			t.Errorf("newAPIError(%+v) = %v, want nil", rec, err)
		}
	}
}

func TestNewAPIErrorLongMessage(t *testing.T) {
	// The second body has a two-byte character across the cut.
	for _, body := range []string{strings.Repeat("x", 2000), "x" + strings.Repeat("é", 1000)} {
		rec := answered("GET", "/v2/instances", 500, body)
		msg := apiError(t, &rec).Message
		if !strings.HasSuffix(msg, "…") || len(msg) > maxMessage+len("…") || !utf8.ValidString(msg) {
			t.Errorf("Message of %d bytes = %q, want at most %d bytes of valid text and …", len(body), msg,
				maxMessage)
		}
		if !strings.HasPrefix(body, strings.TrimSuffix(msg, "…")) {
			t.Errorf("Message %q does not start the body", msg)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"3", 3 * time.Second},
		{"3600", time.Hour}, // only the transport's own waits are capped
		{"-1", 0},
		{"soon", 0},
		{"99999999999999", 0}, // too long to be a time.Duration
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		if got := retryAfter(h, now); got != tc.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
