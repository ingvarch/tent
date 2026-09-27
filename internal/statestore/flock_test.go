package statestore_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

func TestFlockLocker(t *testing.T) {
	storetest.RunLocker(t, func(t *testing.T, _ func() time.Time) (string, func() statestore.Locker) {
		root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
		// Each locker opens the store itself and takes its own file handles, as another process would.
		return "prod", func() statestore.Locker { return newFlock(t, openFile(t, root), layout) }
	}, storetest.LockerOptions{ProcessBound: true})
}

// newFlock returns the locker that NewLocker picks for a file store, which must be flock.
func newFlock(t *testing.T, s statestore.Store, l statestore.Layout) statestore.Locker {
	t.Helper()
	releaseFlocksAtEnd(t)
	lk, m, err := statestore.NewLocker(t.Context(), s, l, nil)
	if err != nil || m != statestore.MechanismFlock {
		t.Fatalf("NewLocker = %q, %v; want flock", m, err)
	}
	return lk
}

// releaseFlocksAtEnd releases, when the test ends, the flock locks taken since now, as the end of the process would:
// Windows cannot remove the temp dir while their files are open. Call it after t.TempDir, so that it runs first.
func releaseFlocksAtEnd(t *testing.T) {
	before := statestore.HeldFlocks()
	t.Cleanup(func() {
		for _, h := range statestore.HeldFlocks() {
			if slices.Contains(before, h) {
				continue
			}
			if err := statestore.ReleaseFlock(h); err != nil {
				t.Errorf("release the flock lock on %s: %v", h, err)
			}
		}
		if n := len(statestore.HeldFlocks()); n > len(before) {
			t.Errorf("%d flock locks are still held at the end of the test", n-len(before))
		}
	})
}

// locksDir and leaseFile return where the flock locker of the cluster prod keeps its files.
func locksDir(root string) string  { return filepath.Join(root, ".tent-locks") }
func leaseFile(root string) string { return filepath.Join(locksDir(root), "prod.lease") }

// writeLeaseFile writes the lease file as a holder that died would leave it.
func writeLeaseFile(t *testing.T, root string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(locksDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaseFile(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func encodeLease(t *testing.T, l statestore.Lease) []byte {
	t.Helper()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeLease(t *testing.T, data []byte) statestore.Lease {
	t.Helper()
	var l statestore.Lease
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("the lease file holds %q: %v", data, err)
	}
	return l
}

func wantLeaseFile(t *testing.T, root string, want statestore.Lease) {
	t.Helper()
	data, err := os.ReadFile(leaseFile(root))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, decodeLease(t, data)); diff != "" {
		t.Errorf("the lease file (-want +got):\n%s", diff)
	}
}

func wantLocksDir(t *testing.T, root string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(locksDir(root))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %q, want %q", locksDir(root), got, want)
	}
}

func TestFlockLeaseFile(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	s := openFile(t, root)
	a, b := newFlock(t, s, layout), newFlock(t, openFile(t, root), layout)
	first := testLease("first", time.Hour)
	tryLockFree(t, a, first)
	wantLeaseFile(t, root, first)
	if diff := cmp.Diff(&first, holder(t, b)); diff != "" {
		t.Errorf("Holder (-want +got):\n%s", diff)
	}

	// Renew writes a new file and renames it over the old one, so a reader never sees half a lease.
	renewed := first
	renewed.ExpiresAt = renewed.ExpiresAt.Add(time.Hour)
	if runtime.GOOS == "windows" {
		renew(t, a, renewed) // Windows cannot replace a file that is open
	} else {
		f, err := os.Open(leaseFile(root))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		renew(t, a, renewed)
		old, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(first, decodeLease(t, old)); diff != "" {
			t.Errorf("the lease file opened before Renew (-want +got):\n%s", diff)
		}
	}
	wantLeaseFile(t, root, renewed)
	if diff := cmp.Diff(&renewed, holder(t, b)); diff != "" {
		t.Errorf("Holder after Renew (-want +got):\n%s", diff)
	}

	// The files are outside the objects, and no temp file is left.
	wantLocksDir(t, root, "prod.lease", "prod.lock")
	if paths, err := s.List(t.Context(), ""); err != nil || len(paths) != 0 {
		t.Errorf("List = %q, %v; want nothing", paths, err)
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]fs.FileMode{".": 0o700, "prod.lock": 0o600, "prod.lease": 0o600} {
			fi, err := os.Stat(filepath.Join(locksDir(root), name))
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != want {
				t.Errorf("%s: mode %v, want %v", name, got, want)
			}
		}
	}

	// Unlock removes the lease and keeps the empty lock file.
	if err := a.Unlock(t.Context(), renewed); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantLocksDir(t, root, "prod.lock")
	if fi, err := os.Stat(filepath.Join(locksDir(root), "prod.lock")); err != nil || fi.Size() != 0 {
		t.Errorf("the lock file: %v, %v; want it empty", fi, err)
	}
}

