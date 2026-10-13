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
	createTimeout   = 25 * time.Minute
	metadataEvery   = 5 * time.Second
	metadataTimeout = 4 * time.Minute
	introEvery      = 5 * time.Second
	introTimeout    = 90 * time.Second
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

// smokeImage runs the steps of one image.
func smokeImage(t *testing.T, image string) {
	c := newCluster(t, clusterName(suite.ID, image), image)
	steps := []clusterStep{
		{"create", c.create},
		{"validate", func(t *testing.T) { c.validate(t, "validate") }},
		{"export", func(t *testing.T) { c.export(t, "export") }},
		{"service", c.service},
		{"metadata", c.metadata},
		{"intro-token", c.introToken},
		{"validate-again", func(t *testing.T) { c.validate(t, "validate-again") }},
	}
	if !suite.Settings.Keep {
		steps = append(steps, clusterStep{"delete", c.delete}, clusterStep{"leftovers", c.leftovers})
	}
	runSteps(t, c, steps)
}

func (c *cluster) create(t *testing.T) {
	args := createArgs(suite.Settings, c.name, c.image, suite.SSHKey+".pub", suite.Runner)
	c.tent(t, createTimeout, "create", args...)
}

// metadata runs a probe on each network a container can use and checks that none reaches the metadata service.
func (c *cluster) metadata(t *testing.T) {
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
func (c *cluster) introToken(t *testing.T) {
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
func (c *cluster) readIntro(ctx context.Context, serverIP, clientIP string, epoch int64) (introEvidence, error) {
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
