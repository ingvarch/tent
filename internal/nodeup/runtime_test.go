package nodeup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// Paths and commands of the runtime phase.
const (
	dockerDir      = "/etc/docker"
	dockerConfPath = dockerDir + "/daemon.json"
	dockerUnitPath = "/usr/lib/systemd/system/docker.service" // the package's
	dpkgQuery      = "dpkg-query -W -f=${Status} docker.io"
	dpkgConfigure  = "env DEBIAN_FRONTEND=noninteractive dpkg --force-confdef --force-confold --configure -a"
	aptUpdate      = "env DEBIAN_FRONTEND=noninteractive apt-get update"
	aptInstall     = "env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " +
		"-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold docker.io"
	dockerEnabled = "systemctl is-enabled docker.service"
	dockerActive  = "systemctl is-active docker.service"
	dockerRestart = "systemctl restart docker.service"
	dockerJob     = "systemctl show -p Job --value docker.service"
	dockerStart   = "systemctl start docker.service"
)

// oldDockerConf is a daemon.json with other settings than tent's.
const oldDockerConf = "{\n  \"log-driver\": \"journald\"\n}\n"

// runRuntime runs the runtime phase.
func runRuntime(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig) (nodeup.Result, error) {
	t.Helper()
	return runPhase(t, "runtime", h, nc, &nodeuptest.Environment{})
}

// captureLog makes h log into the buffer it returns, as text.
func captureLog(h *nodeup.Host) *bytes.Buffer {
	var logs bytes.Buffer
	h.Log = slog.New(slog.NewTextHandler(&logs, nil))
	return &logs
}

// warnings returns the lines of logs at the level WARN.
func warnings(logs *bytes.Buffer) []string {
	var lines []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, " level=WARN ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// installedDocker returns a fake Ubuntu machine on which the runtime phase has installed Docker for the combined
// config, as ubuntuMachine returns it, and the config.
func installedDocker(t *testing.T) (
	*nodeup.Host, *nodeuptest.FS, *nodeuptest.Runner, *nodeuptest.Machine, *nodeconfig.NodeConfig,
) {
	t.Helper()
	h, fsys, r, m := ubuntuMachine(t)
	nc := combined(t)
	if _, err := runRuntime(t, h, nc); err != nil {
		t.Fatal(err)
	}
	return h, fsys, r, m, nc
}

func TestRuntime(t *testing.T) {
	h, fsys, r, _ := ubuntuMachine(t)
	logs := captureLog(h)
	res, err := runRuntime(t, h, combined(t))
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("runtime: %+v, %v; want done", res, err)
	}
	// The package enables and starts Docker.
	want := []string{dpkgQuery, dpkgConfigure, aptUpdate, aptInstall, dockerEnabled, dockerActive}
	if diff := cmp.Diff(want, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	// daemon.json comes before the package, so that Docker's first start reads it.
	if diff := cmp.Diff([]string{dockerDir, dockerConfPath, dockerUnitPath}, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	checkEntry(t, fsys, dockerConfPath, "daemon.json.golden")
	if dir, _ := fsys.Entry(dockerDir); !dir.Dir || dir.Mode != 0o755 || dir.Owner != nodeconfig.Owner {
		t.Errorf("%s is %+v, want a directory with mode 0755 owned by root:root", dockerDir, dir)
	}
	// Docker takes only strings as log-opts.
	conf, _ := fsys.Entry(dockerConfPath)
	var got map[string]any
	if err := json.Unmarshal(conf.Data, &got); err != nil {
		t.Fatalf("daemon.json: %v", err)
	}
	wantConf := map[string]any{
		"live-restore": true, "log-driver": "json-file",
		"log-opts": map[string]any{"max-file": "3", "max-size": "10m"},
	}
	if diff := cmp.Diff(wantConf, got); diff != "" {
		t.Errorf("daemon.json (-want +got):\n%s", diff)
	}
	if !strings.Contains(logs.String(), ` level=INFO msg="install Docker"`) || len(warnings(logs)) != 0 {
		t.Errorf("the log does not say that runtime installs Docker, or warns:\n%s", logs)
	}
}

func TestRuntimeSecondRunChangesNothing(t *testing.T) {
	h, fsys, r, _, nc := installedDocker(t)
	changes, commands := len(fsys.Changes()), len(r.Commands())
	res, err := runRuntime(t, h, nc)
	if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
		t.Errorf("the second run: %+v, %v; want unchanged", res, err)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second run changed %q, want nothing", got)
	}
	if diff := cmp.Diff([]string{dpkgQuery, dockerEnabled, dockerActive}, r.Commands()[commands:]); diff != "" {
		t.Errorf("the second run's commands (-want +got):\n%s", diff)
	}
}

func TestRuntimeKeepsAnInstalledDocker(t *testing.T) {
	// The first word is the selection: apt-mark hold makes it hold, and a remove that has not run yet deinstall.
	for _, status := range []string{"hold ok installed", "deinstall ok installed"} {
		t.Run(status, func(t *testing.T) {
			h, fsys, r, _, nc := installedDocker(t)
			r.On(dpkgQuery, nodeuptest.Output(status))
			changes, commands := len(fsys.Changes()), len(r.Commands())
			res, err := runRuntime(t, h, nc)
			if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
				t.Errorf("runtime: %+v, %v; want unchanged", res, err)
			}
			if diff := cmp.Diff([]string{dpkgQuery, dockerEnabled, dockerActive}, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
			if got := fsys.Changes()[changes:]; len(got) != 0 {
				t.Errorf("runtime changed %q, want nothing", got)
			}
		})
	}
}

func TestRuntimeFailsOnAConfigItCannotWrite(t *testing.T) {
	h, fsys, r, _ := ubuntuMachine(t)
	fsys.Fail(dockerConfPath, fs.ErrPermission)
	_, err := runRuntime(t, h, combined(t))
	if want := "configure Docker: write " + dockerConfPath + ": permission denied"; errText(err) != want {
		t.Errorf("runtime: %q, want %q", errText(err), want)
	}
	if diff := cmp.Diff([]string{dpkgQuery}, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
}

func TestRuntimeRestartsDockerForANewConfig(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, fsys *nodeuptest.FS)
	}{
		{"other settings", func(t *testing.T, fsys *nodeuptest.FS) {
			fsys.AddFile(t, dockerConfPath, []byte(oldDockerConf), 0o644, nodeconfig.Owner)
		}},
		{"no daemon.json", func(t *testing.T, fsys *nodeuptest.FS) {
			if _, err := fsys.Remove(dockerConfPath); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _, nc := installedDocker(t)
			c.edit(t, fsys)
			changes, commands := len(fsys.Changes()), len(r.Commands())
			res, err := runRuntime(t, h, nc)
			if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
				t.Fatalf("runtime: %+v, %v; want done", res, err)
			}
			want := []string{dpkgQuery, dockerRestart, dockerEnabled, dockerActive}
			if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{dockerConfPath}, fsys.Changes()[changes:]); diff != "" {
				t.Errorf("changes (-want +got):\n%s", diff)
			}
			checkEntry(t, fsys, dockerConfPath, "daemon.json.golden")
		})
	}
}

