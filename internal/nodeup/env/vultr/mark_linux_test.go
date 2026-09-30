//go:build linux

package vultr

import (
	"errors"
	"net/http"
	"slices"
	"syscall"
	"testing"

	"github.com/ingvarch/tent/internal/nodeup/env"
)

// rawConn is a socket whose Control runs its function on fd, or fails with err.
type rawConn struct {
	fd  uintptr
	err error
}

func (c rawConn) Control(f func(fd uintptr)) error {
	if c.err != nil {
		return c.err
	}
	f(c.fd)
	return nil
}

func (rawConn) Read(func(fd uintptr) bool) error  { return nil }
func (rawConn) Write(func(fd uintptr) bool) error { return nil }

// setsockoptCall is one call of setsockoptInt.
type setsockoptCall struct{ fd, level, opt, value int }

// stubSetsockopt makes setsockoptInt record its calls and return err until the test ends. It returns the calls.
func stubSetsockopt(t *testing.T, err error) *[]setsockoptCall {
	t.Helper()
	var calls []setsockoptCall
	orig := setsockoptInt
	setsockoptInt = func(fd, level, opt, value int) error {
		calls = append(calls, setsockoptCall{fd, level, opt, value})
		return err
	}
	t.Cleanup(func() { setsockoptInt = orig })
	return &calls
}

// TestMarkSocket checks that markSocket sets SO_MARK of the socket to env.MetadataMark.
func TestMarkSocket(t *testing.T) {
	calls := stubSetsockopt(t, nil)
	if err := markSocket("tcp4", "169.254.169.254:80", rawConn{fd: 7}); err != nil {
		t.Fatalf("markSocket: %v", err)
	}
	want := []setsockoptCall{{7, syscall.SOL_SOCKET, syscall.SO_MARK, env.MetadataMark}}
	if !slices.Equal(*calls, want) {
		t.Errorf("setsockopt calls = %v, want %v", *calls, want)
	}
}

// TestMarkSocketErrors checks that markSocket returns the error of the mark, with the capabilities it needs, and the
// error of a socket it cannot control.
func TestMarkSocketErrors(t *testing.T) {
	errClosed := errors.New("use of closed file")
	for _, tc := range []struct {
		name       string
		setErr     error
		controlErr error
		cause      error
		want       string
	}{
		{"no permission", syscall.EPERM, nil, syscall.EPERM,
			"mark the socket for the metadata service: operation not permitted (tent-node needs CAP_NET_ADMIN or " +
				"CAP_NET_RAW)"},
		{"a closed socket", nil, errClosed, errClosed, "mark the socket for the metadata service: use of closed file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSetsockopt(t, tc.setErr)
			err := markSocket("tcp4", "169.254.169.254:80", rawConn{fd: 7, err: tc.controlErr})
			if err == nil || err.Error() != tc.want || !errors.Is(err, tc.cause) {
				t.Errorf("markSocket() error = %v, want %s", err, tc.want)
			}
		})
	}
}

// TestNewMarksItsSockets checks that the dialer of New marks each socket before it connects, and that a failed mark
// fails the dial. It dials a loopback address, so that a dialer that does not mark reaches no other host.
func TestNewMarksItsSockets(t *testing.T) {
	calls := stubSetsockopt(t, syscall.EPERM)
	transport := New().client.Transport
	tr, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("New's transport is a %T, want an *http.Transport", transport)
	}
	conn, err := tr.DialContext(t.Context(), "tcp", "127.0.0.1:9")
	if err == nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("dial error = %v, want the mark's %v", err, syscall.EPERM)
	}
	if len(*calls) != 1 || (*calls)[0].level != syscall.SOL_SOCKET || (*calls)[0].opt != syscall.SO_MARK ||
		(*calls)[0].value != env.MetadataMark {
		t.Errorf("setsockopt calls = %v, want one that sets SO_MARK to %#x", *calls, env.MetadataMark)
	}
}
