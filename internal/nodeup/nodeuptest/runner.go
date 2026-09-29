package nodeuptest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ingvarch/tent/internal/nodeup"
)

// Runner is a nodeup.Runner that records the commands it runs and answers them from a script: a command runs the
// Answer that On gave it, and one without an answer succeeds with no output. Like nodeup.ExecRunner, it names the
// command in its errors, does not run a command whose context has ended, and returns the context's error when the
// context ends while the command runs. The zero Runner has no answers. It is safe for concurrent use.
type Runner struct {
	mu       sync.Mutex
	answers  map[string]Answer // by command
	commands []string
}

// Answer answers a command: its standard output, or its error, such as a *nodeup.ExitError. ctx is the command's
// context.
type Answer func(ctx context.Context) ([]byte, error)

// Output answers with stdout and no error.
func Output(stdout string) Answer {
	return func(context.Context) ([]byte, error) { return []byte(stdout), nil }
}

// Exit answers as a program that wrote stderr and exited with the status code: with a *nodeup.ExitError that holds
// the code and stderr without the white space around it, and no output.
func Exit(code int, stderr string) Answer { return ExitOutput(code, "", stderr) }

// ExitOutput answers as Exit does, with the output stdout, as nodeup.ExecRunner returns what a failed program printed.
func ExitOutput(code int, stdout, stderr string) Answer {
	return func(context.Context) ([]byte, error) {
		return []byte(stdout), &nodeup.ExitError{Code: code, Stderr: strings.TrimSpace(stderr)}
	}
}

// On makes every run of the command, a program and its arguments as nodeup.CommandLine writes them, run a.
func (r *Runner) On(command string, a Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answers == nil {
		r.answers = map[string]Answer{}
	}
	r.answers[command] = a
}

// Commands returns the commands that ran, in order, as nodeup.CommandLine writes them.
func (r *Runner) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.commands)
}

// Run records the command and returns what its answer returns.
func (r *Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := nodeup.CommandLine(name, args...)
	if err := context.Cause(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w", line, err)
	}
	r.mu.Lock()
	r.commands = append(r.commands, line)
	answer := r.answers[line]
	r.mu.Unlock()
	if answer == nil {
		return nil, nil
	}
	out, err := answer(ctx)
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("%s: %w", line, cause)
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", line, err)
	}
	return out, nil
}
