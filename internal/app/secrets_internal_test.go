package app

import (
	"bytes"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

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

// storedTestSecrets stores a new CA, gossip key and bootstrap secret, and returns them.
func storedTestSecrets(t *testing.T, s *Service, l statestore.Layout) clusterSecrets {
	t.Helper()
	planned, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets: %v", err)
	}
	if err := s.writeSecrets(t.Context(), planned.writes); err != nil {
		t.Fatalf("writeSecrets: %v", err)
	}
	return planned
}

// TestStoredSecretsReturnsTheStoredSecrets holds the stored secrets and writes nothing.
func TestStoredSecretsReturnsTheStoredSecrets(t *testing.T) {
	s, l := secretsTestService(t)
	want := storedTestSecrets(t, s, l)
	before := len(list(t, s.Store, ""))

	got, err := s.storedSecrets(t.Context(), l)

	if err != nil {
		t.Fatalf("storedSecrets: %v", err)
	}
	if !bytes.Equal(got.ca.Bundle(), want.ca.Bundle()) || !bytes.Equal(got.ca.Key(), want.ca.Key()) ||
		!bytes.Equal(got.gossip, want.gossip) || !bytes.Equal(got.bootstrap, want.bootstrap) || len(got.writes) != 0 {
		t.Error("storedSecrets differ from the stored secrets, or have writes")
	}
	if after := len(list(t, s.Store, "")); after != before {
		t.Errorf("the store holds %d objects, was %d", after, before)
	}
}

// TestStoredSecretsNamesTheMissingPaths fails with the paths of the secrets that the store lacks, and writes nothing.
func TestStoredSecretsNamesTheMissingPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deleted func(statestore.Layout) []string
		want    string
	}{
		{"nothing", func(statestore.Layout) []string { return nil }, "the state store lacks pki/private/ca.key, " +
			"pki/ca-bundle.pem, secrets/gossip.key and secrets/acl-bootstrap-token"},
		{"the bundle", func(l statestore.Layout) []string { return []string{l.CABundle()} },
			"the state store lacks pki/ca-bundle.pem"},
		{"the gossip key and the bootstrap secret",
			func(l statestore.Layout) []string { return []string{l.GossipKey(), l.ACLBootstrapSecret()} },
			"the state store lacks secrets/gossip.key and secrets/acl-bootstrap-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, l := secretsTestService(t)
			if tc.name != "nothing" {
				storedTestSecrets(t, s, l)
			}
			for _, p := range tc.deleted(l) {
				if err := s.Store.Delete(t.Context(), p); err != nil {
					t.Fatalf("delete %s: %v", p, err)
				}
			}
			before := list(t, s.Store, "")

			_, err := s.storedSecrets(t.Context(), l)

			if _, ok := errors.AsType[*missingSecretsError](err); !ok || err.Error() != tc.want {
				t.Errorf("storedSecrets error = %v, want a missing-secrets error saying %q", err, tc.want)
			}
			if diff := cmp.Diff(before, list(t, s.Store, "")); diff != "" {
				t.Errorf("the store changed (-before +after):\n%s", diff)
			}
		})
	}
}

// TestStoredSecretsFailsForABrokenSecret returns the error of a stored secret that does not load, which is no missing
// secret, and shows no secret.
func TestStoredSecretsFailsForABrokenSecret(t *testing.T) {
	s, l := secretsTestService(t)
	storedTestSecrets(t, s, l)
	broken := []byte("hunter2-not-a-key")
	if _, err := s.Store.Put(t.Context(), l.GossipKey(), broken, statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := s.storedSecrets(t.Context(), l)

	if err == nil {
		t.Fatal("storedSecrets succeeded with a broken gossip key")
	}
	if _, missing := errors.AsType[*missingSecretsError](err); missing {
		t.Errorf("error %v is a missing-secrets error", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q shows the secret", err)
	}
}

// list returns the paths in the store.
func list(t *testing.T, s statestore.Store, prefix string) []string {
	t.Helper()
	paths, err := s.List(t.Context(), prefix)
	if err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return paths
}
