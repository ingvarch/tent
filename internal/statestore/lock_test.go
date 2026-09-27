package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
)

func TestLockedErrorMessage(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	lease := statestore.Lease{
		ID: "abc", Owner: "igor", Host: "mac", PID: 42, Operation: "update",
		AcquiredAt: time.Date(2026, 9, 26, 14, 0, 1, 0, berlin),
		ExpiresAt:  time.Date(2026, 9, 26, 14, 2, 1, 0, berlin),
	}
	noOp := lease
	noOp.Operation = ""
	for _, tc := range []struct {
		holder statestore.Lease
		want   string
	}{
		{lease, "cluster prod is locked by igor@mac (pid 42) for update since 2026-09-26 12:00:01 UTC"},
		{noOp, "cluster prod is locked by igor@mac (pid 42) since 2026-09-26 12:00:01 UTC"},
		{statestore.Lease{}, "cluster prod is locked by an unknown holder"},
	} {
		var err error = &statestore.LockedError{Cluster: "prod", Holder: tc.holder}
		if got := err.Error(); got != tc.want {
			t.Errorf("Error() = %q\nwant      %q", got, tc.want)
		}
		if !errors.Is(err, statestore.ErrLocked) {
			t.Errorf("errors.Is(%v, ErrLocked) = false", err)
		}
	}
}

func TestLeaseString(t *testing.T) {
	lease := statestore.Lease{
		ID: "abc", Owner: "igor", Host: "mac", PID: 42, Operation: "update",
		AcquiredAt: time.Date(2026, 9, 26, 14, 0, 1, 0, time.FixedZone("CEST", 2*60*60)),
	}
	noOp := lease
	noOp.Operation = ""
	for _, tc := range []struct {
		lease statestore.Lease
		want  string
	}{
		{lease, "igor@mac (pid 42) for update since 2026-09-26 12:00:01 UTC"},
		{noOp, "igor@mac (pid 42) since 2026-09-26 12:00:01 UTC"},
		{statestore.Lease{Operation: "delete"}, "an unknown holder for delete"},
		{statestore.Lease{}, "an unknown holder"},
	} {
		if got := tc.lease.String(); got != tc.want {
			t.Errorf("String() = %q\nwant       %q", got, tc.want)
		}
	}
}

// otherHolder is the lease of someone else who holds the lock in the tests with a fakeLocker.
var otherHolder = statestore.Lease{
	ID: "other", Owner: "igor", Host: "mac", PID: 42, Operation: "update",
	AcquiredAt: time.Date(2026, 9, 26, 12, 0, 1, 0, time.UTC),
	ExpiresAt:  time.Date(2026, 9, 26, 12, 2, 1, 0, time.UTC),
}

func lockedByOther(context.Context, int, statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
	return statestore.Lease{}, nil, &statestore.LockedError{Cluster: "prod", Holder: otherHolder}
}

func TestAcquireDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{tryLock: func(ctx context.Context, n int, l statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			if n < 2 {
				return lockedByOther(ctx, n, l)
			}
			return l, nil, nil
		}}
		start := time.Now()
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{Operation: "update"})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		// Tries come 2s apart, each with a fresh lease of one holder that lasts 2m.
		tries := f.tries()
		if len(tries) != 3 {
			t.Fatalf("%d tries, want 3", len(tries))
		}
		for i, c := range tries {
			at := start.Add(time.Duration(i) * 2 * time.Second)
			if !c.at.Equal(at) || !c.lease.AcquiredAt.Equal(at) || !c.lease.ExpiresAt.Equal(at.Add(2*time.Minute)) {
				t.Errorf("try %d at %v with a lease from %v to %v, want all from %v and 2m later",
					i, c.at, c.lease.AcquiredAt, c.lease.ExpiresAt, at)
			}
			if c.lease.ID != tries[0].lease.ID {
				t.Errorf("try %d has the ID %q, the first had %q", i, c.lease.ID, tries[0].lease.ID)
			}
		}
		got := lock.Lease()
		host, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		at := start.Add(4 * time.Second)
		want := statestore.Lease{
			ID: tries[0].lease.ID, Owner: got.Owner, Host: host, PID: os.Getpid(), Operation: "update",
			AcquiredAt: at, ExpiresAt: at.Add(2 * time.Minute),
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Lease() (-want +got):\n%s", diff)
		}
		if len(got.ID) < 16 || got.Owner == "" {
			t.Errorf("Lease() = %+v, want a random ID and an owner", got)
		}
		if lock.Previous() != nil {
			t.Errorf("Previous() = %+v, want nil", lock.Previous())
		}

		// Renewed every 40s, until Release.
		time.Sleep(85 * time.Second)
		synctest.Wait()
		renews := f.renews()
		if len(renews) != 2 {
			t.Fatalf("%d renewals in 85s, want 2", len(renews))
		}
		for i, c := range renews {
			at := start.Add(4*time.Second + time.Duration(i+1)*40*time.Second)
			if !c.at.Equal(at) || c.lease.ID != want.ID || !c.lease.ExpiresAt.Equal(at.Add(2*time.Minute)) {
				t.Errorf("renewal %d at %v of %q until %v, want at %v until 2m later", i, c.at, c.lease.ID,
					c.lease.ExpiresAt, at)
			}
		}
		if got := lock.Lease(); !got.ExpiresAt.Equal(renews[1].lease.ExpiresAt) {
			t.Errorf("Lease() expires at %v, want the renewed %v", got.ExpiresAt, renews[1].lease.ExpiresAt)
		}
		release(t, lock)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if n := len(f.renews()); n != 2 {
			t.Errorf("%d renewals after Release, want still 2", n)
		}
		if unlocks := f.unlocks(); len(unlocks) != 1 || unlocks[0].lease != lock.Lease() {
			t.Errorf("unlocked %+v, want once, the lease %+v", unlocks, lock.Lease())
		}
	})
}

func TestAcquireOptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{tryLock: func(ctx context.Context, n int, l statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			if n < 1 {
				return lockedByOther(ctx, n, l)
			}
			return l, nil, nil
		}}
		fixed := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
		start := time.Now()
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{
			TTL: 30 * time.Second, Retry: 100 * time.Millisecond, Clock: func() time.Time { return fixed },
		})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer release(t, lock)
		if tries := f.tries(); len(tries) != 2 || tries[1].at.Sub(start) != 100*time.Millisecond {
			t.Errorf("tries %+v, want 2, 100ms apart", tries)
		}
		got := lock.Lease()
		if !got.AcquiredAt.Equal(fixed) || !got.ExpiresAt.Equal(fixed.Add(30*time.Second)) {
			t.Errorf("Lease() from %v to %v, want from %v for 30s", got.AcquiredAt, got.ExpiresAt, fixed)
		}
		time.Sleep(25 * time.Second) // renewed at 10s and 20s
		synctest.Wait()
		if n := len(f.renews()); n != 2 {
			t.Errorf("%d renewals in 25s with a TTL of 30s, want 2", n)
		}
	})
}

func TestAcquireLeaseIDsDiffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ids := map[string]bool{}
		for range 3 {
			lock, err := statestore.Acquire(t.Context(), &fakeLocker{}, "prod", statestore.AcquireOptions{})
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			ids[lock.Lease().ID] = true
			release(t, lock)
		}
		if len(ids) != 3 {
			t.Errorf("three Acquires used the IDs %v, want three different", ids)
		}
	})
}

func TestAcquirePrevious(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{tryLock: func(_ context.Context, _ int, l statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			previous := otherHolder
			return l, &previous, nil
		}}
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer release(t, lock)
		if got := lock.Previous(); got == nil || *got != otherHolder {
			t.Errorf("Previous() = %+v, want %+v", got, otherHolder)
		}
	})
}

func TestAcquireContextDoneBeforeFirstTry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := &fakeLocker{}
	_, err := statestore.Acquire(ctx, f, "prod", statestore.AcquireOptions{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, statestore.ErrLocked) {
		t.Errorf("Acquire with a canceled context: error = %v, want context.Canceled only", err)
	}
	if n := len(f.tries()); n != 0 {
		t.Errorf("%d tries with a canceled context, want none", n)
	}
}

