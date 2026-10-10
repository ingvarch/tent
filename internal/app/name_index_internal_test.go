package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/statestore"
)

// putRecord is a put that watchedStore saw.
type putRecord struct {
	Path string
	Data string
	Opts statestore.PutOptions
}

// watchedStore records the puts it gets, can fail reads and writes, and can hide the conditional puts of its store.
type watchedStore struct {
	statestore.Store
	puts         []putRecord
	getErr       error // fails every read of an object under names/
	putErr       error // fails every put, before it reaches the store
	noConditions bool
	capsErr      error // fails Capabilities
	capsCalls    int   // the calls of Capabilities
}

func (s *watchedStore) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	if s.getErr != nil && strings.Contains(p, "/names/") {
		return nil, "", s.getErr
	}
	return s.Store.Get(ctx, p)
}

func (s *watchedStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	s.puts = append(s.puts, putRecord{p, string(data), opts})
	if s.putErr != nil {
		return "", s.putErr
	}
	if s.noConditions && (opts.IfNoneMatch || opts.IfMatch != "") {
		return "", errors.ErrUnsupported
	}
	return s.Store.Put(ctx, p, data, opts)
}

func (s *watchedStore) Capabilities(ctx context.Context) (statestore.Capabilities, error) {
	s.capsCalls++
	if s.capsErr != nil {
		return statestore.Capabilities{}, s.capsErr
	}
	if s.noConditions {
		return statestore.Capabilities{}, nil
	}
	return s.Store.Capabilities(ctx)
}

// namesTestService returns a service over a watched file store, and the layout of cluster prod.
func namesTestService(t *testing.T) (*Service, *watchedStore, statestore.Layout) {
	t.Helper()
	svc, l := secretsTestService(t)
	store := &watchedStore{Store: svc.Store}
	svc.Store = store
	return svc, store, l
}

// putName stores data as the object of the group's highest index, as another writer would, and returns the version.
func putName(t *testing.T, s statestore.Store, l statestore.Layout, group, data string) statestore.Version {
	t.Helper()
	p, err := l.NameIndex(group)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Put(t.Context(), p, []byte(data), statestore.PutOptions{})
	if err != nil {
		t.Fatalf("put %s: %v", p, err)
	}
	return v
}

// storedName returns what the store holds as the highest index of the group.
func storedName(t *testing.T, s statestore.Store, l statestore.Layout, group string) string {
	t.Helper()
	p, err := l.NameIndex(group)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := s.Get(t.Context(), p)
	if err != nil {
		t.Fatalf("get %s: %v", p, err)
	}
	return string(data)
}

// TestServerGroups lists the server and combined groups of a model in its order, and only those among the names that
// are asked for.
func TestServerGroups(t *testing.T) {
	t.Parallel()
	m := rollTestModel()
	for _, tc := range []struct {
		name string
		only []string
		want []string
	}{
		{"every group", nil, []string{"servers", "all"}},
		{"a client group asked for", []string{"workers"}, nil},
		{"a server and a client group asked for", []string{"workers", "all"}, []string{"all"}},
		{"both, in the order of the model", []string{"all", "servers"}, []string{"servers", "all"}},
	} {
		if diff := cmp.Diff(tc.want, serverGroups(m, tc.only)); diff != "" {
			t.Errorf("%s: serverGroups (-want +got):\n%s", tc.name, diff)
		}
	}
}

// TestReadNamesGivesTheIndexAboveTheStoredOne checks next for a group without an object, with one that holds an index
// with or without its newline, and for a group that was not read.
func TestReadNamesGivesTheIndexAboveTheStoredOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stored string // "" for no object
		want   int
	}{
		{"no object", "", 0},
		{"zero", "0\n", 1},
		{"an index", "4\n", 5},
		{"an index without the newline", "7", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, store, l := namesTestService(t)
			if tc.stored != "" {
				putName(t, store.Store, l, "servers", tc.stored)
			}

			n, err := svc.readNames(t.Context(), l, []string{"servers"})

			if err != nil {
				t.Fatalf("readNames: %v", err)
			}
			if got := n.next("servers"); got != tc.want {
				t.Errorf("next = %d, want %d", got, tc.want)
			}
			if got := n.next("other"); got != 0 {
				t.Errorf("next of a group that was not read = %d, want 0", got)
			}
			if len(store.puts) != 0 {
				t.Errorf("readNames wrote %v", store.puts)
			}
		})
	}
}

// TestReadNamesRefusesAnObjectThatIsNoIndex fails with the text that names the object, for each content that is no
// index of a node name.
func TestReadNamesRefusesAnObjectThatIsNoIndex(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ stored, shown string }{
		{"x\n", `"x"`},
		{"\n", `""`},
		{"-1\n", `"-1"`},
		{"+1\n", `"+1"`},
		{"1 2\n", `"1 2"`},
		{"3\n\n", `"3\n"`},
		{"2147483648\n", `"2147483648"`},
	} {
		t.Run(tc.shown, func(t *testing.T) {
			t.Parallel()
			svc, store, l := namesTestService(t)
			putName(t, store.Store, l, "servers", tc.stored)

			_, err := svc.readNames(t.Context(), l, []string{"servers"})

			want := "prod/names/servers holds " + tc.shown + ", which is no index of a node name; it must hold the " +
				"highest index that a machine name of node group servers has had"
			if err == nil || err.Error() != want {
				t.Errorf("readNames error = %v, want %q", err, want)
			}
		})
	}
}

