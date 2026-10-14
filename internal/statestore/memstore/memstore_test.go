package memstore_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/memstore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

func TestStore(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(*testing.T) statestore.Store { return memstore.New() })
}

// TestPutChangesTheVersionEveryTime checks that a put of the same content still gives a new version, so that an
// IfMatch on the earlier version fails. A file store would keep the version, which is a digest of the content.
func TestPutChangesTheVersionEveryTime(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	v1, err := s.Put(ctx, "a", []byte("x"), statestore.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.Put(ctx, "a", []byte("x"), statestore.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Errorf("two puts gave the version %q twice", v1)
	}
	_, err = s.Put(ctx, "a", []byte("y"), statestore.PutOptions{IfMatch: v1})
	if !errors.Is(err, statestore.ErrPreconditionFailed) {
		t.Errorf("Put with the earlier version: error = %v, want ErrPreconditionFailed", err)
	}
	if _, err = s.Put(ctx, "a", []byte("y"), statestore.PutOptions{IfMatch: v2}); err != nil {
		t.Errorf("Put with the current version: %v", err)
	}
}

// TestVersionsAreUniqueAcrossObjects checks that two objects with the same content have different versions, and that
// the version of a deleted and created object is not one that it had before.
func TestVersionsAreUniqueAcrossObjects(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	put := func(p string) statestore.Version {
		t.Helper()
		v, err := s.Put(ctx, p, []byte("x"), statestore.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a, b := put("a"), put("b")
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	again := put("a")
	if a == b || a == again || b == again {
		t.Errorf("versions %q, %q and %q: want three different ones", a, b, again)
	}
}

// TestDataIsCopied checks that the store keeps what Put got, whatever the caller does with its slice later, and that
// a slice from Get is the caller's own.
func TestDataIsCopied(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	in := []byte("kept")
	if _, err := s.Put(ctx, "a", in, statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	in[0] = 'X'
	out, _, err := s.Get(ctx, "a")
	if err != nil || string(out) != "kept" {
		t.Fatalf("Get = %q, %v; want the content of the put", out, err)
	}
	out[0] = 'Y'
	again, _, err := s.Get(ctx, "a")
	if err != nil || string(again) != "kept" {
		t.Errorf("Get after a change of an earlier answer = %q, %v; want %q", again, err, "kept")
	}
}

// TestPutOfNilHoldsAnEmptyObject checks that a put without data stores an object that Get finds.
func TestPutOfNilHoldsAnEmptyObject(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	if _, err := s.Put(ctx, "a", nil, statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	data, _, err := s.Get(ctx, "a")
	if err != nil || len(data) != 0 {
		t.Errorf("Get = %q, %v; want an empty object", data, err)
	}
}

func TestCapabilitiesReportConditionalPuts(t *testing.T) {
	t.Parallel()
	caps, err := memstore.New().Capabilities(t.Context())
	if err != nil || !caps.ConditionalPut {
		t.Errorf("Capabilities = %+v, %v; want conditional puts", caps, err)
	}
}

func TestString(t *testing.T) {
	t.Parallel()
	if got := memstore.New().String(); got != "memory://" {
		t.Errorf("String = %q, want memory://", got)
	}
}

// TestStoresAreIndependent checks that two stores share no objects.
func TestStoresAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := memstore.New(), memstore.New()
	if _, err := a.Put(t.Context(), "x", []byte("1"), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Get(t.Context(), "x"); !errors.Is(err, statestore.ErrNotFound) {
		t.Errorf("Get from the other store: error = %v, want ErrNotFound", err)
	}
}

// TestCallsRunConcurrently runs every call from several goroutines at once, for the race detector and for the
// runtime's check of concurrent map writes.
func TestCallsRunConcurrently(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, _ = s.Put(ctx, "a", []byte("x"), statestore.PutOptions{})
				_, _, _ = s.Get(ctx, "a")
				_, _ = s.List(ctx, "")
				_ = s.Delete(ctx, "a")
			}
		})
	}
	wg.Wait()
}

// TestPutRefusesAPathAboveOrBelowAnObject checks that the store refuses "a" while "a/b" exists and "a/b" while "a"
// exists, as the file store does, so that a layout that only an object store accepts fails in the tests too. As the
// file store's, the error wraps no sentinel of statestore.
func TestPutRefusesAPathAboveOrBelowAnObject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, first, second, want string }{
		{"above", "a/b", "a", `put "a": "a" holds other objects`},
		{"below", "a", "a/b", `put "a/b": "a" is an object, so it cannot hold "a/b"`},
		{"below two levels", "a", "a/b/c", `put "a/b/c": "a" is an object, so it cannot hold "a/b/c"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := memstore.New(), t.Context()
			if _, err := s.Put(ctx, tc.first, []byte("x"), statestore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := s.Put(ctx, tc.second, []byte("y"), statestore.PutOptions{})
			if err == nil || err.Error() != tc.want {
				t.Errorf("Put(%q) after %q: error = %v, want %q", tc.second, tc.first, err, tc.want)
			}
			if errors.Is(err, statestore.ErrPreconditionFailed) || errors.Is(err, statestore.ErrNotFound) {
				t.Errorf("Put(%q) after %q: error %v is a sentinel", tc.second, tc.first, err)
			}
			if got, serr := s.List(ctx, ""); serr != nil || len(got) != 1 || got[0] != tc.first {
				t.Errorf("List = %v, %v, want only %q", got, serr, tc.first)
			}
		})
	}
}

// TestPutAcceptsSiblingsAndANameThatOnlyStartsTheSame checks that "a" and "ab", and "a/b" and "a/c", are no clash.
func TestPutAcceptsSiblingsAndANameThatOnlyStartsTheSame(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	for _, p := range []string{"a", "ab", "a-b/c", "d/b", "d/c"} {
		if _, err := s.Put(ctx, p, []byte("x"), statestore.PutOptions{}); err != nil {
			t.Errorf("Put(%q): %v", p, err)
		}
	}
}

// TestPutOverAnObjectKeepsItsPath checks that a put of an existing path is no clash with itself.
func TestPutOverAnObjectKeepsItsPath(t *testing.T) {
	t.Parallel()
	s, ctx := memstore.New(), t.Context()
	for range 2 {
		if _, err := s.Put(ctx, "a/b", []byte("x"), statestore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPutChecksItsConditionBeforeThePath checks that a conditional put on a path above or below an object fails on its
// condition first, as a file store's does: the file store checks the condition before it writes.
func TestPutChecksItsConditionBeforeThePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, first, second string }{
		{"above", "a/b", "a"},
		{"below", "a", "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ctx := memstore.New(), t.Context()
			if _, err := s.Put(ctx, tc.first, []byte("x"), statestore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := s.Put(ctx, tc.second, []byte("y"), statestore.PutOptions{IfMatch: "1"})
			if !errors.Is(err, statestore.ErrPreconditionFailed) {
				t.Errorf("Put(%q, IfMatch) after %q: error = %v, want ErrPreconditionFailed", tc.second, tc.first, err)
			}
		})
	}
}
