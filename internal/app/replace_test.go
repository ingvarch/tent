package app_test

import (
	"errors"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
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

// moved is the detail of a problem with a cluster's provider or region that a change moves from was to now.
func moved(was, now string) string {
	return "cannot change from " + was + " to " + now + "; a cluster moves by creating a new one"
}

// TestReplaceKeepsTheCloud refuses a Cluster on another provider or in another region: the cloud objects of the
// cluster would stay behind where tent made them. The other fields still change.
func TestReplaceKeepsTheCloud(t *testing.T) {
	hetzner := edit(t, edit(t, clusterYAML, "provider: vultr", "provider: hetzner"), "region: ams",
		"region: eu-central\n    zones: [fsn1, nbg1, hel1]")
	for _, tc := range []struct {
		name string
		doc  string
		want []v1alpha1.FieldError
	}{
		{"provider and region", hetzner, []v1alpha1.FieldError{
			{Object: "Cluster prod", Path: "spec.cloud.provider", Detail: moved("vultr", "hetzner")},
			{Object: "Cluster prod", Path: "spec.cloud.region", Detail: moved("ams", "eu-central")},
		}},
		{"region", edit(t, clusterYAML, "region: ams", "region: fra"), []v1alpha1.FieldError{
			{Object: "Cluster prod", Path: "spec.cloud.region", Detail: moved("ams", "fra")},
		}},
		{"region with another problem", edit(t, clusterYAML, "region: ams", "region: fra\n  channel: Beta"),
			[]v1alpha1.FieldError{
				{Object: "Cluster prod", Path: "spec.cloud.region", Detail: moved("ams", "fra")},
				{Object: "Cluster prod", Path: "spec.channel", Detail: `must match ^[a-z][a-z0-9-]*$`},
			}},
		// An empty value is left to validation.
		{"no region", edit(t, clusterYAML, "    region: ams\n", ""), []v1alpha1.FieldError{
			{Object: "Cluster prod", Path: "spec.cloud.region", Detail: "required"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			stored := snapshot(t, svc.Store)
			for _, apply := range []bool{false, true} {
				_, err := svc.Replace(t.Context(), decode(t, tc.doc), apply)
				wantFieldErrors(t, err, tc.want...)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}

	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	narrow := edit(t, clusterYAML, "region: ams", "region: ams\n  access:\n    api: [203.0.113.0/24]")
	changes, err := svc.Replace(t.Context(), decode(t, narrow), true)
	if err != nil {
		t.Fatalf("Replace of another field: %v", err)
	}
	wantChanges(t, changes, cluster(app.Replaced))
	wantStored(t, svc.Store, clusterPath, encode(t, narrow))
}

// brokenCluster is the error of the test cluster's cluster.yaml when it holds "kind: Cluster" alone: the decode error,
// then how to repair the file.
const brokenCluster = clusterPath + ": document 1 (Cluster): apiVersion is required; fix " + clusterPath +
	" in the state store by hand and keep its spec.cloud.provider and spec.cloud.region, since deleting the file " +
	"would leave the cloud objects of the cluster behind"

// breakCluster stores a cluster.yaml of the test cluster that does not decode.
func breakCluster(t *testing.T, svc *app.Service) {
	t.Helper()
	put(t, svc.Store, clusterPath, []byte("kind: Cluster\n"))
}

// wantBroken fails the test unless err is brokenCluster, which wraps the decode error.
func wantBroken(t *testing.T, err error) {
	t.Helper()
	wantError(t, err, brokenCluster)
	if cause := errors.Unwrap(err); cause == nil || cause.Error() != "document 1 (Cluster): apiVersion is required" {
		t.Errorf("errors.Unwrap(%v) = %v, want the decode error", err, cause)
	}
}

// TestReplaceKeepsTheCloudOfABrokenStore refuses to replace a stored cluster.yaml that does not decode: its cloud is
// unknown, so the change could move the cluster. The error says how to repair the file.
func TestReplaceKeepsTheCloudOfABrokenStore(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	breakCluster(t, svc)
	_, err := svc.Replace(t.Context(), decode(t, clusterYAML), true)
	wantBroken(t, err)
	wantStored(t, svc.Store, clusterPath, []byte("kind: Cluster\n"))
}

// TestBrokenClusterSaysHowToRepairIt says how to repair a stored cluster.yaml that does not decode, whichever use case
// reads it, and changes nothing.
func TestBrokenClusterSaysHowToRepairIt(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	breakCluster(t, svc)
	svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
		t.Error("a use case looked up a provider")
		return nil, errors.New("no provider")
	}
	stored := snapshot(t, svc.Store)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"Load", func() error { _, _, err := svc.Load(t.Context(), "prod", v1alpha1.KindCluster, ""); return err }},
		{"Get", func() error { _, err := svc.Get(t.Context(), "prod", false); return err }},
		{"Update", func() error { _, err := svc.Update(t.Context(), "prod", true); return err }},
		{"DeleteCluster plan", func() error {
			_, err := svc.DeleteCluster(t.Context(), "prod", false, false)
			return err
		}},
		{"DeleteCluster", func() error { _, err := svc.DeleteCluster(t.Context(), "prod", true, false); return err }},
		{"DeleteCluster with force", func() error {
			_, err := svc.DeleteCluster(t.Context(), "prod", true, true)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantBroken(t, tc.run())
			wantSnapshot(t, svc.Store, stored)
		})
	}
}
