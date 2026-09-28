package cloud

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// opIDPattern matches the operation ids that NewOpID makes.
var opIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewOpID returns a new operation id: a random lower-case UUID of version 4, such as
// 5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70.
//
// An operation id marks what one create call makes. On a cloud whose names are not unique, a create whose outcome is
// unknown cannot be told apart by name, so the caller searches for the id: it adopts what it finds and otherwise
// creates again with the same id. The caller makes one id per object and keeps it across retries.
func NewOpID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])  // never fails: crypto/rand.Read ends the program instead
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // the variant of RFC 9562
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ValidOpID reports whether s has the form of the operation ids that NewOpID makes: a lower-case UUID of version 4
// and of the variant of RFC 9562, with its dashes and nothing around it.
func ValidOpID(s string) bool { return opIDPattern.MatchString(s) }
