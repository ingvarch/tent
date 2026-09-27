package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ingvarch/tent/internal/buildinfo"
)

// goBuild builds this package with ldflags into dir and returns the binary's path.
func goBuild(dir, ldflags string) (string, error) {
	bin := filepath.Join(dir, "tent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return bin, nil
}

// buildTent builds this package with the given ldflags for one test and returns the binary path.
func buildTent(t *testing.T, ldflags string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the tent binary")
	}
	bin, err := goBuild(t.TempDir(), ldflags)
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// shared is the tent binary that sharedTent builds once for the package's tests, and TestMain removes.
var shared struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// sharedTent returns the tent binary without ldflags, built on the first call.
func sharedTent(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the tent binary")
	}
	shared.once.Do(func() {
		if shared.dir, shared.err = os.MkdirTemp("", "tent-bin-"); shared.err == nil {
			shared.path, shared.err = goBuild(shared.dir, "")
		}
	})
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	return shared.path
}

// TestMain removes the binary that sharedTent built.
func TestMain(m *testing.M) {
	code := m.Run()
	if shared.dir != "" {
		_ = os.RemoveAll(shared.dir) // a leftover temporary directory fails no test
	}
	os.Exit(code)
}

// tent returns a command that runs bin with args, without the user's config file and TENT_ variables.
func tent(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir(), "TENT_STATE=", "TENT_CLUSTER=")
	return cmd
}

func TestBinaryReportsLinkedVersion(t *testing.T) {
	const pkg = "github.com/ingvarch/tent/internal/buildinfo"
	bin := buildTent(t, "-X "+pkg+".version=1.2.3 -X "+pkg+".commit=abc1234 -X "+pkg+".date=2026-09-25T12:00:00Z")

	out, err := tent(t, bin, "version", "-o", "json").Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("tent version: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("tent version: %v", err)
	}
	var got buildinfo.Info
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Version != "1.2.3" || got.Commit != "abc1234" || got.Date != "2026-09-25T12:00:00Z" {
		t.Errorf("version info = %+v, want the linked values", got)
	}
}

func TestBinaryExitsOneOnError(t *testing.T) {
	bin := sharedTent(t)
	var stdout, stderr bytes.Buffer
	cmd := tent(t, bin, "frobnicate")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Errorf("tent frobnicate: err = %v, want exit code 1", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want the error", stderr.String())
	}
}
