package nodeuptest_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// The fake is a nodeup.Runner.
var _ nodeup.Runner = (*nodeuptest.Runner)(nil)

func TestRunnerRecordsAndAnswers(t *testing.T) {
	var r nodeuptest.Runner
	r.On("systemctl is-enabled nomad.service", nodeuptest.Output("enabled\n"))
	r.On("systemctl start nomad.service", nodeuptest.Exit(1, "Job for nomad.service failed.\n"))

	out, err := r.Run(t.Context(), "systemctl", "is-enabled", "nomad.service")
	if err != nil || string(out) != "enabled\n" {
		t.Errorf("is-enabled: %q, %v; want the scripted output", out, err)
	}
	// A command without an answer succeeds with no output.
	if out, err := r.Run(t.Context(), "systemctl", "daemon-reload"); err != nil || len(out) != 0 {
		t.Errorf("daemon-reload: %q, %v; want no output and no error", out, err)
	}
	_, err = r.Run(t.Context(), "systemctl", "start", "nomad.service")
	exit, ok := errors.AsType[*nodeup.ExitError](err)
	if !ok || exit.Code != 1 || exit.Stderr != "Job for nomad.service failed." {
		t.Errorf("start: err %v, want an ExitError with code 1 and the trimmed standard error", err)
	}
	// The error names the command, as the error of nodeup.ExecRunner does.
	if want := "systemctl start nomad.service: exit status 1: Job for nomad.service failed."; errText(err) != want {
		t.Errorf("start: err %q, want %q", errText(err), want)
	}
	// A program can print and fail, as systemctl is-active does for a unit that is not active.
	r.On("systemctl is-active nomad.service", nodeuptest.ExitOutput(3, "inactive\n", ""))
	out, err = r.Run(t.Context(), "systemctl", "is-active", "nomad.service")
	if exit, ok := errors.AsType[*nodeup.ExitError](err); !ok || exit.Code != 3 || string(out) != "inactive\n" {
		t.Errorf("is-active: %q, %v; want %q and an ExitError with code 3", out, err, "inactive\n")
	}
	want := []string{
		"systemctl is-enabled nomad.service", "systemctl daemon-reload", "systemctl start nomad.service",
		"systemctl is-active nomad.service",
	}
	if diff := cmp.Diff(want, r.Commands()); diff != "" {
		t.Errorf("Commands() (-want +got):\n%s", diff)
	}
}

func TestRunnerNext(t *testing.T) {
	var r nodeuptest.Runner
	r.On("systemctl daemon-reload", nodeuptest.Output("on\n"))
	r.Next("systemctl daemon-reload", nodeuptest.Exit(1, "Failed to reload daemon: Access denied"))
	r.Next("systemctl daemon-reload", nodeuptest.Output("second\n"))
	var got []string
	for range 4 {
		out, err := r.Run(t.Context(), "systemctl", "daemon-reload")
		got = append(got, string(out)+errText(err))
	}
	// The queued answers run once each, in order; then the answer of On again.
	want := []string{
		"systemctl daemon-reload: exit status 1: Failed to reload daemon: Access denied", "second\n", "on\n", "on\n",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the runs (-want +got):\n%s", diff)
	}
}

func TestRunnerStopsWithItsContext(t *testing.T) {
	var r nodeuptest.Runner
	ctx, cancel := context.WithCancel(t.Context())
	r.On("systemctl start nomad.service", func(ctx context.Context) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return []byte("partial"), nil // the context's end wins, as with a program that the runner stops
	})
	out, err := r.Run(ctx, "systemctl", "start", "nomad.service")
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Errorf("a command whose context ends: %q, %v; want no output and an error that matches context.Canceled",
			out, err)
	}
	// A command whose context has ended does not start, and is not recorded.
	if _, err := r.Run(ctx, "systemctl", "daemon-reload"); !errors.Is(err, context.Canceled) {
		t.Errorf("a command after the context ended: %v, want an error that matches context.Canceled", err)
	}
	if diff := cmp.Diff([]string{"systemctl start nomad.service"}, r.Commands()); diff != "" {
		t.Errorf("Commands() (-want +got):\n%s", diff)
	}
}

func TestNewHost(t *testing.T) {
	f, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	h := nodeuptest.NewHost("prod-core-0", f, r)
	if h.FS != f || h.Runner != r {
		t.Error("the host does not use the given fakes")
	}
	if h.HostName != "prod-core-0" || h.EUID != 0 || h.OS != "linux" || h.Arch != "amd64" ||
		h.Version != nodeuptest.Version || h.Log != nil {
		t.Errorf("host = %+v, want a root process named prod-core-0 on linux/amd64 at nodeuptest.Version", h)
	}
	if first, second := h.Now(), h.Now(); first.IsZero() || !first.Equal(second) || first.Location() != time.UTC {
		t.Errorf("the clock read %v and then %v, want a time in UTC that stands still", first, second)
	}
	// A test serves what the machine downloads; nothing reaches the network.
	if h.Transport == nil {
		t.Fatal("the host has no transport, so it would use the network")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://github.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.Transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if want := "nodeuptest: the machine has no network: serve the files with httptest"; errText(err) != want {
		t.Errorf("a request: %q, want %q", errText(err), want)
	}
}

func TestTerminated(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(nodeuptest.Terminated)
	err := context.Cause(ctx)
	if !errors.Is(err, context.Canceled) || errText(err) != "terminated signal received" {
		t.Errorf("the cause of SIGTERM is %v, want %q, which matches context.Canceled", err,
			"terminated signal received")
	}
}

// errText returns the error's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
