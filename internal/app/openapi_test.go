package app_test

import (
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

// TestOpenAPI tells OnOpenAPI when a change leaves a cluster whose Nomad API the whole internet may reach.
func TestOpenAPI(t *testing.T) {
	svc, _ := newService(t)
	calls := 0
	svc.OnOpenAPI = func() { calls++ }
	wantCalls := func(step string, want int) {
		t.Helper()
		if calls != want {
			t.Errorf("after %s, OnOpenAPI was called %d times, want %d", step, calls, want)
		}
	}
	narrow := edit(t, clusterYAML, "region: ams", "region: ams\n  access:\n    api: [203.0.113.0/24]")
	bigger := edit(t, workersYAML, "size: 2", "size: 4")

	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML) // access.api left out is 0.0.0.0/0
	wantCalls("create", 1)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML) // unchanged, and still open
	wantCalls("create again", 2)
	mustReplace(t, svc, bigger) // the stored Cluster is open
	wantCalls("replace of a node group", 3)
	if _, err := svc.Replace(t.Context(), decode(t, workersYAML), false); err != nil { // the cluster is still open
		t.Fatalf("Replace without apply: %v", err)
	}
	wantCalls("a check", 3)
	mustReplace(t, svc, narrow)
	wantCalls("replace with a narrow API", 3)
	mustReplace(t, svc, workersYAML)
	wantCalls("replace of a node group of a narrow cluster", 3)
	mustReplace(t, svc, edit(t, narrow, "203.0.113.0/24", "\"::/0\""))
	wantCalls("replace with ::/0", 4)

	_, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "size: 2", "size: -1")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{Object: "NodeGroup workers", Path: "spec.size",
		Detail: "must not be negative"})
	wantCalls("a failed replace", 4)
}

// mustReplace replaces objects and fails the test on an error.
func mustReplace(t *testing.T, svc *app.Service, docs ...string) {
	t.Helper()
	if _, err := svc.Replace(t.Context(), decode(t, docs...), true); err != nil {
		t.Fatalf("Replace: %v", err)
	}
}

// TestUpdateWarnsWhenTheAPIIsOpen tells OnOpenAPI once, before the first change, when an update applies changes to a
// cluster whose Nomad API the whole internet may reach.
func TestUpdateWarnsWhenTheAPIIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t) // access.api left out is 0.0.0.0/0
		var events []string
		svc.OnOpenAPI = func() { events = append(events, "open") }
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }
		wantWarnings := func(step string, want int) {
			t.Helper()
			n := 0
			for _, e := range events {
				if e == "open" {
					n++
				}
			}
			if n != want {
				t.Errorf("after %s, OnOpenAPI was called %d times, want %d", step, n, want)
			}
		}

		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantWarnings("a plan", 0)
		mustUpdate(t, svc)
		wantWarnings("an update that applies changes", 1)
		if len(events) < 2 || events[0] != "open" {
			t.Errorf("the events start with %q, want the warning before the first change", events[:min(2, len(events))])
		}
		mustUpdate(t, svc)
		wantWarnings("an update without changes", 1)

		narrow := edit(t, keyedClusterYAML, "region: ams", "region: ams\n  access:\n    api: [203.0.113.0/24]")
		mustReplace(t, svc, narrow)
		plan := mustUpdate(t, svc)
		if !plan.HasChanges() {
			t.Fatal("narrowing the API changes nothing")
		}
		wantWarnings("an update of a cluster with a narrow API", 1)
	})
}
