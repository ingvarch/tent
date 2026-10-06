package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secret"
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
func executeTest(ctx context.Context, t *testing.T, args []string, s Streams, opts ...Option) int {
	t.Helper()
	return executeWithSignals(ctx, args, s, nil, func(code int) { t.Errorf("tent exited with %d on a signal", code) },
		opts...)
}

// onVultr returns providers that reach the Vultr fake f and log to the command's logger. Every provider other than
// vultr is one that tent cannot manage yet.
func onVultr(f *vultrfake.Fake) Providers {
	return func(name v1alpha1.Provider, log *slog.Logger) (cloud.Provider, error) {
		if name != v1alpha1.ProviderVultr {
			return nil, cloud.UnsupportedProvider(name)
		}
		return vultr.New(f, vultr.WithLogger(log)), nil
	}
}

// testAssets serves the release files that nodes download from assetstest, and gives a development build of tent the
// tent-node that it needs.
func testAssets() assets.Options {
	return assets.Options{
		Client: &http.Client{Transport: assetstest.New()}, DevURL: assetstest.DevURL, DevSHA256: assetstest.DevSHA256,
		Now: assetstest.Now,
	}
}

// staticNomad returns the Nomad factory of a cluster that has a leader, three healthy servers that vote, and the three
// workers of the test cluster registered at 10.64.0.6 to 10.64.0.8. Its Raft peers and the servers that autopilot
// reports are the servers at 10.64.0.3 to 10.64.0.5, the addresses that the test cluster's machines get on the fake;
// they and the workers run the Nomad version that the test assets pin. Its ACL system counts as bootstrapped
// with the token of the first client that is made, which is the secret that tent holds, as that of a cluster that an
// earlier run built. It does not follow the cloud, and the CLI tests check output, not the order of the calls.
func staticNomad() func(nomadops.Config) (nomadops.API, error) {
	return nomadOf("servers", "workers", 6)
}

// combinedNomad returns the Nomad factory of staticNomad for a cluster of the combined node group nodes: its three
// servers and clients are the same machines, at 10.64.0.3 to 10.64.0.5.
func combinedNomad() func(nomadops.Config) (nomadops.API, error) {
	return nomadOf("nodes", "nodes", 3)
}

// nomadOf returns the Nomad factory of staticNomad for servers in the node group servers, and three clients of the
// node group clients that have the private addresses 10.64.0.<first> and the two that follow.
func nomadOf(servers, clients string, first byte) func(nomadops.Config) (nomadops.API, error) {
	f := nomadfake.New()
	f.SetLeader("10.64.0.3:4647")
	health := nomadops.Health{Healthy: true, Voters: 3}
	var peers []nomadops.Peer
	for i := range 3 {
		f.Register(nomadops.Node{
			Name: fmt.Sprintf("prod-%s-%d", clients, i), Status: "ready", Eligible: true,
			Address: netip.AddrFrom4([4]byte{10, 64, 0, first + byte(i)}), Version: assetstest.NomadVersion,
		})
		name := fmt.Sprintf("prod-%s-%d.global", servers, i)
		addr := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 64, 0, byte(3 + i)}), 4647)
		peers = append(peers, nomadops.Peer{Name: name, Address: addr, Voter: true})
		health.Servers = append(health.Servers, nomadops.ServerHealth{
			Name: name, Address: addr, Serf: "alive", Healthy: true, Voter: true, Leader: i == 0,
			Version: assetstest.NomadVersion,
		})
	}
	f.SetHealth(health)
	f.SetPeers(peers)
	var once sync.Once
	return func(cfg nomadops.Config) (nomadops.API, error) {
		once.Do(func() { f.SetBootstrapped(cfg.Token) })
		return f.Client(cfg), nil
	}
}

// runOn executes tent with args as Execute does, its providers reaching the Vultr fake f.
func runOn(t *testing.T, f *vultrfake.Fake, args ...string) result {
	t.Helper()
	return runProviders(t, onVultr(f), args...)
}

// runProviders executes tent with args as Execute does, with the providers p.
func runProviders(t *testing.T, p Providers, args ...string) result {
	t.Helper()
	return runWithNomad(t, p, staticNomad(), args...)
}

// runWithNomad executes tent with args as Execute does, with the providers p and the Nomad factory nomad.
func runWithNomad(t *testing.T, p Providers, nomad func(nomadops.Config) (nomadops.API, error), args ...string) result {
	t.Helper()
	var out, errOut syncBuffer
	code := executeTest(t.Context(), t, args, Streams{In: strings.NewReader(""), Out: &out, Err: &errOut},
		WithProviders(p), WithAssets(testAssets()), WithNomad(nomad))
	return result{code, out.String(), errOut.String()}
}

// lateNomad returns the Nomad factory of staticNomad, in which the node called name is listed only once a client has
// asked for an intro token: the node of a machine that never registered, and then of the machine that replaces it.
func lateNomad(name string) func(nomadops.Config) (nomadops.API, error) {
	inner := staticNomad()
	var introduced atomic.Bool
	return func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return &lateAPI{API: api, name: name, introduced: &introduced}, err
	}
}

// lateAPI is a nomadops.API that lists the node called name only once introduced is set, which an intro token sets.
type lateAPI struct {
	nomadops.API
	name       string
	introduced *atomic.Bool
}

func (a *lateAPI) IntroToken(ctx context.Context, req nomadops.IntroRequest) (secret.Secret, error) {
	a.introduced.Store(true)
	return a.API.IntroToken(ctx, req)
}

func (a *lateAPI) Nodes(ctx context.Context) ([]nomadops.Node, error) {
	nodes, err := a.API.Nodes(ctx)
	if a.introduced.Load() {
		return nodes, err
	}
	return slices.DeleteFunc(nodes, func(n nomadops.Node) bool { return n.Name == a.name }), err
}

// runOnCloud executes tent with args, its providers reaching a Vultr fake that holds nothing of the test cluster.
func runOnCloud(t *testing.T, args ...string) result {
	t.Helper()
	return runOn(t, vultrfake.New(), args...)
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

// combinedWarning is what a create warns when the cluster has the combined node group nodes that create cluster
// --combined makes.
const combinedWarning = "WARNING: node group nodes is combined: its nodes run the Nomad servers and the workloads " +
	"together, which is meant for development and small clusters; workloads share them with Raft and the gossip key\n"

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
	return runWith(t, &globalOptions{openStore: wrapped(wrap)}, args...)
}

// wrapped returns an openStore that opens the store at a URL and wraps it with wrap.
func wrapped(wrap func(statestore.Store) statestore.Store) func(context.Context, string) (statestore.Store, error) {
	return func(ctx context.Context, u string) (statestore.Store, error) {
		s, err := statestore.Open(ctx, u)
		if err != nil {
			return nil, err
		}
		return wrap(s), nil
	}
}

// runWith executes tent with args, starting from opts, which the flags and the config file complete.
func runWith(t *testing.T, opts *globalOptions, args ...string) result {
	t.Helper()
	var out, errOut syncBuffer
	root := newRootCommand(Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, opts)
	code := execute(t.Context(), root, args, &errOut)
	return result{code, out.String(), errOut.String()}
}
