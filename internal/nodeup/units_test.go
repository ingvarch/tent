package nodeup_test

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// Where the user data puts tent-node and its config.
const (
	configPath = "/etc/tent/node.json"
	binaryPath = "/usr/local/bin/tent-node"
)

// Paths of tent-node's units.
const (
	servicePath     = "/etc/systemd/system/tent-node.service"
	joinServicePath = "/etc/systemd/system/tent-node-join.service"
	joinTimerPath   = "/etc/systemd/system/tent-node-join.timer"
)

// renderUnits returns the units for the paths of the user data and a refresh every minute.
func renderUnits(t *testing.T) []nodeconfig.File {
	t.Helper()
	files, err := nodeup.RenderUnits(configPath, binaryPath, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRenderUnits(t *testing.T) {
	files := renderUnits(t)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if f.Mode != 0o644 || f.Owner != nodeconfig.Owner || f.Secret || f.PerNode {
			t.Errorf("%s: mode %#o, owner %s, secret %v, per node %v; want 0644, root:root and neither", f.Path,
				f.Mode, f.Owner, f.Secret, f.PerNode)
		}
		checkGolden(t, path.Base(f.Path)+".golden", f.Content)
	}
	if diff := cmp.Diff([]string{servicePath, joinServicePath, joinTimerPath}, paths); diff != "" {
		t.Errorf("paths (-want +got):\n%s", diff)
	}
}

// unitValues returns the values of the settings of a unit file, by key, each value split at white space.
func unitValues(content []byte) map[string][]string {
	values := map[string][]string{}
	for line := range strings.Lines(string(content)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(key, "#") {
			values[key] = append(values[key], strings.Fields(value)...)
		}
	}
	return values
}

// TestUnitsWaitForNoCloudInit checks the orderings that would hang or break the boot. tent-node install runs inside
// cloud-final.service and waits for tent-node.service, so no unit may wait for cloud-final.service or
// cloud-init.target: the first boot would hang. cloud-config.service is left out as a precaution, and so is
// cloud-init-main.service, which runs every stage in one process on Ubuntu 26.04.
// WantedBy=multi-user.target orders the target after tent-node.service, so an order after multi-user.target would be
// a cycle. Nomad is started by up itself, not ordered after it.
func TestUnitsWaitForNoCloudInit(t *testing.T) {
	forbidden := []string{"cloud-final.service", "cloud-init.target", "cloud-config.service",
		"cloud-init-main.service", "multi-user.target"}
	units := renderUnits(t)
	// nomad.service comes from tent through the node config.
	units = append(units, nodeconfig.RenderNomadService())
	nomadSeen := false
	for _, f := range units {
		values := unitValues(f.Content)
		for _, key := range []string{"After", "Requires", "Wants", "Requisite", "BindsTo"} {
			for _, unit := range values[key] {
				if slices.Contains(forbidden, unit) {
					t.Errorf("%s has %s=%s", f.Path, key, unit)
				}
			}
		}
		if slices.Contains(values["Before"], "nomad.service") {
			t.Errorf("%s has Before=nomad.service", f.Path)
		}
		// up starts Nomad from inside tent-node.service, so an order between them would deadlock; and nomad.service
		// is never enabled for boot: up starts it after the host firewall.
		if f.Path == nodeconfig.NomadServiceFile {
			nomadSeen = true
			for _, key := range []string{"After", "Requires", "Wants", "Requisite", "BindsTo"} {
				if slices.Contains(values[key], "tent-node.service") {
					t.Errorf("nomad.service has %s=tent-node.service", key)
				}
			}
			if strings.Contains(string(f.Content), "[Install]") {
				t.Error("nomad.service has an [Install] section, which would let it be enabled for boot")
			}
		}
	}
	if !nomadSeen {
		t.Error("nomad.service was not among the audited units")
	}
	service := unitValues(renderUnits(t)[0].Content)
	for _, key := range []string{"After", "Wants"} {
		if got := service[key]; !cmp.Equal(got, []string{"network-online.target"}) {
			t.Errorf("tent-node.service has %s=%q, want network-online.target alone", key, got)
		}
	}
}

// TestNomadStopsBeforeDocker checks that nomad.service starts after docker.service, so systemd stops Nomad before
// Docker. Nothing more ties them: a restart of Docker leaves Nomad running, and a server without Docker starts Nomad.
func TestNomadStopsBeforeDocker(t *testing.T) {
	values := unitValues(nodeconfig.RenderNomadService().Content)
	if !slices.Contains(values["After"], "docker.service") {
		t.Errorf("nomad.service has After=%q, want docker.service among them", values["After"])
	}
	for _, key := range []string{"Requires", "Wants", "Requisite", "BindsTo", "PartOf"} {
		if slices.Contains(values[key], "docker.service") {
			t.Errorf("nomad.service has %s=docker.service", key)
		}
	}
}

// TestServicesHaveAStartTimeout checks that no service can run for ever: a oneshot service has no start timeout unless
// it sets one, and a refresh-join that hangs would keep the timer from starting the next.
func TestServicesHaveAStartTimeout(t *testing.T) {
	want := map[string]string{servicePath: "45min", joinServicePath: "5min"}
	for _, f := range renderUnits(t) {
		if !strings.HasSuffix(f.Path, ".service") {
			continue
		}
		if got := unitValues(f.Content)["TimeoutStartSec"]; !cmp.Equal(got, []string{want[f.Path]}) {
			t.Errorf("%s has TimeoutStartSec=%q, want %s", f.Path, got, want[f.Path])
		}
	}
}

// TestRefreshLockWaitEndsBeforeTheUnit checks that refresh-join ends 15 seconds before systemd would stop
// tent-node-join.service, and stops waiting for the lock a minute before it, which leaves the refresh time.
func TestRefreshLockWaitEndsBeforeTheUnit(t *testing.T) {
	timeout := unitValues(renderUnits(t)[1].Content)["TimeoutStartSec"]
	if len(timeout) != 1 || !strings.HasSuffix(timeout[0], "min") {
		t.Fatalf("tent-node-join.service has TimeoutStartSec=%q, want minutes", timeout)
	}
	d, err := time.ParseDuration(strings.TrimSuffix(timeout[0], "in"))
	if err != nil {
		t.Fatal(err)
	}
	if nodeup.RefreshLockWait != d-time.Minute {
		t.Errorf("RefreshLockWait is %s, want %s: a minute less than the unit's TimeoutStartSec=%s",
			nodeup.RefreshLockWait, d-time.Minute, timeout[0])
	}
	if nodeup.RefreshTimeout != d-15*time.Second {
		t.Errorf("RefreshTimeout is %s, want %s: 15 seconds less than the unit's TimeoutStartSec=%s",
			nodeup.RefreshTimeout, d-15*time.Second, timeout[0])
	}
}

func TestRenderUnitsRefreshInterval(t *testing.T) {
	for refresh, want := range map[time.Duration]string{
		time.Second: "1s", 90 * time.Second: "90s", 1500 * time.Millisecond: "1500ms",
	} {
		files, err := nodeup.RenderUnits(configPath, binaryPath, refresh)
		if err != nil {
			t.Fatal(err)
		}
		timer := unitValues(files[2].Content)
		if !cmp.Equal(timer["OnBootSec"], []string{want}) || !cmp.Equal(timer["OnUnitActiveSec"], []string{want}) {
			t.Errorf("a refresh every %s: OnBootSec=%q and OnUnitActiveSec=%q, want %s", refresh,
				timer["OnBootSec"], timer["OnUnitActiveSec"], want)
		}
	}
}

func TestRenderUnitsRefuses(t *testing.T) {
	cases := []struct {
		name, config, binary string
		refresh              time.Duration
		want                 string
	}{
		{"relative config", "etc/tent/node.json", binaryPath, time.Minute,
			`units: the config's path "etc/tent/node.json" is not absolute and clean`},
		{"unclean binary", configPath, "/usr/local/bin/../bin/tent-node", time.Minute,
			`units: the binary's path "/usr/local/bin/../bin/tent-node" is not absolute and clean`},
		{"a space", configPath, "/opt/tent node/tent-node", time.Minute,
			`units: the binary's path "/opt/tent node/tent-node" has a character other than a letter, a digit, ` +
				`".", "_", "-" and "/"`},
		{"a specifier", "/etc/tent/%H.json", binaryPath, time.Minute,
			`units: the config's path "/etc/tent/%H.json" has a character other than a letter, a digit, ".", ` +
				`"_", "-" and "/"`},
		{"a short refresh", configPath, binaryPath, 500 * time.Millisecond,
			"units: the refresh interval 500ms is less than 1s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if files, err := nodeup.RenderUnits(c.config, c.binary, c.refresh); errText(err) != c.want || files != nil {
				t.Errorf("RenderUnits: %d files, %q; want none and %q", len(files), errText(err), c.want)
			}
		})
	}
}