func TestRuntimeRetriesAFailedRestart(t *testing.T) {
	h, fsys, r, _, nc := installedDocker(t)
	fsys.AddFile(t, dockerConfPath, []byte(oldDockerConf), 0o644, nodeconfig.Owner)
	const failed = "Job for docker.service failed because the control process exited with error code."
	r.On(dockerRestart, nodeuptest.Exit(1, failed))
	res, err := runRuntime(t, h, nc)
	if want := "configure Docker: " + dockerRestart + ": exit status 1: " + failed; errText(err) != want ||
		res != (nodeup.Result{}) {
		t.Errorf("runtime: %+v, %q; want no result and %q", res, errText(err), want)
	}
	// The file goes, so that the next run writes it again and restarts Docker again.
	if _, ok := fsys.Entry(dockerConfPath); ok {
		t.Errorf("%s is left after the restart failed", dockerConfPath)
	}
	r.On(dockerRestart, nodeuptest.Output(""))
	if res, err := runRuntime(t, h, nc); err != nil || res.Status != nodeup.Done {
		t.Fatalf("the next run: %+v, %v; want done", res, err)
	}
	checkEntry(t, fsys, dockerConfPath, "daemon.json.golden")
	if n := countOf(r.Commands(), dockerRestart); n != 2 {
		t.Errorf("%s ran %d times, want twice", dockerRestart, n)
	}
}

