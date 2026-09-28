// Package english writes lists as English sentences do, for tent's messages.
package english

import "strings"

// And joins items as a sentence lists them: "a", "a and b", "a, b and c".
func And(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	last := len(items) - 1
	return strings.Join(items[:last], ", ") + " and " + items[last]
}
