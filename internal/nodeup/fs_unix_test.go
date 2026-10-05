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
	for i, g := range groups[:2] {
		owner := u.Username + ":" + g.Name
		want, err := strconv.Atoi(g.Gid)
		if err != nil {
			t.Fatal(err)
		}
		write(t, fsys, "/a.conf", "a\n", 0o644, owner, true)
		write(t, fsys, "/a.conf", "a\n", 0o644, owner, false)
		// The second group changes only the owner of the stream's file.
		if i > 0 && hasContent(t, fsys, "/b.bin", "b\n", 0o755, owner) {
			t.Errorf("b.bin of another group has the owner %s", owner)
		}
		writeStream(t, fsys, "/b.bin", "b\n", 0o755, owner, true)
		writeStream(t, fsys, "/b.bin", "b\n", 0o755, owner, false)
		if !hasContent(t, fsys, "/b.bin", "b\n", 0o755, owner) {
			t.Errorf("b.bin does not have the owner %s", owner)
		}
		ensureDir(t, fsys, "/d", 0o755, owner, true)
		ensureDir(t, fsys, "/d", 0o755, owner, false)
		for _, name := range []string{"a.conf", "b.bin", "d"} {
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

	// WriteStream and HasContent take a link as WriteFile does.
	symlink(t, target, filepath.Join(root, "same.bin"))
	if hasContent(t, fsys, "/same.bin", "a\n", 0o644, owner) {
		t.Error("a link to a file with the content has it")
	}
	writeStream(t, fsys, "/same.bin", "a\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "same.bin"), "a\n")
	symlink(t, target, filepath.Join(root, "other.bin"))
	writeStream(t, fsys, "/other.bin", "b\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "other.bin"), "b\n")
	wantRegular(t, target, "a\n")
	symlink(t, missing, filepath.Join(root, "dangling.bin"))
	writeStream(t, fsys, "/dangling.bin", "c\n", 0o644, owner, true)
	wantRegular(t, filepath.Join(root, "dangling.bin"), "c\n")
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the target of the dangling link: %v, want it still missing", err)
	}
}

// TestOSFSRemovesStaleTemporaryFiles checks that a write removes the temporary files that an earlier write of the
// same file left when it was killed before its rename, and nothing else: no directory, no link, no other file's.
func TestOSFSRemovesStaleTemporaryFiles(t *testing.T) {
	owner := localOwner(t)
	for name, writeA := range map[string]func(t *testing.T, fsys nodeup.FS){
		"WriteFile":   func(t *testing.T, fsys nodeup.FS) { write(t, fsys, "/d/a.bin", "a\n", 0o644, owner, true) },
		"WriteStream": func(t *testing.T, fsys nodeup.FS) { writeStream(t, fsys, "/d/a.bin", "a\n", 0o644, owner, true) },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{".a.bin.tmp123456", ".a.bin.tmp7", ".b.bin.tmp1", "a.bin.tmp1"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("half a binary"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, ".a.bin.tmpdir"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join(dir, "a.bin.tmp1"), filepath.Join(dir, ".a.bin.tmplink"))
			writeA(t, nodeup.OSFS{Root: root})
			wantOnly(t, dir, "a.bin", ".a.bin.tmpdir", ".a.bin.tmplink", ".b.bin.tmp1", "a.bin.tmp1")
		})
	}
}

// TestOSFSFailsWhereItCannotLookForTemporaryFiles checks that a write into a directory that cannot be listed fails,
// and writes nothing: it cannot remove what a killed write left.
func TestOSFSFailsWhereItCannotLookForTemporaryFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists any directory")
	}
	owner := localOwner(t)
	for name, writeA := range map[string]func(fsys nodeup.FS) (bool, error){
		"WriteFile": func(fsys nodeup.FS) (bool, error) {
			return fsys.WriteFile("/d/a.bin", []byte("a\n"), 0o644, owner)
		},
		"WriteStream": func(fsys nodeup.FS) (bool, error) {
			return fsys.WriteStream("/d/a.bin", strings.NewReader("a\n"), 0o644, owner)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "d")
			if err := os.Mkdir(dir, 0o300); err != nil { // write and search, not read
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			changed, err := writeA(nodeup.OSFS{Root: root})
			if !errors.Is(err, fs.ErrPermission) || changed {
				t.Errorf("%s into an unlistable directory: %v, %v; want a permission error", name, changed, err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			wantOnly(t, dir)
		})
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
