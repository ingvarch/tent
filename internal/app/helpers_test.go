package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// The objects of the test cluster prod, with every defaulted field left out.
const (
	clusterYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: vultr
    region: ams
`
	serversYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  size: 3
`
	workersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  size: 2
`
)

// Store paths of the test cluster.
const (
	clusterPath = "prod/cluster.yaml"
	serversPath = "prod/nodegroups/servers.yaml"
	workersPath = "prod/nodegroups/workers.yaml"
	versionPath = "prod/tent-version"
	lockPath    = "prod/lock"
)

// decode reads YAML documents into objects.
func decode(t *testing.T, docs ...string) spec.Objects {
	t.Helper()
	objs, err := spec.Decode([]byte(strings.Join(docs, "---\n")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return objs
}

// edit returns a YAML document with old replaced by with.
func edit(t *testing.T, doc, old, with string) string {
	t.Helper()
	if !strings.Contains(doc, old) {
		t.Fatalf("%q is not in\n%s", old, doc)
	}
	return strings.Replace(doc, old, with, 1)
}

// encode returns what the store keeps for one object: spec.Encode of the object alone.
func encode(t *testing.T, doc string) []byte {
	t.Helper()
	data, err := spec.Encode(decode(t, doc))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

// fileURL returns the file:// URL of an absolute directory: file:///srv/state, or file:///C:/state on Windows.
func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// newService returns a service over a new file store and the store's root directory, which does not exist yet.
func newService(t *testing.T) (*app.Service, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	s, err := statestore.Open(t.Context(), fileURL(root))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return &app.Service{Store: s, Version: "dev"}, root
}

// mustCreate creates objects and fails the test on an error.
func mustCreate(t *testing.T, svc *app.Service, docs ...string) {
	t.Helper()
	if _, err := svc.Create(t.Context(), decode(t, docs...), true); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func put(t *testing.T, s statestore.Store, p string, data []byte) {
	t.Helper()
	if _, err := s.Put(t.Context(), p, data, statestore.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", p, err)
	}
}

func get(t *testing.T, s statestore.Store, p string) []byte {
	t.Helper()
	data, _, err := s.Get(t.Context(), p)
	if err != nil {
		t.Fatalf("get %s: %v", p, err)
	}
	return data
}

func list(t *testing.T, s statestore.Store, prefix string) []string {
	t.Helper()
	paths, err := s.List(t.Context(), prefix)
	if err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	return paths
}

// wantStored fails the test unless the store holds data at p.
func wantStored(t *testing.T, s statestore.Store, p string, data []byte) {
	t.Helper()
	if diff := cmp.Diff(string(data), string(get(t, s, p))); diff != "" {
		t.Errorf("%s (-want +got):\n%s", p, diff)
	}
}

// wantPaths fails the test unless the store holds exactly these objects.
func wantPaths(t *testing.T, s statestore.Store, want ...string) {
	t.Helper()
	if diff := cmp.Diff(want, list(t, s, "")); diff != "" {
		t.Errorf("objects in the store (-want +got):\n%s", diff)
	}
}

// snapshot returns every object in the store with its content.
func snapshot(t *testing.T, s statestore.Store) map[string]string {
	t.Helper()
	objs := map[string]string{}
	for _, p := range list(t, s, "") {
		objs[p] = string(get(t, s, p))
	}
	return objs
}

// wantSnapshot fails the test unless the store holds exactly the objects of want.
func wantSnapshot(t *testing.T, s statestore.Store, want map[string]string) {
	t.Helper()
	if diff := cmp.Diff(want, snapshot(t, s)); diff != "" {
		t.Errorf("the store (-want +got):\n%s", diff)
	}
}

// wantNothingWritten fails the test unless the store's root was never created: nothing was written, no lock taken.
func wantNothingWritten(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the store was written to: stat %s: %v", root, err)
	}
}

func wantChanges(t *testing.T, got []app.Change, want ...app.Change) {
	t.Helper()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
}

func wantError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Errorf("error = %v\nwant    %s", err, want)
	}
}

// wantFieldErrors fails the test unless err is exactly these validation errors.
func wantFieldErrors(t *testing.T, err error, want ...v1alpha1.FieldError) {
	t.Helper()
	got, ok := errors.AsType[v1alpha1.Errors](err)
	if !ok {
		t.Fatalf("error = %v, want v1alpha1.Errors", err)
	}
	if diff := cmp.Diff(v1alpha1.Errors(want), got); diff != "" {
		t.Errorf("validation errors (-want +got):\n%s", diff)
	}
	if err.Error() != got.Error() {
		t.Errorf("error = %q, want the validation errors alone: %q", err, got)
	}
}

func cluster(action app.Action) app.Change {
	return app.Change{Kind: v1alpha1.KindCluster, Name: "prod", Action: action}
}

func group(name string, action app.Action) app.Change {
	return app.Change{Kind: v1alpha1.KindNodeGroup, Name: name, Action: action}
}

// notFound is the error for an object missing from the store.
func notFound(svc *app.Service, what string) string {
	return fmt.Sprintf("%s not found in %s", what, svc.Store.String())
}

// leaseJSON encodes a lease as the lock mechanisms store it.
func leaseJSON(t *testing.T, l statestore.Lease) []byte {
	t.Helper()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// flockLease returns the file that holds the lease of the test cluster's flock in a file store.
func flockLease(root string) string { return filepath.Join(root, ".tent-locks", "prod.lease") }

// leaveFlockLease leaves the lease of a tent that died holding the test cluster's flock, which the OS released, and
// returns it. Its expiry is in the future: under flock, only the free lock shows that its holder is gone.
func leaveFlockLease(t *testing.T, root string) statestore.Lease {
	t.Helper()
	dead := otherLease(time.Now().Add(time.Hour).UTC())
	if err := os.MkdirAll(filepath.Dir(flockLease(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, data := range map[string][]byte{
		filepath.Join(root, ".tent-locks", "prod.lock"): nil,
		flockLease(root): leaseJSON(t, dead),
	} {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dead
}

// unwrapped hides the file store from statestore.NewLocker, which then locks with a lease in the store: the store
// enforces conditional puts.
type unwrapped struct{ statestore.Store }

// noConditions is a store without conditional puts: statestore.NewLocker gives it a best-effort lock.
type noConditions struct{ statestore.Store }

func (noConditions) Capabilities(context.Context) (statestore.Capabilities, error) {
	return statestore.Capabilities{}, nil
}

func (s noConditions) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if opts.IfNoneMatch || opts.IfMatch != "" {
		return "", fmt.Errorf("put %q: conditional puts: %w", p, errors.ErrUnsupported)
	}
	return s.Store.Put(ctx, p, data, opts)
}
