package nodeup

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// The units that run tent-node: up at every boot, and refresh-join from a timer.
const (
	serviceUnit     = "tent-node.service"
	joinServiceUnit = "tent-node-join.service"
	joinTimerUnit   = "tent-node-join.timer"
)

// unitDir is where the units that the administrator installs live.
const unitDir = "/etc/systemd/system"

// unitPath matches a path that a unit file holds as it is: systemd expands % and $, splits at white space and reads
// quotes and backslashes.
var unitPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// serviceTemplate is tent-node.service. It waits only for the network. tent-node install runs inside cloud-final and
// waits for it, so an order after cloud-final or cloud-init.target would hang the first boot; cloud-config is left out
// as a precaution, and so is cloud-init-main, which runs every stage in one process on Ubuntu 26.04.
// WantedBy=multi-user.target orders the target after the service, so an order after multi-user.target would be a
// cycle. up starts Nomad itself.
const serviceTemplate = nodeconfig.NodeHeader + `[Unit]
Description=tent-node up: set the machine up as a Nomad agent of its node group
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%[1]s up --config %[2]s
TimeoutStartSec=45min
KillMode=mixed

[Install]
WantedBy=multi-user.target
`

// joinServiceTemplate is tent-node-join.service, which the timer starts. A oneshot service has no start timeout of its
// own, and while a refresh hangs, the timer starts no other.
const joinServiceTemplate = nodeconfig.NodeHeader + `[Unit]
Description=tent-node refresh-join: keep the Nomad servers that the node joins current
After=` + serviceUnit + `

[Service]
Type=oneshot
ExecStart=%[1]s refresh-join --config %[2]s
TimeoutStartSec=%[3]dmin
`

// joinTimeoutMinutes is the start timeout of tent-node-join.service.
const joinTimeoutMinutes = 5

// RefreshTimeout bounds a run of refresh-join, the wait for the lock included: 15 seconds less than the start timeout
// of tent-node-join.service, so that refresh-join ends before systemd stops it.
const RefreshTimeout = joinTimeoutMinutes*time.Minute - 15*time.Second

// RefreshLockWait is how long refresh-join waits for the lock that another tent-node run holds: a minute less than the
// start timeout of tent-node-join.service, which leaves the refresh time within RefreshTimeout.
const RefreshLockWait = (joinTimeoutMinutes - 1) * time.Minute

// joinTimerTemplate is tent-node-join.timer. Without AccuracySec, systemd could start the refresh up to a minute late.
const joinTimerTemplate = nodeconfig.NodeHeader + `[Unit]
Description=Run tent-node refresh-join every %[1]s

[Timer]
OnBootSec=%[1]s
OnUnitActiveSec=%[1]s
AccuracySec=1s

[Install]
WantedBy=timers.target
`

// RenderUnits returns the systemd units that run tent-node, the binary at binaryPath, with the node config at
// configPath, in /etc/systemd/system with mode 0644 and owned by root:root:
//   - tent-node.service runs tent-node up once at every boot, once the network is online, for at most 45 minutes;
//   - tent-node-join.service runs tent-node refresh-join, after tent-node.service;
//   - tent-node-join.timer starts tent-node-join.service refresh after the boot and then every refresh.
//
// The paths must be absolute and clean, of letters, digits, ".", "_", "-" and "/", and refresh at least a second. It
// writes refresh in seconds, or in milliseconds when it is not a whole number of seconds.
func RenderUnits(configPath, binaryPath string, refresh time.Duration) ([]nodeconfig.File, error) {
	files, err := renderUnits(configPath, binaryPath, refresh)
	if err != nil {
		return nil, fmt.Errorf("units: %w", err)
	}
	return files, nil
}

func renderUnits(configPath, binaryPath string, refresh time.Duration) ([]nodeconfig.File, error) {
	for _, p := range []struct{ name, path string }{{"the config's", configPath}, {"the binary's", binaryPath}} {
		switch {
		case CheckPath(p.path) != nil:
			return nil, fmt.Errorf("%s path %q is not absolute and clean", p.name, p.path)
		case !unitPath.MatchString(p.path):
			return nil, fmt.Errorf(`%s path %q has a character other than a letter, a digit, ".", "_", "-" and "/"`,
				p.name, p.path)
		}
	}
	if refresh < time.Second {
		return nil, fmt.Errorf("the refresh interval %s is less than 1s", refresh)
	}
	unit := func(name, content string) nodeconfig.File {
		return nodeconfig.File{Path: path.Join(unitDir, name), Mode: 0o644, Owner: nodeconfig.Owner,
			Content: []byte(content)}
	}
	return []nodeconfig.File{
		unit(serviceUnit, fmt.Sprintf(serviceTemplate, binaryPath, configPath)),
		unit(joinServiceUnit, fmt.Sprintf(joinServiceTemplate, binaryPath, configPath, joinTimeoutMinutes)),
		unit(joinTimerUnit, fmt.Sprintf(joinTimerTemplate, timeSpan(refresh))),
	}, nil
}

// timeSpan writes d as systemd reads a time span: in seconds, or in milliseconds when d is not a whole number of
// seconds. It drops what is below a millisecond.
func timeSpan(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// Install installs tent-node on the machine, the binary at binaryPath with the node config nc at configPath: it
// writes the units of RenderUnits, has systemd read them again when one changed since systemd read it, enables
// tent-node.service and tent-node-join.timer, starts tent-node.service and waits until up has run, and starts the
// timer. It enables and starts only what is not enabled or active, so that a second run only reads. When up fails,
// Install fails, and the timer starts at the next boot.
func Install(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig, configPath, binaryPath string) error {
	units, err := RenderUnits(configPath, binaryPath, nc.Join.RefreshInterval)
	if err != nil {
		return err
	}
	for _, u := range units {
		if _, err := h.FS.WriteFile(u.Path, u.Content, fs.FileMode(u.Mode), u.Owner); err != nil {
			return err
		}
	}
	sd := h.systemd()
	if err := reloadUnits(ctx, sd, units); err != nil {
		return err
	}
	for _, unit := range []string{serviceUnit, joinTimerUnit} {
		_, enabled, err := sd.IsEnabled(ctx, unit)
		if err == nil && !enabled {
			err = sd.Enable(ctx, unit)
		}
		if err != nil {
			return err
		}
	}
	for _, unit := range []string{serviceUnit, joinTimerUnit} {
		_, active, err := sd.IsActive(ctx, unit)
		if err == nil && !active {
			h.logger().Info("start the unit", "unit", unit)
			err = sd.Start(ctx, unit)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// reloadUnits has systemd read the units again when one of them changed since systemd read it. It asks systemd
// rather than trusting its own writes, so that the next run makes good a run that stopped between writing a unit and
// the reload.
func reloadUnits(ctx context.Context, sd Systemd, units []nodeconfig.File) error {
	for _, u := range units {
		needs, err := sd.NeedsReload(ctx, path.Base(u.Path))
		if err != nil {
			return err
		}
		if needs {
			return sd.DaemonReload(ctx)
		}
	}
	return nil
}
