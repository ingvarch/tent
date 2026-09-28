package cloud

import "github.com/ingvarch/tent/internal/uuid"

// NewOpID returns a new operation id: a random lower-case UUID of version 4, such as
// 5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70.
//
// An operation id marks what one create call makes. On a cloud whose names are not unique, a create whose outcome is
// unknown cannot be told apart by name, so the caller searches for the id: it adopts what it finds and otherwise
// creates again with the same id. The caller makes one id per object and keeps it across retries.
func NewOpID() string { return uuid.New() }

// ValidOpID reports whether s has the form of the operation ids that NewOpID makes: a lower-case UUID of version 4
// and of the variant of RFC 9562, with its dashes and nothing around it.
func ValidOpID(s string) bool { return uuid.Valid(s) }