func tryLockFree(t *testing.T, lk statestore.Locker, l statestore.Lease) {
	t.Helper()
	if _, previous, err := lk.TryLock(t.Context(), l); err != nil || previous != nil {
		t.Fatalf("TryLock(%q) = previous %+v, %v; want the free lock", l.ID, previous, err)
	}
}

func renew(t *testing.T, lk statestore.Locker, l statestore.Lease) {
	t.Helper()
	if _, err := lk.Renew(t.Context(), l); err != nil {
		t.Fatalf("Renew: %v", err)
	}
}

func TestFlockRejectsEmptyID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	lk := newFlock(t, openFile(t, root), mustLayout(t, "prod"))
	if _, _, err := lk.TryLock(t.Context(), testLease("", time.Hour)); err == nil {
		t.Error("TryLock of a lease without an ID succeeded")
	}
	if h := holder(t, lk); h != nil {
		t.Errorf("TryLock of a lease without an ID took the lock for %+v", h)
	}
}

func TestFlockLockLostWhenNotHeld(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	lk := newFlock(t, openFile(t, root), mustLayout(t, "prod"))
	first, other := testLease("first", time.Hour), testLease("other", time.Hour)
	lost := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, statestore.ErrLockLost) {
			t.Errorf("%s: error = %v, want ErrLockLost", what, err)
		}
	}
	_, err := lk.Renew(t.Context(), first)
	lost("Renew before TryLock", err)
	lost("Unlock before TryLock", lk.Unlock(t.Context(), first))

	tryLockFree(t, lk, first)
	_, err = lk.Renew(t.Context(), other)
	lost("Renew of another lease", err)
	lost("Unlock of another lease", lk.Unlock(t.Context(), other))

	if err := lk.Unlock(t.Context(), first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	_, err = lk.Renew(t.Context(), first)
	lost("Renew after Unlock", err)
	lost("Unlock after Unlock", lk.Unlock(t.Context(), first))
}

// lockFileChanges delete or replace the lock file of the cluster prod while it is held, so a newcomer can lock a new
// file. Windows cannot delete or replace a file that is open, so the tests that use them skip there.
var lockFileChanges = []struct {
	name   string
	change func(t *testing.T, root string)
}{
	{"deleted", func(t *testing.T, root string) {
		if err := os.Remove(filepath.Join(locksDir(root), "prod.lock")); err != nil {
			t.Fatal(err)
		}
	}},
	{"replaced", func(t *testing.T, root string) {
		tmp := filepath.Join(locksDir(root), "new")
		if err := os.WriteFile(tmp, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(locksDir(root), "prod.lock")); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestFlockLockFileChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot delete or replace a file that is open")
	}
	for _, tc := range lockFileChanges {
		t.Run(tc.name, func(t *testing.T) {
			root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
			a, b := newFlock(t, openFile(t, root), layout), newFlock(t, openFile(t, root), layout)
			first := testLease("first", time.Hour)
			tryLockFree(t, a, first)
			tc.change(t, root)
			second := testLease("second", time.Hour)
			if _, _, err := b.TryLock(t.Context(), second); err != nil {
				t.Fatalf("TryLock of the new lock file: %v", err)
			}

			// The first holder learns it lost the lock, and leaves the new holder's lease alone.
			renewed := first
			renewed.ExpiresAt = renewed.ExpiresAt.Add(time.Hour)
			if _, err := a.Renew(t.Context(), renewed); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Renew: error = %v, want ErrLockLost", err)
			}
			wantLeaseFile(t, root, second)
			if err := a.Unlock(t.Context(), first); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Unlock: error = %v, want ErrLockLost", err)
			}
			wantLeaseFile(t, root, second)
			if diff := cmp.Diff(&second, holder(t, b)); diff != "" {
				t.Errorf("Holder (-want +got):\n%s", diff)
			}
			if _, err := a.Renew(t.Context(), renewed); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Renew after Unlock: error = %v, want ErrLockLost", err)
			}
		})
	}
}

