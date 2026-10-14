//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const (
	updateTimeout       = 25 * time.Minute
	rollServersTimeout  = 60 * time.Minute
	rollWorkersTimeout  = 45 * time.Minute
	rolledEvery         = 10 * time.Second
	rolledTimeout       = 2 * time.Minute
	bootEvery           = 5 * time.Second
	bootTimeout         = 6 * time.Minute
	registrationEvery   = 5 * time.Second
	registrationTimeout = 5 * time.Minute
	// specTimeout bounds a command that reads or writes the specs in the state store and reaches no cloud.
	specTimeout = time.Minute
)

// TestHA builds a cluster of three servers and two workers, changes its specs so that every machine is outdated, and
// rolls it: the servers first, then, after a restart of an old worker that must find the new servers on its own, the
// workers. A probe asks the servers for a leader and the service for a healthy allocation during each roll.
//
// It is parallel, unlike TestSmoke: Go runs the tests that are not parallel first and releases the parallel ones
// after them, so a full run builds the clusters of smoke, and only then those of this test and of TestUpgrade
// together. That keeps at most ten machines up at once.
func TestHA(t *testing.T) {
	t.Parallel()
	h := &haScenario{cluster: newCluster(t, scenarioName(suite.ID, "ha"), suite.Settings.haImage())}
	steps := []clusterStep{
		{"create", h.createHA},
		{"validate", func(t *testing.T) { h.validate(t, "validate") }},
		{"export", func(t *testing.T) { h.export(t, "export") }},
		{"service", h.pairService},
		{"change", h.change},
		{"update", h.update},
		{"roll-servers", h.rollServers},
		{"restart-client", h.restartClient},
		{"roll-workers", h.rollWorkers},
		{"validate-again", func(t *testing.T) { h.validate(t, "validate-again") }},
		// The backup step goes here, after validate-again.
	}
	if !suite.Settings.Keep {
		steps = append(steps, clusterStep{"delete", h.delete}, clusterStep{"leftovers", h.leftovers})
	}
	runSteps(t, h.cluster, steps)
}

// haScenario is the cluster of TestHA and the machines that its steps recorded before the rolls.
type haScenario struct {
	*cluster
	servers []vultrapi.Instance
	workers []vultrapi.Instance
}

func (h *haScenario) createHA(t *testing.T) {
	args := haCreateArgs(suite.Settings, h.name, suite.SSHKey+".pub", suite.Runner)
	h.tent(t, createTimeout, "create", args...)
}

// change adds an extra Nomad configuration to the specs, which changes the spec hash of every server and every worker,
// and records the machines that the rolls must replace.
func (h *haScenario) change(t *testing.T) {
	res := h.tent(t, specTimeout, "get-cluster", "get", "clusters", h.name, "-o", "json")
	changed, err := withRollChange(res.Stdout, suite.ID)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(suite.Dir, h.name+"-roll-change.json")
	if err := os.WriteFile(file, changed, 0o600); err != nil {
		t.Fatalf("write the changed specs: %v", err)
	}
	h.tent(t, specTimeout, "replace", "replace", "-f", file)

	instances, err := h.machines(stepContext(suite.Ctx, t))
	if err != nil {
		t.Fatalf("list the machines: %v", err)
	}
	if err := checkCounts(instances, h.name, haServers, haWorkers); err != nil {
		t.Fatal(err)
	}
	h.servers = machinesOf(instances, h.name, "server")
	h.workers = machinesOf(instances, h.name, "client")
}

// update applies the changed specs to the infrastructure, which leaves every machine outdated: a roll is due.
func (h *haScenario) update(t *testing.T) {
	h.tent(t, updateTimeout, "update", "update", "cluster", h.name, "--yes")
	h.rollDue(t, "roll-due", 2)
}

// rollServers rolls the servers alone while the probe runs, and waits until Vultr lists only new servers and still
// lists the old workers.
func (h *haScenario) rollServers(t *testing.T) {
	h.rollWithProbe(t, "server", "servers", "roll-servers", rollServersTimeout,
		"rolling-update", "cluster", h.name, "--yes", "--nodegroups", "servers")
	h.waitMachines(t, "the servers rolled and the workers kept", func(instances []vultrapi.Instance) error {
		if err := checkRolled(h.servers, instances); err != nil {
			return err
		}
		if err := checkCounts(instances, h.name, haServers, haWorkers); err != nil {
			return err
		}
		return checkKept(h.workers, instances)
	})
}

