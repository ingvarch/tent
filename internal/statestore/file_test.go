package statestore_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/ingvarch/tent/internal/statestore"
)

// fileURL returns the file:// URL of an absolute directory: file:///srv/state, or file:///C:/state on Windows.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func openFile(t *testing.T, root string) statestore.Store {
	t.Helper()
	s, err := statestore.Open(t.Context(), fileURL(root))
	if err != nil {
		t.Fatalf("Open(%q): %v", fileURL(root), err)
	}
	return s
}

func mustPut(t *testing.T, s statestore.Store, p string) {
	t.Helper()
	if _, err := s.Put(t.Context(), p, []byte(p), statestore.PutOptions{}); err != nil {
		t.Fatalf("Put(%q): %v", p, err)
	}
}

func mustDelete(t *testing.T, s statestore.Store, p string) {
	t.Helper()
	if err := s.Delete(t.Context(), p); err != nil {
		t.Fatalf("Delete(%q): %v", p, err)
	}
}

func TestFileCapabilities(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"))
	caps, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ConditionalPut {
		t.Error("ConditionalPut = false, want true")
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permissions")
	}
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	mustPut(t, s, "prod/pki/private/ca.key")

	for _, tc := range []struct {
		path string
		want fs.FileMode
	}{
		{"", 0o700},
		{"prod", 0o700},
		{"prod/pki", 0o700},
		{"prod/pki/private", 0o700},
		{"prod/pki/private/ca.key", 0o600},
		{".tent-store.lock", 0o600},
	} {
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(tc.path)))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s/%s: mode %v, want %v", root, tc.path, got, tc.want)
		}
	}
}

func TestSyncDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot sync a directory")
	}
	dir := t.TempDir()
	if err := statestore.SyncDir(dir); err != nil {
		t.Errorf("SyncDir(%s): %v", dir, err)
	}
	// Proof that it opens the directory rather than doing nothing.
	if err := statestore.SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SyncDir of a missing directory: error = %v, want fs.ErrNotExist", err)
	}
}

func TestFileWritesLeaveNoTempFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	ctx := t.Context()
	mustPut(t, s, "a")
	mustPut(t, s, "a") // over an existing file
	mustPut(t, s, "dir/b")
	if _, err := s.Put(ctx, "a", []byte("x"), statestore.PutOptions{IfNoneMatch: true}); err == nil {
		t.Error("create-only Put of an existing object succeeded")
	}
	// A file cannot replace a directory: the rename fails, and the temp file must go too.
	if _, err := s.Put(ctx, "dir", []byte("x"), statestore.PutOptions{}); err == nil {
		t.Error("Put over a directory succeeded")
	}

	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(filepath.Base(p), ".tent-tmp-") {
			t.Errorf("temp file left: %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFileObjectPrefixClash checks the errors for what an object store allows and a file system does not.
func TestFileObjectPrefixClash(t *testing.T) {
	s := openFile(t, filepath.Join(t.TempDir(), "state"))
	mustPut(t, s, "a")
	mustPut(t, s, "dir/x")
	for _, tc := range []struct{ path, want string }{
		{"a/b", `put "a/b": "a" is an object, so it cannot hold "a/b"`},
		{"a/b/c", `put "a/b/c": "a" is an object, so it cannot hold "a/b/c"`},
		{"dir", `put "dir": "dir" holds other objects`},
	} {
		_, err := s.Put(t.Context(), tc.path, nil, statestore.PutOptions{})
		if err == nil || err.Error() != tc.want {
			t.Errorf("Put(%q) error = %v, want %s", tc.path, err, tc.want)
		}
	}
}

func TestFileDeleteRemovesEmptyParents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	mustPut(t, s, "a/b/c")
	mustPut(t, s, "a/d")

	mustDelete(t, s, "a/b/c")
	wantMissing(t, filepath.Join(root, "a", "b"))
	if _, err := os.Stat(filepath.Join(root, "a", "d")); err != nil {
		t.Errorf("Delete removed a sibling: %v", err)
	}

	mustDelete(t, s, "a/d")
	wantMissing(t, filepath.Join(root, "a"))
}

// TestDeleteNeverRemovesTheRoot checks the directories Delete may remove directly: through the store, the lock file
// always keeps the root non-empty.
func TestDeleteNeverRemovesTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	got := statestore.ParentsBelowRoot(root, filepath.Join(root, "a", "b", "c"))
	if want := []string{filepath.Join(root, "a", "b"), filepath.Join(root, "a")}; !slices.Equal(got, want) {
		t.Errorf("ParentsBelowRoot = %q, want %q", got, want)
	}
	if got := statestore.ParentsBelowRoot(root, filepath.Join(root, "x")); len(got) != 0 {
		t.Errorf("ParentsBelowRoot of an object in the root = %q, want none", got)
	}
}

func TestFileSymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	s := openFile(t, link)
	mustPut(t, s, "a")
	mustPut(t, s, "prod/cluster.yaml")
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{"a", "prod/cluster.yaml"}},
		{"pro", []string{"prod/cluster.yaml"}},
		{"prod/", []string{"prod/cluster.yaml"}},
	} {
		got, err := s.List(t.Context(), tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("List(%q) = %q, want %q", tc.prefix, got, tc.want)
		}
	}
}

