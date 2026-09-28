// Package uuid makes and checks random UUIDs of version 4 in their lower-case text form.
package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// pattern matches the UUIDs that New makes.
var pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// New returns a new random lower-case UUID of version 4, such as 5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70.
func New() string {
	var b [16]byte
	_, _ = rand.Read(b[:])  // never fails: crypto/rand.Read ends the program instead
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // the variant of RFC 9562
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Valid reports whether s has the form of the UUIDs that New makes: a lower-case UUID of version 4 and of the variant
// of RFC 9562, with its dashes and nothing around it.
func Valid(s string) bool { return pattern.MatchString(s) }
