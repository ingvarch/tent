package statestore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
)

func mustLayout(t *testing.T, cluster string) statestore.Layout {
	t.Helper()
	l, err := statestore.NewLayout(cluster)
	if err != nil {
		t.Fatalf("NewLayout(%q): %v", cluster, err)
	}
	return l
}

func mustNodeGroup(t *testing.T, l statestore.Layout, name string) string {
	t.Helper()
	p, err := l.NodeGroup(name)
	if err != nil {
		t.Fatalf("NodeGroup(%q): %v", name, err)
	}
	return p
}

func mustNameIndex(t *testing.T, l statestore.Layout, group string) string {
	t.Helper()
	p, err := l.NameIndex(group)
	if err != nil {
		t.Fatalf("NameIndex(%q): %v", group, err)
	}
	return p
}

func TestLayoutPaths(t *testing.T) {
	l := mustLayout(t, "prod")
	for _, tc := range []struct{ name, got, want string }{
		{"Cluster", l.Cluster(), "prod"},
		{"Prefix", l.Prefix(), "prod/"},
		{"TentVersion", l.TentVersion(), "prod/tent-version"},
		{"ClusterSpec", l.ClusterSpec(), "prod/cluster.yaml"},
		{"NodeGroup", mustNodeGroup(t, l, "workers"), "prod/nodegroups/workers.yaml"},
		{"NodeGroups", l.NodeGroups(), "prod/nodegroups/"},
		{"NameIndexes", l.NameIndexes(), "prod/names/"},
		{"NameIndex", mustNameIndex(t, l, "servers"), "prod/names/servers"},
		{"Completed", l.Completed(), "prod/cluster.completed.yaml"},
		{"Lock", l.Lock(), "prod/lock"},
		{"CAKey", l.CAKey(), "prod/pki/private/ca.key"},
		{"CABundle", l.CABundle(), "prod/pki/ca-bundle.pem"},
		{"GossipKey", l.GossipKey(), "prod/secrets/gossip.key"},
		{"ACLBootstrapSecret", l.ACLBootstrapSecret(), "prod/secrets/acl-bootstrap-token"},
		{"NomadBootstrapped", l.NomadBootstrapped(), "prod/nomad/bootstrapped"},
		{"NomadRestored", l.NomadRestored(), "prod/nomad/restored"},
		{"Backups", l.Backups(), "prod/backups/"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s() = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestLayoutSecrets checks that Secrets lists the PKI and secret objects in the order they are written.
func TestLayoutSecrets(t *testing.T) {
	l := mustLayout(t, "prod")
	want := []string{
		"prod/pki/private/ca.key", "prod/pki/ca-bundle.pem", "prod/secrets/gossip.key",
		"prod/secrets/acl-bootstrap-token",
	}
	if diff := cmp.Diff(want, l.Secrets()); diff != "" {
		t.Errorf("Secrets() (-want +got):\n%s", diff)
	}
	// Each call returns a new slice, so a caller cannot change the next one.
	l.Secrets()[0] = "changed"
	if diff := cmp.Diff(want, l.Secrets()); diff != "" {
		t.Errorf("Secrets() after a caller changed an earlier result (-want +got):\n%s", diff)
	}
}

// TestLayoutPathsInStore checks that the store accepts the paths of a layout and finds them under its prefixes.
func TestLayoutPathsInStore(t *testing.T) {
	s := openFile(t, t.TempDir())
	l := mustLayout(t, "prod")
	objects := append(
		[]string{l.TentVersion(), l.ClusterSpec(), mustNodeGroup(t, l, "workers"), l.Completed(), l.Lock(),
			mustNameIndex(t, l, "servers"), l.NomadRestored(), l.Backup(backupTaken, backupBootstrap).Path},
		l.Secrets()...)
	for _, p := range objects {
		mustPut(t, s, p)
	}
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{l.Prefix(), []string{backupPath, "prod/cluster.completed.yaml", "prod/cluster.yaml", "prod/lock",
			"prod/names/servers", "prod/nodegroups/workers.yaml", "prod/nomad/restored", "prod/pki/ca-bundle.pem",
			"prod/pki/private/ca.key", "prod/secrets/acl-bootstrap-token", "prod/secrets/gossip.key",
			"prod/tent-version"}},
		{l.NodeGroups(), []string{"prod/nodegroups/workers.yaml"}},
		{l.NameIndexes(), []string{"prod/names/servers"}},
		{l.Backups(), []string{backupPath}},
	} {
		got, err := s.List(t.Context(), tc.prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", tc.prefix, err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("List(%q) (-want +got):\n%s", tc.prefix, diff)
		}
	}
}

func TestNewLayoutRejects(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", "invalid cluster name: empty"},
		{"a/b", `invalid cluster name: segment "a/b": use only`},
		{"..", `invalid cluster name: segment "..": paths have no . or .. segments`},
		{".x", `invalid cluster name: segment ".x" starts with '.'`},
		{"nul", `invalid cluster name: segment "nul" is a Windows device name`},
	} {
		l, err := statestore.NewLayout(tc.name)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("NewLayout(%q) = %#v, %v; want an error starting with %s", tc.name, l, err, tc.want)
		}
	}
}

