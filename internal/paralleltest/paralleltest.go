// Package paralleltest checks that the tests of a package run in parallel. It is for tests only.
package paralleltest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
)

// T is the part of *testing.T that Check uses.
type T interface {
	Helper()
	Error(args ...any)
	Errorf(format string, args ...any)
}

// Check parses every *_test.go file of dir and reports to t each top-level test (a function whose name starts with
// Test and that takes one *testing.T) that does not call t.Parallel() first. Go runs serial tests one after another
// before it releases the parallel ones, so the tests in serial, each with the reason it cannot run in parallel, are
// the only exceptions. Check also reports a serial entry that has no test, that calls t.Parallel() first or that has
// no reason, and a package with fewer than minTests tests, which shows that the parser lost some.
func Check(t T, dir string, serial map[string]string, minTests int) {
	t.Helper()
	found, tests, err := problems(dir, serial)
	if err != nil {
		t.Error(err)
		return
	}
	for _, p := range found {
		t.Error(p)
	}
	if tests < minTests {
		t.Errorf("checked %d tests, want at least %d", tests, minTests)
	}
}

func problems(dir string, serial map[string]string) (found []string, tests int, err error) {
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
			_, listed := serial[name]
			parallel := startsParallel(fn)
			switch {
			case listed && parallel:
				found = append(found, fmt.Sprintf("%s calls t.Parallel() but is listed as serial", name))
			case !listed && !parallel:
				found = append(found, fmt.Sprintf("%s: %s does not call t.Parallel() first",
					filepath.Base(file), name))
			}
			seen[name] = true
		}
	}
	for name, reason := range serial {
		if !seen[name] {
			found = append(found, fmt.Sprintf("%s is listed as serial but no such test exists", name))
		}
		if reason == "" {
			found = append(found, fmt.Sprintf("%s is listed as serial without a reason", name))
		}
	}
	slices.Sort(found)
	return found, tests, nil
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
