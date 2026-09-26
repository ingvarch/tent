package statestore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
)

// Version identifies one content of one object and is opaque to callers. Equal contents may have equal versions.
type Version string

// Errors that stores wrap, with the path, so callers match them with errors.Is.
var (
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrPreconditionFailed means a conditional Put did not write because the object did not match its condition.
	ErrPreconditionFailed = errors.New("precondition failed")
)

// PutOptions make a Put conditional. At most one may be set. A store without Capabilities.ConditionalPut rejects
// both with an error wrapping errors.ErrUnsupported: it never checks and then writes in a separate step.
type PutOptions struct {
	// IfNoneMatch creates the object only if it does not exist yet.
	IfNoneMatch bool
	// IfMatch replaces the object only if it still has this version.
	IfMatch Version
}

// valid rejects options that set both conditions.
func (o PutOptions) valid() error {
	if o.IfNoneMatch && o.IfMatch != "" {
		return errors.New("IfNoneMatch and IfMatch are mutually exclusive")
	}
	return nil
}

// Capabilities are what a store can do beyond plain reads and writes.
type Capabilities struct {
	// ConditionalPut means the store enforces PutOptions atomically, even between processes. Without it, a
	// conditional Put fails with an error wrapping errors.ErrUnsupported.
	ConditionalPut bool
}

// Store keeps objects by path. Implementations are safe for concurrent use.
type Store interface {
	// Get returns an object's content and version, or an error wrapping ErrNotFound.
	Get(ctx context.Context, path string) ([]byte, Version, error)
	// Put writes an object whole and returns its new version. When the condition in opts does not hold it writes
	// nothing and returns an error wrapping ErrPreconditionFailed; a store that cannot enforce conditions writes
	// nothing and returns an error wrapping errors.ErrUnsupported.
	Put(ctx context.Context, path string, data []byte, opts PutOptions) (Version, error)
	// List returns the paths of the objects that start with prefix, sorted. The prefix may end in the middle of a
	// segment or with a slash, or be empty.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes an object. Deleting a missing object succeeds.
	Delete(ctx context.Context, path string) error
	// Capabilities reports what the store can do. A store may ask its backend once and then remember the answer.
	Capabilities(ctx context.Context) (Capabilities, error)
	// String returns the store's URL without credentials, for messages.
	String() string
}

// checkedStore is the Store that Open returns. It rejects invalid calls before the backend sees them, so backends
// trust their input, and it names the operation and the path in every error.
type checkedStore struct{ backend Store }

func (s checkedStore) Get(ctx context.Context, p string) ([]byte, Version, error) {
	if err := ready(ctx, validPath(p)); err != nil {
		return nil, "", opError("get", p, err)
	}
	data, v, err := s.backend.Get(ctx, p)
	if err != nil {
		return nil, "", opError("get", p, err)
	}
	return data, v, nil
}

func (s checkedStore) Put(ctx context.Context, p string, data []byte, opts PutOptions) (Version, error) {
	if err := ready(ctx, cmp.Or(validPath(p), opts.valid())); err != nil {
		return "", opError("put", p, err)
	}
	v, err := s.backend.Put(ctx, p, data, opts)
	if err != nil {
		return "", opError("put", p, err)
	}
	return v, nil
}

func (s checkedStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ready(ctx, validPrefix(prefix)); err != nil {
		return nil, opError("list", prefix, err)
	}
	paths, err := s.backend.List(ctx, prefix)
	if err != nil {
		return nil, opError("list", prefix, err)
	}
	return paths, nil
}

func (s checkedStore) Delete(ctx context.Context, p string) error {
	if err := ready(ctx, validPath(p)); err != nil {
		return opError("delete", p, err)
	}
	if err := s.backend.Delete(ctx, p); err != nil {
		return opError("delete", p, err)
	}
	return nil
}

func (s checkedStore) Capabilities(ctx context.Context) (Capabilities, error) {
	caps, err := s.backend.Capabilities(ctx)
	if err != nil {
		return Capabilities{}, fmt.Errorf("capabilities: %w", err)
	}
	return caps, nil
}

func (s checkedStore) String() string { return s.backend.String() }

// ready returns why a call must not reach the backend: an invalid argument, or else a done context.
func ready(ctx context.Context, invalid error) error {
	if invalid != nil {
		return invalid
	}
	return ctx.Err()
}

func opError(op, p string, err error) error { return fmt.Errorf("%s %q: %w", op, p, err) }