// waitMachines polls the machines of Vultr until check passes; Vultr drops deleted machines from its list late.
func (h *haScenario) waitMachines(t *testing.T, what string, check func([]vultrapi.Instance) error) {
	t.Helper()
	ctx := stepContext(suite.Ctx, t)
	err := pollUntil(ctx, rolledEvery, rolledTimeout, func(ctx context.Context) string {
		instances, err := h.machines(ctx)
		if err != nil {
			return err.Error()
		}
		if err := check(instances); err != nil {
			return err.Error()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("wait for %s: %v", what, err)
	}
}

// restartClient restarts the old worker with the lowest label after every server was replaced. The worker was built
// with the old servers, so only what refresh-join wrote since leads it to the new servers. The step waits until the
// worker's agent of the new boot has registered with the same node ID and the service still runs.
func (h *haScenario) restartClient(t *testing.T) {
	h.export(t, "export-servers-rolled")
	ctx := stepContext(suite.Ctx, t)
	worker, err := lowestLabel(h.workers)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := listNodes(ctx, h.nomad)
	if err != nil {
		t.Fatal(err)
	}
	node, err := nodeNamed(nodes, worker.Label)
	if err != nil {
		t.Fatal(err)
	}
	out, err := suite.sshRun(ctx, worker.MainIP, bootIDScript())
	if err != nil {
		t.Fatalf("read the boot ID of %s: %v", worker.Label, err)
	}
	e := restartEvidence{Machine: worker.Label, NodeID: node.ID}
	if e.BootBefore, err = parseBootID(out); err != nil {
		t.Fatal(err)
	}
	if _, err := suite.sshRun(ctx, worker.MainIP, rebootScript()); err != nil {
		t.Fatalf("restart %s: %v", worker.Label, err)
	}

	start := time.Now()
	err = pollUntil(ctx, bootEvery, bootTimeout, func(ctx context.Context) string {
		out, err := suite.sshRun(ctx, worker.MainIP, bootIDScript())
		if problem := bootProblem(out, err, e.BootBefore); problem != "" {
			return problem
		}
		e.BootAfter = strings.TrimSpace(out)
		return ""
	})
	e.BootWait = time.Since(start).Round(time.Second)
	if err != nil {
		t.Fatalf("wait for %s to boot again: %v", worker.Label, err)
	}

	start = time.Now()
	err = pollUntil(ctx, registrationEvery, registrationTimeout, func(ctx context.Context) string {
		out, err := suite.sshRun(ctx, worker.MainIP, registrationScript())
		if err != nil {
			return err.Error()
		}
		e.Registrations = registrationLines(out)
		nodes, err := listNodes(ctx, h.nomad)
		if err != nil {
			return err.Error()
		}
		return registeredProblem(e.Registrations, nodes, e.NodeID)
	})
	e.RegisterWait = time.Since(start).Round(time.Second)
	e.JoinFile = h.joinFile(ctx, worker)
	record := filepath.Join(suite.Dir, h.name+"-restart.txt")
	if werr := os.WriteFile(record, []byte(restartRecord(e)), 0o600); werr != nil {
		t.Errorf("write the record of the restart: %v", werr)
	}
	if err != nil {
		t.Fatalf("wait for %s to register again: %v\nrecord: %s", worker.Label, err, record)
	}
	if err := waitService(ctx, h.nomad, webJob, webService, serviceEvery, serviceTimeout); err != nil {
		t.Fatalf("service %s after the restart: %v\nrecord: %s", webService, err, record)
	}
}

// joinFile reads the file that tells the agent which servers to join, or says why it could not.
func (h *haScenario) joinFile(ctx context.Context, worker vultrapi.Instance) string {
	out, err := suite.sshRun(ctx, worker.MainIP, joinFileScript())
	if err != nil {
		return fmt.Sprintf("not read: %v\n", err)
	}
	return out
}

// rollWorkers rolls the workers, which are the only outdated machines now, and checks that each new worker carries
// the run's node meta value.
func (h *haScenario) rollWorkers(t *testing.T) {
	h.rollWithProbe(t, "server", "workers", "roll-workers", rollWorkersTimeout,
		"rolling-update", "cluster", h.name, "--yes")
	h.rollDue(t, "roll-done", 0)
	h.waitMachines(t, "the workers rolled", func(instances []vultrapi.Instance) error {
		if err := checkRolled(h.workers, instances); err != nil {
			return err
		}
		return checkCounts(instances, h.name, haServers, haWorkers)
	})
	h.export(t, "export-rolled")
	ctx := stepContext(suite.Ctx, t)
	err := pollUntil(ctx, rolledEvery, rolledTimeout, func(ctx context.Context) string {
		metas, err := h.workerMetas(ctx)
		if err != nil {
			return err.Error()
		}
		if err := checkRollMeta(metas, suite.ID, haWorkers); err != nil {
			return err.Error()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("wait for the meta of the new workers: %v", err)
	}
}

// workerMetas returns the meta values of each node that Nomad lists, by node name.
func (h *haScenario) workerMetas(ctx context.Context) (map[string]map[string]string, error) {
	nodes, err := listNodes(ctx, h.nomad)
	if err != nil {
		return nil, err
	}
	metas := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		if metas[n.Name], err = nodeMeta(ctx, h.nomad, n.ID); err != nil {
			return nil, err
		}
	}
	return metas, nil
}
