package buildconfig_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/licenses"
)

// listEntry returns an entry of a .goreleaser.yaml list that is either text or a map, such as a hook (cmd) or an
// archive file (src).
func listEntry(v any, key string) string {
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		s, _ := v[key].(string)
		return s
	}
	return ""
}

func TestMakeNoticesChecksWhatMakeLicensesChecks(t *testing.T) {
	// Both run the same check on the same packages; notices also writes the file the release ships.
	notices, check := oneCommand(t, "notices"), oneCommand(t, "licenses")
	if !strings.Contains(notices, " -notices "+noticesFile+" ") {
		t.Fatalf("make notices runs %q, which does not write %s", notices, noticesFile)
	}
	if got := strings.Replace(notices, " -notices "+noticesFile, "", 1); got != check {
		t.Errorf("make notices runs %q, make licenses runs %q: they check different things", notices, check)
	}
}

func TestReleaseWritesTheNoticesBeforeItBuilds(t *testing.T) {
	// A before hook runs in the snapshot too, so every pull request writes the notices and checks the licences.
	want := oneCommand(t, "notices")
	hooks := release(t).Before.Hooks
	if !slices.ContainsFunc(hooks, func(h any) bool { return listEntry(h, "cmd") == want }) {
		t.Errorf(".goreleaser.yaml before hooks %v do not run %q, as make notices does", hooks, want)
	}
}

func TestReleaseArchivesShipTheLicenceAndTheNotices(t *testing.T) {
	archives := release(t).Archives
	if len(archives) == 0 {
		t.Fatal(".goreleaser.yaml has no archives")
	}
	for i, a := range archives {
		for _, file := range []string{"LICENSE", noticesFile} {
			if !slices.ContainsFunc(a.Files, func(f any) bool { return listEntry(f, "src") == file }) {
				t.Errorf("archive %d does not ship %s: files %v", i, file, a.Files)
			}
		}
	}
}

func TestCIChecksTheLicencesLikeMakeLicenses(t *testing.T) {
	want := oneCommand(t, "licenses")
	lint := workflowJob(t, "ci.yml", "lint")
	i := slices.IndexFunc(lint.Steps, func(s step) bool { return s.Run == want })
	if i < 0 {
		t.Fatalf("%s does not run %q, as make licenses does", lint.where(), want)
	}
	// A failed step before it, such as the linter, must not hide a licence failure; the job still fails.
	if got := lint.Steps[i].If; got != "success() || failure()" {
		t.Errorf("%s checks the licences with if %q, want \"success() || failure()\"", lint.where(), got)
	}
}

func TestLicenceCheckCoversEveryReleasePlatform(t *testing.T) {
	// A module linked on only one platform, such as cobra's mousetrap on Windows, still ships in that archive.
	var want []string
	for _, b := range release(t).Builds {
		for _, target := range b.Targets {
			parts := strings.SplitN(target, "_", 3)
			if len(parts) < 2 {
				t.Fatalf("target %q is not goos_goarch", target)
			}
			want = append(want, parts[0]+"/"+parts[1])
		}
	}
	if diff := cmp.Diff(want, licenses.Platforms, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("licenses.Platforms (-release targets +checked):\n%s", diff)
	}
}

func TestGitIgnoresTheNotices(t *testing.T) {
	// The release generates it; a tracked copy would go stale, and an untracked one would make the tree dirty.
	for line := range strings.Lines(string(repoFile(t, ".gitignore"))) {
		if strings.TrimSpace(line) == "/"+noticesFile {
			return
		}
	}
	t.Errorf(".gitignore does not ignore /%s", noticesFile)
}
