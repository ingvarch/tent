package app_test

import (
	"testing"

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
