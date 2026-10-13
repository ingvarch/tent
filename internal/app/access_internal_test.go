package app

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/statestore"
)

// markFailingStore counts the reads of the bootstrap mark, and fails them with err when err is set.
type markFailingStore struct {
	statestore.Store
	markPath string
	err      error
	reads    int
}

func (s *markFailingStore) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	if p == s.markPath {
		s.reads++
		if s.err != nil {
			return nil, "", s.err
		}
	}
	return s.Store.Get(ctx, p)
}

// builtTestService returns a service over a store that holds the specs and the four secrets of cluster prod, and no
// mark.
func builtTestService(t *testing.T) (*Service, statestore.Layout) {
	t.Helper()
	s, l := secretsTestService(t)
	put := func(p, doc string) {
		t.Helper()
		if _, err := s.Store.Put(t.Context(), p, []byte(doc), statestore.PutOptions{}); err != nil {
			t.Fatalf("put %s: %v", p, err)
		}
	}
	put(l.ClusterSpec(), pinTestSpec(""))
	group, err := l.NodeGroup("servers")
	if err != nil {
		t.Fatal(err)
	}
	put(group, pinTestGroup)
	secrets, err := s.planSecrets(t.Context(), l)
	if err != nil {
		t.Fatalf("planSecrets: %v", err)
	}
	if err := s.writeSecrets(t.Context(), secrets.writes); err != nil {
		t.Fatalf("writeSecrets: %v", err)
	}
	return s, l
}

// TestLoadBuiltReturnsTheMarkAsStored reads the mark once and returns its bytes as stored, with the region, the
// provider and the stored CA of the cluster.
func TestLoadBuiltReturnsTheMarkAsStored(t *testing.T) {
	t.Parallel()
	s, l := builtTestService(t)
	const mark = "2026-10-09T23:02:07Z\n  with bytes as stored\n"
	if _, err := s.Store.Put(t.Context(), l.NomadBootstrapped(), []byte(mark), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	counted := &markFailingStore{Store: s.Store, markPath: l.NomadBootstrapped()}
	s.Store = counted

	got, err := s.loadBuilt(t.Context(), l)
	if err != nil {
		t.Fatalf("loadBuilt: %v", err)
	}
	if string(got.mark) != mark {
		t.Errorf("mark = %q, want %q", got.mark, mark)
	}
	if got.layout.Cluster() != "prod" || got.provider != v1alpha1.ProviderVultr ||
		got.region != v1alpha1.DefaultNomadRegion {
		t.Errorf("layout %q, provider %q, region %q", got.layout.Cluster(), got.provider, got.region)
	}
	stored, err := s.storedSecrets(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	if got.secrets.ca == nil || !bytes.Equal(got.secrets.ca.Bundle(), stored.ca.Bundle()) ||
		!bytes.Equal(got.secrets.bootstrap, stored.bootstrap) {
		t.Error("loadBuilt holds other secrets than the store")
	}
	if counted.reads != 1 {
		t.Errorf("the mark was read %d times, want once", counted.reads)
	}
}

// TestLoadBuiltReturnsTheReadErrorOfTheMark returns a failure of the mark's read as it is, not as a missing mark.
func TestLoadBuiltReturnsTheReadErrorOfTheMark(t *testing.T) {
	t.Parallel()
	s, l := builtTestService(t)
	boom := errors.New("the store is down")
	s.Store = &markFailingStore{Store: s.Store, markPath: l.NomadBootstrapped(), err: boom}
	if _, err := s.loadBuilt(t.Context(), l); !errors.Is(err, boom) {
		t.Errorf("loadBuilt error = %v, want %v", err, boom)
	}
}
