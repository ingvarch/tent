package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/buildinfo"
)

// buildTent builds this package with the given ldflags and returns the binary path.
func buildTent(t *testing.T, ldflags string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the tent binary")
	}
	bin := filepath.Join(t.TempDir(), "tent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestBinaryReportsLinkedVersion(t *testing.T) {
	const pkg = "github.com/ingvarch/tent/internal/buildinfo"
	bin := buildTent(t, "-X "+pkg+".version=1.2.3 -X "+pkg+".commit=abc1234 -X "+pkg+".date=2026-09-25T12:00:00Z")

	out, err := exec.Command(bin, "version", "-o", "json").Output()
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
	bin := buildTent(t, "")
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "frobnicate")
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