// TestFlockLostWhenLockFileChanged checks that Acquire's renewals notice a deleted or replaced lock file.
func TestFlockLostWhenLockFileChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot delete or replace a file that is open")
	}
	for _, tc := range lockFileChanges {
		t.Run(tc.name, func(t *testing.T) {
			root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
			lock, err := statestore.Acquire(t.Context(), newFlock(t, openFile(t, root), layout), "prod",
				statestore.AcquireOptions{TTL: 30 * time.Millisecond}) // renewed every 10ms
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			tc.change(t, root)
			select {
			case <-lock.Lost():
			case <-time.After(10 * time.Second):
				t.Fatal("Lost() did not close within 10s")
			}
			if err := lock.Release(t.Context()); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Release: error = %v, want ErrLockLost", err)
			}
		})
	}
}

// TestFlockHeldAfterGC checks that a lock stays held when nothing refers to its locker any more: the finalizer of the
// lock's open file must not close it.
func TestFlockHeldAfterGC(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	first := testLease("first", time.Hour)
	func() { tryLockFree(t, newFlock(t, openFile(t, root), layout), first) }()
	collectGarbage()
	_, _, err := newFlock(t, openFile(t, root), layout).TryLock(t.Context(), testLease("second", time.Hour))
	wantLockedBy(t, err, first.ID)
}

// TestFlockUnlockForgetsTheHandle checks that Unlock no longer keeps the handle of the lock reachable, also when the
// lock was lost.
func TestFlockUnlockForgetsTheHandle(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	lk := newFlock(t, openFile(t, root), layout)
	before := len(statestore.HeldFlocks())
	wantHeld := func(what string, want int) {
		t.Helper()
		if n := len(statestore.HeldFlocks()); n != want {
			t.Errorf("%s: %d handles kept, want %d", what, n, want)
		}
	}
	first := testLease("first", time.Hour)
	tryLockFree(t, lk, first)
	wantHeld("after TryLock", before+1)
	if err := lk.Unlock(t.Context(), first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantHeld("after Unlock", before)

	if runtime.GOOS == "windows" {
		return // Windows cannot delete a file that is open, so the lock cannot be lost this way
	}
	tryLockFree(t, lk, first)
	if err := os.Remove(filepath.Join(locksDir(root), "prod.lock")); err != nil {
		t.Fatal(err)
	}
	if err := lk.Unlock(t.Context(), first); !errors.Is(err, statestore.ErrLockLost) {
		t.Fatalf("Unlock of a deleted lock file: error = %v, want ErrLockLost", err)
	}
	wantHeld("after Unlock of a lost lock", before)
}

// collectGarbage runs the garbage collector a few times and lets the finalizers run.
func collectGarbage() {
	for range 3 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFlockUnlockFailureKeepsTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	a, b := newFlock(t, openFile(t, root), layout), newFlock(t, openFile(t, root), layout)
	first := testLease("first", time.Hour)
	tryLockFree(t, a, first)
	// The lease file cannot be removed from a read-only directory.
	if err := os.Chmod(locksDir(root), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locksDir(root), 0o700) }) // so that the temp dir can be removed
	if err := a.Unlock(t.Context(), first); err == nil || errors.Is(err, statestore.ErrLockLost) {
		t.Errorf("Unlock from a read-only directory: error = %v, want another error than ErrLockLost", err)
	}

	// The lock is still held, and a retry releases it.
	if diff := cmp.Diff(&first, holder(t, b)); diff != "" {
		t.Errorf("Holder after the failed Unlock (-want +got):\n%s", diff)
	}
	if err := os.Chmod(locksDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(t.Context(), first); err != nil {
		t.Errorf("Unlock again: %v", err)
	}
	if h := holder(t, b); h != nil {
		t.Errorf("Holder after Unlock = %+v, want nil", h)
	}
}

