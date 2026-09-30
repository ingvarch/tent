package nodeup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/retry"
)

// dockerConfigFile is Docker's configuration, which the runtime phase writes.
const dockerConfigFile = "/etc/docker/daemon.json"

// dockerConfig keeps containers running while Docker restarts, and keeps three log files of 10 MB for each container.
// Nomad sets its own limits on the containers that it starts.
const dockerConfig = `{
  "live-restore": true,
  "log-driver": "json-file",
  "log-opts": {
    "max-file": "3",
    "max-size": "10m"
  }
}
`

// dockerUnit is the unit of Ubuntu's docker.io package.
const dockerUnit = "docker.service"

// noninteractive keeps debconf from asking the packages' questions. Runner cannot set a variable for one command, so
// the commands run through env.
const noninteractive = "DEBIAN_FRONTEND=noninteractive"

// dpkg and apt-get take their locks without waiting, and apt-daily or unattended-upgrades on the first boot may hold
// them, so retryCommand tries each command of the install again every retryDelay for retryTimeout.
const (
	retryTimeout = 10 * time.Minute
	retryDelay   = 5 * time.Second
)

// containerRuntime is the phase that installs Docker from Ubuntu's docker.io package on a node whose nc asks for it.
// It writes Docker's configuration before the install, so that Docker's first start reads it, and restarts an
// installed Docker when the configuration changed. Then it enables and starts Docker where it is not.
func containerRuntime(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	if !nc.System.Docker {
		return Result{Status: Skipped, Reason: "this node group runs no Docker"}, nil
	}
	installed, err := dockerInstalled(ctx, h.Runner)
	if err != nil {
		return Result{}, fmt.Errorf("check for Docker: %w", err)
	}
	steps := []step{configureDocker(installed)}
	if !installed {
		steps = append(steps, installDocker)
	}
	return runSteps(ctx, h, nc, append(steps, startDocker)...)
}

// dockerInstalled reports whether dpkg has docker.io installed. dpkg-query exits with 1 for a package that dpkg never
// had. Otherwise it prints the selection, a flag and the state, such as hold ok installed: only the state installed
// counts, whatever the selection. The states config-files after a remove and half-configured after an install that
// stopped are not installed.
func dockerInstalled(ctx context.Context, r Runner) (bool, error) {
	out, err := r.Run(ctx, "dpkg-query", "-W", "-f=${Status}", "docker.io")
	if exit, ok := errors.AsType[*ExitError](err); ok && exit.Code == 1 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status := strings.Fields(string(out))
	return len(status) == 3 && status[2] == "installed", nil
}

// configureDocker returns the step that writes dockerConfig into dockerConfigFile. With restart, it restarts Docker to
// read the file when that changed it, and when the restart fails, the file goes, so that the next run tries again.
func configureDocker(restart bool) step {
	return func(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
		changed, err := writeThen(ctx, h.FS, dockerConfigFile, dockerConfig, func(ctx context.Context) error {
			if !restart {
				return nil
			}
			return h.systemd().Restart(ctx, dockerUnit)
		})
		if err != nil {
			return false, fmt.Errorf("configure Docker: %w", err)
		}
		return changed, nil
	}
}

// installDocker installs docker.io, whose postinst enables and starts Docker. dpkg --configure -a first finishes an
// install that an earlier run stopped halfway, after which apt-get would refuse to install; apt-get update fetches the
// package lists, which the image may lack. Each command runs with retryCommand. --force-confdef and --force-confold
// keep a configuration file that both the admin and the package changed: dpkg would ask about it, and without a
// terminal the question fails dpkg.
func installDocker(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	h.logger().Info("install Docker")
	for _, command := range [][]string{
		{"dpkg", "--force-confdef", "--force-confold", "--configure", "-a"},
		{"apt-get", "update"},
		{
			"apt-get", "install", "-y", "--no-install-recommends",
			"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "docker.io",
		},
	} {
		if err := retryCommand(ctx, h, "env", append([]string{noninteractive}, command...)...); err != nil {
			return false, fmt.Errorf("install Docker: %w", err)
		}
	}
	return true, nil
}

// startDocker enables Docker to start at boot and starts it, where it does not. When systemd has a job for Docker, as
// the start job of a boot, queued or running, Docker starts on its own: start waits until it is up, and that is no
// change. Any job counts, a stop job that another client queued included.
func startDocker(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	sd := h.systemd()
	_, enabled, err := sd.IsEnabled(ctx, dockerUnit)
	if err != nil {
		return false, fmt.Errorf("start Docker: %w", err)
	}
	if !enabled {
		if err := sd.Enable(ctx, dockerUnit); err != nil {
			return false, fmt.Errorf("start Docker: %w", err)
		}
	}
	_, active, err := sd.IsActive(ctx, dockerUnit)
	if err != nil {
		return false, fmt.Errorf("start Docker: %w", err)
	}
	starting := false
	if !active {
		if starting, err = sd.HasJob(ctx, dockerUnit); err != nil {
			return false, fmt.Errorf("start Docker: %w", err)
		}
		if err := sd.Start(ctx, dockerUnit); err != nil {
			return false, fmt.Errorf("start Docker: %w", err)
		}
	}
	return !enabled || (!active && !starting), nil
}

// retryCommand runs the command on h until it succeeds, again retryDelay after each failure, and fails with the last
// error once retryTimeout has passed since the first try. It logs the first failure. When ctx ends, it stops waiting
// at once, and the error keeps the last try's.
func retryCommand(ctx context.Context, h *Host, name string, args ...string) error {
	deadline := time.Now().Add(retryTimeout)
	for try := 1; ; try++ {
		_, err := h.Runner.Run(ctx, name, args...)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil: // the command stopped with ctx
			return err
		case !time.Now().Before(deadline):
			return fmt.Errorf("gave up after %v: %w", retryTimeout, err)
		case try == 1:
			h.logger().Warn("command failed, trying again", "command", CommandLine(name, args...), "error", err,
				"every", retryDelay, "within", retryTimeout)
		}
		if !retry.Sleep(ctx, retryDelay) {
			return fmt.Errorf("%w; stopped waiting: %w", err, context.Cause(ctx))
		}
	}
}
