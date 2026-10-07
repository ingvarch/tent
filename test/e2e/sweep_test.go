package e2e

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
)

// leftoverAge is how old a cluster must be for the sweep before a run to delete it.
const leftoverAge = 3 * time.Hour

// sweepLeftovers deletes the clusters of earlier runs that are older than leftoverAge at now.
func sweepLeftovers(ctx context.Context, api janitor.API, out io.Writer, now time.Time) error {
	j := janitor.New(api, out)
	found, err := j.Find(ctx, leftoverAge, now)
	if err != nil {
		return fmt.Errorf("look for leftovers: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	if err := j.Sweep(ctx, found); err != nil {
		return fmt.Errorf("delete leftovers: %w", err)
	}
	return nil
}
