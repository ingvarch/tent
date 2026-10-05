package nodeuptest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
)

// Version is tent-node's version on the hosts that NewHost returns.
const Version = "v0.3.0"

// NewHost returns a machine on the fakes fsys and r: a root process on linux/amd64 named name that runs tent-node
// Version, with a clock that stands at 2026-09-29 12:00 UTC, no logger and no network: its transport fails every
// request and its dialer every call, so a test serves what the machine downloads and dials.
func NewHost(name string, fsys *FS, r *Runner) *nodeup.Host {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return &nodeup.Host{
		FS: fsys, Runner: r, Transport: noNetwork{},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("nodeuptest: the machine has no network: dial the servers with a stub")
		},
		Version: Version, HostName: name, EUID: 0, OS: "linux", Arch: "amd64",
		Now: func() time.Time { return now },
	}
}

// Terminated is the cause that signal.NotifyContext gives a context that SIGTERM ends, as tent-node's main makes
// it: it says so as Go 1.26 does, and matches context.Canceled.
var Terminated error = terminated{}

type terminated struct{}

func (terminated) Error() string        { return "terminated signal received" }
func (terminated) Is(target error) bool { return target == context.Canceled }

// noNetwork is a transport that fails every request.
type noNetwork struct{}

func (noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("nodeuptest: the machine has no network: serve the files with httptest")
}
