package nomadops_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

const (
	addr1 = "198.51.100.1:4646"
	addr2 = "198.51.100.2:4646"
)

// stub is an API that logs its calls and answers each with err, or with a leader when err is nil.
type stub struct {
	mu    sync.Mutex
	err   error
	calls []string
	onErr func() // runs when a call fails
}

func (s *stub) record(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if s.err != nil && s.onErr != nil {
		s.onErr()
	}
	return s.err
}

func (s *stub) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *stub) Leader(context.Context) (string, error) {
	if err := s.record("Leader"); err != nil {
		return "", err
	}
	return leaderAddr, nil
}

func (s *stub) Bootstrap(context.Context, secret.Secret) error { return s.record("Bootstrap") }

func (s *stub) IntroToken(context.Context, nomadops.IntroRequest) (secret.Secret, error) {
	if err := s.record("IntroToken"); err != nil {
		return nil, err
	}
	return secret.Secret("token"), nil
}

func (s *stub) CreateToken(context.Context, nomadops.TokenRequest) (nomadops.Token, error) {
	if err := s.record("CreateToken"); err != nil {
		return nomadops.Token{}, err
	}
	return nomadops.Token{Accessor: "accessor", Secret: secret.Secret("token")}, nil
}

func (s *stub) Nodes(context.Context) ([]nomadops.Node, error) {
	if err := s.record("Nodes"); err != nil {
		return nil, err
	}
	return []nomadops.Node{{Name: "n"}}, nil
}

func (s *stub) Peers(context.Context) ([]nomadops.Peer, error) {
	if err := s.record("Peers"); err != nil {
		return nil, err
	}
	return []nomadops.Peer{{Name: "s0", Voter: true}}, nil
}

func (s *stub) Health(context.Context) (nomadops.Health, error) {
	if err := s.record("Health"); err != nil {
		return nomadops.Health{}, err
	}
	return nomadops.Health{Healthy: true, Voters: 3}, nil
}