// needsReload are the commands that ask whether systemd must read tent-node's units again.
var needsReload = []string{
	"systemctl show -p NeedDaemonReload --value tent-node.service",
	"systemctl show -p NeedDaemonReload --value tent-node-join.service",
	"systemctl show -p NeedDaemonReload --value tent-node-join.timer",
}

// installCommands are what install runs on a machine where the units are new: systemd reads a new unit when asked
// about it, so it needs no reload.
var installCommands = append(slices.Clone(needsReload),
	"systemctl is-enabled tent-node.service", "systemctl enable tent-node.service",
	"systemctl is-enabled tent-node-join.timer", "systemctl enable tent-node-join.timer",
	"systemctl is-active tent-node.service", "systemctl start tent-node.service",
	"systemctl is-active tent-node-join.timer", "systemctl start tent-node-join.timer",
)

// installAgainCommands are what install runs once the units are installed, enabled and started, after it reloaded
// systemd when it had to: it only reads.
var installAgainCommands = []string{
	"systemctl is-enabled tent-node.service", "systemctl is-enabled tent-node-join.timer",
	"systemctl is-active tent-node.service", "systemctl is-active tent-node-join.timer",
}

func TestInstall(t *testing.T) {
	h, fsys, r, _ := ubuntu(t)
	var logs bytes.Buffer
	h.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := nodeup.Install(t.Context(), h, combined(t), configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	// Starting tent-node.service waits until up has run, so the log says what install waits for.
	for _, unit := range startUnits {
		if want := `msg="start the unit" unit=` + unit; !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, logs.String())
		}
	}
	if diff := cmp.Diff([]string{servicePath, joinServicePath, joinTimerPath}, fsys.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	for _, f := range renderUnits(t) {
		e, ok := fsys.Entry(f.Path)
		if want := (nodeuptest.Entry{Data: f.Content, Mode: 0o644, Owner: "root:root"}); !ok || !cmp.Equal(e, want) {
			t.Errorf("%s is %+v, want the rendered unit with mode 0644 owned by root:root", f.Path, e)
		}
	}
	if diff := cmp.Diff(installCommands, r.Commands()); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
}

