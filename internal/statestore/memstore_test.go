package statestore_test

import (
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/memstore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

// TestMemoryLeaseLocker runs the locker suite on a memory store with the lease locker, which NewLocker picks for
// it. The memory store is for tests of other packages; its locking is checked here, where the lease locker's
// constructor with a clock is at hand.
func TestMemoryLeaseLocker(t *testing.T) {
	storetest.RunLocker(t, sharedStoreLockers(func(*testing.T) statestore.Store { return memstore.New() }),
		storetest.LockerOptions{})
}

// TestMemoryStoreNewLocker checks that NewLocker picks the conditional-put lease for a memory store.
func TestMemoryStoreNewLocker(t *testing.T) {
	wantMechanism(t, memstore.New(), statestore.MechanismConditionalPut)
}

// TestMemoryStoreHolder checks that Holder reads the lease that NewLocker's locker wrote, and that ForceUnlock of
// that locker removes it and returns it.
func TestMemoryStoreHolder(t *testing.T) {
	s, layout := memstore.New(), mustLayout(t, "prod")
	if h, err := statestore.Holder(t.Context(), s, layout); err != nil || h != nil {
		t.Fatalf("Holder of a free lock = %+v, %v; want nil", h, err)
	}
	lk, _, err := statestore.NewLocker(t.Context(), s, layout, nil)
	if err != nil {
		t.Fatal(err)
	}
	mine := testLease("mine", time.Hour)
	_, _, err = lk.TryLock(t.Context(), mine)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	h, err := statestore.Holder(t.Context(), s, layout)
	if err != nil || h == nil || h.ID != mine.ID {
		t.Fatalf("Holder = %+v, %v; want %q", h, err, mine.ID)
	}
	forced, err := lk.ForceUnlock(t.Context())
	if err != nil || forced == nil || forced.ID != mine.ID {
		t.Fatalf("ForceUnlock = %+v, %v; want %q", forced, err, mine.ID)
	}
	if h, err := statestore.Holder(t.Context(), s, layout); err != nil || h != nil {
		t.Errorf("Holder after ForceUnlock = %+v, %v; want nil", h, err)
	}
}
