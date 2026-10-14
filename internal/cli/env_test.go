package cli

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// TestEnvironmentIsTheProcessByDefault reads the process's variables and home directory when a test sets no seam.
func TestEnvironmentIsTheProcessByDefault(t *testing.T) {
	t.Parallel()
	o := &globalOptions{}
	if got, want := o.env("PATH"), os.Getenv("PATH"); got != want {
		t.Errorf("env(PATH) = %q, want %q", got, want)
	}
	got, err := o.home()
	want, wantErr := os.UserHomeDir()
	if got != want || (err == nil) != (wantErr == nil) {
		t.Errorf("home() = %q, %v, want %q, %v", got, err, want, wantErr)
	}
}

// TestEnvironmentSeam reads the variables and the home directory that the options hold instead of the process's.
func TestEnvironmentSeam(t *testing.T) {
	t.Parallel()
	errNoHome := errors.New("no home")
	o := &globalOptions{
		getenv:  func(key string) string { return "value of " + key },
		homeDir: func() (string, error) { return "", errNoHome },
	}
	if got := o.env("PATH"); got != "value of PATH" {
		t.Errorf("env(PATH) = %q, want the seam's value", got)
	}
	if _, err := o.home(); !errors.Is(err, errNoHome) {
		t.Errorf("home() error = %v, want the seam's error", err)
	}
}

// TestSetenvScopesToTheTestAndItsSubtests sets a variable for the commands of a test, which its subtests see, and
// which neither a sibling test nor the process sees.
func TestSetenvScopesToTheTestAndItsSubtests(t *testing.T) {
	t.Parallel()
	setenv(t, "TENT_PROBE", "parent")
	t.Run("sees the parent", func(t *testing.T) {
		o := &globalOptions{}
		testOptions(t)(o)
		if got := o.env("TENT_PROBE"); got != "parent" {
			t.Errorf("env(TENT_PROBE) = %q, want parent", got)
		}
	})
	t.Run("overrides it for itself only", func(t *testing.T) {
		setenv(t, "TENT_PROBE", "child")
		o := &globalOptions{}
		testOptions(t)(o)
		if got := o.env("TENT_PROBE"); got != "child" {
			t.Errorf("env(TENT_PROBE) = %q, want child", got)
		}
	})
	t.Run("sees the parent again", func(t *testing.T) {
		o := &globalOptions{}
		testOptions(t)(o)
		if got := o.env("TENT_PROBE"); got != "parent" {
			t.Errorf("env(TENT_PROBE) = %q, want parent", got)
		}
	})
	o := &globalOptions{}
	testOptions(t)(o)
	if got := o.env("TENT_PROBE"); got != "parent" {
		t.Errorf("env(TENT_PROBE) = %q, want parent", got)
	}
}

// TestSetenvIsForgottenWhenTheTestEnds leaves nothing behind for a test that has the name of one that ended.
func TestSetenvIsForgottenWhenTheTestEnds(t *testing.T) {
	t.Parallel()
	var name string
	t.Run("sets", func(t *testing.T) {
		name = t.Name()
		setenv(t, "TENT_PROBE", "set")
	})
	if e := ancestorEnv(name); e != nil {
		t.Errorf("the environment of %s outlives the test: %v", name, e.vars)
	}
}

// TestDefaultTestEnvironment holds the empty config directory only, and has no home.
func TestDefaultTestEnvironment(t *testing.T) {
	t.Parallel()
	o := &globalOptions{}
	testOptions(t)(o)
	if got := o.env("XDG_CONFIG_HOME"); got != emptyConfigDir || got == "" {
		t.Errorf("XDG_CONFIG_HOME = %q, want the shared empty directory %q", got, emptyConfigDir)
	}
	for _, key := range []string{envState, envCluster, "HOME", "SHELL", "XDG_CACHE_HOME", "TENT_EDITOR"} {
		if got := o.env(key); got != "" {
			t.Errorf("%s = %q, want it unset", key, got)
		}
	}
	if home, err := o.home(); !errors.Is(err, errNoHome) {
		t.Errorf("home() = %q, %v, want %v", home, err, errNoHome)
	}
}

// TestTestOptionsKeepWhatOptionsSet leaves the store and the environment that another option has set.
func TestTestOptionsKeepWhatOptionsSet(t *testing.T) {
	t.Parallel()
	opened := false
	o := &globalOptions{
		getenv:    func(string) string { return "mine" },
		tempDir:   "/mine",
		openStore: func(context.Context, string) (statestore.Store, error) { opened = true; return nil, nil },
	}
	testOptions(t)(o)
	if got := o.env("HOME"); got != "mine" || o.tempDir != "/mine" {
		t.Errorf("env(HOME) = %q, tempDir = %q, want the values that were set", got, o.tempDir)
	}
	if _, err := o.openStore(t.Context(), ""); err != nil || !opened {
		t.Errorf("openStore = %v, opened %v, want the store that was set", err, opened)
	}
}
