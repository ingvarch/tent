package paralleltest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/paralleltest"
)

const source = `package x

import "testing"

func TestGood(t *testing.T) { t.Parallel() }
func TestSerial(t *testing.T) {}
func TestLate(t *testing.T) { _ = 1; t.Parallel() }
func TestHelperFirst(t *testing.T) { t.Helper(); t.Parallel() }
func TestOtherReceiver(t *testing.T) { u.Parallel() }
func TestListed(t *testing.T) {}
func TestListedButParallel(t *testing.T) { t.Parallel() }
func TestNoReason(t *testing.T) {}
func helper(t *testing.T) {}
func TestMain(m *testing.M) {}
func TestNoParams() {}
func TestOther(t *other.T) {}

type s struct{}

func (s) TestMethod(t *testing.T) {}
`

func writeTests(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fakeT struct{ errors []string }

func (*fakeT) Helper()             {}
func (f *fakeT) Error(args ...any) { f.errors = append(f.errors, fmt.Sprint(args...)) }
func (f *fakeT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func check(t *testing.T, dir string, serial map[string]string, minTests int) []string {
	t.Helper()
	f := &fakeT{}
	paralleltest.Check(f, dir, serial, minTests)
	return f.errors
}

// TestCheckReportsEachRule checks every rule on one package: a serial test, a test whose t.Parallel() is not
// first, a test that calls another method of t first, a test that calls Parallel on another value, a listed test
// that runs in parallel, a listed name without a test and a listed test without a reason; and that TestMain, a
// method, a test without parameters and a test of another type are not counted (8 tests are).
func TestCheckReportsEachRule(t *testing.T) {
	t.Parallel()
	dir := writeTests(t, source)
	serial := map[string]string{
		"TestListed": "why", "TestListedButParallel": "why", "TestGone": "why", "TestNoReason": "",
	}
	want := []string{
		"TestGone is listed as serial but no such test exists",
		"TestListedButParallel calls t.Parallel() but is listed as serial",
		"TestNoReason is listed as serial without a reason",
		"x_test.go: TestHelperFirst does not call t.Parallel() first",
		"x_test.go: TestLate does not call t.Parallel() first",
		"x_test.go: TestOtherReceiver does not call t.Parallel() first",
		"x_test.go: TestSerial does not call t.Parallel() first",
	}
	if got := check(t, dir, serial, 8); !slices.Equal(got, want) {
		t.Errorf("errors = %q, want %q", got, want)
	}
	got := check(t, dir, serial, 9)
	if want := append(slices.Clone(want), "checked 8 tests, want at least 9"); !slices.Equal(got, want) {
		t.Errorf("errors = %q, want %q", got, want)
	}
}

// TestCheckReadsEveryTestFile checks that tests in several files are all counted and that other files are not.
func TestCheckReadsEveryTestFile(t *testing.T) {
	t.Parallel()
	dir := writeTests(t, "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Parallel() }\n")
	other := "package x\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "y_test.go"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	notTest := "package x\n\nfunc TestC(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "z.go"), []byte(notTest), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"y_test.go: TestB does not call t.Parallel() first"}
	if got := check(t, dir, nil, 2); !slices.Equal(got, want) {
		t.Errorf("errors = %q, want %q", got, want)
	}
}

// TestCheckAcceptsAPackageThatMeetsEveryRule checks that Check is silent at exactly the minimum.
func TestCheckAcceptsAPackageThatMeetsEveryRule(t *testing.T) {
	t.Parallel()
	dir := writeTests(t, "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Parallel() }\n")
	if got := check(t, dir, nil, 1); len(got) != 0 {
		t.Errorf("errors = %q, want none", got)
	}
}

// TestCheckReportsAParseError checks that a file that does not parse is reported once and that no count follows.
func TestCheckReportsAParseError(t *testing.T) {
	t.Parallel()
	dir := writeTests(t, "package x\n\nfunc (")
	if got := check(t, dir, nil, 1); len(got) != 1 || !strings.Contains(got[0], "x_test.go") {
		t.Errorf("errors = %q, want one that names x_test.go", got)
	}
}
