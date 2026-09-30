package nodeup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// cniBinDir is where Nomad looks for the CNI plugins, its default cni_path.
const cniBinDir = "/opt/cni/bin"

// maxPluginBytes is the largest file that the cni phase takes from the archive.
const maxPluginBytes = 256 << 20

// pluginName is the form of the names of the files that the cni phase takes from the archive.
var pluginName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// entryKinds describe the types of tar entries that the cni phase refuses.
var entryKinds = map[byte]string{
	tar.TypeDir: "a directory", tar.TypeSymlink: "a symbolic link", tar.TypeLink: "a hard link",
	tar.TypeChar: "a character device", tar.TypeBlock: "a block device", tar.TypeFifo: "a named pipe",
}

// cni is the phase that puts the CNI plugins of nc's cni-plugins asset into cniBinDir, on a node that runs a client.
// It fetches the archive, checks every entry, and then writes each file with the mode of the archive, masked to 0755.
// Files that the archive lacks stay.
func cni(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	if !nc.Role.RunsClient() {
		return Result{Status: Skipped, Reason: "servers run no workloads"}, nil
	}
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.CNIPluginsAsset })
	if i < 0 {
		return Result{}, errors.New("NodeConfig has no cni-plugins asset")
	}
	asset := nc.Assets[i]
	var archive []byte
	fetchArchive := func(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (changed bool, err error) {
		archive, changed, err = fetch(ctx, h, asset)
		return changed, err
	}
	unpack := func(_ context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
		changed, err := unpackPlugins(h.FS, archive)
		if err != nil {
			return false, fmt.Errorf("unpack %s %s: %w", asset.Name, asset.Version, err)
		}
		return changed, nil
	}
	return runSteps(ctx, h, nc, fetchArchive, unpack)
}

// unpackPlugins writes the files of the gzip tar archive into cniBinDir, once every entry has passed checkEntry and
// no file comes twice, and reports whether that changed anything.
func unpackPlugins(fsys FS, archive []byte) (bool, error) {
	files := map[string]bool{}
	err := eachEntry(archive, func(h *tar.Header, _ io.Reader) error {
		if err := checkEntry(h); err != nil || h.Typeflag == tar.TypeDir {
			return err
		}
		// ./bridge and bridge are one file, which would be written twice on every run.
		name := strings.TrimPrefix(h.Name, "./")
		if files[name] {
			return fmt.Errorf("%q: the archive holds %s twice", h.Name, name)
		}
		files[name] = true
		return nil
	})
	if err != nil {
		return false, err
	}
	changed := false
	for _, dir := range []string{path.Dir(cniBinDir), cniBinDir} {
		c, err := fsys.EnsureDir(dir, 0o755, nodeconfig.Owner)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	err = eachEntry(archive, func(h *tar.Header, content io.Reader) error {
		if h.Typeflag == tar.TypeDir {
			return nil
		}
		data := make([]byte, h.Size)
		if _, err := io.ReadFull(content, data); err != nil {
			return err
		}
		name := cniBinDir + "/" + strings.TrimPrefix(h.Name, "./")
		c, err := fsys.WriteFile(name, data, fs.FileMode(h.Mode)&0o755, nodeconfig.Owner)
		changed = changed || c
		return err
	})
	return changed, err
}

// checkEntry checks that h is the header of an entry that the cni phase takes: the top directory, ./, or a file at
// the top level, such as ./bridge or bridge, of at most maxPluginBytes.
func checkEntry(h *tar.Header) error {
	switch {
	case h.Typeflag == tar.TypeDir && h.Name == "./":
		return nil
	case h.Typeflag != tar.TypeReg:
		kind, ok := entryKinds[h.Typeflag]
		if !ok {
			kind = fmt.Sprintf("an entry of type %q", h.Typeflag)
		}
		return fmt.Errorf("%q is %s, not a file at the top level", h.Name, kind)
	case !pluginName.MatchString(strings.TrimPrefix(h.Name, "./")):
		return fmt.Errorf("%q is not a plain file name at the top level", h.Name)
	case h.Size > maxPluginBytes:
		return fmt.Errorf("%q has %d bytes, more than %d", h.Name, h.Size, maxPluginBytes)
	}
	return nil
}

// eachEntry calls fn with the header and the content of each entry of the gzip tar archive, in order, and stops at the
// first error.
func eachEntry(archive []byte, fn func(h *tar.Header, content io.Reader) error) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}
