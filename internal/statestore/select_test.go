package statestore_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

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

// refuseWrites answers every put and delete with an error and lets reads through.
func refuseWrites(r *http.Request, _ string, _ []byte) *fakeError {
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		return &fakeError{http.StatusForbidden, "AccessDenied"}
	}
	return nil
}

func TestHolderOnS3SendsOneReadAndNoWrite(t *testing.T) {
	layout := mustLayout(t, "prod")
	for _, tc := range []struct {
		name string
		put  *statestore.Lease // the lease in the store, or nil for none
		want bool              // whether Holder returns a lease
	}{
		{"a live lease", new(testLease("live", time.Hour)), true},
		{"an expired lease", new(testLease("dead", -time.Minute)), false},
		{"no lease", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openS3(t, fakeURL(t, f))
			if tc.put != nil {
				putLease(t, s, layout, *tc.put)
			}
			f.set(func(f *fakeS3) { f.intercept = refuseWrites; f.requests = nil })
			got, err := statestore.Holder(t.Context(), s, layout)
			if err != nil {
				t.Fatalf("Holder: %v", err)
			}
			switch {
			case tc.want && (got == nil || got.ID != tc.put.ID):
				t.Errorf("Holder = %+v, want the lease %q", got, tc.put.ID)
			case !tc.want && got != nil:
				t.Errorf("Holder = %+v, want nil", got)
			}
			if want := []string{"GET state/" + layout.Lock()}; !slices.Equal(f.log(), want) {
				t.Errorf("requests = %q, want %q", f.log(), want)
			}
		})
	}
}

func TestHolderOnS3ReportsAnUnreadableLease(t *testing.T) {
	layout := mustLayout(t, "prod")
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	mustPutS3(t, s, layout.Lock(), "not json")
	if _, err := statestore.Holder(t.Context(), s, layout); !errors.Is(err, statestore.ErrInvalidLease) {
		t.Errorf("Holder = %v, want ErrInvalidLease", err)
	}
}

// noCapabilities is a store whose Capabilities fails the test. A wrapper counts as a store that is no file store.
type noCapabilities struct {
	t *testing.T
	statestore.Store
}

func (s noCapabilities) Capabilities(context.Context) (statestore.Capabilities, error) {
	s.t.Error("Capabilities was called")
	return statestore.Capabilities{}, errors.New("not expected")
}

// TestHolderOnAWrappedStoreReadsTheLease also fails at the first Capabilities call: the s3 store answers it with a
// probe that writes.
func TestHolderOnAWrappedStoreReadsTheLease(t *testing.T) {
	layout := mustLayout(t, "prod")
	s := noCapabilities{t, openFile(t, t.TempDir())}
	putLease(t, s, layout, testLease("live", time.Hour))
	got, err := statestore.Holder(t.Context(), s, layout)
	if err != nil || got == nil || got.ID != "live" {
		t.Errorf("Holder = %+v, %v; want the lease %q", got, err, "live")
	}
}

func TestHolderOnAFileStoreAnswersAsFlockDoes(t *testing.T) {
	root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
	s := openFile(t, root)
	if got, err := statestore.Holder(t.Context(), s, layout); err != nil || got != nil {
		t.Fatalf("Holder of a free lock = %+v, %v; want nil", got, err)
	}
	// A lease object that nobody holds under flock is no holder, however fresh it is.
	putLease(t, s, layout, testLease("left over", time.Hour))
	if got, err := statestore.Holder(t.Context(), s, layout); err != nil || got != nil {
		t.Fatalf("Holder with only a left-over lease object = %+v, %v; want nil", got, err)
	}
	lk := newFlock(t, openFile(t, root), layout)
	mine := testLease("mine", time.Hour)
	if _, _, err := lk.TryLock(t.Context(), mine); err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	other := openFile(t, root)
	got, err := statestore.Holder(t.Context(), other, layout)
	want, wantErr := newFlock(t, other, layout).Holder(t.Context())
	if !errors.Is(err, wantErr) || !cmp.Equal(got, want) {
		t.Errorf("Holder = %+v, %v; the flock locker says %+v, %v", got, err, want, wantErr)
	}
}
