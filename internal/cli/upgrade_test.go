package cli

import (
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// upgradeOptions returns the options of a run that has the test release files, no cloud and no Nomad: an upgrade
// needs neither of the two.
func upgradeOptions() *globalOptions { return &globalOptions{assets: testAssets()} }

// upgradeArgs returns the arguments that upgrade the test cluster in the store s, followed by more.
func upgradeArgs(s state, more ...string) []string {
	return append([]string{"upgrade", "cluster", "prod", "--state", s.url}, more...)
}

// onNomad creates the test cluster with the given Nomad version in a new store, as create cluster does.
func onNomad(t *testing.T, version string, more ...string) state {
	t.Helper()
	s := newState(t)
	args := append(createProd(s, "--nomad-version", version), more...)
	got := runWith(t, upgradeOptions(), args...)
	if got.code != 0 {
		t.Fatalf("create cluster: exit code %d\n%s", got.code, got.errOut)
	}
	return s
}

const (
	upgradeDue = "cluster prod: upgrade Nomad from 2.0.6 to 2.0.7, which channel stable recommends.\n" +
		"After the upgrade: tent update cluster, then tent rolling-update cluster.\n"
	upgradeApplied = "cluster prod: spec.nomad.version is now 2.0.7; the cluster was on Nomad 2.0.6.\n" +
		"Next: tent update cluster, then tent rolling-update cluster.\n"
	upgradeHint = "run with --yes to write spec.nomad.version\n"
)

// TestUpgradeClusterPrintsThePlanAndChangesNothing without --yes, and says how to write it.
func TestUpgradeClusterPrintsThePlanAndChangesNothing(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6")
	before := s.objects(t)
	wantResult(t, runWith(t, upgradeOptions(), upgradeArgs(s)...), 0, upgradeDue, upgradeHint)
	s.want(t, before)
}

// TestUpgradeClusterWritesTheVersion with --yes: the text, the warning about the open API, and a get that shows
// 2.0.7.
func TestUpgradeClusterWritesTheVersion(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6")
	wantResult(t, runWith(t, upgradeOptions(), upgradeArgs(s, "--yes")...), 0, upgradeApplied, openAPIWarning)
	got := runIn(t, "", "get", "cluster", "prod", "-o", "yaml", "--state", s.url)
	if !strings.Contains(got.out, "    version: 2.0.7\n") || strings.Contains(got.out, "2.0.6") {
		t.Errorf("get cluster prod:\n%s\nwant spec.nomad.version 2.0.7", got.out)
	}
}

// TestUpgradeClusterTakesNameOrFlag like the other commands that name a cluster.
func TestUpgradeClusterTakesNameOrFlag(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6")
	wantResult(t, runWith(t, upgradeOptions(), "upgrade", "cluster", "--name", "prod", "--state", s.url), 0,
		upgradeDue, upgradeHint)
}

// TestUpgradeClusterPrintsThePlanAsData with -o json and -o yaml, before and after the write.
func TestUpgradeClusterPrintsThePlanAsData(t *testing.T) {
	t.Parallel()
	const (
		planJSON = "{\n  \"cluster\": \"prod\",\n  \"channel\": \"stable\",\n  \"recommended\": \"2.0.7\",\n" +
			"  \"spec\": \"2.0.6\",\n  \"current\": \"2.0.6\",\n  \"target\": \"2.0.7\"\n}\n"
		appliedJSON = "{\n  \"cluster\": \"prod\",\n  \"channel\": \"stable\",\n  \"recommended\": \"2.0.7\",\n" +
			"  \"spec\": \"2.0.6\",\n  \"current\": \"2.0.6\",\n  \"target\": \"2.0.7\",\n  \"applied\": true\n}\n"
		planYAML    = "channel: stable\ncluster: prod\ncurrent: 2.0.6\nrecommended: 2.0.7\nspec: 2.0.6\ntarget: 2.0.7\n"
		appliedYAML = "applied: true\n" + planYAML
	)
	for _, tc := range []struct {
		name   string
		args   []string
		out    string
		errOut string
	}{
		{"json", []string{"-o", "json"}, planJSON, upgradeHint},
		{"yaml", []string{"-o", "yaml"}, planYAML, upgradeHint},
		{"json applied", []string{"-o", "json", "--yes"}, appliedJSON, openAPIWarning},
		{"yaml applied", []string{"-o", "yaml", "--yes"}, appliedYAML, openAPIWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := onNomad(t, "2.0.6")
			wantResult(t, runWith(t, upgradeOptions(), upgradeArgs(s, tc.args...)...), 0, tc.out, tc.errOut)
		})
	}
}

