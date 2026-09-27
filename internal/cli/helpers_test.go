package cli

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
)

// The test cluster prod as spec.Encode writes it: the Cluster, then its node groups by name.
const (
	clusterYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: vultr
    region: ams
    vultr: {}
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
  size: 3
`
)

// Store paths of the test cluster.
const (
	clusterPath = "prod/cluster.yaml"
	serversPath = "prod/nodegroups/servers.yaml"
	workersPath = "prod/nodegroups/workers.yaml"
)

// docs joins YAML documents into one multi-document file.
func docs(d ...string) string { return strings.Join(d, "---\n") }

// replaced returns doc with old replaced by with.
func replaced(t *testing.T, doc, old, with string) string {
	t.Helper()
	if !strings.Contains(doc, old) {
		t.Fatalf("%q is not in\n%s", old, doc)
	}
	return strings.Replace(doc, old, with, 1)
}

// state is a file store in a temporary directory.
type state struct {
	url  string
	root string // does not exist until something is written
}

// newState returns a new, empty file store.
func newState(t *testing.T) state {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	p := filepath.ToSlash(root)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path
	}
	return state{url: (&url.URL{Scheme: "file", Path: p}).String(), root: root}
}

func (s state) open(t *testing.T) statestore.Store {
	t.Helper()
	store, err := statestore.Open(t.Context(), s.url)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return store
}

// objects returns every object in the store with its content.
func (s state) objects(t *testing.T) map[string]string {
	t.Helper()
	store := s.open(t)
	paths, err := store.List(t.Context(), "")
	if err != nil {
		t.Fatalf("list the store: %v", err)
	}
	objs := make(map[string]string, len(paths))
	for _, p := range paths {
		data, _, err := store.Get(t.Context(), p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		objs[p] = string(data)
	}
	return objs
}

// want fails the test unless the store holds exactly the objects of want.
func (s state) want(t *testing.T, want map[string]string) {
	t.Helper()
	if diff := cmp.Diff(want, s.objects(t)); diff != "" {
		t.Errorf("the store (-want +got):\n%s", diff)
	}
}

// put writes an object to the store.
func (s state) put(t *testing.T, p, data string) {
	t.Helper()
	if _, err := s.open(t).Put(t.Context(), p, []byte(data), statestore.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", p, err)
	}
}

// wantEmpty fails the test unless nothing was ever written to the store.
func (s state) wantEmpty(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(s.root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the store was written to: stat %s: %v", s.root, err)
	}
}

// withCluster returns a store that holds the test cluster.
func withCluster(t *testing.T) state {
	t.Helper()
	s := newState(t)
	s.put(t, clusterPath, clusterYAML)
	s.put(t, serversPath, serversYAML)
	s.put(t, workersPath, workersYAML)
	return s
}

// writeFile writes content to a new file and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// result is what one run of tent did.
type result struct {
	code        int
	out, errOut string
}

// runIn executes tent with args and stdin and returns what it did.
func runIn(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return startIn(t, stdin, args...).wait()
}

// started is tent running in another goroutine.
type started struct {
	out, errOut syncBuffer
	code        chan int
}

// executeTest runs tent as Execute does, but without the process's signals: a test that needs them sends them
// itself, and signal.Notify from a synctest bubble crashes the test binary.
func executeTest(ctx context.Context, t *testing.T, args []string, s Streams) int {
	t.Helper()
	return executeWithSignals(ctx, args, s, nil, func(code int) { t.Errorf("tent exited with %d on a signal", code) })
}

// startIn starts tent with args and stdin in another goroutine.
func startIn(t *testing.T, stdin string, args ...string) *started {
	t.Helper()
	r := &started{code: make(chan int, 1)}
	go func() {
		r.code <- executeTest(t.Context(), t, args, Streams{In: strings.NewReader(stdin), Out: &r.out, Err: &r.errOut})
	}()
	return r
}

// wait waits for tent to exit and returns what it did.
func (r *started) wait() result {
	code := <-r.code
	return result{code, r.out.String(), r.errOut.String()}
}

// wantResult fails the test unless tent exited with code and wrote exactly out and errOut.
func wantResult(t *testing.T, got result, code int, out, errOut string) {
	t.Helper()
	if got.code != code {
		t.Errorf("exit code = %d, want %d", got.code, code)
	}
	if diff := cmp.Diff(out, got.out); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(errOut, got.errOut); diff != "" {
		t.Errorf("stderr (-want +got):\n%s", diff)
	}
}

// wantOK fails the test unless tent succeeded, wrote out and nothing to stderr.
func wantOK(t *testing.T, got result, out string) {
	t.Helper()
	wantResult(t, got, 0, out, "")
}

// openAPIWarning is what a create or replace prints when the cluster lets the whole internet reach the Nomad API,
// as the test cluster does.
const openAPIWarning = "WARNING: spec.access.api lets the whole internet reach the Nomad API (port 4646); mTLS and " +
	"ACLs protect it; narrow it with --api-access or spec.access.api\n"

// wantDone fails the test unless tent succeeded, wrote out, and warned that the Nomad API is open.
func wantDone(t *testing.T, got result, out string) {
	t.Helper()
	wantResult(t, got, 0, out, openAPIWarning)
}

// wantError fails the test unless tent failed with exit code 1, wrote nothing to stdout and errOut to stderr.
func wantError(t *testing.T, got result, errOut string) {
	t.Helper()
	wantResult(t, got, 1, "", errOut)
}

// syncBuffer is a buffer that a command running in another goroutine may write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// holdLock takes the lock of cluster prod as another tent would, until the test releases it or ends.
func holdLock(t *testing.T, s state) *statestore.Lock {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	lk, _, err := statestore.NewLocker(t.Context(), s.open(t), l, nil)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := statestore.Acquire(t.Context(), lk, "prod", statestore.AcquireOptions{Operation: "update"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release(context.Background()) })
	return lock
}

// leaseStore hides the file store from statestore.NewLocker, which then locks with a lease object in the store, as on
// S3: a store where state unlock --force matters.
type leaseStore struct{ statestore.Store }

// runWithStore executes tent with args on the store that wrap makes of the one --state names.
func runWithStore(t *testing.T, wrap func(statestore.Store) statestore.Store, args ...string) result {
	t.Helper()
	var out, errOut syncBuffer
	opts := &globalOptions{openStore: func(ctx context.Context, u string) (statestore.Store, error) {
		s, err := statestore.Open(ctx, u)
		if err != nil {
			return nil, err
		}
		return wrap(s), nil
	}}
	root := newRootCommand(Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, opts)
	code := execute(t.Context(), root, args, &errOut)
	return result{code, out.String(), errOut.String()}
}
