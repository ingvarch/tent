package pki_test

import (
	"bytes"
	"testing"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
)

func TestSecretString(t *testing.T) {
	s := pki.Secret("0123456789")
	if got, want := s.String(), "[secret, 10 bytes]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := s.GoString(), "[secret, 10 bytes]"; got != want {
		t.Errorf("GoString() = %q, want %q", got, want)
	}
}

// TestSecretNeverPrints checks that no way of printing or logging a secret, alone or in a struct, shows it: only its
// size.
func TestSecretNeverPrints(t *testing.T) {
	s := pki.Secret("gossip-key-Zm9vYmFyYmF6")
	const redaction = "[secret, 23 bytes]"
	secrets := map[string][]byte{"the secret": s}
	secrettest.CheckHidden(t, secrettest.Printed(t, s), secrets, redaction)
	secrettest.CheckHidden(t, secrettest.Printed(t, struct{ Token pki.Secret }{s}), secrets, redaction)
	// The value itself stays the bytes as given.
	if !bytes.Equal(s.Bytes(), []byte("gossip-key-Zm9vYmFyYmF6")) {
		t.Errorf("Bytes() of %d bytes differs from the bytes as given", len(s.Bytes()))
	}
}
