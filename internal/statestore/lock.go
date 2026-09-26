package statestore

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sync"
	"time"
)

// Lease is the record of who holds a cluster lock.
type Lease struct {
	ID         string    `json:"id"`        // random, identifies one holder
	Owner      string    `json:"owner"`     // OS user name
	Host       string    `json:"host"`      // host name
	PID        int       `json:"pid"`       // process id on Host
	Operation  string    `json:"operation"` // update, delete, ...
	AcquiredAt time.Time `json:"acquiredAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Errors of the lock, for errors.Is.
var (
	// ErrLocked means another holder has the lock. A *LockedError matches it.
	ErrLocked = errors.New("locked")
	// ErrLockLost means the caller no longer holds the lock: it expired and was taken over, or it was removed.
	ErrLockLost = errors.New("lock lost")
)

// LockedError says who holds a cluster's lock.
type LockedError struct {
	Cluster string
	Holder  Lease
}

func (e *LockedError) Error() string {
	h := e.Holder
	msg := "cluster " + e.Cluster + " is locked by an unknown holder"
	if h.Owner != "" || h.Host != "" {
		msg = fmt.Sprintf("cluster %s is locked by %s@%s (pid %d)", e.Cluster, h.Owner, h.Host, h.PID)
	}
	if h.Operation != "" {
		msg += " for " + h.Operation
	}
	if !h.AcquiredAt.IsZero() {
		msg += " since " + h.AcquiredAt.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	return msg
}

// Is makes errors.Is(err, ErrLocked) hold.
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// Locker is one lock mechanism for one cluster. Implementations must be safe for use by one holder at a time, and
// every method must return soon after its ctx ends.
//
// A lease with an expiry in the past, by the clock of the one who wants the lock, may be taken over. Clocks of
// different hosts may differ by less than a lease's lifetime; the default of two minutes leaves the margin. A mechanism
// bound to its holder's process, such as flock, ignores expiry: its lock ends when the holder's process does.
type Locker interface {
	// TryLock takes the lock for l without waiting and returns the lease it holds. When it takes over a lease whose
	// holder expired or ended, it returns that lease as previous; its expiry may be in the future. When another
	// holder has the lock, err is a *LockedError. Any other error that matches ErrLocked means the lock changed hands
	// during the call, or its holder cannot be named; a retry may succeed.
	TryLock(ctx context.Context, l Lease) (held Lease, previous *Lease, err error)
	// Renew replaces the held lease with l, which has the same ID and a later expiry, and returns the lease it
	// holds. It fails with ErrLockLost if the lock is no longer held by l.ID.
	Renew(ctx context.Context, l Lease) (Lease, error)
	// Unlock releases the lock held by l. It fails with ErrLockLost if the lock is no longer held by l.ID.
	Unlock(ctx context.Context, l Lease) error
	// Holder returns the lease of the lock's holder, or nil when the lock is free. The lease may have expired. When
	// the lock is held but its holder cannot be named, the error matches ErrLocked. Any other error, such as for a
	// lease that cannot be read, leaves open whether the lock is held.
	Holder(ctx context.Context) (*Lease, error)
	// ForceUnlock removes the lock whoever holds it and returns the removed lease, or nil when the lock was free. A
	// mechanism bound to its holder's process refuses ForceUnlock while the holder runs, with an error that matches
	// ErrLocked.
	ForceUnlock(ctx context.Context) (*Lease, error)
}

// AcquireOptions tune Acquire. The zero value takes the defaults.
type AcquireOptions struct {
	// Operation names what the holder does, such as update or delete.
	Operation string
	// TTL is how long a lease lasts unless renewed; it is renewed every TTL/3. The default is 2 minutes.
	TTL time.Duration
	// Retry is the wait between tries while another holder has the lock. The default is 2 seconds.
	Retry time.Duration
	// Clock returns the current time. The default is time.Now.
	Clock func() time.Time
}

func (o AcquireOptions) withDefaults() AcquireOptions {
	if o.TTL <= 0 {
		o.TTL = 2 * time.Minute
	}
	if o.Retry <= 0 {
		o.Retry = 2 * time.Second
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return o
}

// Acquire waits until it holds the cluster's lock or ctx ends, then renews the lease in the background until
// Release. If ctx ends while another holder has the lock, the error wraps ctx.Err() and a *LockedError that names
// the last holder seen; only when no try could name the holder does it wrap ErrLocked without one.
func Acquire(ctx context.Context, lk Locker, cluster string, opts AcquireOptions) (*Lock, error) {
	opts = opts.withDefaults()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("lock cluster %s: %w", cluster, err)
	}
	lease := newLease(opts.Operation)
	// The last tries that found the lock taken: one that named the holder, and one that saw it change hands.
	var named, changed error
	for {
		lease.AcquiredAt = opts.Clock()
		lease.ExpiresAt = lease.AcquiredAt.Add(opts.TTL)
		held, previous, err := lk.TryLock(ctx, lease)
		_, isNamed := errors.AsType[*LockedError](err)
		switch {
		case err == nil:
			return startLock(ctx, lk, held, previous, opts), nil
		case isNamed:
			named = err
		case errors.Is(err, ErrLocked):
			changed = err
		case (named != nil || changed != nil) && ctx.Err() != nil:
			if !errors.Is(err, ctx.Err()) {
				err = fmt.Errorf("%w; %w", ctx.Err(), err)
			}
			return nil, stoppedWaiting(named, changed, err)
		default:
			return nil, err
		}
		if err := sleep(ctx, opts.Retry); err != nil {
			return nil, stoppedWaiting(named, changed, err)
		}
	}
}

// stoppedWaiting is the error of an Acquire that stopped waiting for why. It says who holds the lock: named, or
// changed when no try named the holder.
func stoppedWaiting(named, changed, why error) error {
	return fmt.Errorf("%w; stopped waiting: %w", cmp.Or(named, changed), why)
}

// newLease returns a lease with a new ID for this process, without its times.
func newLease(operation string) Lease {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return Lease{
		ID:        rand.Text(),
		Owner:     ownerName(user.Current, os.Getenv),
		Host:      host,
		PID:       os.Getpid(),
		Operation: operation,
	}
}

// ownerName returns the OS user's name, else $USER or $USERNAME, else "unknown".
func ownerName(current func() (*user.User, error), getenv func(string) string) string {
	if u, err := current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, key := range []string{"USER", "USERNAME"} {
		if name := getenv(key); name != "" {
			return name
		}
	}
	return "unknown"
}

// sleep waits for d or until ctx ends, and then returns ctx.Err().
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	return ctx.Err()
}

// Lock is a held cluster lock. It renews its lease in the background until Release.
type Lock struct {
	locker   Locker
	previous *Lease
	lost     chan struct{}
	stop     chan struct{}
	stopped  chan struct{}

	mu    sync.Mutex
	lease Lease

	release    sync.Once
	releaseErr error
}

// abandonTimeout bounds cleaning up after the caller's context has ended: releasing a lock or removing a lease.
const abandonTimeout = 5 * time.Second

// startLock returns the Lock of a held lease and starts renewing it. Renewals ignore the end of ctx and run until
// Release.
func startLock(ctx context.Context, lk Locker, held Lease, previous *Lease, opts AcquireOptions) *Lock {
	l := &Lock{
		locker:   lk,
		previous: previous,
		lost:     make(chan struct{}),
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
		lease:    held,
	}
	go l.renew(context.WithoutCancel(ctx), opts.TTL, opts.Clock)
	return l
}

// renew renews the lease every ttl/3 until Release. Each renewal gets one interval; one that takes longer fails. It
// closes Lost when the lock is gone, or when renewals failed until the lease would expire before the next one.
func (l *Lock) renew(ctx context.Context, ttl time.Duration, clock func() time.Time) {
	defer close(l.stopped)
	interval := ttl / 3
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-timer.C:
		}
		next := l.Lease()
		next.ExpiresAt = clock().Add(ttl)
		rctx, cancel := context.WithTimeout(ctx, interval)
		renewed, err := l.locker.Renew(rctx, next)
		cancel()
		if err == nil {
			l.mu.Lock()
			l.lease = renewed
			l.mu.Unlock()
		} else if errors.Is(err, ErrLockLost) || !clock().Add(interval).Before(l.Lease().ExpiresAt) {
			close(l.lost)
			return
		}
		timer.Reset(interval)
	}
}

// Lease returns the lease the lock holds now.
func (l *Lock) Lease() Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lease
}

// Previous returns the lease that this lock took over, whose holder expired or ended, for a warning, or nil.
func (l *Lock) Previous() *Lease { return l.previous }

// Lost returns a channel that is closed when the lock is lost. The operation it guards must then stop.
func (l *Lock) Lost() <-chan struct{} { return l.lost }

// Release stops the renewals and unlocks, also when ctx has ended: it waits for a running renewal, at most one
// renewal interval, and gives the unlock a short timeout of its own. Later calls return the result of the first.
func (l *Lock) Release(ctx context.Context) error {
	l.release.Do(func() {
		close(l.stop)
		<-l.stopped
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonTimeout)
		defer cancel()
		l.releaseErr = l.locker.Unlock(ctx, l.Lease())
	})
	return l.releaseErr
}
