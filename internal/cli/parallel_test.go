package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// serialTests lists the top-level tests that may run without t.Parallel(), each with the reason it cannot.
var serialTests = map[string]string{
	"TestNoArgsIgnoresProcessArgs":                  "sets os.Args, which the whole process shares",
	"TestExportNomadMakesRelativeDirectoryAbsolute": "changes the working directory, which the whole process shares",
	"TestExecuteDropsSIGQUITInTheEditor":            "sends SIGQUIT to the test binary, which every test would receive",
	"TestExecuteStopsOnARealCtrlC":                  "sends SIGINT to the test binary, which every test would receive",
}

// takesT reports whether fn has one parameter, of type *testing.T.
func takesT(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) != 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "T"
}

// startsParallel reports whether the first statement of fn is a call of t.Parallel().
func startsParallel(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) == 0 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == "t" && sel.Sel.Name == "Parallel"
}

// parallelProblems parses every *_test.go file of dir and returns the tests that must call t.Parallel() first but do
// not, the exceptions that name no test or a test that calls it, the exceptions without a reason, and the number of
// top-level tests it checked.
func parallelProblems(dir string, exceptions map[string]string) (problems []string, tests int, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			return nil, 0, err
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || !takesT(fn) {
				continue
			}
			tests++
			name := fn.Name.Name
			_, excepted := exceptions[name]
			parallel := startsParallel(fn)
			switch {
			case excepted && parallel:
				problems = append(problems, fmt.Sprintf("%s calls t.Parallel() but is listed as serial", name))
			case !excepted && !parallel:
				problems = append(problems, fmt.Sprintf("%s: %s does not call t.Parallel() first",
					filepath.Base(file), name))
			}
			seen[name] = true
		}
	}
	for name, reason := range exceptions {
		if !seen[name] {
			problems = append(problems, fmt.Sprintf("%s is listed as serial but no such test exists", name))
		}
		if reason == "" {
			problems = append(problems, fmt.Sprintf("%s is listed as serial without a reason", name))
		}
	}
	slices.Sort(problems)
	return problems, tests, nil
}

// TestEveryTestRunsInParallel checks that every top-level test of the package calls t.Parallel() first, except the
// ones that serialTests lists: Go runs serial tests one after another before it releases the parallel ones.
func TestEveryTestRunsInParallel(t *testing.T) {
	t.Parallel()
	problems, tests, err := parallelProblems(".", serialTests)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if tests < 340 {
		t.Errorf("checked %d tests, want at least 340", tests)
	}
}

// TestParallelProblems checks what the guard reports on a directory of its own: a serial test, a listed test that
// runs in parallel, a listed name without a test, a listed test without a reason, a test whose t.Parallel() is not
// first, TestMain and a method, which are not tests.
func TestParallelProblems(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := `package x

import "testing"

func TestGood(t *testing.T) { t.Parallel() }
func TestSerial(t *testing.T) {}
func TestLate(t *testing.T) { _ = 1; t.Parallel() }
func TestListed(t *testing.T) {}
func TestListedButParallel(t *testing.T) { t.Parallel() }
func TestNoReason(t *testing.T) {}
func helper(t *testing.T) {}
func TestMain(m *testing.M) {}

type s struct{}

func (s) TestMethod(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	exceptions := map[string]string{
		"TestListed": "why", "TestListedButParallel": "why", "TestGone": "why", "TestNoReason": "",
	}
	got, tests, err := parallelProblems(dir, exceptions)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"TestGone is listed as serial but no such test exists",
		"TestListedButParallel calls t.Parallel() but is listed as serial",
		"TestNoReason is listed as serial without a reason",
		"x_test.go: TestLate does not call t.Parallel() first",
		"x_test.go: TestSerial does not call t.Parallel() first",
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
	if tests != 6 {
		t.Errorf("tests = %d, want 6", tests)
	}
}
