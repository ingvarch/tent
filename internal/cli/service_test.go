package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
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
