package nodeup

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Systemd manages the units of systemd with systemctl. Errors are those of Runner, which name the command.
type Systemd struct {
	Runner Runner
}

// DaemonReload makes systemd read the unit files again.
func (s Systemd) DaemonReload(ctx context.Context) error { return s.run(ctx, "daemon-reload") }

// Enable makes the units start at boot.
func (s Systemd) Enable(ctx context.Context, units ...string) error {
	return s.run(ctx, append([]string{"enable"}, units...)...)
}

// Start starts the unit and waits until it has started; for a oneshot service, until it has run.
func (s Systemd) Start(ctx context.Context, unit string) error { return s.run(ctx, "start", unit) }

// Restart stops the unit, if it runs, and starts it.
func (s Systemd) Restart(ctx context.Context, unit string) error { return s.run(ctx, "restart", unit) }

// IsEnabled returns the unit's enablement state as systemctl is-enabled prints it, such as enabled, enabled-runtime,
// static, disabled, masked or not-found, and whether the unit starts at every boot: only enabled does.
// enabled-runtime lasts until the next boot, and a static unit starts only when another unit asks for it. When
// systemctl prints no state, as when it cannot reach systemd, IsEnabled returns its error.
func (s Systemd) IsEnabled(ctx context.Context, unit string) (state string, enabled bool, err error) {
	state, _, err = s.state(ctx, "is-enabled", unit)
	return state, state == "enabled", err
}

// IsActive returns the unit's active state as systemctl is-active prints it, such as active, reloading, activating,
// deactivating, inactive or failed, and whether the unit is active: whether systemctl exits with 0, as it does for
// active and reloading. When systemctl prints no state, as when it cannot reach systemd, IsActive returns its error.
func (s Systemd) IsActive(ctx context.Context, unit string) (state string, active bool, err error) {
	return s.state(ctx, "is-active", unit)
}

// NeedsReload reports whether the unit's files changed since systemd read them, so that systemd must read them
// again, as systemctl show -p NeedDaemonReload says. A unit that systemd has not read yet, it reads now, and does not
// need a reload.
func (s Systemd) NeedsReload(ctx context.Context, unit string) (bool, error) {
	args := []string{"show", "-p", "NeedDaemonReload", "--value", unit}
	out, err := s.Runner.Run(ctx, "systemctl", args...)
	if err != nil {
		return false, err
	}
	switch value := strings.TrimSpace(string(out)); value {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s: printed %q, not yes or no", CommandLine("systemctl", args...), value)
	}
}

func (s Systemd) run(ctx context.Context, args ...string) error {
	_, err := s.Runner.Run(ctx, "systemctl", args...)
	return err
}

// state asks systemctl for the state of the unit with verb and returns the state it prints and whether it exited
// with 0. systemctl prints the state and exits with another status for a state that is not the one asked about; it
// prints nothing when it cannot tell, and then state returns its error.
func (s Systemd) state(ctx context.Context, verb, unit string) (state string, ok bool, err error) {
	out, err := s.Runner.Run(ctx, "systemctl", verb, unit)
	state = strings.TrimSpace(string(out))
	_, exited := errors.AsType[*ExitError](err)
	switch {
	case err != nil && (!exited || state == ""):
		return "", false, err
	case state == "":
		return "", false, fmt.Errorf("%s: printed no state", CommandLine("systemctl", verb, unit))
	}
	return state, err == nil, nil
}
