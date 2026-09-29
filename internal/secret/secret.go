// Package secret holds the type of keys and tokens that must never print.
package secret

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Secret holds a private key or a secret token. It never prints: fmt with any verb, slog and encoding/json show only
// its size, such as [secret, 44 bytes]. Bytes returns the content, to write it where it belongs.
type Secret []byte

// Bytes returns the content of the secret.
func (s Secret) Bytes() []byte { return s }

// String returns the size of the secret, such as [secret, 44 bytes], and never its content.
func (s Secret) String() string { return fmt.Sprintf("[secret, %d bytes]", len(s)) }

// GoString returns what String does, so that %#v shows no content either.
func (s Secret) GoString() string { return s.String() }

// Format writes what String returns, whatever the verb, width and flags.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

// LogValue makes slog log what String returns.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalJSON writes what String returns as a JSON string.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
