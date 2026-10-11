package statestore_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

func TestFileStoreSyncsAWrite(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"))
	log := statestore.RecordSyncs(t, s)

	mustPut(t, s, "x/y")
	// The new root and the new directory each sync their parent, then the file, then its directory.
	if got, want := log.Take(), []string{"dir", "dir", "file", "dir"}; !slices.Equal(got, want) {
		t.Errorf("syncs of a first Put = %v, want %v", got, want)
	}
	mustPut(t, s, "x/y")
	if got, want := log.Take(), []string{"file", "dir"}; !slices.Equal(got, want) {
		t.Errorf("syncs of an overwriting Put = %v, want %v", got, want)
	}
	mustDelete(t, s, "x/y")
	if got := log.Take(); len(got) != 0 {
		t.Errorf("syncs of a Delete = %v, want none", got)
	}
}

func TestFileStoreSyncsALock(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"))
	lk := newFlock(t, s, mustLayout(t, "prod"))
	log := statestore.RecordSyncs(t, s)

	tryLockFree(t, lk, testLease("a", time.Hour))
	// The new root and the new locks directory each sync their parent, then the lease file, then its directory.
	if got, want := log.Take(), []string{"dir", "dir", "file", "dir"}; !slices.Equal(got, want) {
		t.Errorf("syncs of a first lock = %v, want %v", got, want)
	}
}

func TestFileStoreWithoutSyncSyncsNothing(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"), statestore.WithoutSync())
	lk := newFlock(t, s, mustLayout(t, "prod"))
	log := statestore.RecordSyncs(t, s)

	mustPut(t, s, "x/y")
	mustPut(t, s, "x/y")
	mustDelete(t, s, "x/y")
	tryLockFree(t, lk, testLease("a", time.Hour))
	renew(t, lk, testLease("a", 2*time.Hour))
	if got := log.Take(); len(got) != 0 {
		t.Errorf("syncs = %v, want none", got)
	}
}

// TestFileStoreWithoutSyncKeepsBehaviour runs the same calls on a store with and without the option and compares
// what they return, the errors included.
func TestFileStoreWithoutSyncKeepsBehaviour(t *testing.T) {
	run := func(s statestore.Store) []string {
		ctx := t.Context()
		var out []string
		note := func(what string, err error) {
			if err != nil {
				what += ": " + err.Error()
			}
			out = append(out, what)
		}
		v, err := s.Put(ctx, "a/b", []byte("one"), statestore.PutOptions{IfNoneMatch: true})
		note("put "+string(v), err)
		_, err = s.Put(ctx, "a/b", []byte("two"), statestore.PutOptions{IfNoneMatch: true})
		note("put again", err)
		_, err = s.Put(ctx, "a/b", []byte("two"), statestore.PutOptions{IfMatch: "other"})
		note("put stale", err)
		_, err = s.Put(ctx, "a", []byte("clash"), statestore.PutOptions{})
		note("put clash", err)
		data, _, err := s.Get(ctx, "a/b")
		note("get "+string(data), err)
		paths, err := s.List(ctx, "")
		note("list "+strings.Join(paths, ","), err)
		note("delete", s.Delete(ctx, "a/b"))
		_, _, err = s.Get(ctx, "a/b")
		if !errors.Is(err, statestore.ErrNotFound) {
			t.Errorf("Get after Delete: %v, want ErrNotFound", err)
		}
		note("get gone", err)
		return out
	}
	with := run(openFile(t, filepath.Join(t.TempDir(), "state")))
	without := run(openFile(t, filepath.Join(t.TempDir(), "state"), statestore.WithoutSync()))
	if !slices.Equal(with, without) {
		t.Errorf("without sync:\n%v\nwith sync:\n%v", without, with)
	}
}

func TestFileStoreWithoutSync(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store {
		return openFile(t, filepath.Join(t.TempDir(), "state"), statestore.WithoutSync())
	})
}

func TestFlockLockerWithoutSync(t *testing.T) {
	storetest.RunLocker(t, func(t *testing.T, _ func() time.Time) (string, func() statestore.Locker) {
		root, layout := filepath.Join(t.TempDir(), "state"), mustLayout(t, "prod")
		return "prod", func() statestore.Locker {
			return newFlock(t, openFile(t, root, statestore.WithoutSync()), layout)
		}
	}, storetest.LockerOptions{ProcessBound: true})
}

// TestS3StoreIgnoresWithoutSync checks that the option changes nothing for a bucket.
func TestS3StoreIgnoresWithoutSync(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store {
		return openS3(t, fakeURL(t, newFakeS3(t)), statestore.WithoutSync())
	})
}
