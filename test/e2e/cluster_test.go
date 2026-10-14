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
	validateTimeout  = 7 * time.Minute
	deleteTimeout    = 15 * time.Minute
	purgeTimeout     = time.Minute
	serviceEvery     = 5 * time.Second
	serviceTimeout   = 5 * time.Minute
	leftoversEvery   = 10 * time.Second
	leftoversTimeout = 2 * time.Minute
	noClustersText   = "no clusters in"
	// previewTimeout bounds a run of a command that reads the cloud and Nomad and changes nothing.
	previewTimeout = 5 * time.Minute
	// pairAllocations is the number of allocations of the job in testdata/web-pair.nomad.hcl.
	pairAllocations = 2
)

// cluster is a cluster of a scenario and what its steps hand to each other.
type cluster struct {
	name  string
	image string
	nomad *nomadAPI
	// ctxErr is the error of the context of the latest tent command after tent returned. It tells a command that a
	// deadline or a signal cut short from one that ended on its own.
	ctxErr error
	// deleteRan is true once the delete step has started; deleteDone is true once tent delete exited 0; deleteErr is
	// ctxErr after the delete step. The cleanup deletes the cluster only when no delete ran or the delete was cut
	// short (deleteInCleanup).
	deleteRan  bool
	deleteDone bool
	deleteErr  error
}

// newCluster returns the cluster and registers its cleanups on t: the removal of its export directory and, when its
// steps did not delete it, the delete.
func newCluster(t *testing.T, name, image string) *cluster {
	c := &cluster{name: name, image: image}
	t.Cleanup(func() {
		if err := os.RemoveAll(c.exportDir()); err != nil {
			t.Logf("remove the export of %s, which holds its token and client key: %v", c.name, err)
		}
	})
	t.Cleanup(func() { c.cleanup(t) })
	return c
}

func (c *cluster) exportDir() string {
	return filepath.Join(suite.Dir, "export-"+c.name)
}

// clusterStep is one subtest of a cluster.
type clusterStep struct {
	name string
	run  func(*testing.T)
}

// runSteps runs the steps of a cluster in order, each as a subtest that logs the time it took. The first step that
// fails ends the test.
func runSteps(t *testing.T, c *cluster, steps []clusterStep) {
	for _, step := range steps {
		ok := t.Run(step.name, func(t *testing.T) {
			start := time.Now()
			defer func() { t.Logf("%s %s took %s", c.name, step.name, time.Since(start).Round(time.Second)) }()
			step.run(t)
		})
		if !ok {
			t.FailNow()
		}
	}
}

// tentCode runs a tent command of the cluster and returns its result, whatever its exit code, for the commands
// whose code a scenario reads. It fails the step when tent cannot be run.
func (c *cluster) tentCode(t *testing.T, timeout time.Duration, step string, args ...string) tentResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(stepContext(suite.Ctx, t), timeout)
	defer cancel()
	res, err := suite.Tent.run(ctx, c.name, step, args...)
	c.ctxErr = ctx.Err()
	if err != nil {
		t.Fatalf("tent %s: %v", step, err)
	}
	return res
}

// tent runs a tent command of the cluster and fails the step when it does not exit with 0.
func (c *cluster) tent(t *testing.T, timeout time.Duration, step string, args ...string) tentResult {
	t.Helper()
	res := c.tentCode(t, timeout, step, args...)
	if res.Code != 0 {
		t.Fatal(tentFailure(step, res, suite.Tent.logPath(c.name, step)))
	}
	return res
}

func (c *cluster) validate(t *testing.T, step string) {
	c.tent(t, validateTimeout, step, validateArgs(c.name)...)
}

// export has tent write the files that reach Nomad and builds the client from them. The step names its log file.
func (c *cluster) export(t *testing.T, step string) {
	res := c.tent(t, time.Minute, step, "export", "nomad", c.name, "--dir", c.exportDir(), "-o", "json")
	x, err := parseExport(res.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if c.nomad, err = newNomad(x); err != nil {
		t.Fatal(err)
	}
}

// submitJob submits the job of a file in testdata and leaves it in the cluster.
func (c *cluster) submitJob(ctx context.Context, t *testing.T, file string) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	if err := submitJob(ctx, c.nomad, string(text)); err != nil {
		t.Fatal(err)
	}
}

// runJob submits the job of a file in testdata and purges it when the step ends.
func (c *cluster) runJob(ctx context.Context, t *testing.T, file, job string) {
	t.Helper()
	c.submitJob(ctx, t, file)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
		defer cancel()
		if err := purgeJob(ctx, c.nomad, job); err != nil {
			t.Errorf("purge the job %s: %v", job, err)
		}
	})
}

// service runs the web job and waits until its allocation, its check and its registration agree.
func (c *cluster) service(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	c.runJob(ctx, t, "web.nomad.hcl", webJob)
	if err := waitService(ctx, c.nomad, webJob, webService, serviceEvery, serviceTimeout); err != nil {
		t.Fatalf("service %s: %v", webService, err)
	}
}

