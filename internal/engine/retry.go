package engine

import (
	"context"
	"errors"
	"time"
)

// retryable is an error that the engine may retry.
type retryable struct {
	err   error
	after time.Duration // the wait before the next attempt; a backoff when not positive
}

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

// Retryable marks err as one the engine may retry: after `after` when it is positive, otherwise after a backoff. The
// result prints as err, and errors.Is and errors.As see err through it. Retryable(nil, after) is nil.
func Retryable(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryable{err: err, after: after}
}

// The backoff between attempts starts at firstBackoff and doubles up to maxBackoff.
const (
	firstBackoff = time.Second
	maxBackoff   = 30 * time.Second
)

// backoff returns the wait before retry n, counting from 0: a uniform draw from [d/2, d], where d is firstBackoff
// doubled n times, at most maxBackoff. draw(m) returns a uniform value in [0, m).
func backoff(n int, draw func(time.Duration) time.Duration) time.Duration {
	d := firstBackoff
	for i := 0; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	d = min(d, maxBackoff)
	return d/2 + draw(d-d/2+1)
}

// retryWait returns the wait before retry n, counting from 0, of an attempt that failed with err, and whether to
// retry at all: only when err is retryable, ctx has not ended and the wait ends before ctx's deadline.
func retryWait(ctx context.Context, err error, n int, draw func(time.Duration) time.Duration) (time.Duration, bool) {
	var r *retryable
	if !errors.As(err, &r) || ctx.Err() != nil {
		return 0, false
	}
	wait := r.after
	if wait <= 0 {
		wait = backoff(n, draw)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Add(wait).Before(deadline) {
		return 0, false
	}
	return wait, true
}

// sleep waits for d, or until ctx ends and then returns ctx's error.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
