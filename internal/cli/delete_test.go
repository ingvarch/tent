package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

func TestDeleteClusterPreview(t *testing.T) {
	s := withCluster(t)
	const want = "would delete:\n" +
		"  prod/nodegroups/servers.yaml\n" +
		"  prod/nodegroups/workers.yaml\n" +
		"  prod/cluster.yaml\n" +
		"run with --yes to delete them\n"
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "--state", s.url), want)
	wantOK(t, runIn(t, "", "delete", "cluster", "--name", "prod", "--state", s.url), want)
	s.want(t, prodObjects)
}

func TestDeleteCluster(t *testing.T) {
	s := withCluster(t)
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "--yes", "--state", s.url),
		"deleted prod/nodegroups/servers.yaml\ndeleted prod/nodegroups/workers.yaml\ndeleted prod/cluster.yaml\n")
	s.want(t, map[string]string{})
}

// TestDeleteClusterJSON tells the preview from the deletion.
func TestDeleteClusterJSON(t *testing.T) {
	s := withCluster(t)
	want := func(applied string) string {
		return `{
  "cluster": "prod",
  "applied": ` + applied + `,
  "paths": [
    "prod/nodegroups/servers.yaml",
    "prod/nodegroups/workers.yaml",
    "prod/cluster.yaml"
  ]
}
`
	}
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "-o", "json", "--state", s.url), want("false"))
	s.want(t, prodObjects)
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "-o", "json", "--yes", "--state", s.url), want("true"))
	s.want(t, map[string]string{})
}

func TestDeleteClusterUnknownObjects(t *testing.T) {
	s := withCluster(t)
	s.put(t, "prod/notes.txt", "mine")
	const refused = "Error: cluster prod holds objects tent does not know: prod/notes.txt; delete them yourself or " +
		"use --force\n"
	wantError(t, runIn(t, "", "delete", "cluster", "prod", "--state", s.url), refused)
	wantError(t, runIn(t, "", "delete", "cluster", "prod", "--yes", "--state", s.url), refused)
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "--force", "--state", s.url), "would delete:\n"+
		"  prod/nodegroups/servers.yaml\n"+
		"  prod/nodegroups/workers.yaml\n"+
		"  prod/notes.txt\n"+
		"  prod/cluster.yaml\n"+
		"run with --yes --force to delete them\n")
	s.want(t, map[string]string{
		clusterPath: clusterYAML, serversPath: serversYAML, workersPath: workersYAML, "prod/notes.txt": "mine",
	})
	wantOK(t, runIn(t, "", "delete", "cluster", "prod", "--yes", "--force", "--state", s.url),
		"deleted prod/nodegroups/servers.yaml\ndeleted prod/nodegroups/workers.yaml\ndeleted prod/notes.txt\n"+
			"deleted prod/cluster.yaml\n")
	s.want(t, map[string]string{})
}

func TestDeleteClusterMissing(t *testing.T) {
	s := withCluster(t)
	wantError(t, runIn(t, "", "delete", "cluster", "dev", "--yes", "--state", s.url),
		"Error: cluster dev not found in "+s.url+"\n")
	s.want(t, prodObjects)
}

func TestDeleteClusterHelpSaysItKeepsTheCloud(t *testing.T) {
	got := runIn(t, "", "delete", "cluster", "--help")
	const want = "In this version it deletes only the state, not the cloud resources."
	if got.code != 0 || !strings.Contains(got.out, want) {
		t.Errorf("exit code = %d, stdout\n%s\nwant 0 and it to hold %q", got.code, got.out, want)
	}
}

// losingStore removes the lock's lease after it deletes one path, as state unlock --force by someone else would.
type losingStore struct {
	statestore.Store
	after string
}

func (s losingStore) Delete(ctx context.Context, p string) error {
	err := s.Store.Delete(ctx, p)
	if err == nil && p == s.after {
		err = s.Store.Delete(ctx, "prod/lock")
	}
	return err
}

// TestDeleteClusterLosesItsLock prints what it deleted, and fails.
func TestDeleteClusterLosesItsLock(t *testing.T) {
	s := withCluster(t)
	losing := func(st statestore.Store) statestore.Store { return losingStore{Store: st, after: clusterPath} }
	wantResult(t, runWithStore(t, losing, "delete", "cluster", "prod", "--yes", "--state", s.url), 1,
		"deleted prod/nodegroups/servers.yaml\ndeleted prod/nodegroups/workers.yaml\ndeleted prod/cluster.yaml\n",
		"Error: the change is saved, but the lock of cluster prod was lost before tent released it\n")
	s.want(t, map[string]string{})
}
