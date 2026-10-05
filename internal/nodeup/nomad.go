package nodeup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// nomadUnit is the unit of the Nomad agent, which tent renders into the node config's files.
var nomadUnit = path.Base(nodeconfig.NomadServiceFile)

// Directories of the Nomad agent.
const (
	nomadDataDir = "/var/lib/nomad"
	// nomadStateDir is the client's state directory, which holds the intro token: only its owner reads it.
	nomadStateDir = nomadDataDir + "/client"
)

// maxNomadBinary bounds the binary in the Nomad zip, by the size that the zip declares: archive/zip fails a read
// past it.
const maxNomadBinary = 256 << 20

// restartFile marks a change that the running Nomad has not read. A run writes it with the change and removes it once
// Nomad has started or restarted, so that a run that fails between the two leaves the restart to the next run.
const restartFile = "/var/lib/tent/nomad-restart"

// nomad is the phase that installs the Nomad binary of the nomad asset, writes the agent's files of the node config
// and starts nomad.service. It restarts Nomad only when the binary, a file or the unit changed: a restart for nothing
// interrupts the agent.
func nomad(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	return runSteps(ctx, h, nc, installNomad, configureNomad, reloadNomad, runNomad)
}

// installNomad writes the binary of the nomad asset, which it fetches through the asset cache, to NomadBinary, unless
// the file there is the same, and marks a restart when it writes it.
func installNomad(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (bool, error) {
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.NomadAsset })
	if i < 0 {
		return false, fmt.Errorf("install Nomad: NodeConfig has no %s asset", nodeconfig.NomadAsset)
	}
	zipFile, cached, err := fetch(ctx, h, nc.Assets[i])
	if err != nil {
		return false, fmt.Errorf("install Nomad: %w", err)
	}
	changed, err := writeNomadBinary(h, zipFile)
	if err != nil {
		return false, fmt.Errorf("install Nomad: %w", err)
	}
	return cached || changed, nil
}

// writeNomadBinary writes the binary of the Nomad zip at zipFile to NomadBinary. It reads the zip from the file and
// the binary twice, to compare it with the file and to write it only when they differ, so that neither the zip nor
// the binary is held in memory, and an unchanged binary is never written again.
func writeNomadBinary(h *Host, zipFile string) (bool, error) {
	f, err := h.FS.Open(zipFile)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	r, err := readerOf(f)
	if err != nil {
		return false, err
	}
	zf, err := nomadEntry(r, r.Size())
	if err != nil {
		return false, err
	}
	size, sum, err := hashEntry(zf)
	if err != nil {
		return false, err
	}
	dir, err := h.FS.EnsureDir(path.Dir(nodeconfig.NomadBinary), 0o755, nodeconfig.Owner)
	if err != nil {
		return false, err
	}
	same, err := h.FS.HasContent(nodeconfig.NomadBinary, size, sum, 0o755, nodeconfig.Owner)
	if err != nil || same {
		return dir, err
	}
	// Mark first: a write that then fails costs at most one extra restart; a mark after the write could lose the
	// restart.
	if err := markRestart(h); err != nil {
		return false, err
	}
	rc, err := zf.Open()
	if err != nil {
		return false, fmt.Errorf("the nomad archive's nomad: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := h.FS.WriteStream(nodeconfig.NomadBinary, rc, 0o755, nodeconfig.Owner); err != nil {
		return false, err
	}
	return true, nil
}

// nomadEntry returns the entry of the Nomad binary in the zip r of size bytes: the one regular file named nomad at the
// top, of at most maxNomadBinary bytes. HashiCorp's zips hold LICENSE.txt too, which stays unread.
func nomadEntry(r io.ReaderAt, size int64) (*zip.File, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("the nomad archive: %w", err)
	}
	var found *zip.File
	for _, f := range zr.File {
		if f.Name != "nomad" {
			continue
		}
		if found != nil {
			return nil, errors.New("the nomad archive has nomad twice")
		}
		found = f
	}
	switch {
	case found == nil:
		return nil, errors.New("the nomad archive has no nomad")
	case !found.Mode().IsRegular():
		return nil, errors.New("the nomad archive's nomad is not a regular file")
	case found.UncompressedSize64 > maxNomadBinary:
		return nil, fmt.Errorf("the nomad archive's nomad is over %d bytes", maxNomadBinary)
	}
	return found, nil
}

// hashEntry returns the size and the sha256 of the zip entry's content.
func hashEntry(zf *zip.File) (int64, []byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return 0, nil, fmt.Errorf("the nomad archive's nomad: %w", err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return 0, nil, fmt.Errorf("the nomad archive's nomad: %w", err)
	}
	return n, h.Sum(nil), nil
}

