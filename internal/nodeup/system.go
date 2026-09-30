package nodeup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// Paths of the files that the system phase writes.
const (
	modulesFile  = "/etc/modules-load.d/tent.conf"          // systemd-modules-load loads them at boot
	sysctlFile   = "/etc/sysctl.d/99-tent.conf"             // systemd-sysctl applies them at boot, after the modules
	journaldFile = "/etc/systemd/journald.conf.d/tent.conf" // a drop-in over journald.conf
)

// journaldConf limits the journal on disk to 1 GiB, which systemd would otherwise let grow to a tenth of the file
// system, up to 4 GiB.
const journaldConf = nodeconfig.NodeHeader + "[Journal]\nSystemMaxUse=1G\n"

// system is the phase that sets the operating system up: it loads the kernel modules of nc's system settings and
// has them load at boot, applies its sysctls and keeps them for the next boot, turns the time sync on, and limits the
// journal. A node without kernel modules or sysctls, such as a server, gets no file for them. A command that applies a
// file runs only when the file changed, and when it fails, the file goes, so that the next run tries again.
func system(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	// The modules come first: sysctls such as net.bridge.bridge-nf-call-iptables exist once br_netfilter is loaded.
	return runSteps(ctx, h, nc, loadModules, applySysctls, syncTime, limitJournal)
}

// loadModules writes the kernel modules of nc into modulesFile and loads each with modprobe.
func loadModules(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (bool, error) {
	modules := nc.System.KernelModules
	if len(modules) == 0 {
		return false, nil
	}
	content := nodeconfig.NodeHeader + strings.Join(modules, "\n") + "\n"
	changed, err := writeThen(ctx, h.FS, modulesFile, content, func(ctx context.Context) error {
		for _, m := range modules {
			if _, err := h.Runner.Run(ctx, "modprobe", m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("load kernel modules: %w", err)
	}
	return changed, nil
}

// applySysctls writes the sysctls of nc into sysctlFile, sorted by key, and applies the file with sysctl -p.
func applySysctls(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (bool, error) {
	sysctls := nc.System.Sysctls
	if len(sysctls) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.WriteString(nodeconfig.NodeHeader)
	for _, key := range slices.Sorted(maps.Keys(sysctls)) {
		b.WriteString(key + " = " + sysctls[key] + "\n")
	}
	changed, err := writeThen(ctx, h.FS, sysctlFile, b.String(), func(ctx context.Context) error {
		_, err := h.Runner.Run(ctx, "sysctl", "-p", sysctlFile)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("apply the sysctls: %w", err)
	}
	return changed, nil
}

// timeServices are the services that keep the time where timedatectl cannot turn NTP on, such as chrony, which some
// images run instead of systemd-timesyncd. Ubuntu's default is chrony since 25.10; Vultr's 26.04 image does not use it.
var timeServices = []string{"chrony.service", "systemd-timesyncd.service"}

// syncTime turns NTP on with timedatectl when it can and NTP is off. Where it cannot, it checks that one of
// timeServices is active.
func syncTime(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	out, err := h.Runner.Run(ctx, "timedatectl", "show", "-p", "CanNTP", "-p", "NTP")
	if err != nil {
		return false, fmt.Errorf("check the time sync: %w", err)
	}
	props := map[string]string{}
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		props[key] = value
	}
	switch {
	case props["CanNTP"] == "yes" && props["NTP"] == "yes":
		return false, nil
	case props["CanNTP"] == "yes":
		if _, err := h.Runner.Run(ctx, "timedatectl", "set-ntp", "true"); err != nil {
			return false, fmt.Errorf("turn on the time sync: %w", err)
		}
		return true, nil
	}
	for _, service := range timeServices {
		_, active, err := h.systemd().IsActive(ctx, service)
		if err != nil {
			return false, fmt.Errorf("check the time sync: %w", err)
		}
		if active {
			return false, nil
		}
	}
	return false, fmt.Errorf("no time sync: timedatectl cannot turn NTP on, and neither %s is active",
		strings.Join(timeServices, " nor "))
}

// limitJournal writes journaldConf into journaldFile and restarts journald to read it.
func limitJournal(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	changed, err := writeThen(ctx, h.FS, journaldFile, journaldConf, func(ctx context.Context) error {
		return h.systemd().Restart(ctx, "systemd-journald.service")
	})
	if err != nil {
		return false, fmt.Errorf("limit the journal: %w", err)
	}
	return changed, nil
}

// writeThen makes p a file with content, mode 0644 and owned by root:root, in a directory that it makes when it is
// missing, and calls then when that changed the file. When then fails, writeThen removes the file, so that the next
// run writes it again and calls then again. It reports whether it changed anything.
//
// A run killed between the write and then, as by SIGKILL, leaves the file without its command: the next run finds the
// file as it should be and does not run the command. The next boot heals it, as systemd-modules-load, systemd-sysctl
// and journald read the files at boot, and Docker reads daemon.json at its start; a run by hand before it does not.
func writeThen(ctx context.Context, fsys FS, p, content string, then func(context.Context) error) (bool, error) {
	dirChanged, err := fsys.EnsureDir(path.Dir(p), 0o755, nodeconfig.Owner)
	if err != nil {
		return false, err
	}
	changed, err := fsys.WriteFile(p, []byte(content), 0o644, nodeconfig.Owner)
	if err != nil || !changed {
		return dirChanged, err
	}
	if err := then(ctx); err != nil {
		if _, rerr := fsys.Remove(p); rerr != nil {
			return true, errors.Join(err, rerr)
		}
		return true, err
	}
	return true, nil
}