// TestFilePutReplacesTheFile checks that Put renames a new file over the object instead of writing into it. The syncs
// are not tested: only a power loss would show them.
func TestFilePutReplacesTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot replace a file that is open")
	}
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	if _, err := s.Put(t.Context(), "a", []byte("old"), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := s.Put(t.Context(), "a", []byte("new"), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// A reader that opened the object before still sees a whole old version.
	if got, err := io.ReadAll(f); err != nil || string(got) != "old" {
		t.Errorf("the open file reads %q, %v; want %q", got, err, "old")
	}
}

func TestFilePutOnCanceledContextCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Put(ctx, "a", nil, statestore.PutOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Put error = %v, want context.Canceled", err)
	}
	wantMissing(t, root)
}

func wantMissing(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: error = %v, want it gone", p, err)
	}
}

func TestFileListSkipsInvalidNames(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	mustPut(t, s, "a/b")
	// What a crashed write or a backend-internal directory leaves behind, and files with names that are not paths.
	for _, p := range []string{
		".tent-tmp-1", "a/.tent-tmp-2", ".tent-locks/prod.lock", ".TENT-upper", ".hidden", "bad name.txt",
		"a/é.txt", "bad dir/c",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b"}; !slices.Equal(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
}

// TestFileStoreLock holds the store lock through a second handle in this process, the way another tent would.
func TestFileStoreLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s := openFile(t, root)
	mustPut(t, s, "a")
	lk := flock.New(filepath.Join(root, ".tent-store.lock"))

	blocked := func(t *testing.T, what string, op func(ctx context.Context) error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if err := op(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s while the lock is held: error = %v, want context.DeadlineExceeded", what, err)
		}
	}
	get := func(ctx context.Context) error { _, _, err := s.Get(ctx, "a"); return err }
	list := func(ctx context.Context) error { _, err := s.List(ctx, ""); return err }
	put := func(ctx context.Context) error {
		_, err := s.Put(ctx, "a", []byte("x"), statestore.PutOptions{})
		return err
	}
	del := func(ctx context.Context) error { return s.Delete(ctx, "a") }
	// quick bounds what must not wait, so a regression fails instead of hanging.
	quick := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("exclusive holder blocks everything", func(t *testing.T) {
		if ok, err := lk.TryLock(); !ok || err != nil {
			t.Fatalf("TryLock = %v, %v", ok, err)
		}
		defer unlock(t, lk)
		blocked(t, "Get", get)
		blocked(t, "List", list)
		blocked(t, "Put", put)
		blocked(t, "Delete", del)
	})
	t.Run("shared holder blocks writes only", func(t *testing.T) {
		if ok, err := lk.TryRLock(); !ok || err != nil {
			t.Fatalf("TryRLock = %v, %v", ok, err)
		}
		defer unlock(t, lk)
		if err := get(quick(t)); err != nil {
			t.Errorf("Get: %v", err)
		}
		if err := list(quick(t)); err != nil {
			t.Errorf("List: %v", err)
		}
		blocked(t, "Put", put)
		blocked(t, "Delete", del)
	})
	if err := put(quick(t)); err != nil {
		t.Errorf("Put after the lock is released: %v", err)
	}
}

func unlock(t *testing.T, lk *flock.Flock) {
	t.Helper()
	if err := lk.Unlock(); err != nil {
		t.Error(err)
	}
}

const (
	helperIDEnv = "TENT_STATESTORE_HELPER_ID"
	raceRounds  = 20
)

// TestFileCrossProcessCreateOnly runs helper processes that create the same objects at the same time. The store lock
// must let exactly one process create each object.
func TestFileCrossProcessCreateOnly(t *testing.T) {
	const procs = 4
	root := filepath.Join(t.TempDir(), "state")
	helpers := make([]*helper, procs)
	for i := range helpers {
		helpers[i] = startHelper(t, "TestFileHelperProcess", helperURLEnv+"="+fileURL(root),
			fmt.Sprintf("%s=%d", helperIDEnv, i))
	}
	// Start every helper's writes at once.
	for _, h := range helpers {
		h.expect(t, "ready")
	}
	for _, h := range helpers {
		h.send(t, "go")
	}

	winners := map[string][]int{}
	for i, h := range helpers {
		lines, err := h.wait()
		var output []string // the test log of a failed helper
		for _, line := range lines {
			if p, ok := strings.CutPrefix(line, "created "); ok {
				winners[p] = append(winners[p], i)
			} else {
				output = append(output, line)
			}
		}
		if err != nil {
			t.Fatalf("helper %d: %v, output:\n%s\n%s", i, err, strings.Join(output, "\n"), h.stderr.String())
		}
	}

	s := openFile(t, root)
	for r := range raceRounds {
		p := fmt.Sprintf("race/%d", r)
		if len(winners[p]) != 1 {
			t.Errorf("%s was created by helpers %v, want exactly one", p, winners[p])
			continue
		}
		got, _, err := s.Get(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprint(winners[p][0]); string(got) != want {
			t.Errorf("%s holds %q, want the winner's %q", p, got, want)
		}
	}
}

// TestFileHelperProcess is a helper process of TestFileCrossProcessCreateOnly, not a test.
func TestFileHelperProcess(t *testing.T) {
	u := os.Getenv(helperURLEnv)
	if u == "" {
		t.Skip("a helper process of TestFileCrossProcessCreateOnly")
	}
	s, err := statestore.Open(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatalf("waiting for go: %v", err)
	}
	id := []byte(os.Getenv(helperIDEnv))
	for r := range raceRounds {
		p := fmt.Sprintf("race/%d", r)
		_, err := s.Put(t.Context(), p, id, statestore.PutOptions{IfNoneMatch: true})
		switch {
		case err == nil:
			fmt.Println("created", p)
		case !errors.Is(err, statestore.ErrPreconditionFailed):
			t.Fatal(err)
		}
	}
}
