package cli

import (
	"strings"
	"testing"
)

// nameRule is what is wrong with a name such as PROD.
const nameRule = "must be 2 to 20 lowercase letters, digits or dashes, starting with a letter and ending with a " +
	"letter or digit"

// TestNamesOutsideTheRuleAreRefused keeps PROD from naming prod's state on a disk that ignores case.
func TestNamesOutsideTheRuleAreRefused(t *testing.T) {
	s := withCluster(t)
	const invalid = "Error: invalid cluster name \"PROD\": " + nameRule + "\n"
	for _, args := range [][]string{
		{"get", "PROD"},
		{"get", "nodegroups", "--name", "PROD"},
		{"delete", "cluster", "PROD", "--yes"},
		{"state", "unlock", "PROD", "--force"},
	} {
		wantError(t, runIn(t, "", append(args, "--state", s.url)...), invalid)
		s.want(t, prodObjects)
	}
}

// TestSpecNamesOutsideTheRuleAreRefused before tent reads the store under them: PROD is no copy of prod.
func TestSpecNamesOutsideTheRuleAreRefused(t *testing.T) {
	s := withCluster(t)
	const invalid = "Error: invalid spec:\n  Cluster PROD: metadata.name: " + nameRule + "\n"
	upper := strings.ReplaceAll(prodYAML, "prod", "PROD")
	for _, args := range [][]string{
		{"create", "-f", "-"},
		{"replace", "-f", "-"},
		{"create", "cluster", "PROD", "--provider", "vultr", "--region", "ams", "--machine-type", "x", "--dry-run"},
	} {
		wantError(t, runIn(t, upper, append(args, "--state", s.url)...), invalid)
		s.want(t, prodObjects)
	}
}

// TestMistypedSubcommands fail instead of printing help.
func TestMistypedSubcommands(t *testing.T) {
	s := withCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"state", "unlok", "prod"}, `Error: unknown command "unlok" for "tent state"`},
		{[]string{"delete", "clustr", "prod"}, `Error: unknown command "clustr" for "tent delete"`},
		{[]string{"create", "clustr", "prod"}, `Error: unknown command "clustr" for "tent create"`},
		{[]string{"get", "nodegrups", "prod"}, `Error: unknown command "nodegrups" for "tent get"`},
	} {
		wantError(t, runIn(t, "", append(tc.args, "--state", s.url)...), tc.want+"\n")
	}
	s.want(t, prodObjects)
}

// TestGroupCommandsShowHelp without a subcommand.
func TestGroupCommandsShowHelp(t *testing.T) {
	for _, name := range []string{"state", "delete", "create"} {
		got := runIn(t, "", name)
		if want := "Usage:\n  tent " + name + " [flags]\n  tent " + name + " [command]\n"; got.code != 0 ||
			got.errOut != "" || !strings.Contains(got.out, want) {
			t.Errorf("tent %s: exit code %d, stderr %q, stdout\n%s\nwant 0, nothing and the usage", name, got.code,
				got.errOut, got.out)
		}
	}
}

// TestNameAndNameFlag refuses a NAME and a --name that differ.
func TestNameAndNameFlag(t *testing.T) {
	s := withCluster(t)
	const differ = "Error: NAME prod and --name dev differ; give one\n"
	for _, args := range [][]string{
		{"get", "prod"},
		{"delete", "cluster", "prod", "--yes"},
		{"state", "unlock", "prod"},
		{"create", "cluster", "prod", "--provider", "vultr", "--region", "ams", "--machine-type", "x", "--dry-run"},
	} {
		wantError(t, runIn(t, "", append(args, "--name", "dev", "--state", s.url)...), differ)
	}
	s.want(t, prodObjects)

	wantOK(t, runIn(t, "", "get", "prod", "--name", "prod", "--state", s.url), prodYAML)
	t.Setenv(envCluster, "dev") // a default, which NAME overrides
	wantOK(t, runIn(t, "", "get", "prod", "--state", s.url), prodYAML)
}

func TestNoNameSaysHowToGiveOne(t *testing.T) {
	path := configFile(t)
	const how = "Error: no cluster name: give NAME or set --name, TENT_CLUSTER or \"cluster\" in "
	for _, args := range [][]string{{"get"}, {"delete", "cluster"}, {"state", "unlock"}} {
		wantError(t, runIn(t, "", append(args, "--state", newState(t).url)...), how+path+"\n")
	}
	// nodegroups takes no cluster NAME.
	wantError(t, runIn(t, "", "get", "nodegroups", "--state", newState(t).url),
		"Error: no cluster name: set --name, TENT_CLUSTER or \"cluster\" in "+path+"\n")
}
