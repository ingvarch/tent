package app_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func TestCreateCluster(t *testing.T) {
	svc, _ := newService(t)
	objs := decode(t, workersYAML, clusterYAML, serversYAML)
	changes, err := svc.Create(t.Context(), objs, true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantChanges(t, changes, cluster(app.Created), group("servers", app.Created), group("workers", app.Created))
	// The user specs as given, one object each, without defaults; a development build records no tent version.
	wantPaths(t, svc.Store, clusterPath, serversPath, workersPath)
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
	wantStored(t, svc.Store, serversPath, encode(t, serversYAML))
	wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
	if diff := cmp.Diff(decode(t, workersYAML, clusterYAML, serversYAML), objs); diff != "" {
		t.Errorf("Create changed its input (-want +got):\n%s", diff)
	}
}

func TestCreateNodeGroups(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	changes, err := svc.Create(t.Context(), decode(t, workersYAML), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantChanges(t, changes, group("workers", app.Created))
	wantPaths(t, svc.Store, clusterPath, serversPath, workersPath)
	wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
}

func TestCreateAgain(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	changes, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML, workersYAML), true)
	if err != nil {
		t.Fatalf("Create again: %v", err)
	}
	wantChanges(t, changes, cluster(app.Unchanged), group("servers", app.Unchanged), group("workers", app.Unchanged))
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
}

func TestCreateExisting(t *testing.T) {
	const hint = " already exists; change it with edit or replace -f"
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	for _, tc := range []struct {
		name string
		docs []string
		want string
	}{
		{"cluster", []string{edit(t, clusterYAML, "ams", "fra"), serversYAML}, "cluster prod" + hint},
		{"node group", []string{edit(t, workersYAML, "size: 2", "size: 4")},
			"node group workers of cluster prod" + hint},
		{"both", []string{edit(t, clusterYAML, "ams", "fra"), serversYAML, edit(t, workersYAML, "size: 2", "size: 4")},
			"cluster prod" + hint + "\nnode group workers of cluster prod" + hint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(t.Context(), decode(t, tc.docs...), true)
			wantError(t, err, tc.want)
			wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
			wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
		})
	}
}

// TestCreateInterrupted re-runs a create that stopped after writing the node groups: cluster.yaml comes last.
func TestCreateInterrupted(t *testing.T) {
	svc, _ := newService(t)
	put(t, svc.Store, serversPath, encode(t, serversYAML))
	put(t, svc.Store, workersPath, encode(t, workersYAML))
	changes, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML, workersYAML), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantChanges(t, changes, cluster(app.Created), group("servers", app.Unchanged), group("workers", app.Unchanged))
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
}

func TestCreateInvalid(t *testing.T) {
	svc, root := newService(t)
	_, err := svc.Create(t.Context(), decode(t, clusterYAML, edit(t, serversYAML, "size: 3", "size: 2")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server",
	})
	wantNothingWritten(t, root)
}

// TestCreateChecksTheWholeCluster adds a second server group to a cluster: the groups are valid alone, the cluster
// is not.
func TestCreateChecksTheWholeCluster(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML)
	_, err := svc.Create(t.Context(), decode(t, edit(t, serversYAML, "name: servers", "name: more")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "Cluster prod", Path: "nodeGroups",
		Detail: "need exactly one server or combined group, found 2: more, servers",
	})
	wantPaths(t, svc.Store, clusterPath, serversPath)
}

func TestCreateSingleServer(t *testing.T) {
	single := edit(t, serversYAML, "size: 3", "size: 1")
	svc, _ := newService(t)
	_, err := svc.Create(t.Context(), decode(t, clusterYAML, single), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "size 1 needs --allow-single-server",
	})
	svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
	if _, err := svc.Create(t.Context(), decode(t, clusterYAML, single), true); err != nil {
		t.Fatalf("Create with AllowSingleServer: %v", err)
	}
	wantStored(t, svc.Store, serversPath, encode(t, single))
}

func TestCreateNodeGroupsOfMissingCluster(t *testing.T) {
	svc, root := newService(t)
	_, err := svc.Create(t.Context(), decode(t, workersYAML), true)
	wantError(t, err, notFound(svc, "cluster prod"))
	wantNothingWritten(t, root)
}

func TestCreateBadInput(t *testing.T) {
	svc, root := newService(t)
	for _, tc := range []struct {
		name string
		docs []string
		want string
	}{
		{"nothing", nil, "the spec holds no Cluster and no NodeGroup"},
		{"groups of two clusters", []string{workersYAML, edit(t, serversYAML, "cluster: prod", "cluster: dev")},
			"the node groups belong to different clusters: dev, prod"},
		{"cluster name", []string{edit(t, clusterYAML, "name: prod", "name: con")},
			"Cluster con: metadata.name: must not be con: Windows reserves that name\n" +
				"Cluster con: nodeGroups: need exactly one server or combined group, found 0"},
		{"node group name", []string{clusterYAML, edit(t, serversYAML, "name: servers", `name: ""`)},
			"NodeGroup (no name): metadata.name: required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(t.Context(), decode(t, tc.docs...), true)
			wantError(t, err, tc.want)
		})
	}
	wantNothingWritten(t, root)
}

// TestCreateReplacesLeftovers runs a create again whose first run stopped after writing the node groups, with one
// group changed since: without cluster.yaml, the groups are leftovers of that run.
func TestCreateReplacesLeftovers(t *testing.T) {
	svc, _ := newService(t)
	put(t, svc.Store, serversPath, encode(t, serversYAML))
	put(t, svc.Store, workersPath, encode(t, edit(t, workersYAML, "size: 2", "size: 4")))
	changes, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML, workersYAML), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantChanges(t, changes, cluster(app.Created), group("servers", app.Unchanged), group("workers", app.Replaced))
	wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
	wantStored(t, svc.Store, clusterPath, encode(t, clusterYAML))
}

func TestCreateNodeGroupWithoutCluster(t *testing.T) {
	svc, root := newService(t)
	_, err := svc.Create(t.Context(), decode(t, edit(t, workersYAML, "  cluster: prod\n", "")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup workers", Path: "metadata.cluster", Detail: "required",
	})
	wantNothingWritten(t, root)
}

// TestCreateRefusesLeftoversMissingFromTheSpec runs a create again whose first run stopped after writing the node
// groups, from a spec that no longer has some of them.
func TestCreateRefusesLeftoversMissingFromTheSpec(t *testing.T) {
	const left = " is left from an interrupted create; add it to the spec or run delete cluster first"
	svc, _ := newService(t)
	put(t, svc.Store, serversPath, encode(t, serversYAML))
	put(t, svc.Store, workersPath, encode(t, workersYAML))
	put(t, svc.Store, "prod/nodegroups/web.yaml", encode(t, edit(t, workersYAML, "name: workers", "name: web")))
	stored := snapshot(t, svc.Store)
	for _, apply := range []bool{false, true} {
		_, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML), apply)
		wantError(t, err, "node group web of cluster prod"+left+"\nnode group workers of cluster prod"+left)
	}
	wantSnapshot(t, svc.Store, stored)
}
