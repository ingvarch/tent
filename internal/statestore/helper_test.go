package statestore_test

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperURLEnv gives a helper process the URL of the store it works on.
const helperURLEnv = "TENT_STATESTORE_HELPER_URL"

// helperTimeout bounds a helper process, so a stuck one fails the test instead of hanging it.
const helperTimeout = 2 * time.Minute

// helper is the test binary run again as a helper process. It runs one test function and talks to the test through
// its stdin and stdout.
type helper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr strings.Builder
}

// startHelper runs the test binary again to run only the test function named run, with env added to its
// environment. The helper is killed after helperTimeout or when the test ends, and the test waits for it to exit.
// Call t.TempDir for the helper's files before startHelper, so that the helper has exited when they are removed:
// Windows cannot remove a file that a process holds open.
func startHelper(t *testing.T, run string, env ...string) *helper {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperTimeout)
	t.Cleanup(cancel)
	h := &helper{cmd: exec.CommandContext(ctx, os.Args[0], "-test.run=^"+run+"$", "-test.count=1")}
	h.cmd.Env = append(os.Environ(), env...)
	h.cmd.Stderr = &h.stderr
	var err error
	if h.stdin, err = h.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := h.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h.stdout = bufio.NewScanner(stdout)
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.cmd.Process.Kill() // fails when the helper has exited
		_ = h.cmd.Wait()         // fails when the test has waited already
	})
	return h
}

// pid returns the helper's process id.
func (h *helper) pid() int { return h.cmd.Process.Pid }

// expect reads the helper's next line and fails the test unless it is want.
func (h *helper) expect(t *testing.T, want string) {
	t.Helper()
	if !h.stdout.Scan() || h.stdout.Text() != want {
		h.fail(t, "the line "+want+" did not come; got "+h.stdout.Text())
	}
}

// send writes a line to the helper and closes its stdin.
func (h *helper) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := h.stdin.Close(); err != nil {
		t.Fatal(err)
	}
}

// wait reads the helper's output to the end and waits for it to exit.
func (h *helper) wait() ([]string, error) {
	var out []string
	for h.stdout.Scan() {
		out = append(out, h.stdout.Text())
	}
	return out, h.cmd.Wait()
}

// fail kills the helper and fails the test with why and the helper's output.
func (h *helper) fail(t *testing.T, why string) {
	t.Helper()
	_ = h.cmd.Process.Kill()
	out, err := h.wait() // stderr is complete only after Wait
	t.Fatalf("helper %d: %s; exit: %v, output:\n%s\n%s", h.pid(), why, err, strings.Join(out, "\n"),
		h.stderr.String())
}
