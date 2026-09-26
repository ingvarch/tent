package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// run executes tent with args and returns the exit code, stdout and stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(args, Streams{Out: &out, Err: &errOut})
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
