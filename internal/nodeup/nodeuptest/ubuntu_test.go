package nodeuptest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

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
		"/run/systemd/system", "/etc/systemd/system", "/etc/modules-load.d", "/etc/sysctl.d", "/var/lib", "/etc/tent",
		"/opt",
	} {
		if e, ok := fsys.Entry(dir); !ok || !e.Dir || e.Mode != 0o755 || e.Owner != "root:root" {
			t.Errorf("%s is %+v, want a directory with mode 0755 owned by root:root", dir, e)
		}
	}
	// ufw is installed and enabled, as on Vultr's images.
	if e, ok := fsys.Entry("/etc/ufw/ufw.conf"); !ok || !strings.Contains(string(e.Data), "\nENABLED=yes\n") ||
		e.Mode != 0o644 || e.Owner != "root:root" {
		t.Errorf("/etc/ufw/ufw.conf is %+v, want a file with ENABLED=yes, mode 0644 and owned by root:root", e)
	}
	if e, ok := fsys.Entry("/usr/lib/systemd/system/ufw.service"); !ok || e.Dir {
		t.Errorf("/usr/lib/systemd/system/ufw.service is %+v, want ufw's unit file", e)
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

func TestUbuntuUFW(t *testing.T) {
	fsys, r, sd := ubuntu(t)
	ctx := t.Context()
	enabled := func(when, want string) {
		t.Helper()
		if state, _, err := sd.IsEnabled(ctx, "ufw.service"); state != want || err != nil {
			t.Errorf("is-enabled ufw.service %s: %q, %v; want %q", when, state, err, want)
		}
	}
	enabled("at first", "enabled")
	out, err := r.Run(ctx, "ufw", "disable")
	if string(out) != "Firewall stopped and disabled on system startup\n" || err != nil {
		t.Errorf("ufw disable: %q, %v", out, err)
	}
	conf, _ := fsys.Entry("/etc/ufw/ufw.conf")
	if !strings.Contains(string(conf.Data), "\nENABLED=no\n") || strings.Contains(string(conf.Data), "ENABLED=yes") {
		t.Errorf("ufw disable left the conf:\n%s", conf.Data)
	}
	if want := []string{"/etc/ufw/ufw.conf"}; !slices.Equal(fsys.Changes(), want) {
		t.Errorf("Changes() = %q, want %q", fsys.Changes(), want)
	}
	// ufw disable leaves the unit enabled; at boot it does nothing while the conf says no.
	enabled("after ufw disable", "enabled")
	if err := sd.Disable(ctx, "ufw.service"); err != nil {
		t.Fatal(err)
	}
	enabled("after systemctl disable", "disabled")
}

func TestUbuntuFirewalld(t *testing.T) {
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	m := nodeuptest.Ubuntu(t, fsys, r)
	sd := nodeup.Systemd{Runner: r}
	ctx := t.Context()
	check := func(when, wantEnabled, wantActive string) {
		t.Helper()
		if state, _, err := sd.IsEnabled(ctx, "firewalld.service"); state != wantEnabled || err != nil {
			t.Errorf("is-enabled firewalld.service %s: %q, %v; want %q", when, state, err, wantEnabled)
		}
		if state, _, err := sd.IsActive(ctx, "firewalld.service"); state != wantActive || err != nil {
			t.Errorf("is-active firewalld.service %s: %q, %v; want %q", when, state, err, wantActive)
		}
	}
	check("before it is installed", "not-found", "inactive")
	want := "systemctl disable --now firewalld.service: exit status 1: Failed to disable unit: Unit file " +
		"firewalld.service does not exist."
	if err := sd.DisableNow(ctx, "firewalld.service"); err == nil || err.Error() != want {
		t.Errorf("DisableNow of a unit without a file: %v, want %q", err, want)
	}
	m.InstallFirewalld(t)
	check("once installed", "enabled", "active")
	if err := sd.DisableNow(ctx, "firewalld.service"); err != nil {
		t.Fatal(err)
	}
	check("after disable --now", "disabled", "inactive")
	if got := fsys.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %q, want none", got)
	}
}

// tentRuleset is a ruleset that replaces tent's table, with comment as the table's comment, or none when it is empty.
func tentRuleset(comment string) string {
	c := ""
	if comment != "" {
		c = "  comment \"" + comment + "\"\n"
	}
	return "# Rendered by tent-node. Do not edit: changes are overwritten.\ntable inet tent\ndelete table inet tent\n" +
		"table inet tent {\n" + c + "  chain input {\n    type filter hook input priority filter; policy drop;\n  }\n}\n"
}

