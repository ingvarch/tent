package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStateURL(t *testing.T) {
	for path, want := range map[string]string{
		"/tmp/run/state-c1":   "file:///tmp/run/state-c1",
		"C:/run/state-c1":     "file:///C:/run/state-c1",
		"/tmp/with space/s-c": "file:///tmp/with space/s-c",
	} {
		if got := stateURL(path); got != want {
			t.Errorf("stateURL(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTentEnvReplacesTheXDGDirectoriesAndDropsTheStateVariables(t *testing.T) {
	environ := []string{
		"A=1",
		"XDG_CONFIG_HOME=/old/config",
		"TENT_STATE=file:///old",
		"TENT_CLUSTER=old",
		"XDG_CACHE_HOME=/old/cache",
		"TENT_STATE_NOT=kept",
		"B=2",
	}
	want := []string{
		"A=1",
		"TENT_STATE_NOT=kept",
		"B=2",
		"XDG_CONFIG_HOME=" + filepath.Join("/run", "xdg", "config"),
		"XDG_CACHE_HOME=" + filepath.Join("/run", "xdg", "cache"),
	}
	if got := tentEnv(environ, "/run"); !reflect.DeepEqual(got, want) {
		t.Errorf("tentEnv = %q, want %q", got, want)
	}
}

// fakeTent writes a sh script as a stand-in for the tent binary and returns its path.
func fakeTent(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tent is a /bin/sh script")
	}
	path := filepath.Join(t.TempDir(), "tent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write the fake tent: %v", err)
	}
	return path
}

const echoScript = `echo "args: $*"
echo "config: $XDG_CONFIG_HOME"
echo "cache: $XDG_CACHE_HOME"
echo "state: ${TENT_STATE-unset}"
echo "cluster: ${TENT_CLUSTER-unset}"
echo "kept: ${E2E_FAKE_KEPT-unset}"
echo "problem" >&2
exit %d`

func TestTentRunnerPassesTheStateAndTheEnvironmentAndKeepsTheExitCode(t *testing.T) {
	t.Setenv("TENT_STATE", "file:///old")
	t.Setenv("TENT_CLUSTER", "old")
	t.Setenv("XDG_CONFIG_HOME", "/old/config")
	t.Setenv("XDG_CACHE_HOME", "/old/cache")
	t.Setenv("E2E_FAKE_KEPT", "yes")
	dir := t.TempDir()
	r := tentRunner{Bin: fakeTent(t, fmt.Sprintf(echoScript, 3)), Dir: dir}

	res, err := r.run(context.Background(), "c1", "update", "update", "cluster", "c1", "--yes")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Code != 3 {
		t.Errorf("Code = %d, want 3", res.Code)
	}
	wantOut := strings.Join([]string{
		"args: update cluster c1 --yes --state " + stateURL(filepath.Join(dir, "state-c1")),
		"config: " + filepath.Join(dir, "xdg", "config"),
		"cache: " + filepath.Join(dir, "xdg", "cache"),
		"state: unset",
		"cluster: unset",
		"kept: yes",
		"",
	}, "\n")
	if string(res.Stdout) != wantOut {
		t.Errorf("stdout:\n%s\nwant:\n%s", res.Stdout, wantOut)
	}
	if string(res.Stderr) != "problem\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "problem\n")
	}
	if res.Took <= 0 {
		t.Errorf("Took = %v, want a positive time", res.Took)
	}
}

func TestTentRunnerWritesALogOnlyTheUserCanRead(t *testing.T) {
	dir := t.TempDir()
	r := tentRunner{Bin: fakeTent(t, fmt.Sprintf(echoScript, 3)), Dir: dir}
	res, err := r.run(context.Background(), "c1", "update", "update", "cluster", "c1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	path := filepath.Join(dir, "c1-update.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the log: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	for _, want := range []string{
		`args: ["update" "cluster" "c1" "--state" "` + stateURL(filepath.Join(dir, "state-c1")) + `"]`,
		"exit code: 3",
		"took: " + res.Took.String(),
		"--- stdout ---\n" + string(res.Stdout),
		"--- stderr ---\nproblem\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the log lacks %q:\n%s", want, data)
		}
	}
}

// cancelWhenReady returns a context that ends once the file ready exists, however slow the machine is, or after 30
// seconds.
func cancelWhenReady(t *testing.T, ready string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		defer cancel()
		for range 3000 {
			if _, err := os.Stat(ready); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ctx
}

func TestTentRunnerKillsTentWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	r := tentRunner{Bin: fakeTent(t, "echo started\ntouch '"+ready+"'\nexec sleep 60"), Dir: dir}
	ctx := cancelWhenReady(t, ready)

	res, err := r.run(ctx, "c1", "slow", "update")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Code != -1 {
		t.Errorf("Code = %d, want -1", res.Code)
	}
	if res.Took > 30*time.Second {
		t.Errorf("Took = %v: tent was not killed", res.Took)
	}
	data, err := os.ReadFile(filepath.Join(dir, "c1-slow.log"))
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if !strings.Contains(string(data), "exit code: -1") || !strings.Contains(string(data), "started") {
		t.Errorf("the log of a killed tent:\n%s", data)
	}
}

func TestTentRunnerStopsWaitingForPipesThatAChildOfTentHolds(t *testing.T) {
	old := killGrace
	killGrace = 200 * time.Millisecond
	t.Cleanup(func() { killGrace = old })
	// The background sleep keeps the output pipes open for 5 seconds after tent is killed.
	ready := filepath.Join(t.TempDir(), "ready")
	r := tentRunner{Bin: fakeTent(t, "sleep 5 &\ntouch '"+ready+"'\nexec sleep 60"), Dir: t.TempDir()}
	ctx := cancelWhenReady(t, ready)

	res, err := r.run(ctx, "c1", "slow", "update")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Took > 3*time.Second {
		t.Errorf("Took = %v: Wait waited for the pipes of the child", res.Took)
	}
}

func TestTentRunnerReportsATentThatCannotStart(t *testing.T) {
	r := tentRunner{Bin: filepath.Join(t.TempDir(), "missing"), Dir: t.TempDir()}
	_, err := r.run(context.Background(), "c1", "update", "update")
	// Windows reports a missing program without an extension as exec.ErrNotFound.
	if err == nil || !strings.HasPrefix(err.Error(), "run tent: ") ||
		(!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, exec.ErrNotFound)) {
		t.Errorf("error = %v, want a start failure of tent", err)
	}
	if _, statErr := os.Stat(filepath.Join(r.Dir, "c1-update.log")); statErr == nil {
		t.Error("a log was written for a tent that did not start")
	}
}

func TestTentRunnerReportsALogThatCannotBeWritten(t *testing.T) {
	r := tentRunner{Bin: fakeTent(t, "echo hi"), Dir: filepath.Join(t.TempDir(), "missing")}
	res, err := r.run(context.Background(), "c1", "update", "update")
	if err == nil || !strings.Contains(err.Error(), "c1-update.log") {
		t.Errorf("error = %v, want one naming the log", err)
	}
	if string(res.Stdout) != "hi\n" {
		t.Errorf("stdout = %q: the result is lost with the log", res.Stdout)
	}
}
