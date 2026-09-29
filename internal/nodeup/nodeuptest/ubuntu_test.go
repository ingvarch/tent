package nodeuptest_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

func TestUbuntuFiles(t *testing.T) {
	fsys := nodeuptest.NewFS()
	nodeuptest.Ubuntu(t, fsys, &nodeuptest.Runner{})
	release, err := fsys.ReadFile("/etc/os-release")
	if err != nil || !strings.Contains(string(release), "\nID=ubuntu\n") ||
		!strings.Contains(string(release), "\nVERSION_ID=\"24.04\"\n") {
		t.Errorf("/etc/os-release: %q, %v; want Ubuntu 24.04", release, err)
	}
	for _, dir := range []string{
		"/run/systemd/system", "/etc/systemd/system", "/etc/modules-load.d", "/etc/sysctl.d", "/var/lib",
	} {
		if e, ok := fsys.Entry(dir); !ok || !e.Dir || e.Mode != 0o755 || e.Owner != "root:root" {
			t.Errorf("%s is %+v, want a directory with mode 0755 owned by root:root", dir, e)
		}
	}
	if got := fsys.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %q, want none", got)
	}
}

// ubuntu returns an Ubuntu machine with the units tent-node.service and tent-node-join.timer, and systemd over its
// runner.
func ubuntu(t *testing.T) (*nodeuptest.FS, *nodeuptest.Runner, nodeup.Systemd) {
	t.Helper()
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	nodeuptest.Ubuntu(t, fsys, r, "tent-node.service", "tent-node-join.timer")
	return fsys, r, nodeup.Systemd{Runner: r}
}

// addUnit writes a unit file with content, as a test's setup that records no change.
func addUnit(t *testing.T, fsys *nodeuptest.FS, unit, content string) {
	t.Helper()
	fsys.AddFile(t, "/etc/systemd/system/"+unit, []byte(content), 0o644, "root:root")
}

// isEnabled runs systemctl is-enabled of the unit and returns what it printed and its exit status.
func isEnabled(t *testing.T, r *nodeuptest.Runner, unit string) (stdout string, code int, stderr string) {
	t.Helper()
	out, err := r.Run(t.Context(), "systemctl", "is-enabled", unit)
	if err == nil {
		return string(out), 0, ""
	}
	exit, ok := errors.AsType[*nodeup.ExitError](err)
	if !ok {
		t.Fatalf("systemctl is-enabled %s: %v, want an exit status", unit, err)
	}
	return string(out), exit.Code, exit.Stderr
}

func TestUbuntuSystemd(t *testing.T) {
	fsys, r, sd := ubuntu(t)
	addUnit(t, fsys, "tent-node.service", "[Service]\n")
	addUnit(t, fsys, "tent-node-join.timer", "[Timer]\n")
	ctx := t.Context()
	check := func(unit, wantEnabled, wantActive string) {
		t.Helper()
		if state, _, err := sd.IsEnabled(ctx, unit); state != wantEnabled || err != nil {
			t.Errorf("is-enabled %s: %q, %v; want %q", unit, state, err, wantEnabled)
		}
		if state, _, err := sd.IsActive(ctx, unit); state != wantActive || err != nil {
			t.Errorf("is-active %s: %q, %v; want %q", unit, state, err, wantActive)
		}
	}
	check("tent-node.service", "disabled", "inactive")
	if err := sd.Enable(ctx, "tent-node.service"); err != nil {
		t.Fatal(err)
	}
	check("tent-node.service", "enabled", "inactive")
	if err := sd.Start(ctx, "tent-node.service"); err != nil {
		t.Fatal(err)
	}
	check("tent-node.service", "enabled", "active")
	check("tent-node-join.timer", "disabled", "inactive")
	if out, code, stderr := isEnabled(t, r, "tent-node-join.timer"); out != "disabled\n" || code != 1 || stderr != "" {
		t.Errorf("is-enabled of a unit with a file: %q, exit status %d, stderr %q; want disabled, 1 and none",
			out, code, stderr)
	}
}

