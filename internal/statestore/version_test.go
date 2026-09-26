package statestore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// countingStore counts writes, to tell a no-op from a rewrite of equal content. With putErr set, Put fails with it.
type countingStore struct {
	statestore.Store
	puts   int
	putErr error
}

func (s *countingStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error) {
	s.puts++
	if s.putErr != nil {
		return "", s.putErr
	}
	return s.Store.Put(ctx, p, data, opts)
}

// versionStore returns a store and the layout of cluster prod, whose tent-version holds stored, or is missing when
// stored is empty.
func versionStore(t *testing.T, stored string) (*countingStore, statestore.Layout) {
	t.Helper()
	s := openFile(t, t.TempDir())
	l := mustLayout(t, "prod")
	if stored != "" {
		if _, err := s.Put(t.Context(), l.TentVersion(), []byte(stored), statestore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return &countingStore{Store: s}, l
}

// storedVersion returns the content of the cluster's tent-version, or "" when it is missing.
func storedVersion(t *testing.T, s statestore.Store, l statestore.Layout) string {
	t.Helper()
	data, _, err := s.Get(t.Context(), l.TentVersion())
	if errors.Is(err, statestore.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		name, stored, running string
		want                  string // the error, or "" for none
	}{
		{"missing", "", "v0.3.0", ""},
		{"equal", "v0.3.0\n", "v0.3.0", ""},
		{"newer", "v0.3.0\n", "v0.4.0", ""},
		{"no newline", "v0.3.0", "v0.3.0", ""},
		{"older", "v0.4.0\n", "v0.3.1", "cluster prod needs tent v0.4.0 or newer; this is v0.3.1"},
		{"pre-release of the stored release", "v0.4.0\n", "v0.4.0-rc.1",
			"cluster prod needs tent v0.4.0 or newer; this is v0.4.0-rc.1"},
		{"older pre-release", "v0.4.0-rc.2\n", "v0.4.0-rc.1",
			"cluster prod needs tent v0.4.0-rc.2 or newer; this is v0.4.0-rc.1"},
		{"release of the stored pre-release", "v0.4.0-rc.1\n", "v0.4.0", ""},
		{"describe", "v0.3.0\n", "v0.3.0-4-gabc1234", ""},
		{"older describe", "v0.4.0\n", "v0.3.1-4-gabc1234",
			"cluster prod needs tent v0.4.0 or newer; this is v0.3.1-4-gabc1234"},
		{"dirty describe", "v0.3.0\n", "v0.3.0-4-gabc1234-dirty", ""},
		{"dirty tag", "v0.3.0\n", "v0.3.0-dirty", ""},
		{"dev", "v9.0.0\n", "dev", ""},
		{"bare hash", "v9.0.0\n", "abc1234", ""},
		{"dirty bare hash", "v9.0.0\n", "abc1234-dirty", ""},
		// GoReleaser's default snapshot version, one commit after v0.3.0.
		{"snapshot", "v0.3.0\n", "v0.3.0-SNAPSHOT-48dde55", ""},
		{"garbage running", "v9.0.0\n", "0.3.0", ""},
		{"garbage", "x\n", "v0.3.0", `prod/tent-version holds "x", not a tent version`},
		{"no v", "0.3.0\n", "v0.3.0", `prod/tent-version holds "0.3.0", not a tent version`},
		{"short", "v0.3\n", "v0.3.0", `prod/tent-version holds "v0.3", not a tent version`},
		{"build metadata", "v0.3.0+b\n", "v0.3.0", `prod/tent-version holds "v0.3.0+b", not a tent version`},
		{"two lines", "v0.3.0\nv0.4.0\n", "v0.3.0",
			`prod/tent-version holds "v0.3.0\nv0.4.0", not a tent version`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, l := versionStore(t, tc.stored)
			err := statestore.CheckVersion(t.Context(), s, l, tc.running)
			if got := errorText(err); got != tc.want {
				t.Errorf("CheckVersion(%q) with %q stored = %s\nwant %s", tc.running, tc.stored, got, tc.want)
			}
			if tooOld := strings.Contains(tc.want, "needs tent"); errors.Is(err, statestore.ErrTentTooOld) != tooOld {
				t.Errorf("errors.Is(%v, ErrTentTooOld) = %t, want %t", err, !tooOld, tooOld)
			}
			if s.puts != 0 {
				t.Errorf("CheckVersion wrote %d times", s.puts)
			}
		})
	}
}

func TestRaiseVersion(t *testing.T) {
	for _, tc := range []struct {
		name, stored, running string
		want                  string // tent-version after the call, or "" when missing
		wantErr               string
	}{
		{"missing", "", "v0.3.0", "v0.3.0\n", ""},
		{"newer", "v0.3.0\n", "v0.4.0", "v0.4.0\n", ""},
		{"equal", "v0.3.0\n", "v0.3.0", "v0.3.0\n", ""},
		{"never lowers", "v0.5.0\n", "v0.4.0", "v0.5.0\n", ""},
		{"pre-release", "v0.3.0\n", "v0.4.0-rc.1", "v0.4.0-rc.1\n", ""},
		{"pre-release of the stored release", "v0.4.0\n", "v0.4.0-rc.1", "v0.4.0\n", ""},
		{"release of the stored pre-release", "v0.4.0-rc.1\n", "v0.4.0", "v0.4.0\n", ""},
		{"describe", "", "v0.3.1-4-gabc1234", "v0.3.1\n", ""},
		{"describe of the stored pre-release's release", "v0.3.0-rc.1\n", "v0.3.0-4-gabc1234", "v0.3.0\n", ""},
		{"describe of a pre-release", "", "v0.3.0-rc.1-4-gabc1234", "v0.3.0-rc.1\n", ""},
		{"dirty describe", "", "v0.3.1-4-gabc1234-dirty", "v0.3.1\n", ""},
		{"dirty tag", "", "v0.3.1-dirty", "v0.3.1\n", ""},
		{"dev", "", "dev", "", ""},
		{"snapshot", "", "v0.3.0-SNAPSHOT-48dde55", "", ""},
		{"snapshot newer than the stored version", "v0.2.0\n", "v0.3.0-SNAPSHOT-48dde55", "v0.2.0\n", ""},
		{"bare hash", "v0.3.0\n", "abc1234", "v0.3.0\n", ""},
		{"garbage", "x\n", "v0.3.0", "x\n", `prod/tent-version holds "x", not a tent version`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, l := versionStore(t, tc.stored)
			err := statestore.RaiseVersion(t.Context(), s, l, tc.running)
			if got := errorText(err); got != tc.wantErr {
				t.Errorf("RaiseVersion(%q) with %q stored = %s\nwant %s", tc.running, tc.stored, got, tc.wantErr)
			}
			if got := storedVersion(t, s, l); got != tc.want {
				t.Errorf("tent-version = %q, want %q", got, tc.want)
			}
			wantPuts := 0
			if tc.want != tc.stored {
				wantPuts = 1
			}
			if s.puts != wantPuts {
				t.Errorf("RaiseVersion wrote %d times, want %d", s.puts, wantPuts)
			}
		})
	}
}

