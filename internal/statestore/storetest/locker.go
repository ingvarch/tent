package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
)

// LockerSetup prepares one subtest of RunLocker. It returns the cluster that the lockers lock and a function that
// returns a new locker of that cluster on every call, independent of the others as if it ran in another process.
// Lockers whose leases expire must take the time from now.
type LockerSetup func(t *testing.T, now func() time.Time) (cluster string, newLocker func() statestore.Locker)

// LockerOptions describe the mechanism that RunLocker tests.
type LockerOptions struct {
	// ProcessBound means the OS holds the lock for the holder's process, as with flock: the lock does not expire,
	// and ForceUnlock cannot break it while the holder runs. The subtests that need either are skipped.
	ProcessBound bool
	// Weak means Renew checks the lease and writes it in two steps, as the best-effort lease does, so it may undo a
	// ForceUnlock that comes between them.
	Weak bool
	// TTL is the lease lifetime in the subtests where Acquire renews the lease in the background: every third of the
	// TTL, and each renewal gets that long. A renewal must have room to wait for the store's own lock several times
	// and to sync to disk, or on a slow machine it misses its deadline every time. Zero means 1s.
	TTL time.Duration
	// Retry is Acquire's wait between tries. Where a store takes one write per second to a key, it must be longer
	// than a second, or a waiting Acquire takes every write and the holder cannot unlock. Zero means 1ms.
	Retry time.Duration
}

// ttl returns the lease lifetime for the subtests that renew in the background.
func (o LockerOptions) ttl() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return time.Second
}

// retry returns Acquire's wait between tries.
func (o LockerOptions) retry() time.Duration {
	if o.Retry > 0 {
		return o.Retry
	}
	return time.Millisecond
}

// waitLimit bounds every wait for something that must happen soon.
const waitLimit = 10 * time.Second

// RunLocker runs the conformance suite that every statestore.Locker must pass.
func RunLocker(t *testing.T, setup LockerSetup, opts LockerOptions) {
	tests := []struct {
		name      string
		fn        func(t *testing.T, e lockerEnv)
		breakable bool // needs a lock that expires or that ForceUnlock breaks
	}{
		{"Exclusive", testExclusive, false},
		{"UnlockThenAnother", testUnlockThenAnother, false},
		{"Holder", testHolder, false},
		{"ExpiredTakeover", testExpiredTakeover, true},
		{"RenewalExtends", testRenewalExtends, false},
		{"ForceUnlockFree", testForceUnlockFree, false},
		{"ForceUnlockHeld", testForceUnlockHeld, true},
		{"AcquireSerializes", testAcquireSerializes, false},
		{"AcquireRenews", testAcquireRenews, false},
		{"AcquireGivesUp", testAcquireGivesUp, false},
		{"LostOnForceUnlock", testLostOnForceUnlock, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.breakable && opts.ProcessBound {
				t.Skip("the lock is bound to its holder's process")
			}
			clock := &fakeClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
			cluster, newLocker := setup(t, clock.Now)
			tt.fn(t, lockerEnv{cluster: cluster, newLocker: newLocker, clock: clock, opts: opts})
		})
	}
}

func testExclusive(t *testing.T, e lockerEnv) {
	a, b := e.newLocker(), e.newLocker()
	first := e.lease("first")
	tryLock(t, a, first)
	_, _, err := b.TryLock(t.Context(), e.lease("second"))
	wantLocked(t, err, e.cluster, first)
	wantHolder(t, b, &first)
}

