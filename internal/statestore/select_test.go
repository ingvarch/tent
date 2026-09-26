package statestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/statestore"
)

func TestNewLocker(t *testing.T) {
	cloud := &fakeLocker{}
	same := func(s statestore.Store) statestore.Store { return s }
	// A store that is not a file store and takes only conditional puts, so only the conditional-put lease works.
	conditional := func(s statestore.Store) statestore.Store {
		return hookedStore{s, func(opts statestore.PutOptions) error {
			if !opts.IfNoneMatch && opts.IfMatch == "" {
				return errors.New("an unconditional put")
			}
			return nil
		}}
	}
	// Without conditional puts, only the best-effort lease works.
	plain := func(s statestore.Store) statestore.Store { return noConditions{s} }
	for _, tc := range []struct {
		name  string
		store func(statestore.Store) statestore.Store
		cloud statestore.Locker
		want  statestore.Mechanism
	}{
		{"file store", same, nil, statestore.MechanismFlock},
		{"file store and a cloud locker", same, cloud, statestore.MechanismFlock},
		{"conditional puts", conditional, nil, statestore.MechanismConditionalPut},
		{"conditional puts and a cloud locker", conditional, cloud, statestore.MechanismConditionalPut},
		{"cloud locker", plain, cloud, statestore.MechanismCloud},
		{"nothing better", plain, nil, statestore.MechanismBestEffort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { // the best-effort lease waits 5s
				root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
				s := tc.store(openFile(t, root))
				releaseFlocksAtEnd(t)
				lk, got, err := statestore.NewLocker(t.Context(), s, layout, tc.cloud)
				if err != nil {
					t.Fatalf("NewLocker: %v", err)
				}
				if got != tc.want {
					t.Errorf("mechanism = %q, want %q", got, tc.want)
				}
				if tc.want == statestore.MechanismCloud {
					if lk != statestore.Locker(cloud) {
						t.Errorf("NewLocker returned %T, want the cloud locker", lk)
					}
					return
				}
				mine := testLease("mine", time.Hour)
				if _, _, err := lk.TryLock(t.Context(), mine); err != nil {
					t.Fatalf("TryLock: %v", err)
				}
				// A lease locker keeps the lease in the store, flock in a file outside the objects.
				_, _, objErr := s.Get(t.Context(), layout.Lock())
				_, fileErr := os.Stat(filepath.Join(root, ".tent-locks", "prod.lease"))
				inFile := tc.want == statestore.MechanismFlock
				if inFile != (fileErr == nil) || inFile == (objErr == nil) {
					t.Errorf("the lock object: %v; the lease file: %v; want only the %s one", objErr, fileErr, tc.want)
				}
			})
		})
	}
}

// TestMechanismNames pins the names, which the CLI prints.
func TestMechanismNames(t *testing.T) {
	for _, tc := range []struct {
		m    statestore.Mechanism
		want string
	}{
		{statestore.MechanismFlock, "flock"},
		{statestore.MechanismConditionalPut, "conditional-put"},
		{statestore.MechanismCloud, "cloud"},
		{statestore.MechanismBestEffort, "best-effort"},
	} {
		if string(tc.m) != tc.want {
			t.Errorf("mechanism %q, want %q", tc.m, tc.want)
		}
	}
}

func TestNewLockerCapabilitiesError(t *testing.T) {
	broken := errors.New("the store is unreachable")
	s := capabilitiesError{openFile(t, t.TempDir()), broken}
	lk, m, err := statestore.NewLocker(t.Context(), s, mustLayout(t, "prod"), &fakeLocker{})
	if !errors.Is(err, broken) || lk != nil || m != "" {
		t.Errorf("NewLocker = %v, %q, %v; want the Capabilities error", lk, m, err)
	}
}

// capabilitiesError is a store whose Capabilities fails with err.
type capabilitiesError struct {
	statestore.Store
	err error
}

func (s capabilitiesError) Capabilities(context.Context) (statestore.Capabilities, error) {
	return statestore.Capabilities{}, s.err
}
