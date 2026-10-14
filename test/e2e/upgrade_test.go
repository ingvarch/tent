//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const (
	// upgradeRollTimeout bounds the roll of the combined group, one new node first for each of its nodes.
	upgradeRollTimeout = 60 * time.Minute
	// upgradeTimeout bounds tent upgrade cluster, which downloads and verifies the signed checksums of a Nomad
	// release.
	upgradeTimeout = 5 * time.Minute
)

// TestUpgrade builds a combined cluster of three nodes on the old Nomad version, has tent upgrade cluster move the
// spec to the version that the channel recommends, and rolls the nodes while a probe asks for a leader and a healthy
// allocation of the service. At the end every node and member runs the new version.
//
// It is parallel for the reason that TestHA is.
func TestUpgrade(t *testing.T) {
	t.Parallel()
	u := &upgradeScenario{cluster: newCluster(t, scenarioName(suite.ID, "upgrade"), suite.Settings.upgradeImage())}
	steps := []clusterStep{
		{"create", u.create},
		{"validate", func(t *testing.T) { u.validate(t, "validate") }},
		{"export", func(t *testing.T) { u.export(t, "export") }},
		{"service", u.pairService},
		{"versions-before", u.versionsBefore},
		{"upgrade", u.upgrade},
		{"roll", u.roll},
		{"versions-after", u.versionsAfter},
		{"validate-again", u.validateAgain},
	}
	if !suite.Settings.Keep {
		steps = append(steps, clusterStep{"delete", u.delete}, clusterStep{"leftovers", u.leftovers})
	}
	runSteps(t, u.cluster, steps)
}

// upgradeScenario is the cluster of TestUpgrade, the machines that its steps recorded before the roll and the Nomad
// version that tent upgrade cluster chose.
type upgradeScenario struct {
	*cluster
	before []vultrapi.Instance
	target string
}

// create builds the cluster on the old version. When tent warns that the version is untested, the step logs the
// warning and goes on.
func (u *upgradeScenario) create(t *testing.T) {
	args := upgradeCreateArgs(suite.Settings, u.name, u.image, suite.SSHKey+".pub", suite.Runner)
	res := u.tent(t, createTimeout, "create", args...)
	if warning := untestedVersionWarning(res.Stderr); warning != "" {
		t.Logf("tent warns about the old version: %s", warning)
	}
}

// versionsBefore checks that every node runs the old version and records the machines that the roll must replace.
func (u *upgradeScenario) versionsBefore(t *testing.T) {
	ctx := stepContext(suite.Ctx, t)
	nodes, err := listNodes(ctx, u.nomad)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != upgradeNodes {
		t.Fatalf("Nomad lists %d nodes, want %d", len(nodes), upgradeNodes)
	}
	for _, n := range nodes {
		if n.Version != suite.Settings.UpgradeFrom {
			t.Fatalf("node %s runs Nomad %s, want %s", n.Name, n.Version, suite.Settings.UpgradeFrom)
		}
	}
	instances, err := u.machines(ctx)
	if err != nil {
		t.Fatalf("list the machines: %v", err)
	}
	u.before = machinesOf(instances, u.name, "combined")
	if len(u.before) != upgradeNodes {
		t.Fatalf("Vultr lists %d combined machines of %s, want %d", len(u.before), u.name, upgradeNodes)
	}
}

// upgrade moves the spec to the version that the channel recommends, applies it to the infrastructure, which leaves
// every node outdated, and checks that a roll is due.
func (u *upgradeScenario) upgrade(t *testing.T) {
	res := u.tent(t, upgradeTimeout, "upgrade", "upgrade", "cluster", u.name, "--yes")
	said, err := upgradedVersion(res.Stdout, u.name)
	if err != nil {
		t.Fatal(err)
	}
	res = u.tent(t, specTimeout, "get-cluster", "get", "clusters", u.name, "-o", "json")
	if u.target, err = upgradeTarget(res.Stdout, suite.Settings.UpgradeFrom); err != nil {
		t.Fatal(err)
	}
	if u.target != said {
		t.Fatalf("tent upgrade says the spec has Nomad %s now, get clusters gives %s", said, u.target)
	}
	t.Logf("%s upgrades from Nomad %s to %s", u.name, suite.Settings.UpgradeFrom, u.target)
	u.tent(t, updateTimeout, "update", "update", "cluster", u.name, "--yes")
	u.rollDue(t, "roll-due", 2)
}

// roll rolls the combined group while the probe runs and checks that no roll is due afterwards.
func (u *upgradeScenario) roll(t *testing.T) {
	u.rollWithProbe(t, "combined", "nodes", "roll", defaultProbeLimits, upgradeRollTimeout,
		"rolling-update", "cluster", u.name, "--yes")
	u.rollDue(t, "roll-done", 0)
}

// versionsAfter exports again, because the roll deleted the server that the first export named, and waits until the
// old machines are gone and every node and member runs the target version.
func (u *upgradeScenario) versionsAfter(t *testing.T) {
	u.export(t, "export-rolled")
	ctx := stepContext(suite.Ctx, t)
	err := pollUntil(ctx, rolledEvery, rolledTimeout, func(ctx context.Context) string {
		if err := u.versionsProblem(ctx); err != nil {
			return err.Error()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("wait for every node and member to run Nomad %s: %v", u.target, err)
	}
}

// versionsProblem reads the machines, the nodes and the members and returns what keeps them from being the new
// nodes on the target version.
func (u *upgradeScenario) versionsProblem(ctx context.Context) error {
	instances, err := u.machines(ctx)
	if err != nil {
		return err
	}
	if err := checkRolled(u.before, instances); err != nil {
		return err
	}
	current := machinesOf(instances, u.name, "combined")
	if len(current) != upgradeNodes {
		return fmt.Errorf("Vultr lists %d combined machines of %s, want %d", len(current), u.name, upgradeNodes)
	}
	nodes, err := listNodes(ctx, u.nomad)
	if err != nil {
		return err
	}
	members, err := listMembers(ctx, u.nomad)
	if err != nil {
		return err
	}
	return checkVersions(nodes, members, current, u.target)
}

// validateAgain checks that validate exits with 0 and says that the cluster runs the target version.
func (u *upgradeScenario) validateAgain(t *testing.T) {
	res := u.tent(t, validateTimeout, "validate-again", validateArgs(u.name)...)
	if err := checkValidVersion(res.Stdout, u.name, u.target); err != nil {
		t.Fatal(err)
	}
}
