package nodeup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FS is the filesystem of the machine. Paths are absolute, clean and use slashes, as on Linux. Modes are permission
// bits alone, such as 0o644, and owners are user:group names, such as root:root. Errors never show a file's data.
type FS interface {
	// ReadFile returns the content of a file. A missing file gives an error that matches fs.ErrNotExist.
	ReadFile(path string) ([]byte, error)
	// Open opens a file to read it at any offset, so that a large file is never held in memory whole. A missing file
	// gives an error that matches fs.ErrNotExist; a directory fails.
	Open(path string) (File, error)
	// Stat describes a file or a directory. A missing one gives an error that matches fs.ErrNotExist.
	Stat(path string) (fs.FileInfo, error)
	// WriteFile makes path a file with data, mode and owner, and reports whether it changed anything: it does nothing
	// when the file has them already. Otherwise it replaces the file at once, so that a reader sees either the old
	// file or the new one, and a crash leaves one of them. The directory must exist.
	WriteFile(path string, data []byte, mode fs.FileMode, owner string) (changed bool, err error)
	// WriteStream is WriteFile for content that is read from r, so that a large file is never held in memory whole.
	WriteStream(path string, r io.Reader, mode fs.FileMode, owner string) (changed bool, err error)
	// HasContent reports whether path is a regular file of size bytes with the sha256 sum, mode and owner: one that
	// WriteStream of that content leaves as it is. It reads the file as a stream. A missing file has not.
	HasContent(path string, size int64, sum []byte, mode fs.FileMode, owner string) (bool, error)
	// EnsureDir makes path a directory with mode and owner, and reports whether it changed anything: it creates the
	// directory, or gives one that exists the mode and owner. The parent directory must exist.
	EnsureDir(path string, mode fs.FileMode, owner string) (changed bool, err error)
	// Remove removes a file or an empty directory, and reports whether there was one.
	Remove(path string) (removed bool, err error)
}

// File is a file that FS.Open opened: it reads at any offset, gives its size through Stat, and must be closed.
type File interface {
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// readerOf returns a reader of the whole content of f, which also reads at any offset and knows its size.
func readerOf(f File) (*io.SectionReader, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(f, 0, fi.Size()), nil
}

// CheckPath checks that p is a path that FS takes: absolute, clean, with slashes.
func CheckPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("path %q is not absolute and clean", p)
	}
	return nil
}

// CheckMode checks that mode is permission bits alone, as FS takes them.
func CheckMode(mode fs.FileMode) error {
	if mode&^fs.ModePerm != 0 {
		return fmt.Errorf("mode %#o has bits beyond the permissions 0777", uint32(mode))
	}
	return nil
}

// SplitOwner returns the user and the group of an owner that FS takes, user:group, where neither is empty.
func SplitOwner(owner string) (user, group string, err error) {
	user, group, ok := strings.Cut(owner, ":")
	if !ok || user == "" || group == "" || strings.Contains(group, ":") {
		return "", "", fmt.Errorf("owner %q is not user:group", owner)
	}
	return user, group, nil
}

// errIsDir is the error of a directory where FS wants a file, errNotDir the other way round, and errSymlink that of a
// link where EnsureDir wants a directory.
var (
	errIsDir   = errors.New("is a directory")
	errNotDir  = errors.New("not a directory")
	errSymlink = errors.New("is a symbolic link")
)

// OSFS is the filesystem of the machine, under Root when Root is set, such as a temporary directory in tests. On
// Unix it sets and compares modes and owners, and looks owners up by name. On other systems it neither sets nor
// compares them, and WriteFile and WriteStream compare only the data.
type OSFS struct {
	Root string
}

// real returns where the path p is on disk.
func (o OSFS) real(p string) (string, error) {
	if err := CheckPath(p); err != nil {
		return "", err
	}
	return filepath.Join(o.Root, filepath.FromSlash(p)), nil
}

