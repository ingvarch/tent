package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ingvarch/tent/internal/rollout"
)

// TestRollRunWaitsOnlyForARefusalAfterAWrite waits for a refusal of the decisions once the run has sent a write, and
// for no other error and no refusal before one.
func TestRollRunWaitsOnlyForARefusalAfterAWrite(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("node group servers: unhealthy: %w", rollout.ErrRefused)
	for _, tc := range []struct {
		name  string
		wrote bool
		err   error
		want  bool
	}{
		{"a refusal after a write", true, refusal, true},
		{"a refusal at the first decision", false, refusal, false},
		{"another error after a write", true, errors.New("rollout: unknown mode 3"), false},
		{"no error after a write", true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &rollRun{rollLoop: newRollLoop()}
			r.wrote = tc.wrote
			if got := r.waitsFor(tc.err); got != tc.want {
				t.Errorf("waitsFor(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
