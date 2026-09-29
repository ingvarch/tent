package nodeup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// Facts of the machines that tent-node runs on.
var (
	archs          = []string{"amd64", "arm64"}
	ubuntuVersions = []string{"24.04", "26.04"} // the ones tent tests; others work with a warning
)

// Paths that preflight reads.
const (
	systemdRunDir = "/run/systemd/system" // exists only while systemd runs the machine
	osReleaseFile = "/etc/os-release"
)

// metadataTimeout is how long preflight waits for the metadata service, which the environment asks again after a
// failure, before it fails.
const metadataTimeout = 3 * time.Minute

// preflight returns the phase that checks that tent-node can set the machine up for nc, and reads the machine from
// the metadata service environment into the host. It checks the platform, root, systemd and Ubuntu; that the host
// name is the node's name, so that a node never acts on another node's config; and that tent-node's version is that
// of nc's tent-node asset. Then it gives the metadata service metadataTimeout to answer. It changes nothing: it
// reports Unchanged, with a warning as the reason on a version of Ubuntu that tent does not test.
func preflight(environment env.Environment) Phase {
	return Phase{Name: "preflight", Run: func(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
		warning, err := checkMachine(h, nc)
		if err != nil {
			return Result{}, err
		}
		if warning != "" {
			h.logger().Warn(warning)
		}
		h.logger().Info("read the metadata service", "within", metadataTimeout)
		readCtx, cancel := context.WithTimeoutCause(ctx, metadataTimeout,
			fmt.Errorf("no answer within %v", metadataTimeout))
		defer cancel()
		inst, err := environment.Read(readCtx)
		if err != nil {
			return Result{}, fmt.Errorf("read the metadata service: %w", err)
		}
		h.Instance = inst
		return Result{Status: Unchanged, Reason: warning}, nil
	}}
}

// checkMachine checks the machine h against nc, and returns a warning for a version of Ubuntu that tent does not test.
func checkMachine(h *Host, nc *nodeconfig.NodeConfig) (warning string, err error) {
	switch {
	case h.OS != "linux":
		return "", fmt.Errorf("tent-node runs on linux, not on %s", h.OS)
	case !slices.Contains(archs, h.Arch):
		return "", fmt.Errorf("tent-node runs on %s, not on %s", strings.Join(archs, " and "), h.Arch)
	case h.EUID != 0:
		return "", fmt.Errorf("tent-node runs as root, not as the user id %d", h.EUID)
	}
	if err := checkSystemd(h.FS); err != nil {
		return "", err
	}
	warning, err = checkUbuntu(h.FS)
	if err != nil {
		return "", err
	}
	if h.HostName != nc.Name {
		return "", fmt.Errorf("the host name is %s, not %s: the node config is another node's", h.HostName, nc.Name)
	}
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.TentNodeAsset })
	switch {
	case i < 0:
		return "", errors.New("the node config has no tent-node asset, which gives the version of tent-node")
	case nc.Assets[i].Version != h.Version:
		return "", fmt.Errorf("tent-node is %s, not %s, the version of the node config's tent-node asset", h.Version,
			nc.Assets[i].Version)
	}
	return warning, nil
}

// checkSystemd checks that systemd runs the machine, as sd_booted(3) does.
func checkSystemd(fsys FS) error {
	fi, err := fsys.Stat(systemdRunDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("systemd does not run the machine: %s is missing", systemdRunDir)
	case err != nil:
		return fmt.Errorf("check for systemd: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("systemd does not run the machine: %s is not a directory", systemdRunDir)
	}
	return nil
}

// checkUbuntu checks that the OS is Ubuntu, and returns a warning for a version that tent does not test.
func checkUbuntu(fsys FS) (warning string, err error) {
	data, err := fsys.ReadFile(osReleaseFile)
	if err != nil {
		return "", fmt.Errorf("read the OS: %w", err)
	}
	id, version := osRelease(data)
	if id != "ubuntu" {
		return "", fmt.Errorf("the OS is %q, not ubuntu: tent-node runs on Ubuntu", id)
	}
	if slices.Contains(ubuntuVersions, version) {
		return "", nil
	}
	release := "Ubuntu " + version
	if version == "" {
		release = "Ubuntu without a VERSION_ID"
	}
	return fmt.Sprintf("%s is not tested: tent supports Ubuntu %s", release, strings.Join(ubuntuVersions, " and ")),
		nil
}

// osRelease returns the ID and the VERSION_ID of os-release(5) data, without their quotes.
func osRelease(data []byte) (id, version string) {
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	return id, version
}