// TestFlockHolderReadersTogether checks that readers of a free lock do not keep each other out.
func TestFlockHolderReadersTogether(t *testing.T) {
	const readers, reads = 8, 50
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	lk := newFlock(t, openFile(t, root), layout)
	first := testLease("first", time.Hour)
	tryLockFree(t, lk, first) // creates the lock file
	if err := lk.Unlock(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	lockers := make([]statestore.Locker, readers)
	for i := range lockers {
		lockers[i] = newFlock(t, openFile(t, root), layout)
	}
	failures := make(chan string, readers)
	var wg sync.WaitGroup
	for _, r := range lockers {
		wg.Go(func() {
			for range reads {
				if h, err := r.Holder(t.Context()); h != nil || err != nil {
					failures <- fmt.Sprintf("Holder = %+v, %v; want nil, nil", h, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}
}

// TestFlockWithoutLocksDir checks a store that has objects but has never been locked.
func TestFlockWithoutLocksDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	mustPut(t, s, "prod/cluster.yaml")
	lk := newFlock(t, s, mustLayout(t, "prod"))
	if h, err := lk.Holder(t.Context()); h != nil || err != nil {
		t.Errorf("Holder = %+v, %v; want nil, nil", h, err)
	}
	if removed, err := lk.ForceUnlock(t.Context()); removed != nil || err != nil {
		t.Errorf("ForceUnlock = %+v, %v; want nil, nil", removed, err)
	}
	wantMissing(t, locksDir(root))
}

// TestFlockReadersCreateNoLockFile checks that Holder and ForceUnlock of a cluster that was never locked create no
// lock file, which nothing would delete, when another cluster's lock has created the locks directory.
func TestFlockReadersCreateNoLockFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	other := newFlock(t, s, mustLayout(t, "other"))
	first := testLease("first", time.Hour)
	tryLockFree(t, other, first)
	if err := other.Unlock(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	lk := newFlock(t, s, mustLayout(t, "prod"))
	if h := holder(t, lk); h != nil {
		t.Errorf("Holder = %+v, want nil", h)
	}
	wantForceUnlock(t, lk, nil)
	wantLocksDir(t, root, "other.lock")

	// Nor over a lease left without its lock file, as when someone deleted the lock file.
	stale := testLease("stale", time.Hour)
	writeLeaseFile(t, root, encodeLease(t, stale))
	if h := holder(t, lk); h != nil {
		t.Errorf("Holder over a stale lease = %+v, want nil", h)
	}
	wantForceUnlock(t, lk, &stale)
	wantLocksDir(t, root, "other.lock")
}

// TestFlockTryLockOfReplacedFile checks that TryLock does not keep a lock on a file that was replaced while it locked
// it, since a newcomer could then lock the new file: it reports that the lock changed hands, so Acquire tries again.
// Nothing can pause TryLock at that point, so a helper process replaces the lock file over and over until TryLock
// sees it happen.
func TestFlockTryLockOfReplacedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot delete or replace a file that is open")
	}
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	if err := os.MkdirAll(locksDir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	h := startHelper(t, "TestFlockHelperProcess", flockHelperEnv+"=replace", helperURLEnv+"="+fileURL(root),
		helperFileEnv+"="+filepath.Join(locksDir(root), "prod.lock"))
	h.expect(t, "replacing")
	lk := newFlock(t, openFile(t, root), layout)
	mine := testLease("mine", time.Hour)
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, _, err := lk.TryLock(t.Context(), mine)
		if err != nil {
			wantUnnamed(t, "TryLock of a replaced lock file", err)
			if errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("TryLock error = %v, want it not to match ErrLockLost", err)
			}
			break
		}
		// The file was replaced after TryLock checked it, or not during the call at all.
		if err := lk.Unlock(t.Context(), mine); err != nil && !errors.Is(err, statestore.ErrLockLost) {
			t.Fatalf("Unlock: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("TryLock did not report a replaced lock file within 20s")
		}
	}
	if _, err := lk.Renew(t.Context(), mine); !errors.Is(err, statestore.ErrLockLost) {
		t.Errorf("Renew after the failed TryLock: error = %v, want ErrLockLost: the locker holds nothing", err)
	}
}

// TestFlockStaleLease checks a lease file under a free lock, which a holder that died leaves behind: the lock is free,
// TryLock takes it over and reports the old lease, and ForceUnlock removes it and reports it.
func TestFlockStaleLease(t *testing.T) {
	stale := testLease("stale", time.Hour) // not expired: only the free lock shows its holder is gone
	t.Run("TryLock", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		writeLeaseFile(t, root, encodeLease(t, stale))
		lk := newFlock(t, openFile(t, root), mustLayout(t, "prod"))
		if h := holder(t, lk); h != nil {
			t.Errorf("Holder = %+v, want nil: the lock is free", h)
		}
		mine := testLease("mine", time.Hour)
		_, previous, err := lk.TryLock(t.Context(), mine)
		if err != nil {
			t.Fatalf("TryLock: %v", err)
		}
		if diff := cmp.Diff(&stale, previous); diff != "" {
			t.Errorf("TryLock previous (-want +got):\n%s", diff)
		}
		wantLeaseFile(t, root, mine)
	})
	t.Run("ForceUnlock", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		writeLeaseFile(t, root, encodeLease(t, stale))
		lk := newFlock(t, openFile(t, root), mustLayout(t, "prod"))
		wantForceUnlock(t, lk, &stale)
		wantMissing(t, leaseFile(root))
		wantLocksDir(t, root) // kept for a TryLock that is about to create its lock file there
		wantForceUnlock(t, lk, nil)
		tryLockFree(t, lk, testLease("mine", time.Hour))
	})
}

func wantForceUnlock(t *testing.T, lk statestore.Locker, want *statestore.Lease) {
	t.Helper()
	removed, err := lk.ForceUnlock(t.Context())
	if err != nil {
		t.Fatalf("ForceUnlock: %v", err)
	}
	if diff := cmp.Diff(want, removed); diff != "" {
		t.Errorf("ForceUnlock removed (-want +got):\n%s", diff)
	}
}

func TestFlockUnreadableStaleLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	writeLeaseFile(t, root, []byte("not a lease"))
	lk := newFlock(t, openFile(t, root), mustLayout(t, "prod"))
	_, _, err := lk.TryLock(t.Context(), testLease("mine", time.Hour))
	if errors.Is(err, statestore.ErrLocked) {
		t.Errorf("TryLock over an unreadable lease: error = %v, want a read error", err)
	}
	wantInvalidLease(t, "TryLock", err)
	if h := holder(t, lk); h != nil {
		t.Errorf("Holder = %+v, want nil: the lock is free", h)
	}
	// ForceUnlock removes it anyway, and says it could not read it.
	removed, err := lk.ForceUnlock(t.Context())
	if removed != nil {
		t.Errorf("ForceUnlock of an unreadable lease removed %+v, want nil with the error", removed)
	}
	wantInvalidLease(t, "ForceUnlock", err)
	wantMissing(t, leaseFile(root))
	tryLockFree(t, lk, testLease("mine", time.Hour))
}

