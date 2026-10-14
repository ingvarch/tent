// Package memstore is a state store in memory for tests of other packages. It is faster than a file store and
// leaves nothing on disk. Tests that check files, their modes, syncs or the OS's file lock use a file store.
package memstore

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ingvarch/tent/internal/statestore"
)

// New returns an empty store that checks its calls as the stores of statestore.Open do. It enforces conditional
// puts, so statestore.NewLocker gives it the lease locker.
func New() statestore.Store { return statestore.Checked(&store{objects: map[string]object{}}) }

type object struct {
	data    []byte
	version statestore.Version
}

type store struct {
	mu      sync.Mutex
	objects map[string]object
	puts    uint64
}

func (s *store) Get(_ context.Context, p string) ([]byte, statestore.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[p]
	if !ok {
		return nil, "", statestore.ErrNotFound
	}
	return slices.Clone(o.data), o.version, nil
}

func (s *store) Put(_ context.Context, p string, data []byte, opts statestore.PutOptions) (statestore.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The condition first, as a file store checks it before it writes.
	old, exists := s.objects[p]
	switch {
	case opts.IfNoneMatch && exists:
		return "", statestore.ErrPreconditionFailed
	case opts.IfMatch != "" && (!exists || old.version != opts.IfMatch):
		return "", statestore.ErrPreconditionFailed
	}
	if err := s.clash(p); err != nil {
		return "", err
	}
	s.puts++
	v := statestore.Version(strconv.FormatUint(s.puts, 10))
	s.objects[p] = object{data: slices.Clone(data), version: v}
	return v, nil
}

// clash returns an error when p is below an object or above others, which a file store cannot hold (it explains the
// same in statestore). The caller holds the lock.
func (s *store) clash(p string) error {
	for i := range len(p) {
		if p[i] != '/' {
			continue
		}
		if _, ok := s.objects[p[:i]]; ok {
			return fmt.Errorf("%q is an object, so it cannot hold %q", p[:i], p)
		}
	}
	for other := range s.objects {
		if strings.HasPrefix(other, p+"/") {
			return fmt.Errorf("%q holds other objects", p)
		}
	}
	return nil
}

func (s *store) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for p := range s.objects {
		if strings.HasPrefix(p, prefix) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

func (s *store) Delete(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, p)
	return nil
}

func (*store) Capabilities(context.Context) (statestore.Capabilities, error) {
	return statestore.Capabilities{ConditionalPut: true}, nil
}

func (*store) String() string { return "memory://" }
