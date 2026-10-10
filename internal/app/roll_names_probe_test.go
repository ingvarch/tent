package app_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/statestore"
)

// capabilityCounter counts the Capabilities calls that reach its store: on an s3 store the first one is a probe that
// writes.
type capabilityCounter struct {
	statestore.Store
	calls *atomic.Int32
}

func (s capabilityCounter) Capabilities(ctx context.Context) (statestore.Capabilities, error) {
	s.calls.Add(1)
	return s.Store.Capabilities(ctx)
}

// TestPlansDoNotAskTheStoreForItsCapabilities plans an update and a rolling update, of every group and of a client
// group alone, without asking the store what it can do.
func TestPlansDoNotAskTheStoreForItsCapabilities(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		var calls atomic.Int32
		svc.Store = capabilityCounter{svc.Store, &calls}

		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("a plan of update asked for the Capabilities %d times, want 0", got)
		}
		for _, tc := range []struct {
			name string
			opts app.RollOptions
		}{
			{"every group", app.RollOptions{}},
			{"a client group", app.RollOptions{NodeGroups: []string{"workers"}}},
		} {
			calls.Store(0)
			if _, err := rollingUpdate(svc, tc.opts); err != nil {
				t.Fatalf("RollingUpdate of %s: %v", tc.name, err)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("a roll plan of %s asked for the Capabilities %d times, want 0", tc.name, got)
			}
		}
	})
}
