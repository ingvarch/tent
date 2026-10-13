package nomadfake_test

import (
	"errors"
	"testing"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
)

// accessor returns the accessor that the client's token has, and fails the test when the call fails.
func accessor(t *testing.T, a nomadops.API) string {
	t.Helper()
	got, err := a.TokenAccessor(t.Context())
	if err != nil {
		t.Fatalf("TokenAccessor: %v", err)
	}
	return got
}

// TestTokenAccessorIsOfTheBootstrapToken checks that the bootstrap secret has an accessor, the same at every read,
// and that a secret that no token has is denied with a permanent error of the class ErrDenied.
func TestTokenAccessorIsOfTheBootstrapToken(t *testing.T) {
	f, a := newBootstrappedAPI(t)

	first := accessor(t, a)

	if first == "" || accessor(t, a) != first {
		t.Errorf("the accessor = %q, then another one, want one that is not empty and the same", first)
	}
	stranger := f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
	_, err := stranger.TokenAccessor(t.Context())
	checkErr(t, err, "nomadfake: TokenAccessor: permission denied", false)
	if !errors.Is(err, nomadops.ErrDenied) {
		t.Errorf("errors.Is(%v, ErrDenied) = false, want true", err)
	}
}

// TestACLDenialsMatchErrDenied checks that a call before the bootstrap fails with the class ErrDenied.
func TestACLDenialsMatchErrDenied(t *testing.T) {
	_, a := newAPI()

	_, err := a.Nodes(t.Context())

	if !errors.Is(err, nomadops.ErrDenied) {
		t.Errorf("errors.Is(%v, ErrDenied) = false, want true", err)
	}
}

// TestEachBootstrapGetsANewAccessor checks that the bootstrap of a new cluster makes a token with a new accessor, also
// with the same secret, and that SetBootstrapped makes one too.
func TestEachBootstrapGetsANewAccessor(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	first := accessor(t, a)

	f.NewCluster()
	f.SetLeader(leader)
	if err := a.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	second := accessor(t, a)
	f.SetBootstrapped(bootstrapSecret)
	third := accessor(t, a)

	if first == second || second == third || first == third {
		t.Errorf("accessors %q, %q and %q, want three that differ", first, second, third)
	}
}

// TestRestoreSnapshotGivesTheSecretTheBackupsAccessor checks that a restore of a snapshot that the fake saved makes the
// bootstrap secret the token of the cluster that the snapshot was taken from, and that bytes that no save made change
// nothing.
func TestRestoreSnapshotGivesTheSecretTheBackupsAccessor(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	earlier := accessor(t, a)
	snap := save(t, a)
	f.NewCluster()
	f.SetLeader(leader)
	if err := a.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	rebuilt := accessor(t, a)

	if err := a.RestoreSnapshot(t.Context(), secret.Secret("nomadfake snapshot 99")); err != nil {
		t.Fatalf("RestoreSnapshot of bytes that no save made: %v", err)
	}
	if got := accessor(t, a); got != rebuilt {
		t.Errorf("accessor = %q after a restore of unknown bytes, want the cluster's own %q", got, rebuilt)
	}
	if err := a.RestoreSnapshot(t.Context(), snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	if got := accessor(t, a); got != earlier || got == rebuilt {
		t.Errorf("accessor = %q after the restore, want the backup's %q and not the new cluster's %q",
			got, earlier, rebuilt)
	}
}
