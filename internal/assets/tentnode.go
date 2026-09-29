package assets

import (
	"context"
	"fmt"

	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// TentNode returns the tent-node for linux on arch that goes with tent of the given version. A release or
// pre-release of tent (v0.3.0, v0.3.0-rc.1) reads the sha256 from checksums.txt of its own release. Any other build,
// git describe output and snapshots included, is a development build: no release holds its tent-node, which is at
// opts.DevURL with the sha256 opts.DevSHA256 for every architecture.
func TentNode(ctx context.Context, opts Options, version, arch string) (Asset, error) {
	if !buildinfo.IsRelease(version) {
		return devTentNode(opts, version)
	}
	dir := releaseDir(opts.tentURL, tentReleases, version)
	sumsURL := dir + "checksums.txt"
	sums, err := opts.get(ctx, sumsURL)
	if err != nil {
		return Asset{}, err
	}
	file := TentNodeFile(arch)
	sum, err := sumOf(sumsURL, sums, file)
	if err != nil {
		return Asset{}, err
	}
	return Asset{Name: nodeconfig.TentNodeAsset, Version: version, URLs: []string{dir + file}, SHA256: sum}, nil
}

// TentNodeFile is the name of tent-node for linux on arch in tent's release and its checksums.txt.
func TentNodeFile(arch string) string { return "tent-node_linux_" + arch }

// devTentNode returns the tent-node of a development build of tent, which no release holds.
func devTentNode(opts Options, version string) (Asset, error) {
	if opts.DevURL == "" || opts.DevSHA256 == "" {
		build := "tent " + version + " is a development build"
		if version == "" {
			build = "this tent has no version"
		}
		return Asset{}, fmt.Errorf("%s, so no release holds its tent-node: set TENT_NODE_URL and TENT_NODE_SHA256 "+
			"to a tent-node built from the same commit", build)
	}
	if !sha256Hex.MatchString(opts.DevSHA256) {
		return Asset{}, fmt.Errorf("TENT_NODE_SHA256 is %q, want 64 lower-case hex digits", opts.DevSHA256)
	}
	return Asset{
		Name: nodeconfig.TentNodeAsset, Version: version, URLs: []string{opts.DevURL}, SHA256: opts.DevSHA256,
	}, nil
}
