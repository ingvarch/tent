package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFileReads(t *testing.T) {
	defaults := fileConfig{"", "", "table", "text"}
	tests := []struct {
		name string
		file string
		want fileConfig
	}{
		{"empty", "", defaults},
		{"comments only", "# nothing yet\n", defaults},
		{"empty document", "---\n", defaults},
		{"document marker", "---\ncluster: prod\n", fileConfig{"", "prod", "table", "text"}},
		{"quoted string", "cluster: \"42\"\n", fileConfig{"", "42", "table", "text"}},
		{
			"alias", "state: &s file:///state\ncluster: *s\n",
			fileConfig{"file:///state", "file:///state", "table", "text"},
		},
		{"empty string", "state: \"\"\n", defaults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeConfig(t, tt.file)
			opts, _, err := resolve(t)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := fileValues(opts); got != tt.want {
				t.Errorf("resolved %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigFileErrors(t *testing.T) {
	const keys = "want state, cluster, output or logFormat"
	tests := []struct {
		name string
		file string
		want string
	}{
		{"unknown key", "state: file:///state\nstat: x\n", `line 2: unknown key "stat": ` + keys},
		{"key in the wrong case", "State: file:///state\n", `line 1: unknown key "State": ` + keys},
		{"invalid YAML", "state: x\n  cluster: y\n", "yaml: line 2: mapping values are not allowed in this context"},
		{"duplicate key", "state: a\nstate: b\n", `line 2: duplicate key "state"`},
		{"not a mapping", "- state\n", "line 1: not a mapping"},
		{"list value", "cluster: [a]\n", "line 1: cluster: must be a string"},
		{"number value", "cluster: 42\n", "line 1: cluster: must be a string"},
		{"no value", "state:\n", "line 1: state: must be a string"},
		{"invalid output", "output: xml\n", `line 1: invalid output format "xml": want table, yaml or json`},
		{"invalid log format", "logFormat: xml\n", `line 1: invalid log format "xml": want text or json`},
		{"second document", "state: a\n---\nfoo: 1\n", "line 2: more than one document"},
		{"empty second document", "state: a\n---\n", "line 2: more than one document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.file)
			code, out, errOut := run(t, "version")
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
			want := "Error: " + path + ": " + tt.want + "\n"
			if errOut != want {
				t.Errorf("stderr\n got %q\nwant %q", errOut, want)
			}
		})
	}
}

func TestUnreadableConfigFile(t *testing.T) {
	path := configFile(t)
	if err := os.MkdirAll(path, 0o755); err != nil { // a directory where the file belongs
		t.Fatal(err)
	}
	code, _, errOut := run(t, "version")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(errOut, "Error: reading the config file: ") || !strings.Contains(errOut, path) {
		t.Errorf("stderr = %q, want an error that names %s", errOut, path)
	}
}

// setHome sets the home directory, on every operating system, and XDG_CONFIG_HOME.
func setHome(t *testing.T, home, xdgConfigHome string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // the home directory on Windows
}

func TestNoHomeMeansNoConfigFile(t *testing.T) {
	setHome(t, "", "")
	code, out, errOut := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, errOut)
	}
	if out == "" || errOut != "" {
		t.Errorf("stdout = %q, stderr = %q, want the version and no error", out, errOut)
	}
}

func TestNoHomeIsLoggedAtDebug(t *testing.T) {
	setHome(t, "", "")
	_, errOut, err := resolve(t, "-vv")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := errOut.String(); !strings.Contains(got, ` level=DEBUG msg="no config file" reason=`) ||
		!strings.Contains(got, " is not defined") {
		t.Errorf("stderr = %q, want a debug line that says why there is no config file", got)
	}
}

func TestRequireWithoutAConfigFile(t *testing.T) {
	setHome(t, "", "")
	opts, _, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := opts.requireState(); err == nil || err.Error() != "no state store: set --state or TENT_STATE" {
		t.Errorf("requireState() error = %v, want the flag and the variable only", err)
	}
	if _, err := opts.requireCluster(); err == nil || err.Error() != "no cluster name: set --name or TENT_CLUSTER" {
		t.Errorf("requireCluster() error = %v, want the flag and the variable only", err)
	}
}

func TestConfigFileUnderTheHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home, "")
	opts, _, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, err = opts.requireState()
	path := filepath.Join(home, ".config", "tent", "config.yaml")
	want := `no state store: set --state, TENT_STATE or "state" in ` + path
	if err == nil || err.Error() != want {
		t.Errorf("requireState() error = %v, want %q", err, want)
	}
}

func TestRelativeXDGConfigHomeIsIgnored(t *testing.T) {
	home := t.TempDir()
	setHome(t, home, filepath.Join("relative", "config"))
	dir := filepath.Join(home, ".config", "tent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("cluster: from-home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, _, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if opts.cluster != "from-home" {
		t.Errorf("cluster = %q, want from-home from the config file under the home directory", opts.cluster)
	}
}
