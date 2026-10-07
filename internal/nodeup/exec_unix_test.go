//go:build unix

package nodeup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
)

// writePIDs is for a script that has started a child: it writes the script's PID and the child's to the file $1, whole
// through a rename. The script then waits for the child or sleeps.
const writePIDs = `echo $$ $! > "$1.tmp" && mv "$1.tmp" "$1"; `

// startScript runs the shell script with r in the background, with the path of a file as $1. It returns the path, the
// cancel of Run's context and a channel that gets Run's error.
func startScript(t *testing.T, r nodeup.ExecRunner, script string) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "pids")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, "sh", "-c", script, "sh", file)
		result <- err
	}()
	return file, cancel, result
}

// readPIDs waits until the script has written its PID and its child's to file and returns them.
func readPIDs(t *testing.T, file string, result <-chan error) []int {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		if data, err := os.ReadFile(file); err == nil {
			var pids []int
			for _, field := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatalf("the script wrote %q, want two PIDs", data)
				}
				pids = append(pids, pid)
			}
			if len(pids) != 2 {
				t.Fatalf("the script wrote %q, want two PIDs", data)
			}
			return pids
		}
		select {
		case err := <-result:
			t.Fatalf("Run returned before the script wrote its PIDs: %v", err)
		case <-timeout:
			t.Fatal("the script wrote no PIDs within 30s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// stop cancels the run and returns how long Run took to return after the cancel, and its error.
func stop(t *testing.T, cancel context.CancelFunc, result <-chan error) (time.Duration, error) {
	t.Helper()
	start := time.Now()
	cancel()
	select {
	case err := <-result:
		return time.Since(start), err
	case <-time.After(time.Minute):
		t.Fatal("Run did not return within a minute of the cancel")
		return 0, nil
	}
}

// waitGone checks that the processes pids end. Signal 0 finds a killed process until its parent, or init for an
// orphan, reaps it, so it waits up to 10 seconds.
func waitGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range pids {
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if time.Now().After(deadline) {
				t.Errorf("process %d still runs after Run returned", pid)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestExecRunnerStopsTheProcessGroup(t *testing.T) {
	// The program records the SIGTERM before it exits; its child gets the signal too. It loops over short sleeps and
	// never calls wait: bash 3.2, the sh of macOS, crashes when a trapped signal arrives as wait starts. A sleep that
	// is starting misses the signal, and the trap runs once that sleep ends.
	file, cancel, result := startScript(t, nodeup.ExecRunner{},
		`trap 'echo TERM > "$1.term"; exit 1' TERM; sleep 30 & `+writePIDs+`while :; do sleep 0.1; done`)
	pids := readPIDs(t, file, result)
	took, err := stop(t, cancel, result)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v, want an error that matches context.Canceled", err)
	}
	// Well under the 10s that Run waits for a child that holds the output.
	if took > 5*time.Second {
		t.Errorf("Run took %s after the cancel, want the group stopped at once", took)
	}
	if data, err := os.ReadFile(file + ".term"); string(data) != "TERM\n" {
		t.Errorf("the program recorded %q, %v; want SIGTERM", data, err)
	}
	waitGone(t, pids...)
}

func TestExecRunnerKillsAProcessGroupThatIgnoresSIGTERM(t *testing.T) {
	const delay = time.Second
	file, cancel, result := startScript(t, nodeup.ExecRunner{WaitDelay: delay}, `trap '' TERM; sleep 30 & `+writePIDs+`wait`)
	pids := readPIDs(t, file, result)
	took, err := stop(t, cancel, result)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v, want an error that matches context.Canceled", err)
	}
	// The upper bound is well under the default delay of 10s.
	if took < delay || took > delay+5*time.Second {
		t.Errorf("Run took %s after the cancel, want %s and a little more", took, delay)
	}
	waitGone(t, pids...)
}

func TestExecRunnerKillsTheGroupOnceTheProgramHasExited(t *testing.T) {
	// The child ignores SIGTERM from its fork on and does not hold the output, so the program exits alone. The delay
	// would outlast the child's sleep.
	file, cancel, result := startScript(t, nodeup.ExecRunner{WaitDelay: time.Minute},
		`trap '' TERM; sleep 30 >/dev/null 2>&1 & trap - TERM; `+writePIDs+`wait`)
	pids := readPIDs(t, file, result)
	took, err := stop(t, cancel, result)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v, want an error that matches context.Canceled", err)
	}
	if took > 5*time.Second {
		t.Errorf("Run took %s after the cancel, want it to return once the program has exited", took)
	}
	waitGone(t, pids...)
}