// stopHolder ends the error of a ForceUnlock that a running holder refuses.
const stopHolder = "; that tent is still running and holds a file lock that only it can release: " +
	"stop that process first"

func TestFlockForceUnlockWhileHeld(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	a, b := newFlock(t, openFile(t, root), layout), newFlock(t, openFile(t, root), layout)
	at := time.Date(2026, 9, 26, 12, 0, 1, 0, time.UTC)
	held := statestore.Lease{
		ID: "abc", Owner: "igor", Host: "mac", PID: 42, Operation: "update", AcquiredAt: at,
		ExpiresAt: at.Add(2 * time.Minute),
	}
	tryLockFree(t, a, held)
	removed, err := b.ForceUnlock(t.Context())
	const want = "cluster prod is locked by igor@mac (pid 42) for update since 2026-09-26 12:00:01 UTC" + stopHolder
	if removed != nil || err == nil || err.Error() != want {
		t.Errorf("ForceUnlock = %+v, %v\nwant nil and the error %s", removed, err, want)
	}
	wantLockedBy(t, err, held.ID)
	if diff := cmp.Diff(&held, holder(t, b)); diff != "" {
		t.Errorf("Holder after ForceUnlock (-want +got):\n%s", diff)
	}
}

// TestFlockHeldWithoutLease checks a held lock whose lease file is gone or unreadable: the lock is held all the same,
// by a holder that cannot be named, until the holder renews its lease.
func TestFlockHeldWithoutLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spoil      func(t *testing.T, root string)
		unreadable bool
	}{
		{"missing", func(t *testing.T, root string) {
			if err := os.Remove(leaseFile(root)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"unreadable", func(t *testing.T, root string) { writeLeaseFile(t, root, []byte("not a lease")) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
			a, b := newFlock(t, openFile(t, root), layout), newFlock(t, openFile(t, root), layout)
			first := testLease("first", time.Hour)
			tryLockFree(t, a, first)
			tc.spoil(t, root)

			_, _, err := b.TryLock(t.Context(), testLease("second", time.Hour))
			wantUnnamed(t, "TryLock", err)
			if got := errors.Is(err, statestore.ErrInvalidLease); got != tc.unreadable {
				t.Errorf("TryLock: errors.Is(%v, ErrInvalidLease) = %t, want %t", err, got, tc.unreadable)
			}
			h, err := b.Holder(t.Context())
			wantUnnamed(t, "Holder", err)
			if h != nil {
				t.Errorf("Holder = %+v, want nil with the error", h)
			}
			removed, err := b.ForceUnlock(t.Context())
			wantUnnamed(t, "ForceUnlock", err)
			if removed != nil {
				t.Errorf("ForceUnlock removed %+v, want nil with the error", removed)
			}
			if err == nil || !strings.HasSuffix(err.Error(), stopHolder) {
				t.Errorf("ForceUnlock error = %v, want it to end with %q", err, stopHolder)
			}

			renew(t, a, first)
			if diff := cmp.Diff(&first, holder(t, b)); diff != "" {
				t.Errorf("Holder after Renew (-want +got):\n%s", diff)
			}
		})
	}
}

// wantUnnamed checks that err says the lock is held, without a *LockedError: its holder cannot be named.
func wantUnnamed(t *testing.T, what string, err error) {
	t.Helper()
	var locked *statestore.LockedError
	if !errors.Is(err, statestore.ErrLocked) || errors.As(err, &locked) {
		t.Errorf("%s: error = %v, want ErrLocked without a holder", what, err)
	}
}

const (
	flockHelperEnv = "TENT_STATESTORE_FLOCK_HELPER" // what the helper does: hold, serialize or replace
	helperFileEnv  = "TENT_STATESTORE_HELPER_FILE"  // the file that a helper appends to or replaces
	// serializePause is how long a serializing helper holds the lock between its two lines. It is long enough that
	// processes without a lock would interleave their lines.
	serializePause = 100 * time.Millisecond
)

// helperLease is the lease that a holding helper process takes. It expired long ago: flock ignores expiry.
func helperLease(pid int) statestore.Lease {
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	return statestore.Lease{
		ID: "helper", Owner: "igor", Host: "mac", PID: pid, Operation: "update", AcquiredAt: at,
		ExpiresAt: at.Add(2 * time.Minute),
	}
}

func TestFlockHeldByAnotherProcess(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	h := startHelper(t, "TestFlockHelperProcess", flockHelperEnv+"=hold", helperURLEnv+"="+fileURL(root))
	h.expect(t, "locked")
	held := helperLease(h.pid())
	lk := newFlock(t, openFile(t, root), layout)

	// The live holder keeps the lock, although its lease has expired, and ForceUnlock cannot break it.
	_, _, err := lk.TryLock(t.Context(), testLease("mine", time.Hour))
	var locked *statestore.LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("TryLock error = %v, want a *LockedError", err)
	}
	if diff := cmp.Diff(held, locked.Holder); diff != "" {
		t.Errorf("TryLock names the holder (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&held, holder(t, lk)); diff != "" {
		t.Errorf("Holder (-want +got):\n%s", diff)
	}
	removed, err := lk.ForceUnlock(t.Context())
	if removed != nil || !errors.Is(err, statestore.ErrLocked) || !strings.Contains(err.Error(), "still running") {
		t.Errorf("ForceUnlock = %+v, %v; want the error that the holder still runs", removed, err)
	}

	// Killing the holder frees the lock: the OS releases it. Its lease file stays until ForceUnlock removes it.
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = h.wait() // killed
	waitFree(t, lk)
	wantLeaseFile(t, root, held)
	wantForceUnlock(t, lk, &held)
	wantForceUnlock(t, lk, nil)
	tryLockFree(t, lk, testLease("mine", time.Hour))
}

// waitFree waits until Holder reports the lock free. Windows may release a lock a little after its process ended.
func waitFree(t *testing.T, lk statestore.Locker) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h := holder(t, lk); h != nil; h = holder(t, lk) {
		if time.Now().After(deadline) {
			t.Fatalf("the lock is still held by %+v", h)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFlockSerializesProcesses runs two helper processes that lock the same cluster of the same file store and write
// two lines each while they hold the lock. The lock must keep their lines apart.
func TestFlockSerializesProcesses(t *testing.T) {
	root, out := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "out")
	var helpers [2]*helper
	for i := range helpers {
		helpers[i] = startHelper(t, "TestFlockHelperProcess", flockHelperEnv+"=serialize",
			helperURLEnv+"="+fileURL(root), helperFileEnv+"="+out)
	}
	for _, h := range helpers {
		h.expect(t, "ready")
	}
	for _, h := range helpers {
		h.send(t, "go")
	}
	for i, h := range helpers {
		if output, err := h.wait(); err != nil {
			t.Fatalf("helper %d: %v, output:\n%s\n%s", i, err, strings.Join(output, "\n"), h.stderr.String())
		}
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the lines of the helpers:\n%s", data)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	serial := func(first, second int) []string {
		return []string{
			fmt.Sprint("start ", first), fmt.Sprint("end ", first), fmt.Sprint("start ", second),
			fmt.Sprint("end ", second),
		}
	}
	a, b := helpers[0].pid(), helpers[1].pid()
	if !slices.Equal(lines, serial(a, b)) && !slices.Equal(lines, serial(b, a)) {
		t.Errorf("the helpers wrote %q, want %q or %q", lines, serial(a, b), serial(b, a))
	}
}

// TestFlockHelperProcess is a helper process of the cross-process flock tests, not a test.
func TestFlockHelperProcess(t *testing.T) {
	mode := os.Getenv(flockHelperEnv)
	if mode == "" {
		t.Skip("a helper process of the cross-process flock tests")
	}
	s, err := statestore.Open(t.Context(), os.Getenv(helperURLEnv))
	if err != nil {
		t.Fatal(err)
	}
	lk := newFlock(t, s, mustLayout(t, "prod"))
	switch mode {
	case "hold":
		if _, _, err := lk.TryLock(t.Context(), helperLease(os.Getpid())); err != nil {
			t.Fatal(err)
		}
		collectGarbage() // the locker is garbage now, and its lock must stay held
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin) // hold the lock until killed
	case "serialize":
		fmt.Println("ready")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatalf("waiting for go: %v", err)
		}
		lock, err := statestore.Acquire(t.Context(), lk, "prod",
			statestore.AcquireOptions{Operation: "update", Retry: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		out := os.Getenv(helperFileEnv)
		appendLine(t, out, fmt.Sprint("start ", os.Getpid()))
		time.Sleep(serializePause)
		appendLine(t, out, fmt.Sprint("end ", os.Getpid()))
		if err := lock.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
	case "replace":
		// Replace the file with a new one over and over until killed, or until stdin closes because the test ended.
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(done)
		}()
		name := os.Getenv(helperFileEnv)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			tmp := name + ".new"
			if err := os.WriteFile(tmp, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, name); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				fmt.Println("replacing")
			}
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func appendLine(t *testing.T, name, line string) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintln(f, line)
	if err = errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
}