func TestRuntimeRetriesTheInstall(t *testing.T) {
	// Another apt, such as unattended-upgrades on the first boot, holds a lock that the command does not wait for.
	cases := []struct {
		name, command string
		lock          func(m *nodeuptest.Machine, tries int)
	}{
		{"dpkg --configure", dpkgConfigure, (*nodeuptest.Machine).LockDpkg},
		{"apt-get update", aptUpdate, (*nodeuptest.Machine).LockAptLists},
		{"apt-get install", aptInstall, (*nodeuptest.Machine).LockAptArchives},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, _, r, m := ubuntuMachine(t)
				logs := captureLog(h)
				c.lock(m, 2)
				start := time.Now()
				res, err := runRuntime(t, h, combined(t))
				if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
					t.Fatalf("runtime: %+v, %v; want done", res, err)
				}
				// It tries again 5 seconds after each failure.
				if waited := time.Since(start); waited != 10*time.Second {
					t.Errorf("runtime took %s, want 10s", waited)
				}
				var want []string
				for _, command := range []string{dpkgQuery, dpkgConfigure, aptUpdate, aptInstall} {
					want = append(want, command)
					if command == c.command {
						want = append(want, command, command)
					}
				}
				want = append(want, dockerEnabled, dockerActive)
				if diff := cmp.Diff(want, r.Commands()); diff != "" {
					t.Errorf("commands (-want +got):\n%s", diff)
				}
				checkRetryWarning(t, logs, c.command)
			})
		})
	}
}

// checkRetryWarning fails t unless logs hold one warning, of the first failure of the command, which says how it
// tries again.
func checkRetryWarning(t *testing.T, logs *bytes.Buffer, command string) {
	t.Helper()
	lines := warnings(logs)
	want := `msg="command failed, trying again" command="` + command + `" error=`
	if len(lines) != 1 || !strings.Contains(lines[0], want) || !strings.HasSuffix(lines[0], " every=5s within=10m0s\n") {
		t.Errorf("warnings %q, want one with %q that ends with every=5s within=10m0s", lines, want)
	}
}

func TestRuntimeGivesUpOnTheInstall(t *testing.T) {
	// stderr holds the try's number in %d, so that the error shows which try it is.
	cases := []struct {
		name, command, stderr string
		code                  int
		commands              []string // the commands before it
	}{
		{"dpkg --configure", dpkgConfigure, "dpkg: error: dpkg frontend lock was locked by another process with pid %d",
			2, []string{dpkgQuery}},
		{"apt-get update", aptUpdate, "E: Could not get lock /var/lib/apt/lists/lock. It is held by process %d " +
			"(unattended-upgr)", 100, []string{dpkgQuery, dpkgConfigure}},
		{"apt-get install", aptInstall, "E: Could not get lock /var/cache/apt/archives/lock. It is held by process %d " +
			"(apt-get)", 100, []string{dpkgQuery, dpkgConfigure, aptUpdate}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, fsys, r, _ := ubuntuMachine(t)
				logs := captureLog(h)
				tries := 0
				r.On(c.command, func(context.Context) ([]byte, error) {
					tries++
					return nil, &nodeup.ExitError{Code: c.code, Stderr: fmt.Sprintf(c.stderr, 1000+tries)}
				})
				start := time.Now()
				res, err := runRuntime(t, h, combined(t))
				// The error is the last try's.
				want := fmt.Sprintf("install Docker: gave up after 10m0s: %s: exit status %d: "+c.stderr, c.command,
					c.code, 1121)
				if errText(err) != want || res != (nodeup.Result{}) {
					t.Errorf("runtime: %+v, %q; want no result and %q", res, errText(err), want)
				}
				// A try every 5 seconds, from the first at 0s to the last at 10m0s.
				if waited := time.Since(start); waited != 10*time.Minute || tries != 121 {
					t.Errorf("runtime gave up after %s and %d tries, want 10m0s and 121", waited, tries)
				}
				if diff := cmp.Diff(c.commands, r.Commands()[:len(c.commands)]); diff != "" {
					t.Errorf("the commands before it (-want +got):\n%s", diff)
				}
				if got := r.Commands()[len(c.commands)+tries:]; len(got) != 0 {
					t.Errorf("runtime ran %q after it gave up, want nothing", got)
				}
				// daemon.json stays for Docker's first start, which the package makes on the next run.
				checkEntry(t, fsys, dockerConfPath, "daemon.json.golden")
				checkRetryWarning(t, logs, c.command)
			})
		})
	}
}

func TestRuntimeStopsWaitingWhenTheContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, r, m := ubuntuMachine(t)
		m.LockAptLists(1000)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(7*time.Second, cancel) // during the wait after the second try
		start := time.Now()
		res, err := phaseNamed(t, "runtime", &nodeuptest.Environment{}).Run(ctx, h, combined(t))
		// The error keeps the last try's, which names the lock's holder.
		want := "install Docker: " + aptUpdate + ": exit status 100: E: Could not get lock " +
			"/var/lib/apt/lists/lock. It is held by process 1234 (apt-get)\nE: Unable to lock directory " +
			"/var/lib/apt/lists/; stopped waiting: context canceled"
		if !errors.Is(err, context.Canceled) || errText(err) != want || res != (nodeup.Result{}) {
			t.Errorf("runtime: %+v, %q; want no result and %q", res, errText(err), want)
		}
		if waited := time.Since(start); waited != 7*time.Second {
			t.Errorf("runtime returned after %s, want at once when its context ended, after 7s", waited)
		}
		if n := countOf(r.Commands(), aptUpdate); n != 2 {
			t.Errorf("apt-get update ran %d times, want twice", n)
		}
	})
}

