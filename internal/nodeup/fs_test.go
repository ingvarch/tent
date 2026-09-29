package nodeup_test

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/secrettest"
)

// fsCase is a filesystem that the contract tests run on.
type fsCase struct {
	name  string
	fs    nodeup.FS
	owner string // an owner that the tests' files can have
	meta  bool   // whether the filesystem sets and compares modes and owners
}

// filesystems returns a fresh OSFS in a temporary directory and a fresh in-memory FS. OSFS sets no modes and owners
// on Windows.
func filesystems(t *testing.T) []fsCase {
	t.Helper()
	return []fsCase{
		{"OSFS", nodeup.OSFS{Root: t.TempDir()}, localOwner(t), runtime.GOOS != "windows"},
		{"nodeuptest.FS", nodeuptest.NewFS(), "root:root", true},
	}
}

// localOwner returns the user and primary group of the test process as user:group, which OSFS can give a file
// without root. Windows has no such owners, and OSFS ignores them there.
func localOwner(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "tester:testers"
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return u.Username + ":" + g.Name
}

// write calls WriteFile and fails t on an error or when changed is not want.
func write(t *testing.T, fsys nodeup.FS, path, data string, mode fs.FileMode, owner string, want bool) {
	t.Helper()
	changed, err := fsys.WriteFile(path, []byte(data), mode, owner)
	if err != nil || changed != want {
		t.Fatalf("WriteFile(%s, %q, %#o): changed %v, err %v; want changed %v", path, data, mode, changed, err, want)
	}
}

// ensureDir calls EnsureDir and fails t on an error or when changed is not want.
func ensureDir(t *testing.T, fsys nodeup.FS, path string, mode fs.FileMode, owner string, want bool) {
	t.Helper()
	changed, err := fsys.EnsureDir(path, mode, owner)
	if err != nil || changed != want {
		t.Fatalf("EnsureDir(%s, %#o): changed %v, err %v; want changed %v", path, mode, changed, err, want)
	}
}

// wantFile fails t unless path is a file with data and, where the filesystem keeps modes, mode.
func wantFile(t *testing.T, c fsCase, path, data string, mode fs.FileMode) {
	t.Helper()
	got, err := c.fs.ReadFile(path)
	if err != nil || string(got) != data {
		t.Errorf("ReadFile(%s) = %q, %v; want %q", path, got, err, data)
	}
	fi, err := c.fs.Stat(path)
	switch {
	case err != nil:
		t.Errorf("Stat(%s): %v", path, err)
	case fi.IsDir() || fi.Size() != int64(len(data)):
		t.Errorf("Stat(%s): a directory %v of %d bytes, want a file of %d", path, fi.IsDir(), fi.Size(), len(data))
	case c.meta && fi.Mode().Perm() != mode:
		t.Errorf("Stat(%s): mode %#o, want %#o", path, fi.Mode().Perm(), mode)
	}
}

// wantDir fails t unless path is a directory with, where the filesystem keeps modes, mode.
func wantDir(t *testing.T, c fsCase, path string, mode fs.FileMode) {
	t.Helper()
	fi, err := c.fs.Stat(path)
	switch {
	case err != nil:
		t.Errorf("Stat(%s): %v", path, err)
	case !fi.IsDir():
		t.Errorf("Stat(%s): not a directory", path)
	case c.meta && fi.Mode().Perm() != mode:
		t.Errorf("Stat(%s): mode %#o, want %#o", path, fi.Mode().Perm(), mode)
	}
}

func TestFSWriteFile(t *testing.T) {
	for _, c := range filesystems(t) {
		t.Run(c.name, func(t *testing.T) {
			write(t, c.fs, "/a.conf", "one\n", 0o644, c.owner, true)
			wantFile(t, c, "/a.conf", "one\n", 0o644)
			write(t, c.fs, "/a.conf", "one\n", 0o644, c.owner, false)
			write(t, c.fs, "/a.conf", "two\n", 0o644, c.owner, true)
			wantFile(t, c, "/a.conf", "two\n", 0o644)
			write(t, c.fs, "/a.conf", "", 0o644, c.owner, true)
			wantFile(t, c, "/a.conf", "", 0o644)
			if c.meta {
				write(t, c.fs, "/a.conf", "", 0o600, c.owner, true)
				wantFile(t, c, "/a.conf", "", 0o600)
				write(t, c.fs, "/a.conf", "", 0o600, c.owner, false)
			}
		})
	}
}

func TestFSEnsureDir(t *testing.T) {
	for _, c := range filesystems(t) {
		t.Run(c.name, func(t *testing.T) {
			ensureDir(t, c.fs, "/etc", 0o755, c.owner, true)
			wantDir(t, c, "/etc", 0o755)
			ensureDir(t, c.fs, "/etc", 0o755, c.owner, false)
			if c.meta {
				// An existing directory gets the mode it should have, such as a client's state directory.
				ensureDir(t, c.fs, "/etc", 0o700, c.owner, true)
				wantDir(t, c, "/etc", 0o700)
				ensureDir(t, c.fs, "/etc", 0o700, c.owner, false)
			}
			write(t, c.fs, "/etc/a.conf", "a\n", 0o644, c.owner, true)
			wantFile(t, c, "/etc/a.conf", "a\n", 0o644)
		})
	}
}

