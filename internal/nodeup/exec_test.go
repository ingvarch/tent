package nodeup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
)

// helperEnv makes the test binary act as the program that ExecRunner runs: TestHelperProcess then does what its
// arguments after -- say.
const helperEnv = "TENT_NODEUP_HELPER"

// TestHelperProcess is the program of the ExecRunner tests. It does nothing unless helperEnv is set.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv(helperEnv) == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(100)
	}
	switch args[1] {
	case "echo": // echo <words>: prints the words
		fmt.Println(strings.Join(args[2:], " "))
		os.Exit(0)
	case "fail": // fail <code> <stderr>: prints stderr to the standard error and exits with code
		code, _ := strconv.Atoi(args[2])
		fmt.Fprint(os.Stderr, args[3])
		os.Exit(code)
	case "print-and-fail": // print-and-fail <code> <stdout>: prints stdout and exits with code
		code, _ := strconv.Atoi(args[2])
		fmt.Println(args[3])
		os.Exit(code)
	case "sleep": // sleep: sleeps for a minute
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(101)
}

// helper returns the program and the arguments that make the test binary do what args say.
func helper(t *testing.T, args ...string) (string, []string) {
	t.Helper()
	t.Setenv(helperEnv, "1")
	return os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
}

func TestExecRunnerReturnsTheOutput(t *testing.T) {
	name, args := helper(t, "echo", "hello", "node")
	out, err := nodeup.ExecRunner{}.Run(t.Context(), name, args...)
	if err != nil || string(out) != "hello node\n" {
		t.Errorf("Run = %q, %v; want %q", out, err, "hello node\n")
	}
}

func TestExecRunnerReportsTheExitStatus(t *testing.T) {
	name, args := helper(t, "fail", "3", "  Unit nomad.service not found.\n")
	_, err := nodeup.ExecRunner{}.Run(t.Context(), name, args...)
	exit, ok := errors.AsType[*nodeup.ExitError](err)
	if !ok || exit.Code != 3 || exit.Stderr != "Unit nomad.service not found." {
		t.Fatalf("Run: %v, want an ExitError with code 3 and the trimmed standard error", err)
	}
	want := nodeup.CommandLine(name, args...) + ": exit status 3: Unit nomad.service not found."
	if err.Error() != want {
		t.Errorf("Run: error %q, want %q", err, want)
	}
}

func TestExecRunnerReturnsTheOutputOfAFailedProgram(t *testing.T) {
	// systemctl is-active prints the state, such as inactive, and exits with 3.
	name, args := helper(t, "print-and-fail", "3", "inactive")
	out, err := nodeup.ExecRunner{}.Run(t.Context(), name, args...)
	if _, ok := errors.AsType[*nodeup.ExitError](err); !ok || string(out) != "inactive\n" {
		t.Errorf("Run = %q, %v; want %q and an ExitError", out, err, "inactive\n")
	}
}

func TestExecRunnerKeepsTheEndOfALongStandardError(t *testing.T) {
	name, args := helper(t, "fail", "1", strings.Repeat("noise ", 1000)+"the reason")
	_, err := nodeup.ExecRunner{}.Run(t.Context(), name, args...)
	exit, ok := errors.AsType[*nodeup.ExitError](err)
	if !ok || !strings.HasSuffix(exit.Stderr, "noise the reason") || len(exit.Stderr) > 1024 {
		t.Errorf("Run: %v, want an ExitError with at most 1024 bytes that end with the reason", err)
	}
}

func TestExecRunnerStopsTheProgramWhenTheContextEnds(t *testing.T) {
	name, args := helper(t, "sleep")
	// Long enough for the program to start under the race detector; the bound below still proves the kill.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := nodeup.ExecRunner{}.Run(ctx, name, args...)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run: %v, want an error that matches context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("Run took %s after its context ended, want it to stop the program", took)
	}
}

func TestExecRunnerReportsAMissingProgram(t *testing.T) {
	_, err := nodeup.ExecRunner{}.Run(t.Context(), "tent-no-such-program", "--flag")
	if _, ok := errors.AsType[*nodeup.ExitError](err); err == nil || ok ||
		!strings.HasPrefix(err.Error(), "tent-no-such-program --flag: ") {
		t.Errorf("Run of a missing program: %v, want an error that names the command and is no ExitError", err)
	}
}
