package uuid_test

import (
	"regexp"
	"testing"

	"github.com/ingvarch/tent/internal/uuid"
)

// uuidV4 matches a lower-case UUID of version 4 and of the variant of RFC 9562.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNew(t *testing.T) {
	const n = 5000
	seen := make(map[string]bool, n)
	chars := make([]map[byte]bool, 36) // the characters seen at each position
	for i := range chars {
		chars[i] = map[byte]bool{}
	}
	for range n {
		id := uuid.New()
		if !uuidV4.MatchString(id) {
			t.Fatalf("New() = %q, want a lower-case UUID v4", id)
		}
		if !uuid.Valid(id) {
			t.Fatalf("Valid(%q) = false for a UUID of New", id)
		}
		if seen[id] {
			t.Fatalf("New() gave %q twice", id)
		}
		seen[id] = true
		for i := range len(id) {
			chars[i][id[i]] = true
		}
	}
	// Every hex digit but the version is random, and the variant digit keeps two random bits.
	for i, got := range chars {
		want := 2
		switch i {
		case 8, 13, 14, 18, 23: // the "-" and the version, which the pattern checks
			continue
		case 19: // the variant: 8, 9, a or b
			want = 4
		}
		if len(got) < want {
			t.Errorf("position %d of %d UUIDs took %d values, want at least %d", i, n, len(got), want)
		}
	}
}

func TestValid(t *testing.T) {
	const valid = "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70"
	for _, tc := range []struct {
		name string
		s    string
		want bool
	}{
		{"lower-case v4", valid, true},
		{"variant 8", "5f0c2a9e-8d1b-4c7e-8f3a-2b6d8e1c4a70", true},
		{"variant a", "5f0c2a9e-8d1b-4c7e-af3a-2b6d8e1c4a70", true},
		{"variant b", "5f0c2a9e-8d1b-4c7e-bf3a-2b6d8e1c4a70", true},
		{"empty", "", false},
		{"upper case", "5F0C2A9E-8D1B-4C7E-9F3A-2B6D8E1C4A70", false},
		{"one upper-case digit", "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4A70", false},
		{"braces", "{" + valid + "}", false},
		{"urn", "urn:uuid:" + valid, false},
		{"no dashes", "5f0c2a9e8d1b4c7e9f3a2b6d8e1c4a70", false},
		{"a dash out of place", "5f0c2a9-e8d1b-4c7e-9f3a-2b6d8e1c4a70", false},
		{"version 1", "5f0c2a9e-8d1b-1c7e-9f3a-2b6d8e1c4a70", false},
		{"version 7", "5f0c2a9e-8d1b-7c7e-9f3a-2b6d8e1c4a70", false},
		{"variant 7", "5f0c2a9e-8d1b-4c7e-7f3a-2b6d8e1c4a70", false},
		{"variant c", "5f0c2a9e-8d1b-4c7e-cf3a-2b6d8e1c4a70", false},
		{"the nil UUID", "00000000-0000-0000-0000-000000000000", false},
		{"not hex", "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4g70", false},
		{"too short", valid[:35], false},
		{"too long", valid + "0", false},
		{"a trailing newline", valid + "\n", false},
		{"a word", "op-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uuid.Valid(tc.s); got != tc.want {
				t.Errorf("Valid(%q) = %t, want %t", tc.s, got, tc.want)
			}
		})
	}
}
