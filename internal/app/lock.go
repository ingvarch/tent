package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ingvarch/tent/internal/statestore"
)

// locked runs work under the cluster's lock, after the version check. The work's context ends when the lock is lost.
func (s *Service) locked(ctx context.Context, l statestore.Layout, op string, work func(context.Context) error) error {
	lock, err := s.lock(ctx, l, op, s.LockTimeout)
	if err != nil {
		return err
	}
	if p := lock.Previous(); p != nil && s.OnTakeover != nil {
		s.OnTakeover(*p)
	}
	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-lock.Lost():
			cancel(statestore.ErrLockLost)
		case <-wctx.Done():
		}
	}()
	err = statestore.CheckVersion(wctx, s.Store, l, s.Version)
	if err == nil {
		err = work(wctx)
	}
	lost := errors.Is(context.Cause(wctx), statestore.ErrLockLost)
	rerr := lock.Release(wctx)
	switch {
	case lost && err != nil:
		// Releasing a lost lock fails too.
		return &lockLost{fmt.Sprintf("lost the lock of cluster %s; stopped: %v", l.Cluster(), err), err}
	case err == nil && (lost || errors.Is(rerr, statestore.ErrLockLost)):
		return &lockLost{fmt.Sprintf("the change is saved, but the lock of cluster %s was lost before tent released it",
			l.Cluster()), nil}
	}
	// A release that failed stays in view next to the interruption.
	return errors.Join(stopped(ctx, err), rerr)
}

// lockLost is the error of a change whose lock was lost: it stopped for err, or it was saved when err is nil. It
// matches statestore.ErrLockLost.
type lockLost struct {
	msg string
	err error
}

func (e *lockLost) Error() string { return e.msg }

func (e *lockLost) Unwrap() []error {
	if e.err == nil {
		return []error{statestore.ErrLockLost}
	}
	return []error{statestore.ErrLockLost, e.err}
}

// saved reports whether err is the error of a change that was saved although its lock was lost at the end.
func saved(err error) bool {
	e, ok := errors.AsType[*lockLost](err)
	return ok && e.err == nil
}

// lock takes the cluster's lock for op. It waits up to timeout for another holder; 0 means it does not wait.
func (s *Service) lock(ctx context.Context, l statestore.Layout, op string, timeout time.Duration) (
	*statestore.Lock, error,
) {
	lk, mech, err := statestore.NewLocker(ctx, s.Store, l, nil)
	if err != nil {
		return nil, err
	}
	if mech == statestore.MechanismBestEffort && s.OnWeakLock != nil {
		s.OnWeakLock()
	}
	var (
		wait context.Context
		stop context.CancelFunc
	)
	if timeout > 0 {
		wait, stop = context.WithTimeout(ctx, timeout)
	} else {
		wait, stop = context.WithCancel(ctx)
	}
	defer stop()
	w := &waiting{Locker: lk, busy: func(holder error) {
		switch {
		case timeout <= 0:
			stop()
		case s.OnWait != nil:
			s.OnWait(holder)
		}
	}}
	lock, err := statestore.Acquire(wait, w, l.Cluster(), statestore.AcquireOptions{Operation: op})
	switch {
	case err == nil:
		return lock, nil
	case timeout <= 0 && w.first != nil:
		return nil, w.first // it did not wait
	case w.last() != nil && ctx.Err() != nil:
		return nil, &stoppedWaiting{held: w.last(), why: ctx.Err(), words: "interrupted"}
	case w.last() != nil && wait.Err() != nil:
		return nil, &stoppedWaiting{held: w.last(), why: wait.Err(), words: "gave up after " + timeout.String()}
	}
	return nil, err
}

// stoppedWaiting is the error of a change that stopped waiting for the lock: held says who holds it, and words why
// it stopped. It matches held and why, the error that ended the wait.
type stoppedWaiting struct {
	held, why error
	words     string
}

func (e *stoppedWaiting) Error() string { return e.held.Error() + "; " + e.words }

func (e *stoppedWaiting) Unwrap() []error { return []error{e.held, e.why} }

// waiting is a Locker that calls busy once, when a try first finds the lock held, with that try's error. It keeps the
// errors of the tries that found the lock held.
type waiting struct {
	statestore.Locker
	busy    func(holder error)
	first   error // of the first try that found the lock held
	named   error // of the last try that named the holder
	unnamed error // of the last try that could not name the holder
}

func (w *waiting) TryLock(ctx context.Context, l statestore.Lease) (statestore.Lease, *statestore.Lease, error) {
	held, previous, err := w.Locker.TryLock(ctx, l)
	if !errors.Is(err, statestore.ErrLocked) {
		return held, previous, err
	}
	if _, ok := errors.AsType[*statestore.LockedError](err); ok {
		w.named = err
	} else {
		w.unnamed = err
	}
	if w.first == nil {
		w.first = err
		w.busy(err)
	}
	return held, previous, err
}

// last returns the error that names the lock's holder best: the last that named it, else the last that could not.
func (w *waiting) last() error { return cmp.Or(w.named, w.unnamed) }

// Unlock removes the lock of a cluster whose holder expired or ended, and returns the lease it removed, or nil when
// the cluster was not locked. Without force it takes the lock for a moment, which takes over only an expired lease,
// and releases it; it refuses a holder that is still live. With force it removes the lease whoever holds it. A file
// lock differs: the OS holds it while its holder runs, so Unlock refuses a running holder even with force, and
// removes the lease of one that died. A cluster that is not locked and has nothing in the store is not found.
func (s *Service) Unlock(ctx context.Context, cluster string, force bool) (_ *statestore.Lease, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return nil, err
	}
	removed, err := s.unlock(ctx, l, force)
	if err != nil || removed != nil {
		return removed, err
	}
	paths, err := s.Store.List(ctx, l.Prefix())
	switch {
	case err != nil:
		return nil, err
	case len(paths) == 0:
		return nil, s.notFound(clusterLabel(cluster))
	}
	return nil, nil
}

// unlock is Unlock once the cluster's name is checked.
func (s *Service) unlock(ctx context.Context, l statestore.Layout, force bool) (*statestore.Lease, error) {
	lk, mech, err := statestore.NewLocker(ctx, s.Store, l, nil)
	if err != nil {
		return nil, err
	}
	if force || mech == statestore.MechanismFlock {
		return lk.ForceUnlock(ctx)
	}
	lock, err := s.lock(ctx, l, "unlock", 0)
	if errors.Is(err, statestore.ErrLocked) || errors.Is(err, statestore.ErrInvalidLease) {
		return nil, fmt.Errorf("%w; run it again with --force if that tent is gone", err)
	}
	if err != nil {
		return nil, err
	}
	if err := lock.Release(ctx); err != nil {
		return nil, err
	}
	return lock.Previous(), nil
}
