package vultr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ingvarch/tent/internal/engine"
)

// outputID is the name of the output that holds an object's Vultr id.
const outputID = "id"

// snapshotOf returns the Vultr snapshot that a task plans against. A nil snapshot has no objects.
func snapshotOf(env *engine.Env) (*snapshot, error) {
	if env.Snapshot == nil {
		return &snapshot{}, nil
	}
	s, ok := env.Snapshot.(*snapshot)
	if !ok {
		return nil, fmt.Errorf("the snapshot is a %T, not a Vultr snapshot", env.Snapshot)
	}
	return s, nil
}

// opState is what a task keeps across the attempts of a create whose object carries the task's operation id.
type opState struct {
	mu sync.Mutex
	// search is set once a create got no answer: the object may exist, so every later attempt searches for it
	// before it creates.
	search bool
}

// createWithOp creates an object whose marker carries the task's operation id, and returns the object's id. create
// creates the object; find looks it up by the operation id. Vultr's names are not unique, so a create that may have
// been carried out is never sent again blindly:
//
//   - After a create without an answer (ErrUnavailable), it searches. When the search finds the object, it adopts
//     it; otherwise the error is retryable, and every later attempt searches before it creates again.
//   - A create that follows a search that found nothing searches once more when it succeeds: the lost copy may be
//     listed by then. It returns the id that find gives, which is the copy the inventory keeps; when the search
//     fails or finds nothing, the created id.
//   - A search lists objects, so its errors are retryable as those of an idempotent call.
//   - Any other error of the create is retryable only when Vultr did not carry the call out, such as a rate limit.
//     An ErrLimitReached error tells how to raise the limit.
func createWithOp(ctx context.Context, s *opState, find func(context.Context) (string, bool, error),
	create func(context.Context) (string, error)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.search {
		id, ok, err := search(ctx, find)
		if err != nil || ok {
			return id, err
		}
	}
	id, err := create(ctx)
	switch {
	case err == nil && s.search:
		if kept, ok, _ := find(ctx); ok {
			return kept, nil
		}
		return id, nil
	case err == nil:
		return id, nil
	case !errors.Is(err, ErrUnavailable):
		return "", withLimitHint(markRetryable(err, false))
	}
	s.search = true
	id, ok, serr := search(ctx, find)
	switch {
	case serr != nil:
		return "", serr
	case !ok:
		return "", engine.Retryable(fmt.Errorf("%w; no object with its operation id is listed yet", err), 0)
	}
	return id, nil
}

// search runs find, and marks its error retryable as that of an idempotent call.
func search(ctx context.Context, find func(context.Context) (string, bool, error)) (string, bool, error) {
	id, ok, err := find(ctx)
	if err != nil {
		err = fmt.Errorf("search by operation id after a create without an answer: %w", err)
		return "", false, markRetryable(err, true)
	}
	return id, ok, nil
}

// opFinder returns the find of createWithOp for objects of the type t: it lists them with list, and returns the id of
// the object that the cluster owns and whose marker carries the operation id op, and whether there is one.
func opFinder[T any](list func(context.Context) ([]T, error), t objectType[T], cluster,
	op string) func(context.Context) (string, bool, error) {
	return func(ctx context.Context) (string, bool, error) {
		objs, err := list(ctx)
		if err != nil {
			return "", false, err
		}
		o, ok := findByOp(objs, t, cluster, op)
		if !ok {
			return "", false, nil
		}
		return t.id(o), true, nil
	}
}

// findByOp returns the object of objs that the cluster owns, as the inventory decides it, and whose marker carries
// the operation id op. Of several, it returns the one that the inventory keeps.
func findByOp[T any](objs []T, t objectType[T], cluster, op string) (T, bool) {
	var found []T
	for _, o := range objs {
		if m, _, owned, _ := t.keyOf(cluster, t.text(o)); owned && m.Op == op {
			found = append(found, o)
		}
	}
	if len(found) == 0 {
		var zero T
		return zero, false
	}
	return slices.MinFunc(found, t.keep), true
}

// deleted returns the outcome of a delete call for the engine: nil when the object is gone, since an earlier attempt
// may have deleted it, and otherwise the call's error, marked retryable as that of an idempotent call.
func deleted(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return markRetryable(err, true)
}
