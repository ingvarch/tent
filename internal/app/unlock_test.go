package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/statestore"
)

// otherLease is the lease of another tent that expires at expires.
func otherLease(expires time.Time) statestore.Lease {
	return statestore.Lease{
		ID: "other", Owner: "ops", Host: "ci", PID: 42, Operation: "update",
		AcquiredAt: expires.Add(-2 * time.Minute), ExpiresAt: expires,
	}
}

func wantUnlocked(t *testing.T, got *statestore.Lease, err error, want *statestore.Lease) {
	t.Helper()
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Unlock removed (-want +got):\n%s", diff)
	}
}

func TestUnlockNotLocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(statestore.Store) statestore.Store
	}{
		{"flock", func(s statestore.Store) statestore.Store { return s }},
		{"lease", func(s statestore.Store) statestore.Store { return unwrapped{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML)
			svc.Store = tc.wrap(svc.Store)
			stored := snapshot(t, svc.Store)
			for _, force := range []bool{false, true} {
				removed, err := svc.Unlock(t.Context(), "prod", force)
				wantUnlocked(t, removed, err, nil)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

// TestUnlockMissingCluster reports a cluster with no state and no lock as missing, rather than as not locked.
func TestUnlockMissingCluster(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(statestore.Store) statestore.Store
	}{
		{"flock", func(s statestore.Store) statestore.Store { return s }},
		{"lease", func(s statestore.Store) statestore.Store { return unwrapped{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML)
			svc.Store = tc.wrap(svc.Store)
			stored := snapshot(t, svc.Store)
			for _, force := range []bool{false, true} {
				removed, err := svc.Unlock(t.Context(), "dev", force)
				wantError(t, err, notFound(svc, "cluster dev"))
				if removed != nil {
					t.Errorf("Unlock(force=%t) removed %+v", force, removed)
				}
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

// stopHolder ends the error for a flock that a running tent holds.
const stopHolder = "; that tent is still running and holds a file lock that only it can release: " +
	"stop that process first"

func TestUnlockRunningFlockHolder(t *testing.T) {
	svc, _ := newService(t)
	held := holdLock(t, svc.Store)
	for _, force := range []bool{false, true} {
		removed, err := svc.Unlock(t.Context(), "prod", force)
		wantError(t, err, lockedBy(held.Lease())+stopHolder)
		if removed != nil || !errors.Is(err, statestore.ErrLocked) {
			t.Errorf("Unlock(force=%t) = %+v, %v; want nil and an error that matches ErrLocked", force, removed, err)
		}
	}
	release(t, held)
}

// TestUnlockDeadFlockHolder removes the lease that a tent left when it died holding the flock, which the OS released.
func TestUnlockDeadFlockHolder(t *testing.T) {
	svc, root := newService(t)
	dead := leaveFlockLease(t, root)
	removed, err := svc.Unlock(t.Context(), "prod", false)
	wantUnlocked(t, removed, err, &dead)
	if _, err := os.Stat(flockLease(root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease is left: %v", err)
	}
}

// leaseService returns a service whose store locks with a lease object, holding lease.
func leaseService(t *testing.T, lease statestore.Lease) *app.Service {
	t.Helper()
	svc, _ := newService(t)
	svc.Store = unwrapped{svc.Store}
	put(t, svc.Store, lockPath, leaseJSON(t, lease))
	return svc
}

func TestUnlockLiveLease(t *testing.T) {
	live := otherLease(time.Now().Add(time.Hour).UTC())
	svc := leaseService(t, live)
	removed, err := svc.Unlock(t.Context(), "prod", false)
	wantError(t, err, lockedBy(live)+"; run it again with --force if that tent is gone")
	if removed != nil || !errors.Is(err, statestore.ErrLocked) {
		t.Errorf("Unlock = %+v, %v; want nil and an error that matches ErrLocked", removed, err)
	}
	wantPaths(t, svc.Store, lockPath)

	removed, err = svc.Unlock(t.Context(), "prod", true)
	wantUnlocked(t, removed, err, &live)
	wantPaths(t, svc.Store)
}

func TestUnlockExpiredLease(t *testing.T) {
	expired := otherLease(time.Now().Add(-time.Minute).UTC())
	svc := leaseService(t, expired)
	removed, err := svc.Unlock(t.Context(), "prod", false)
	wantUnlocked(t, removed, err, &expired)
	wantPaths(t, svc.Store)
}

// swapping gives out the lease it holds on the first read of the lock, then puts live in its place, as a tent that
// takes over the expired lease right after that read would.
type swapping struct {
	statestore.Store
	live    []byte
	swapped bool
}

func (s *swapping) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	data, v, err := s.Store.Get(ctx, p)
	if p == lockPath && err == nil && !s.swapped {
		s.swapped = true
		if _, err := s.Put(ctx, p, s.live, statestore.PutOptions{}); err != nil {
			return nil, "", err
		}
	}
	return data, v, err
}

func TestUnlockExpiredLeaseTakenMeanwhile(t *testing.T) {
	expired := otherLease(time.Now().Add(-time.Minute).UTC())
	live := otherLease(time.Now().Add(time.Hour).UTC())
	live.ID, live.Host = "taker", "laptop"
	svc := leaseService(t, expired)
	svc.Store = &swapping{Store: svc.Store, live: leaseJSON(t, live)}
	removed, err := svc.Unlock(t.Context(), "prod", false)
	wantError(t, err, lockedBy(live)+"; run it again with --force if that tent is gone")
	if removed != nil {
		t.Errorf("Unlock removed %+v", removed)
	}
	wantStored(t, svc.Store, lockPath, leaseJSON(t, live))
}

func TestUnlockChecksTheTentVersion(t *testing.T) {
	expired := otherLease(time.Now().Add(-time.Minute).UTC())
	svc := leaseService(t, expired)
	put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
	svc.Version = "v0.4.0"
	for _, force := range []bool{false, true} {
		_, err := svc.Unlock(t.Context(), "prod", force)
		wantError(t, err, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
		if !errors.Is(err, statestore.ErrTentTooOld) {
			t.Errorf("errors.Is(%v, ErrTentTooOld) = false", err)
		}
	}
	wantStored(t, svc.Store, lockPath, leaseJSON(t, expired))
}

// failingPuts fails every Put, as a store that cannot be reached would.
type failingPuts struct{ statestore.Store }

func (failingPuts) Put(_ context.Context, p string, _ []byte, _ statestore.PutOptions) (statestore.Version, error) {
	return "", fmt.Errorf("put %q: connection refused", p)
}

func TestUnlockUnreadableLease(t *testing.T) {
	const hint = "; run it again with --force if that tent is gone"
	svc, _ := newService(t)
	svc.Store = unwrapped{svc.Store}
	put(t, svc.Store, lockPath, []byte("not a lease"))
	jerr := json.Unmarshal([]byte("not a lease"), new(statestore.Lease))
	_, err := svc.Unlock(t.Context(), "prod", false)
	wantError(t, err, "lock cluster prod: invalid lease: "+jerr.Error()+hint)

	// A store that fails gets no hint: --force would fail the same way.
	if err := svc.Store.Delete(t.Context(), lockPath); err != nil {
		t.Fatal(err)
	}
	svc.Store = failingPuts{svc.Store}
	_, err = svc.Unlock(t.Context(), "prod", false)
	wantError(t, err, `lock cluster prod: put "prod/lock": connection refused`)
}
