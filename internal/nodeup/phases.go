package nodeup

import (
	"context"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// Phases returns the phases of tent-node up in the order they run: preflight, which reads the machine from the
// metadata service environment, system, hostfirewall, runtime, cni, join, nomad and verify.
func Phases(environment env.Environment) []Phase {
	return []Phase{
		preflight(environment),
		{Name: "system", Run: system},
		{Name: "hostfirewall", Run: hostfirewall},
		{Name: "runtime", Run: containerRuntime},
		{Name: "cni", Run: cni},
		{Name: "join", Run: join},
		{Name: "nomad", Run: nomad},
		{Name: "verify", Run: verify},
	}
}

// step is a part of a phase. It reports whether it changed the machine.
type step func(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (bool, error)

// runSteps runs the steps in order and stops at the first that fails. The result is Done when a step changed the
// machine, else Unchanged.
func runSteps(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig, steps ...step) (Result, error) {
	changed := false
	for _, s := range steps {
		c, err := s(ctx, h, nc)
		if err != nil {
			return Result{}, err
		}
		changed = changed || c
	}
	if changed {
		return Result{Status: Done}, nil
	}
	return Result{Status: Unchanged}, nil
}
