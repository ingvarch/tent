package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// killGrace is how long Wait waits for the output pipes of a killed tent to close; a child of tent can hold them.
var killGrace = 10 * time.Second

// tentRunner runs the tent binary at Bin for a run whose files are under Dir.
type tentRunner struct {
	Bin string
	Dir string
}

// tentResult is what one run of tent left. Code is -1 when a signal ended tent, as the kill of an ended context does.
type tentResult struct {
	Stdout []byte
	Stderr []byte
	Code   int
	Took   time.Duration
}

// stateURL returns the file:// URL of a directory with an absolute path, in the form tent wants: file:///abs/path.
func stateURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}

// tentEnv returns environ for tent: the XDG directories under dir, so that no user config file or cache is read,
// and without TENT_STATE and TENT_CLUSTER, which would change which cluster or store tent uses.
func tentEnv(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "TENT_STATE", "TENT_CLUSTER", "XDG_CONFIG_HOME", "XDG_CACHE_HOME":
		default:
			out = append(out, kv)
		}
	}
	return append(out,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg", "config"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "xdg", "cache"),
	)
}

// run starts tent with args and the state store of cluster, waits for it and writes <Dir>/<cluster>-<step>.log with
// the arguments, the output, the exit code and the time. A tent that exits with a code is not an error: the code is in
// the result. The error says that tent did not start, that its output stayed open killGrace after it exited, or
// that the log was not written; the result is filled in the last case. A context that ends kills tent, and the code
// is then -1.
func (r tentRunner) run(ctx context.Context, cluster, step string, args ...string) (tentResult, error) {
	full := append(slices.Clone(args), "--state", stateURL(filepath.Join(r.Dir, "state-"+cluster)))
	cmd := exec.CommandContext(ctx, r.Bin, full...)
	cmd.Env = tentEnv(os.Environ(), r.Dir)
	cmd.WaitDelay = killGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	err := cmd.Run()
	res := tentResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Took: time.Since(start)}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		res.Code = exit.ExitCode()
	case err != nil:
		return res, fmt.Errorf("run tent: %w", err)
	}
	log := fmt.Sprintf("args: %q\nexit code: %d\ntook: %s\n--- stdout ---\n%s--- stderr ---\n%s",
		full, res.Code, res.Took, res.Stdout, res.Stderr)
	path := filepath.Join(r.Dir, cluster+"-"+step+".log")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		return res, fmt.Errorf("write the log of tent: %w", err)
	}
	return res, nil
}
