package app_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
)

func TestLoad(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	for _, tc := range []struct {
		kind, name, path, doc string
		ref                   app.Ref
	}{
		{v1alpha1.KindCluster, "", clusterPath, clusterYAML, app.Ref{Cluster: "prod", Kind: "Cluster", Name: "prod"}},
		{v1alpha1.KindNodeGroup, "workers", workersPath, workersYAML,
			app.Ref{Cluster: "prod", Kind: "NodeGroup", Name: "workers"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			objs, ref, err := svc.Load(t.Context(), "prod", tc.kind, tc.name)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if diff := cmp.Diff(decode(t, tc.doc), objs); diff != "" {
				t.Errorf("Load (-want +got):\n%s", diff)
			}
			_, tc.ref.Version, _ = svc.Store.Get(t.Context(), tc.path)
			if diff := cmp.Diff(tc.ref, ref); diff != "" {
				t.Errorf("Load ref (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	for _, tc := range []struct {
		cluster, kind, name, want string
	}{
		{"dev", v1alpha1.KindCluster, "", notFound(svc, "cluster dev")},
		{"prod", v1alpha1.KindNodeGroup, "workers", notFound(svc, "node group workers of cluster prod")},
		{"prod", "Pod", "web", `unknown kind "Pod", want Cluster or NodeGroup`},
	} {
		_, _, err := svc.Load(t.Context(), tc.cluster, tc.kind, tc.name)
		wantError(t, err, tc.want)
	}
}

// load reads an object for editing and fails the test on an error.
func load(t *testing.T, svc *app.Service, kind, name string) app.Ref {
	t.Helper()
	_, ref, err := svc.Load(t.Context(), "prod", kind, name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return ref
}

func TestSave(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	ref := load(t, svc, v1alpha1.KindNodeGroup, "workers")
	bigger := edit(t, workersYAML, "size: 2", "size: 4")
	changes, err := svc.Save(t.Context(), ref, decode(t, bigger), true)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantChanges(t, changes, group("workers", app.Replaced))
	wantStored(t, svc.Store, workersPath, encode(t, bigger))
}

func TestSaveUnchanged(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	ref := load(t, svc, v1alpha1.KindCluster, "")
	changes, err := svc.Save(t.Context(), ref, decode(t, clusterYAML), true)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantChanges(t, changes, cluster(app.Unchanged))
}

func TestSaveChangedMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		kind, name, path  string
		meanwhile, edited string
		want              string
	}{
		{
			v1alpha1.KindCluster, "", clusterPath,
			edit(t, clusterYAML, "region: ams", "region: ams\n  sshKeys: []"),
			edit(t, clusterYAML, "region: ams", "region: ams\n  channel: beta"),
			"cluster prod changed while you edited it; run edit again",
		},
		{
			v1alpha1.KindNodeGroup, "workers", workersPath,
			edit(t, workersYAML, "size: 2", "size: 3"),
			edit(t, workersYAML, "size: 2", "size: 4"),
			"node group workers of cluster prod changed while you edited it; run edit again",
		},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			ref := load(t, svc, tc.kind, tc.name)
			if _, err := svc.Replace(t.Context(), decode(t, tc.meanwhile), true); err != nil {
				t.Fatalf("Replace: %v", err)
			}
			for _, apply := range []bool{false, true} {
				_, err := svc.Save(t.Context(), ref, decode(t, tc.edited), apply)
				wantError(t, err, tc.want)
			}
			wantStored(t, svc.Store, tc.path, encode(t, tc.meanwhile))
		})
	}
}

func TestSaveInvalid(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	ref := load(t, svc, v1alpha1.KindNodeGroup, "servers")
	for _, apply := range []bool{false, true} {
		_, err := svc.Save(t.Context(), ref, decode(t, edit(t, serversYAML, "size: 3", "size: 2")), apply)
		wantFieldErrors(t, err, v1alpha1.FieldError{
			Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server",
		})
	}
	wantStored(t, svc.Store, serversPath, encode(t, serversYAML))
}

func TestSaveDeletedMeanwhile(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	ref := load(t, svc, v1alpha1.KindNodeGroup, "workers")
	if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Save(t.Context(), ref, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
	wantError(t, err, notFound(svc, "node group workers of cluster prod"))
	wantPaths(t, svc.Store, clusterPath, serversPath)
}

func TestSaveRename(t *testing.T) {
	const (
		stillWorkers = "edit cannot rename; the spec must still be node group workers of cluster prod"
		stillCluster = "edit cannot rename; the spec must still be cluster prod"
	)
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	workers := load(t, svc, v1alpha1.KindNodeGroup, "workers")
	prod := load(t, svc, v1alpha1.KindCluster, "")
	stored := snapshot(t, svc.Store)
	for _, tc := range []struct {
		name string
		ref  app.Ref
		doc  string
		want string
	}{
		{"node group", workers, edit(t, workersYAML, "name: workers", "name: servers"), stillWorkers},
		{"cluster of a node group", workers, edit(t, workersYAML, "cluster: prod", "cluster: dev"), stillWorkers},
		{"kind", workers, clusterYAML, stillWorkers},
		{"cluster", prod, edit(t, clusterYAML, "name: prod", "name: dev"), stillCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Save(t.Context(), tc.ref, decode(t, tc.doc), true)
			wantError(t, err, tc.want)
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

func TestSaveOneObject(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	ref := load(t, svc, v1alpha1.KindCluster, "")
	for _, tc := range []struct {
		objs spec.Objects
		want string
	}{
		{spec.Objects{}, "edit saves one Cluster or one NodeGroup; the spec holds 0 objects"},
		{decode(t, clusterYAML, workersYAML), "edit saves one Cluster or one NodeGroup; the spec holds 2 objects"},
	} {
		_, err := svc.Save(t.Context(), ref, tc.objs, true)
		wantError(t, err, tc.want)
	}
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
}
