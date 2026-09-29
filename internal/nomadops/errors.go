package nomadops

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// Classes of the errors of the calls, for errors.Is. An error that does not match ErrNotReady is permanent: the same
// call fails the same way. A call that fails because the caller's context ended matches the context's error instead.
var (
	// ErrNotReady means the server did not answer, broke its answer off, did not answer in time, answered with a 5xx
	// or a 429, or has no leader. The same call may succeed later, on this server or on another one. For a write the
	// outcome is unknown: the server may have carried it out.
	ErrNotReady = errors.New("not ready")
	// ErrBootstrapMismatch means the cluster's ACL system was bootstrapped with a secret other than the one given.
	ErrBootstrapMismatch = errors.New(
		"the ACL system was bootstrapped with a secret other than the one in secrets/acl-bootstrap-token")
)

// maxMessage bounds the message of an answer in bytes, since a proxy may answer with a whole HTML page.
const maxMessage = 512

// errNoLeader is why Leader fails when the server knows no leader.
var errNoLeader = errors.New("no leader")

// callError is a failed call: an answer with an error status, no answer, or an answer that the client cannot use.
type callError struct {
	method, path string
	status       int    // the status of the answer; 0 when there was none
	message      string // the answer's body, on one line and cut to maxMessage bytes
	notReady     bool   // the error matches ErrNotReady
	cause        error  // why the call failed without an answer, or why the answer is of no use
}

// Error prints "nomad: METHOD path: status: message" for an answer with an error status, and
// "nomad: METHOD path: cause" otherwise.
func (e *callError) Error() string {
	s := "nomad: " + e.method + " " + e.path + ": "
	if e.cause != nil {
		return s + e.cause.Error()
	}
	s += strconv.Itoa(e.status)
	if e.message != "" {
		s += ": " + e.message
	}
	return s
}

// Is makes errors.Is hold for ErrNotReady when the error is of that class.
func (e *callError) Is(target error) bool { return e.notReady && target == ErrNotReady }

// Unwrap returns the cause of the error, such as the error of the caller's context, or nil for an answer with an
// error status.
func (e *callError) Unwrap() error { return e.cause }

// newCallError returns the error of a call that failed with err, the error of the Nomad API module, while both the
// caller's context and the call's own went on.
func newCallError(method, path string, err error) error {
	e := &callError{method: method, path: path}
	var answer api.UnexpectedResponseError
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case errors.As(err, &answer):
		e.status, e.message = answer.StatusCode(), message(answer.Body())
		e.notReady = e.status == http.StatusTooManyRequests || e.status >= http.StatusInternalServerError
	case errors.As(err, &urlErr):
		e.cause, e.notReady = urlErr.Err, !tlsFailure(urlErr.Err)
	default: // an answer that broke off or that the client cannot read
		e.cause = err
		e.notReady = !tlsFailure(err) && (errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr))
	}
	return e
}

// tlsFailure reports whether err is a failed TLS handshake that a new attempt fails the same way: a server certificate
// that the client does not trust, an alert from a server that does not trust the client, or a server that does not
// speak TLS, such as one that answers in plain HTTP.
func tlsFailure(err error) bool {
	var verify *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var op *net.OpError
	return errors.As(err, &verify) || errors.As(err, &header) || errors.Is(err, http.ErrSchemeMismatch) ||
		errors.As(err, &op) && op.Op == "remote error"
}

// message returns the body of an answer on one line, cut to maxMessage bytes.
func message(body string) string {
	text := strings.Join(strings.Fields(strings.ToValidUTF8(body, "\uFFFD")), " ")
	if len(text) > maxMessage {
		text = strings.ToValidUTF8(text[:maxMessage], "") + "…" // drops a character cut in half
	}
	return text
}
