// Package storetest holds the conformance suites of the state store: Run for every statestore.Store backend, and
// RunLocker for every statestore.Locker.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// Run runs the Store conformance suite. open must return a new, empty store on every call; each subtest opens its own.
func Run(t *testing.T, open func(t *testing.T) statestore.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s statestore.Store)
	}{
		{"GetMissing", testGetMissing},
		{"RoundTrip", testRoundTrip},
		{"Overwrite", testOverwrite},
		{"IfNoneMatch", testIfNoneMatch},
		{"IfMatch", testIfMatch},
		{"BothConditions", testBothConditions},
		{"UnsupportedConditions", testUnsupportedConditions},
		{"List", testList},
		{"Delete", testDelete},
		{"ValidPaths", testValidPaths},
		{"InvalidPaths", testInvalidPaths},
		{"ReservedNames", testReservedNames},
		{"ConcurrentCreateOnly", testConcurrentCreateOnly},
		{"ConcurrentIfMatch", testConcurrentIfMatch},
		{"CanceledContext", testCanceledContext},
		{"Capabilities", testCapabilities},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, open(t)) })
	}
}

func testGetMissing(t *testing.T, s statestore.Store) {
	wantNotFound(t, s, "missing") // in an empty store
	put(t, s, "obj", "x")
	put(t, s, "dir/obj", "x")
	// A prefix of an object and a path below an object are not objects either.
	for _, p := range []string{"missing", "dir", "obj/missing", "dir/missing/deeper"} {
		wantNotFound(t, s, p)
	}
}

func testRoundTrip(t *testing.T, s statestore.Store) {
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"binary", binary},
		{"1MiB", big},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := "roundtrip/" + tc.name
			v, err := s.Put(t.Context(), p, tc.data, statestore.PutOptions{})
			if err != nil {
				t.Fatalf("Put(%q): %v", p, err)
			}
			if v == "" {
				t.Errorf("Put(%q) returned an empty version", p)
			}
			got, gv, err := s.Get(t.Context(), p)
			if err != nil {
				t.Fatalf("Get(%q): %v", p, err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Errorf("Get(%q) returned %d bytes that differ from the %d put", p, len(got), len(tc.data))
			}
			if gv != v {
				t.Errorf("Get(%q) version = %q, Put returned %q", p, gv, v)
			}
		})
	}
}

func testOverwrite(t *testing.T, s statestore.Store) {
	v1 := put(t, s, "a", "one")
	v2 := put(t, s, "a", "two")
	if v2 == v1 {
		t.Errorf("new content kept the version %q", v1)
	}
	wantContent(t, s, "a", "two", v2)
}

func testIfNoneMatch(t *testing.T, s statestore.Store) {
	skipWithoutConditionalPut(t, s)
	createOnly := statestore.PutOptions{IfNoneMatch: true}
	v, err := s.Put(t.Context(), "a", []byte("first"), createOnly)
	if err != nil {
		t.Fatalf("create-only Put of a missing object: %v", err)
	}
	wantContent(t, s, "a", "first", v)

	_, err = s.Put(t.Context(), "a", []byte("second"), createOnly)
	wantErrIs(t, err, statestore.ErrPreconditionFailed, "create-only Put of an existing object", "a")
	wantContent(t, s, "a", "first", v)
}

func testIfMatch(t *testing.T, s statestore.Store) {
	skipWithoutConditionalPut(t, s)
	v1 := put(t, s, "a", "one")
	v2, err := s.Put(t.Context(), "a", []byte("two"), statestore.PutOptions{IfMatch: v1})
	if err != nil {
		t.Fatalf("Put with the current version: %v", err)
	}
	if v2 == v1 {
		t.Errorf("Put with the current version returned the old version %q", v1)
	}
	wantContent(t, s, "a", "two", v2)

	_, err = s.Put(t.Context(), "a", []byte("three"), statestore.PutOptions{IfMatch: v1})
	wantErrIs(t, err, statestore.ErrPreconditionFailed, "Put with a stale version", "a")
	wantContent(t, s, "a", "two", v2)

	_, err = s.Put(t.Context(), "missing", []byte("x"), statestore.PutOptions{IfMatch: v1})
	wantErrIs(t, err, statestore.ErrPreconditionFailed, "Put with a version of a missing object", "missing")
	wantNotFound(t, s, "missing")
}

