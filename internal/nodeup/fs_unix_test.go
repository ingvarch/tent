//go:build unix

package nodeup_test

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/secrettest"
)

func TestOSFSModeIgnoresTheUmask(t *testing.T) {
	// The umask is the process's; the tests of this package do not run in parallel.
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	root := t.TempDir()
	c := fsCase{"OSFS", nodeup.OSFS{Root: root}, localOwner(t), true}
	ensureDir(t, c.fs, "/etc", 0o755, c.owner, true)
	wantDir(t, c, "/etc", 0o755)
	write(t, c.fs, "/etc/a.conf", "a\n", 0o644, c.owner, true)
	wantFile(t, c, "/etc/a.conf", "a\n", 0o644)
}

// groupOf returns the id of the group that owns the file at path.
func groupOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Gid)
}

func TestOSFSSetsTheOwner(t *testing.T) {
	// Without root, a file can move only between the groups of its owner, so the test needs two of them.
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	var groups []*user.Group
	for _, gid := range gids {
		if g, err := user.LookupGroupId(gid); err == nil && !strings.Contains(g.Name, ":") {
			groups = append(groups, g)
		}
	}
	if len(groups) < 2 {
		t.Skipf("user %s has %d named groups, the test needs two", u.Username, len(groups))
	}
	root := t.TempDir()
	fsys := nodeup.OSFS{Root: root}
	for _, g := range groups[:2] {
		owner := u.Username + ":" + g.Name
		want, err := strconv.Atoi(g.Gid)
		if err != nil {
			t.Fatal(err)
		}
		write(t, fsys, "/a.conf", "a\n", 0o644, owner, true)
		write(t, fsys, "/a.conf", "a\n", 0o644, owner, false)
		ensureDir(t, fsys, "/d", 0o755, owner, true)
		ensureDir(t, fsys, "/d", 0o755, owner, false)
		for _, name := range []string{"a.conf", "d"} {
			if got := groupOf(t, filepath.Join(root, name)); got != want {
				t.Errorf("%s belongs to the group %d, want %s (%d)", name, got, g.Name, want)
			}
		}
	}
}

func TestOSFSReplacesAFileAtOnce(t *testing.T) {
	// A reader sees the old file or the new one: WriteFile writes a new file beside it and renames it over the old.
	// Only on Unix: on Windows, os.SameFile opens the path again and so compares the new file with itself.
	root := t.TempDir()
	fsys, owner := nodeup.OSFS{Root: root}, localOwner(t)
	write(t, fsys, "/a.conf", "one\n", 0o644, owner, true)
	before, err := os.Stat(filepath.Join(root, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, fsys, "/a.conf", "two\n", 0o644, owner, true)
	after, err := os.Stat(filepath.Join(root, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("WriteFile rewrote the file in place, want a new file renamed over it")
	}
}

func TestOSFSFailedWriteKeepsTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can give a file to root")
	}
	root := t.TempDir()
	fsys, owner := nodeup.OSFS{Root: root}, localOwner(t)
	write(t, fsys, "/a.conf", "one\n", 0o600, owner, true)
	secret := []byte("secret-node-key-stand-in-0c4e8a1f7b3d9265")
	// Only root may give a file to root, so the write fails after it made its temporary file.
	_, group, _ := strings.Cut(owner, ":")
	changed, err := fsys.WriteFile("/a.conf", secret, 0o600, "root:"+group)
	switch {
	case err == nil || changed:
		t.Fatalf("WriteFile as root:%s without root: changed %v, err %v; want an error", group, changed, err)
	case secrettest.Shows(err.Error(), secret):
		t.Error("the error shows the data")
	}
	if got, err := os.ReadFile(filepath.Join(root, "a.conf")); err != nil || string(got) != "one\n" {
		t.Errorf("after the failed write the file holds %q, %v; want the old content", got, err)
	}
	wantOnly(t, root, "a.conf")
	// An owner that does not exist fails before anything is written.
	if _, err := fsys.WriteFile("/b.conf", secret, 0o600, "tent-no-such-user:tent-no-such-group"); err == nil {
		t.Error("WriteFile with an owner that does not exist: no error")
	}
	wantOnly(t, root, "a.conf")
}

