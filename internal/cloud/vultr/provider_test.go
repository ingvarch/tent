package vultr

import (
	"bytes"
	"log/slog"
	"regexp"
	"testing"
)

func TestProviderName(t *testing.T) {
	if got := New(nil).Name(); got != "vultr" {
		t.Errorf("Name() = %q, want vultr", got)
	}
}

func TestProviderLogger(t *testing.T) {
	if got := New(nil).log; got != slog.Default() {
		t.Errorf("the default logger is %p, want slog.Default() %p", got, slog.Default())
	}
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if got := New(nil, WithLogger(l)).log; got != l {
		t.Errorf("WithLogger(l) gives the logger %p, want l %p", got, l)
	}
	if got := New(nil, WithLogger(nil)).log; got != slog.Default() {
		t.Errorf("WithLogger(nil) gives the logger %p, want slog.Default() %p", got, slog.Default())
	}
}

func TestProviderOpIDs(t *testing.T) {
	p := New(nil, withOpIDs(func() string { return "op-1" }))
	if got := p.opID(); got != "op-1" {
		t.Errorf("opID() = %q, want the one withOpIDs gives, op-1", got)
	}
}

// uuidV4 matches a lower-case UUID of version 4 and of the variant of RFC 9562.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestProviderDefaultOpIDs(t *testing.T) {
	const n = 1000
	p := New(nil)
	seen := make(map[string]bool, n)
	chars := make([]map[byte]bool, 36) // the characters seen at each position
	for i := range chars {
		chars[i] = map[byte]bool{}
	}
	for range n {
		id := p.opID()
		if !uuidV4.MatchString(id) {
			t.Fatalf("opID() = %q, want a lower-case UUID v4", id)
		}
		if seen[id] {
			t.Fatalf("opID() gave %q twice", id)
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
			t.Errorf("position %d of %d ids took %d values, want at least %d", i, n, len(got), want)
		}
	}
}
