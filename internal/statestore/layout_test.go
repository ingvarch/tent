package statestore_test

import (
	"strings"
	"testing"

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
			mustNameIndex(t, l, "servers")},
		l.Secrets()...)
	for _, p := range objects {
		mustPut(t, s, p)
	}
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{l.Prefix(), []string{"prod/cluster.completed.yaml", "prod/cluster.yaml", "prod/lock",
			"prod/names/servers", "prod/nodegroups/workers.yaml", "prod/pki/ca-bundle.pem", "prod/pki/private/ca.key",
			"prod/secrets/acl-bootstrap-token", "prod/secrets/gossip.key", "prod/tent-version"}},
		{l.NodeGroups(), []string{"prod/nodegroups/workers.yaml"}},
		{l.NameIndexes(), []string{"prod/names/servers"}},
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
