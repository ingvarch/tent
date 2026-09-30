package nodeup

import (
	"context"
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

// Fetch returns the content of the asset a as the phases fetch it, and reports whether that changed the cache.
func Fetch(ctx context.Context, h *Host, a nodeconfig.Asset) ([]byte, bool, error) {
	return fetch(ctx, h, a)
}

// FetchUpTo fetches as Fetch does, but takes an asset of at most size bytes.
func FetchUpTo(ctx context.Context, h *Host, a nodeconfig.Asset, size int64) ([]byte, bool, error) {
	l := assetLimits
	l.size = size
	return l.fetch(ctx, h, a)
}
