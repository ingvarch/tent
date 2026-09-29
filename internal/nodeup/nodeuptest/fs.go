// Package nodeuptest holds fakes of a machine for the tests of tent-node: an in-memory filesystem, a runner that
// answers commands from a script, and a host built on them.
package nodeuptest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
)

// FS is an in-memory filesystem that behaves as nodeup.OSFS does on Linux. It starts with the root directory alone,
// with mode 0755 and owned by root:root. It records the paths that WriteFile, EnsureDir and Remove change, and Fail
// makes those calls fail for a path. It is safe for concurrent use.
type FS struct {
	mu      sync.Mutex
	entries map[string]Entry // by path
	changes []string
	faults  map[string]error // by path
}

// Entry is a file or a directory of an FS.
type Entry struct {
	Dir   bool
	Data  []byte      // a file's content
	Mode  fs.FileMode // permission bits
	Owner string      // user:group
}

// errIsDir is the error of a directory where a file should be, errNotDir the other way round, and errNotEmpty that of
// removing a directory that holds something.
var (
	errIsDir    = errors.New("is a directory")
	errNotDir   = errors.New("not a directory")
	errNotEmpty = errors.New("directory not empty")
)

// NewFS returns a filesystem that holds only the root directory.
func NewFS() *FS {
	return &FS{
		entries: map[string]Entry{"/": {Dir: true, Mode: 0o755, Owner: nodeconfig.Owner}},
		faults:  map[string]error{},
	}
}

// AddDir makes the directory path with mode and owner, and its missing parents with mode 0755 and owned by root:root.
// It records no change. A file in the way stops t.
func (f *FS) AddDir(t testing.TB, path string, mode fs.FileMode, owner string) {
	t.Helper()
	f.add(t, path, Entry{Dir: true, Mode: mode, Owner: owner})
}

// AddFile puts a file with data, mode and owner at path, over any file there, and makes its missing parents as AddDir
// does. It records no change. A directory in the way stops t.
func (f *FS) AddFile(t testing.TB, path string, data []byte, mode fs.FileMode, owner string) {
	t.Helper()
	f.add(t, path, Entry{Data: bytes.Clone(data), Mode: mode, Owner: owner})
}

func (f *FS) add(t testing.TB, p string, e Entry) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := check(p, e.Mode, e.Owner); err != nil {
		t.Fatalf("nodeuptest: add %s: %v", p, err)
		return
	}
	var parents []string
	for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
		parents = append(parents, dir)
	}
	for _, dir := range slices.Backward(parents) {
		if old, ok := f.entries[dir]; ok && !old.Dir {
			t.Fatalf("nodeuptest: add %s: %s is a file", p, dir)
			return
		}
	}
	if old, ok := f.entries[p]; ok && old.Dir != e.Dir {
		kind := "a file"
		if old.Dir {
			kind = "a directory"
		}
		t.Fatalf("nodeuptest: add %s: %s is there", p, kind)
		return
	}
	for _, dir := range parents {
		if _, ok := f.entries[dir]; !ok {
			f.entries[dir] = Entry{Dir: true, Mode: 0o755, Owner: nodeconfig.Owner}
		}
	}
	f.entries[p] = e
}

// Entry returns a copy of the file or directory at path.
func (f *FS) Entry(path string) (Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[path]
	e.Data = bytes.Clone(e.Data)
	return e, ok
}

// Paths returns the paths of every file and directory, sorted.
func (f *FS) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.entries))
}

// Changes returns the paths that WriteFile, EnsureDir and Remove changed, in order, once for each call that changed
// one.
func (f *FS) Changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.changes)
}

// Fail makes every WriteFile, EnsureDir and Remove of path fail with an error that wraps err, and change nothing. A
// nil err ends that.
func (f *FS) Fail(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.faults, path)
		return
	}
	f.faults[path] = err
}

// ReadFile returns a copy of the content of the file at p.
func (f *FS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.lookup("open", p)
	if err != nil {
		return nil, err
	}
	if e.Dir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: errIsDir}
	}
	return bytes.Clone(e.Data), nil
}

