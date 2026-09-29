package nodeup_test

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// units are the units that tent-node installs, and startUnits those of them that it enables and starts.
var (
	units      = []string{"tent-node.service", "tent-node-join.service", "tent-node-join.timer"}
	startUnits = []string{"tent-node.service", "tent-node-join.timer"}
)

// instance is the machine that the fake metadata service describes.
var instance = env.Instance{
	ID: "9d1c6f3e-2b7a-4e58-8c0d-5a4f1e7b3c92", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.7"),
}

// Paths of the files that the system phase writes.
const (
	modulesPath  = "/etc/modules-load.d/tent.conf"
	sysctlPath   = "/etc/sysctl.d/99-tent.conf"
	journaldDir  = "/etc/systemd/journald.conf.d"
	journaldPath = journaldDir + "/tent.conf"
)

// ubuntu returns a fake Ubuntu 24.04 machine named prod-core-0 that runs systemd, as machine and nodeuptest.Ubuntu
// make it, its filesystem and runner, and a metadata service that describes instance.
func ubuntu(t *testing.T) (*nodeup.Host, *nodeuptest.FS, *nodeuptest.Runner, *nodeuptest.Environment) {
	t.Helper()
	h, fsys, r := machine(t)
	nodeuptest.Ubuntu(t, fsys, r, units...)
	return h, fsys, r, &nodeuptest.Environment{Instance: instance}
}

// combined returns the sample with a tent-node asset of the fake host's version and the system settings of a
// combined node that runs Docker.
func combined(t *testing.T) *nodeconfig.NodeConfig {
	t.Helper()
	nc := sample(t)
	nc.Assets = []nodeconfig.Asset{{
		Name: nodeconfig.TentNodeAsset, Version: nodeuptest.Version,
		URLs:   []string{"https://github.com/ingvarch/tent/releases/download/v0.3.0/tent-node_linux_amd64"},
		SHA256: "3c7d1e9a5b2f8046c1e7a3d9b5f20864e1c7a3d9b5f20864e1c7a3d9b5f20864",
	}}
	nc.System = nodeconfig.System{
		Sysctls: map[string]string{
			"net.bridge.bridge-nf-call-arptables": "1", "net.bridge.bridge-nf-call-ip6tables": "1",
			"net.bridge.bridge-nf-call-iptables": "1",
		},
		KernelModules: []string{"br_netfilter", "overlay"},
		Docker:        true,
	}
	return rehash(t, nc)
}

// server returns the combined config as a server's, which has no system settings.
func server(t *testing.T) *nodeconfig.NodeConfig {
	t.Helper()
	nc := combined(t)
	nc.Role, nc.System = v1alpha1.RoleServer, nodeconfig.System{}
	return rehash(t, nc)
}

// rehash gives nc its spec hash, and fails t unless nc is valid.
func rehash(t *testing.T, nc *nodeconfig.NodeConfig) *nodeconfig.NodeConfig {
	t.Helper()
	nc.SpecHash = nodeconfig.SpecHash(nc)
	if err := nc.Validate(); err != nil {
		t.Fatal(err)
	}
	return nc
}

// runPhase runs the phase name of the phases on the metadata service e.
func runPhase(t *testing.T, name string, h *nodeup.Host, nc *nodeconfig.NodeConfig, e env.Environment) (
	nodeup.Result, error,
) {
	t.Helper()
	phases := nodeup.Phases(e)
	i := slices.IndexFunc(phases, func(p nodeup.Phase) bool { return p.Name == name })
	if i < 0 {
		t.Fatalf("no phase %s", name)
	}
	return phases[i].Run(t.Context(), h, nc)
}

// addUnits puts tent-node's units into fsys, as a test's setup that records no change.
func addUnits(t *testing.T, fsys *nodeuptest.FS) {
	t.Helper()
	for _, f := range renderUnits(t) {
		fsys.AddFile(t, f.Path, f.Content, fs.FileMode(f.Mode), f.Owner)
	}
}

