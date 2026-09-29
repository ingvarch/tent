//go:build unix

package nodeup

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// ids are the numeric user and group of an owner.
type ids struct{ uid, gid int }

// lookupOwner looks up the user and the group of owner, user:group, on the machine.
func lookupOwner(owner string) (ids, error) {
	userName, groupName, err := SplitOwner(owner)
	if err != nil {
		return ids{}, err
	}
	u, err := user.Lookup(userName)
	if err != nil {
		return ids{}, fmt.Errorf("owner %s: %w", owner, err)
	}
	g, err := user.LookupGroup(groupName)
	if err != nil {
		return ids{}, fmt.Errorf("owner %s: %w", owner, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return ids{}, fmt.Errorf("owner %s: user id %q: %w", owner, u.Uid, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return ids{}, fmt.Errorf("owner %s: group id %q: %w", owner, g.Gid, err)
	}
	return ids{uid, gid}, nil
}

// sameMeta reports whether fi has the permission bits mode, no other mode bits such as setuid, and the owner own.
func sameMeta(fi fs.FileInfo, mode fs.FileMode, own ids) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == mode &&
		int(st.Uid) == own.uid && int(st.Gid) == own.gid
}

// setMeta gives the open file f mode and owner. Unlike creating a file, Chmod does not apply the umask.
func setMeta(f *os.File, mode fs.FileMode, own ids) error {
	if err := f.Chown(own.uid, own.gid); err != nil {
		return err
	}
	// After Chown, which may clear the setuid and setgid bits.
	return f.Chmod(mode)
}

// setDirMeta gives the directory name mode and owner. It opens name only as a directory and without following a link,
// so that a link or a file that took the directory's place since it was checked is neither followed nor changed.
func setDirMeta(name string, mode fs.FileMode, own ids) error {
	d, err := os.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := setMeta(d, mode, own); err != nil {
		_ = d.Close() // read-only; the error to report is setMeta's
		return err
	}
	return d.Close()
}

// syncDir flushes the entries of the directory dir to disk, so that a rename in it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() // read-only; the error to report is Sync's
		return err
	}
	return d.Close()
}