func TestNodeGroupRejects(t *testing.T) {
	l := mustLayout(t, "prod")
	funcs := []struct {
		name string
		path func(string) (string, error)
	}{{"NodeGroup", l.NodeGroup}, {"NameIndex", l.NameIndex}}
	for _, tc := range []struct{ name, want string }{
		{"", "invalid node group name: empty"},
		{"a/b", `invalid node group name: segment "a/b": use only`},
		{"..", `invalid node group name: segment "..": paths have no . or .. segments`},
		{".x", `invalid node group name: segment ".x" starts with '.'`},
		{"nul", `invalid node group name: segment "nul" is a Windows device name`},
	} {
		for _, f := range funcs {
			p, err := f.path(tc.name)
			if err == nil || p != "" || !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("%s(%q) = %q, %v; want an error starting with %s", f.name, tc.name, p, err, tc.want)
			}
		}
	}
}

func TestClusters(t *testing.T) {
	s := openFile(t, t.TempDir())
	for _, p := range []string{
		"prod/cluster.yaml",
		"prod/nodegroups/workers.yaml",
		"prod/tent-version",
		"prod-eu/cluster.yaml",    // listed before prod/cluster.yaml: '-' sorts before '/'
		"cluster.yaml",            // a stray top-level file
		"staging/tent-version",    // a directory without cluster.yaml
		"old/backup/cluster.yaml", // not at the top level
	} {
		mustPut(t, s, p)
	}
	got, err := statestore.Clusters(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"prod", "prod-eu"}, got); diff != "" {
		t.Errorf("Clusters() (-want +got):\n%s", diff)
	}
}

func TestClustersEmptyStore(t *testing.T) {
	got, err := statestore.Clusters(t.Context(), openFile(t, t.TempDir()))
	if err != nil || len(got) != 0 {
		t.Errorf("Clusters() = %q, %v; want none", got, err)
	}
}

var (
	backupTaken     = time.Date(2026, 10, 9, 23, 2, 7, 0, time.UTC)
	backupBootstrap = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
)

const backupPath = "prod/backups/20261001T100000Z/20261009T230207Z.snap"

func TestBackupName(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	local := time.Date(2026, 10, 10, 2, 2, 7, 999_999_999, zone)
	if got, want := statestore.BackupName(local), "20261009T230207Z"; got != want {
		t.Errorf("BackupName(%v) = %q, want %q", local, got, want)
	}
}

func TestLayoutBackup(t *testing.T) {
	l := mustLayout(t, "prod")
	taken := time.Date(2026, 10, 10, 2, 2, 7, 500, time.FixedZone("UTC+3", 3*60*60))
	want := statestore.Backup{
		Name: "20261009T230207Z", Time: backupTaken, Bootstrap: "20261001T100000Z", Path: backupPath,
	}
	got := l.Backup(taken, backupBootstrap)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Backup() (-want +got):\n%s", diff)
	}
	if got.Time.Location() != time.UTC {
		t.Errorf("Backup().Time is in %v, want UTC", got.Time.Location())
	}
}