// ReadFile returns the content of the file at p.
func (o OSFS) ReadFile(p string) ([]byte, error) {
	name, err := o.real(p)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// Open opens the file at p, following symbolic links.
func (o OSFS) Open(p string) (File, error) {
	name, err := o.real(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close() // read-only; the error to report is Stat's
		return nil, err
	}
	if fi.IsDir() {
		_ = f.Close() // read-only, and refused
		return nil, &fs.PathError{Op: "open", Path: p, Err: errIsDir}
	}
	return f, nil
}

// Stat describes the file or directory at p, following symbolic links.
func (o OSFS) Stat(p string) (fs.FileInfo, error) {
	name, err := o.real(p)
	if err != nil {
		return nil, err
	}
	return os.Stat(name)
}

// WriteFile makes p a file with data, mode and owner. It writes a temporary file beside it, flushes it to disk and
// renames it over the old one; first it removes the temporary files of p that a killed write left.
func (o OSFS) WriteFile(p string, data []byte, mode fs.FileMode, owner string) (bool, error) {
	changed, err := o.writeFile(p, data, mode, owner)
	if err != nil {
		return false, fmt.Errorf("write %s: %w", p, err)
	}
	return changed, nil
}

func (o OSFS) writeFile(p string, data []byte, mode fs.FileMode, owner string) (bool, error) {
	name, own, err := o.prepare(p, mode, owner)
	if err != nil {
		return false, err
	}
	same, err := sameFile(name, data, mode, own)
	if err != nil || same {
		return false, err
	}
	if err := replaceFile(name, data, mode, own); err != nil {
		return false, err
	}
	return true, nil
}

// WriteStream makes p a file with what r holds, mode and owner. It hashes the stream and the file as it writes a
// temporary file beside it, and does nothing when the file has the content, the mode and the owner already.
// Otherwise it flushes the temporary file to disk and renames it over the old one, as WriteFile does. On an error it
// removes the temporary file, and the old file stays. Like WriteFile, it first removes the temporary files of p that
// a killed write left.
func (o OSFS) WriteStream(p string, r io.Reader, mode fs.FileMode, owner string) (bool, error) {
	changed, err := o.writeStream(p, r, mode, owner)
	if err != nil {
		return false, fmt.Errorf("write %s: %w", p, err)
	}
	return changed, nil
}

func (o OSFS) writeStream(p string, r io.Reader, mode fs.FileMode, owner string) (bool, error) {
	name, own, err := o.prepare(p, mode, owner)
	if err != nil {
		return false, err
	}
	return streamFile(name, r, mode, own)
}

// HasContent reports whether p is a regular file, not a link, of size bytes with the sha256 sum, mode and owner.
func (o OSFS) HasContent(p string, size int64, sum []byte, mode fs.FileMode, owner string) (bool, error) {
	name, own, err := o.prepare(p, mode, owner)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p, err)
	}
	has, err := sameContent(name, size, sum, mode, own)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p, err)
	}
	return has, nil
}

// EnsureDir makes p a directory with mode and owner.
func (o OSFS) EnsureDir(p string, mode fs.FileMode, owner string) (bool, error) {
	changed, err := o.ensureDir(p, mode, owner)
	if err != nil {
		return false, fmt.Errorf("make directory %s: %w", p, err)
	}
	return changed, nil
}

func (o OSFS) ensureDir(p string, mode fs.FileMode, owner string) (bool, error) {
	name, own, err := o.prepare(p, mode, owner)
	if err != nil {
		return false, err
	}
	fi, err := os.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(name, mode); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case fi.Mode()&fs.ModeSymlink != 0:
		return false, errSymlink
	case !fi.IsDir():
		return false, errNotDir
	case sameMeta(fi, mode, own):
		return false, nil
	}
	// Mkdir applies the umask, and an existing directory may have another mode and owner.
	if err := setDirMeta(name, mode, own); err != nil {
		return false, err
	}
	return true, nil
}

