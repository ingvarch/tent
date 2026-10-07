package e2e

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSSHArgsRunTheScriptFromStdinAsRootWithoutHostKeyChecks(t *testing.T) {
	want := []string{
		"-F", "/dev/null",
		"-i", "/r/ssh/id_ed25519",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"root@203.0.113.9", "sh", "-s",
	}
	if got := sshArgs("/r/ssh/id_ed25519", "203.0.113.9"); !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgs = %q, want %q", got, want)
	}
}

// fakeSSH puts a stand-in for ssh first on PATH.
func fakeSSH(t *testing.T, body string) {
	t.Helper()
	path := fakeProgram(t, "ssh", body)
	t.Setenv("PATH", filepath.Dir(path)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSSHRunGivesTheScriptToSSHOnStdinAndReturnsItsOutput(t *testing.T) {
	fakeSSH(t, `echo "args: $*"; cat`)
	got, err := sshRun(t.Context(), "/r/ssh/id_ed25519", "203.0.113.9", "date +%s\n")
	want := "args: " + strings.Join(sshArgs("/r/ssh/id_ed25519", "203.0.113.9"), " ") + "\ndate +%s\n"
	if err != nil || got != want {
		t.Errorf("sshRun = %q, %v, want %q", got, err, want)
	}
}

func TestSSHRunReportsTheHostAndWhatSSHSaid(t *testing.T) {
	fakeSSH(t, `echo "Permission denied" >&2; exit 255`)
	_, err := sshRun(t.Context(), "/r/key", "203.0.113.9", "true\n")
	if err == nil || !strings.HasPrefix(err.Error(), "ssh root@203.0.113.9: ") ||
		!strings.HasSuffix(err.Error(), ": Permission denied") {
		t.Errorf("sshRun error = %v, want the host first and what ssh said last", err)
	}
}

func TestSSHRunEndsAfterTheSSHTimeout(t *testing.T) {
	old := sshTimeout
	sshTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sshTimeout = old })
	fakeSSH(t, `exec sleep 60`)
	start := time.Now()
	_, err := sshRun(t.Context(), "/r/key", "203.0.113.9", "true\n")
	if err == nil || !strings.HasPrefix(err.Error(), "ssh root@203.0.113.9: ") {
		t.Errorf("sshRun error = %v, want a failure of ssh", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("sshRun took %v: the timeout did not end ssh", took)
	}
}

func TestSSHRunEndsWhenTheContextEnds(t *testing.T) {
	fakeSSH(t, `exec sleep 60`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if _, err := sshRun(ctx, "/r/key", "203.0.113.9", "true\n"); err == nil {
		t.Error("sshRun = nil, want a failure")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("sshRun took %v: the context did not end ssh", took)
	}
}