func testBothConditions(t *testing.T, s statestore.Store) {
	v := put(t, s, "a", "one")
	both := statestore.PutOptions{IfNoneMatch: true, IfMatch: v}
	for _, p := range []string{"a", "missing"} {
		_, err := s.Put(t.Context(), p, []byte("two"), both)
		if err == nil || errors.Is(err, statestore.ErrPreconditionFailed) {
			t.Errorf("Put(%q) with IfNoneMatch and IfMatch: error = %v, want a usage error", p, err)
		}
	}
	wantContent(t, s, "a", "one", v)
	wantNotFound(t, s, "missing")
}

// testUnsupportedConditions checks that a store without conditional puts rejects conditions instead of checking and
// writing in two steps.
func testUnsupportedConditions(t *testing.T, s statestore.Store) {
	if conditionalPut(t, s) {
		t.Skip("the store enforces conditional puts")
	}
	v := put(t, s, "a", "one")
	for _, tc := range []struct {
		path string
		opts statestore.PutOptions
	}{
		{"missing", statestore.PutOptions{IfNoneMatch: true}},
		{"a", statestore.PutOptions{IfNoneMatch: true}},
		{"a", statestore.PutOptions{IfMatch: v}},
	} {
		_, err := s.Put(t.Context(), tc.path, []byte("two"), tc.opts)
		wantErrIs(t, err, errors.ErrUnsupported, fmt.Sprintf("Put with %+v", tc.opts), tc.path)
	}
	wantContent(t, s, "a", "one", v)
	wantNotFound(t, s, "missing")
}

func testList(t *testing.T, s statestore.Store) {
	if got := list(t, s, ""); len(got) != 0 {
		t.Errorf("List of an empty store = %q, want none", got)
	}
	// Written out of order; "a.txt" sorts before "a/b" because '.' < '/'.
	for _, p := range []string{
		"z", "prod/nodegroups/workers.yaml", "a/b", "production/x", "prod/cluster.yaml", "a.txt",
	} {
		put(t, s, p, p)
	}
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{
			"a.txt", "a/b", "prod/cluster.yaml", "prod/nodegroups/workers.yaml", "production/x", "z",
		}},
		{"prod/", []string{"prod/cluster.yaml", "prod/nodegroups/workers.yaml"}},
		{"pro", []string{"prod/cluster.yaml", "prod/nodegroups/workers.yaml", "production/x"}},
		{"prod/nodegroups/", []string{"prod/nodegroups/workers.yaml"}},
		{"prod/cluster", []string{"prod/cluster.yaml"}},
		{"prod/cluster.yaml", []string{"prod/cluster.yaml"}},
		{"a", []string{"a.txt", "a/b"}},
		{"z/", nil},
		{"missing/", nil},
	} {
		if got := list(t, s, tc.prefix); !slices.Equal(got, tc.want) {
			t.Errorf("List(%q) = %q, want %q", tc.prefix, got, tc.want)
		}
	}
}

