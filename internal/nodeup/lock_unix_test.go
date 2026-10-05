//go:build linux || darwin

package nodeup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tent-node.lock")
	unlock, err := nodeup.LockAt(t.Context(), path, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("LockAt: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the lock file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the lock file's mode is %#o, want 0600", info.Mode().Perm())
	}

	// A second caller waits for the holder, and its context bounds the wait.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := nodeup.LockAt(ctx, path, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("LockAt while held: %v, want the context's deadline", err)
	}

	// Once the holder leaves, the lock is free at once.
	unlock()
	second, err := nodeup.LockAt(t.Context(), path, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("LockAt after the holder left: %v", err)
	}
	second()
}

// TestLockWaitsForTheHolder checks that a caller gets the lock at its first try after the holder leaves it.
func TestLockWaitsForTheHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tent-node.lock")
		unlock, err := nodeup.LockAt(t.Context(), path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(2500 * time.Millisecond)
			unlock()
		}()
		start := time.Now()
		second, err := nodeup.LockAt(t.Context(), path, time.Second)
		if err != nil {
			t.Fatalf("LockAt while the holder leaves: %v", err)
		}
		second()
		// Tries at 0, 1 and 2 seconds find the lock held; the one at 3 takes it.
		if waited := time.Since(start); waited != 3*time.Second {
			t.Errorf("LockAt took the lock after %s, want 3s", waited)
		}
	})
}

// TestLockWithAnEndedContext checks that a caller whose context has ended, as SIGTERM ends tent-node's, does not take
// even a free lock, and says why.
func TestLockWithAnEndedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tent-node.lock")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(nodeuptest.Terminated)
	_, err := nodeup.LockAt(ctx, path, time.Millisecond)
	if want := "lock " + path + ": terminated signal received"; !errors.Is(err, context.Canceled) ||
		errText(err) != want {
		t.Errorf("LockAt with an ended context: %q, want %q, which matches context.Canceled", errText(err), want)
	}
	unlock, err := nodeup.LockAt(t.Context(), path, time.Millisecond)
	if err != nil {
		t.Fatalf("LockAt after the refusal: %v, want the lock free", err)
	}
	unlock()
}

// TestLockStopsWaitingWithItsContext checks that a caller that waits for the holder stops at once when its context
// ends, as SIGTERM ends tent-node's, and says why.
func TestLockStopsWaitingWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tent-node.lock")
		unlock, err := nodeup.LockAt(t.Context(), path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		ctx, cancel := context.WithCancelCause(t.Context())
		time.AfterFunc(1500*time.Millisecond, func() { cancel(nodeuptest.Terminated) })
		start := time.Now()
		_, err = nodeup.LockAt(ctx, path, time.Second)
		if want := "lock " + path + ": terminated signal received"; errText(err) != want {
			t.Errorf("LockAt while held: %q, want %q", errText(err), want)
		}
		if waited := time.Since(start); waited != 1500*time.Millisecond {
			t.Errorf("LockAt stopped after %s, want 1.5s", waited)
		}
	})
}

func TestLockNeedsItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "tent-node.lock")
	if _, err := nodeup.LockAt(t.Context(), path, time.Millisecond); err == nil {
		t.Fatal("LockAt with a missing directory: no error")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("LockAt error %q does not name the path", err)
	}
}
