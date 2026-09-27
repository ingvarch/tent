package vultr

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Classes of the Vultr API's errors, for errors.Is. An *APIError matches at most one of them. 401, 403, 404, 409,
// 423, 429 and 5xx get their class from the status alone; for any other 4xx the message may decide.
var (
	// ErrNotFound means the object or the endpoint does not exist: 404.
	ErrNotFound = errors.New("not found")
	// ErrRateLimited means Vultr refused the request because it got too many: 429. The request was not carried out.
	ErrRateLimited = errors.New("rate limited")
	// ErrLimitReached means a limit of the account or of an object was reached, for example the account's instance
	// limit, 5 VPCs per region or 50 rules per firewall group: a 4xx whose message has the word "limit" or "limits", or
	// "reached the maximum", and does not speak of the rate limit. Vultr raises account limits on request; the other
	// limits stay.
	ErrLimitReached = errors.New("limit reached")
	// ErrInUse means the object is busy or still in use, and the same request may succeed later: 409, 423, or a 4xx
	// to a DELETE whose message says the object is attached or in use, such as the 400 that a VPC delete gets while
	// Vultr still counts servers attached to the VPC.
	ErrInUse = errors.New("in use")
	// ErrInvalid means the request itself is wrong or the API does not offer it, and the same request gets the same
	// answer: 501, and 400, 405, 413, 414, 415 and 422 whose message puts them in no other class.
	ErrInvalid = errors.New("invalid request")
	// ErrForbidden means the API key is wrong or may not do this: 401 and 403.
	ErrForbidden = errors.New("forbidden")
	// ErrUnavailable means the API did not answer, or failed: a 5xx other than 501, or a failed connection. For a
	// POST the outcome is unknown: the call may have been carried out.
	ErrUnavailable = errors.New("unavailable")
)

// maxMessage bounds APIError.Message in bytes, since a proxy in front of the API may answer with a whole HTML page.
const maxMessage = 512

// Words that put a message into a class: limitWords find a limit, rateLimitWords rule out the rate limit, and
// inUseWords find an object that is attached or in use.
var (
	limitWords     = regexp.MustCompile(`(?i)\blimits?\b|\breached the maximum\b`)
	rateLimitWords = regexp.MustCompile(`(?i)\brate[ -]limit`)
	inUseWords     = regexp.MustCompile(`(?i)\bare attached\b|\bin use\b`)
)

// APIError is a failed call to the Vultr API: an answer with an error status, no answer at all, or a success of a
// POST that the client cannot read.
type APIError struct {
	Method string // the HTTP method, such as POST
	Path   string // the URL path, such as /v2/vpcs
	Status int    // the HTTP status; 0 when the call got no answer
	// Message is the answer's error text: the error field of its JSON body, or else the body itself, on one line and
	// cut to 512 bytes.
	Message string
	// RetryAfter is how long the answer's Retry-After header asks to wait; 0 when it asks for no wait.
	RetryAfter time.Duration

	kind  error // the class the error matches, or nil
	cause error // why the call got no answer that the client can read
}

// Error prints "vultr: METHOD path: status text: message" for an answer with an error status, and
// "vultr: METHOD path: cause" for a call without an answer that the client can read.
func (e *APIError) Error() string {
	s := "vultr: " + e.Method + " " + e.Path + ": "
	if e.cause != nil {
		return s + e.cause.Error()
	}
	s += strconv.Itoa(e.Status)
	if text := http.StatusText(e.Status); text != "" {
		s += " " + text
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is makes errors.Is hold for the error's class.
func (e *APIError) Is(target error) bool { return e.kind != nil && target == e.kind }

// Unwrap returns why the call got no answer that the client can read, such as context.DeadlineExceeded, or nil for
// an answer with an error status.
func (e *APIError) Unwrap() error { return e.cause }

// errNoAnswer is the cause of a call without an answer when none is given.
var errNoAnswer = errors.New("no answer")

// NewAPIError returns the error of a call that got an answer with status. message is the answer's error text, and
// retryAfter the wait it asks for. The error matches the class that the client gives such an answer; a status below
// 400 matches none. It lets a stand-in for the client, such as a fake, fail as the client does.
func NewAPIError(method, path string, status int, message string, retryAfter time.Duration) *APIError {
	e := &APIError{Method: method, Path: path, Status: status, Message: message, RetryAfter: retryAfter}
	if status >= http.StatusBadRequest {
		e.kind = class(method, status, message)
	}
	return e
}

// NewNoAnswerError returns the error of a call that got no answer because of cause, such as a lost connection or the
// end of its context. The error matches ErrUnavailable and unwraps to cause.
func NewNoAnswerError(method, path string, cause error) *APIError {
	if cause == nil {
		cause = errNoAnswer
	}
	return &APIError{Method: method, Path: path, kind: ErrUnavailable, cause: cause}
}

// newAPIError returns the *APIError of a recorded call, or nil when the call got an answer without an error status
// or the record is empty.
func newAPIError(rec *callRecord) error {
	switch {
	case rec.cause != nil:
		return NewNoAnswerError(rec.method, rec.path, rec.cause)
	case rec.status < http.StatusBadRequest:
		return nil
	}
	return NewAPIError(rec.method, rec.path, rec.status, message(rec.body), retryAfter(rec.header, time.Now()))
}

// class returns the class of an answer to method with an error status and its message, or nil when none fits. The
// status decides first, then the message: a limit, then, for a DELETE, an object in use. ErrInvalid comes last, so
// that a 400 about a limit or an object in use gets the class of its message. Only a DELETE reads "in use" from the
// message: a create told that a name is in use would get the same answer every time.
func class(method string, status int, msg string) error {
	switch status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrForbidden
	case http.StatusConflict, http.StatusLocked:
		return ErrInUse
	case http.StatusNotImplemented:
		return ErrInvalid
	}
	switch {
	case status >= http.StatusInternalServerError:
		return ErrUnavailable
	case limitWords.MatchString(msg) && !rateLimitWords.MatchString(msg):
		return ErrLimitReached
	case method == http.MethodDelete && inUseWords.MatchString(msg):
		return ErrInUse
	}
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
		http.StatusRequestURITooLong, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return ErrInvalid
	}
	return nil
}

// message returns the error text of an answer's body: its JSON error field, or else the body itself. Either comes on
// one line, cut to maxMessage bytes.
func message(body []byte) string {
	var v struct {
		Error string `json:"error"`
	}
	text := string(body)
	if json.Unmarshal(body, &v) == nil && strings.TrimSpace(v.Error) != "" {
		text = v.Error
	}
	text = strings.Join(strings.Fields(strings.ToValidUTF8(text, "\uFFFD")), " ")
	if len(text) > maxMessage {
		text = strings.ToValidUTF8(text[:maxMessage], "") + "…" // drops a character cut in half
	}
	return text
}

// retryAfter returns the wait that a Retry-After header asks for, given in seconds or as an HTTP date. It returns 0
// without the header, for a wait that has passed, and for a value it cannot read.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 || n > int64(math.MaxInt64/time.Second) {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}
