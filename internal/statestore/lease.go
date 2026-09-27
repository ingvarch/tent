package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// newLeaseLocker returns a Locker that keeps a cluster's lease as JSON in the store and changes it only with
// conditional puts, so the store must enforce them. It judges expiry by clock, or time.Now when clock is nil.
func newLeaseLocker(s Store, l Layout, clock func() time.Time) Locker {
	return &leaseLocker{leaseObject: newLeaseObject(s, l, clock)}
}

// newBestEffortLocker returns a Locker for a store without conditional puts. It keeps a cluster's lease as JSON in
// the store: it writes its lease, waits settle, and holds the lock if the lease it reads back is still its own. It
// is a best effort: two holders may both get the lock when one of them takes longer than settle between reading and
// writing, and a renewal, which reads and then writes, may undo a ForceUnlock that comes between the two. It judges
// expiry by clock, or time.Now when clock is nil. A settle of zero means 5 seconds.
func newBestEffortLocker(s Store, l Layout, clock func() time.Time, settle time.Duration) Locker {
	if settle <= 0 {
		settle = 5 * time.Second
	}
	return &bestEffortLocker{leaseObject: newLeaseObject(s, l, clock), settle: settle}
}

var errNoID = errors.New("the lease has no ID")

// leaseObject is a cluster's lease in a store. Both lease lockers share its reads and ForceUnlock.
type leaseObject struct {
	store   Store
	path    string
	cluster string
	clock   func() time.Time
}

func newLeaseObject(s Store, l Layout, clock func() time.Time) leaseObject {
	if clock == nil {
		clock = time.Now
	}
	return leaseObject{store: s, path: l.Lock(), cluster: l.Cluster(), clock: clock}
}

func (o leaseObject) Holder(ctx context.Context) (*Lease, error) {
	l, _, err := o.read(ctx)
	if err != nil {
		return nil, o.fail("read the lock of", err)
	}
	return l, nil
}

// ForceUnlock removes the lease even when it cannot read it, and then reports why.
func (o leaseObject) ForceUnlock(ctx context.Context) (*Lease, error) {
	data, _, err := o.store.Get(ctx, o.path)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err == nil {
		err = o.store.Delete(ctx, o.path)
	}
	if err != nil {
		return nil, o.fail("force unlock", err)
	}
	l, err := decodeLease(data)
	if err != nil {
		return nil, o.fail("force unlock", fmt.Errorf("%w; removed it anyway", err))
	}
	return l, nil
}

// read returns the lease in the store and its version, or nil when the lock is free.
func (o leaseObject) read(ctx context.Context) (*Lease, Version, error) {
	data, v, err := o.store.Get(ctx, o.path)
	if errors.Is(err, ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	l, err := decodeLease(data)
	return l, v, err
}

func decodeLease(data []byte) (*Lease, error) {
	var l Lease
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidLease, err)
	}
	return &l, nil
}

// put writes the lease l.
func (o leaseObject) put(ctx context.Context, l Lease, opts PutOptions) (Version, error) {
	data, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("encode the lease: %w", err)
	}
	return o.store.Put(ctx, o.path, data, opts)
}

// abandon deletes the lease if it has the ID id, after a TryLock that fails with err: a failed write may have been
// carried out all the same, as when its answer was lost. It ignores the end of ctx and takes at most abandonTimeout.
// It returns err, with the error of the removal if that fails. A rival that takes the lock between the read and the
// Delete loses its lease, as with Unlock; that needs our fresh lease to have expired meanwhile.
func (o leaseObject) abandon(ctx context.Context, id string, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonTimeout)
	defer cancel()
	cur, _, rerr := o.read(ctx)
	if rerr == nil && cur != nil && cur.ID == id {
		rerr = o.store.Delete(ctx, o.path)
	}
	if rerr != nil {
		return fmt.Errorf("%w; remove the lease: %w", err, rerr)
	}
	return err
}

// expired reports whether the lease l may be taken over now.
func (o leaseObject) expired(l Lease) bool { return !o.clock().Before(l.ExpiresAt) }

func (o leaseObject) locked(l Lease) error { return &LockedError{Cluster: o.cluster, Holder: l} }

// changed is the error of a TryLock that saw the lock change hands and could not tell who holds it now.
func (o leaseObject) changed() error {
	return fmt.Errorf("lock cluster %s: the lock changed hands meanwhile: %w", o.cluster, ErrLocked)
}

func (o leaseObject) fail(op string, err error) error {
	return fmt.Errorf("%s cluster %s: %w", op, o.cluster, err)
}

// leaseLocker takes the lease with conditional puts. It remembers the version of the lease it holds.
type leaseLocker struct {
	leaseObject
	id      string  // of the lease held
	version Version // of the lease held
}

// TryLock creates the lease, or replaces an expired one. It tries twice: once more when the lease it found was
// released, or taken over by another holder, before it could act on it. When a write fails, it removes l if the store
// holds it all the same.
func (k *leaseLocker) TryLock(ctx context.Context, l Lease) (Lease, *Lease, error) {
	if l.ID == "" {
		return Lease{}, nil, k.fail("lock", errNoID)
	}
	for range 2 {
		v, err := k.put(ctx, l, PutOptions{IfNoneMatch: true})
		if err == nil {
			return k.took(l, v, nil)
		}
		if !errors.Is(err, ErrPreconditionFailed) {
			return Lease{}, nil, k.fail("lock", k.abandon(ctx, l.ID, err))
		}
		cur, curV, err := k.read(ctx)
		switch {
		case err != nil:
			return Lease{}, nil, k.fail("lock", err)
		case cur == nil:
			continue
		case !k.expired(*cur):
			return Lease{}, nil, k.locked(*cur)
		}
		v, err = k.put(ctx, l, PutOptions{IfMatch: curV})
		if err == nil {
			return k.took(l, v, cur)
		}
		if !errors.Is(err, ErrPreconditionFailed) {
			return Lease{}, nil, k.fail("lock", k.abandon(ctx, l.ID, err))
		}
	}
	return Lease{}, nil, k.changed()
}

