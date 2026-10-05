//go:build linux || darwin

package nodeup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/ingvarch/tent/internal/nodeup/retry"
)

// lock opens path, creating it for root alone, and takes an exclusive flock on it, trying again every wait until the
// context ends, and then fails with the context's cause, such as SIGTERM's. Closing the file frees the lock.
func lock(ctx context.Context, path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	for {
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, context.Cause(ctx))
		}
		switch err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
		case err == nil:
			return func() { _ = f.Close() }, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			if !retry.Sleep(ctx, wait) {
				_ = f.Close()
				return nil, fmt.Errorf("lock %s: %w", path, context.Cause(ctx))
			}
		default:
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
	}
}