// TestReadNamesReportsAFailedRead names the object in the error of a read that fails.
func TestReadNamesReportsAFailedRead(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	boom := errors.New("boom")
	store.getErr = boom

	_, err := svc.readNames(t.Context(), l, []string{"servers"})

	if !errors.Is(err, boom) || err.Error() != "read prod/names/servers: boom" {
		t.Errorf("readNames error = %v, want %q", err, "read prod/names/servers: boom")
	}
}

// TestNamesRefuseWhatTheLayoutRefuses fails readNames for a group whose name is no path segment, and raise too, with
// no write.
func TestNamesRefuseWhatTheLayoutRefuses(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)

	if _, err := svc.readNames(t.Context(), l, []string{"a/b"}); err == nil {
		t.Error("readNames accepted a group name that is no path segment")
	}
	n, err := svc.readNames(t.Context(), l, nil)
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	if err := n.raise(t.Context(), "a/b", 1); err == nil || len(store.puts) != 0 {
		t.Errorf("raise for a group name that is no path segment: error %v, puts %v, want an error and no write", err,
			store.puts)
	}
}

// TestReadNamesDoesNotAskForTheCapabilities reads without a Capabilities call, since on some stores it writes.
func TestReadNamesDoesNotAskForTheCapabilities(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	store.capsErr = errors.New("boom")

	if _, err := svc.readNames(t.Context(), l, []string{"servers"}); err != nil {
		t.Errorf("readNames: %v", err)
	}
	if store.capsCalls != 0 {
		t.Errorf("readNames asked for the Capabilities %d times, want 0", store.capsCalls)
	}
}

