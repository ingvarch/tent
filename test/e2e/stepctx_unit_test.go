package e2e

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitDone fails the test when ctx does not end within 30 seconds.
func waitDone(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the context did not end")
	}
}

// watchedContext is a context that never ends and records whether a wait on it was registered and stopped:
// context.AfterFunc uses its AfterFunc method.
type watchedContext struct {
	done       chan struct{}
	registered atomic.Bool
	stopped    atomic.Bool
}

func newWatchedContext() *watchedContext { return &watchedContext{done: make(chan struct{})} }

func (c *watchedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *watchedContext) Done() <-chan struct{}       { return c.done }
func (c *watchedContext) Err() error                  { return nil }
func (c *watchedContext) Value(any) any               { return nil }

func (c *watchedContext) AfterFunc(func()) func() bool {
	c.registered.Store(true)
	return func() bool {
		c.stopped.Store(true)
		return true
	}
}

func TestJoinContextEndsWhenTheOtherContextEnds(t *testing.T) {
	other, cancel := context.WithCancel(t.Context())
	ctx, release := joinContext(t.Context(), other)
	defer release()
	if ctx.Err() != nil {
		t.Fatalf("the joined context ended before anything did: %v", ctx.Err())
	}
	cancel()
	waitDone(ctx, t)
}

func TestJoinContextEndsWhenTheParentEndsAndLeavesTheOtherAlone(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	other := t.Context()
	ctx, release := joinContext(parent, other)
	defer release()
	cancel()
	waitDone(ctx, t)
	if other.Err() != nil {
		t.Errorf("the other context ended: %v", other.Err())
	}
}

func TestJoinContextReleaseEndsTheContextAndStopsTheWaitOnTheOther(t *testing.T) {
	other := newWatchedContext()
	ctx, release := joinContext(t.Context(), other)
	if !other.registered.Load() {
		t.Fatal("joinContext did not wait on the other context")
	}
	if other.stopped.Load() {
		t.Fatal("the wait was stopped before release")
	}
	release()
	if ctx.Err() == nil {
		t.Error("release did not end the context")
	}
	if !other.stopped.Load() {
		t.Error("release did not stop the wait on the other context")
	}
}

func TestStepContextIsReleasedWhenTheTestEnds(t *testing.T) {
	suiteCtx := newWatchedContext()
	var ctx context.Context
	t.Run("step", func(t *testing.T) {
		ctx = stepContext(suiteCtx, t)
		if suiteCtx.stopped.Load() {
			t.Error("the wait was stopped while the test ran")
		}
	})
	if ctx.Err() == nil {
		t.Error("the context of a test that ended is still live")
	}
	if !suiteCtx.stopped.Load() {
		t.Error("the wait on the suite's context outlived the test")
	}
}

func TestStepContextEndsWhenTheSuiteIsInterruptedAndTheCleanupOfTheTestStillRuns(t *testing.T) {
	suiteCtx, interrupt := context.WithCancel(t.Context())
	cleanupRan, stepEnded := false, false
	t.Run("step", func(t *testing.T) {
		t.Cleanup(func() { cleanupRan = true })
		ctx := stepContext(suiteCtx, t)
		interrupt()
		waitDone(ctx, t)
		stepEnded = true
	})
	if !stepEnded || !cleanupRan {
		t.Errorf("step ended %t, cleanup ran %t, want both after the interrupt", stepEnded, cleanupRan)
	}
}

// lockedBuffer is a writer that the AfterFunc goroutine and the test can use together.
type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

func TestAnnounceInterruptWritesTheLineOnceWhenTheContextEnds(t *testing.T) {
	ctx, interrupt := context.WithCancel(t.Context())
	var out lockedBuffer
	stop := announceInterrupt(ctx, &out)
	defer stop()
	if got := out.String(); got != "" {
		t.Fatalf("output before the interrupt = %q, want none", got)
	}
	interrupt()
	err := pollUntil(t.Context(), time.Millisecond, 30*time.Second, func(context.Context) string {
		if out.String() == "" {
			return "nothing written yet"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	interrupt()
	if got := out.String(); got != interruptedText {
		t.Errorf("output = %q, want %q", got, interruptedText)
	}
}

func TestAnnounceInterruptWritesNothingAfterStop(t *testing.T) {
	ctx, interrupt := context.WithCancel(t.Context())
	var out lockedBuffer
	stop := announceInterrupt(ctx, &out)
	if !stop() {
		t.Fatal("stop = false, want true: the line was already written or being written")
	}
	interrupt()
	if got := out.String(); got != "" {
		t.Errorf("output = %q, want none after stop", got)
	}
}
