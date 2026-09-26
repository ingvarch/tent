package statestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

const (
	lockFile   = ".tent-store.lock"
	tmpPattern = ".tent-tmp-*"
	lockRetry  = 10 * time.Millisecond
	// Secrets live in the store: only the owner may read it.
	dirPerm  = 0o700
	filePerm = 0o600
)

// fileStore keeps objects as files under a local directory. Every operation holds the store lock, a file lock on
// <root>/.tent-store.lock: shared to read, exclusive to write or delete. It makes conditional puts atomic between
// processes and keeps readers' open files away from renames, which Windows refuses over an open file.
type fileStore struct {
	root string // absolute and clean
	url  string
}

func newFileStore(u *url.URL) (*fileStore, error) {
	switch {
	case u.Host != "":
		return nil, errors.New("state store URL: a file URL has no host: use file:///abs/path (three slashes)")
	case u.RawQuery != "":
		return nil, errors.New("state store URL: a file URL takes no query")
	case u.Fragment != "":
		return nil, errors.New("state store URL: a file URL takes no fragment")
	}
	root := localPath(u)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("state store URL: the path %q is not absolute: use file:///abs/path", root)
	}
	root = filepath.Clean(root)
	if filepath.Dir(root) == root {
		return nil, fmt.Errorf("state store URL: %q is the root of a disk: name a directory", root)
	}
	p := filepath.ToSlash(root)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path
	}
	return &fileStore{root: root, url: (&url.URL{Scheme: "file", Path: p}).String()}, nil
}

// localPath returns the OS path of a file URL: /C:/state is C:\state on Windows.
func localPath(u *url.URL) string {
	if u.Opaque != "" {
		return filepath.FromSlash(u.Opaque) // file:relative/path
	}
	p := u.Path
	if rest, ok := strings.CutPrefix(p, "/"); ok && filepath.VolumeName(filepath.FromSlash(rest)) != "" {
		p = rest
	}
	return filepath.FromSlash(p)
}

func (s *fileStore) Get(ctx context.Context, p string) ([]byte, Version, error) {
	var data []byte
	err := s.withLock(ctx, false, func() (err error) {
		data, err = s.read(p)
		return err
	})
	if errors.Is(err, errNoRoot) {
		err = ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return data, versionOf(data), nil
}

func (s *fileStore) Put(ctx context.Context, p string, data []byte, opts PutOptions) (Version, error) {
	if err := mkdirAll(s.root); err != nil {
		return "", err
	}
	err := s.withLock(ctx, true, func() error {
		if err := s.check(p, opts); err != nil {
			return err
		}
		return s.write(p, data)
	})
	if err != nil {
		return "", err
	}
	return versionOf(data), nil
}

func (s *fileStore) List(ctx context.Context, prefix string) ([]string, error) {
	var paths []string
	err := s.withLock(ctx, false, func() (err error) {
		paths, err = s.walk(prefix)
		return err
	})
	if errors.Is(err, errNoRoot) {
		return nil, nil
	}
	return paths, err
}

func (s *fileStore) Delete(ctx context.Context, p string) error {
	err := s.withLock(ctx, true, func() error { return s.remove(p) })
	if errors.Is(err, errNoRoot) {
		return nil
	}
	return err
}

func (s *fileStore) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{ConditionalPut: true}, nil
}

func (s *fileStore) String() string { return s.url }

// errNoRoot means the root does not exist yet, so the store is empty.
var errNoRoot = errors.New("the store directory does not exist")

// withLock runs fn holding the store lock: shared for a read, exclusive for a write. Each call opens its own handle:
// on a local disk the OS excludes two handles even within one process, while one shared handle would not exclude
// goroutines. Network file systems such as NFS lock per process, so there goroutines would not exclude each other.
func (s *fileStore) withLock(ctx context.Context, exclusive bool, fn func() error) (err error) {
	lk := flock.New(filepath.Join(s.root, lockFile), flock.SetPermissions(filePerm))
	try := lk.TryRLockContext
	if exclusive {
		try = lk.TryLockContext
	}
	if _, err := try(ctx, lockRetry); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errNoRoot
		}
		return fmt.Errorf("lock the store: %w", err)
	}
	defer func() {
		if uerr := lk.Unlock(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unlock the store: %w", uerr))
		}
	}()
	return fn()
}

// file returns the OS path of an object.
func (s *fileStore) file(p string) string { return filepath.Join(s.root, filepath.FromSlash(p)) }

