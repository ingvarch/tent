// Package app runs tent's use cases: it creates, gets, replaces and edits cluster specs in the state store, removes a
// stale cluster lock, updates a cluster's cloud objects to its specs, and deletes a cluster's cloud objects and its
// state.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/statestore"
)

// Service runs the use cases on one state store.
type Service struct {
	Store statestore.Store
	// Version is the running tent's version, for the cluster's version guard.
	Version string
	// LockTimeout is how long a change waits for another holder of the cluster lock; 0 means it does not wait.
	LockTimeout time.Duration
	// Validate holds the validation options, such as AllowSingleServer.
	Validate v1alpha1.ValidateOptions
	// OnWait, when set, is called once when a change starts to wait for the cluster's lock, with the error that says
	// who holds it. The error of a change that stops waiting starts with the same words.
	OnWait func(holder error)
	// OnWeakLock, when set, is called when the store allows only a best-effort cluster lock.
	OnWeakLock func()
	// OnTakeover, when set, is called with the lease of a holder that expired or ended when a change takes its lock.
	OnTakeover func(previous statestore.Lease)
	// OnWarning, when set, is called with each warning about the cluster that a change goes ahead with, such as a Nomad
	// API that the whole internet may reach: after a create, replace or save, and before an update applies its
	// changes. One change never calls it concurrently.
	OnWarning func(warning string)
	// Channels, when set, returns the release channel called name, which a cluster's spec names; it defaults to the
	// channels embedded in tent.
	Channels func(name string) (*channels.Channel, error)
	// Providers returns the cloud provider that a cluster's spec names, for an update or a delete. It fails for a
	// provider that tent does not know or cannot reach, such as one without its credentials.
	Providers func(v1alpha1.Provider) (cloud.Provider, error)
	// OnProgress, when set, is told what happens while an update or a delete applies its plan. One update or delete
	// never calls it concurrently.
	OnProgress func(Progress)
	// OnUpdatePlan, when set, is called with the plan of an update that has changes, made under the cluster's lock,
	// just before the update applies it. When it returns an error, the update stops before it changes anything and
	// returns that error.
	OnUpdatePlan func(UpdatePlan) error
	// OnDeletePlan, when set, is called with the plan of a delete, made under the cluster's lock, just before the
	// delete applies it. When it returns an error, the delete stops before it changes anything and returns that error.
	OnDeletePlan func(DeletePlan) error
	// Now, when set, returns the current time for new CA certificates; it defaults to time.Now.
	Now func() time.Time
}

// now returns the current time for new CA certificates: that of Now, or else time.Now.
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Action is what a use case did to one object.
type Action string

// Actions.
const (
	Created   Action = "created"
	Replaced  Action = "replaced"
	Unchanged Action = "unchanged"
)

// Change is what a use case did to one object.
type Change struct {
	Kind   string // v1alpha1.KindCluster or v1alpha1.KindNodeGroup
	Name   string
	Action Action
}

// step is one write to the store.
type step func(context.Context) error

// interrupted is the error of a use case whose context ended, as on Ctrl-C. It matches the error it stands for.
type interrupted struct{ err error }

func (e *interrupted) Error() string { return "interrupted" }

func (e *interrupted) Unwrap() error { return e.err }

// stopped returns err, or when err comes from the end of ctx, an error that says the use case was interrupted. An
// error that says so already, such as that of a change that stopped waiting for the lock, stays.
func stopped(ctx context.Context, err error) error {
	_, said := errors.AsType[*interrupted](err)
	_, waited := errors.AsType[*stoppedWaiting](err)
	if err == nil || ctx.Err() == nil || !errors.Is(err, ctx.Err()) || said || waited {
		return err
	}
	return &interrupted{err}
}

// change plans a change of a cluster and, with apply, carries it out under the cluster's lock for op. It plans first
// without the lock, so that a change that fails or changes nothing takes no lock, then again under the lock, since
// the store may have changed meanwhile.
func (s *Service) change(ctx context.Context, l statestore.Layout, op string, apply bool,
	plan func(context.Context) ([]step, error),
) error {
	if err := statestore.CheckVersion(ctx, s.Store, l, s.Version); err != nil {
		return err
	}
	steps, err := plan(ctx)
	if err != nil || len(steps) == 0 || !apply {
		return err
	}
	return s.locked(ctx, l, op, func(ctx context.Context) error {
		steps, err := plan(ctx)
		if err != nil {
			return err
		}
		for _, st := range steps {
			if err := st(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// openAPIWarning is the warning about a cluster whose Nomad API the whole internet may reach.
const openAPIWarning = "spec.access.api lets the whole internet reach the Nomad API (port 4646); mTLS and ACLs " +
	"protect it; narrow it with --api-access or spec.access.api"

// warn tells OnWarning each warning, when it is set.
func (s *Service) warn(warnings ...string) {
	if s.OnWarning == nil {
		return
	}
	for _, w := range warnings {
		s.OnWarning(w)
	}
}