// Remove removes the file or empty directory at p.
func (o OSFS) Remove(p string) (bool, error) {
	name, err := o.real(p)
	if err != nil {
		return false, err
	}
	switch err := os.Remove(name); {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// prepare checks the path and the mode, looks the owner up, and returns where the path is on disk.
func (o OSFS) prepare(p string, mode fs.FileMode, owner string) (string, ids, error) {
	name, err := o.real(p)
	if err != nil {
		return "", ids{}, err
	}
	if err := CheckMode(mode); err != nil {
		return "", ids{}, err
	}
	own, err := lookupOwner(owner)
	if err != nil {
		return "", ids{}, err
	}
	return name, own, nil
}

// sameFile reports whether name is a regular file, not a link, with data, mode and owner.
func sameFile(name string, data []byte, mode fs.FileMode, own ids) (bool, error) {
	fi, err := os.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case fi.IsDir():
		return false, errIsDir
	case !fi.Mode().IsRegular() || fi.Size() != int64(len(data)) || !sameMeta(fi, mode, own):
		return false, nil
	}
	old, err := os.ReadFile(name)
	if err != nil {
		return false, err
	}
	return bytes.Equal(old, data), nil
}

// replaceFile writes data to a temporary file beside name, with mode and owner, flushes it to disk and renames it over
// name. On an error it removes the temporary file.
func replaceFile(name string, data []byte, mode fs.FileMode, own ids) (err error) {
	dir := filepath.Dir(name)
	if err := removeTemps(name); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(name)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()      // already closed when the error came later; the first error is the one to report
			_ = os.Remove(tmp) // gone when the rename succeeded
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = setMeta(f, mode, own); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, name); err != nil {
		return err
	}
	return syncDir(dir)
}

// streamFile writes what r holds to name as replaceFile does, and reports whether name changed: when name is already
// a regular file with the same content, mode and owner, the temporary file goes and name stays. The content is
// hashed as it is copied, so neither the stream nor the file is held in memory.
func streamFile(name string, r io.Reader, mode fs.FileMode, own ids) (changed bool, err error) {
	dir := filepath.Dir(name)
	if fi, statErr := os.Lstat(name); statErr == nil && fi.IsDir() {
		return false, errIsDir
	}
	if err := removeTemps(name); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(name)+".tmp*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() {
		if err != nil || !changed {
			_ = f.Close()      // closed already when the error came later; the first error is the one to report
			_ = os.Remove(tmp) // gone when the rename succeeded
		}
	}()
	h := sha256.New()
	n, err := io.Copy(f, io.TeeReader(r, h))
	if err != nil {
		return false, err
	}
	if err = setMeta(f, mode, own); err != nil {
		return false, err
	}
	same, err := sameContent(name, n, h.Sum(nil), mode, own)
	if err != nil || same {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, name); err != nil {
		return false, err
	}
	if err = syncDir(dir); err != nil {
		return false, err
	}
	return true, nil
}

// removeTemps removes the temporary files that an earlier write of name left when it stopped before its rename, as
// on SIGKILL or a power loss: the regular files .<base>.tmp* beside name, and nothing else.
func removeTemps(name string) error {
	dir, prefix := filepath.Dir(name), "."+filepath.Base(name)+".tmp"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// sameContent reports whether name is a regular file, not a link, of size bytes with the sha256 sum, the mode and
// the owner.
func sameContent(name string, size int64, sum []byte, mode fs.FileMode, own ids) (bool, error) {
	fi, err := os.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case fi.IsDir():
		return false, errIsDir
	case !fi.Mode().IsRegular() || fi.Size() != size || !sameMeta(fi, mode, own):
		return false, nil
	}
	old, err := hashFile(name)
	if err != nil {
		return false, err
	}
	return bytes.Equal(old, sum), nil
}

// hashFile returns the sha256 of the file at name.
func hashFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