// TestUpgradeClusterHasNothingToUpgrade prints why, exits with 0, writes nothing and warns of nothing, also with
// --yes.
func TestUpgradeClusterHasNothingToUpgrade(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		make func(t *testing.T) state
		want string
	}{
		{"the channel's version", func(t *testing.T) state { return onNomad(t, "2.0.7") },
			"cluster prod: nothing to upgrade: its Nomad version is 2.0.7, which channel stable recommends.\n"},
		{"no version yet", withCluster,
			"cluster prod: nothing to upgrade: it has no Nomad version yet; its first tent update cluster takes " +
				"2.0.7.\n"},
		{"a newer version", func(t *testing.T) state { return onNomad(t, "2.0.8") },
			"cluster prod: nothing to upgrade: its Nomad version is 2.0.8, newer than the 2.0.7 that channel stable " +
				"recommends.\n"},
	} {
		for _, yes := range []bool{false, true} {
			t.Run(tc.name+" yes="+map[bool]string{false: "no", true: "yes"}[yes], func(t *testing.T) {
				s := tc.make(t)
				before := s.objects(t)
				var more []string
				if yes {
					more = []string{"--yes"}
				}
				wantOK(t, runWith(t, upgradeOptions(), upgradeArgs(s, more...)...), tc.want)
				s.want(t, before)
			})
		}
	}
}

// TestUpgradeClusterFailsForAMissingClusterOrName prints the error, exits with 1 and writes nothing, also for two
// names.
func TestUpgradeClusterFailsForAMissingClusterOrName(t *testing.T) {
	t.Parallel()
	s := withCluster(t)
	before := s.objects(t)
	path := configFile(t)
	wantError(t, runWith(t, upgradeOptions(), "upgrade", "cluster", "dev", "--state", s.url),
		"Error: cluster dev not found in "+s.url+"\n")
	wantError(t, runWith(t, upgradeOptions(), "upgrade", "cluster", "--state", s.url),
		"Error: no cluster name: give NAME or set --name, TENT_CLUSTER or \"cluster\" in "+path+"\n")
	wantError(t, runWith(t, upgradeOptions(), upgradeArgs(s, "dev", "--yes")...),
		"Error: accepts at most 1 arg(s), received 2\n")
	s.want(t, before)
}

// TestUpgradeClusterNeedsAllowSingleServer for a cluster of one server, as every spec writer does.
func TestUpgradeClusterNeedsAllowSingleServer(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6", "--servers", "1", "--allow-single-server")
	before := s.objects(t)
	wantError(t, runWith(t, upgradeOptions(), upgradeArgs(s, "--yes")...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: size 1 needs --allow-single-server\n")
	s.want(t, before)
	wantResult(t, runWith(t, upgradeOptions(), upgradeArgs(s, "--yes", "--allow-single-server")...), 0,
		upgradeApplied, openAPIWarning)
}

// TestUpgradeClusterPrintsWhatItWroteWhenTheLockIsLost and then the error, as replace does.
func TestUpgradeClusterPrintsWhatItWroteWhenTheLockIsLost(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6")
	losing := func(st statestore.Store) statestore.Store { return losingStore{Store: st, after: clusterPath} }
	opts := upgradeOptions()
	opts.openStore = wrapped(losing)
	wantResult(t, runWith(t, opts, upgradeArgs(s, "--yes")...), 1, upgradeApplied,
		openAPIWarning+"Error: the change is saved, but the lock of cluster prod was lost before tent released it\n")
}

// TestUpgradeClusterTakesNoExitCodeAndNoTarget has no flag for either.
func TestUpgradeClusterTakesNoExitCodeAndNoTarget(t *testing.T) {
	t.Parallel()
	s := onNomad(t, "2.0.6")
	for _, flag := range []string{"--exit-code", "--nomad-version"} {
		got := runWith(t, upgradeOptions(), upgradeArgs(s, flag, "2.0.7")...)
		if got.code != 1 || !strings.HasPrefix(got.errOut, "Error: unknown flag: "+flag+"\n") {
			t.Errorf("%s: exit code %d, stderr %q; want 1 and an unknown flag", flag, got.code, got.errOut)
		}
	}
}

// TestUpgradeClusterHelp tells what the command does and which flags it takes.
func TestUpgradeClusterHelp(t *testing.T) {
	t.Parallel()
	got := runIn(t, "", "upgrade", "cluster", "--help")
	for _, want := range []string{"tent upgrade cluster [NAME] [flags]", "spec.nomad.version", "at most one minor version",
		"--yes", "--allow-single-server", "needs no cloud credentials"} {
		if !strings.Contains(got.out, want) {
			t.Errorf("tent upgrade cluster --help lacks %q:\n%s", want, got.out)
		}
	}
}
