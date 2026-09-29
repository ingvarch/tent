package nodeup

import "io/fs"

// SetDirMeta gives the directory name mode and owner as EnsureDir does once it has checked the directory, for tests.
func SetDirMeta(name string, mode fs.FileMode, owner string) error {
	own, err := lookupOwner(owner)
	if err != nil {
		return err
	}
	return setDirMeta(name, mode, own)
}
