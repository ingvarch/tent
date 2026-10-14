package cli

import (
	"testing"

	"github.com/ingvarch/tent/internal/paralleltest"
)

// serialTests lists the top-level tests that may run without t.Parallel(), each with the reason it cannot.
var serialTests = map[string]string{
	"TestNoArgsIgnoresProcessArgs":                  "sets os.Args, which the whole process shares",
	"TestExportNomadMakesRelativeDirectoryAbsolute": "changes the working directory, which the whole process shares",
	"TestExecuteDropsSIGQUITInTheEditor":            "sends SIGQUIT to the test binary, which every test would receive",
	"TestExecuteStopsOnARealCtrlC":                  "sends SIGINT to the test binary, which every test would receive",
}

// TestEveryTestRunsInParallel checks that every top-level test of the package calls t.Parallel() first, except the
// ones that serialTests lists.
func TestEveryTestRunsInParallel(t *testing.T) {
	t.Parallel()
	paralleltest.Check(t, ".", serialTests, 340)
}
