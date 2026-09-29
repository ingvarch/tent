// Package pki makes a cluster's certificate authority, the certificates of its nodes and operators, the gossip key
// and the ACL bootstrap secret.
package pki

import "github.com/ingvarch/tent/internal/secret"

// Secret holds a private key or a secret token, and never prints. Bytes returns the content, to write it to the state
// store.
type Secret = secret.Secret
