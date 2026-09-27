package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// TestMain points every test at an empty config directory and clears the variables tent reads, so the user's own
// settings never reach a test. A test that needs a config file or a variable sets its own with t.Setenv.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	dir, err := os.MkdirTemp("", "tent-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for _, err := range []error{
		os.Setenv("XDG_CONFIG_HOME", dir),
		os.Unsetenv(envState),
		os.Unsetenv(envCluster),
	} {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}

// configFile gives the test its own config directory and returns the path of the config file, which does not exist.
func configFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "tent", "config.yaml")
}

// writeConfig gives the test its own config directory with a config file that holds content, and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := configFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolve runs tent with args and a command that does nothing, and returns the options the root resolved for it
// and what tent wrote to stderr.
func resolve(t *testing.T, args ...string) (*globalOptions, *bytes.Buffer, error) {
	t.Helper()
	var errOut bytes.Buffer
	opts := &globalOptions{}
	root := newRootCommand(Streams{Out: io.Discard, Err: &errOut}, opts)
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }})
	root.SetArgs(append([]string{"probe"}, args...))
	return opts, &errOut, root.Execute()
}

// run executes tent with args and returns the exit code, stdout and stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := executeTest(t.Context(), t, args, Streams{Out: &out, Err: &errOut})
	return code, out.String(), errOut.String()
}

func TestInvalidOutputFormat(t *testing.T) {
	code, out, errOut := run(t, "version", "-o", "xml")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	want := "Error: invalid output format \"xml\": want table, yaml or json\n"
	if errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}

func TestExecuteWritesErrors(t *testing.T) {
	invalid := v1alpha1.Errors{
		{Object: "Cluster prod", Path: "spec.cloud.region", Detail: "required"},
		{Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server"},
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"one line", errors.New("broken"), "Error: broken\n"},
		{"joined", errors.Join(errors.New("first"), errors.New("second")), "Error: first\n  second\n"},
		{"invalid spec", invalid, "Error: invalid spec:\n" +
			"  Cluster prod: spec.cloud.region: required\n" +
			"  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n"},
		{"wrapped invalid spec", fmt.Errorf("stopped: %w", invalid),
			"Error: stopped: Cluster prod: spec.cloud.region: required\n" +
				"  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			root := newRootCommand(Streams{Out: io.Discard, Err: io.Discard}, &globalOptions{})
			root.AddCommand(&cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return tc.err }})
			if code := execute(t.Context(), root, []string{"probe"}, &errOut); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if got := errOut.String(); got != tc.want {
				t.Errorf("stderr\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// A suggestion from cobra keeps its blank line and its indented command.
func TestUnknownCommandSuggests(t *testing.T) {
	code, out, errOut := run(t, "creat")
	want := "Error: unknown command \"creat\" for \"tent\"\n\n  Did you mean this?\n\tcreate\n"
	if code != 1 || out != "" || errOut != want {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 1, nothing and %q", code, out, errOut, want)
	}
}

func TestExecutePassesTheContext(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "value")
	var got any
	root := newRootCommand(Streams{Out: io.Discard, Err: io.Discard}, &globalOptions{})
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		got = cmd.Context().Value(key{})
		return nil
	}})
	if code := execute(ctx, root, []string{"probe"}, io.Discard); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got != "value" {
		t.Errorf("the command's context holds %v, want the context given to execute", got)
	}
}

// Only the root resolves the options: cobra runs just the nearest PersistentPreRun(E), so a subcommand with its own
// would run without a config file and a logger.
func TestOnlyTheRootHasPersistentHooks(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			if c.PersistentPreRun != nil || c.PersistentPreRunE != nil {
				t.Errorf("%q sets PersistentPreRun or PersistentPreRunE", c.CommandPath())
			}
			walk(c)
		}
	}
	walk(newRootCommand(Streams{Out: io.Discard, Err: io.Discard}, &globalOptions{}))
}

func TestInvalidOutputFormatStopsTheCommand(t *testing.T) {
	_, _, err := resolve(t, "-o", "xml")
	want := `invalid output format "xml": want table, yaml or json`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// cobra's own commands need no settings, so a broken config file does not break them.
func TestCobraCommandsIgnoreTheConfigFile(t *testing.T) {
	for _, args := range [][]string{
		{"help"},
		{"help", "version"},
		{"completion", "bash"},
		{cobra.ShellCompRequestCmd, ""},
		{cobra.ShellCompNoDescRequestCmd, ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			writeConfig(t, "stat: x\n")
			code, out, errOut := run(t, args...)
			if code != 0 || out == "" {
				t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 0 and output", code, out, errOut)
			}
		})
	}
}

func TestCommandsReportABrokenConfigFile(t *testing.T) {
	path := writeConfig(t, "stat: x\n")
	code, _, errOut := run(t, "version")
	want := "Error: " + path + `: line 1: unknown key "stat": want state, cluster, output or logFormat` + "\n"
	if code != 1 || errOut != want {
		t.Errorf("exit code = %d, stderr = %q; want 1 and %q", code, errOut, want)
	}
}

func TestGlobalFlagsHelp(t *testing.T) {
	code, out, errOut := run(t, "version", "--help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	want := `Global Flags:
      --lock-timeout duration   how long to wait for the cluster lock (default 5m0s)
      --log-format string       log format: text or json (default "text")
      --name string             cluster name (env TENT_CLUSTER)
  -o, --output string           output format: table, yaml or json (default "table")
      --state string            state store URL: file:///abs/path or s3://bucket[/prefix] (env TENT_STATE)
  -v, --verbose count           log more: -v for info, -vv for debug
`
	if !strings.HasSuffix(out, want) {
		t.Errorf("help\n%s\nwant it to end with\n%s", out, want)
	}
}

func TestNoArgsIgnoresProcessArgs(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{"tent", "frobnicate"} // what cobra would read if Execute passed nil through

	code, out, errOut := run(t)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("stdout = %q, want the usage text", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := run(t, "frobnicate")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want it to name the unknown command", errOut)
	}
}
