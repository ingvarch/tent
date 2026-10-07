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
	metadataEvery    = 5 * time.Second
	metadataTimeout  = 4 * time.Minute
	introEvery       = 5 * time.Second
	introTimeout     = 90 * time.Second
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
	// deleteRan is true once the delete step has started; deleteDone is true once tent delete exited 0. The cleanup
	// deletes the cluster only when no delete ran or a signal ended it (deleteInCleanup).
	deleteRan  bool
	deleteDone bool
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
		{"metadata", c.metadata},
		{"intro-token", c.introToken},
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
	ctx, cancel := context.WithTimeout(stepContext(suite.Ctx, t), timeout)
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

// runJob submits the job of a file in testdata and purges it when the step ends.
func (c *smokeCluster) runJob(ctx context.Context, t *testing.T, file, job string) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	if err := submitJob(ctx, c.nomad, string(text)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
		defer cancel()
		if err := purgeJob(ctx, c.nomad, job); err != nil {
			t.Errorf("purge the job %s: %v", job, err)
		}
	})
}

// service runs the web job and waits until its allocation, its check and its registration agree.
func (c *smokeCluster) service(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	c.runJob(ctx, t, "web.nomad.hcl", webJob)
	if err := waitService(ctx, c.nomad, webJob, webService, serviceEvery, serviceTimeout); err != nil {
		t.Fatalf("service %s: %v", webService, err)
	}
}

// metadata runs a probe on each network a container can use and checks that none reaches the metadata service.
func (c *smokeCluster) metadata(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	c.runJob(ctx, t, "metadata.nomad.hcl", metadataJob)
	results, err := collectMetadata(ctx, c.nomad, metadataEvery, metadataTimeout)
	for _, r := range results {
		if r.Terminated {
			t.Logf("%s: exit %d, stderr: %s", r.Group, r.ExitCode, strings.TrimSpace(r.Stderr))
		}
	}
	if err := metadataOutcome(results, err); err != nil {
		t.Fatalf("metadata: %v", err)
	}
}

// introToken starts a second agent without an intro token on the client machine and checks that the server
// refuses it. The agent is stopped and its files are removed when the step ends.
func (c *smokeCluster) introToken(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	instances, err := suite.Vultr.Instances(ctx)
	if err != nil {
		t.Fatalf("list the machines: %v", err)
	}
	server, err := onlyMachine(instances, c.name, "server")
	if err != nil {
		t.Fatal(err)
	}
	client, err := onlyMachine(instances, c.name, "client")
	if err != nil {
		t.Fatal(err)
	}
	clock, err := suite.sshRun(ctx, server.MainIP, "date +%s\n")
	if err != nil {
		t.Fatalf("read the server's clock: %v", err)
	}
	epoch, err := parseEpoch(clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := suite.sshRun(context.Background(), client.MainIP, rogueStopScript()); err != nil {
			t.Errorf("stop the rogue agent on %s: %v", client.Label, err)
		}
	})
	if _, err := suite.sshRun(ctx, client.MainIP, rogueStartScript()); err != nil {
		t.Fatalf("start the rogue agent on %s: %v", client.Label, err)
	}
	read := func(ctx context.Context) (introEvidence, error) {
		return c.readIntro(ctx, server.MainIP, client.MainIP, epoch)
	}
	ev, waitErr := waitIntroRejected(ctx, read, introEvery, introTimeout)
	record := filepath.Join(suite.Dir, c.name+"-intro-token.txt")
	if err := os.WriteFile(record, []byte(introRecord(ev)), 0o600); err != nil {
		t.Errorf("write the record of the step: %v", err)
	}
	if waitErr != nil {
		t.Fatalf("intro token: %v\nrecord: %s", waitErr, record)
	}
}

// readIntro reads the three places that show whether the rogue agent was refused: the node list, the server's
// journal since epoch and the rogue's log on the client machine. It returns what it read when a read fails.
func (c *smokeCluster) readIntro(ctx context.Context, serverIP, clientIP string, epoch int64) (introEvidence, error) {
	var ev introEvidence
	var err error
	if ev.Nodes, err = listNodeNames(ctx, c.nomad); err != nil {
		return ev, err
	}
	journal, err := suite.sshRun(ctx, serverIP, journalScript(epoch))
	if err != nil {
		return ev, err
	}
	ev.Journal = nonEmptyLines(journal)
	ev.RogueLog, err = suite.sshRun(ctx, clientIP, rogueLogScript())
	return ev, err
}

func (c *smokeCluster) delete(t *testing.T) {
	c.deleteRan = true
	c.tent(t, deleteTimeout, "delete", "delete", "cluster", c.name, "--yes")
	c.deleteDone = true
}

// leftovers checks that Vultr holds nothing of the cluster and that its store is empty.
func (c *smokeCluster) leftovers(t *testing.T) {
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

// cleanup deletes the cluster when the delete step did not run or a signal ended it (deleteInCleanup), with a context
// of its own because the test's has ended. A run that keeps its clusters deletes nothing and logs the command that
// does.
func (c *smokeCluster) cleanup(t *testing.T) {
	if !deleteInCleanup(c.deleteRan, c.deleteDone, suite.Ctx.Err() != nil) {
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
