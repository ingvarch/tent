package statestore_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withoutSyncUses returns the Go files below root, tests excluded, that use WithoutSync, as slash paths relative
// to root. It skips the directories .git, bin, dist and testdata.
func withoutSyncUses(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		if usesWithoutSync(parsed) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

// usesWithoutSync reports whether the file names WithoutSync other than to declare it: a call, qualified or not, or
// the function taken as a value, which a later call would hide.
func usesWithoutSync(f *ast.File) bool {
	declared := map[*ast.Ident]bool{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			declared[fn.Name] = true
		}
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "WithoutSync" && !declared[id] {
			found = true
		}
		return true
	})
	return found
}

// TestOnlyTestsUseWithoutSync checks that no Go file of the module outside the tests uses WithoutSync: tent itself
// never opens a store that skips its syncs.
func TestOnlyTestsUseWithoutSync(t *testing.T) {
	files, err := withoutSyncUses(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		t.Errorf("%s uses WithoutSync outside a test", f)
	}
}

// TestWithoutSyncUses checks what the guard reports on a tree of its own: a qualified call, a plain call, the
// function as a value, a mention that is no use, a test file, and the skipped directories.
func TestWithoutSyncUses(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a/qualified.go":        "package a\nvar _ = statestore.WithoutSync()\n",
		"b/plain.go":            "package b\nvar _ = WithoutSync()\n",
		"c/value.go":            "package c\nvar _ = statestore.WithoutSync\n",
		"c/mention.go":          "package c\n// WithoutSync()\nfunc WithoutSync() string { return \"WithoutSync\" }\n",
		"d/call_test.go":        "package d\nvar _ = statestore.WithoutSync()\n",
		"e/other.go":            "package e\nvar _ = statestore.Open()\n",
		"bin/skipped.go":        "package bin\nvar _ = statestore.WithoutSync()\n",
		"dist/skipped.go":       "package dist\nvar _ = statestore.WithoutSync()\n",
		"f/testdata/skipped.go": "package testdata\nvar _ = statestore.WithoutSync()\n",
		".git/skipped.go":       "package git\nvar _ = statestore.WithoutSync()\n",
	}
	for name, src := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := withoutSyncUses(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/qualified.go", "b/plain.go", "c/value.go"}
	if !slices.Equal(got, want) {
		t.Errorf("withoutSyncUses = %v, want %v", got, want)
	}
}
