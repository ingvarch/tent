// Package shellenvtest runs the lines that shellenv writes in a real shell, for tests.
package shellenvtest

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Shells are the shells that shellenv writes for.
var Shells = []string{"sh", "fish"}

// Print returns the command that prints the values of the variables, one per line, in the shell.
func Print(shell string, names ...string) string {
	if shell == "fish" {
		return `printf '%s\n' $` + strings.Join(names, " $")
	}
	return `printf '%s\n' "$` + strings.Join(names, `" "$`) + `"`
}

// Run runs script in the shell with an empty environment and returns its output. It skips the test when the shell is
// not installed or the platform is Windows.
func Run(t *testing.T, shell, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the lines are for POSIX shells and fish")
	}
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s is not installed", shell)
	}
	cmd := exec.Command(path, "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v, output:\n%s", shell, err, out)
	}
	return string(out)
}