func (k *leaseLocker) took(l Lease, v Version, previous *Lease) (Lease, *Lease, error) {
	k.id, k.version = l.ID, v
	return l, previous, nil
}

// holds reports whether the locker holds the lease l. Without a version it holds nothing: a conditional put with
// an empty version would be unconditional.
func (k *leaseLocker) holds(l Lease) bool { return k.version != "" && l.ID == k.id }

// Renew writes l over the held lease. When the lease has changed since the locker last wrote it but still has l.ID,
// as after a renewal whose answer was lost, it takes the lease's version and writes once more.
func (k *leaseLocker) Renew(ctx context.Context, l Lease) (Lease, error) {
	if !k.holds(l) {
		return Lease{}, k.fail("renew the lock of", ErrLockLost)
	}
	v, err := k.put(ctx, l, PutOptions{IfMatch: k.version})
	if errors.Is(err, ErrPreconditionFailed) {
		if err = k.adopt(ctx); err == nil {
			v, err = k.put(ctx, l, PutOptions{IfMatch: k.version})
		}
	}
	if errors.Is(err, ErrPreconditionFailed) {
		err = ErrLockLost
	}
	if err != nil {
		return Lease{}, k.fail("renew the lock of", err)
	}
	k.version = v
	return l, nil
}

// Unlock reads the lease and deletes it if it is still the one held. A lease that another holder writes between the
// two steps would be deleted too; that needs the held lease to expire and be taken over meanwhile, which renewals
// prevent.
func (k *leaseLocker) Unlock(ctx context.Context, l Lease) error {
	if !k.holds(l) {
		return k.fail("unlock", ErrLockLost)
	}
	err := k.adopt(ctx)
	if err == nil {
		err = k.store.Delete(ctx, k.path)
	}
	if err != nil {
		return k.fail("unlock", err)
	}
	k.id, k.version = "", ""
	return nil
}

// adopt reads the lease and, if it has the held ID, takes its version as the held one. When the lease is missing,
// unreadable or another holder's, the error wraps ErrLockLost.
func (k *leaseLocker) adopt(ctx context.Context) error {
	data, v, err := k.store.Get(ctx, k.path)
	if errors.Is(err, ErrNotFound) {
		return ErrLockLost
	}
	if err != nil {
		return err
	}
	cur, err := decodeLease(data)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", ErrLockLost, err)
	case cur.ID != k.id:
		return ErrLockLost
	}
	k.version = v
	return nil
}

// bestEffortLocker takes the lease with plain puts and a wait. It remembers the ID of the lease it holds.
type bestEffortLocker struct {
	leaseObject
	settle time.Duration
	id     string // of the lease held
}

// TryLock writes l when the lock is free, waits settle, and reads it back. If the write fails, or ctx ends during the
// wait, it removes l again unless another holder has overwritten it.
func (k *bestEffortLocker) TryLock(ctx context.Context, l Lease) (Lease, *Lease, error) {
	if l.ID == "" {
		return Lease{}, nil, k.fail("lock", errNoID)
	}
	cur, _, err := k.read(ctx)
	if err != nil {
		return Lease{}, nil, k.fail("lock", err)
	}
	if cur != nil && !k.expired(*cur) {
		return Lease{}, nil, k.locked(*cur)
	}
	if _, err := k.put(ctx, l, PutOptions{}); err != nil {
		return Lease{}, nil, k.fail("lock", k.abandon(ctx, l.ID, err))
	}
	if err := sleep(ctx, k.settle); err != nil {
		return Lease{}, nil, k.fail("lock", k.abandon(ctx, l.ID, err))
	}
	now, _, err := k.read(ctx)
	switch {
	case err != nil:
		return Lease{}, nil, k.fail("lock", err)
	case now == nil:
		return Lease{}, nil, k.changed()
	case now.ID != l.ID:
		return Lease{}, nil, k.locked(*now)
	}
	k.id = l.ID
	return l, cur, nil
}

func (k *bestEffortLocker) Renew(ctx context.Context, l Lease) (Lease, error) {
	err := k.check(ctx, l)
	if err == nil {
		_, err = k.put(ctx, l, PutOptions{})
	}
	if err != nil {
		return Lease{}, k.fail("renew the lock of", err)
	}
	return l, nil
}

func (k *bestEffortLocker) Unlock(ctx context.Context, l Lease) error {
	err := k.check(ctx, l)
	if err == nil {
		err = k.store.Delete(ctx, k.path)
	}
	if err != nil {
		return k.fail("unlock", err)
	}
	k.id = ""
	return nil
}

// check returns ErrLockLost unless the store holds l, the lease this locker took.
func (k *bestEffortLocker) check(ctx context.Context, l Lease) error {
	if k.id == "" || l.ID != k.id {
		return ErrLockLost
	}
	cur, _, err := k.read(ctx)
	if err != nil {
		return err
	}
	if cur == nil || cur.ID != l.ID {
		return ErrLockLost
	}
	return nil
}
