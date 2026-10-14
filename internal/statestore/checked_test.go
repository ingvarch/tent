package statestore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// callLog is a backend that records each call it receives and answers with fixed values.
type callLog struct {
	calls []string
}

func (b *callLog) Get(_ context.Context, p string) ([]byte, statestore.Version, error) {
	b.calls = append(b.calls, "get "+p)
	return []byte("data"), "v1", nil
}

func (b *callLog) Put(_ context.Context, p string, _ []byte, _ statestore.PutOptions) (statestore.Version, error) {
	b.calls = append(b.calls, "put "+p)
	return "v2", nil
}

func (b *callLog) List(_ context.Context, prefix string) ([]string, error) {
	b.calls = append(b.calls, "list "+prefix)
	return []string{"a"}, nil
}

func (b *callLog) Delete(_ context.Context, p string) error {
	b.calls = append(b.calls, "delete "+p)
	return nil
}

func (b *callLog) Capabilities(context.Context) (statestore.Capabilities, error) {
	return statestore.Capabilities{ConditionalPut: true}, nil
}

func (b *callLog) String() string { return "calllog://" }

// TestCheckedStopsInvalidCallsBeforeTheBackend checks that a store from Checked rejects an invalid path, an invalid
// prefix, contradicting conditions and a done context without a call to the backend, and names the operation and the
// path in the error.
func TestCheckedStopsInvalidCallsBeforeTheBackend(t *testing.T) {
	backend := &callLog{}
	s := statestore.Checked(backend)
	ctx := t.Context()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, getErr := s.Get(ctx, "a/../b")
	_, putErr := s.Put(ctx, "a b", nil, statestore.PutOptions{})
	_, bothErr := s.Put(ctx, "a", nil, statestore.PutOptions{IfNoneMatch: true, IfMatch: "v"})
	_, listErr := s.List(ctx, "a/.")
	deleteErr := s.Delete(ctx, ".tent-x")
	_, _, doneErr := s.Get(canceled, "a")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"get", getErr, `get "a/../b": invalid path: segment "..": paths have no . or .. segments`},
		{"put", putErr, `put "a b": invalid path: segment "a b": use only ASCII letters, digits, '_', '-' ` +
			`and '.', and start with a letter, digit or '_'`},
		{"both conditions", bothErr, `put "a": IfNoneMatch and IfMatch are mutually exclusive`},
		{"list", listErr, `list "a/.": invalid prefix: segment ".": paths have no . or .. segments`},
		{"delete", deleteErr, `delete ".tent-x": invalid path: segment ".tent-x" starts with '.': such names are ` +
			`reserved for the store`},
		{"done context", doneErr, `get "a": context canceled`},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("%s: error = %v\nwant %s", tc.name, tc.err, tc.want)
		}
	}
	if len(backend.calls) != 0 {
		t.Errorf("the backend got %q, want no call", backend.calls)
	}
}

// TestCheckedPassesValidCallsOn checks that a valid call reaches the backend with its path, that the answers come
// back, and that the store reports the backend's capabilities and URL.
func TestCheckedPassesValidCallsOn(t *testing.T) {
	backend := &callLog{}
	s := statestore.Checked(backend)
	ctx := t.Context()
	data, v, err := s.Get(ctx, "a/b")
	if err != nil || string(data) != "data" || v != "v1" {
		t.Errorf("Get = %q, %q, %v; want the backend's answer", data, v, err)
	}
	if v, err := s.Put(ctx, "a/b", nil, statestore.PutOptions{}); err != nil || v != "v2" {
		t.Errorf("Put = %q, %v; want the backend's answer", v, err)
	}
	if paths, err := s.List(ctx, "a/"); err != nil || !slices.Equal(paths, []string{"a"}) {
		t.Errorf("List = %q, %v; want the backend's answer", paths, err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if want := []string{"get a/b", "put a/b", "list a/", "delete a/b"}; !slices.Equal(backend.calls, want) {
		t.Errorf("the backend got %q, want %q", backend.calls, want)
	}
	if caps, err := s.Capabilities(ctx); err != nil || !caps.ConditionalPut {
		t.Errorf("Capabilities = %+v, %v; want the backend's", caps, err)
	}
	if got := s.String(); got != "calllog://" {
		t.Errorf("String = %q, want the backend's", got)
	}
}

// TestCheckedWrapsTheBackendError checks that an error of the backend keeps its type through the wrapper.
func TestCheckedWrapsTheBackendError(t *testing.T) {
	s := statestore.Checked(noObjects{&callLog{}})
	_, _, err := s.Get(t.Context(), "a")
	if !errors.Is(err, statestore.ErrNotFound) {
		t.Errorf("Get error = %v, want one that matches ErrNotFound", err)
	}
}

// noObjects is a backend whose Get finds nothing.
type noObjects struct{ *callLog }

func (noObjects) Get(context.Context, string) ([]byte, statestore.Version, error) {
	return nil, "", statestore.ErrNotFound
}
