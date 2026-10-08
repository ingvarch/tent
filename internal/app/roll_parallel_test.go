package app_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rollTestFiles lists the test files of the rolling update, which run their tests in parallel: each test builds its
// own world, and none changes state that the package shares.
func rollTestFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("roll*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	return append(files, "world_test.go", "nodekit_internal_test.go")
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

// TestRollTestsRunInParallel checks that every top-level test of the rolling update's test files calls t.Parallel
// first: the roll's tests take most of the package's time, and they run one after another otherwise.
func TestRollTestsRunInParallel(t *testing.T) {
	t.Parallel()
	files := rollTestFiles(t)
	if len(files) < 3 {
		t.Fatalf("found only %v", files)
	}
	var tests int
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			tests++
			if !startsParallel(fn) {
				t.Errorf("%s: %s does not call t.Parallel() first", file, fn.Name.Name)
			}
		}
	}
	if tests < 120 {
		t.Errorf("checked %d tests, want at least 120", tests)
	}
	if !slices.Contains(files, "roll_parallel_test.go") {
		t.Errorf("the files %v leave out this one", files)
	}
	if !slices.Contains(files, "roll_test.go") {
		t.Errorf("the files %v leave out roll_test.go", files)
	}
}