func (s *stub) KeyringReady(context.Context) (bool, error) {
	if err := s.record("KeyringReady"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *stub) MarkIneligible(context.Context, string) error { return s.record("MarkIneligible") }

func (s *stub) Drain(context.Context, string, nomadops.DrainRequest) error { return s.record("Drain") }

func (s *stub) Purge(context.Context, string) error { return s.record("Purge") }

func (s *stub) TransferLeadership(context.Context, string) error {
	return s.record("TransferLeadership")
}

func (s *stub) RemovePeer(context.Context, string) error { return s.record("RemovePeer") }

func (s *stub) Members(context.Context) ([]nomadops.Member, error) {
	if err := s.record("Members"); err != nil {
		return nil, err
	}
	return []nomadops.Member{{Name: "s0.eu", Status: "alive"}}, nil
}

func (s *stub) ForceLeave(context.Context, string) error { return s.record("ForceLeave") }

// notReady is an error of the class ErrNotReady, as a server that broke its answer off makes.
func notReady(path string) error {
	return nomadops.NewCallError("GET", path, io.ErrUnexpectedEOF)
}

// servers returns Servers over the stubs, which are at addr1, addr2 and so on in order.
func servers(t *testing.T, stubs ...*stub) *nomadops.Servers {
	t.Helper()
	var list []nomadops.Server
	for i, s := range stubs {
		list = append(list, nomadops.Server{Address: fmt.Sprintf("198.51.100.%d:4646", i+1), API: s})
	}
	return newServers(t, list)
}

// newServers returns Servers over list and fails the test when it cannot be made.
func newServers(t *testing.T, list []nomadops.Server) *nomadops.Servers {
	t.Helper()
	s, err := nomadops.NewServers(list...)
	if err != nil {
		t.Fatalf("NewServers: %v", err)
	}
	return s
}

// fakeServers returns Servers over clients of f at addr1 and addr2 with token, each wrapped by wrap when it is not nil.
func fakeServers(
	t *testing.T, f *nomadfake.Fake, token secret.Secret, wrap func(nomadops.API) nomadops.API,
) *nomadops.Servers {
	t.Helper()
	var list []nomadops.Server
	for _, addr := range []string{addr1, addr2} {
		api := f.Client(nomadops.Config{Address: addr, Token: token})
		if wrap != nil {
			api = wrap(api)
		}
		list = append(list, nomadops.Server{Address: addr, API: api})
	}
	return newServers(t, list)
}

func wantStubCalls(t *testing.T, s *stub, want ...string) {
	t.Helper()
	if diff := cmp.Diff(want, s.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

func TestNewServersRefusesBadInput(t *testing.T) {
	for name, list := range map[string][]nomadops.Server{
		"no server": nil,
		"a nil API": {{Address: addr1, API: &stub{}}, {Address: addr2}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := nomadops.NewServers(list...)
			if err == nil || s != nil {
				t.Fatalf("NewServers() = %v, %v; want an error", s, err)
			}
			if errors.Is(err, nomadops.ErrNotReady) {
				t.Errorf("error %q matches ErrNotReady", err)
			}
		})
	}
	if _, err := nomadops.NewServers(); err == nil || err.Error() != "nomad: no servers" {
		t.Errorf("NewServers() error = %v, want %q", err, "nomad: no servers")
	}
}

func TestServersFirstAnswers(t *testing.T) {
	a, b := &stub{}, &stub{}
	s := servers(t, a, b)

	got, err := s.Leader(t.Context())

	if err != nil || got != leaderAddr {
		t.Errorf("Leader() = %q, %v; want %s", got, err, leaderAddr)
	}
	wantStubCalls(t, a, "Leader")
	wantStubCalls(t, b)
}

func TestServersMoveOnAndStayOnTheServerThatAnswered(t *testing.T) {
	a, b := &stub{err: notReady("/v1/status/leader")}, &stub{}
	s := servers(t, a, b)

	for range 2 {
		if _, err := s.Leader(t.Context()); err != nil {
			t.Fatalf("Leader: %v", err)
		}
	}

	wantStubCalls(t, a, "Leader")
	wantStubCalls(t, b, "Leader", "Leader")
}

func TestServersEveryMethodMovesOn(t *testing.T) {
	calls := map[string]func(context.Context, nomadops.API) error{
		"Leader": func(ctx context.Context, a nomadops.API) error { _, err := a.Leader(ctx); return err },
		"Bootstrap": func(ctx context.Context, a nomadops.API) error {
			return a.Bootstrap(ctx, pki.NewBootstrapSecret())
		},
		"IntroToken": func(ctx context.Context, a nomadops.API) error {
			_, err := a.IntroToken(ctx, nomadops.IntroRequest{NodeName: "n", NodePool: "default", TTL: time.Minute})
			return err
		},
		"CreateToken": func(ctx context.Context, a nomadops.API) error {
			_, err := a.CreateToken(ctx, nomadops.TokenRequest{Name: "n", TTL: time.Hour})
			return err
		},
		"Nodes":  func(ctx context.Context, a nomadops.API) error { _, err := a.Nodes(ctx); return err },
		"Health": func(ctx context.Context, a nomadops.API) error { _, err := a.Health(ctx); return err },
		"Peers":  func(ctx context.Context, a nomadops.API) error { _, err := a.Peers(ctx); return err },
		"KeyringReady": func(ctx context.Context, a nomadops.API) error {
			ready, err := a.KeyringReady(ctx)
			if err == nil && !ready {
				return errors.New("the keyring is not ready")
			}
			return err
		},
		"MarkIneligible": func(ctx context.Context, a nomadops.API) error { return a.MarkIneligible(ctx, nodeID) },
		"Drain":          func(ctx context.Context, a nomadops.API) error { return a.Drain(ctx, nodeID, drainReq) },
		"Purge":          func(ctx context.Context, a nomadops.API) error { return a.Purge(ctx, nodeID) },
		"TransferLeadership": func(ctx context.Context, a nomadops.API) error {
			return a.TransferLeadership(ctx, raftID)
		},
		"RemovePeer": func(ctx context.Context, a nomadops.API) error { return a.RemovePeer(ctx, raftID) },
		"Members":    func(ctx context.Context, a nomadops.API) error { _, err := a.Members(ctx); return err },
		"ForceLeave": func(ctx context.Context, a nomadops.API) error { return a.ForceLeave(ctx, memberName) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			a, b := &stub{err: notReady("/p")}, &stub{}
			if err := call(t.Context(), servers(t, a, b)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			wantStubCalls(t, a, name)
			wantStubCalls(t, b, name)
		})
	}
}

// A server that answered last stops answering: the call goes on to the servers before it in the list.
func TestServersWrapAroundToTheServersBeforeTheLast(t *testing.T) {
	a, b := &stub{err: notReady("/p")}, &stub{}
	s := servers(t, a, b)
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	a.err, b.err = nil, notReady("/p")

	for range 2 {
		if _, err := s.Leader(t.Context()); err != nil {
			t.Fatalf("Leader after the last server went down: %v", err)
		}
	}

	wantStubCalls(t, a, "Leader", "Leader", "Leader")
	wantStubCalls(t, b, "Leader", "Leader")
}

// No server is ready and the last answer came from the second: the error names the second first.
func TestServersNoneReadyFromTheLastAnswer(t *testing.T) {
	a, b := &stub{err: notReady("/p")}, &stub{}
	s := servers(t, a, b)
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	a.err = fmt.Errorf("first down: %w", nomadops.ErrNotReady)
	b.err = fmt.Errorf("second down: %w", nomadops.ErrNotReady)

	_, err := s.Leader(t.Context())

	checkErr(t, err, "nomad: no server is ready: "+addr2+": second down: not ready; "+addr1+": first down: not ready",
		true)
	wantStubCalls(t, a, "Leader", "Leader")
	wantStubCalls(t, b, "Leader", "Leader")
}

// ctxRecorder is an API that records the context of each call, then passes the call on.
type ctxRecorder struct {
	nomadops.API
	seen *[]context.Context
}

func (r ctxRecorder) record(ctx context.Context) { *r.seen = append(*r.seen, ctx) }

func (r ctxRecorder) Leader(ctx context.Context) (string, error) {
	r.record(ctx)
	return r.API.Leader(ctx)
}

func (r ctxRecorder) Bootstrap(ctx context.Context, s secret.Secret) error {
	r.record(ctx)
	return r.API.Bootstrap(ctx, s)
}

func (r ctxRecorder) IntroToken(ctx context.Context, req nomadops.IntroRequest) (secret.Secret, error) {
	r.record(ctx)
	return r.API.IntroToken(ctx, req)
}

func (r ctxRecorder) CreateToken(ctx context.Context, req nomadops.TokenRequest) (nomadops.Token, error) {
	r.record(ctx)
	return r.API.CreateToken(ctx, req)
}

func (r ctxRecorder) Nodes(ctx context.Context) ([]nomadops.Node, error) {
	r.record(ctx)
	return r.API.Nodes(ctx)
}

func (r ctxRecorder) Peers(ctx context.Context) ([]nomadops.Peer, error) {
	r.record(ctx)
	return r.API.Peers(ctx)
}

func (r ctxRecorder) KeyringReady(ctx context.Context) (bool, error) {
	r.record(ctx)
	return r.API.KeyringReady(ctx)
}

func (r ctxRecorder) Health(ctx context.Context) (nomadops.Health, error) {
	r.record(ctx)
	return r.API.Health(ctx)
}

func (r ctxRecorder) MarkIneligible(ctx context.Context, nodeID string) error {
	r.record(ctx)
	return r.API.MarkIneligible(ctx, nodeID)
}

func (r ctxRecorder) Drain(ctx context.Context, nodeID string, req nomadops.DrainRequest) error {
	r.record(ctx)
	return r.API.Drain(ctx, nodeID, req)
}

func (r ctxRecorder) Purge(ctx context.Context, nodeID string) error {
	r.record(ctx)
	return r.API.Purge(ctx, nodeID)
}

func (r ctxRecorder) TransferLeadership(ctx context.Context, raftID string) error {
	r.record(ctx)
	return r.API.TransferLeadership(ctx, raftID)
}

func (r ctxRecorder) RemovePeer(ctx context.Context, raftID string) error {
	r.record(ctx)
	return r.API.RemovePeer(ctx, raftID)
}

func (r ctxRecorder) Members(ctx context.Context) ([]nomadops.Member, error) {
	r.record(ctx)
	return r.API.Members(ctx)
}

func (r ctxRecorder) ForceLeave(ctx context.Context, name string) error {
	r.record(ctx)
	return r.API.ForceLeave(ctx, name)
}

// The arguments, the context itself and the values pass through Servers, and a write moves on after a lost answer.
func TestServersPassArgumentsContextAndValues(t *testing.T) {
	f := nomadfake.New()
	f.SetLeader(leaderAddr)
	f.Register(nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true})
	f.SetHealth(nomadops.Health{Healthy: true, Voters: 3})
	f.SetPeers([]nomadops.Peer{{Name: "prod-servers-0.eu", Voter: true}})
	var seen []context.Context
	s := fakeServers(t, f, pki.NewBootstrapSecret(), func(a nomadops.API) nomadops.API {
		return ctxRecorder{API: a, seen: &seen}
	})
	single := f.Client(nomadops.Config{Address: "single", Token: pki.NewBootstrapSecret()})
	req := nomadops.IntroRequest{NodeName: "prod-workers-1", NodePool: "default", TTL: 30 * time.Minute}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bootstrapSecret := pki.NewBootstrapSecret()
	f.LoseResponse(t, "Bootstrap")
	f.LoseResponse(t, "IntroToken")

	if err := s.Bootstrap(ctx, bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// The fake refuses another secret, so this shows which secret reached the cluster.
	if err := single.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Errorf("the cluster was bootstrapped with another secret: %v", err)
	}
	token, err := s.IntroToken(ctx, req)
	if err != nil {
		t.Fatalf("IntroToken: %v", err)
	}
	wantToken, err := single.IntroToken(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(token) != string(wantToken) {
		t.Error("IntroToken() is not the token that a single client gets for the request")
	}
	if got, err := s.Leader(ctx); err != nil || got != leaderAddr {
		t.Errorf("Leader() = %q, %v; want %s", got, err, leaderAddr)
	}
	nodes, err := s.Nodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "prod-workers-1" {
		t.Errorf("Nodes() = %v, %v; want prod-workers-1", nodes, err)
	}
	if h, err := s.Health(ctx); err != nil || !cmp.Equal(h, nomadops.Health{Healthy: true, Voters: 3}, equateAddrs) {
		t.Errorf("Health() = %+v, %v; want the set one", h, err)
	}
	if got, err := s.Peers(ctx); err != nil || len(got) != 1 || got[0].Name != "prod-servers-0.eu" {
		t.Errorf("Peers() = %+v, %v; want the set one", got, err)
	}
	f.SetKeyringDelay(1)
	for _, want := range []bool{false, true} {
		if got, err := s.KeyringReady(ctx); err != nil || got != want {
			t.Errorf("KeyringReady() = %v, %v; want %v, nil", got, err, want)
		}
	}

	const bootstrapArg = "[secret, 36 bytes]"
	const introArg = "prod-workers-1 default 30m0s"
	want := []nomadfake.Call{
		{Name: "Bootstrap", Server: addr1, Arg: bootstrapArg},
		{Name: "Bootstrap", Server: addr2, Arg: bootstrapArg},
		{Name: "Bootstrap", Server: "single", Arg: bootstrapArg},
		{Name: "IntroToken", Server: addr2, Arg: introArg},
		{Name: "IntroToken", Server: addr1, Arg: introArg},
		{Name: "IntroToken", Server: "single", Arg: introArg},
		{Name: "Leader", Server: addr1},
		{Name: "Nodes", Server: addr1},
		{Name: "Health", Server: addr1},
		{Name: "Peers", Server: addr1},
		{Name: "KeyringReady", Server: addr1},
		{Name: "KeyringReady", Server: addr1},
	}
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(seen) != 10 { // the calls of Servers: the ones of single have no recorder
		t.Fatalf("%d calls seen, want 10", len(seen))
	}
	for i, c := range seen {
		if c != ctx {
			t.Errorf("call %d got another context than the caller's", i)
		}
	}
}

func TestNewServersKeepsItsOwnCopyOfTheList(t *testing.T) {
	a, b := &stub{}, &stub{}
	list := []nomadops.Server{{Address: addr1, API: a}}
	s := newServers(t, list)

	list[0].API = b
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}

	wantStubCalls(t, a, "Leader")
	wantStubCalls(t, b)
}

func TestServersNoneReady(t *testing.T) {
	a := &stub{err: fmt.Errorf("first down: %w", nomadops.ErrNotReady)}
	b := &stub{err: fmt.Errorf("second down: %w", nomadops.ErrNotReady)}
	s := servers(t, a, b)

	_, err := s.Leader(t.Context())

	checkErr(t, err, "nomad: no server is ready: "+addr1+": first down: not ready; "+addr2+": second down: not ready",
		true)
	wantStubCalls(t, a, "Leader")
	wantStubCalls(t, b, "Leader")
}

func TestServersPermanentErrorStopsAtOnce(t *testing.T) {
	permanent := errors.New("forbidden")
	a, b := &stub{err: permanent}, &stub{}
	s := servers(t, a, b)

	_, err := s.Leader(t.Context())

	if !errors.Is(err, permanent) || err.Error() != permanent.Error() {
		t.Errorf("Leader() error = %v, want the permanent error as it is", err)
	}
	wantStubCalls(t, b)
}

// A permanent error after a server that was not ready is returned as it is, and the servers after it are not tried.
func TestServersPermanentErrorAfterNotReadyStopsAtOnce(t *testing.T) {
	permanent := errors.New("forbidden")
	a, b, c := &stub{err: notReady("/p")}, &stub{err: permanent}, &stub{}
	s := servers(t, a, b, c)

	_, err := s.Leader(t.Context())

	if !errors.Is(err, permanent) || err.Error() != permanent.Error() {
		t.Errorf("Leader() error = %v, want the permanent error as it is", err)
	}
	wantStubCalls(t, a, "Leader")
	wantStubCalls(t, b, "Leader")
	wantStubCalls(t, c)
}

func TestServersEndedContextStopsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	a, b := &stub{err: notReady("/p"), onErr: cancel}, &stub{}
	s := servers(t, a, b)

	_, err := s.Leader(ctx)

	if !errors.Is(err, context.Canceled) || errors.Is(err, nomadops.ErrNotReady) {
		t.Errorf("Leader() error = %v, want one that matches context.Canceled only", err)
	}
	wantStubCalls(t, b)
}

func TestWaitLeaderOverServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := nomadfake.New()
		s := fakeServers(t, f, pki.NewBootstrapSecret(), nil)
		after(3*time.Second, func() { f.SetLeader(leaderAddr) })
		start := time.Now()

		got, err := nomadops.WaitLeader(bounded(t), s)

		if err != nil || got != leaderAddr {
			t.Errorf("WaitLeader() = %q, %v; want %s", got, err, leaderAddr)
		}
		wantTook(t, start, 4*time.Second)
		var servers []string
		for _, c := range f.Calls() {
			servers = append(servers, c.Server)
		}
		want := []string{addr1, addr2, addr1, addr2, addr1}
		if diff := cmp.Diff(want, servers); diff != "" {
			t.Errorf("servers of the calls (-want +got):\n%s", diff)
		}
	})
}

// A token whose answer was lost is made again on the next server, and the cluster holds both tokens.
func TestServersCreateTokenMovesOnAfterALostAnswer(t *testing.T) {
	f := nomadfake.New()
	f.SetLeader(leaderAddr)
	s := fakeServers(t, f, pki.NewBootstrapSecret(), nil)
	if err := s.Bootstrap(t.Context(), pki.NewBootstrapSecret()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	f.LoseResponse(t, "CreateToken")
	req := nomadops.TokenRequest{Name: "tent export nomad ana@laptop", TTL: time.Hour}

	got, err := s.CreateToken(t.Context(), req)

	if err != nil || len(got.Secret) == 0 {
		t.Fatalf("CreateToken: %v", err)
	}
	if want := []string{addr1, addr2}; !slices.Equal(callServers(f, "CreateToken"), want) {
		t.Errorf("servers of the CreateToken calls = %v, want %v", callServers(f, "CreateToken"), want)
	}
	issued := f.Issued()
	if len(issued) != 2 || issued[1].Accessor != got.Accessor || issued[0].Accessor == got.Accessor {
		t.Errorf("issued = %+v, want the lost token and then the returned one %s", issued, got.Accessor)
	}
	if last := s.Last(); last != addr2 {
		t.Errorf("Last() = %q, want %s", last, addr2)
	}
}

// callServers returns the servers of the calls of the API method name, in order.
func callServers(f *nomadfake.Fake, name string) []string {
	var out []string
	for _, c := range f.Calls() {
		if c.Name == name {
			out = append(out, c.Server)
		}
	}
	return out
}

func TestServersLastIsTheFirstServerBeforeAnyCall(t *testing.T) {
	s := servers(t, &stub{}, &stub{})

	if got := s.Last(); got != addr1 {
		t.Errorf("Last() = %q before any call, want %s", got, addr1)
	}
}

func TestServersLastFollowsTheServerThatAnswered(t *testing.T) {
	a, b := &stub{err: notReady("/p")}, &stub{}
	s := servers(t, a, b)
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	if got := s.Last(); got != addr2 {
		t.Errorf("Last() = %q after the second server answered, want %s", got, addr2)
	}
	a.err, b.err = nil, notReady("/p")

	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}

	if got := s.Last(); got != addr1 {
		t.Errorf("Last() = %q after the first server answered, want %s", got, addr1)
	}
}

