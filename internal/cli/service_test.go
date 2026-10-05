package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/nomadops"
)

// TestServiceInterruptedWhileOpening says interrupted, as the use cases do, when Ctrl-C comes before the store opens.
func TestServiceInterruptedWhileOpening(t *testing.T) {
	opts, _, err := resolve(t, "--state", newState(t).url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ctx, interrupt := context.WithCancel(t.Context())
	interrupt()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	_, err = opts.service(cmd, v1alpha1.ValidateOptions{})
	if err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled) {
		t.Errorf("service = %v, want interrupted, matching context.Canceled", err)
	}
}

// TestServiceGivesAssetsAndNomad passes the options WithAssets and WithNomad on to the service, and leaves the Nomad
// factory unset without WithNomad.
func TestServiceGivesAssetsAndNomad(t *testing.T) {
	errNomad := errors.New("no Nomad in this test")
	nomad := func(nomadops.Config) (nomadops.API, error) { return nil, errNomad }
	assetOpts := assets.Options{DevURL: "https://example.test/tent-node", DevSHA256: strings.Repeat("ab", 32)}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	opts, _, err := resolve(t, "--state", newState(t).url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	WithAssets(assetOpts)(opts)
	WithNomad(nomad)(opts)
	svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.Assets.DevURL != assetOpts.DevURL || svc.Assets.DevSHA256 != assetOpts.DevSHA256 {
		t.Errorf("service.Assets = %+v, want %+v", svc.Assets, assetOpts)
	}
	if svc.Nomad == nil {
		t.Fatal("service.Nomad is nil, want the factory of WithNomad")
	}
	if _, err := svc.Nomad(nomadops.Config{}); !errors.Is(err, errNomad) {
		t.Errorf("service.Nomad returned %v, want the error of the factory of WithNomad", err)
	}

	bare, _, err := resolve(t, "--state", newState(t).url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	svc, err = bare.service(cmd, v1alpha1.ValidateOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.Nomad != nil {
		t.Error("service.Nomad is set without WithNomad, want nil")
	}
}