// Stat describes the file or the directory at p. It has no modification time.
func (f *FS) Stat(p string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.lookup("stat", p)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: path.Base(p), entry: e}, nil
}

// WriteFile makes p a file with data, mode and owner, unless it has them already.
func (f *FS) WriteFile(p string, data []byte, mode fs.FileMode, owner string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.prepare(p, mode, owner); err != nil {
		return false, fmt.Errorf("write %s: %w", p, err)
	}
	old, ok := f.entries[p]
	switch {
	case ok && old.Dir:
		return false, fmt.Errorf("write %s: %w", p, errIsDir)
	case ok && bytes.Equal(old.Data, data) && old.Mode == mode && old.Owner == owner:
		return false, nil
	}
	f.change(p, Entry{Data: bytes.Clone(data), Mode: mode, Owner: owner})
	return true, nil
}

// EnsureDir makes p a directory with mode and owner, unless it is one already.
func (f *FS) EnsureDir(p string, mode fs.FileMode, owner string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.prepare(p, mode, owner); err != nil {
		return false, fmt.Errorf("make directory %s: %w", p, err)
	}
	old, ok := f.entries[p]
	switch {
	case ok && !old.Dir:
		return false, fmt.Errorf("make directory %s: %w", p, errNotDir)
	case ok && old.Mode == mode && old.Owner == owner:
		return false, nil
	}
	f.change(p, Entry{Dir: true, Mode: mode, Owner: owner})
	return true, nil
}

// Remove removes the file or the empty directory at p, if there is one.
func (f *FS) Remove(p string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := nodeup.CheckPath(p); err != nil {
		return false, err
	}
	if err := f.faults[p]; err != nil {
		return false, &fs.PathError{Op: "remove", Path: p, Err: err}
	}
	if _, ok := f.entries[p]; !ok {
		return false, nil
	}
	within := strings.TrimSuffix(p, "/") + "/"
	for other := range f.entries {
		if other != p && strings.HasPrefix(other, within) {
			return false, &fs.PathError{Op: "remove", Path: p, Err: errNotEmpty}
		}
	}
	delete(f.entries, p)
	f.changes = append(f.changes, p)
	return true, nil
}

// lookup returns the entry at p, or an error for op that matches fs.ErrNotExist when there is none.
func (f *FS) lookup(op, p string) (Entry, error) {
	if err := nodeup.CheckPath(p); err != nil {
		return Entry{}, err
	}
	e, ok := f.entries[p]
	if !ok {
		return Entry{}, &fs.PathError{Op: op, Path: p, Err: fs.ErrNotExist}
	}
	return e, nil
}

// prepare checks the arguments of a change of p, that no fault is set for p, and that p's directory exists.
func (f *FS) prepare(p string, mode fs.FileMode, owner string) error {
	if err := check(p, mode, owner); err != nil {
		return err
	}
	if err := f.faults[p]; err != nil {
		return err
	}
	dir := path.Dir(p)
	switch e, ok := f.entries[dir]; {
	case !ok:
		return fmt.Errorf("directory %s: %w", dir, fs.ErrNotExist)
	case !e.Dir:
		return fmt.Errorf("directory %s: %w", dir, errNotDir)
	}
	return nil
}

// change puts e at p and records the change.
func (f *FS) change(p string, e Entry) {
	f.entries[p] = e
	f.changes = append(f.changes, p)
}

// check checks a path, a mode and an owner as nodeup.OSFS does.
func check(p string, mode fs.FileMode, owner string) error {
	if err := nodeup.CheckPath(p); err != nil {
		return err
	}
	if err := nodeup.CheckMode(mode); err != nil {
		return err
	}
	_, _, err := nodeup.SplitOwner(owner)
	return err
}

// fileInfo describes an entry of an FS.
type fileInfo struct {
	name  string
	entry Entry
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return int64(len(i.entry.Data)) }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.entry.Dir }
func (i fileInfo) Sys() any           { return nil }

func (i fileInfo) Mode() fs.FileMode {
	if i.entry.Dir {
		return fs.ModeDir | i.entry.Mode
	}
	return i.entry.Mode
}
