package nodeup

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner runs programs on the machine. Arguments must hold no secrets: errors and logs show them.
type Runner interface {
	// Run runs the program name with args, waits for it to exit and returns what it wrote to its standard output.
	// The error names the command. A program that exits with a status other than 0 gives an error that matches
	// *ExitError, and its output. When ctx ends, Run stops the program and returns an error that matches ctx's error.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExitError is the error of a program that exited with a status other than 0.
type ExitError struct {
	Code   int    // the exit status, or -1 when a signal ended the program
	Stderr string // the end of what the program wrote to its standard error, without white space around it
}

// Error returns the exit status and the standard error, such as exit status 1: Unit nomad.service not found.
func (e *ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr)
}

// CommandLine returns a program and its arguments as errors and logs show the command, separated by spaces.
func CommandLine(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// maxStderr is how much of a failed program's standard error its ExitError keeps: the end, where the reason is.
const maxStderr = 1024

// waitDelay is how long ExecRunner waits by default for a stopped program to exit, and for the output of a program
// that has exited to close, which a child that the program started can hold open.
const waitDelay = 10 * time.Second

// ExecRunner runs programs as processes of the machine. On Unix each program runs in a process group of its own: when
// ctx ends, Run sends SIGTERM to the group, and once it has stopped waiting for the program and its output, SIGKILL to
// what is left. After WaitDelay it kills the program and stops reading its output, which bounds that wait. Elsewhere
// it kills the program alone.
type ExecRunner struct {
	// WaitDelay is how long Run waits for a stopped program to exit, and for the output of a program that has exited
	// to close; zero means 10 seconds.
	WaitDelay time.Duration
}

// Run runs the program name with args.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = cmp.Or(r.WaitDelay, waitDelay)
	afterWait := stopGroup(cmd)
	err := cmd.Run()
	afterWait()
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%s: %w", CommandLine(name, args...), context.Cause(ctx))
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.Bytes(), fmt.Errorf("%s: %w", CommandLine(name, args...),
			&ExitError{Code: exit.ExitCode(), Stderr: lastBytes(strings.TrimSpace(stderr.String()), maxStderr)})
	}
	return nil, fmt.Errorf("%s: %w", CommandLine(name, args...), err)
}

// lastBytes returns the last n bytes of s, or s when it is shorter. It may cut a character in two.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
