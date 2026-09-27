package app_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
)

const completedPath = "prod/cluster.completed.yaml"

// newCluster returns a service over a store with the test cluster, its completed spec and its tent version.
func newCluster(t *testing.T) *app.Service {
	t.Helper()
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	put(t, svc.Store, completedPath, []byte("completed"))
	put(t, svc.Store, versionPath, []byte("v0.4.0\n"))
	return svc
}

// allState is every object of the test cluster, in the order DeleteState deletes them.
var allState = []string{completedPath, serversPath, workersPath, clusterPath, versionPath}

func wantDeleted(t *testing.T, got []string, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("DeleteState: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DeleteState (-want +got):\n%s", diff)
	}
}

func TestDeleteStatePlan(t *testing.T) {
	svc := newCluster(t)
	put(t, svc.Store, lockPath, []byte("{}")) // the lock's lease goes when the lock is released
	paths, err := svc.DeleteState(t.Context(), "prod", false, false)
	wantDeleted(t, paths, err, allState...)
	wantPaths(t, svc.Store, completedPath, clusterPath, lockPath, serversPath, workersPath, versionPath)
}

func TestDeleteState(t *testing.T) {
	svc := newCluster(t)
	paths, err := svc.DeleteState(t.Context(), "prod", true, false)
	wantDeleted(t, paths, err, allState...)
	wantPaths(t, svc.Store)
}

// TestDeleteStateAgain runs a delete again that stopped before the Cluster's spec and the tent version.
func TestDeleteStateAgain(t *testing.T) {
	svc := newCluster(t)
	for _, p := range []string{completedPath, serversPath, workersPath} {
		if err := svc.Store.Delete(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := svc.DeleteState(t.Context(), "prod", true, false)
	wantDeleted(t, paths, err, clusterPath, versionPath)
	wantPaths(t, svc.Store)
}

func TestDeleteStateUnknown(t *testing.T) {
	unknown := []string{"prod/nodegroups/old/x.yaml", "prod/notes.txt", "prod/pki/ca.pem"}
	svc := newCluster(t)
	for _, p := range unknown {
		put(t, svc.Store, p, []byte("?"))
	}
	stored := list(t, svc.Store, "")
	for _, apply := range []bool{false, true} {
		_, err := svc.DeleteState(t.Context(), "prod", apply, false)
		wantError(t, err, "cluster prod holds objects tent does not know: prod/nodegroups/old/x.yaml, prod/notes.txt, "+
			"prod/pki/ca.pem; delete them yourself or use --force")
		wantPaths(t, svc.Store, stored...)
	}
	want := []string{
		completedPath, unknown[0], serversPath, workersPath, unknown[1], unknown[2], clusterPath, versionPath,
	}
	paths, err := svc.DeleteState(t.Context(), "prod", false, true)
	wantDeleted(t, paths, err, want...)
	paths, err = svc.DeleteState(t.Context(), "prod", true, true)
	wantDeleted(t, paths, err, want...)
	wantPaths(t, svc.Store)
}

func TestDeleteStateMissing(t *testing.T) {
	svc := newCluster(t)
	for _, apply := range []bool{false, true} {
		_, err := svc.DeleteState(t.Context(), "dev", apply, false)
		wantError(t, err, notFound(svc, "cluster dev"))
	}
	wantPaths(t, svc.Store, completedPath, clusterPath, serversPath, workersPath, versionPath)
}
