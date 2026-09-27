// Package enginetest holds the checks that tests of engine tasks run.
package enginetest

import (
	"context"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/engine"
)

// ApplyReplan checks that tasks bring the cloud to what they want. It takes a snapshot with inventory, plans the tasks
// against it and applies the plan with the default options, then takes a fresh snapshot and plans again. Any error
// stops t. The second plan must have no changes; when it has some, t fails with the plan's text.
func ApplyReplan(t testing.TB, tasks []engine.Task, kinds []engine.Kind,
	inventory func(context.Context) (engine.Snapshot, error)) {
	t.Helper()
	plan := func(when string) *engine.Plan {
		t.Helper()
		snap, err := inventory(t.Context())
		if err != nil {
			t.Fatalf("inventory %s: %v", when, err)
		}
		p, err := engine.NewPlan(t.Context(), tasks, kinds, snap)
		if err != nil {
			t.Fatalf("plan %s: %v", when, err)
		}
		return p
	}
	if err := plan("before apply").Apply(t.Context(), engine.ApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	again := plan("after apply")
	if !again.HasChanges() {
		return
	}
	var text strings.Builder
	if err := again.WriteText(&text); err != nil {
		t.Fatalf("writing the plan after apply: %v", err)
	}
	t.Errorf("the plan after apply has changes, want none:\n%s", text.String())
}
