package pki

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/ingvarch/tent/internal/uuid"
)

// gossipKeySize is how many bytes a gossip key has.
const gossipKeySize = 32

// NewGossipKey returns a new key that encrypts the gossip of Nomad servers: 32 random bytes in standard base64, as
// nomad operator gossip keyring generate makes.
func NewGossipKey() Secret {
	var b [gossipKeySize]byte
	_, _ = rand.Read(b[:]) // never fails: crypto/rand.Read ends the program instead
	return Secret(base64.StdEncoding.EncodeToString(b[:]))
}

// CheckGossipKey checks that key is a gossip key as NewGossipKey makes one: 32 bytes in standard base64, with nothing
// else, not even a line end. Its errors never show the key.
func CheckGossipKey(key Secret) error {
	b, err := base64.StdEncoding.DecodeString(string(key))
	switch {
	// The decoder skips line ends; only the canonical form encodes back to the key.
	case err != nil || base64.StdEncoding.EncodeToString(b) != string(key):
		return errors.New("gossip key: not standard base64")
	case len(b) != gossipKeySize:
		return fmt.Errorf("gossip key: decodes to %d bytes, not %d", len(b), gossipKeySize)
	}
	return nil
}

// NewBootstrapSecret returns a new secret for the ACL bootstrap token: a random lower-case UUID of version 4.
func NewBootstrapSecret() Secret { return Secret(uuid.New()) }

// CheckBootstrapSecret checks that secret is a secret for the ACL bootstrap token as NewBootstrapSecret makes one: a
// lower-case UUID of version 4. Its errors never show the secret.
func CheckBootstrapSecret(secret Secret) error {
	if !uuid.Valid(string(secret)) {
		return errors.New("ACL bootstrap secret: not a lower-case UUID of version 4")
	}
	return nil
}
