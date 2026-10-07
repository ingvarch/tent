package e2e

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLastLines(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
		want string
	}{
		{"empty", "", 3, ""},
		{"fewer lines than asked", "a\nb\n", 3, "a\nb"},
		{"exactly as many lines as asked", "a\nb\nc\n", 3, "a\nb\nc"},
		{"more lines than asked", "a\nb\nc\nd\ne\n", 3, "c\nd\ne"},
		{"no final newline", "a\nb\nc\nd", 2, "c\nd"},
		{"blank lines at the end are dropped", "a\nb\n\n\n", 2, "a\nb"},
		{"a blank line inside counts", "a\n\nb\n", 2, "\nb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastLines(c.text, c.n); got != c.want {
				t.Errorf("lastLines(%q, %d) = %q, want %q", c.text, c.n, got, c.want)
			}
		})
	}
}

func TestTentFailureNamesTheStepTheCodeTheLogAndTheLastTwentyLinesOfStderr(t *testing.T) {
	var stderr strings.Builder
	for i := 1; i <= 25; i++ {
		stderr.WriteString("line " + string(rune('a'+i-1)) + "\n")
	}
	log := filepath.Join("results", "x1y2z3", "e2e-x1y2z3-2404-create.log")
	got := tentFailure("create", tentResult{Code: 3, Stderr: []byte(stderr.String())}, log)
	for _, want := range []string{"tent create exited with code 3", "log: " + log, "line y", "line f"} {
		if !strings.Contains(got, want) {
			t.Errorf("tentFailure = %q, want it to hold %q", got, want)
		}
	}
	if strings.Contains(got, "line e") {
		t.Errorf("tentFailure = %q, holds the 21st line from the end", got)
	}
}

func TestTentFailureSaysSoWhenAContextOrASignalEndedTent(t *testing.T) {
	got := tentFailure("validate", tentResult{Code: -1}, "l.log")
	if !strings.Contains(got, "tent validate was ended by a signal or by its context") {
		t.Errorf("tentFailure = %q", got)
	}
	if strings.Contains(got, "exited with code") {
		t.Errorf("tentFailure = %q, names a code that tent never gave", got)
	}
}

func TestTentFailureSaysWhenStderrIsEmpty(t *testing.T) {
	if got := tentFailure("delete", tentResult{Code: 1}, "l.log"); !strings.Contains(got, "stderr:\n(empty)") {
		t.Errorf("tentFailure = %q, want an empty stderr said", got)
	}
}

func TestLogPathIsWhereTheRunnerWritesTheLog(t *testing.T) {
	r := tentRunner{Dir: filepath.Join("results", "run")}
	want := filepath.Join("results", "run", "e2e-ab-2404-create.log")
	if got := r.logPath("e2e-ab-2404", "create"); got != want {
		t.Errorf("logPath = %q, want %q", got, want)
	}
}

func TestCreateArgs(t *testing.T) {
	cfg := settings{Region: "ams", Plan: "vc2-1c-1gb"}
	got := createArgs(cfg, "e2e-ab-2404", "ubuntu-24.04", "/k/id.pub", "198.51.100.7/32")
	want := []string{
		"create", "cluster", "e2e-ab-2404",
		"--provider", "vultr", "--region", "ams", "--machine-type", "vc2-1c-1gb",
		"--servers", "1", "--allow-single-server", "--workers", "1",
		"--image", "ubuntu-24.04", "--ssh-key", "/k/id.pub",
		"--ssh-access", "198.51.100.7/32", "--api-access", "198.51.100.7/32", "--yes",
	}
	if !slices.Equal(got, want) {
		t.Errorf("createArgs = %q, want %q", got, want)
	}
}

func TestDeleteCommandHoldsTheBinaryTheClusterAndTheStoreOfTheCluster(t *testing.T) {
	r := tentRunner{Bin: filepath.Join("run", "bin", "tent"), Dir: "run"}
	want := r.Bin + " delete cluster e2e-ab-2404 --yes --state " + stateURL(filepath.Join("run", "state-e2e-ab-2404"))
	if got := r.deleteCommand("e2e-ab-2404"); got != want {
		t.Errorf("deleteCommand = %q, want %q", got, want)
	}
}

func TestValidateArgsAcceptTheSingleServer(t *testing.T) {
	got := validateArgs("e2e-ab-2404")
	want := []string{"validate", "cluster", "e2e-ab-2404", "--wait", "5m", "--allow-single-server"}
	if !slices.Equal(got, want) {
		t.Errorf("validateArgs = %q, want %q", got, want)
	}
}

func TestDeleteInCleanup(t *testing.T) {
	cases := []struct {
		name                   string
		ran, done, interrupted bool
		want                   bool
	}{
		{"no delete ran", false, false, false, true},
		{"delete exited 0", true, true, false, false},
		{"delete failed on its own", true, false, false, false},
		{"a signal ended the delete", true, false, true, true},
		{"a signal after the delete exited 0", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deleteInCleanup(tc.ran, tc.done, tc.interrupted); got != tc.want {
				t.Fatalf("deleteInCleanup(%v, %v, %v) = %v, want %v", tc.ran, tc.done, tc.interrupted, got, tc.want)
			}
		})
	}
}
