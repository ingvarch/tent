//go:build !unix

package nodeup

import (
	"io/fs"
	"os"
)

// ids stands for an owner, which systems other than Unix do not have in the same form.
type ids struct{}

// lookupOwner checks only that owner is user:group.
func lookupOwner(owner string) (ids, error) {
	_, _, err := SplitOwner(owner)
	return ids{}, err
}

// sameMeta reports true: modes and owners are not compared.
func sameMeta(fs.FileInfo, fs.FileMode, ids) bool { return true }

// setMeta does nothing: modes and owners are not set.
func setMeta(*os.File, fs.FileMode, ids) error { return nil }

// setDirMeta does nothing: modes and owners are not set.
func setDirMeta(string, fs.FileMode, ids) error { return nil }

// syncDir does nothing: directories cannot be flushed on their own.
func syncDir(string) error { return nil }