// read returns an object's content, or ErrNotFound.
func (s *fileStore) read(p string) ([]byte, error) {
	f, err := os.Open(s.file(p))
	if isMissing(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // nothing to flush after a read
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotFound // a directory holds other objects
	}
	return io.ReadAll(f)
}

// check returns an error wrapping ErrPreconditionFailed when the object does not meet the condition in opts.
func (s *fileStore) check(p string, opts PutOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return nil
	}
	data, err := s.read(p)
	switch {
	case errors.Is(err, ErrNotFound) && opts.IfMatch != "":
		return fmt.Errorf("%w: the object does not exist", ErrPreconditionFailed)
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case opts.IfNoneMatch:
		return fmt.Errorf("%w: the object exists", ErrPreconditionFailed)
	case versionOf(data) != opts.IfMatch:
		return fmt.Errorf("%w: the object has changed", ErrPreconditionFailed)
	}
	return nil
}

// write replaces an object atomically and durably: through a synced temp file in the same directory, renamed over
// the object, then the directory synced.
func (s *fileStore) write(p string, data []byte) error {
	full := s.file(p)
	dir := filepath.Dir(full)
	if err := mkdirAll(dir); err != nil {
		return s.explain(p, err)
	}
	tmp, err := os.CreateTemp(dir, tmpPattern) // mode 0600
	if err != nil {
		return s.explain(p, err)
	}
	err = writeAndClose(tmp, data)
	if err == nil {
		err = os.Rename(tmp.Name(), full)
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) // the write error matters more; List skips a leftover temp file
		return s.explain(p, err)
	}
	return syncDir(dir)
}

// explain replaces the OS error of a failed write when the path clashes with another object: a file system cannot
// hold both "a" and "a/b", which object stores allow. Other errors pass unchanged.
func (s *fileStore) explain(p string, err error) error {
	if fi, serr := os.Stat(s.file(p)); serr == nil && fi.IsDir() {
		return fmt.Errorf("%q holds other objects", p)
	}
	for i := range len(p) {
		if p[i] != '/' {
			continue
		}
		if fi, serr := os.Stat(s.file(p[:i])); serr == nil && !fi.IsDir() {
			return fmt.Errorf("%q is an object, so it cannot hold %q", p[:i], p)
		}
	}
	return err
}

// mkdirAll creates dir and its missing parents, and syncs the parent of each new directory.
func mkdirAll(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// syncDir writes a directory's entries to disk, so a rename or a new entry in it survives a power loss. Windows
// cannot sync a directory, so there it does nothing.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// writeAndClose writes data to f, syncs it to disk and closes it.
func writeAndClose(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// walk returns the sorted paths of the objects that start with prefix.
func (s *fileStore) walk(prefix string) ([]string, error) {
	// Walk only the directory the prefix names in full: "prod/no" walks prod. fs.WalkDir stats its start, so it follows
	// a symlinked root where filepath.WalkDir would not, and it yields slash paths relative to the root.
	start := "."
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = prefix[:i]
	}
	var paths []string
	err := fs.WalkDir(os.DirFS(s.root), start, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && p == start && isMissing(err):
			return fs.SkipAll // nothing stored under the prefix
		case err != nil:
			return err
		case p != start && badSegment(d.Name()) != "": // the backend's own files, or foreign ones
			if d.IsDir() {
				return fs.SkipDir
			}
		case d.Type().IsRegular() && strings.HasPrefix(p, prefix):
			paths = append(paths, p)
		}
		return nil
	})
	slices.Sort(paths)
	return paths, err
}

// remove deletes an object's file, then the directories it leaves empty, up to the root.
func (s *fileStore) remove(p string) error {
	full := s.file(p)
	fi, err := os.Stat(full)
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return nil // a directory holds other objects
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	for _, dir := range parentsBelowRoot(s.root, full) {
		empty, err := isEmptyDir(dir)
		if err != nil || !empty {
			return err
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

// parentsBelowRoot returns the directories that hold the file full, nearest first, up to but not including root.
func parentsBelowRoot(root, full string) []string {
	var dirs []string
	for dir := filepath.Dir(full); dir != root && len(dir) > len(root); dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
	}
	return dirs
}

func isEmptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // nothing to flush after a read
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

func versionOf(data []byte) Version {
	sum := sha256.Sum256(data)
	return Version(hex.EncodeToString(sum[:]))
}

// isMissing reports whether err means a path does not exist, also when a file stands where a directory would be.
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
