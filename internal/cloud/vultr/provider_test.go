package vultr

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/ingvarch/tent/internal/cloud"
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

// TestProviderDefaultOpIDs checks that the default operation ids are new ones of the form that cloud.NewOpID makes.
func TestProviderDefaultOpIDs(t *testing.T) {
	p := New(nil)
	a, b := p.opID(), p.opID()
	for _, id := range []string{a, b} {
		if !cloud.ValidOpID(id) {
			t.Errorf("opID() = %q, want a lower-case UUID v4", id)
		}
	}
	if a == b {
		t.Errorf("opID() gave %q twice", a)
	}
}