func TestRuntimeStopsWhenTheContextEndsDuringATry(t *testing.T) {
	h, _, r, _ := ubuntuMachine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.On(aptUpdate, func(context.Context) ([]byte, error) {
		cancel()
		return nil, &nodeup.ExitError{Code: -1}
	})
	res, err := phaseNamed(t, "runtime", &nodeuptest.Environment{}).Run(ctx, h, combined(t))
	if want := "install Docker: " + aptUpdate + ": context canceled"; !errors.Is(err, context.Canceled) ||
		errText(err) != want || res != (nodeup.Result{}) {
		t.Errorf("runtime: %+v, %q; want no result and %q", res, errText(err), want)
	}
	if n := countOf(r.Commands(), aptUpdate); n != 1 {
		t.Errorf("apt-get update ran %d times, want once", n)
	}
}

func TestRuntimeSkipsANodeWithoutDocker(t *testing.T) {
	noDocker := combined(t)
	noDocker.System.Docker = false
	cases := []struct {
		name string
		nc   *nodeconfig.NodeConfig
	}{
		{"server", server(t)},
		{"combined without the docker driver", rehash(t, noDocker)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _ := ubuntuMachine(t)
			res, err := runRuntime(t, h, c.nc)
			want := nodeup.Result{Status: nodeup.Skipped, Reason: "this node group runs no Docker"}
			if err != nil || res != want {
				t.Errorf("runtime: %+v, %v; want %+v", res, err, want)
			}
			if len(r.Commands()) != 0 || len(fsys.Changes()) != 0 {
				t.Errorf("runtime ran %q and changed %q, want nothing", r.Commands(), fsys.Changes())
			}
		})
	}
}

