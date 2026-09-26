package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

// locksDir holds the cluster locks of a file store. Its name starts with '.', so it is never an object.
const locksDir = ".tent-locks"

// heldFlocks keeps the handles of held locks reachable until Unlock. Otherwise a locker that became garbage would let
// the finalizer of its open file close it, and the OS would release the lock.
var heldFlocks sync.Map // *flock.Flock → struct{}

// flockLocker locks a cluster of a file store with a file lock on <root>/.tent-locks/<cluster>.lock, which the OS
// holds for the holder's process and releases when that process ends. So the lock never expires and is never left
// behind, and nothing but its holder can release it while that runs. Only TryLock creates the lock file, and it is
// never deleted: a waiter could then lock the old file while a newcomer locks a new one. When someone else deletes or
// replaces it, Renew and Unlock fail with ErrLockLost and leave the lease file to the new holder, and a TryLock that
// locked the old file reports that the lock changed hands.
//
// The holder's lease is in <cluster>.lease, a separate file because Windows keeps other processes from reading a
// locked one. A lease under a free lock was left by a holder that died: Holder ignores it, TryLock replaces it and
// returns it as previous, and ForceUnlock removes it and returns it. Every step holds the store lock, so the lock and
// the lease change together.
type flockLocker struct {
	store     *fileStore
	cluster   string
	lockFile  string       // OS path
	leasePath string       // relative to the root, like an object path
	held      *flock.Flock // the handle that holds the lock, or nil
	id        string       // of the lease held
}

func newFlockLocker(s *fileStore, l Layout) *flockLocker {
	return &flockLocker{
		store:     s,
		cluster:   l.Cluster(),
		lockFile:  filepath.Join(s.root, locksDir, l.Cluster()+".lock"),
		leasePath: locksDir + "/" + l.Cluster() + ".lease",
	}
}

// TryLock takes the lock and writes the lease. The lease's expiry does not matter: a live holder keeps the lock.
func (k *flockLocker) TryLock(ctx context.Context, l Lease) (Lease, *Lease, error) {
	if l.ID == "" {
		return Lease{}, nil, k.fail("lock", errNoID)
	}
	if err := mkdirAll(filepath.Dir(k.lockFile)); err != nil {
		return Lease{}, nil, k.fail("lock", err)
	}
	var previous *Lease
	err := k.store.withLock(ctx, true, func() error {
		lk := k.createFlock()
		ok, err := lk.TryLock()
		switch {
		case err != nil:
			return err
		case !ok:
			h, err := k.liveHolder()
			if err != nil {
				return err
			}
			return &LockedError{Cluster: k.cluster, Holder: h}
		}
		// The file may have been deleted or replaced between opening and locking it.
		if err := k.sameFile(lk); err != nil {
			if errors.Is(err, ErrLockLost) {
				err = fmt.Errorf("lock cluster %s: the lock file %s was deleted or replaced meanwhile: %w", k.cluster,
					k.lockFile, ErrLocked)
			}
			return errors.Join(err, lk.Unlock())
		}
		previous, err = k.readLease()
		if err == nil {
			err = k.writeLease(l)
		}
		if err != nil {
			return errors.Join(err, lk.Unlock())
		}
		heldFlocks.Store(lk, struct{}{})
		k.held, k.id = lk, l.ID
		return nil
	})
	switch {
	case errors.Is(err, ErrLocked):
		return Lease{}, nil, err
	case err != nil:
		return Lease{}, nil, k.fail("lock", err)
	}
	return l, previous, nil
}

// Renew writes l to the lease file. The expiry there only informs: the OS keeps the lock while the holder runs.
func (k *flockLocker) Renew(ctx context.Context, l Lease) (Lease, error) {
	if !k.holds(l) {
		return Lease{}, k.fail("renew the lock of", ErrLockLost)
	}
	err := k.store.withLock(ctx, true, func() error {
		if err := k.sameFile(k.held); err != nil {
			return err
		}
		return k.writeLease(l)
	})
	if err != nil {
		return Lease{}, k.fail("renew the lock of", err)
	}
	return l, nil
}

// Unlock removes the lease, then releases the lock. When it fails otherwise than with ErrLockLost, the lock stays
// held and Unlock may be retried.
func (k *flockLocker) Unlock(ctx context.Context, l Lease) error {
	if !k.holds(l) {
		return k.fail("unlock", ErrLockLost)
	}
	err := k.store.withLock(ctx, true, func() error {
		err := k.sameFile(k.held)
		if err == nil {
			err = k.removeLease()
		}
		if err != nil && !errors.Is(err, ErrLockLost) {
			return err
		}
		return errors.Join(err, k.held.Unlock()) // a lost lock leaves the lease file to the new holder
	})
	if err == nil || errors.Is(err, ErrLockLost) {
		heldFlocks.Delete(k.held)
		k.held, k.id = nil, ""
	}
	if err != nil {
		return k.fail("unlock", err)
	}
	return nil
}

