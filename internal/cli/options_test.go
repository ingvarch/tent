package cli

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fileValues returns the options that the config file can set.
func fileValues(o *globalOptions) fileConfig {
	return fileConfig{o.state, o.cluster, o.output, o.logFormat}
}

func TestPrecedence(t *testing.T) {
	const file = "state: file:///from-file\ncluster: from-file\noutput: yaml\nlogFormat: json\n"
	env := map[string]string{envState: "file:///from-env", envCluster: "from-env"}
	tests := []struct {
		name string
		file string
		env  map[string]string
		args []string
		want fileConfig
	}{
		{"built-in defaults", "", nil, nil, fileConfig{"", "", "table", "text"}},
		{"config file", file, nil, nil, fileConfig{"file:///from-file", "from-file", "yaml", "json"}},
		{"environment", "", env, nil, fileConfig{"file:///from-env", "from-env", "table", "text"}},
		{"environment over the file", file, env, nil, fileConfig{"file:///from-env", "from-env", "yaml", "json"}},
		{
			"empty variables are unset",
			file, map[string]string{envState: "", envCluster: ""}, nil,
			fileConfig{"file:///from-file", "from-file", "yaml", "json"},
		},
		{
			"flags over the environment and the file",
			file, env,
			[]string{"--state", "file:///from-flag", "--name", "from-flag", "-o", "json", "--log-format", "text"},
			fileConfig{"file:///from-flag", "from-flag", "json", "text"},
		},
		// A flag wins because it is on the command line, even with its default value.
		{
			"flags with their default values",
			file, nil, []string{"-o", "table", "--log-format", "text"},
			fileConfig{"file:///from-file", "from-file", "table", "text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.file != "" {
				writeConfig(t, tt.file)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, _, err := resolve(t, tt.args...)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := fileValues(opts); got != tt.want {
				t.Errorf("resolved %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestVerbosity(t *testing.T) {
	tests := []struct {
		args []string
		want slog.Level
	}{
		{nil, slog.LevelWarn},
		{[]string{"-v"}, slog.LevelInfo},
		{[]string{"--verbose"}, slog.LevelInfo},
		{[]string{"-vv"}, slog.LevelDebug},
		{[]string{"-v", "-v", "-v"}, slog.LevelDebug},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			opts, _, err := resolve(t, tt.args...)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if !opts.logger.Enabled(t.Context(), tt.want) || opts.logger.Enabled(t.Context(), tt.want-1) {
				t.Errorf("the logger does not log from level %s up", tt.want)
			}
		})
	}
}

func TestLogsAreTextOnStderr(t *testing.T) {
	opts, errOut, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	opts.logger.Warn("probe", "key", "value")
	if got := errOut.String(); !strings.HasSuffix(got, " level=WARN msg=probe key=value\n") {
		t.Errorf("stderr = %q, want a text log line", got)
	}
}

func TestLogFormatJSON(t *testing.T) {
	opts, errOut, err := resolve(t, "--log-format", "json")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	opts.logger.Warn("probe", "key", "<a & b>")
	var line map[string]any
	if err := json.Unmarshal(errOut.Bytes(), &line); err != nil || strings.Count(errOut.String(), "\n") != 1 {
		t.Fatalf("stderr is not one JSON line: %v\n%s", err, errOut)
	}
	if line["level"] != "WARN" || line["msg"] != "probe" || line["key"] != "<a & b>" {
		t.Errorf("log line = %v, want level WARN, msg probe and key <a & b>", line)
	}
}

func TestInvalidLogFormat(t *testing.T) {
	code, out, errOut := run(t, "version", "--log-format", "xml")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	want := "Error: invalid log format \"xml\": want text or json\n"
	if errOut != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}

func TestLockTimeout(t *testing.T) {
	tests := []struct {
		args []string
		want time.Duration
	}{
		{nil, 5 * time.Minute},
		{[]string{"--lock-timeout", "30s"}, 30 * time.Second},
		{[]string{"--lock-timeout", "1h30m"}, 90 * time.Minute},
		{[]string{"--lock-timeout", "0"}, 0},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			opts, _, err := resolve(t, tt.args...)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if opts.lockTimeout != tt.want {
				t.Errorf("lock timeout = %s, want %s", opts.lockTimeout, tt.want)
			}
		})
	}
}

func TestInvalidLockTimeout(t *testing.T) {
	tests := []struct {
		arg  string
		want string
	}{
		{
			"--lock-timeout=soon",
			`Error: invalid argument "soon" for "--lock-timeout" flag: time: invalid duration "soon"`,
		},
		{"--lock-timeout=-1s", "Error: invalid --lock-timeout -1s: must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			code, out, errOut := run(t, "version", tt.arg)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
			if errOut != tt.want+"\n" {
				t.Errorf("stderr = %q, want %q", errOut, tt.want+"\n")
			}
		})
	}
}

func TestRequireStateSaysHowToSetIt(t *testing.T) {
	path := configFile(t)
	opts, _, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, err = opts.requireState()
	want := `no state store: set --state, TENT_STATE or "state" in ` + path
	if err == nil || err.Error() != want {
		t.Errorf("requireState() error = %v, want %q", err, want)
	}
}

func TestRequireClusterSaysHowToSetIt(t *testing.T) {
	path := configFile(t)
	opts, _, err := resolve(t)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, err = opts.requireCluster()
	want := `no cluster name: set --name, TENT_CLUSTER or "cluster" in ` + path
	if err == nil || err.Error() != want {
		t.Errorf("requireCluster() error = %v, want %q", err, want)
	}
}

func TestRequireReturnsTheValues(t *testing.T) {
	opts, _, err := resolve(t, "--state", "file:///state", "--name", "prod")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, err := opts.requireState(); got != "file:///state" || err != nil {
		t.Errorf("requireState() = %q, %v, want file:///state", got, err)
	}
	if got, err := opts.requireCluster(); got != "prod" || err != nil {
		t.Errorf("requireCluster() = %q, %v, want prod", got, err)
	}
}