// enableUnits puts tent-node's units into fsys and enables those that start at boot, as install does.
func enableUnits(t *testing.T, fsys *nodeuptest.FS, r *nodeuptest.Runner) {
	t.Helper()
	addUnits(t, fsys)
	for _, u := range startUnits {
		if err := (nodeup.Systemd{Runner: r}).Enable(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPhasesOrder(t *testing.T) {
	var names []string
	for _, p := range nodeup.Phases(&nodeuptest.Environment{}) {
		names = append(names, p.Name)
	}
	want := []string{"preflight", "system", "hostfirewall", "runtime", "cni", "join", "nomad", "verify"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("phases (-want +got):\n%s", diff)
	}
}

func TestPreflight(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			h, fsys, r, e := ubuntu(t)
			h.Arch = arch
			res, err := runPhase(t, "preflight", h, combined(t), e)
			if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
				t.Errorf("preflight: %+v, %v; want unchanged without a reason", res, err)
			}
			if h.Instance != instance || e.Reads() != 1 {
				t.Errorf("the host's instance is %+v after %d reads, want %+v after one", h.Instance, e.Reads(), instance)
			}
			if len(r.Commands()) != 0 || len(fsys.Changes()) != 0 {
				t.Errorf("preflight ran %q and changed %q, want nothing", r.Commands(), fsys.Changes())
			}
		})
	}
}

func TestPreflightRefuses(t *testing.T) {
	type edit func(h *nodeup.Host, fsys *nodeuptest.FS, nc *nodeconfig.NodeConfig, e *nodeuptest.Environment)
	osRelease := func(data string) edit {
		return func(_ *nodeup.Host, fsys *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			fsys.AddFile(t, "/etc/os-release", []byte(data), 0o644, nodeconfig.Owner)
		}
	}
	cases := []struct {
		name string
		edit edit
		want string
	}{
		{"not linux", func(h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			h.OS = "windows"
		}, "tent-node runs on linux, not on windows"},
		{"not amd64 or arm64", func(h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			h.Arch = "386"
		}, "tent-node runs on amd64 and arm64, not on 386"},
		{"not root", func(h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			h.EUID = 1000
		}, "tent-node runs as root, not as the user id 1000"},
		{"no systemd", func(_ *nodeup.Host, fsys *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			if _, err := fsys.Remove("/run/systemd/system"); err != nil {
				t.Fatal(err)
			}
		}, "systemd does not run the machine: /run/systemd/system is missing"},
		{"no os-release", func(_ *nodeup.Host, fsys *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			if _, err := fsys.Remove("/etc/os-release"); err != nil {
				t.Fatal(err)
			}
		}, "read the OS: open /etc/os-release: file does not exist"},
		{"debian", osRelease("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nVERSION_ID=\"13\"\nID=debian\n"),
			`the OS is "debian", not ubuntu: tent-node runs on Ubuntu`},
		{"no OS id", osRelease("NAME=\"Linux\"\n"), `the OS is "", not ubuntu: tent-node runs on Ubuntu`},
		{"host name", func(h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			h.HostName = "prod-core-9"
		}, "the host name is prod-core-9, not prod-core-0: the node config is another node's"},
		{"version", func(h *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, _ *nodeuptest.Environment) {
			h.Version = "v0.3.1"
		}, "tent-node is v0.3.1, not v0.3.0, the version of the node config's tent-node asset"},
		{"no tent-node asset", func(_ *nodeup.Host, _ *nodeuptest.FS, nc *nodeconfig.NodeConfig,
			_ *nodeuptest.Environment,
		) {
			nc.Assets = nil
			nc.SpecHash = nodeconfig.SpecHash(nc)
		}, "the node config has no tent-node asset, which gives the version of tent-node"},
		{"metadata", func(_ *nodeup.Host, _ *nodeuptest.FS, _ *nodeconfig.NodeConfig, e *nodeuptest.Environment) {
			e.Err = errors.New("vultr metadata: no instance-v2-id")
		}, "read the metadata service: vultr metadata: no instance-v2-id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, r, e := ubuntu(t)
			nc := combined(t)
			c.edit(h, h.FS.(*nodeuptest.FS), nc, e)
			res, err := runPhase(t, "preflight", h, nc, e)
			if errText(err) != c.want || res != (nodeup.Result{}) {
				t.Errorf("preflight: %+v, %q; want no result and %q", res, errText(err), c.want)
			}
			if h.Instance != (nodeup.Instance{}) {
				t.Errorf("the host's instance is %+v after a refusal, want none", h.Instance)
			}
			// The metadata service is read last, once the machine has passed every other check.
			wantReads := 0
			if c.name == "metadata" {
				wantReads = 1
			}
			if e.Reads() != wantReads {
				t.Errorf("the metadata service was read %d times, want %d", e.Reads(), wantReads)
			}
			if got := r.Commands(); len(got) != 0 {
				t.Errorf("preflight ran %q, want no commands", got)
			}
		})
	}
}

