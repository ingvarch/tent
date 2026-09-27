package statestore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

// settle is the best-effort locker's wait in tests.
const settle = 10 * time.Millisecond

func TestLeaseLocker(t *testing.T) {
	storetest.RunLocker(t, func(t *testing.T, now func() time.Time) (string, func() statestore.Locker) {
		root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
		return "prod", func() statestore.Locker {
			return statestore.NewLeaseLocker(openFile(t, root), layout, now)
		}
	}, storetest.LockerOptions{})
}

func TestBestEffortLocker(t *testing.T) {
	storetest.RunLocker(t, func(t *testing.T, now func() time.Time) (string, func() statestore.Locker) {
		root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
		return "prod", func() statestore.Locker {
			return statestore.NewBestEffortLocker(noConditions{openFile(t, root)}, layout, now, settle)
		}
	}, storetest.LockerOptions{Weak: true})
}

// lockers are the constructors of the lease lockers, for tests that hold for both.
var lockers = []struct {
	name string
	new  func(s statestore.Store, l statestore.Layout, clock func() time.Time) statestore.Locker
}{
	{"lease", statestore.NewLeaseLocker},
	{"best-effort", func(s statestore.Store, l statestore.Layout, clock func() time.Time) statestore.Locker {
		return statestore.NewBestEffortLocker(noConditions{s}, l, clock, settle)
	}},
}

func TestLeaseLockersRejectEmptyID(t *testing.T) {
	for _, tc := range lockers {
		t.Run(tc.name, func(t *testing.T) {
			lk := tc.new(openFile(t, t.TempDir()), mustLayout(t, "prod"), time.Now)
			if _, _, err := lk.TryLock(t.Context(), testLease("", time.Hour)); err == nil {
				t.Error("TryLock of a lease without an ID succeeded")
			}
			if h := holder(t, lk); h != nil {
				t.Errorf("TryLock of a lease without an ID wrote %+v", h)
			}
		})
	}
}