func TestInstallSecondRunChangesNothing(t *testing.T) {
	h, fsys, r, _ := ubuntu(t)
	nc := combined(t)
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	changes, commands := len(fsys.Changes()), len(r.Commands())
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second install changed %q, want nothing", got)
	}
	if diff := cmp.Diff(append(slices.Clone(needsReload), installAgainCommands...),
		r.Commands()[commands:]); diff != "" {
		t.Errorf("the second install's commands, which must only read (-want +got):\n%s", diff)
	}
}

// oldTimer is a tent-node-join.timer that an older install left.
const oldTimer = "[Timer]\nOnBootSec=5min\n"

// readOldTimer puts oldTimer in place of tent-node-join.timer and has systemd read it. It returns the timer that
// was there.
func readOldTimer(t *testing.T, fsys *nodeuptest.FS, r *nodeuptest.Runner) []byte {
	t.Helper()
	timer, _ := fsys.Entry(joinTimerPath)
	fsys.AddFile(t, joinTimerPath, []byte(oldTimer), 0o644, nodeconfig.Owner)
	if err := (nodeup.Systemd{Runner: r}).DaemonReload(t.Context()); err != nil {
		t.Fatal(err)
	}
	return timer.Data
}

func TestInstallReloadsAChangedUnit(t *testing.T) {
	h, fsys, r, _ := ubuntu(t)
	nc := combined(t)
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	readOldTimer(t, fsys, r)
	changes, commands := len(fsys.Changes()), len(r.Commands())
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{joinTimerPath}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	want := append(append(slices.Clone(needsReload), "systemctl daemon-reload"), installAgainCommands...)
	if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
}