func testUnlockThenAnother(t *testing.T, e lockerEnv) {
	a, b := e.newLocker(), e.newLocker()
	first := e.lease("first")
	tryLock(t, a, first)
	if err := a.Unlock(t.Context(), first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	second := e.lease("second")
	tryLock(t, b, second)
	wantHolder(t, a, &second)
}

func testHolder(t *testing.T, e lockerEnv) {
	a, b := e.newLocker(), e.newLocker()
	wantHolder(t, b, nil)
	first := e.lease("first")
	tryLock(t, a, first)
	wantHolder(t, a, &first)
	wantHolder(t, b, &first)
	if err := a.Unlock(t.Context(), first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantHolder(t, b, nil)
}

func testExpiredTakeover(t *testing.T, e lockerEnv) {
	a, b := e.newLocker(), e.newLocker()
	first := e.lease("first") // expires in a minute
	tryLock(t, a, first)
	e.clock.Advance(59 * time.Second)
	_, _, err := b.TryLock(t.Context(), e.lease("second"))
	wantLocked(t, err, e.cluster, first)

	e.clock.Advance(2 * time.Second)
	second := e.lease("second")
	held, previous, err := b.TryLock(t.Context(), second)
	if err != nil {
		t.Fatalf("TryLock of an expired lock: %v", err)
	}
	if diff := cmp.Diff(second, held); diff != "" {
		t.Errorf("TryLock held (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&first, previous); diff != "" {
		t.Errorf("TryLock previous (-want +got):\n%s", diff)
	}
	wantHolder(t, a, &second)
	_, err = a.Renew(t.Context(), e.renewed(first))
	wantLost(t, err, "Renew of a lease taken over")
	wantLost(t, a.Unlock(t.Context(), first), "Unlock of a lease taken over")
	wantHolder(t, a, &second)
}

func testRenewalExtends(t *testing.T, e lockerEnv) {
	a, b := e.newLocker(), e.newLocker()
	first := e.lease("first") // expires in a minute
	tryLock(t, a, first)
	e.clock.Advance(50 * time.Second)
	renewed := e.renewed(first)
	got, err := a.Renew(t.Context(), renewed)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if diff := cmp.Diff(renewed, got); diff != "" {
		t.Errorf("Renew (-want +got):\n%s", diff)
	}
	wantHolder(t, b, &renewed)

	e.clock.Advance(20 * time.Second) // past the first expiry
	_, _, err = b.TryLock(t.Context(), e.lease("second"))
	wantLocked(t, err, e.cluster, renewed)
	// The locker follows its own renewals.
	again := e.renewed(renewed)
	if _, err := a.Renew(t.Context(), again); err != nil {
		t.Fatalf("second Renew: %v", err)
	}
	if err := a.Unlock(t.Context(), again); err != nil {
		t.Fatalf("Unlock after Renew: %v", err)
	}
	wantHolder(t, b, nil)
}

func testForceUnlockFree(t *testing.T, e lockerEnv) {
	a := e.newLocker()
	wantForceUnlock(t, a, nil)
	first := e.lease("first")
	tryLock(t, a, first)
	if err := a.Unlock(t.Context(), first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantForceUnlock(t, e.newLocker(), nil)
}

func testForceUnlockHeld(t *testing.T, e lockerEnv) {
	a, b, c := e.newLocker(), e.newLocker(), e.newLocker()
	first := e.lease("first")
	tryLock(t, a, first)
	wantForceUnlock(t, b, &first)
	wantHolder(t, b, nil)
	_, err := a.Renew(t.Context(), e.renewed(first))
	wantLost(t, err, "Renew of a removed lease")

	second := e.lease("second")
	tryLock(t, c, second)
	wantLost(t, a.Unlock(t.Context(), first), "Unlock of a removed lease")
	wantHolder(t, b, &second)
}

func testAcquireSerializes(t *testing.T, e lockerEnv) {
	first, err := statestore.Acquire(t.Context(), e.newLocker(), e.cluster, e.options())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	type result struct {
		lock *statestore.Lock
		err  error
	}
	waiting := make(chan struct{}, 1)
	done := make(chan result, 1)
	b := watched{e.newLocker(), func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}}
	go func() {
		lock, err := statestore.Acquire(t.Context(), b, e.cluster, e.options())
		done <- result{lock, err}
	}()
	// The second Acquire keeps finding the lock taken while the first holds it.
	for range 3 {
		select {
		case <-waiting:
		case r := <-done:
			t.Fatalf("the second Acquire returned while the first held the lock: %v", r.err)
		case <-time.After(waitLimit):
			t.Fatalf("the second Acquire did not try the lock within %v", waitLimit)
		}
	}
	// A clean Release shows the first lease was still in place.
	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	r := receive(t, done, "the second Acquire returns after Release")
	if r.err != nil {
		t.Fatalf("second Acquire: %v", r.err)
	}
	second := r.lock.Lease()
	wantHolder(t, b, &second)
	if err := first.Release(t.Context()); err != nil {
		t.Errorf("second Release of the first lock: %v", err)
	}
	if err := r.lock.Release(t.Context()); err != nil {
		t.Errorf("Release of the second lock: %v", err)
	}
	wantHolder(t, b, nil)
}

func testAcquireRenews(t *testing.T, e lockerEnv) {
	b := e.newLocker()
	opts := e.options()
	opts.TTL = e.opts.ttl()
	lock, err := statestore.Acquire(t.Context(), e.newLocker(), e.cluster, opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	e.clock.Advance(time.Hour)
	want := e.clock.Now().Add(opts.TTL)
	deadline := time.Now().Add(waitLimit)
	for {
		h := holder(t, b)
		if h != nil && h.ExpiresAt.Equal(want) && lock.Lease().ExpiresAt.Equal(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lease was not renewed within %v: holder %+v, lock %+v", waitLimit, h, lock.Lease())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := lock.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func testAcquireGivesUp(t *testing.T, e lockerEnv) {
	a := e.newLocker()
	first := e.lease("first")
	tryLock(t, a, first)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := statestore.Acquire(ctx, watched{e.newLocker(), cancel}, e.cluster, e.options())
	wantLocked(t, err, e.cluster, first)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Acquire error = %v, want it to wrap context.Canceled", err)
	}
	wantHolder(t, a, &first)
}

func testLostOnForceUnlock(t *testing.T, e lockerEnv) {
	opts := e.options()
	opts.TTL = e.opts.ttl()
	lock, err := statestore.Acquire(t.Context(), e.newLocker(), e.cluster, opts)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	held, b := lock.Lease(), e.newLocker()
	wantForceUnlock(t, b, &held)
	deadline := time.After(waitLimit)
	for {
		select {
		case <-lock.Lost():
			wantLost(t, lock.Release(t.Context()), "Release of a lost lock")
			return
		case <-deadline:
			t.Fatalf("Lost() did not close within %v of ForceUnlock", waitLimit)
		case <-time.After(100 * time.Millisecond):
			if !e.opts.Weak {
				continue
			}
			// A renewal that read the lease before ForceUnlock wrote it back.
			if _, err := b.ForceUnlock(t.Context()); err != nil {
				t.Fatalf("ForceUnlock: %v", err)
			}
		}
	}
}

// lockerEnv is what one subtest of RunLocker works with.
type lockerEnv struct {
	cluster   string
	newLocker func() statestore.Locker
	clock     *fakeClock
	opts      LockerOptions
}

// lease returns the lease of the holder id, from now for a minute.
func (e lockerEnv) lease(id string) statestore.Lease {
	now := e.clock.Now()
	return statestore.Lease{
		ID: id, Owner: "tester", Host: "test-host", PID: 1, Operation: "update",
		AcquiredAt: now, ExpiresAt: now.Add(time.Minute),
	}
}

// renewed returns l renewed now for a minute.
func (e lockerEnv) renewed(l statestore.Lease) statestore.Lease {
	l.ExpiresAt = e.clock.Now().Add(time.Minute)
	return l
}

// options returns AcquireOptions for the suite: its clock, and its wait between tries.
func (e lockerEnv) options() statestore.AcquireOptions {
	return statestore.AcquireOptions{Operation: "update", Retry: e.opts.retry(), Clock: e.clock.Now}
}

// fakeClock is a clock that moves only when told. All lockers of a subtest share it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// watched is a locker that calls onLocked each time TryLock finds the lock taken.
type watched struct {
	statestore.Locker
	onLocked func()
}

func (w watched) TryLock(ctx context.Context, l statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
	held, previous, err := w.Locker.TryLock(ctx, l)
	if errors.Is(err, statestore.ErrLocked) {
		w.onLocked()
	}
	return held, previous, err
}

// tryLock takes a free lock for l.
func tryLock(t *testing.T, lk statestore.Locker, l statestore.Lease) {
	t.Helper()
	held, previous, err := lk.TryLock(t.Context(), l)
	if err != nil {
		t.Fatalf("TryLock(%q): %v", l.ID, err)
	}
	if diff := cmp.Diff(l, held); diff != "" {
		t.Errorf("TryLock(%q) held (-want +got):\n%s", l.ID, diff)
	}
	if previous != nil {
		t.Errorf("TryLock(%q) of a free lock took over %+v", l.ID, previous)
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

func wantHolder(t *testing.T, lk statestore.Locker, want *statestore.Lease) {
	t.Helper()
	if diff := cmp.Diff(want, holder(t, lk)); diff != "" {
		t.Errorf("Holder (-want +got):\n%s", diff)
	}
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

// wantLocked checks that err is a *LockedError that names the cluster and the holder.
func wantLocked(t *testing.T, err error, cluster string, holder statestore.Lease) {
	t.Helper()
	var locked *statestore.LockedError
	if !errors.As(err, &locked) || !errors.Is(err, statestore.ErrLocked) {
		t.Fatalf("error = %v, want a *LockedError", err)
	}
	if locked.Cluster != cluster || locked.Holder.ID != holder.ID {
		t.Errorf("the lock of %q is held by %q, want %q held by %q", locked.Cluster, locked.Holder.ID, cluster,
			holder.ID)
	}
}

func wantLost(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, statestore.ErrLockLost) {
		t.Errorf("%s: error = %v, want ErrLockLost", what, err)
	}
}

// receive returns the next value from ch, or fails the test after waitLimit.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLimit):
		t.Fatalf("%s: not within %v", what, waitLimit)
		var zero T
		return zero
	}
}
