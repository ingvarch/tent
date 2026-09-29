package nodeuptest

import (
	"context"
	"sync"

	"github.com/ingvarch/tent/internal/nodeup/env"
)

// Environment is a metadata service that describes Instance, or fails with Err when it is set. When Silent is set, it
// answers nothing, and Read waits until its context ends. Like a real one, it fails with the context's cause once its
// context has ended. It counts its reads, and is safe for concurrent use.
type Environment struct {
	Instance env.Instance
	Err      error
	Silent   bool

	mu    sync.Mutex
	reads int
}

// Read returns Instance, or Err.
func (e *Environment) Read(ctx context.Context) (env.Instance, error) {
	e.mu.Lock()
	e.reads++
	silent, inst, err := e.Silent, e.Instance, e.Err
	e.mu.Unlock()
	if silent {
		<-ctx.Done()
	}
	switch {
	case context.Cause(ctx) != nil:
		return env.Instance{}, context.Cause(ctx)
	case err != nil:
		return env.Instance{}, err
	}
	return inst, nil
}

// Reads returns how often Read was called.
func (e *Environment) Reads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads
}
