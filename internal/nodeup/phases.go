package nodeup

import (
	"context"
	"fmt"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// Phases returns the phases of tent-node up in the order they run: preflight, which reads the machine from the
// metadata service environment, system, hostfirewall, runtime, cni, join, nomad and verify. hostfirewall, runtime,
// cni, join and nomad are not built yet, and are skipped.
func Phases(environment env.Environment) []Phase {
	return []Phase{
		preflight(environment),
		{Name: "system", Run: system},
		notBuilt("hostfirewall"),
		notBuilt("runtime"),
		notBuilt("cni"),
		notBuilt("join"),
		notBuilt("nomad"),
		{Name: "verify", Run: verify},
	}
}

// notBuilt returns a phase that tent-node does not have yet: it changes nothing and is skipped.
func notBuilt(name string) Phase {
	return Phase{Name: name, Run: func(context.Context, *Host, *nodeconfig.NodeConfig) (Result, error) {
		return Result{Status: Skipped, Reason: "not built yet"}, nil
	}}
}

// verify is the phase that checks that the units that run tent-node start at every boot. It changes nothing.
func verify(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (Result, error) {
	sd := h.systemd()
	for _, unit := range []string{serviceUnit, joinTimerUnit} {
		state, enabled, err := sd.IsEnabled(ctx, unit)
		if err != nil {
			return Result{}, err
		}
		if !enabled {
			return Result{}, fmt.Errorf("%s is %s, not enabled", unit, state)
		}
	}
	return Result{Status: Unchanged}, nil
}
