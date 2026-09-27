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

// makeWith runs a Makefile target with only the given golangci-lint script on PATH ("" means none).
func makeWith(t *testing.T, target, linter string) (string, error) {
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
	if linter != "" {
		if err := os.WriteFile(filepath.Join(bin, "golangci-lint"), []byte(linter), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(maker, "-s", "-C", filepath.Join("..", ".."), target)
	cmd.Env = []string{"PATH=" + bin}
	out, err := cmd.CombinedOutput()
	return string(out), err
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
