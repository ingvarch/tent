package nodeup

import (
	"context"
	"io"
	"io/fs"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// SetDirMeta gives the directory name mode and owner as EnsureDir does once it has checked the directory, for tests.
func SetDirMeta(name string, mode fs.FileMode, owner string) error {
	own, err := lookupOwner(owner)
	if err != nil {
		return err
	}
	return setDirMeta(name, mode, own)
}

// Fetch fetches the asset a as the phases do, and returns the content of its cache file and whether that changed the
// cache. It reads the file through FS.Open, which the fake does not count as a read of a whole file.
func Fetch(ctx context.Context, h *Host, a nodeconfig.Asset) ([]byte, bool, error) {
	file, changed, err := fetch(ctx, h, a)
	return content(h, file, changed, err)
}

// FetchUpTo fetches as Fetch does, but takes an asset of at most size bytes.
func FetchUpTo(ctx context.Context, h *Host, a nodeconfig.Asset, size int64) ([]byte, bool, error) {
	l := assetLimits
	l.size = size
	file, changed, err := l.fetch(ctx, h, a)
	return content(h, file, changed, err)
}

// content returns the content of file through FS.Open, and changed, unless err is set.
func content(h *Host, file string, changed bool, err error) ([]byte, bool, error) {
	if err != nil {
		return nil, false, err
	}
	f, err := h.FS.Open(file)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	r, err := readerOf(f)
	if err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(r)
	return data, changed, err
}

// LockAt takes the lock that Lock takes, at the given path and with the given wait between tries, for tests.
var LockAt = lock

// CheckHealth checks the node's Nomad agent as verify does, with the given wait between tries, for tests.
var CheckHealth = checkHealth
