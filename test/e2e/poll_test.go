package e2e

import (
	"context"
	"fmt"
	"time"
)

// pollUntil calls check at once and then every `every` until it returns "", which means done, or until timeout has
// passed or ctx has ended. The error wraps the reason the wait ended and ends with the last problem check
// returned; a problem that only reports that the wait's context ended does not replace an earlier one.
func pollUntil(ctx context.Context, every, timeout time.Duration, check func(context.Context) string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	last := ""
	for {
		problem := check(ctx)
		if problem == "" {
			return nil
		}
		if ctx.Err() == nil || last == "" {
			last = problem
		}
		// select may pick a tick that is due together with the end, so the end is read again.
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (waiting up to %s): %s", ctx.Err(), timeout, last)
		}
	}
}
