package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/statestore"
)

// nameRule is what is wrong with a name such as PROD.
const nameRule = "must be 2 to 20 lowercase letters, digits or dashes, starting with a letter and ending with a " +
	"letter or digit"

// reads records the paths and prefixes a store is read at.
type reads struct {
	statestore.Store
	paths []string
}

func (r *reads) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	r.paths = append(r.paths, p)
	return r.Store.Get(ctx, p)
}

func (r *reads) List(ctx context.Context, prefix string) ([]string, error) {
	r.paths = append(r.paths, prefix)
	return r.Store.List(ctx, prefix)
}

// TestNamesAreChecked refuses names outside the name rule before it reads the store under them. On a disk that
// ignores case, PROD would name prod's state.
func TestNamesAreChecked(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	store := svc.Store
	stored := snapshot(t, store)
	const cluster = `invalid cluster name "PROD": ` + nameRule
	upper := decode(t, strings.ReplaceAll(strings.Join([]string{clusterYAML, serversYAML, workersYAML}, "---\n"),
		"prod", "PROD"))
	const upperSpec = "Cluster PROD: metadata.name: " + nameRule
	mixed := decode(t, strings.ReplaceAll(strings.Join([]string{
		clusterYAML, edit(t, serversYAML, "size: 3", "size: 2"), edit(t, workersYAML, "name: workers", "name: ../y"),
	}, "---\n"), "prod", "PROD"))
	for _, tc := range []struct {
		name string
		run  func() error
		want string
		spec bool // the name is from a spec: the error is v1alpha1.Errors
	}{
		{"Get", func() error { _, err := svc.Get(t.Context(), "PROD", false); return err }, cluster, false},
		{"DeleteCluster plan", func() error {
			_, err := svc.DeleteCluster(t.Context(), "PROD", false, false)
			return err
		}, cluster, false},
		{"DeleteCluster", func() error {
			_, err := svc.DeleteCluster(t.Context(), "PROD", true, true)
			return err
		}, cluster, false},
		{"Update plan", func() error { _, err := svc.Update(t.Context(), "PROD", false); return err }, cluster, false},
		{"Update", func() error { _, err := svc.Update(t.Context(), "PROD", true); return err }, cluster, false},
		{"Unlock", func() error { _, err := svc.Unlock(t.Context(), "PROD", true); return err }, cluster, false},
		{"Load cluster", func() error {
			_, _, err := svc.Load(t.Context(), "PROD", v1alpha1.KindCluster, "")
			return err
		}, cluster, false},
		{"Load node group", func() error {
			_, _, err := svc.Load(t.Context(), "prod", v1alpha1.KindNodeGroup, "Workers")
			return err
		}, `invalid node group name "Workers": ` + nameRule, false},
		{"Create", func() error { _, err := svc.Create(t.Context(), upper, true); return err }, upperSpec, true},
		{"Create dry run", func() error { _, err := svc.Create(t.Context(), upper, false); return err }, upperSpec,
			true},
		{"Replace", func() error { _, err := svc.Replace(t.Context(), upper, true); return err }, upperSpec, true},
		// With a Cluster the input's own problems show, not only the names.
		{"Create with other problems", func() error {
			_, err := svc.Create(t.Context(), mixed, true)
			return err
		}, upperSpec + "\nNodeGroup ../y: metadata.name: " + nameRule +
			"\nNodeGroup servers: spec.size: must be 1, 3 or 5 for role=server", true},
		// Node groups alone are checked against the stored Cluster, so only their names show.
		{"Create node group", func() error {
			y := edit(t, edit(t, workersYAML, "name: workers", "name: ../y"), "size: 2", "size: -1")
			_, err := svc.Create(t.Context(), decode(t, y), true)
			return err
		}, "NodeGroup ../y: metadata.name: " + nameRule, true},
		{"Replace node group of cluster", func() error {
			_, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "cluster: prod", "cluster: ../x")), true)
			return err
		}, "NodeGroup workers: metadata.cluster: " + nameRule, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &reads{Store: store}
			svc.Store = r
			err := tc.run()
			svc.Store = store
			wantError(t, err, tc.want)
			if _, ok := errors.AsType[v1alpha1.Errors](err); ok != tc.spec {
				t.Errorf("error %v is v1alpha1.Errors: %t, want %t", err, ok, tc.spec)
			}
			if len(r.paths) > 0 {
				t.Errorf("read the store at %q before checking the names", r.paths)
			}
			wantSnapshot(t, store, stored)
		})
	}
}
