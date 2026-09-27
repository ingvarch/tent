package app_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

func TestGet(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	got, err := svc.Get(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(decode(t, clusterYAML, serversYAML, workersYAML), got); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}
}

// TestGetSortsNodeGroups stores groups whose paths sort in another order than their names: web-2.yaml before
// web.yaml.
func TestGetSortsNodeGroups(t *testing.T) {
	svc, _ := newService(t)
	web := edit(t, workersYAML, "name: workers", "name: web")
	web2 := edit(t, workersYAML, "name: workers", "name: web-2")
	mustCreate(t, svc, clusterYAML, serversYAML, web2, web)
	got, err := svc.Get(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var names []string
	for _, g := range got.NodeGroups {
		names = append(names, g.Metadata.Name)
	}
	if diff := cmp.Diff([]string{"servers", "web", "web-2"}, names); diff != "" {
		t.Errorf("node groups (-want +got):\n%s", diff)
	}
}

func TestGetFull(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	got, err := svc.Get(t.Context(), "prod", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(full(t, clusterYAML, serversYAML, workersYAML), got); diff != "" {
		t.Errorf("Get full (-want +got):\n%s", diff)
	}
	// The store keeps the user spec.
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
}

func TestGetMissing(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	_, err := svc.Get(t.Context(), "dev", false)
	wantError(t, err, notFound(svc, "cluster dev"))
}

func TestClusters(t *testing.T) {
	svc, _ := newService(t)
	rename := func(doc string) string { return edit(t, doc, "prod", "dev") }
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	mustCreate(t, svc, rename(clusterYAML), rename(serversYAML))
	for _, tc := range []struct {
		full bool
		want []spec.Objects
	}{
		{false, []spec.Objects{
			decode(t, rename(clusterYAML), rename(serversYAML)),
			decode(t, clusterYAML, serversYAML, workersYAML),
		}},
		{true, []spec.Objects{
			full(t, rename(clusterYAML), rename(serversYAML)),
			full(t, clusterYAML, serversYAML, workersYAML),
		}},
	} {
		got, err := svc.Clusters(t.Context(), tc.full)
		if err != nil {
			t.Fatalf("Clusters(full=%t): %v", tc.full, err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("Clusters(full=%t) (-want +got):\n%s", tc.full, diff)
		}
	}
}

func TestClustersOfEmptyStore(t *testing.T) {
	svc, _ := newService(t)
	got, err := svc.Clusters(t.Context(), false)
	if err != nil || len(got) != 0 {
		t.Errorf("Clusters = %v, %v; want none", got, err)
	}
}

// full decodes YAML documents and fills in the defaults.
func full(t *testing.T, docs ...string) spec.Objects {
	t.Helper()
	objs := decode(t, docs...)
	v1alpha1.SetDefaults(objs.Cluster, objs.NodeGroups)
	return objs
}

// TestReadsCheckTheTentVersion reads a cluster that a newer tent wrote in a format this one cannot decode.
func TestReadsCheckTheTentVersion(t *testing.T) {
	const want = "cluster prod needs tent v0.9.0 or newer; this is v0.4.0"
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	put(t, svc.Store, clusterPath, []byte(clusterYAML+"  future: yes\n"))
	put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
	svc.Version = "v0.4.0"
	_, err := svc.Get(t.Context(), "prod", false)
	wantError(t, err, want)
	_, _, err = svc.Load(t.Context(), "prod", v1alpha1.KindCluster, "")
	wantError(t, err, want)
	_, err = svc.Clusters(t.Context(), false)
	wantError(t, err, want)
	if !errors.Is(err, statestore.ErrTentTooOld) {
		t.Errorf("errors.Is(%v, ErrTentTooOld) = false", err)
	}
}

func TestNodeGroups(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	for _, tc := range []struct {
		names []string
		full  bool
		want  spec.Objects
	}{
		{nil, false, decode(t, serversYAML, workersYAML)},
		{[]string{"workers"}, false, decode(t, workersYAML)},
		{[]string{"workers", "servers"}, false, decode(t, workersYAML, serversYAML)},
		{[]string{"workers"}, true, spec.Objects{NodeGroups: full(t, clusterYAML, workersYAML).NodeGroups}},
	} {
		got, err := svc.NodeGroups(t.Context(), "prod", tc.names, tc.full)
		if err != nil {
			t.Fatalf("NodeGroups(%v): %v", tc.names, err)
		}
		if diff := cmp.Diff(tc.want.NodeGroups, got); diff != "" {
			t.Errorf("NodeGroups(%v, full=%t) (-want +got):\n%s", tc.names, tc.full, diff)
		}
	}
	_, err := svc.NodeGroups(t.Context(), "prod", []string{"servers", "web"}, false)
	wantError(t, err, notFound(svc, "node group web of cluster prod"))
	_, err = svc.NodeGroups(t.Context(), "prod", []string{"Web"}, false)
	wantError(t, err, `invalid node group name "Web": `+nameRule)
	_, err = svc.NodeGroups(t.Context(), "dev", nil, false)
	wantError(t, err, notFound(svc, "cluster dev"))
}
