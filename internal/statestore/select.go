package statestore

import (
	"context"
	"fmt"
)

// Mechanism names how a Locker locks, so callers can warn about a weak one.
type Mechanism string

// The lock mechanisms, from the strongest.
const (
	// MechanismFlock is a file lock that the OS releases when its holder's process ends. Only a file store has it.
	MechanismFlock Mechanism = "flock"
	// MechanismConditionalPut is a lease in the store that only conditional puts change.
	MechanismConditionalPut Mechanism = "conditional-put"
	// MechanismCloud is the cloud provider's own lock.
	MechanismCloud Mechanism = "cloud"
	// MechanismBestEffort is a lease in the store written without conditions: two holders may both get it, so callers
	// should warn loudly.
	MechanismBestEffort Mechanism = "best-effort"
)

// NewLocker picks the lock mechanism for a cluster: flock for a file store, a conditional-put lease for a store that
// enforces conditional writes, the provider's cloud-native locker when one is given, and a best-effort lease otherwise.
// A file store also enforces conditional writes, but flock wins there: the OS releases it when tent dies. A file store
// is one that Open returned for a file:// URL; a wrapper around it counts as another store.
func NewLocker(ctx context.Context, s Store, l Layout, cloud Locker) (Locker, Mechanism, error) {
	if f, ok := fileStoreOf(s); ok {
		return newFlockLocker(f, l), MechanismFlock, nil
	}
	caps, err := s.Capabilities(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("choose the lock of cluster %s: %w", l.Cluster(), err)
	}
	switch {
	case caps.ConditionalPut:
		return newLeaseLocker(s, l, nil), MechanismConditionalPut, nil
	case cloud != nil:
		return cloud, MechanismCloud, nil
	}
	return newBestEffortLocker(s, l, nil, 0), MechanismBestEffort, nil
}

// fileStoreOf returns the file store that Open made s for.
func fileStoreOf(s Store) (*fileStore, bool) {
	if c, ok := s.(checkedStore); ok {
		s = c.backend
	}
	f, ok := s.(*fileStore)
	return f, ok
}

// Holder returns the lease of the cluster's lock without taking the lock and without choosing a lock mechanism. It
// returns nil when the lock is free. For a file store it answers as the flock locker does. For any other store it
// reads the lease object, and returns nil when that lease has expired: a tent that died leaves such a lease, and a
// file store reports that state as free. It never calls Store.Capabilities and never writes, so a key that may only
// read can use it.
func Holder(ctx context.Context, s Store, l Layout) (*Lease, error) {
	if f, ok := fileStoreOf(s); ok {
		return newFlockLocker(f, l).Holder(ctx)
	}
	o := newLeaseObject(s, l, nil)
	lease, err := o.Holder(ctx)
	if err != nil || lease == nil || o.expired(*lease) {
		return nil, err
	}
	return lease, nil
}