func TestLayoutBackupAt(t *testing.T) {
	l := mustLayout(t, "prod")
	want := l.Backup(backupTaken, backupBootstrap)
	if got, ok := l.BackupAt(backupPath); !ok || cmp.Diff(want, got) != "" {
		t.Errorf("BackupAt(%q) = %+v, %v; want %+v, true", backupPath, got, ok, want)
	}
	for _, p := range []string{
		"prod-2/backups/20261001T100000Z/20261009T230207Z.snap",
		"other/backups/20261001T100000Z/20261009T230207Z.snap",
		"20261001T100000Z/20261009T230207Z.snap",
		"prod/backups/x.snap",
		"prod/backups/20261001T100000Z/20261009T230207Z",
		"prod/backups/20261001t100000Z/20261009T230207Z.snap",
		"prod/backups/20261001T100000z/20261009T230207Z.snap",
		"prod/backups/20261001T100000Z/20261009t230207Z.snap",
		"prod/backups/20261001T100000Z/20261009T230207z.snap",
		"prod/backups/2026-10-01T10:00:00Z/20261009T230207Z.snap",
		"prod/backups/20261001T100000Z/2026-10-09T23:02:07Z.snap",
		"prod/backups/20261001T100000Z/20261009T230207.5Z.snap",
		"prod/backups/20261001T100000.5Z/20261009T230207Z.snap",
		"prod/backups/20261301T100000Z/20261009T230207Z.snap",
		"prod/backups/20261001T100000Z/20261309T230207Z.snap",
		"prod/backups/20261001T100000Z/extra/20261009T230207Z.snap",
		"prod/backups/20261001T100000Z/20261009T230207Z.snap.bak",
		"prod/backups/20261001T100000Z/20261009T230207Z.snap/",
		"prod/backups/20261001T100000Z/.snap",
		"prod/backups/20261001T100000Z/20261009T230207.snap",
		"prod/backups/20261001T100000Z/20261009T230207Z.SNAP",
		"prod/backups/20261001T100000/20261009T230207Z.snap",
		"prod/backups//20261009T230207Z.snap",
		"prod/backups/20261001T100000Z",
		"prod/backups/",
		"prod/nomad/restored",
		"",
	} {
		if got, ok := l.BackupAt(p); ok {
			t.Errorf("BackupAt(%q) = %+v, true; want false", p, got)
		}
	}
}

// listStore is a store whose List returns paths and err, in the order given.
type listStore struct {
	statestore.Store
	paths []string
	err   error
}

func (s listStore) List(context.Context, string) ([]string, error) { return s.paths, s.err }

func TestBackups(t *testing.T) {
	l := mustLayout(t, "prod")
	at := func(h int) time.Time { return time.Date(2026, 10, 9, h, 0, 0, 0, time.UTC) }
	boot1, boot2 := at(1), at(2)
	early, late := l.Backup(at(5), boot2), l.Backup(at(6), boot1)
	tie1, tie2 := l.Backup(at(7), boot1), l.Backup(at(7), boot2)
	s := openFile(t, t.TempDir())
	for _, p := range []string{
		tie2.Path, late.Path, tie1.Path, early.Path,
		"prod/backups/notes.txt",
		"prod/backups/20261001T100000Z/x.snap",
		"prod-2/backups/20261001T100000Z/20261009T230207Z.snap",
		l.NomadRestored(),
	} {
		mustPut(t, s, p)
	}
	got, err := statestore.Backups(t.Context(), s, l)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]statestore.Backup{early, late, tie1, tie2}, got); diff != "" {
		t.Errorf("Backups() (-want +got):\n%s", diff)
	}
}

// TestBackupsOrdersOneTimeByBootstrap checks that Backups orders the backups of one time by their bootstrap when the
// store lists them in another order.
func TestBackupsOrdersOneTimeByBootstrap(t *testing.T) {
	l := mustLayout(t, "prod")
	at := time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC)
	first, second := l.Backup(at, at.Add(-2*time.Hour)), l.Backup(at, at.Add(-time.Hour))
	got, err := statestore.Backups(t.Context(), listStore{paths: []string{second.Path, first.Path}}, l)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]statestore.Backup{first, second}, got); diff != "" {
		t.Errorf("Backups() (-want +got):\n%s", diff)
	}
}

func TestBackupsEmptyStore(t *testing.T) {
	got, err := statestore.Backups(t.Context(), openFile(t, t.TempDir()), mustLayout(t, "prod"))
	if err != nil || len(got) != 0 {
		t.Errorf("Backups() = %+v, %v; want none", got, err)
	}
}

func TestBackupsListError(t *testing.T) {
	broken := errors.New("disk on fire")
	s := listStore{err: broken}
	got, err := statestore.Backups(t.Context(), s, mustLayout(t, "prod"))
	if !errors.Is(err, broken) || got != nil ||
		!strings.HasPrefix(err.Error(), "list the backups of cluster prod: ") {
		t.Errorf("Backups() = %+v, %v; want a wrapped error starting with %q", got, err,
			"list the backups of cluster prod: ")
	}
}
