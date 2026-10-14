package app_test

import (
	"testing"

	"github.com/ingvarch/tent/internal/paralleltest"
)

// serialTests lists the top-level tests that may run without t.Parallel(), each with the reason it cannot. The
// tests of this package build their own state, so the list is empty.
var serialTests = map[string]string{}

// TestEveryTestRunsInParallel checks that every top-level test of the package calls t.Parallel() first, except the
// ones that serialTests lists.
func TestEveryTestRunsInParallel(t *testing.T) {
	t.Parallel()
	paralleltest.Check(t, ".", serialTests, 700)
}
