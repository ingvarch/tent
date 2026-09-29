package nodeuptest

import (
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
)

// Version is tent-node's version on the hosts that NewHost returns.
const Version = "v0.3.0"

// NewHost returns a machine on the fakes fsys and r: a root process on linux/amd64 named name that runs tent-node
// Version, with a clock that stands at 2026-09-29 12:00 UTC and no logger.
func NewHost(name string, fsys *FS, r *Runner) *nodeup.Host {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return &nodeup.Host{
		FS: fsys, Runner: r,
		Version: Version, HostName: name, EUID: 0, OS: "linux", Arch: "amd64",
		Now: func() time.Time { return now },
	}
}