func TestUbuntuNftables(t *testing.T) {
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	m := nodeuptest.Ubuntu(t, fsys, r)
	ctx := t.Context()
	// list returns the comment of tent's table, and whether the table is loaded.
	list := func() (string, bool) {
		t.Helper()
		out, err := r.Run(ctx, "nft", "-j", "list", "tables")
		if err != nil {
			t.Fatalf("nft list tables: %v", err)
		}
		var tables struct {
			Nftables []map[string]json.RawMessage `json:"nftables"`
		}
		if err := json.Unmarshal(out, &tables); err != nil {
			t.Fatalf("nft -j list tables: %v in %s", err, out)
		}
		// nft prints its metainfo first, then one object per table.
		if _, ok := tables.Nftables[0]["metainfo"]; !ok {
			t.Errorf("nft -j list tables printed no metainfo first: %s", out)
		}
		for _, object := range tables.Nftables[1:] {
			var table struct{ Family, Name, Comment string }
			if err := json.Unmarshal(object["table"], &table); err != nil {
				t.Fatalf("nft -j list tables printed %s, which is not a table", out)
			}
			if table.Family == "inet" && table.Name == "tent" {
				return table.Comment, true
			}
		}
		return "", false
	}
	load := func(ruleset string) error {
		t.Helper()
		fsys.AddFile(t, "/etc/tent/firewall.nft", []byte(ruleset), 0o600, "root:root")
		_, err := r.Run(ctx, "nft", "-f", "/etc/tent/firewall.nft")
		return err
	}

	if _, ok := list(); ok {
		t.Error("tent's table is loaded at first")
	}
	want := "nft -f /etc/tent/firewall.nft: exit status 1: Error: Could not open file \"/etc/tent/firewall.nft\": " +
		"No such file or directory"
	if _, err := r.Run(ctx, "nft", "-f", "/etc/tent/firewall.nft"); err == nil || err.Error() != want {
		t.Errorf("nft -f without the file: %v, want %q", err, want)
	}
	if err := load(tentRuleset("tent-node 1a2b")); err != nil {
		t.Fatal(err)
	}
	if comment, ok := list(); !ok || comment != "tent-node 1a2b" {
		t.Errorf("after nft -f: table %v with the comment %q, want tent-node 1a2b", ok, comment)
	}
	if err := load("table inet tent {\n}\n"); err == nil {
		t.Error("nft -f loaded a file that tent-node did not render")
	}
	if comment, _ := list(); comment != "tent-node 1a2b" {
		t.Errorf("a failed nft -f changed the comment to %q", comment)
	}
	if err := load(tentRuleset("")); err != nil {
		t.Fatal(err)
	}
	if comment, ok := list(); !ok || comment != "" {
		t.Errorf("a table without a comment: table %v with the comment %q, want none", ok, comment)
	}
	m.Reboot()
	if _, ok := list(); ok {
		t.Error("tent's table is loaded after a reboot")
	}
}

// Commands of Docker's install, as the runtime phase runs them.
const (
	dpkgQuery     = "dpkg-query -W -f=${Status} docker.io"
	dpkgConfigure = "env DEBIAN_FRONTEND=noninteractive dpkg --force-confdef --force-confold --configure -a"
	aptUpdate     = "env DEBIAN_FRONTEND=noninteractive apt-get update"
	aptInstall    = "env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " +
		"-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold docker.io"
)

// run runs the command, a program and its arguments separated by spaces, and returns what it printed.
func run(t *testing.T, r *nodeuptest.Runner, command string) (string, error) {
	t.Helper()
	fields := strings.Fields(command)
	out, err := r.Run(t.Context(), fields[0], fields[1:]...)
	return string(out), err
}