func TestRaiseVersionTwice(t *testing.T) {
	s, l := versionStore(t, "")
	for range 2 {
		if err := statestore.RaiseVersion(t.Context(), s, l, "v0.4.0"); err != nil {
			t.Fatal(err)
		}
	}
	if s.puts != 1 {
		t.Errorf("two raises wrote %d times, want 1", s.puts)
	}
}

func TestRaiseVersionPutError(t *testing.T) {
	s, l := versionStore(t, "v0.3.0\n")
	putErr := errors.New("disk full")
	s.putErr = putErr
	if err := statestore.RaiseVersion(t.Context(), s, l, "v0.4.0"); !errors.Is(err, putErr) {
		t.Errorf("RaiseVersion = %v, want %v", err, putErr)
	}
}

func TestVersionGuardStoreErrors(t *testing.T) {
	s, l := versionStore(t, "v0.3.0\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, clustersErr := statestore.Clusters(ctx, s)
	for name, err := range map[string]error{
		"Clusters":     clustersErr,
		"CheckVersion": statestore.CheckVersion(ctx, s, l, "v0.3.0"),
		"RaiseVersion": statestore.RaiseVersion(ctx, s, l, "v0.4.0"),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v, want context.Canceled", name, err)
		}
	}
}

// errorText returns an error's message, or "" for no error.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
