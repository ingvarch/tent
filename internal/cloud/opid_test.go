package cloud_test

import (
	"testing"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/uuid"
)

// The form of operation ids is tested in internal/uuid; these tests check that NewOpID and ValidOpID use it.

func TestNewOpID(t *testing.T) {
	a, b := cloud.NewOpID(), cloud.NewOpID()
	for _, id := range []string{a, b} {
		if !uuid.Valid(id) || !cloud.ValidOpID(id) {
			t.Errorf("NewOpID() = %q, want a lower-case UUID v4", id)
		}
	}
	if a == b {
		t.Errorf("NewOpID() gave %q twice", a)
	}
}

func TestValidOpID(t *testing.T) {
	for _, id := range []string{
		"5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70", "5F0C2A9E-8D1B-4C7E-9F3A-2B6D8E1C4A70",
		"5f0c2a9e-8d1b-1c7e-9f3a-2b6d8e1c4a70", "op-1", "",
	} {
		if got, want := cloud.ValidOpID(id), uuid.Valid(id); got != want {
			t.Errorf("ValidOpID(%q) = %t, want %t as uuid.Valid", id, got, want)
		}
	}
}