func TestRuntimeInstallsDockerThatIsNotInstalled(t *testing.T) {
	// A remove leaves deinstall ok config-files; an install that stopped halfway leaves unpacked, half-configured or
	// half-installed, which dpkg --configure -a or apt-get install finishes.
	for _, status := range []string{
		"deinstall ok config-files", "install ok unpacked", "install ok half-configured",
		"install reinstreq half-installed",
	} {
		t.Run(status, func(t *testing.T) {
			h, _, r, _ := ubuntuMachine(t)
			r.On(dpkgQuery, nodeuptest.Output(status))
			res, err := runRuntime(t, h, combined(t))
			if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
				t.Fatalf("runtime: %+v, %v; want done", res, err)
			}
			want := []string{dpkgQuery, dpkgConfigure, aptUpdate, aptInstall, dockerEnabled, dockerActive}
			if diff := cmp.Diff(want, r.Commands()); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRuntimeFailsWhenDpkgQueryFails(t *testing.T) {
	cases := []struct {
		name   string
		answer nodeuptest.Answer
		want   string
	}{
		{"exit status 2", nodeuptest.Exit(2, "dpkg-query: error: parsing file '/var/lib/dpkg/status' near line 42"),
			"check for Docker: " + dpkgQuery + ": exit status 2: dpkg-query: error: parsing file " +
				"'/var/lib/dpkg/status' near line 42"},
		{"no dpkg-query", func(context.Context) ([]byte, error) {
			return nil, errors.New(`exec: "dpkg-query": executable file not found in $PATH`)
		}, "check for Docker: " + dpkgQuery + `: exec: "dpkg-query": executable file not found in $PATH`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _ := ubuntuMachine(t)
			r.On(dpkgQuery, c.answer)
			res, err := runRuntime(t, h, combined(t))
			if errText(err) != c.want || res != (nodeup.Result{}) {
				t.Errorf("runtime: %+v, %q; want no result and %q", res, errText(err), c.want)
			}
			if diff := cmp.Diff([]string{dpkgQuery}, r.Commands()); diff != "" || len(fsys.Changes()) != 0 {
				t.Errorf("runtime changed %q; commands (-want +got):\n%s", fsys.Changes(), diff)
			}
		})
	}
}

func TestRuntimeStartsDocker(t *testing.T) {
	const failed = "Job for docker.service failed because the control process exited with error code."
	cases := []struct {
		name     string
		answers  map[string]nodeuptest.Answer
		commands []string
		err      string
	}{
		{"disabled", map[string]nodeuptest.Answer{dockerEnabled: nodeuptest.ExitOutput(1, "disabled\n", "")},
			[]string{dpkgQuery, dockerEnabled, "systemctl enable docker.service", dockerActive}, ""},
		// Without a job, nothing else starts Docker.
		{"inactive", map[string]nodeuptest.Answer{dockerActive: inactive},
			[]string{dpkgQuery, dockerEnabled, dockerActive, dockerJob, dockerStart}, ""},
		{"failed", map[string]nodeuptest.Answer{dockerActive: nodeuptest.ExitOutput(3, "failed\n", "")},
			[]string{dpkgQuery, dockerEnabled, dockerActive, dockerJob, dockerStart}, ""},
		// Waiting out RestartSec after a crash: systemd restarts it; start waits for that (255) or cuts it short (259).
		{"activating without a job", map[string]nodeuptest.Answer{
			dockerActive: nodeuptest.ExitOutput(3, "activating\n", ""),
		}, []string{dpkgQuery, dockerEnabled, dockerActive, dockerJob, dockerStart}, ""},
		{"enable fails", map[string]nodeuptest.Answer{
			dockerEnabled:                     nodeuptest.ExitOutput(1, "disabled\n", ""),
			"systemctl enable docker.service": nodeuptest.Exit(1, "Failed to enable unit: Unit docker.service is masked."),
		}, []string{dpkgQuery, dockerEnabled, "systemctl enable docker.service"},
			"start Docker: systemctl enable docker.service: exit status 1: Failed to enable unit: Unit " +
				"docker.service is masked."},
		{"start fails", map[string]nodeuptest.Answer{dockerActive: inactive, dockerStart: nodeuptest.Exit(1, failed)},
			[]string{dpkgQuery, dockerEnabled, dockerActive, dockerJob, dockerStart},
			"start Docker: systemctl start docker.service: exit status 1: " + failed},
		{"job query fails", map[string]nodeuptest.Answer{
			dockerActive: inactive,
			dockerJob:    nodeuptest.Exit(1, "Failed to connect to bus: No such file or directory"),
		}, []string{dpkgQuery, dockerEnabled, dockerActive, dockerJob},
			"start Docker: " + dockerJob + ": exit status 1: Failed to connect to bus: No such file or directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _, nc := installedDocker(t)
			for command, answer := range c.answers {
				r.On(command, answer)
			}
			changes, commands := len(fsys.Changes()), len(r.Commands())
			res, err := runRuntime(t, h, nc)
			wantRes := nodeup.Result{Status: nodeup.Done}
			if c.err != "" {
				wantRes = nodeup.Result{}
			}
			if errText(err) != c.err || res != wantRes {
				t.Errorf("runtime: %+v, %q; want %+v and %q", res, errText(err), wantRes, c.err)
			}
			if diff := cmp.Diff(c.commands, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
			if got := fsys.Changes()[changes:]; len(got) != 0 {
				t.Errorf("runtime changed %q, want nothing", got)
			}
		})
	}
}

func TestRuntimeWaitsForDockerThatSystemdStarts(t *testing.T) {
	// systemd has a job for Docker: it starts Docker on its own, and runtime changes nothing.
	cases := []struct {
		name  string
		setup func(m *nodeuptest.Machine, r *nodeuptest.Runner)
	}{
		// Its start job is still queued, as after a reboot on Ubuntu 24.04 (VM check 2026-09-30).
		{"queued after a reboot", func(m *nodeuptest.Machine, _ *nodeuptest.Runner) { m.Reboot() }},
		{"activating", func(_ *nodeuptest.Machine, r *nodeuptest.Runner) {
			r.On(dockerActive, nodeuptest.ExitOutput(3, "activating\n", ""))
			r.On(dockerJob, nodeuptest.Output("172\n"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, m, nc := installedDocker(t)
			c.setup(m, r)
			changes, commands := len(fsys.Changes()), len(r.Commands())
			res, err := runRuntime(t, h, nc)
			if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
				t.Errorf("runtime: %+v, %v; want unchanged", res, err)
			}
			// start waits until Docker is up, which the later phases need.
			want := []string{dpkgQuery, dockerEnabled, dockerActive, dockerJob, dockerStart}
			if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
			if got := fsys.Changes()[changes:]; len(got) != 0 {
				t.Errorf("runtime changed %q, want nothing", got)
			}
		})
	}
}
