package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

func TestFileStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store {
		// The root does not exist yet: the store creates it on the first write.
		return openFile(t, filepath.Join(t.TempDir(), "state"))
	})
}

func TestErrorMessages(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"))
	ctx := t.Context()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Put(ctx, "x", nil, statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, getErr := s.Get(ctx, "missing")
	_, listErr := s.List(canceled, "")
	_, _, dotErr := s.Get(ctx, "a/../b")
	_, prefixErr := s.List(ctx, "a/.")
	_, bothErr := s.Put(ctx, "a", nil, statestore.PutOptions{IfNoneMatch: true, IfMatch: "v"})
	_, existsErr := s.Put(ctx, "x", nil, statestore.PutOptions{IfNoneMatch: true})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{getErr, `get "missing": not found`},
		{listErr, `list "": context canceled`},
		{dotErr, `get "a/../b": invalid path: segment "..": paths have no . or .. segments`},
		{prefixErr, `list "a/.": invalid prefix: segment ".": paths have no . or .. segments`},
		{bothErr, `put "a": IfNoneMatch and IfMatch are mutually exclusive`},
		{existsErr, `put "x": precondition failed: the object exists`},
		{s.Delete(ctx, "a b"), `delete "a b": invalid path: segment "a b": use only ASCII letters, digits, '_', '-' ` +
			`and '.', and start with a letter, digit or '_'`},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("error = %v\nwant    %s", tc.err, tc.want)
		}
	}
}

// TestStoreWithoutConditionalPut runs the suite against a store that cannot enforce conditions.
func TestStoreWithoutConditionalPut(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store {
		return noConditions{openFile(t, filepath.Join(t.TempDir(), "state"))}
	})
}

// noConditions is a file store that reports no conditional puts and rejects them, as such a backend must.
type noConditions struct{ statestore.Store }

func (noConditions) Capabilities(context.Context) (statestore.Capabilities, error) {
	return statestore.Capabilities{}, nil
}

func (s noConditions) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if opts.IfNoneMatch || opts.IfMatch != "" {
		return "", fmt.Errorf("put %q: conditional puts: %w", p, errors.ErrUnsupported)
	}
	return s.Store.Put(ctx, p, data, opts)
}