func TestPreflightGivesUpOnASilentMetadataService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, _, e := ubuntu(t)
		e.Silent = true
		var logs bytes.Buffer
		h.Log = slog.New(slog.NewTextHandler(&logs, nil))
		start := time.Now()
		_, err := runPhase(t, "preflight", h, combined(t), e)
		if want := "read the metadata service: no answer within 3m0s"; errText(err) != want {
			t.Errorf("preflight: %q, want %q", errText(err), want)
		}
		if waited := time.Since(start); waited != 3*time.Minute {
			t.Errorf("preflight gave up after %s, want 3m0s", waited)
		}
		// The log says what preflight waits for.
		if !strings.Contains(logs.String(), `msg="read the metadata service" within=3m0s`) {
			t.Errorf("the log does not say that preflight reads the metadata service:\n%s", logs.String())
		}
	})
}

func TestPreflightWarnsOnAnUntestedUbuntu(t *testing.T) {
	cases := []struct{ version, reason string }{
		{"24.04", ""},
		{"26.04", ""},
		{"22.04", "Ubuntu 22.04 is not tested: tent supports Ubuntu 24.04 and 26.04"},
		{"", "Ubuntu without a VERSION_ID is not tested: tent supports Ubuntu 24.04 and 26.04"},
	}
	for _, c := range cases {
		t.Run("VERSION_ID="+c.version, func(t *testing.T) {
			h, fsys, _, e := ubuntu(t)
			var logs bytes.Buffer
			h.Log = slog.New(slog.NewTextHandler(&logs, nil))
			release := strings.Replace(nodeuptest.OSRelease, `VERSION_ID="24.04"`, `VERSION_ID="`+c.version+`"`, 1)
			fsys.AddFile(t, "/etc/os-release", []byte(release), 0o644, nodeconfig.Owner)
			res, err := runPhase(t, "preflight", h, combined(t), e)
			if want := (nodeup.Result{Status: nodeup.Unchanged, Reason: c.reason}); err != nil || res != want {
				t.Errorf("preflight: %+v, %v; want %+v", res, err, want)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != (c.reason != "") {
				t.Errorf("logged a warning %v, want %v:\n%s", warned, c.reason != "", logs.String())
			}
			// The warning is logged before the metadata service is read, which may fail.
			logs.Reset()
			e.Err = errors.New("vultr metadata: no instance-v2-id")
			if _, err := runPhase(t, "preflight", h, combined(t), e); err == nil {
				t.Fatal("preflight passed without the metadata")
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != (c.reason != "") {
				t.Errorf("logged a warning %v before a failed read, want %v:\n%s", warned, c.reason != "",
					logs.String())
			}
		})
	}
}

// checkEntry fails t unless path is a file with mode 0644 owned by root:root, whose content is the golden file name.
func checkEntry(t *testing.T, fsys *nodeuptest.FS, path, golden string) {
	t.Helper()
	e, ok := fsys.Entry(path)
	if !ok || e.Dir || e.Mode != 0o644 || e.Owner != nodeconfig.Owner {
		t.Fatalf("%s is %+v, want a file with mode 0644 owned by root:root", path, e)
	}
	checkGolden(t, golden, e.Data)
}

func TestSystem(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	res, err := runPhase(t, "system", h, combined(t), e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("system: %+v, %v; want done", res, err)
	}
	// The modules come first: the bridge sysctls exist only once br_netfilter is loaded.
	wantCommands := []string{
		"modprobe br_netfilter", "modprobe overlay",
		"sysctl -p " + sysctlPath,
		"timedatectl show -p CanNTP -p NTP", "timedatectl set-ntp true",
		"systemctl restart systemd-journald.service",
	}
	if diff := cmp.Diff(wantCommands, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{modulesPath, sysctlPath, journaldDir, journaldPath}, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	checkEntry(t, fsys, modulesPath, "modules-load.conf.golden")
	checkEntry(t, fsys, sysctlPath, "sysctl.conf.golden")
	checkEntry(t, fsys, journaldPath, "journald.conf.golden")
	if dir, _ := fsys.Entry(journaldDir); !dir.Dir || dir.Mode != 0o755 || dir.Owner != nodeconfig.Owner {
		t.Errorf("%s is %+v, want a directory with mode 0755 owned by root:root", journaldDir, dir)
	}
}

func TestSystemSecondRunChangesNothing(t *testing.T) {
	for name, nc := range map[string]*nodeconfig.NodeConfig{"combined": combined(t), "server": server(t)} {
		t.Run(name, func(t *testing.T) {
			h, fsys, r, e := ubuntu(t)
			if _, err := runPhase(t, "system", h, nc, e); err != nil {
				t.Fatal(err)
			}
			changes, commands := len(fsys.Changes()), len(r.Commands())
			res, err := runPhase(t, "system", h, nc, e)
			if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
				t.Errorf("the second run: %+v, %v; want unchanged", res, err)
			}
			if got := fsys.Changes()[changes:]; len(got) != 0 {
				t.Errorf("the second run changed %q, want nothing", got)
			}
			// Only the check of the time sync runs again.
			if diff := cmp.Diff([]string{"timedatectl show -p CanNTP -p NTP"}, r.Commands()[commands:]); diff != "" {
				t.Errorf("the second run's commands (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSystemOfAServerWritesNoSystemSettings(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	res, err := runPhase(t, "system", h, server(t), e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Fatalf("system: %+v, %v; want done", res, err)
	}
	for _, p := range []string{modulesPath, sysctlPath} {
		if _, ok := fsys.Entry(p); ok {
			t.Errorf("%s exists on a server, which has no system settings", p)
		}
	}
	if diff := cmp.Diff([]string{journaldDir, journaldPath}, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	want := []string{
		"timedatectl show -p CanNTP -p NTP", "timedatectl set-ntp true",
		"systemctl restart systemd-journald.service",
	}
	if diff := cmp.Diff(want, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
}

// Answers of the time sync.
var (
	timedatedOff = nodeuptest.Output("CanNTP=yes\nNTP=no\n") // timedatectl can turn NTP on, and it is off
	noTimedated  = nodeuptest.Output("CanNTP=no\nNTP=no\n")  // timedatectl cannot, as on an image that runs chrony
	active       = nodeuptest.Output("active\n")
	inactive     = nodeuptest.ExitOutput(3, "inactive\n", "")
)

func TestSystemTimeSync(t *testing.T) {
	const (
		show      = "timedatectl show -p CanNTP -p NTP"
		chrony    = "systemctl is-active chrony.service"
		timesyncd = "systemctl is-active systemd-timesyncd.service"
	)
	cases := []struct {
		name     string
		answers  map[string]nodeuptest.Answer
		status   nodeup.Status
		commands []string
		err      string
	}{
		{"timesyncd on", nil, nodeup.Unchanged, []string{show}, ""},
		{"timesyncd off", map[string]nodeuptest.Answer{show: timedatedOff}, nodeup.Done,
			[]string{show, "timedatectl set-ntp true"}, ""},
		{"chrony active", map[string]nodeuptest.Answer{show: noTimedated, chrony: active}, nodeup.Unchanged,
			[]string{show, chrony}, ""},
		{"timesyncd active, which timedatectl does not manage", map[string]nodeuptest.Answer{
			show: noTimedated, chrony: inactive, timesyncd: active,
		}, nodeup.Unchanged, []string{show, chrony, timesyncd}, ""},
		{"no time service", map[string]nodeuptest.Answer{show: noTimedated, chrony: inactive, timesyncd: inactive},
			"", []string{show, chrony, timesyncd}, "no time sync: timedatectl cannot turn NTP on, and neither " +
				"chrony.service nor systemd-timesyncd.service is active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, r, e := ubuntu(t)
			nc := server(t)
			// The first run writes the journald drop-in and turns NTP on with timedatectl.
			if _, err := runPhase(t, "system", h, nc, e); err != nil {
				t.Fatal(err)
			}
			for command, answer := range c.answers {
				r.On(command, answer)
			}
			commands := len(r.Commands())
			res, err := runPhase(t, "system", h, nc, e)
			if res.Status != c.status || errText(err) != c.err {
				t.Errorf("system: %+v, %q; want %q and %q", res, errText(err), c.status, c.err)
			}
			if diff := cmp.Diff(c.commands, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSystemRetriesAFailedCommand(t *testing.T) {
	cases := []struct {
		command, file, want string
	}{
		{"modprobe overlay", modulesPath, "load kernel modules: modprobe overlay: exit status 1: modprobe: FATAL: " +
			"Module overlay not found."},
		{"sysctl -p " + sysctlPath, sysctlPath, "apply the sysctls: sysctl -p " + sysctlPath + ": exit status 1: " +
			"sysctl: cannot stat /proc/sys/net/bridge/bridge-nf-call-iptables: No such file or directory"},
		{"systemctl restart systemd-journald.service", journaldPath, "limit the journal: systemctl restart " +
			"systemd-journald.service: exit status 1: Job for systemd-journald.service failed."},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			h, fsys, r, e := ubuntu(t)
			stderr := c.want[strings.LastIndex(c.want, "exit status 1: ")+len("exit status 1: "):]
			r.On(c.command, nodeuptest.Exit(1, stderr))
			res, err := runPhase(t, "system", h, combined(t), e)
			if errText(err) != c.want || res != (nodeup.Result{}) {
				t.Errorf("system: %+v, %q; want no result and %q", res, errText(err), c.want)
			}
			// The file goes, so that the next run writes it again and runs the command again.
			if _, ok := fsys.Entry(c.file); ok {
				t.Errorf("%s is left after %s failed", c.file, c.command)
			}
			r.On(c.command, nodeuptest.Output(""))
			if res, err := runPhase(t, "system", h, combined(t), e); err != nil || res.Status != nodeup.Done {
				t.Fatalf("the next run: %+v, %v; want done", res, err)
			}
			if _, ok := fsys.Entry(c.file); !ok {
				t.Errorf("the next run did not write %s", c.file)
			}
			if n := countOf(r.Commands(), c.command); n != 2 {
				t.Errorf("%s ran %d times, want twice", c.command, n)
			}
		})
	}
}

func TestSystemTimeSyncFailures(t *testing.T) {
	cases := []struct {
		command, want string
	}{
		{"timedatectl show -p CanNTP -p NTP", "check the time sync: timedatectl show -p CanNTP -p NTP: " +
			"exit status 1: Failed to connect to bus: No such file or directory"},
		{"timedatectl set-ntp true", "turn on the time sync: timedatectl set-ntp true: exit status 1: Failed to " +
			"connect to bus: No such file or directory"},
		{"systemctl is-active chrony.service", "check the time sync: systemctl is-active chrony.service: exit " +
			"status 1: Failed to connect to bus: No such file or directory"},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			h, _, r, e := ubuntu(t)
			if c.command == "systemctl is-active chrony.service" {
				r.On("timedatectl show -p CanNTP -p NTP", noTimedated)
			}
			r.On(c.command, nodeuptest.Exit(1, "Failed to connect to bus: No such file or directory"))
			if _, err := runPhase(t, "system", h, server(t), e); errText(err) != c.want {
				t.Errorf("system: %q, want %q", errText(err), c.want)
			}
		})
	}
}

func TestSystemFailsOnAFileItCannotWrite(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	fsys.Fail(modulesPath, fs.ErrPermission)
	_, err := runPhase(t, "system", h, combined(t), e)
	if want := "load kernel modules: write " + modulesPath + ": permission denied"; errText(err) != want {
		t.Errorf("system: %q, want %q", errText(err), want)
	}
	if got := r.Commands(); len(got) != 0 {
		t.Errorf("system ran %q after a failed write, want nothing", got)
	}
}

// countOf returns how often s is in list.
func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func TestStubs(t *testing.T) {
	for _, name := range []string{"hostfirewall", "runtime", "cni", "join", "nomad"} {
		t.Run(name, func(t *testing.T) {
			h, fsys, r, e := ubuntu(t)
			res, err := runPhase(t, name, h, combined(t), e)
			if want := (nodeup.Result{Status: nodeup.Skipped, Reason: "not built yet"}); err != nil || res != want {
				t.Errorf("%s: %+v, %v; want %+v", name, res, err, want)
			}
			if len(r.Commands()) != 0 || len(fsys.Changes()) != 0 {
				t.Errorf("%s ran %q and changed %q, want nothing", name, r.Commands(), fsys.Changes())
			}
		})
	}
}

func TestVerify(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	enableUnits(t, fsys, r)
	res, err := runPhase(t, "verify", h, combined(t), e)
	if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
		t.Errorf("verify: %+v, %v; want unchanged", res, err)
	}
	want := []string{
		"systemctl enable tent-node.service", "systemctl enable tent-node-join.timer",
		"systemctl is-enabled tent-node.service", "systemctl is-enabled tent-node-join.timer",
	}
	if diff := cmp.Diff(want, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	if len(fsys.Changes()) != 0 {
		t.Errorf("verify changed %q, want nothing", fsys.Changes())
	}
}

func TestVerifyFails(t *testing.T) {
	t.Run("a disabled timer", func(t *testing.T) {
		h, fsys, r, e := ubuntu(t)
		addUnits(t, fsys)
		if err := (nodeup.Systemd{Runner: r}).Enable(t.Context(), "tent-node.service"); err != nil {
			t.Fatal(err)
		}
		_, err := runPhase(t, "verify", h, combined(t), e)
		if want := "tent-node-join.timer is disabled, not enabled"; errText(err) != want {
			t.Errorf("verify: %q, want %q", errText(err), want)
		}
	})
	t.Run("no state", func(t *testing.T) {
		h, _, r, e := ubuntu(t)
		r.On("systemctl is-enabled tent-node.service", nodeuptest.Exit(1,
			"Failed to get unit file state for tent-node.service: No such file or directory"))
		_, err := runPhase(t, "verify", h, combined(t), e)
		want := "systemctl is-enabled tent-node.service: exit status 1: Failed to get unit file state for " +
			"tent-node.service: No such file or directory"
		if errText(err) != want {
			t.Errorf("verify: %q, want %q", errText(err), want)
		}
	})
}

func TestUpWithThePhases(t *testing.T) {
	h, fsys, r, e := ubuntu(t)
	// A clock that moves, as a real one does, so that the status changes on the second run.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	enableUnits(t, fsys, r)
	nc := combined(t)
	first, err := nodeup.Up(t.Context(), h, nc, nodeup.Phases(e))
	if err != nil {
		t.Fatal(err)
	}
	status, _ := fsys.Entry(nodeup.StatusFile)
	checkGolden(t, "status-up.json.golden", status.Data)
	if first.Instance != instance {
		t.Errorf("the report's instance is %+v, want %+v", first.Instance, instance)
	}
	changes, commands := len(fsys.Changes()), len(r.Commands())

	second, err := nodeup.Up(t.Context(), h, nc, nodeup.Phases(e))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range second.Phases {
		if p.Status != nodeup.Unchanged && p.Status != nodeup.Skipped {
			t.Errorf("phase %s is %s on the second run, want unchanged or skipped", p.Name, p.Status)
		}
	}
	if diff := cmp.Diff([]string{nodeup.StatusFile}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("the second run changed files (-want +got):\n%s", diff)
	}
	want := []string{
		"timedatectl show -p CanNTP -p NTP",
		"systemctl is-enabled tent-node.service", "systemctl is-enabled tent-node-join.timer",
	}
	if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
		t.Errorf("the second run's commands, which must only read (-want +got):\n%s", diff)
	}
}