func TestAcquireGivesUp(t *testing.T) {
	broken := errors.New("disk failed")
	// thenFail finds the lock held by otherHolder, then fails once ctx ends.
	thenFail := func(fail func(ctx context.Context) error) func(context.Context, int, statestore.Lease) (
		statestore.Lease, *statestore.Lease, error,
	) {
		return func(ctx context.Context, n int, l statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
			if n == 0 {
				return lockedByOther(ctx, n, l)
			}
			<-ctx.Done()
			return statestore.Lease{}, nil, fail(ctx)
		}
	}
	for _, tc := range []struct {
		name    string
		tryLock func(ctx context.Context, n int, l statestore.Lease) (statestore.Lease, *statestore.Lease, error)
		also    error // wrapped besides the context's error
	}{
		{"between tries", lockedByOther, nil},
		{"during a try", thenFail(func(ctx context.Context) error {
			return fmt.Errorf("lock cluster prod: %w", ctx.Err())
		}), nil},
		{"during a try that fails more", thenFail(func(ctx context.Context) error {
			return fmt.Errorf("lock cluster prod: %w; remove the lease: %w", ctx.Err(), broken)
		}), broken},
		{"during a try that fails otherwise", thenFail(func(context.Context) error { return broken }), broken},
		// The error names the last holder seen, not a later change of hands.
		{"after the lock changed hands", func(ctx context.Context, n int, l statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			if n == 0 {
				return lockedByOther(ctx, n, l)
			}
			return statestore.Lease{}, nil, errChangedHands
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				_, err := statestore.Acquire(ctx, &fakeLocker{tryLock: tc.tryLock}, "prod",
					statestore.AcquireOptions{})
				wantGaveUp(t, err, otherHolder, context.DeadlineExceeded)
				if tc.also != nil && !errors.Is(err, tc.also) {
					t.Errorf("error = %v, want it to wrap %v", err, tc.also)
				}
			})
		})
	}
}

// errChangedHands is the error of a TryLock that saw the lock change hands and cannot name the holder.
var errChangedHands = fmt.Errorf("lock cluster prod: the lock changed hands meanwhile: %w", statestore.ErrLocked)

func TestAcquireRetriesWhenTheLockChangesHands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{tryLock: func(_ context.Context, n int, l statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			if n == 0 {
				return statestore.Lease{}, nil, errChangedHands
			}
			return l, nil, nil
		}}
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		release(t, lock)
		if n := len(f.tries()); n != 2 {
			t.Errorf("%d tries, want 2", n)
		}
	})
}

// wantGaveUp checks the error of an Acquire whose context ended while holder had the lock.
func wantGaveUp(t *testing.T, err error, holder statestore.Lease, ctxErr error) {
	t.Helper()
	var locked *statestore.LockedError
	if !errors.As(err, &locked) || !errors.Is(err, statestore.ErrLocked) || !errors.Is(err, ctxErr) {
		t.Fatalf("error = %v, want a *LockedError that also wraps %v", err, ctxErr)
	}
	if locked.Holder.ID != holder.ID {
		t.Errorf("the error names the holder %q, want %q", locked.Holder.ID, holder.ID)
	}
	if msg := err.Error(); !strings.Contains(msg, "locked by") || !strings.Contains(msg, ctxErr.Error()) {
		t.Errorf("error %q does not say who holds the lock and why waiting stopped", msg)
	}
}

func TestAcquireFailsOnOtherErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down := errors.New("store down")
		f := &fakeLocker{tryLock: func(context.Context, int, statestore.Lease) (
			statestore.Lease, *statestore.Lease, error,
		) {
			return statestore.Lease{}, nil, down
		}}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err := statestore.Acquire(ctx, f, "prod", statestore.AcquireOptions{})
		if !errors.Is(err, down) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want %v at once", err, down)
		}
		if n := len(f.tries()); n != 1 {
			t.Errorf("%d tries, want 1", n)
		}
	})
}

func TestLockLost(t *testing.T) {
	transient := errors.New("store unreachable")
	lost := fmt.Errorf("renew the lock of cluster prod: %w", statestore.ErrLockLost)
	for _, tc := range []struct {
		name    string
		results []error // of the renewals, one every 10s
		lostAt  int     // the renewal after which Lost() is closed
	}{
		{"lock lost", []error{nil, lost}, 2},
		// A transient failure loses the lock only when the lease would expire before the next renewal.
		{"transient errors", []error{transient, nil, transient, transient}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeLocker{renew: func(_ context.Context, n int, l statestore.Lease) (statestore.Lease, error) {
					return l, tc.results[n]
				}}
				opts := statestore.AcquireOptions{TTL: 30 * time.Second}
				lock, err := statestore.Acquire(t.Context(), f, "prod", opts)
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				time.Sleep(5 * time.Second) // check between renewals
				for n := 1; n <= len(tc.results); n++ {
					time.Sleep(10 * time.Second)
					synctest.Wait()
					if got, want := isClosed(lock.Lost()), n >= tc.lostAt; got != want {
						t.Fatalf("after renewal %d: Lost() closed = %v, want %v", n, got, want)
					}
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if n := len(f.renews()); n != tc.lostAt {
					t.Errorf("%d renewals, want renewals to stop after %d", n, tc.lostAt)
				}
				_ = lock.Release(t.Context()) // the fake unlocks anyway
			})
		})
	}
}

func TestLockReleaseWaitsForRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proceed := make(chan struct{})
		f := &fakeLocker{renew: func(_ context.Context, _ int, l statestore.Lease) (statestore.Lease, error) {
			<-proceed
			return l, nil
		}}
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{TTL: 30 * time.Second})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		time.Sleep(15 * time.Second) // a renewal started at 10s and waits
		synctest.Wait()
		released := make(chan error)
		go func() { released <- lock.Release(t.Context()) }()
		synctest.Wait()
		if n := len(f.unlocks()); n != 0 {
			t.Fatalf("Release unlocked while a renewal ran")
		}
		close(proceed)
		if err := <-released; err != nil {
			t.Fatalf("Release: %v", err)
		}
		renews, unlocks := f.renews(), f.unlocks()
		if len(renews) != 1 || len(unlocks) != 1 || unlocks[0].lease != renews[0].lease {
			t.Errorf("renewed %+v, then unlocked %+v, want the renewed lease unlocked once", renews, unlocks)
		}
	})
}

func TestLockReleaseTwice(t *testing.T) {
	failed := errors.New("store down")
	for _, unlockErr := range []error{nil, failed} {
		t.Run(fmt.Sprint(unlockErr), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeLocker{unlock: func(context.Context, statestore.Lease) error { return unlockErr }}
				lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{})
				if err != nil {
					t.Fatalf("Acquire: %v", err)
				}
				for i := range 2 {
					if err := lock.Release(t.Context()); !errors.Is(err, unlockErr) {
						t.Errorf("Release %d: error = %v, want %v", i+1, err, unlockErr)
					}
				}
				if n := len(f.unlocks()); n != 1 {
					t.Errorf("%d unlocks, want 1", n)
				}
			})
		})
	}
}

// hangingRenew is a renewal whose store never answers: it returns only when ctx ends.
func hangingRenew(ctx context.Context, _ int, _ statestore.Lease) (statestore.Lease, error) {
	<-ctx.Done()
	return statestore.Lease{}, ctx.Err()
}

func TestLockLostWhenRenewalsHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{renew: hangingRenew}
		start := time.Now()
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{TTL: 30 * time.Second})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		select {
		case <-lock.Lost():
		case <-time.After(time.Minute):
			t.Fatal("Lost() did not close while renewals hung")
		}
		if lostAfter := time.Since(start); lostAfter >= 30*time.Second {
			t.Errorf("Lost() closed after %v, not before the lease expired after 30s", lostAfter)
		}
		_ = lock.Release(t.Context()) // the fake unlocks anyway
	})
}

func TestLockReleaseDuringHungRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{renew: hangingRenew}
		lock, err := statestore.Acquire(t.Context(), f, "prod", statestore.AcquireOptions{TTL: 30 * time.Second})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		time.Sleep(15 * time.Second) // a renewal started at 10s and hangs
		start := time.Now()
		release(t, lock)
		if waited := time.Since(start); waited > 10*time.Second {
			t.Errorf("Release waited %v for the renewal, want at most its 10s interval", waited)
		}
		if n := len(f.unlocks()); n != 1 {
			t.Errorf("%d unlocks, want 1", n)
		}
	})
}

func TestLockReleaseAfterContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The store never answers the unlock: Release gives up after 5s.
		f := &fakeLocker{unlock: func(ctx context.Context, _ statestore.Lease) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		ctx, cancel := context.WithCancel(t.Context())
		lock, err := statestore.Acquire(ctx, f, "prod", statestore.AcquireOptions{})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		cancel()
		start := time.Now()
		if err := lock.Release(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Release error = %v, want context.DeadlineExceeded", err)
		}
		if waited := time.Since(start); waited != 5*time.Second {
			t.Errorf("Release gave up after %v, want 5s", waited)
		}
	})
}

func TestLockRenewalsOutliveAcquireContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeLocker{renew: func(ctx context.Context, _ int, l statestore.Lease) (statestore.Lease, error) {
			return l, ctx.Err()
		}}
		ctx, cancel := context.WithCancel(t.Context())
		lock, err := statestore.Acquire(ctx, f, "prod", statestore.AcquireOptions{TTL: 30 * time.Second})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		cancel()
		time.Sleep(65 * time.Second) // renewed every 10s
		synctest.Wait()
		renews := f.renews()
		if isClosed(lock.Lost()) || len(renews) != 6 {
			t.Fatalf("Lost() closed = %v after %d renewals, want open after 6", isClosed(lock.Lost()), len(renews))
		}
		if got, want := lock.Lease().ExpiresAt, renews[5].lease.ExpiresAt; !got.Equal(want) {
			t.Errorf("Lease() expires at %v, want the last renewal's %v", got, want)
		}
		release(t, lock)
	})
}