func TestUbuntuSystemdWithoutTheUnitFile(t *testing.T) {
	_, r, sd := ubuntu(t)
	ctx := t.Context()
	cases := []struct {
		name string
		call func() error
		code int
		want string
	}{
		{"enable", func() error { return sd.Enable(ctx, "tent-node.service") }, 1,
			"systemctl enable tent-node.service: exit status 1: Failed to enable unit: Unit file tent-node.service " +
				"does not exist."},
		{"start", func() error { return sd.Start(ctx, "tent-node.service") }, 5,
			"systemctl start tent-node.service: exit status 5: Failed to start tent-node.service: Unit " +
				"tent-node.service not found."},
	}
	for _, c := range cases {
		err := c.call()
		if exit, ok := errors.AsType[*nodeup.ExitError](err); !ok || exit.Code != c.code || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// As systemd 255 and 259 answer on Vultr's Ubuntu 24.04 and 26.04 images (VM check 2026-09-29).
	if out, code, stderr := isEnabled(t, r, "tent-node.service"); out != "not-found\n" || code != 4 || stderr != "" {
		t.Errorf("is-enabled after a failed enable: %q, exit status %d, stderr %q; want not-found, 4 and none",
			out, code, stderr)
	}
	if state, _, _ := sd.IsActive(ctx, "tent-node.service"); state != "inactive" {
		t.Errorf("is-active after a failed start: %q, want inactive", state)
	}
}

func TestUbuntuNeedDaemonReload(t *testing.T) {
	fsys, r, sd := ubuntu(t)
	ctx := t.Context()
	needs := func(when string, want bool) {
		t.Helper()
		if got, err := sd.NeedsReload(ctx, "tent-node.service"); got != want || err != nil {
			t.Errorf("NeedDaemonReload %s: %v, %v; want %v", when, got, err, want)
		}
	}
	needs("without a unit file", false)
	addUnit(t, fsys, "tent-node.service", "[Service]\n")
	needs("of a new unit file, which systemd reads when asked", false)
	addUnit(t, fsys, "tent-node.service", "[Service]\nType=oneshot\n")
	needs("after the file changed", true)
	if err := sd.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	needs("after daemon-reload", false)
	addUnit(t, fsys, "tent-node.service", "[Service]\nType=simple\n")
	if err := sd.Enable(ctx, "tent-node.service"); err != nil {
		t.Fatal(err)
	}
	needs("after enable, which reloads", false)
	if _, err := fsys.Remove("/etc/systemd/system/tent-node.service"); err != nil {
		t.Fatal(err)
	}
	needs("once the file systemd read is gone", true)
	if got := r.Commands(); len(got) != 8 {
		t.Errorf("ran %q, want 8 commands", got)
	}
}

func TestUbuntuTimeSync(t *testing.T) {
	_, r, sd := ubuntu(t)
	ctx := t.Context()
	check := func(when, wantShow, wantTimesyncd string) {
		t.Helper()
		out, err := r.Run(ctx, "timedatectl", "show", "-p", "CanNTP", "-p", "NTP")
		if string(out) != wantShow || err != nil {
			t.Errorf("timedatectl show %s: %q, %v; want %q", when, out, err, wantShow)
		}
		if state, _, err := sd.IsActive(ctx, "systemd-timesyncd.service"); state != wantTimesyncd || err != nil {
			t.Errorf("systemd-timesyncd.service %s: %q, %v; want %q", when, state, err, wantTimesyncd)
		}
		if state, _, err := sd.IsActive(ctx, "chrony.service"); state != "inactive" || err != nil {
			t.Errorf("chrony.service %s: %q, %v; want inactive", when, state, err)
		}
	}
	check("before set-ntp", "CanNTP=yes\nNTP=no\n", "inactive")
	if _, err := r.Run(ctx, "timedatectl", "set-ntp", "true"); err != nil {
		t.Fatal(err)
	}
	check("after set-ntp true", "CanNTP=yes\nNTP=yes\n", "active")
}

func TestEnvironment(t *testing.T) {
	inst := env.Instance{
		ID: "9d1c6f3e-2b7a-4e58-8c0d-5a4f1e7b3c92", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.7"),
	}
	e := &nodeuptest.Environment{Instance: inst}
	var _ env.Environment = e
	if got, err := e.Read(t.Context()); got != inst || err != nil {
		t.Errorf("Read: %+v, %v; want %+v", got, err, inst)
	}
	boom := errors.New("vultr metadata: GET http://169.254.169.254/v1.json answered 404 Not Found")
	e.Err = boom
	if got, err := e.Read(t.Context()); !errors.Is(err, boom) || got != (env.Instance{}) {
		t.Errorf("Read with Err set: %+v, %v; want no instance and Err", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Read after the context ended: %v, want an error that matches context.Canceled", err)
	}
	if got := e.Reads(); got != 3 {
		t.Errorf("Reads() = %d, want 3", got)
	}
}

func TestEnvironmentSilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &nodeuptest.Environment{Silent: true}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		if _, err := e.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Read: %v, want an error that matches context.DeadlineExceeded", err)
		}
		if waited := time.Since(start); waited != time.Minute {
			t.Errorf("Read returned after %s, want when its context ended, after 1m0s", waited)
		}
	})
}