func TestFSRemove(t *testing.T) {
	for _, c := range filesystems(t) {
		t.Run(c.name, func(t *testing.T) {
			ensureDir(t, c.fs, "/etc", 0o755, c.owner, true)
			write(t, c.fs, "/etc/a.conf", "a\n", 0o644, c.owner, true)
			if removed, err := c.fs.Remove("/etc"); err == nil || removed {
				t.Errorf("Remove of a directory with a file: removed %v, err %v; want an error", removed, err)
			}
			for _, path := range []string{"/etc/a.conf", "/etc"} {
				if removed, err := c.fs.Remove(path); err != nil || !removed {
					t.Errorf("Remove(%s): removed %v, err %v; want it removed", path, removed, err)
				}
				if _, err := c.fs.Stat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("Stat(%s) after Remove: %v, want an error that matches fs.ErrNotExist", path, err)
				}
				if removed, err := c.fs.Remove(path); err != nil || removed {
					t.Errorf("Remove(%s) again: removed %v, err %v; want nothing removed and no error", path, removed,
						err)
				}
			}
		})
	}
}

func TestFSMissing(t *testing.T) {
	for _, c := range filesystems(t) {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.fs.ReadFile("/missing"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("ReadFile of a missing file: %v, want an error that matches fs.ErrNotExist", err)
			}
			if _, err := c.fs.Stat("/missing"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Stat of a missing file: %v, want an error that matches fs.ErrNotExist", err)
			}
			if _, err := c.fs.WriteFile("/missing/a.conf", nil, 0o644, c.owner); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("WriteFile into a missing directory: %v, want an error that matches fs.ErrNotExist", err)
			}
			if _, err := c.fs.EnsureDir("/missing/d", 0o755, c.owner); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("EnsureDir in a missing directory: %v, want an error that matches fs.ErrNotExist", err)
			}
		})
	}
}

func TestFSRefuses(t *testing.T) {
	// The data is a secret: no error shows it.
	data := []byte("secret-gossip-key-stand-in-5b0e7c2a91d4f3e8")
	for _, c := range filesystems(t) {
		t.Run(c.name, func(t *testing.T) {
			ensureDir(t, c.fs, "/d", 0o755, c.owner, true)
			write(t, c.fs, "/f", "f\n", 0o644, c.owner, true)
			cases := []struct {
				name string
				call func() (bool, error)
			}{
				{"a relative path", func() (bool, error) { return c.fs.WriteFile("a.conf", data, 0o600, c.owner) }},
				{"a path that is not clean", func() (bool, error) {
					return c.fs.WriteFile("/d/../a.conf", data, 0o600, c.owner)
				}},
				{"a file onto a directory", func() (bool, error) { return c.fs.WriteFile("/d", data, 0o600, c.owner) }},
				{"a file in a file", func() (bool, error) { return c.fs.WriteFile("/f/a.conf", data, 0o600, c.owner) }},
				{"a mode beyond 0777", func() (bool, error) { return c.fs.WriteFile("/a", data, 0o4755, c.owner) }},
				{"an owner without a group", func() (bool, error) { return c.fs.WriteFile("/a", data, 0o600, "root") }},
				{"an owner without a user", func() (bool, error) { return c.fs.WriteFile("/a", data, 0o600, ":root") }},
				{"a directory onto a file", func() (bool, error) { return c.fs.EnsureDir("/f", 0o755, c.owner) }},
				{"a directory with a relative path", func() (bool, error) { return c.fs.EnsureDir("d", 0o755, c.owner) }},
				{"a directory with a mode beyond 0777", func() (bool, error) {
					return c.fs.EnsureDir("/e", 0o1777, c.owner)
				}},
				{"removing a relative path", func() (bool, error) { return c.fs.Remove("f") }},
			}
			for _, cc := range cases {
				changed, err := cc.call()
				switch {
				case err == nil || changed:
					t.Errorf("%s: changed %v, err %v; want an error and no change", cc.name, changed, err)
				case secrettest.Shows(err.Error(), data):
					t.Errorf("%s: the error shows the data", cc.name)
				}
			}
			for _, path := range []string{"/a", "/e", "/a.conf"} {
				if _, err := c.fs.Stat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("Stat(%s) after the refusals: %v, want it missing", path, err)
				}
			}
			wantFile(t, c, "/f", "f\n", 0o644)
			if _, err := c.fs.ReadFile("/d"); err == nil {
				t.Error("ReadFile of a directory: no error")
			}
		})
	}
}

func TestOSFSWritesUnderRoot(t *testing.T) {
	root := t.TempDir()
	fsys := nodeup.OSFS{Root: root}
	ensureDir(t, fsys, "/etc", 0o755, localOwner(t), true)
	write(t, fsys, "/etc/a.conf", "a\n", 0o644, localOwner(t), true)
	if got, err := os.ReadFile(filepath.Join(root, "etc", "a.conf")); err != nil || string(got) != "a\n" {
		t.Errorf("the file under the root holds %q, %v; want %q", got, err, "a\n")
	}
}

func TestOSFSReplaceLeavesNoTemporaryFile(t *testing.T) {
	// WriteFile writes a new file beside the old one and renames it over the old, so only the file is left.
	root := t.TempDir()
	fsys, owner := nodeup.OSFS{Root: root}, localOwner(t)
	write(t, fsys, "/a.conf", "one\n", 0o644, owner, true)
	write(t, fsys, "/a.conf", "two\n", 0o644, owner, true)
	if got, err := os.ReadFile(filepath.Join(root, "a.conf")); err != nil || string(got) != "two\n" {
		t.Errorf("the replaced file holds %q, %v; want %q", got, err, "two\n")
	}
	wantOnly(t, root, "a.conf")
}

// wantOnly fails t unless dir holds exactly the named entries: no temporary files are left.
func wantOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	slices.Sort(names)
	if !slices.Equal(got, names) {
		t.Errorf("%s holds %q, want %q", dir, got, names)
	}
}