// TestInstallReloadsAfterAnInterruptedInstall checks that install asks systemd, not its own writes, whether to reload:
// a run that wrote a unit and stopped before the reload leaves nothing for the next run to write.
func TestInstallReloadsAfterAnInterruptedInstall(t *testing.T) {
	h, fsys, r, _ := ubuntu(t)
	nc := combined(t)
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	// systemd read an older timer, and the interrupted run wrote the new one.
	fsys.AddFile(t, joinTimerPath, readOldTimer(t, fsys, r), 0o644, nodeconfig.Owner)
	changes, commands := len(fsys.Changes()), len(r.Commands())
	if err := nodeup.Install(t.Context(), h, nc, configPath, binaryPath); err != nil {
		t.Fatal(err)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("install changed %q, want nothing", got)
	}
	want := append(append(slices.Clone(needsReload), "systemctl daemon-reload"), installAgainCommands...)
	if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	if again, err := (nodeup.Systemd{Runner: r}).NeedsReload(t.Context(), "tent-node-join.timer"); again || err != nil {
		t.Errorf("NeedDaemonReload after install: %v, %v; want false", again, err)
	}
}

func TestInstallFailures(t *testing.T) {
	t.Run("up fails", func(t *testing.T) {
		h, _, r, _ := ubuntu(t)
		r.On("systemctl start tent-node.service", nodeuptest.Exit(1, "Job for tent-node.service failed because "+
			"the control process exited with error code."))
		err := nodeup.Install(t.Context(), h, combined(t), configPath, binaryPath)
		want := "systemctl start tent-node.service: exit status 1: Job for tent-node.service failed because the " +
			"control process exited with error code."
		if errText(err) != want {
			t.Errorf("Install: %q, want %q", errText(err), want)
		}
		// The timer starts at the next boot, as it is enabled.
		if slices.Contains(r.Commands(), "systemctl start tent-node-join.timer") {
			t.Error("Install started the timer after tent-node.service failed")
		}
	})
	t.Run("a unit it cannot write", func(t *testing.T) {
		h, fsys, r, _ := ubuntu(t)
		fsys.Fail(joinServicePath, fs.ErrPermission)
		err := nodeup.Install(t.Context(), h, combined(t), configPath, binaryPath)
		if !errors.Is(err, fs.ErrPermission) || errText(err) != "write "+joinServicePath+": permission denied" {
			t.Errorf("Install: %q, want the failed write", errText(err))
		}
		if got := r.Commands(); len(got) != 0 {
			t.Errorf("Install ran %q after a failed write, want nothing", got)
		}
	})
	t.Run("no reload state", func(t *testing.T) {
		h, _, r, _ := ubuntu(t)
		r.On(needsReload[1], nodeuptest.Exit(1, "Failed to connect to bus: No such file or directory"))
		err := nodeup.Install(t.Context(), h, combined(t), configPath, binaryPath)
		want := needsReload[1] + ": exit status 1: Failed to connect to bus: No such file or directory"
		if errText(err) != want {
			t.Errorf("Install: %q, want %q", errText(err), want)
		}
		if got := r.Commands(); len(got) != 2 {
			t.Errorf("Install ran %q, want to stop after the failed check", got)
		}
	})
	t.Run("no enablement state", func(t *testing.T) {
		h, _, r, _ := ubuntu(t)
		r.On("systemctl is-enabled tent-node.service", nodeuptest.Exit(1, "Failed to connect to bus: No such file "+
			"or directory"))
		err := nodeup.Install(t.Context(), h, combined(t), configPath, binaryPath)
		want := "systemctl is-enabled tent-node.service: exit status 1: Failed to connect to bus: No such file or " +
			"directory"
		if errText(err) != want {
			t.Errorf("Install: %q, want %q", errText(err), want)
		}
	})
	t.Run("a path the units cannot hold", func(t *testing.T) {
		h, fsys, _, _ := ubuntu(t)
		err := nodeup.Install(t.Context(), h, combined(t), configPath, "/opt/tent node/tent-node")
		if !strings.HasPrefix(errText(err), "units: the binary's path") || len(fsys.Changes()) != 0 {
			t.Errorf("Install: %q after changing %q, want the error of RenderUnits and no change", errText(err),
				fsys.Changes())
		}
	})
}