func testDelete(t *testing.T, s statestore.Store) {
	if err := s.Delete(t.Context(), "never"); err != nil {
		t.Errorf("Delete in an empty store: %v", err)
	}
	put(t, s, "a/b/c", "x")
	put(t, s, "a/d", "x")
	put(t, s, "dir/obj", "x")

	if err := s.Delete(t.Context(), "a/b/c"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantNotFound(t, s, "a/b/c")
	if got, want := list(t, s, ""), []string{"a/d", "dir/obj"}; !slices.Equal(got, want) {
		t.Errorf("List after Delete = %q, want %q", got, want)
	}
	// Deleting what is not an object succeeds and changes nothing.
	for _, p := range []string{"a/b/c", "never", "dir", "a/d/below"} {
		if err := s.Delete(t.Context(), p); err != nil {
			t.Errorf("Delete(%q) of a missing object: %v", p, err)
		}
	}
	if got, want := list(t, s, ""), []string{"a/d", "dir/obj"}; !slices.Equal(got, want) {
		t.Errorf("List after deleting missing objects = %q, want %q", got, want)
	}
	put(t, s, "a/b/c", "again") // the emptied prefix is usable again
	wantContent(t, s, "a/b/c", "again", "")
}

// validPaths are unusual but valid on every backend and operating system.
var validPaths = []string{
	"1", "_x", "x-", "a.b.c", "a..b", "NULL", "nul-x", "con1", "com10", "lpt", "console.yaml",
	"Prod_2/history/20260926T120000Z-update.yaml",
}

func testValidPaths(t *testing.T, s statestore.Store) {
	for _, p := range validPaths {
		put(t, s, p, p)
	}
	for _, p := range validPaths {
		wantContent(t, s, p, p, "")
	}
	if got, want := list(t, s, ""), slices.Sorted(slices.Values(validPaths)); !slices.Equal(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
}

// invalidPaths are rejected as object paths. The ones marked prefix are valid List prefixes, whose last segment may
// be unfinished.
var invalidPaths = []struct {
	path   string
	prefix bool
}{
	{"", true},
	{"a/", true},
	{"a/b/", true},
	{"/", false},
	{"/a", false},
	{"//", false},
	{"a//b", false},
	{"a//", false},
	{".", false},
	{"..", false},
	{"./a", false},
	{"a/.", false},
	{"a/./b", false},
	{"../a", false},
	{"a/../b", false},
	{`a\b`, false},
	{`\a`, false},
	{"a\x00b", false},
	{"a\nb", false},
	{"a\tb", false},
	{"a\x7f", false},
	{"a\u0085b", false}, // a C1 control character
	{"\xff", false},     // not UTF-8
	{"a:b", false},
	{"a<b", false},
	{"a>b", false},
	{"a?", false},
	{"a*", false},
	{"a|b", false},
	{`a"b`, false},
	{"a b", false},
	{"é", false},
	{"-a", false},
	{".x", false},
	{"a.", true},
	{"a./b", false},
	{"nul", true},
	{"NUL.yaml", true},
	{"com1", true},
	{"Lpt9.txt", true},
	{"prod/aux/x", false},
}

func testInvalidPaths(t *testing.T, s statestore.Store) {
	for _, tc := range invalidPaths {
		wantRejected(t, s, tc.path, tc.prefix)
	}
	if got := list(t, s, ""); len(got) != 0 {
		t.Errorf("invalid paths wrote %q", got)
	}
}

func testReservedNames(t *testing.T, s statestore.Store) {
	for _, p := range []string{
		".tent-x", ".tent-", "a/.tent-tmp-1", ".tent-store.lock", "a/.tent-x/b", ".TENT-x", ".hidden", ".t",
	} {
		wantRejected(t, s, p, false)
	}
	if got := list(t, s, ""); len(got) != 0 {
		t.Errorf("reserved names wrote %q", got)
	}
}

func testConcurrentCreateOnly(t *testing.T, s statestore.Store) {
	skipWithoutConditionalPut(t, s)
	winner, v := race(t, func(data []byte) (statestore.Version, error) {
		return s.Put(t.Context(), "race", data, statestore.PutOptions{IfNoneMatch: true})
	})
	wantContent(t, s, "race", winner, v)
}

func testConcurrentIfMatch(t *testing.T, s statestore.Store) {
	skipWithoutConditionalPut(t, s)
	v0 := put(t, s, "race", "initial")
	winner, v := race(t, func(data []byte) (statestore.Version, error) {
		return s.Put(t.Context(), "race", data, statestore.PutOptions{IfMatch: v0})
	})
	wantContent(t, s, "race", winner, v)
}

// race runs write from 16 goroutines at once, each with its own data. Exactly one must succeed and the others must
// fail with ErrPreconditionFailed. It returns the winner's data and version.
func race(t *testing.T, write func(data []byte) (statestore.Version, error)) (string, statestore.Version) {
	t.Helper()
	const n = 16
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, n)
		vers  = make([]statestore.Version, n)
	)
	for i := range n {
		wg.Go(func() {
			<-start
			vers[i], errs[i] = write(fmt.Appendf(nil, "writer %d", i))
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner >= 0:
			t.Errorf("writers %d and %d both succeeded", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, statestore.ErrPreconditionFailed):
			t.Errorf("writer %d: %v, want ErrPreconditionFailed", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer succeeded")
	}
	return fmt.Sprintf("writer %d", winner), vers[winner]
}

func testCanceledContext(t *testing.T, s statestore.Store) {
	put(t, s, "a", "one")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := s.Get(ctx, "a")
	wantErrIs(t, err, context.Canceled, "Get", "a")
	_, err = s.Put(ctx, "b", []byte("x"), statestore.PutOptions{})
	wantErrIs(t, err, context.Canceled, "Put", "b")
	_, err = s.List(ctx, "")
	wantErrIs(t, err, context.Canceled, "List", "")
	err = s.Delete(ctx, "a")
	wantErrIs(t, err, context.Canceled, "Delete", "a")

	wantContent(t, s, "a", "one", "")
	wantNotFound(t, s, "b")
}

func testCapabilities(t *testing.T, s statestore.Store) {
	first, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	second, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities, second call: %v", err)
	}
	if first != second {
		t.Errorf("Capabilities changed between calls: %+v, then %+v", first, second)
	}
}

func skipWithoutConditionalPut(t *testing.T, s statestore.Store) {
	t.Helper()
	if !conditionalPut(t, s) {
		t.Skip("the store does not enforce conditional puts")
	}
}

func conditionalPut(t *testing.T, s statestore.Store) bool {
	t.Helper()
	caps, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	return caps.ConditionalPut
}

func put(t *testing.T, s statestore.Store, p, data string) statestore.Version {
	t.Helper()
	v, err := s.Put(t.Context(), p, []byte(data), statestore.PutOptions{})
	if err != nil {
		t.Fatalf("Put(%q): %v", p, err)
	}
	return v
}

func list(t *testing.T, s statestore.Store, prefix string) []string {
	t.Helper()
	got, err := s.List(t.Context(), prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	return got
}

// wantContent checks an object's content and, unless want is empty, its version.
func wantContent(t *testing.T, s statestore.Store, p, data string, want statestore.Version) {
	t.Helper()
	got, v, err := s.Get(t.Context(), p)
	if err != nil {
		t.Fatalf("Get(%q): %v", p, err)
	}
	if string(got) != data {
		t.Errorf("Get(%q) = %q, want %q", p, got, data)
	}
	if want != "" && v != want {
		t.Errorf("Get(%q) version = %q, want %q", p, v, want)
	}
}

func wantNotFound(t *testing.T, s statestore.Store, p string) {
	t.Helper()
	_, _, err := s.Get(t.Context(), p)
	wantErrIs(t, err, statestore.ErrNotFound, "Get", p)
}

// wantErrIs checks that err wraps target and names the path.
func wantErrIs(t *testing.T, err, target error, what, p string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s(%q): error = %v, want %v", what, p, err, target)
		return
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("%s(%q): error %q does not name the path", what, p, err)
	}
}

// wantRejected checks that every method rejects p, except List when p is a valid prefix.
func wantRejected(t *testing.T, s statestore.Store, p string, validPrefix bool) {
	t.Helper()
	ctx := t.Context()
	_, _, getErr := s.Get(ctx, p)
	_, putErr := s.Put(ctx, p, []byte("x"), statestore.PutOptions{})
	_, createErr := s.Put(ctx, p, []byte("x"), statestore.PutOptions{IfNoneMatch: true})
	for what, err := range map[string]error{
		"Get":             getErr,
		"Put":             putErr,
		"create-only Put": createErr,
		"Delete":          s.Delete(ctx, p),
	} {
		if !isRejection(err) {
			t.Errorf("%s(%q): error = %v, want a rejection", what, p, err)
		}
	}
	_, err := s.List(ctx, p)
	switch {
	case validPrefix && err != nil:
		t.Errorf("List(%q): %v", p, err)
	case !validPrefix && !isRejection(err):
		t.Errorf("List(%q): error = %v, want a rejection", p, err)
	}
}

func isRejection(err error) bool {
	return err != nil && !errors.Is(err, statestore.ErrNotFound) && !errors.Is(err, statestore.ErrPreconditionFailed)
}