// pairService submits the job of two allocations, leaves it in the cluster, and waits until both run and the service
// is healthy.
func (c *cluster) pairService(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	c.submitJob(ctx, t, "web-pair.nomad.hcl")
	err := pollUntil(ctx, serviceEvery, serviceTimeout, func(ctx context.Context) string {
		running, err := runningAllocations(ctx, c.nomad, webJob)
		if err != nil {
			return err.Error()
		}
		if problem := allocationsProblem(running, webJob, pairAllocations); problem != "" {
			return problem
		}
		return serviceProblem(ctx, c.nomad, webJob, webService)
	})
	if err != nil {
		t.Fatalf("service %s: %v", webService, err)
	}
}

// machines lists every machine of the Vultr account; machinesOf picks those of a cluster.
func (c *cluster) machines(ctx context.Context) ([]vultrapi.Instance, error) {
	return suite.Vultr.Instances(ctx)
}

// addresses returns the function that lists the API addresses of the cluster's machines of a role.
func (c *cluster) addresses(role string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		instances, err := c.machines(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the machines: %w", err)
		}
		return serverAddresses(instances, c.name, role), nil
	}
}

// rollWithProbe runs tent with args as the step while a probe polls the machines of a role, and writes the probe's
// records to <cluster>-probe-<roll>.tsv. The step fails when tent does not exit with 0 or when the probe's verdict
// is an error.
func (c *cluster) rollWithProbe(t *testing.T, role, roll, step string, timeout time.Duration, args ...string) {
	t.Helper()
	ctx := stepContext(suite.Ctx, t)
	addresses := c.addresses(role)
	first, err := addresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatalf("no %s machine of %s has an address to probe", role, c.name)
	}
	path := filepath.Join(suite.Dir, c.name+"-probe-"+roll+".tsv")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open the probe's records: %v", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the probe's records %s: %v", path, err)
		}
	}()
	p := startProbe(ctx, newProbeConfig(c.nomad, addresses, webJob, webService, out))
	defer p.stop()

	res := c.tentCode(t, timeout, step, args...)
	summary, verr := probeVerdict(p.stop(), defaultProbeLimits)
	t.Logf("%s probe during %s: %s\nrecords: %s", c.name, roll, summary, path)
	if verr != nil {
		t.Errorf("%s: %v", roll, verr)
	}
	if res.Code != 0 {
		t.Fatal(tentFailure(step, res, suite.Tent.logPath(c.name, step)))
	}
	if verr != nil {
		t.FailNow()
	}
}

// rollDue checks that tent rolling-update without --yes exits with want: 2 while a roll is due, 0 when none is.
func (c *cluster) rollDue(t *testing.T, step string, want int) {
	t.Helper()
	res := c.tentCode(t, previewTimeout, step, "rolling-update", "cluster", c.name, "--exit-code")
	if res.Code != want {
		t.Fatalf("tent %s: want exit code %d\n%s", step, want, tentFailure(step, res, suite.Tent.logPath(c.name, step)))
	}
}

func (c *cluster) delete(t *testing.T) {
	c.deleteRan = true
	defer func() { c.deleteErr = c.ctxErr }()
	c.tent(t, deleteTimeout, "delete", "delete", "cluster", c.name, "--yes")
	c.deleteDone = true
}

// leftovers checks that Vultr holds nothing of the cluster and that its store is empty.
func (c *cluster) leftovers(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	if err := waitNoLeftovers(ctx, suite.Vultr, c.name, leftoversEvery, leftoversTimeout, t.Logf); err != nil {
		t.Fatalf("after the delete of %s: %v", c.name, err)
	}
	res := c.tent(t, time.Minute, "get-clusters", "get", "clusters")
	if !strings.Contains(string(res.Stderr), noClustersText) {
		t.Fatalf("tent get clusters: stderr does not hold %q: %s\nlog: %s", noClustersText,
			lastLines(string(res.Stderr), stderrTailLines), suite.Tent.logPath(c.name, "get-clusters"))
	}
}

// cleanup deletes the cluster when the delete step did not run or was cut short (deleteInCleanup), with a context of
// its own because the test's has ended. A run that keeps its clusters deletes nothing and logs the command that
// does.
func (c *cluster) cleanup(t *testing.T) {
	if !deleteInCleanup(c.deleteRan, c.deleteDone, suite.Ctx.Err() != nil, c.deleteErr) {
		return
	}
	if suite.Settings.Keep {
		t.Logf("keeping cluster %s; delete it with: %s", c.name, suite.Tent.deleteCommand(c.name))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	step := "cleanup-delete"
	res, err := suite.Tent.run(ctx, c.name, step, "delete", "cluster", c.name, "--yes")
	switch {
	case err != nil:
		t.Errorf("cleanup: delete cluster %s: %v", c.name, err)
	case res.Code != 0:
		t.Errorf("cleanup: delete cluster %s failed:\n%s", c.name,
			tentFailure(step, res, suite.Tent.logPath(c.name, step)))
	default:
		t.Logf("cleanup: deleted cluster %s in %s", c.name, res.Took.Round(time.Second))
	}
}
