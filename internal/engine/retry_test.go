package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"testing"
	"testing/synctest"
	"time"
)

func TestRetryable(t *testing.T) {
	if err := Retryable(nil, time.Second); err != nil {
		t.Errorf("Retryable(nil) = %v, want nil", err)
	}
	cause := &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}
	err := fmt.Errorf("apply: %w", Retryable(fmt.Errorf("create: %w", cause), 3*time.Second))
	if got, want := err.Error(), "apply: create: open x: file does not exist"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(%v, fs.ErrNotExist) = false", err)
	}
	if pe := (*fs.PathError)(nil); !errors.As(err, &pe) || pe != cause {
		t.Errorf("errors.As(%v, *fs.PathError) = %v, want the wrapped error", err, pe)
	}
	if r := (*retryable)(nil); !errors.As(err, &r) || r.after != 3*time.Second {
		t.Errorf("errors.As(%v, *retryable) = %v, want one with after 3s", err, r)
	}
}

// lowest and highest are draws for backoff that return the least and the greatest value.
func lowest(time.Duration) time.Duration    { return 0 }
func highest(n time.Duration) time.Duration { return n - 1 }

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		retry    int
		min, max time.Duration
	}{
		{0, 500 * time.Millisecond, time.Second},
		{1, time.Second, 2 * time.Second},
		{2, 2 * time.Second, 4 * time.Second},
		{3, 4 * time.Second, 8 * time.Second},
		{4, 8 * time.Second, 16 * time.Second},
		{5, 15 * time.Second, 30 * time.Second},
		{6, 15 * time.Second, 30 * time.Second},
		{1000, 15 * time.Second, 30 * time.Second},
	} {
		if got := backoff(tc.retry, lowest); got != tc.min {
			t.Errorf("backoff(%d) with the lowest draw = %v, want %v", tc.retry, got, tc.min)
		}
		if got := backoff(tc.retry, highest); got != tc.max {
			t.Errorf("backoff(%d) with the highest draw = %v, want %v", tc.retry, got, tc.max)
		}
		for range 100 {
			if got := backoff(tc.retry, rand.N[time.Duration]); got < tc.min || got > tc.max {
				t.Fatalf("backoff(%d) = %v, want between %v and %v", tc.retry, got, tc.min, tc.max)
			}
		}
	}
}

func TestRetryWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		retry   int
		timeout time.Duration // of the context; none when zero
		cancel  bool
		want    time.Duration
		ok      bool
	}{
		{name: "not retryable", err: errBoom},
		{name: "after", err: Retryable(errBoom, 7*time.Second), want: 7 * time.Second, ok: true},
		{
			name: "wrapped",
			err:  fmt.Errorf("apply: %w", Retryable(errBoom, 7*time.Second)),
			want: 7 * time.Second, ok: true,
		},
		{name: "backoff", err: Retryable(errBoom, 0), retry: 2, want: 2 * time.Second, ok: true},
		{name: "backoff for a negative after", err: Retryable(errBoom, -time.Second), want: 500 * time.Millisecond, ok: true},
		{
			name: "the wait ends before the deadline", err: Retryable(errBoom, 10*time.Second-1),
			timeout: 10 * time.Second, want: 10*time.Second - 1, ok: true,
		},
		{name: "the wait reaches the deadline", err: Retryable(errBoom, 10*time.Second), timeout: 10 * time.Second},
		{name: "the wait passes the deadline", err: Retryable(errBoom, time.Minute), timeout: 10 * time.Second},
		{name: "the context ended", err: Retryable(errBoom, time.Second), cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.timeout > 0 {
					ctx, cancel = context.WithTimeout(ctx, tc.timeout)
					defer cancel()
				}
				if tc.cancel {
					cancel()
				}
				wait, ok := retryWait(ctx, tc.err, tc.retry, lowest)
				if wait != tc.want || ok != tc.ok {
					t.Errorf("retryWait = %v, %v, want %v, %v", wait, ok, tc.want, tc.ok)
				}
			})
		})
	}
}

func TestSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := sleep(t.Context(), time.Minute); err != nil {
			t.Errorf("sleep = %v, want nil", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := sleep(ctx, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("sleep past the deadline = %v, want context.DeadlineExceeded", err)
		}
		if got, want := time.Since(start), time.Minute+time.Second; got != want {
			t.Errorf("slept %v, want %v", got, want)
		}
	})
}
