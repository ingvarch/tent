package app

import (
	"bytes"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/statestore"
)

// secretsTestService returns a service over a new store and the layout of cluster prod.
func secretsTestService(t *testing.T) (*Service, statestore.Layout) {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.ToSlash(t.TempDir())
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	s, err := statestore.Open(t.Context(), (&url.URL{Scheme: "file", Path: dir}).String())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return &Service{Store: s, Version: "dev"}, l
}

// TestPlanSecretsReturnsNewSecrets holds all four secrets of a new cluster, with a write for each.
func TestPlanSecretsReturnsNewSecrets(t *testing.T) {
	s, l := secretsTestService(t)
	got, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets: %v", err)
	}
	if got.ca == nil || len(got.gossip) == 0 || len(got.bootstrap) == 0 {
		t.Fatalf("the secrets hold a CA: %t, a gossip key: %t, a bootstrap secret: %t, want all",
			got.ca != nil, len(got.gossip) > 0, len(got.bootstrap) > 0)
	}
	if len(got.writes) != len(l.Secrets()) {
		t.Fatalf("%d writes, want %d", len(got.writes), len(l.Secrets()))
	}
	byPath := map[string]pki.Secret{}
	for _, w := range got.writes {
		byPath[w.path] = w.data
	}
	if !bytes.Equal(byPath[l.GossipKey()], got.gossip) || !bytes.Equal(byPath[l.ACLBootstrapSecret()], got.bootstrap) ||
		!bytes.Equal(byPath[l.CAKey()], got.ca.Key()) || !bytes.Equal(byPath[l.CABundle()], got.ca.Bundle()) {
		t.Error("the writes differ from the secrets of the run")
	}
}

// TestPlanSecretsReturnsStoredSecrets holds the stored CA, gossip key and bootstrap secret, with nothing to write.
func TestPlanSecretsReturnsStoredSecrets(t *testing.T) {
	s, l := secretsTestService(t)
	first, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets: %v", err)
	}
	if err := s.writeSecrets(t.Context(), first.writes); err != nil {
		t.Fatalf("writeSecrets: %v", err)
	}
	again, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets again: %v", err)
	}
	if len(again.writes) != 0 {
		t.Errorf("%d writes, want none", len(again.writes))
	}
	if again.ca == nil || !bytes.Equal(again.ca.Bundle(), first.ca.Bundle()) ||
		!bytes.Equal(again.ca.Key(), first.ca.Key()) {
		t.Error("the CA is not the stored one")
	}
	if !bytes.Equal(again.gossip, first.gossip) || !bytes.Equal(again.bootstrap, first.bootstrap) {
		t.Error("the gossip key or the bootstrap secret is not the stored one")
	}
}

// TestPlanSecretsCompletesCAFromStoredKey signs a bundle with the stored key when only the key is there.
func TestPlanSecretsCompletesCAFromStoredKey(t *testing.T) {
	s, l := secretsTestService(t)
	first, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets: %v", err)
	}
	if err := s.writeSecrets(t.Context(), first.writes); err != nil {
		t.Fatalf("writeSecrets: %v", err)
	}
	if err := s.Store.Delete(t.Context(), l.CABundle()); err != nil {
		t.Fatalf("delete the bundle: %v", err)
	}
	again, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets again: %v", err)
	}
	if again.ca == nil || !bytes.Equal(again.ca.Key(), first.ca.Key()) {
		t.Fatal("the CA is missing or does not use the stored key")
	}
	if len(again.writes) != 1 || again.writes[0].path != l.CABundle() ||
		!bytes.Equal(again.writes[0].data, again.ca.Bundle()) {
		t.Errorf("writes %v, want only the CA bundle with the bundle of the CA", relativePaths(l, again.writes))
	}
}
