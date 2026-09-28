package cloud_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
)

func TestUnsupportedProvider(t *testing.T) {
	err := cloud.UnsupportedProvider(v1alpha1.ProviderHetzner)
	if want := "tent cannot manage clusters on hetzner yet"; err == nil || err.Error() != want {
		t.Errorf("UnsupportedProvider(hetzner) = %v, want %s", err, want)
	}
	for _, e := range []error{err, fmt.Errorf("look up the provider: %w", err)} {
		if !errors.Is(e, cloud.ErrUnsupportedProvider) {
			t.Errorf("errors.Is(%v, ErrUnsupportedProvider) = false", e)
		}
	}
	// The words alone do not make an unsupported provider.
	if errors.Is(errors.New(err.Error()), cloud.ErrUnsupportedProvider) {
		t.Error("an error with the same text matches ErrUnsupportedProvider")
	}
}
