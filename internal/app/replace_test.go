package app_test

import (
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func TestReplace(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	bigger := edit(t, workersYAML, "size: 2", "size: 4")
	changes, err := svc.Replace(t.Context(), decode(t, clusterYAML, bigger), true)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	wantChanges(t, changes, cluster(app.Unchanged), group("workers", app.Replaced))
	wantStored(t, svc.Store, workersPath, encode(t, bigger))
	wantPaths(t, svc.Store, clusterPath, serversPath, workersPath)

	// Once more changes nothing.
	changes, err = svc.Replace(t.Context(), decode(t, clusterYAML, bigger), true)
	if err != nil {
		t.Fatalf("Replace again: %v", err)
	}
	wantChanges(t, changes, cluster(app.Unchanged), group("workers", app.Unchanged))
	wantStored(t, svc.Store, workersPath, encode(t, bigger))
}

func TestReplaceCluster(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	pinned := edit(t, clusterYAML, "region: ams", "region: ams\n  nomad:\n    version: 2.0.7")
	changes, err := svc.Replace(t.Context(), decode(t, pinned), true)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	wantChanges(t, changes, cluster(app.Replaced))
	wantStored(t, svc.Store, clusterPath, encode(t, pinned))
}

func TestReplaceMissing(t *testing.T) {
	const hint = "; create it with create -f"
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	for _, tc := range []struct {
		name string
		docs []string
		want string
	}{
		{"node group", []string{clusterYAML, workersYAML}, notFound(svc, "node group workers of cluster prod") + hint},
		{"cluster", []string{edit(t, clusterYAML, "prod", "dev")}, notFound(svc, "cluster dev") + hint},
		{"node group of a missing cluster", []string{edit(t, workersYAML, "prod", "dev")},
			notFound(svc, "cluster dev")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Replace(t.Context(), decode(t, tc.docs...), true)
			wantError(t, err, tc.want)
			wantPaths(t, svc.Store, clusterPath, serversPath)
		})
	}
}

func TestReplaceInvalid(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	_, err := svc.Replace(t.Context(), decode(t, edit(t, serversYAML, "role: server", "role: client")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "Cluster prod", Path: "nodeGroups", Detail: "need exactly one server or combined group, found 0",
	})
	wantStored(t, svc.Store, serversPath, encode(t, serversYAML))
}