// TestRaiseFailsWhenTheStoreCannotTellItsCapabilities returns the store's error and sends no put, and asks only for a
// write that is needed.
func TestRaiseFailsWhenTheStoreCannotTellItsCapabilities(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	putName(t, store.Store, l, "servers", "3\n")
	n, err := svc.readNames(t.Context(), l, []string{"servers"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	boom := errors.New("boom")
	store.capsErr = boom

	if err := n.raise(t.Context(), "servers", 3); err != nil || store.capsCalls != 0 {
		t.Errorf("raise of an index that is not higher: error %v after %d Capabilities calls, want none", err,
			store.capsCalls)
	}
	if err := n.raise(t.Context(), "servers", 4); !errors.Is(err, boom) {
		t.Errorf("raise error = %v, want the store's error", err)
	}
	if len(store.puts) != 0 {
		t.Errorf("raise sent %v after a failed Capabilities", store.puts)
	}
	if got := n.next("servers"); got != 4 {
		t.Errorf("next after a failed raise = %d, want 4", got)
	}
}

// TestRaiseWritesOnlyAHigherIndex writes the first index of a group that has none, writes a higher one, and writes
// nothing for an equal or a lower one. On a store with conditional puts, the first write creates the object and the
// next replaces the version that the run wrote.
func TestRaiseWritesOnlyAHigherIndex(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	n, err := svc.readNames(t.Context(), l, []string{"servers"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	raise := func(index int) {
		t.Helper()
		if err := n.raise(t.Context(), "servers", index); err != nil {
			t.Fatalf("raise %d: %v", index, err)
		}
	}

	raise(2)
	raise(2)
	raise(1)
	if got := n.next("servers"); got != 3 {
		t.Errorf("next after raise 2 = %d, want 3", got)
	}
	_, version, err := store.Store.Get(t.Context(), "prod/names/servers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	raise(5)

	want := []putRecord{
		{"prod/names/servers", "2\n", statestore.PutOptions{IfNoneMatch: true}},
		{"prod/names/servers", "5\n", statestore.PutOptions{IfMatch: version}},
	}
	if diff := cmp.Diff(want, store.puts); diff != "" {
		t.Errorf("the puts (-want +got):\n%s", diff)
	}
	if got := storedName(t, store.Store, l, "servers"); got != "5\n" {
		t.Errorf("the store holds %q, want %q", got, "5\n")
	}
}

// TestRaiseReplacesTheVersionThatWasRead conditions the write on the version of the object that the run read.
func TestRaiseReplacesTheVersionThatWasRead(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	version := putName(t, store.Store, l, "servers", "3\n")
	n, err := svc.readNames(t.Context(), l, []string{"servers"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}

	if err := n.raise(t.Context(), "servers", 4); err != nil {
		t.Fatalf("raise: %v", err)
	}

	want := []putRecord{{"prod/names/servers", "4\n", statestore.PutOptions{IfMatch: version}}}
	if diff := cmp.Diff(want, store.puts); diff != "" {
		t.Errorf("the puts (-want +got):\n%s", diff)
	}
}

// TestRaiseRefusesAnObjectThatChangedSinceTheRead fails when another writer changed the object, or created it, after
// the run read it, and leaves what the other writer wrote.
func TestRaiseRefusesAnObjectThatChangedSinceTheRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, first string }{
		{"an object that the run found missing", ""},
		{"an object that the run read", "3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, store, l := namesTestService(t)
			if tc.first != "" {
				putName(t, store.Store, l, "servers", tc.first)
			}
			n, err := svc.readNames(t.Context(), l, []string{"servers"})
			if err != nil {
				t.Fatalf("readNames: %v", err)
			}
			putName(t, store.Store, l, "servers", "9\n")

			err = n.raise(t.Context(), "servers", 4)

			want := "prod/names/servers changed meanwhile; run the command again"
			if err == nil || err.Error() != want {
				t.Errorf("raise error = %v, want %q", err, want)
			}
			if got := storedName(t, store.Store, l, "servers"); got != "9\n" {
				t.Errorf("the store holds %q, want what the other writer wrote, %q", got, "9\n")
			}
		})
	}
}

// TestRaiseOnAStoreWithoutConditionalPutsWritesPlainly puts the object with no condition, whether or not it existed.
func TestRaiseOnAStoreWithoutConditionalPutsWritesPlainly(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	store.noConditions = true
	putName(t, store.Store, l, "servers", "3\n")
	n, err := svc.readNames(t.Context(), l, []string{"servers", "control"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}

	for _, group := range []string{"servers", "control"} {
		if err := n.raise(t.Context(), group, 4); err != nil {
			t.Fatalf("raise %s: %v", group, err)
		}
	}

	want := []putRecord{
		{"prod/names/servers", "4\n", statestore.PutOptions{}},
		{"prod/names/control", "4\n", statestore.PutOptions{}},
	}
	if diff := cmp.Diff(want, store.puts); diff != "" {
		t.Errorf("the puts (-want +got):\n%s", diff)
	}
}

// TestRaiseReportsAFailedWriteAndKeepsTheIndex names the object in the error, and the index stays as it was.
func TestRaiseReportsAFailedWriteAndKeepsTheIndex(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	n, err := svc.readNames(t.Context(), l, []string{"servers"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	boom := errors.New("boom")
	store.putErr = boom

	err = n.raise(t.Context(), "servers", 4)

	if !errors.Is(err, boom) || err.Error() != "write prod/names/servers: boom" {
		t.Errorf("raise error = %v, want %q", err, "write prod/names/servers: boom")
	}
	if got := n.next("servers"); got != 0 {
		t.Errorf("next after a failed write = %d, want 0", got)
	}
}

// TestRaiseToListedRaisesEachGroupToItsHighestListedName raises the groups that were read to the highest index among
// the names of the listed machines that fit the group's pattern, and writes nothing for a group that has no such
// machine or whose object holds a higher index already.
func TestRaiseToListedRaisesEachGroupToItsHighestListedName(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	putName(t, store.Store, l, "servers", "1\n")
	putName(t, store.Store, l, "stale", "12\n")
	n, err := svc.readNames(t.Context(), l, []string{"servers", "control", "empty", "stale"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	var listed []cloud.Instance
	for _, name := range []string{
		"prod-servers-0", "prod-servers-2", "prod-servers-10", "prod-servers-3x", "prod-control-1", "prod-workers-9",
		"prod-stale-4", "other-servers-40",
	} {
		listed = append(listed, cloud.Instance{Name: name})
	}

	if err := n.raiseToListed(t.Context(), listed); err != nil {
		t.Fatalf("raiseToListed: %v", err)
	}

	for group, want := range map[string]string{"servers": "10\n", "control": "1\n", "stale": "12\n"} {
		if got := storedName(t, store.Store, l, group); got != want {
			t.Errorf("the store holds %q for %s, want %q", got, group, want)
		}
	}
	if len(store.puts) != 2 {
		t.Errorf("raiseToListed wrote %v, want a write for servers and one for control", store.puts)
	}
	if _, _, err := store.Store.Get(t.Context(), "prod/names/empty"); !errors.Is(err, statestore.ErrNotFound) {
		t.Errorf("the object of the group without a machine: error = %v, want none stored", err)
	}
}

// TestRaiseToListedReportsAFailedWrite stops at the first write that fails.
func TestRaiseToListedReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	svc, store, l := namesTestService(t)
	n, err := svc.readNames(t.Context(), l, []string{"servers"})
	if err != nil {
		t.Fatalf("readNames: %v", err)
	}
	store.putErr = errors.New("boom")

	err = n.raiseToListed(t.Context(), []cloud.Instance{{Name: "prod-servers-2"}})

	if err == nil || err.Error() != "write prod/names/servers: boom" {
		t.Errorf("raiseToListed error = %v, want %q", err, "write prod/names/servers: boom")
	}
}
