package shellenv_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/shellenv"
	"github.com/ingvarch/tent/internal/shellenv/shellenvtest"
)

func TestExportLineQuotes(t *testing.T) {
	values := []string{"plain", "it's", `back\slash`, `trailing\`, `$HOME and $(id) and ` + "`id`", "a b\tc", `"double"`,
		`\'`, "semi;colon&amp"}
	for _, shell := range shellenvtest.Shells {
		t.Run(shell, func(t *testing.T) {
			for _, v := range values {
				script := shellenv.ExportLine(shell, "A_VAR", v) + "\n" + shellenv.ExportLine(shell, "B_VAR", "x") +
					"\n" + shellenvtest.Print(shell, "A_VAR", "B_VAR")
				if got := shellenvtest.Run(t, shell, script); got != v+"\nx\n" {
					t.Errorf("%s reads %q back as %q", shell, v, strings.TrimSuffix(got, "\nx\n"))
				}
			}
		})
	}
}

func TestExportLineForms(t *testing.T) {
	if got, want := shellenv.ExportLine(shellenv.Fish, "A", "it's"), `set -gx A 'it\'s'`; got != want {
		t.Errorf("fish line = %q, want %q", got, want)
	}
	if got, want := shellenv.ExportLine(shellenv.Sh, "A", "it's"), `export A='it'\''s'`; got != want {
		t.Errorf("sh line = %q, want %q", got, want)
	}
}

func TestLoginShell(t *testing.T) {
	for login, want := range map[string]string{
		"/opt/homebrew/bin/fish": shellenv.Fish,
		"/usr/bin/fish":          shellenv.Fish,
		"/bin/bash":              shellenv.Sh,
		"/bin/zsh":               shellenv.Sh,
		"":                       shellenv.Sh,
	} {
		if got := shellenv.Login(login); got != want {
			t.Errorf("Login(%q) = %q, want %q", login, got, want)
		}
	}
}

func TestValid(t *testing.T) {
	for shell, want := range map[string]bool{"fish": true, "sh": true, "bash": false, "": false} {
		if got := shellenv.Valid(shell); got != want {
			t.Errorf("Valid(%q) = %v, want %v", shell, got, want)
		}
	}
}

func TestFileLineReadsTheFile(t *testing.T) {
	for _, shell := range shellenvtest.Shells {
		t.Run(shell, func(t *testing.T) {
			for _, content := range []string{"s3cret-token", "s3cret-token\n", "s3cret token\n"} {
				dir := filepath.Join(t.TempDir(), "it's a dir")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "my token")
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				script := shellenv.FileLine(shell, "A_VAR", path) + "\n" + shellenvtest.Print(shell, "A_VAR")
				if got, want := shellenvtest.Run(t, shell, script), strings.TrimSuffix(content, "\n")+"\n"; got != want {
					t.Errorf("%s reads the file %q as %q, want %q", shell, content, got, want)
				}
			}
		})
	}
}

func TestFileLineForms(t *testing.T) {
	if got, want := shellenv.FileLine(shellenv.Fish, "A", "/a b"), `set -gx A (cat '/a b')`; got != want {
		t.Errorf("fish line = %q, want %q", got, want)
	}
	if got, want := shellenv.FileLine(shellenv.Sh, "A", "/a b"), `export A="$(cat '/a b')"`; got != want {
		t.Errorf("sh line = %q, want %q", got, want)
	}
}
