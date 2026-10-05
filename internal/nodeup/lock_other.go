//go:build !linux && !darwin

package nodeup

import (
	"context"
	"time"
)

// lock is a no-op on systems other than Linux and macOS: tent-node runs only on Linux, and preflight refuses anything
// else.
func lock(context.Context, string, time.Duration) (func(), error) { return func() {}, nil }
