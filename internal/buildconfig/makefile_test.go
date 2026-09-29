package buildconfig_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/assets"
)

// recipe returns the commands of a Makefile target, one per line, without the leading tab.
func recipe(t *testing.T, target string) []string {
	t.Helper()
	lines := strings.Split(string(repoFile(t, "Makefile")), "\n")
	for i, line := range lines {
		if line != target+":" && !strings.HasPrefix(line, target+": ") {
			continue
		}
		var commands []string
		for _, c := range lines[i+1:] {
			if !strings.HasPrefix(c, "\t") {
				break
			}
			commands = append(commands, strings.TrimPrefix(c, "\t"))
		}
		return commands
	}
	t.Fatalf("the Makefile has no target %q", target)
	return nil
}

// oneCommand returns the only command of a Makefile target.
func oneCommand(t *testing.T, target string) string {
	t.Helper()
	commands := recipe(t, target)
	if len(commands) != 1 {
		t.Fatalf("make %s runs %q, want one command", target, commands)
	}
	return commands[0]
}

// noticesFile is the third-party notices file make notices writes and the release ships.
const noticesFile = "THIRD_PARTY_NOTICES"

// makefileLintVersion is the golangci-lint version the Makefile insists on.
func makefileLintVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION := (\S+)$`).FindSubmatch(repoFile(t, "Makefile"))
	if m == nil {
		t.Fatal("the Makefile sets no GOLANGCI_LINT_VERSION")
	}
	return string(m[1])
}

// ciLintVersion is the golangci-lint version CI lints with, without the v.
func ciLintVersion(t *testing.T) string {
	t.Helper()
	lint := workflowJob(t, "ci.yml", "lint")
	return strings.TrimPrefix(stepUsing(t, lint, "golangci/golangci-lint-action").with("version"), "v")
}

// fakeLinter is a golangci-lint script that answers `version` with the shell command onVersion and echoes any other
// command it gets.
func fakeLinter(onVersion string) string {
	return "#!/bin/sh\ncase \"$1\" in\n  version) " + onVersion + " ;;\n  *) echo linted \"$@\" ;;\nesac\n"
}

// makeWith runs a Makefile target with make -s and only the given golangci-lint script on PATH ("" means none), and
// returns what make wrote to stdout and then to stderr.
func makeWith(t *testing.T, target, linter string) (string, error) {
	t.Helper()
	tools := map[string]string{}
	if linter != "" {
		tools["golangci-lint"] = linter
	}
	stdout, stderr, err := runMake(t, tools, "-s", target)
	return stdout + stderr, err
}

// runMake runs make with args in the repository with only the given tools on PATH, shell scripts by name, and
// returns what it wrote to stdout and to stderr.
func runMake(t *testing.T, tools map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Makefile runs under a POSIX shell")
	}
	maker, err := exec.LookPath("make")
	if err != nil {
		// On a CI runner a skip would drop these checks unnoticed.
		if os.Getenv("CI") != "" {
			t.Fatal("make is not installed")
		}
		t.Skip("make is not installed")
	}
	bin := t.TempDir()
	for name, script := range tools {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// In the repository rather than with -C: GNU make 4 prints "Entering directory" to stdout with -C, unless -s.
	cmd := exec.Command(maker, args...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = []string{"PATH=" + bin}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// linterRuns maps each Makefile target that calls golangci-lint to the command it runs.
var linterRuns = map[string]string{"lint": "run ./...", "fmt": "fmt ./..."}

func TestMakefileLintsWithTheVersionCIUses(t *testing.T) {
	// Another version finds other things than CI does.
	if got, want := makefileLintVersion(t), ciLintVersion(t); got != want {
		t.Errorf("the Makefile wants golangci-lint %s, CI lints with %s", got, want)
	}
	for target, run := range linterRuns {
		t.Run(target, func(t *testing.T) {
			// Only build needs git and date, so the linter is all that speaks.
			out, err := makeWith(t, target, fakeLinter("echo "+ciLintVersion(t)))
			if want := "linted " + run + "\n"; err != nil || out != want {
				t.Errorf("make %s with the CI version: err %v, output %q, want %q", target, err, out, want)
			}
		})
	}
}

func TestMakefileRefusesAnyOtherLinter(t *testing.T) {
	// A check that passes because the linter is missing checks nothing, and fmt must not rewrite the tree with a
	// version CI does not use.
	cases := []struct{ name, linter, says string }{
		{"missing", "", "golangci-lint is not installed"},
		{"another version", fakeLinter("echo 2.0.0"), "golangci-lint 2.0.0 is installed"},
		{"no version", fakeLinter("exit 1"), "golangci-lint an unknown version is installed"},
	}
	for _, c := range cases {
		for target := range linterRuns {
			t.Run(c.name+"/"+target, func(t *testing.T) {
				out, err := makeWith(t, target, c.linter)
				want := c.says + ": CI lints with v" + ciLintVersion(t)
				if err == nil || !strings.Contains(out, want) || strings.Contains(out, "linted") {
					t.Errorf("make %s: err %v, output:\n%s\nwant a failure that says %q and runs no linter", target, err, out, want)
				}
			})
		}
	}
}

func TestCIRunsTheTestsTheMakefileRuns(t *testing.T) {
	// CI cannot call make on Windows, so it runs the same command itself.
	want := recipe(t, "test")
	var got []string
	for _, s := range workflowJob(t, "ci.yml", "test").Steps {
		if s.Run != "" {
			got = append(got, s.Run)
		}
	}
	if len(want) != 1 || len(got) != 1 || got[0] != want[0] {
		t.Errorf("CI runs %q, the Makefile runs %q", got, want)
	}
}

// makeVariable returns the value of a variable the Makefile sets with = or :=, its continued lines joined by spaces.
func makeVariable(t *testing.T, name string) string {
	t.Helper()
	lines := strings.Split(string(repoFile(t, "Makefile")), "\n")
	assign := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `\s*:?=\s*(.*)$`)
	for i, line := range lines {
		m := assign.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := m[1]
		for rest := lines[i+1:]; strings.HasSuffix(value, `\`) && len(rest) > 0; rest = rest[1:] {
			value = strings.TrimSuffix(value, `\`) + " " + rest[0]
		}
		return strings.Join(strings.Fields(value), " ")
	}
	t.Fatalf("the Makefile does not set %s", name)
	return ""
}

// stamped returns the variables that -X flags set, such as github.com/ingvarch/tent/internal/buildinfo.version.
func stamped(ldflags string) []string {
	var vars []string
	for _, m := range regexp.MustCompile(`-X\s+([^=\s]+)=`).FindAllStringSubmatch(ldflags, -1) {
		vars = append(vars, m[1])
	}
	return vars
}

func TestMakefileStampsWhatTheReleaseStamps(t *testing.T) {
	// A development build reports its version, commit and date as a release does.
	module := makeVariable(t, "MODULE")
	got := stamped(strings.ReplaceAll(makeVariable(t, "LDFLAGS"), "$(MODULE)", module))
	want := stamped(strings.Join(byID(t, "builds", release(t).Builds, "tent").Ldflags, " "))
	if len(want) == 0 {
		t.Fatal("the release stamps no variables")
	}
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("variables the Makefile stamps (-release +Makefile):\n%s", diff)
	}
}

func TestMakeBuildBuildsTentNodeLikeTent(t *testing.T) {
	// A development build's nodes run the tent-node it builds, which must carry the same version as its tent.
	if got, want := makeVariable(t, "GOBUILD"), `go build -trimpath -ldflags "$(LDFLAGS)"`; got != want {
		t.Errorf("the Makefile sets GOBUILD = %s, want %s", got, want)
	}
	want := []string{
		"$(GOBUILD) -o bin/tent ./cmd/tent",
		"GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GOBUILD) -o bin/" + assets.TentNodeFile("amd64") + " ./cmd/tent-node",
	}
	if diff := cmp.Diff(want, recipe(t, "build")); diff != "" {
		t.Errorf("make build (-want +got):\n%s", diff)
	}
}

func TestMakeDevUploadUploadsWhatBuildWrites(t *testing.T) {
	// make dev-upload builds, with its output on stderr, then uploads the tent-node that make build writes, for the
	// nodes of that build.
	want := []string{
		"@$(MAKE) --no-print-directory build >&2",
		"@go run ./hack/tent-node-upload -binary bin/" + assets.TentNodeFile("amd64") + " -arch amd64",
	}
	if diff := cmp.Diff(want, recipe(t, "dev-upload")); diff != "" {
		t.Errorf("make dev-upload (-want +got):\n%s", diff)
	}
	phony := regexp.MustCompile(`(?m)^\.PHONY: (.+)$`).FindSubmatch(repoFile(t, "Makefile"))
	if phony == nil || !slices.Contains(strings.Fields(string(phony[1])), "dev-upload") {
		t.Error("the Makefile does not list dev-upload in .PHONY")
	}
}

func TestMakeDevUploadPrintsOnlyTheToolsLines(t *testing.T) {
	// make dev-upload | source runs what reaches stdout: with or without -s, only the tool's lines get there.
	fakeGo := "#!/bin/sh\ncase \"$1\" in\n  build) echo \"built $*\" ;;\n  run) echo \"ran $*\" ;;\nesac\n"
	for _, flags := range [][]string{nil, {"-s"}} {
		stdout, stderr, err := runMake(t, map[string]string{"go": fakeGo}, append(flags, "dev-upload")...)
		want := "ran run ./hack/tent-node-upload -binary bin/" + assets.TentNodeFile("amd64") + " -arch amd64\n"
		if err != nil || stdout != want {
			t.Errorf("make %q dev-upload: err %v, stdout %q, want %q", flags, err, stdout, want)
		}
		if !strings.Contains(stderr, "built build") {
			t.Errorf("make %q dev-upload: stderr lacks the build's output:\n%s", flags, stderr)
		}
	}
}

func TestMakeCleanRemovesWhatTheBuildWrites(t *testing.T) {
	removed := strings.Fields(oneCommand(t, "clean"))
	for _, generated := range []string{"bin", "dist", noticesFile} {
		if !slices.Contains(removed, generated) {
			t.Errorf("make clean runs %q, which leaves %s", strings.Join(removed, " "), generated)
		}
	}
}

func TestMakefileCheckRunsEveryGate(t *testing.T) {
	m := regexp.MustCompile(`(?m)^check: (.+)$`).FindSubmatch(repoFile(t, "Makefile"))
	if m == nil {
		t.Fatal("the Makefile has no check target")
	}
	if got := string(m[1]); got != "fmt lint licenses test build" {
		t.Errorf("make check runs %q, want \"fmt lint licenses test build\"", got)
	}
	// Plain make runs check.
	if !regexp.MustCompile(`(?m)^\.DEFAULT_GOAL := check$`).Match(repoFile(t, "Makefile")) {
		t.Error("the Makefile does not set .DEFAULT_GOAL := check")
	}
}
