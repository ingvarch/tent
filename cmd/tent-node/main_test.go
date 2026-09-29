package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/buildinfo"
)

// runArgs runs tent-node with args and returns its exit code and what it wrote to stdout and stderr.
func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, &out, &errOut, deps{})
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := runArgs(t, "version")
	if want := "tent-node " + buildinfo.Get().String() + "\n"; code != 0 || stdout != want || stderr != "" {
		t.Errorf("tent-node version: code %d, stdout %q, stderr %q; want 0, %q and nothing", code, stdout, stderr, want)
	}
}

func TestHelp(t *testing.T) {
	for _, flag := range []string{"-h", "-help", "--help"} {
		code, stdout, stderr := runArgs(t, flag)
		if code != 0 || stdout != "" || !strings.Contains(stderr, "Usage: tent-node <command>") ||
			!strings.Contains(stderr, "version") {
			t.Errorf("tent-node %s: code %d, stdout %q, stderr %q; want 0 and the usage on stderr", flag, code, stdout,
				stderr)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		says string
	}{
		{"no command", nil, "Usage: tent-node <command>"},
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"unknown flag", []string{"-frobnicate"}, "flag provided but not defined: -frobnicate"},
		{"arguments to version", []string{"version", "extra"}, "version takes no arguments"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, c.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, c.says) {
				t.Errorf("tent-node %q: code %d, stdout %q, stderr %q; want 2 and an error that says %q", c.args, code,
					stdout, stderr, c.says)
			}
		})
	}
}

func TestBinaryReportsLinkedVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the tent-node binary")
	}
	const pkg = "github.com/ingvarch/tent/internal/buildinfo"
	ldflags := "-X " + pkg + ".version=1.2.3 -X " + pkg + ".commit=abc1234 -X " + pkg + ".date=2026-09-25T12:00:00Z"
	bin := filepath.Join(t.TempDir(), "tent-node")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, ".").
		CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("tent-node version: %v", err)
	}
	if want := "tent-node 1.2.3 (commit abc1234, built 2026-09-25T12:00:00Z, "; !strings.HasPrefix(string(out), want) {
		t.Errorf("tent-node version printed %q, want it to start with %q", out, want)
	}
}