// A call that ends with an error that is not ErrNotReady leaves Last as it was, and the next call starts there.
func TestServersLastStaysAfterAPermanentError(t *testing.T) {
	permanent := errors.New("forbidden")
	a, b := &stub{err: notReady("/p")}, &stub{err: permanent}
	s := servers(t, a, b)

	if _, err := s.Leader(t.Context()); !errors.Is(err, permanent) {
		t.Fatalf("Leader() error = %v, want the permanent error", err)
	}

	if got := s.Last(); got != addr1 {
		t.Errorf("Last() = %q after a permanent error of the second server, want %s", got, addr1)
	}
	a.err, b.err = nil, nil
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	wantStubCalls(t, a, "Leader", "Leader")
	wantStubCalls(t, b, "Leader")
}

func TestServersLastStaysWhenNoServerIsReady(t *testing.T) {
	a, b := &stub{err: notReady("/p")}, &stub{}
	s := servers(t, a, b)
	if _, err := s.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %v", err)
	}
	b.err = notReady("/p")

	if _, err := s.Leader(t.Context()); !errors.Is(err, nomadops.ErrNotReady) {
		t.Fatalf("Leader() error = %v, want ErrNotReady", err)
	}

	if got := s.Last(); got != addr2 {
		t.Errorf("Last() = %q after a call that no server answered, want %s", got, addr2)
	}
}