// TestLockKeepsTheLockersLease checks that the lock holds the lease the locker returned, which may differ from the
// one it was given: a store may keep times to the second only.
func TestLockKeepsTheLockersLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		toSecond := func(l statestore.Lease) statestore.Lease {
			l.AcquiredAt, l.ExpiresAt = l.AcquiredAt.Truncate(time.Second), l.ExpiresAt.Truncate(time.Second)
			return l
		}
		f := &fakeLocker{
			tryLock: func(_ context.Context, _ int, l statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
				return toSecond(l), nil, nil
			},
			renew: func(_ context.Context, _ int, l statestore.Lease) (statestore.Lease, error) {
				return toSecond(l), nil
			},
		}
		opts := statestore.AcquireOptions{
			TTL: 30 * time.Second, Clock: func() time.Time { return time.Now().Add(250 * time.Millisecond) },
		}
		lock, err := statestore.Acquire(t.Context(), f, "prod", opts)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer release(t, lock)
		if diff := cmp.Diff(toSecond(f.tries()[0].lease), lock.Lease()); diff != "" {
			t.Errorf("Lease() after TryLock (-want +got):\n%s", diff)
		}
		time.Sleep(15 * time.Second) // renewed at 10s
		synctest.Wait()
		if diff := cmp.Diff(toSecond(f.renews()[0].lease), lock.Lease()); diff != "" {
			t.Errorf("Lease() after Renew (-want +got):\n%s", diff)
		}
	})
}

func TestOwnerName(t *testing.T) {
	failed := func() (*user.User, error) { return nil, errors.New("no such user") }
	named := func(name string) func() (*user.User, error) {
		return func() (*user.User, error) { return &user.User{Username: name}, nil }
	}
	for _, tc := range []struct {
		name    string
		current func() (*user.User, error)
		env     map[string]string
		want    string
	}{
		{"OS user", named("igor"), map[string]string{"USER": "env"}, "igor"},
		{"USER", failed, map[string]string{"USER": "u", "USERNAME": "un"}, "u"},
		{"empty OS user name", named(""), map[string]string{"USER": "u"}, "u"},
		{"USERNAME", failed, map[string]string{"USERNAME": "un"}, "un"},
		{"unknown", failed, nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statestore.OwnerName(tc.current, func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("owner = %q, want %q", got, tc.want)
			}
		})
	}
}

func release(t *testing.T, lock *statestore.Lock) {
	t.Helper()
	if err := lock.Release(t.Context()); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// fakeLocker is a scripted Locker that records its calls. Without a script, every call succeeds.
type fakeLocker struct {
	tryLock func(ctx context.Context, n int, l statestore.Lease) (statestore.Lease, *statestore.Lease, error)
	renew   func(ctx context.Context, n int, l statestore.Lease) (statestore.Lease, error) // n counts from 0
	unlock  func(ctx context.Context, l statestore.Lease) error

	mu                    sync.Mutex
	tried, renewed, freed []call
}

// call is one recorded call: when it came and with which lease.
type call struct {
	at    time.Time
	lease statestore.Lease
}

// record appends a call to calls and returns its index.
func (f *fakeLocker) record(calls *[]call, l statestore.Lease) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	*calls = append(*calls, call{time.Now(), l})
	return len(*calls) - 1
}

func (f *fakeLocker) TryLock(ctx context.Context, l statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
	n := f.record(&f.tried, l)
	if f.tryLock == nil {
		return l, nil, nil
	}
	return f.tryLock(ctx, n, l)
}

func (f *fakeLocker) Renew(ctx context.Context, l statestore.Lease) (statestore.Lease, error) {
	n := f.record(&f.renewed, l)
	if f.renew == nil {
		return l, nil
	}
	return f.renew(ctx, n, l)
}

func (f *fakeLocker) Unlock(ctx context.Context, l statestore.Lease) error {
	f.record(&f.freed, l)
	if f.unlock == nil {
		return nil
	}
	return f.unlock(ctx, l)
}

func (f *fakeLocker) Holder(context.Context) (*statestore.Lease, error) {
	return nil, errors.ErrUnsupported
}

func (f *fakeLocker) ForceUnlock(context.Context) (*statestore.Lease, error) {
	return nil, errors.ErrUnsupported
}

func (f *fakeLocker) tries() []call   { return f.calls(&f.tried) }
func (f *fakeLocker) renews() []call  { return f.calls(&f.renewed) }
func (f *fakeLocker) unlocks() []call { return f.calls(&f.freed) }

func (f *fakeLocker) calls(calls *[]call) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), *calls...)
}
