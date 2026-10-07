//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	createTimeout    = 25 * time.Minute
	validateTimeout  = 7 * time.Minute
	deleteTimeout    = 15 * time.Minute
	purgeTimeout     = time.Minute
	serviceEvery     = 5 * time.Second
	serviceTimeout   = 5 * time.Minute
	leftoversEvery   = 10 * time.Second
	leftoversTimeout = 2 * time.Minute
	noClustersText   = "no clusters in"
)

// TestSmoke builds a Nomad cluster for each image, checks it from outside and deletes it. The images run in
// parallel; the steps of one image run in order and the first step that fails ends that image.
func TestSmoke(t *testing.T) {
	for _, image := range suite.Settings.Images {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			smokeImage(t, image)
		})
	}
}

// smokeCluster is the cluster of one image and what its steps hand to each other.
type smokeCluster struct {
	name  string
	image string
	nomad *nomadAPI
	// deleteRan is true once the delete step has started, so the cleanup does not delete the cluster again.
	deleteRan bool
}

func (c *smokeCluster) exportDir() string {
	return filepath.Join(suite.Dir, "export-"+c.name)
}

// smokeStep is one subtest of an image.
type smokeStep struct {
	name string
	run  func(*testing.T)
}

// smokeImage runs the steps of one image. Each step logs the time it took.
func smokeImage(t *testing.T, image string) {
	c := &smokeCluster{name: clusterName(suite.ID, image), image: image}
	t.Cleanup(func() {
		if err := os.RemoveAll(c.exportDir()); err != nil {
			t.Logf("remove the export of %s, which holds its token and client key: %v", c.name, err)
		}
	})
	t.Cleanup(func() { c.cleanup(t) })

	steps := []smokeStep{
		{"create", c.create},
		{"validate", func(t *testing.T) { c.validate(t, "validate") }},
		{"export", c.export},
		{"service", c.service},
		{"validate-again", func(t *testing.T) { c.validate(t, "validate-again") }},
	}
	if !suite.Settings.Keep {
		steps = append(steps, smokeStep{"delete", c.delete}, smokeStep{"leftovers", c.leftovers})
	}
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

// tent runs a tent command of the cluster and fails the step when it does not exit with 0.
func (c *smokeCluster) tent(t *testing.T, timeout time.Duration, step string, args ...string) tentResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	res, err := suite.Tent.run(ctx, c.name, step, args...)
	if err != nil {
		t.Fatalf("tent %s: %v", step, err)
	}
	if res.Code != 0 {
		t.Fatal(tentFailure(step, res, suite.Tent.logPath(c.name, step)))
	}
	return res
}

func (c *smokeCluster) create(t *testing.T) {
	args := createArgs(suite.Settings, c.name, c.image, suite.SSHKey+".pub", suite.Runner)
	c.tent(t, createTimeout, "create", args...)
}

func (c *smokeCluster) validate(t *testing.T, step string) {
	c.tent(t, validateTimeout, step, validateArgs(c.name)...)
}

// export has tent write the files that reach Nomad and builds the client from them.
func (c *smokeCluster) export(t *testing.T) {
	res := c.tent(t, time.Minute, "export", "export", "nomad", c.name, "--dir", c.exportDir(), "-o", "json")
	x, err := parseExport(res.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if c.nomad, err = newNomad(x); err != nil {
		t.Fatal(err)
	}
}

// service runs the web job and waits until its allocation, its check and its registration agree.
func (c *smokeCluster) service(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "web.nomad.hcl"))
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	if err := submitJob(t.Context(), c.nomad, string(text)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
		defer cancel()
		if err := purgeJob(ctx, c.nomad, webJob); err != nil {
			t.Errorf("purge the job: %v", err)
		}
	})
	if err := waitService(t.Context(), c.nomad, webJob, webService, serviceEvery, serviceTimeout); err != nil {
		t.Fatalf("service %s: %v", webService, err)
	}
}

func (c *smokeCluster) delete(t *testing.T) {
	c.deleteRan = true
	c.tent(t, deleteTimeout, "delete", "delete", "cluster", c.name, "--yes")
}

// leftovers checks that Vultr holds nothing of the cluster and that its store is empty.
func (c *smokeCluster) leftovers(t *testing.T) {
	if err := waitNoLeftovers(t.Context(), suite.Vultr, c.name, leftoversEvery, leftoversTimeout, t.Logf); err != nil {
		t.Fatalf("after the delete of %s: %v", c.name, err)
	}
	res := c.tent(t, time.Minute, "get-clusters", "get", "clusters")
	if !strings.Contains(string(res.Stderr), noClustersText) {
		t.Fatalf("tent get clusters: stderr does not hold %q: %s\nlog: %s", noClustersText,
			lastLines(string(res.Stderr), stderrTailLines), suite.Tent.logPath(c.name, "get-clusters"))
	}
}

// cleanup deletes the cluster when the delete step did not run, with a context of its own because the test's has
// ended. A run that keeps its clusters deletes nothing and logs the command that does.
func (c *smokeCluster) cleanup(t *testing.T) {
	if c.deleteRan {
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