func TestServersConcurrentUse(t *testing.T) {
	a, b := &stub{err: notReady("/p")}, &stub{}
	s := servers(t, a, b)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 5 {
				if _, err := s.Leader(t.Context()); err != nil {
					t.Errorf("Leader: %v", err)
				}
				if got := s.Last(); got != addr1 && got != addr2 {
					t.Errorf("Last() = %q, want one of the servers", got)
				}
			}
		})
	}
	wg.Wait()
}

func TestServersPrintOnlyAddresses(t *testing.T) {
	token := pki.NewBootstrapSecret()
	s := fakeServers(t, nomadfake.New(), token, nil)

	secrettest.CheckHidden(t, secrettest.Printed(t, s), map[string][]byte{"the token": token}, "")
	secrettest.CheckHidden(t, secrettest.Printed(t, *s), map[string][]byte{"the token": token}, "")
	if got, want := fmt.Sprint(s), "nomadops.Servers("+addr1+", "+addr2+")"; got != want {
		t.Errorf("Sprint = %q, want %q", got, want)
	}
}

// TestServersPassTheNodeWrites checks that the node ID and the drain request reach the server, with the caller's
// context, and that the writes change the cluster.
func TestServersPassTheNodeWrites(t *testing.T) {
	f := nomadfake.New()
	f.SetLeader(leaderAddr)
	f.SetBootstrapped(pki.NewBootstrapSecret())
	f.Register(nomadops.Node{ID: nodeID, Name: "prod-workers-1", Status: "ready", Eligible: true})
	var seen []context.Context
	s := fakeServers(t, f, pki.NewBootstrapSecret(), func(a nomadops.API) nomadops.API {
		return ctxRecorder{API: a, seen: &seen}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	for name, err := range map[string]error{
		"MarkIneligible": s.MarkIneligible(ctx, nodeID),
		"Drain":          s.Drain(ctx, nodeID, drainReq),
		"Purge":          s.Purge(ctx, nodeID),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	want := []nomadfake.Call{
		{Name: "MarkIneligible", Server: addr1, Arg: nodeID},
		{Name: "Drain", Server: addr1, Arg: nodeID + " 1h0m0s tent_machine=m-1"},
		{Name: "Purge", Server: addr1, Arg: nodeID},
	}
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(seen) != 3 {
		t.Fatalf("%d calls seen, want 3", len(seen))
	}
	for i, c := range seen {
		if c != ctx {
			t.Errorf("call %d got another context than the caller's", i)
		}
	}
	if nodes, err := s.Nodes(t.Context()); err != nil || len(nodes) != 0 {
		t.Errorf("Nodes() after the purge = %+v, %v; want none", nodes, err)
	}
}

// TestServersLoseTheAnswerOfANodeWrite checks that each write moves on to the next server after an answer that was
// lost, and that the repeat succeeds: the node is ineligible or draining already, or gone.
func TestServersLoseTheAnswerOfANodeWrite(t *testing.T) {
	for _, name := range []string{"MarkIneligible", "Drain", "Purge"} {
		t.Run(name, func(t *testing.T) {
			f := nomadfake.New()
			f.SetLeader(leaderAddr)
			f.SetBootstrapped(pki.NewBootstrapSecret())
			f.Register(nomadops.Node{ID: nodeID, Name: "prod-workers-1", Status: "ready", Eligible: true})
			s := fakeServers(t, f, pki.NewBootstrapSecret(), nil)
			f.LoseResponse(t, name)
			call := map[string]func() error{
				"MarkIneligible": func() error { return s.MarkIneligible(t.Context(), nodeID) },
				"Drain":          func() error { return s.Drain(t.Context(), nodeID, drainReq) },
				"Purge":          func() error { return s.Purge(t.Context(), nodeID) },
			}[name]

			if err := call(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if want := []string{addr1, addr2}; !slices.Equal(callServers(f, name), want) {
				t.Errorf("servers of the %s calls = %v, want %v", name, callServers(f, name), want)
			}
			if last := s.Last(); last != addr2 {
				t.Errorf("Last() = %q, want %s", last, addr2)
			}
		})
	}
}

// TestServersStopAtAGoneTarget checks that ErrGone is permanent: no other server is asked, and the error comes back as
// it is.
func TestServersStopAtAGoneTarget(t *testing.T) {
	gone := fmt.Errorf("the target: %w", nomadops.ErrGone)
	for name, call := range map[string]func(*nomadops.Servers) error{
		"MarkIneligible": func(s *nomadops.Servers) error { return s.MarkIneligible(t.Context(), nodeID) },
		"Drain":          func(s *nomadops.Servers) error { return s.Drain(t.Context(), nodeID, drainReq) },
		"Purge":          func(s *nomadops.Servers) error { return s.Purge(t.Context(), nodeID) },
		"TransferLeadership": func(s *nomadops.Servers) error {
			return s.TransferLeadership(t.Context(), raftID)
		},
		"RemovePeer": func(s *nomadops.Servers) error { return s.RemovePeer(t.Context(), raftID) },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := &stub{err: gone}, &stub{}

			err := call(servers(t, a, b))

			if !errors.Is(err, nomadops.ErrGone) || errors.Is(err, nomadops.ErrNotReady) || err.Error() != gone.Error() {
				t.Errorf("%s() error = %v, want the ErrGone error as it is", name, err)
			}
			wantStubCalls(t, a, name)
			wantStubCalls(t, b)
		})
	}
}

// raftFixture is a cluster of three servers, p-1 leading, and Servers over two clients of it, which record the
// contexts they get.
func raftFixture(t *testing.T, seen *[]context.Context) (*nomadfake.Fake, *nomadops.Servers) {
	t.Helper()
	f := nomadfake.New()
	f.SetLeader("10.0.0.1:4647")
	f.SetBootstrapped(pki.NewBootstrapSecret())
	f.SetPeers([]nomadops.Peer{
		{ID: "p-1", Name: "s1", Address: netip.MustParseAddrPort("10.0.0.1:4647"), Voter: true, Leader: true},
		{ID: "p-2", Name: "s2", Address: netip.MustParseAddrPort("10.0.0.2:4647"), Voter: true},
		{ID: "p-3", Name: "s3", Address: netip.MustParseAddrPort("10.0.0.3:4647"), Voter: true},
	})
	return f, fakeServers(t, f, pki.NewBootstrapSecret(), func(a nomadops.API) nomadops.API {
		return ctxRecorder{API: a, seen: seen}
	})
}

// peerIDs returns the Raft IDs of the peers that the cluster lists, with a "*" after the one that leads.
func peerIDs(t *testing.T, s *nomadops.Servers) []string {
	t.Helper()
	peers, err := s.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	var ids []string
	for _, p := range peers {
		if p.Leader {
			ids = append(ids, p.ID+"*")
		} else {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// TestServersPassTheRaftWrites checks that the Raft ID reaches the server, with the caller's context, and that the
// writes change the cluster.
func TestServersPassTheRaftWrites(t *testing.T) {
	var seen []context.Context
	f, s := raftFixture(t, &seen)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if err := s.TransferLeadership(ctx, "p-2"); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}
	if err := s.RemovePeer(ctx, "p-3"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}

	want := []nomadfake.Call{
		{Name: "TransferLeadership", Server: addr1, Arg: "p-2"},
		{Name: "RemovePeer", Server: addr1, Arg: "p-3"},
	}
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(seen) != 2 || seen[0] != ctx || seen[1] != ctx {
		t.Errorf("the calls got %d contexts, want the caller's twice", len(seen))
	}
	if got, want := peerIDs(t, s), []string{"p-1", "p-2*"}; !slices.Equal(got, want) {
		t.Errorf("peers = %v, want %v", got, want)
	}
}

// TestServersLoseTheAnswerOfARaftWrite checks that each write moves on to the next server after an answer that was
// lost, and that the repeat succeeds: the server leads already (Nomad's Noop), or its peer is gone.
func TestServersLoseTheAnswerOfARaftWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		call func(*nomadops.Servers) error
		want []string // the peers afterwards
	}{
		"TransferLeadership": {
			call: func(s *nomadops.Servers) error { return s.TransferLeadership(t.Context(), "p-2") },
			want: []string{"p-1", "p-2*", "p-3"},
		},
		"RemovePeer": {
			call: func(s *nomadops.Servers) error { return s.RemovePeer(t.Context(), "p-3") },
			want: []string{"p-1*", "p-2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, s := raftFixture(t, new([]context.Context))
			f.LoseResponse(t, name)

			if err := tc.call(s); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if want := []string{addr1, addr2}; !slices.Equal(callServers(f, name), want) {
				t.Errorf("servers of the %s calls = %v, want %v", name, callServers(f, name), want)
			}
			if last := s.Last(); last != addr2 {
				t.Errorf("Last() = %q, want %s", last, addr2)
			}
			if got := peerIDs(t, s); !slices.Equal(got, tc.want) {
				t.Errorf("peers = %v, want %v", got, tc.want)
			}
		})
	}
}

// memberName is the name of a member of the gossip pool in the tests.
const memberName = "s3.eu"

// gossipFixture is a gossip pool of three servers, s3 failed, and Servers over two clients of it, which record the
// contexts they get.
func gossipFixture(t *testing.T, seen *[]context.Context) (*nomadfake.Fake, *nomadops.Servers) {
	t.Helper()
	f := nomadfake.New()
	f.SetLeader(leaderAddr)
	f.SetBootstrapped(pki.NewBootstrapSecret())
	f.SetMembers([]nomadops.Member{
		{Name: "s1.eu", Address: netip.MustParseAddr("10.0.0.1"), Status: "alive"},
		{Name: "s2.eu", Address: netip.MustParseAddr("10.0.0.2"), Status: "alive"},
		{Name: memberName, Address: netip.MustParseAddr("10.0.0.3"), Status: "failed"},
	})
	return f, fakeServers(t, f, pki.NewBootstrapSecret(), func(a nomadops.API) nomadops.API {
		return ctxRecorder{API: a, seen: seen}
	})
}

// memberNames returns the names of the members that the pool lists.
func memberNames(t *testing.T, s *nomadops.Servers) []string {
	t.Helper()
	members, err := s.Members(t.Context())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	var names []string
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
}

// TestServersPassTheGossipCalls checks that the name reaches the server, with the caller's context, that Members
// returns what the server lists, and that ForceLeave changes the pool.
func TestServersPassTheGossipCalls(t *testing.T) {
	var seen []context.Context
	f, s := gossipFixture(t, &seen)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	members, err := s.Members(ctx)
	if err != nil || len(members) != 3 || members[2].Name != memberName || members[2].Status != "failed" {
		t.Fatalf("Members() = %+v, %v; want the three members set", members, err)
	}
	if err := s.ForceLeave(ctx, memberName); err != nil {
		t.Fatalf("ForceLeave: %v", err)
	}

	want := []nomadfake.Call{
		{Name: "Members", Server: addr1},
		{Name: "ForceLeave", Server: addr1, Arg: memberName},
	}
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(seen) != 2 || seen[0] != ctx || seen[1] != ctx {
		t.Errorf("the calls got %d contexts, want the caller's twice", len(seen))
	}
	if got, want := memberNames(t, s), []string{"s1.eu", "s2.eu"}; !slices.Equal(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
}

// TestServersLoseTheAnswerOfAGossipCall checks that each call moves on to the next server after an answer that was
// lost, and that the repeat succeeds: a force-leave of a member that is gone answers 200.
func TestServersLoseTheAnswerOfAGossipCall(t *testing.T) {
	for _, name := range []string{"Members", "ForceLeave"} {
		t.Run(name, func(t *testing.T) {
			f, s := gossipFixture(t, new([]context.Context))
			f.LoseResponse(t, name)

			var err error
			switch name {
			case "Members":
				_, err = s.Members(t.Context())
			case "ForceLeave":
				err = s.ForceLeave(t.Context(), memberName)
			}

			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if want := []string{addr1, addr2}; !slices.Equal(callServers(f, name), want) {
				t.Errorf("servers of the %s calls = %v, want %v", name, callServers(f, name), want)
			}
			if last := s.Last(); last != addr2 {
				t.Errorf("Last() = %q, want %s", last, addr2)
			}
		})
	}
}

// TestServersReturnAPermanentErrorOfAGossipCall checks that a permanent error of Members or ForceLeave comes back as it
// is, and that no other server is asked.
func TestServersReturnAPermanentErrorOfAGossipCall(t *testing.T) {
	permanent := errors.New("forbidden")
	for name, call := range map[string]func(*testing.T, *nomadops.Servers) error{
		"Members": func(t *testing.T, s *nomadops.Servers) error {
			got, err := s.Members(t.Context())
			if got != nil {
				t.Errorf("Members() = %v with an error, want nil", got)
			}
			return err
		},
		"ForceLeave": func(t *testing.T, s *nomadops.Servers) error { return s.ForceLeave(t.Context(), memberName) },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := &stub{err: permanent}, &stub{}
			if err := call(t, servers(t, a, b)); !errors.Is(err, permanent) || err.Error() != permanent.Error() {
				t.Errorf("%s() error = %v, want the permanent error as it is", name, err)
			}
			wantStubCalls(t, a, name)
			wantStubCalls(t, b)
		})
	}
}