// Holder returns nil when the lock is free, even when a lease file is left over.
func (k *flockLocker) Holder(ctx context.Context) (*Lease, error) {
	var holder *Lease
	err := k.store.withLock(ctx, false, func() error {
		lk := k.openFlock()
		ok, err := lk.TryRLock() // shared, so that readers do not keep each other out
		switch {
		case isMissing(err):
			return nil // no lock file, so no holder
		case err != nil:
			return err
		case ok:
			return lk.Unlock()
		}
		h, err := k.liveHolder()
		holder = &h
		return err
	})
	switch {
	case errors.Is(err, errNoRoot):
		return nil, nil
	case errors.Is(err, ErrLocked):
		return nil, err
	case err != nil:
		return nil, k.fail("read the lock of", err)
	}
	return holder, nil
}

// ForceUnlock removes a lease file left over under a free lock. It cannot break the lock of a holder that runs.
func (k *flockLocker) ForceUnlock(ctx context.Context) (*Lease, error) {
	var removed *Lease
	err := k.store.withLock(ctx, true, func() (err error) {
		lk := k.openFlock()
		ok, err := lk.TryLock()
		switch {
		case isMissing(err):
			// No lock file, so no holder; a lease file may be left over all the same.
		case err != nil:
			return err
		case !ok:
			h, err := k.liveHolder()
			if err == nil {
				err = &LockedError{Cluster: k.cluster, Holder: h}
			}
			return fmt.Errorf("%w; that tent is still running and holds a file lock that only it can release: "+
				"stop that process first", err)
		default:
			defer func() { err = errors.Join(err, lk.Unlock()) }()
		}
		data, err := k.store.read(k.leasePath)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err == nil {
			err = k.removeLease()
		}
		if err != nil {
			return err
		}
		if removed, err = decodeLease(data); err != nil {
			return fmt.Errorf("%w; removed it anyway", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoRoot):
		return nil, nil
	case errors.Is(err, ErrLocked):
		return nil, err
	case err != nil:
		return nil, k.fail("force unlock", err)
	}
	return removed, nil
}

// createFlock returns a handle on the lock file that creates the file when it is missing.
func (k *flockLocker) createFlock() *flock.Flock {
	return flock.New(k.lockFile, flock.SetPermissions(filePerm))
}

// openFlock returns a handle on the lock file that fails with fs.ErrNotExist when the file is missing, so that reading
// a lock leaves no file behind. Locking a read-only file works on Linux, macOS and Windows.
func (k *flockLocker) openFlock() *flock.Flock {
	return flock.New(k.lockFile, flock.SetFlag(os.O_RDONLY))
}

func (k *flockLocker) holds(l Lease) bool { return k.held != nil && l.ID == k.id }

// sameFile returns an error wrapping ErrLockLost when the file that lk locks was deleted or replaced, since a newcomer
// may then hold the new one. Windows cannot delete or replace a file that is open.
func (k *flockLocker) sameFile(lk *flock.Flock) error {
	held, err := lk.Stat()
	if err != nil {
		return err
	}
	now, err := os.Stat(k.lockFile)
	switch {
	case isMissing(err):
		return fmt.Errorf("the lock file %s was deleted: %w", k.lockFile, ErrLockLost)
	case err != nil:
		return err
	case !os.SameFile(held, now):
		return fmt.Errorf("the lock file %s was replaced: %w", k.lockFile, ErrLockLost)
	}
	return nil
}

// liveHolder returns the lease of the process that holds the lock. When the lease is missing or unreadable, the error
// matches ErrLocked all the same.
func (k *flockLocker) liveHolder() (Lease, error) {
	l, err := k.readLease()
	switch {
	case err != nil:
		return Lease{}, fmt.Errorf("cluster %s is %w, but its lease cannot be read: %w", k.cluster, ErrLocked, err)
	case l == nil:
		return Lease{}, fmt.Errorf("cluster %s is %w, but its holder has written no lease", k.cluster, ErrLocked)
	}
	return *l, nil
}

// readLease returns the lease in the lease file, or nil when there is none.
func (k *flockLocker) readLease() (*Lease, error) {
	data, err := k.store.read(k.leasePath)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeLease(data)
}

// removeLease removes the lease file. Unlike removing an object, it keeps the emptied locks directory, where a
// TryLock may be about to create its lock file.
func (k *flockLocker) removeLease() error {
	err := os.Remove(k.store.file(k.leasePath))
	if isMissing(err) {
		return nil
	}
	return err
}

// writeLease replaces the lease file atomically, so a reader never sees half a lease.
func (k *flockLocker) writeLease(l Lease) error {
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode the lease: %w", err)
	}
	return k.store.write(k.leasePath, data)
}

func (k *flockLocker) fail(op string, err error) error {
	return fmt.Errorf("%s cluster %s: %w", op, k.cluster, err)
}