// nomadDir is a directory of the agent, with its mode.
type nomadDir struct {
	path string
	mode fs.FileMode
}

// configureNomad writes the agent's files of the node config: the directories first, the client's state directory
// with mode 0700 before the intro token goes into it, then every file with its mode and owner, and on a client
// 11-instance.hcl with the instance id. A file that differs marks a restart before it is written, as the binary does;
// a changed directory does not, as Nomad reads no directory's mode or owner.
func configureNomad(_ context.Context, h *Host, nc *nodeconfig.NodeConfig) (changed bool, err error) {
	dirs := []nomadDir{
		{"/etc/nomad.d", 0o755},
		{"/etc/nomad.d/tls", 0o755},
		{nomadDataDir, 0o755},
	}
	if nc.Role.RunsClient() {
		dirs = append(dirs, nomadDir{nomadStateDir, 0o700})
	}
	for _, d := range dirs {
		c, err := h.FS.EnsureDir(d.path, d.mode, nodeconfig.Owner)
		if err != nil {
			return false, fmt.Errorf("configure Nomad: %w", err)
		}
		changed = changed || c
	}
	files := nc.Files
	if nc.Role.RunsClient() {
		f, err := nodeconfig.RenderInstance(h.Instance.ID)
		if err != nil {
			return false, fmt.Errorf("configure Nomad: %w", err)
		}
		files = append(files, f)
	}
	for _, f := range files {
		sum := sha256.Sum256(f.Content)
		same, err := h.FS.HasContent(f.Path, int64(len(f.Content)), sum[:], fs.FileMode(f.Mode), f.Owner)
		if err != nil {
			return false, fmt.Errorf("configure Nomad: %w", err)
		}
		if same {
			continue
		}
		if err := markRestart(h); err != nil {
			return false, fmt.Errorf("configure Nomad: %w", err)
		}
		if _, err := h.FS.WriteFile(f.Path, f.Content, fs.FileMode(f.Mode), f.Owner); err != nil {
			return false, fmt.Errorf("configure Nomad: %w", err)
		}
		changed = true
	}
	return changed, nil
}

// reloadNomad makes systemd read nomad.service again when the file changed since systemd read it. A reload is a
// change of its own.
func reloadNomad(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	sd := h.systemd()
	// systemd reads a changed unit only after a reload; asking it, rather than trusting the write, lets the next run
	// finish a run that stopped between them.
	needs, err := sd.NeedsReload(ctx, nomadUnit)
	if err != nil {
		return false, fmt.Errorf("reload systemd: %w", err)
	}
	if !needs {
		return false, nil
	}
	if err := sd.DaemonReload(ctx); err != nil {
		return false, fmt.Errorf("reload systemd: %w", err)
	}
	return true, nil
}

// runNomad starts nomad.service when it is not active, and restarts it when it runs and restartFile marks a change
// that it has not read; then it removes the mark. A failed start or restart leaves the mark for the next run. Nomad is
// never enabled for boot: up starts it after the host firewall, so that no workload runs before the rules.
func runNomad(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	sd := h.systemd()
	_, active, err := sd.IsActive(ctx, nomadUnit)
	if err != nil {
		return false, fmt.Errorf("check Nomad: %w", err)
	}
	marked, err := restartMarked(h)
	if err != nil {
		return false, fmt.Errorf("check Nomad: %w", err)
	}
	switch {
	case !active:
		h.logger().Info("start Nomad")
		if err := sd.Start(ctx, nomadUnit); err != nil {
			return false, fmt.Errorf("start Nomad: %w", err)
		}
	case marked:
		h.logger().Info("restart Nomad: its binary or files changed")
		if err := sd.Restart(ctx, nomadUnit); err != nil {
			return false, fmt.Errorf("restart Nomad: %w", err)
		}
	default:
		return false, nil
	}
	if _, err := h.FS.Remove(restartFile); err != nil {
		return false, fmt.Errorf("run Nomad: %w", err)
	}
	return true, nil
}

// markRestart writes restartFile, which only root reads.
func markRestart(h *Host) error {
	if _, err := h.FS.EnsureDir(path.Dir(restartFile), 0o700, nodeconfig.Owner); err != nil {
		return err
	}
	_, err := h.FS.WriteFile(restartFile, nil, 0o600, nodeconfig.Owner)
	return err
}

// restartMarked reports whether restartFile is there.
func restartMarked(h *Host) (bool, error) {
	_, err := h.FS.Stat(restartFile)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}