// symlink makes a symbolic link at link that points at target.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// lmode returns the mode of name, without following a link.
func lmode(t *testing.T, name string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// wantRegular fails t unless name is a regular file, not a link, that holds data.
func wantRegular(t *testing.T, name, data string) {
	t.Helper()
	if mode := lmode(t, name); !mode.IsRegular() {
		t.Fatalf("%s has the mode %v, want a regular file", name, mode)
	}
	if got, err := os.ReadFile(name); err != nil || string(got) != data {
		t.Errorf("%s holds %q, %v; want %q", name, got, err, data)
	}
}

func TestOSFSReplacesASymbolicLink(t *testing.T) {
	// tent owns the path: WriteFile puts a file there and leaves whatever a link points at alone.
	root := t.TempDir()
	fsys, owner := nodeup.OSFS{Root: root}, localOwner(t)
	target := filepath.Join(root, "target.conf")
	write(t, fsys, "/target.conf", "a\n", 0o644, owner, true)

	// A link to a file with the same data, mode and owner is not the file.
	symlink(t, target, filepath.Join(root, "same.conf"))
	write(t, fsys, "/same.conf", "a\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "same.conf"), "a\n")

	symlink(t, target, filepath.Join(root, "other.conf"))
	write(t, fsys, "/other.conf", "b\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "other.conf"), "b\n")
	wantRegular(t, target, "a\n")

	missing := filepath.Join(root, "missing.conf")
	symlink(t, missing, filepath.Join(root, "dangling.conf"))
	write(t, fsys, "/dangling.conf", "c\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "dangling.conf"), "c\n")
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the target of the dangling link: %v, want it still missing", err)
	}
}

func TestOSFSEnsureDirRefusesASymbolicLink(t *testing.T) {
	root := t.TempDir()
	fsys, owner := nodeup.OSFS{Root: root}, localOwner(t)
	ensureDir(t, fsys, "/real", 0o700, owner, true)
	symlink(t, filepath.Join(root, "real"), filepath.Join(root, "link"))
	changed, err := fsys.EnsureDir("/link", 0o755, owner)
	if want := "make directory /link: is a symbolic link"; changed || errText(err) != want {
		t.Errorf("EnsureDir of a link to a directory: changed %v, err %q; want %q", changed, errText(err), want)
	}
	c := fsCase{"OSFS", fsys, owner, true}
	wantDir(t, c, "/real", 0o700)
}

func TestOSFSClearsSpecialModeBits(t *testing.T) {
	// The mode is the permission bits alone: setuid, setgid and sticky bits make a file or a directory differ, and
	// the rewrite clears them.
	cases := []struct {
		name string
		bit  fs.FileMode
		dir  bool
	}{
		{"setuid file", fs.ModeSetuid, false},
		{"setgid file", fs.ModeSetgid, false},
		{"setgid directory", fs.ModeSetgid, true},
		{"sticky directory", fs.ModeSticky, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			fsc := fsCase{"OSFS", nodeup.OSFS{Root: root}, localOwner(t), true}
			apply := func(want bool) {
				t.Helper()
				if c.dir {
					ensureDir(t, fsc.fs, "/x", 0o755, fsc.owner, want)
				} else {
					write(t, fsc.fs, "/x", "x\n", 0o755, fsc.owner, want)
				}
			}
			apply(true)
			name := filepath.Join(root, "x")
			if err := os.Chmod(name, 0o755|c.bit); err != nil {
				t.Fatal(err)
			}
			if mode := lmode(t, name); mode&c.bit == 0 {
				t.Fatalf("%s has the mode %v, want the bit %v set", name, mode, c.bit)
			}
			apply(true)
			if mode := lmode(t, name); mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 || mode.Perm() != 0o755 {
				t.Errorf("%s has the mode %v after the rewrite, want 0755 alone", name, mode)
			}
			apply(false)
		})
	}
}

func TestOSFSSetsTheModeOfADirectoryOnly(t *testing.T) {
	// EnsureDir checks the directory, then sets its mode and owner. A link or a file that took the directory's
	// place in between is neither followed nor changed.
	root := t.TempDir()
	owner := localOwner(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(t, target, filepath.Join(root, "link"))
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(root, "link"), file} {
		if err := nodeup.SetDirMeta(name, 0o755, owner); err == nil {
			t.Errorf("SetDirMeta(%s): no error, want a refusal", name)
		}
	}
	for name, want := range map[string]fs.FileMode{target: 0o700, file: 0o600} {
		if mode := lmode(t, name); mode.Perm() != want {
			t.Errorf("%s has the mode %v, want %#o", name, mode, want)
		}
	}
}
