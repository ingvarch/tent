// Package shellenv writes the lines that set environment variables in fish or sh, for the hack tools whose output a
// shell runs.
package shellenv

import (
	"path"
	"strings"
)

// The shells that the tools write for.
const (
	Sh   = "sh"
	Fish = "fish"
)

// Valid reports whether shell is one that ExportLine writes for.
func Valid(shell string) bool { return shell == Sh || shell == Fish }

// Login returns fish when login, the value of $SHELL, names fish, and sh otherwise.
func Login(login string) string {
	if path.Base(login) == Fish {
		return Fish
	}
	return Sh
}

// Quote returns s in single quotes for the shell, so that the shell reads it as it is.
func Quote(shell, s string) string {
	if shell == Fish {
		return fishQuote(s)
	}
	return shQuote(s)
}

// ExportLine returns the line that sets the environment variable name to value in the shell, quoted so that the
// shell reads the value as it is.
func ExportLine(shell, name, value string) string {
	if shell == Fish {
		return "set -gx " + name + " " + Quote(shell, value)
	}
	return "export " + name + "=" + Quote(shell, value)
}

// shQuote puts s in single quotes for sh, where a single quote of s ends the quoted part, follows escaped by a
// backslash and starts the next quoted part.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// fishQuote puts s in single quotes for fish, where a backslash escapes a backslash and a single quote.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
