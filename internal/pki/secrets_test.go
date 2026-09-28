package pki_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/uuid"
)

func TestNewGossipKey(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k := pki.NewGossipKey()
		b, err := base64.StdEncoding.DecodeString(string(k))
		if err != nil || len(b) != 32 {
			t.Fatalf("NewGossipKey() of %d bytes decodes to %d bytes, %v; want 32 bytes of standard base64",
				len(k), len(b), err)
		}
		if seen[string(k)] {
			t.Fatal("NewGossipKey() gave the same key twice")
		}
		seen[string(k)] = true
	}
}

func TestNewBootstrapSecret(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s := pki.NewBootstrapSecret()
		if !uuid.Valid(string(s)) {
			t.Fatalf("NewBootstrapSecret() of %d bytes is not a lower-case UUID v4", len(s))
		}
		if seen[string(s)] {
			t.Fatal("NewBootstrapSecret() gave the same secret twice")
		}
		seen[string(s)] = true
	}
}

// TestNewSecretsNeverPrint checks that no way of printing or logging the gossip key or the bootstrap secret shows it.
func TestNewSecretsNeverPrint(t *testing.T) {
	for name, s := range map[string]pki.Secret{
		"the gossip key": pki.NewGossipKey(), "the bootstrap secret": pki.NewBootstrapSecret(),
	} {
		t.Run(name, func(t *testing.T) {
			secrettest.CheckHidden(t, secrettest.Printed(t, s), map[string][]byte{name: s},
				fmt.Sprintf("[secret, %d bytes]", len(s)))
		})
	}
}

// badSecret is a secret that a check rejects, with the error it gives.
type badSecret struct {
	name   string
	secret string
	want   string
}

// checkSecret checks that check accepts each of good and rejects each of bad with its error, which shows no secret.
func checkSecret(t *testing.T, check func(pki.Secret) error, good []pki.Secret, bad []badSecret) {
	t.Helper()
	for _, s := range good {
		if err := check(s); err != nil {
			t.Errorf("a good secret of %d bytes: %v", len(s), err)
		}
	}
	for _, tc := range bad {
		err := check(pki.Secret(tc.secret))
		switch {
		case err == nil:
			t.Errorf("%s: no error, want %q", tc.name, tc.want)
		case secrettest.Shows(err.Error(), []byte(tc.secret)):
			t.Errorf("%s: the error shows the secret", tc.name)
		case err.Error() != tc.want:
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestCheckGossipKey(t *testing.T) {
	const short, long = "gossip key: decodes to 16 bytes, not 32", "gossip key: decodes to 33 bytes, not 32"
	const notStandard = "gossip key: not standard base64"
	key := string(pki.NewGossipKey())
	checkSecret(t, pki.CheckGossipKey, []pki.Secret{pki.NewGossipKey(), pki.NewGossipKey()}, []badSecret{
		{"a line end", key + "\n", notStandard},
		{"CRLF", key + "\r\n", notStandard},
		{"a break inside", key[:20] + "\n" + key[20:], notStandard},
		{"empty", "", "gossip key: decodes to 0 bytes, not 32"},
		{"16 bytes", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), short},
		{"33 bytes", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef!")), long},
		{"not base64", "not base64 at all!", "gossip key: not standard base64"},
		{"URL-safe base64", strings.Repeat("-_", 22), "gossip key: not standard base64"},
	})
}

func TestCheckBootstrapSecret(t *testing.T) {
	const want = "ACL bootstrap secret: not a lower-case UUID of version 4"
	good := pki.NewBootstrapSecret()
	checkSecret(t, pki.CheckBootstrapSecret, []pki.Secret{good, pki.NewBootstrapSecret()}, []badSecret{
		{"empty", "", want},
		{"upper case", strings.ToUpper(string(good)), want},
		{"a line end", string(good) + "\n", want},
		{"version 1", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", want},
	})
}