func TestUbuntuDocker(t *testing.T) {
	fsys, r, sd := ubuntu(t)
	ctx := t.Context()
	check := func(when, wantEnabled, wantActive string) {
		t.Helper()
		if state, _, err := sd.IsEnabled(ctx, "docker.service"); state != wantEnabled || err != nil {
			t.Errorf("is-enabled docker.service %s: %q, %v; want %q", when, state, err, wantEnabled)
		}
		if state, _, err := sd.IsActive(ctx, "docker.service"); state != wantActive || err != nil {
			t.Errorf("is-active docker.service %s: %q, %v; want %q", when, state, err, wantActive)
		}
	}
	// dpkg-query answers so for a package that dpkg has never had.
	out, err := run(t, r, dpkgQuery)
	want := dpkgQuery + ": exit status 1: dpkg-query: no packages found matching docker.io"
	if exit, ok := errors.AsType[*nodeup.ExitError](err); out != "" || !ok || exit.Code != 1 || err.Error() != want {
		t.Errorf("dpkg-query before the install: %q, %v; want no output and %q", out, err, want)
	}
	check("before the install", "not-found", "inactive")
	want = "systemctl restart docker.service: exit status 5: Failed to restart docker.service: Unit docker.service " +
		"not found."
	if err := sd.Restart(ctx, "docker.service"); err == nil || err.Error() != want {
		t.Errorf("restart before the install: %v, want %q", err, want)
	}
	for _, command := range []string{dpkgConfigure, aptUpdate, aptInstall} {
		if out, err := run(t, r, command); out != "" || err != nil {
			t.Errorf("%s: %q, %v; want no output and no error", command, out, err)
		}
	}
	if out, err := run(t, r, dpkgQuery); out != "install ok installed" || err != nil {
		t.Errorf("dpkg-query after the install: %q, %v; want install ok installed", out, err)
	}
	// The package's postinst enables and starts Docker.
	check("after the install", "enabled", "active")
	if diff := cmp.Diff([]string{"/usr/lib/systemd/system/docker.service"}, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if err := sd.Restart(ctx, "docker.service"); err != nil {
		t.Errorf("restart after the install: %v", err)
	}
	check("after a restart", "enabled", "active")
}

func TestUbuntuDockerAfterAReboot(t *testing.T) {
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	m := nodeuptest.Ubuntu(t, fsys, r)
	sd := nodeup.Systemd{Runner: r}
	ctx := t.Context()
	job := func(when string, want bool) {
		t.Helper()
		if got, err := sd.HasJob(ctx, "docker.service"); got != want || err != nil {
			t.Errorf("a job for docker.service %s: %v, %v; want %v", when, got, err, want)
		}
	}
	active := func(when, want string) {
		t.Helper()
		if state, _, err := sd.IsActive(ctx, "docker.service"); state != want || err != nil {
			t.Errorf("is-active docker.service %s: %q, %v; want %q", when, state, err, want)
		}
	}
	// Without Docker, a reboot queues nothing.
	m.Reboot()
	job("before the install", false)
	if _, err := run(t, r, aptInstall); err != nil {
		t.Fatal(err)
	}
	job("after the install", false)
	// The enabled unit starts on the same boot as tent-node.service, which is not ordered after it.
	m.Reboot()
	active("after a reboot", "inactive")
	job("after a reboot", true)
	if state, _, _ := sd.IsEnabled(ctx, "docker.service"); state != "enabled" {
		t.Errorf("is-enabled docker.service after a reboot: %q, want enabled", state)
	}
	// systemctl start waits for the queued job.
	if err := sd.Start(ctx, "docker.service"); err != nil {
		t.Fatal(err)
	}
	active("after start", "active")
	job("after start", false)
	// A disabled unit does not start at boot: it gets no job, and is inactive.
	if _, err := run(t, r, "systemctl disable docker.service"); err != nil {
		t.Fatal(err)
	}
	m.Reboot()
	job("after a reboot of a disabled Docker", false)
	active("after a reboot of a disabled Docker", "inactive")
}

func TestUbuntuLocks(t *testing.T) {
	// As apt 2.8 and dpkg 1.22 on Ubuntu 24.04 answer while another process holds the lock.
	cases := []struct {
		name    string
		lock    func(m *nodeuptest.Machine, tries int)
		command string
		code    int
		stderr  string
	}{
		{"package lists", (*nodeuptest.Machine).LockAptLists, aptUpdate, 100,
			"E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1234 (apt-get)\n" +
				"E: Unable to lock directory /var/lib/apt/lists/"},
		{"dpkg", (*nodeuptest.Machine).LockDpkg, dpkgConfigure, 2,
			"dpkg: error: dpkg frontend lock was locked by another process with pid 1234\n" +
				"Note: removing the lock file is always wrong, can damage the locked area\n" +
				"and the entire system. See <https://wiki.debian.org/Teams/Dpkg/FAQ#db-lock>."},
		{"archives", (*nodeuptest.Machine).LockAptArchives, aptInstall, 100,
			"E: Could not get lock /var/cache/apt/archives/lock. It is held by process 1234 (apt-get)\n" +
				"E: Unable to lock directory /var/cache/apt/archives/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
			m := nodeuptest.Ubuntu(t, fsys, r)
			c.lock(m, 2)
			want := fmt.Sprintf("%s: exit status %d: %s", c.command, c.code, c.stderr)
			for try := 1; try <= 2; try++ {
				_, err := run(t, r, c.command)
				if exit, ok := errors.AsType[*nodeup.ExitError](err); !ok || exit.Code != c.code || err.Error() != want {
					t.Errorf("try %d: %v; want %q", try, err, want)
				}
			}
			if got := fsys.Changes(); len(got) != 0 {
				t.Errorf("the failed tries changed %q, want nothing", got)
			}
			if _, err := run(t, r, c.command); err != nil {
				t.Errorf("once the lock is free: %v", err)
			}
		})
	}
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