// TestLeaseLockersNeverWriteUnheld checks that a locker that holds nothing never writes: an empty version would
// make a conditional put unconditional.
func TestLeaseLockersNeverWriteUnheld(t *testing.T) {
	for _, tc := range lockers {
		t.Run(tc.name, func(t *testing.T) {
			s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
			other := testLease("", time.Hour) // another holder's, without an ID
			putLease(t, s, layout, other)
			lk := tc.new(s, layout, time.Now)
			_, err := lk.Renew(t.Context(), statestore.Lease{ExpiresAt: time.Now().Add(2 * time.Hour)})
			if !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Renew by a locker that holds nothing: error = %v, want ErrLockLost", err)
			}
			if err := lk.Unlock(t.Context(), statestore.Lease{}); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Unlock by a locker that holds nothing: error = %v, want ErrLockLost", err)
			}
			if diff := cmp.Diff(&other, holder(t, lk)); diff != "" {
				t.Errorf("the lease changed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLeaseLockersCheckTheID(t *testing.T) {
	for _, tc := range lockers {
		t.Run(tc.name, func(t *testing.T) {
			s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
			lk := tc.new(s, layout, time.Now)
			held, other := testLease("held", time.Hour), testLease("other", 2*time.Hour)
			if _, _, err := lk.TryLock(t.Context(), held); err != nil {
				t.Fatalf("TryLock: %v", err)
			}
			if _, err := lk.Renew(t.Context(), other); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Renew of a lease with another ID: error = %v, want ErrLockLost", err)
			}
			if err := lk.Unlock(t.Context(), other); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Unlock of a lease with another ID: error = %v, want ErrLockLost", err)
			}
			if diff := cmp.Diff(&held, holder(t, lk)); diff != "" {
				t.Errorf("the held lease changed (-want +got):\n%s", diff)
			}
			// Nor when the other lease is the one in the store, as after a takeover.
			putLease(t, s, layout, other)
			if _, err := lk.Renew(t.Context(), other); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Renew of the other holder's lease: error = %v, want ErrLockLost", err)
			}
			if err := lk.Unlock(t.Context(), other); !errors.Is(err, statestore.ErrLockLost) {
				t.Errorf("Unlock of the other holder's lease: error = %v, want ErrLockLost", err)
			}
			if diff := cmp.Diff(&other, holder(t, lk)); diff != "" {
				t.Errorf("the other holder's lease changed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLeaseLockersExpiryBoundary pins when a lease expires: at its ExpiresAt, so from that instant on it may be
// taken over.
func TestLeaseLockersExpiryBoundary(t *testing.T) {
	for _, tc := range lockers {
		t.Run(tc.name, func(t *testing.T) {
			s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
			old := testLease("old", time.Hour)
			putLease(t, s, layout, old)
			now := old.ExpiresAt.Add(-time.Nanosecond)
			lk := tc.new(s, layout, func() time.Time { return now })
			_, _, err := lk.TryLock(t.Context(), testLease("mine", 2*time.Hour))
			wantLockedBy(t, err, old.ID)
			now = old.ExpiresAt
			_, previous, err := lk.TryLock(t.Context(), testLease("mine", 2*time.Hour))
			if err != nil || previous == nil || previous.ID != old.ID {
				t.Errorf("TryLock at the expiry: previous %+v, error %v; want %q taken over", previous, err, old.ID)
			}
		})
	}
}

// TestReleaseAfterContextEnds checks that Release unlocks even when the context it gets has ended, as it has after
// Ctrl-C.
func TestReleaseAfterContextEnds(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	lk := statestore.NewLeaseLocker(s, layout, time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	lock, err := statestore.Acquire(ctx, lk, "prod", statestore.AcquireOptions{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	cancel()
	if err := lock.Release(ctx); err != nil {
		t.Errorf("Release with an ended context: %v", err)
	}
	if h := holder(t, lk); h != nil {
		t.Errorf("Holder after Release = %+v, want nil", h)
	}
}

func TestLeaseLockerRetriesWhenReleased(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	creates := 0
	// The lease exists at the create-only put and is gone at the read that follows.
	store := hookedStore{s, func(opts statestore.PutOptions) error {
		if !opts.IfNoneMatch {
			return nil
		}
		creates++
		if creates > 1 {
			return nil
		}
		return fmt.Errorf("put: %w", statestore.ErrPreconditionFailed)
	}}
	lk := statestore.NewLeaseLocker(store, layout, time.Now)
	mine := testLease("mine", time.Hour)
	if _, _, err := lk.TryLock(t.Context(), mine); err != nil {
		t.Fatalf("TryLock after the lock was released meanwhile: %v", err)
	}
	if h := holder(t, lk); h == nil || h.ID != mine.ID {
		t.Errorf("Holder = %+v, want %q", h, mine.ID)
	}
}

func TestLeaseLockerGivesUpWhenReleasedTwice(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	creates := 0
	store := hookedStore{s, func(statestore.PutOptions) error {
		creates++
		return fmt.Errorf("put: %w", statestore.ErrPreconditionFailed)
	}}
	_, _, err := statestore.NewLeaseLocker(store, layout, time.Now).TryLock(t.Context(), testLease("mine", time.Hour))
	var locked *statestore.LockedError
	if !errors.Is(err, statestore.ErrLocked) || errors.As(err, &locked) {
		t.Errorf("error = %v, want ErrLocked without a holder", err)
	}
	if creates != 2 {
		t.Errorf("%d create-only puts, want 2", creates)
	}
}

func TestLeaseLockerLosesTakeover(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	putLease(t, s, layout, testLease("old", -time.Minute))
	rival := testLease("rival", time.Hour)
	// The rival takes over the expired lease just before us.
	store := hookedStore{s, func(opts statestore.PutOptions) error {
		if opts.IfMatch != "" {
			putLease(t, s, layout, rival)
		}
		return nil
	}}
	_, _, err := statestore.NewLeaseLocker(store, layout, time.Now).TryLock(t.Context(), testLease("mine", time.Hour))
	wantLockedBy(t, err, rival.ID)
}

func TestLeaseLockerClockDefault(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	lk := statestore.NewLeaseLocker(s, layout, nil)
	old := testLease("old", -time.Minute)
	putLease(t, s, layout, old)
	_, previous, err := lk.TryLock(t.Context(), testLease("mine", time.Hour))
	if err != nil || previous == nil || previous.ID != old.ID {
		t.Errorf("TryLock over a lease that expired a minute ago: previous %+v, error %v; want it taken over",
			previous, err)
	}
	_, _, err = statestore.NewLeaseLocker(s, layout, nil).TryLock(t.Context(), testLease("late", time.Hour))
	wantLockedBy(t, err, "mine")
}

func TestBestEffortLockerDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
		old := testLease("old", -time.Minute)
		putLease(t, s, layout, old)
		lk := statestore.NewBestEffortLocker(noConditions{s}, layout, nil, 0)
		start := time.Now()
		_, previous, err := lk.TryLock(t.Context(), testLease("mine", time.Hour))
		if err != nil || previous == nil || previous.ID != old.ID {
			t.Errorf("TryLock over a lease that expired a minute ago: previous %+v, error %v; want it taken over",
				previous, err)
		}
		if waited := time.Since(start); waited != 5*time.Second {
			t.Errorf("TryLock waited %v, want 5s", waited)
		}
	})
}

// TestBestEffortLockerContextEnds checks that a TryLock whose context ends while it waits returns at once and removes
// the lease it wrote, so that it does not block others until the lease expires.
func TestBestEffortLockerContextEnds(t *testing.T) {
	rival := testLease("rival", time.Hour)
	broken := errors.New("disk failed")
	for _, tc := range []struct {
		name   string
		store  func(t *testing.T, s statestore.Store, l statestore.Layout) statestore.Store
		holder string // the ID of the lease left behind, or "" for none
		err    error  // wrapped besides the context's error
	}{
		{"own lease removed", func(_ *testing.T, s statestore.Store, _ statestore.Layout) statestore.Store {
			return noConditions{s}
		}, "", nil},
		{"rival's lease kept", func(t *testing.T, s statestore.Store, l statestore.Layout) statestore.Store {
			return afterPutStore{noConditions{s}, func() { putLease(t, s, l, rival) }}
		}, rival.ID, nil},
		{"removal fails", func(_ *testing.T, s statestore.Store, _ statestore.Layout) statestore.Store {
			return failingDelete{noConditions{s}, broken}
		}, "mine", broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
				lk := statestore.NewBestEffortLocker(tc.store(t, s, layout), layout, time.Now, time.Hour)
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				start := time.Now()
				_, _, err := lk.TryLock(ctx, testLease("mine", time.Hour))
				if !errors.Is(err, context.DeadlineExceeded) || tc.err != nil && !errors.Is(err, tc.err) {
					t.Errorf("TryLock error = %v, want context.DeadlineExceeded and %v", err, tc.err)
				}
				if waited := time.Since(start); waited != time.Second {
					t.Errorf("TryLock returned after %v, want 1s, when the context ended", waited)
				}
				switch h := holder(t, lk); {
				case tc.holder == "" && h != nil:
					t.Errorf("the lease %q is left behind", h.ID)
				case tc.holder != "" && (h == nil || h.ID != tc.holder):
					t.Errorf("Holder = %+v, want %q", h, tc.holder)
				}
			})
		})
	}
}

func TestBestEffortLockerLosesWhileSettling(t *testing.T) {
	rival := testLease("rival", time.Hour)
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, s statestore.Store, l statestore.Layout) // runs after our write
		check func(t *testing.T, err error)
	}{
		{"overwritten", func(t *testing.T, s statestore.Store, l statestore.Layout) { putLease(t, s, l, rival) },
			func(t *testing.T, err error) { wantLockedBy(t, err, rival.ID) }},
		{"removed", func(t *testing.T, s statestore.Store, l statestore.Layout) { mustDelete(t, s, l.Lock()) },
			func(t *testing.T, err error) {
				var locked *statestore.LockedError
				if !errors.Is(err, statestore.ErrLocked) || errors.As(err, &locked) {
					t.Errorf("error = %v, want ErrLocked without a holder", err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
			store := afterPutStore{noConditions{s}, func() { tc.after(t, s, layout) }}
			_, _, err := statestore.NewBestEffortLocker(store, layout, time.Now, settle).
				TryLock(t.Context(), testLease("mine", time.Hour))
			tc.check(t, err)
		})
	}
}

// TestTryLockWriteFailedButDone checks a TryLock whose write the store carried out although the write failed, as when
// its answer was lost or its context ended meanwhile. TryLock removes the lease, which would otherwise block everyone
// until it expired, but keeps a lease that another holder wrote meanwhile.
func TestTryLockWriteFailedButDone(t *testing.T) {
	errLost := errors.New("connection reset")
	rival := testLease("rival", time.Hour)
	for _, lkr := range lockers {
		for _, tc := range []struct {
			name   string
			old    bool // an expired lease waits to be taken over
			fail   func(cancel func(), rival func()) error
			want   error  // wrapped by the error of TryLock
			holder string // the ID of the lease left behind, or "" for none
		}{
			{"answer lost", false, func(func(), func()) error { return errLost }, errLost, ""},
			{"answer of a takeover lost", true, func(func(), func()) error { return errLost }, errLost, ""},
			{"context ends", false, func(cancel func(), _ func()) error {
				cancel()
				return context.Canceled
			}, context.Canceled, ""},
			{"rival's lease kept", false, func(_ func(), rival func()) error {
				rival()
				return errLost
			}, errLost, rival.ID},
		} {
			t.Run(lkr.name+"/"+tc.name, func(t *testing.T) {
				s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
				if tc.old {
					putLease(t, s, layout, testLease("old", -time.Minute))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				lk := lkr.new(answerLost{s, func() error {
					return tc.fail(cancel, func() { putLease(t, s, layout, rival) })
				}}, layout, time.Now)
				_, _, err := lk.TryLock(ctx, testLease("mine", time.Hour))
				if !errors.Is(err, tc.want) || errors.Is(err, statestore.ErrLocked) {
					t.Errorf("TryLock error = %v, want %v", err, tc.want)
				}
				switch h := holder(t, lk); {
				case tc.holder == "" && h != nil:
					t.Errorf("the lease %q is left behind", h.ID)
				case tc.holder != "" && (h == nil || h.ID != tc.holder):
					t.Errorf("Holder = %+v, want %q", h, tc.holder)
				}
			})
		}
	}
}

// TestRenewalAnswerLost checks a renewal that the store carried out although it failed, as when its answer was
// lost: the lock is still held, so the next Renew or Unlock succeeds.
func TestRenewalAnswerLost(t *testing.T) {
	errLost := errors.New("connection reset")
	for _, lkr := range lockers {
		for _, next := range []string{"Renew", "Unlock"} {
			t.Run(lkr.name+"/"+next, func(t *testing.T) {
				s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
				puts := 0
				lk := lkr.new(answerLost{s, func() error {
					puts++
					if puts == 2 { // the first renewal's
						return errLost
					}
					return nil
				}}, layout, time.Now)
				held := testLease("mine", time.Hour)
				if _, _, err := lk.TryLock(t.Context(), held); err != nil {
					t.Fatalf("TryLock: %v", err)
				}
				held.ExpiresAt = held.ExpiresAt.Add(time.Hour)
				if _, err := lk.Renew(t.Context(), held); !errors.Is(err, errLost) {
					t.Fatalf("Renew whose answer is lost: error = %v, want %v", err, errLost)
				}
				if next == "Renew" {
					held.ExpiresAt = held.ExpiresAt.Add(time.Hour)
					if _, err := lk.Renew(t.Context(), held); err != nil {
						t.Fatalf("Renew after a renewal whose answer was lost: %v", err)
					}
					if diff := cmp.Diff(&held, holder(t, lk)); diff != "" {
						t.Errorf("Holder (-want +got):\n%s", diff)
					}
				}
				if err := lk.Unlock(t.Context(), held); err != nil {
					t.Fatalf("Unlock after a renewal whose answer was lost: %v", err)
				}
				if h := holder(t, lk); h != nil {
					t.Errorf("Holder after Unlock = %+v, want nil", h)
				}
			})
		}
	}
}

// TestLeaseLockerHeldLeaseSpoiled checks that a held lease that someone replaced with something unreadable is lost.
func TestLeaseLockerHeldLeaseSpoiled(t *testing.T) {
	s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
	lk := statestore.NewLeaseLocker(s, layout, time.Now)
	held := testLease("mine", time.Hour)
	if _, _, err := lk.TryLock(t.Context(), held); err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if _, err := s.Put(t.Context(), layout.Lock(), []byte("not a lease"), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lk.Renew(t.Context(), held); !errors.Is(err, statestore.ErrLockLost) {
		t.Errorf("Renew: error = %v, want ErrLockLost", err)
	}
	if err := lk.Unlock(t.Context(), held); !errors.Is(err, statestore.ErrLockLost) {
		t.Errorf("Unlock: error = %v, want ErrLockLost", err)
	}
}

func TestUnreadableLease(t *testing.T) {
	for _, tc := range lockers {
		t.Run(tc.name, func(t *testing.T) {
			s, layout := openFile(t, t.TempDir()), mustLayout(t, "prod")
			if _, err := s.Put(t.Context(), layout.Lock(), []byte("not a lease"), statestore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			lk := tc.new(s, layout, time.Now)
			_, _, err := lk.TryLock(t.Context(), testLease("mine", time.Hour))
			if errors.Is(err, statestore.ErrLocked) {
				t.Errorf("TryLock over an unreadable lease: error = %v, want a read error", err)
			}
			wantInvalidLease(t, "TryLock", err)
			_, err = lk.Holder(t.Context())
			wantInvalidLease(t, "Holder", err)
			// ForceUnlock removes it anyway, and says it could not read it.
			removed, err := lk.ForceUnlock(t.Context())
			if removed != nil {
				t.Errorf("ForceUnlock of an unreadable lease removed %+v, want nil with the error", removed)
			}
			wantInvalidLease(t, "ForceUnlock", err)
			if h := holder(t, lk); h != nil {
				t.Errorf("Holder after ForceUnlock = %+v, want nil", h)
			}
		})
	}
}

// testLease returns the lease of the holder id that expires after ttl from now; a negative ttl means it expired.
func testLease(id string, ttl time.Duration) statestore.Lease {
	now := time.Now().UTC().Truncate(time.Second)
	return statestore.Lease{ID: id, Owner: "tester", Host: "test-host", PID: 1, AcquiredAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(ttl)}
}

// putLease writes a lease into the store directly, as another holder would.
func putLease(t *testing.T, s statestore.Store, layout statestore.Layout, l statestore.Lease) {
	t.Helper()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), layout.Lock(), data, statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func holder(t *testing.T, lk statestore.Locker) *statestore.Lease {
	t.Helper()
	h, err := lk.Holder(t.Context())
	if err != nil {
		t.Fatalf("Holder: %v", err)
	}
	return h
}

func wantLockedBy(t *testing.T, err error, id string) {
	t.Helper()
	var locked *statestore.LockedError
	if !errors.As(err, &locked) || locked.Holder.ID != id {
		t.Errorf("error = %v, want a *LockedError naming %q", err, id)
	}
}

// hookedStore runs beforePut before each Put. An error from it fails the Put, which then writes nothing.
type hookedStore struct {
	statestore.Store
	beforePut func(opts statestore.PutOptions) error
}

func (s hookedStore) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if err := s.beforePut(opts); err != nil {
		return "", err
	}
	return s.Store.Put(ctx, p, data, opts)
}

// afterPutStore runs afterPut after each successful Put.
type afterPutStore struct {
	statestore.Store
	afterPut func()
}

func (s afterPutStore) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	v, err := s.Store.Put(ctx, p, data, opts)
	if err == nil {
		s.afterPut()
	}
	return v, err
}

// answerLost is a store that carries out each Put and then fails it with the error of fail, if any, as when the
// answer is lost on its way back.
type answerLost struct {
	statestore.Store
	fail func() error
}

func (s answerLost) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	v, err := s.Store.Put(ctx, p, data, opts)
	if err == nil {
		err = s.fail()
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// failingDelete is a store whose Delete fails with err.
type failingDelete struct {
	statestore.Store
	err error
}

func (s failingDelete) Delete(context.Context, string) error { return s.err }

// wantInvalidLease fails the test unless err, of the call op, says that the stored lease cannot be read.
func wantInvalidLease(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, statestore.ErrInvalidLease) {
		t.Errorf("%s: error = %v, want one that matches ErrInvalidLease", op, err)
	}
}
