package e2e

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPollUntilReturnsAtOnceWhenTheFirstCheckPasses(t *testing.T) {
	calls := 0
	err := pollUntil(t.Context(), time.Hour, time.Hour, func(context.Context) string {
		calls++
		return ""
	})
	if err != nil || calls != 1 {
		t.Errorf("pollUntil = %v after %d calls, want nil after 1", err, calls)
	}
}

func TestPollUntilChecksAgainUntilTheCheckPasses(t *testing.T) {
	calls := 0
	err := pollUntil(t.Context(), time.Millisecond, time.Minute, func(context.Context) string {
		calls++
		if calls < 3 {
			return "not yet"
		}
		return ""
	})
	if err != nil || calls != 3 {
		t.Errorf("pollUntil = %v after %d calls, want nil after 3", err, calls)
	}
}

func TestPollUntilGivesUpAtTheTimeoutWithTheLastProblem(t *testing.T) {
	calls := 0
	err := pollUntil(t.Context(), time.Millisecond, 200*time.Millisecond, func(ctx context.Context) string {
		calls++
		if calls == 5 {
			<-ctx.Done()
			return "the wait ended"
		}
		return "problem " + string(rune('0'+calls))
	})
	if err == nil {
		t.Fatal("pollUntil = nil, want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("pollUntil = %v, want it to wrap the deadline", err)
	}
	if !strings.Contains(err.Error(), "200ms") || !strings.HasSuffix(err.Error(), "problem 4") {
		t.Errorf("pollUntil = %q, want the timeout and %q at its end", err, "problem 4")
	}
	if calls != 5 {
		t.Errorf("check ran %d times, want 5: none after the context ended", calls)
	}
}

// spinFor waits for d without sleeping, so a timer that is due is due when the caller goes on.
func spinFor(d time.Duration) {
	for start := time.Now(); time.Since(start) < d; {
		runtime.Gosched()
	}
}

func TestPollUntilDoesNotCheckAgainOnceTheContextHasEnded(t *testing.T) {
	const every = 100 * time.Microsecond
	// With the end and a tick both due, select picks one at random: repeat until a wrong pick is certain.
	for range 60 {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		err := pollUntil(ctx, every, time.Minute, func(context.Context) string {
			calls++
			if calls == 2 {
				cancel()
				spinFor(20 * every)
			}
			return "problem"
		})
		cancel()
		if !errors.Is(err, context.Canceled) || calls != 2 {
			t.Fatalf("pollUntil = %v after %d checks, want the cancellation after 2", err, calls)
		}
	}
}

func TestPollUntilKeepsTheProblemBeforeTheContextEndedWhenTheLastCheckOnlySawTheEnd(t *testing.T) {
	calls := 0
	err := pollUntil(t.Context(), time.Millisecond, 20*time.Millisecond, func(ctx context.Context) string {
		calls++
		if calls == 1 {
			return "the real problem"
		}
		<-ctx.Done()
		return ctx.Err().Error()
	})
	if err == nil || !strings.HasSuffix(err.Error(), "the real problem") {
		t.Errorf("pollUntil = %v, want it to end with the real problem", err)
	}
}

func TestPollUntilStopsWhenTheParentContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	err := pollUntil(ctx, time.Millisecond, time.Hour, func(context.Context) string {
		cancel()
		return "waiting"
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("pollUntil = %v, want it to wrap the cancellation", err)
	}
}
