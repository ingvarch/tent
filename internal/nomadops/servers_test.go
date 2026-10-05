package nomadops_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func (s *stub) Nodes(context.Context) ([]nomadops.Node, error) {
	if err := s.record("Nodes"); err != nil {
		return nil, err
	}
	return []nomadops.Node{{Name: "n"}}, nil
}

func (s *stub) Health(context.Context) (nomadops.Health, error) {
	if err := s.record("Health"); err != nil {
		return nomadops.Health{}, err
	}
	return nomadops.Health{Healthy: true, Voters: 3}, nil
}

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
		"Nodes":  func(ctx context.Context, a nomadops.API) error { _, err := a.Nodes(ctx); return err },
		"Health": func(ctx context.Context, a nomadops.API) error { _, err := a.Health(ctx); return err },
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

func (r ctxRecorder) Nodes(ctx context.Context) ([]nomadops.Node, error) {
	r.record(ctx)
	return r.API.Nodes(ctx)
}

func (r ctxRecorder) Health(ctx context.Context) (nomadops.Health, error) {
	r.record(ctx)
	return r.API.Health(ctx)
}

// The arguments, the context itself and the values pass through Servers, and a write moves on after a lost answer.
func TestServersPassArgumentsContextAndValues(t *testing.T) {
	f := nomadfake.New()
	f.SetLeader(leaderAddr)
	f.Register(nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true})
	f.SetHealth(nomadops.Health{Healthy: true, Voters: 3})
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
	if h, err := s.Health(ctx); err != nil || h != (nomadops.Health{Healthy: true, Voters: 3}) {
		t.Errorf("Health() = %+v, %v; want the set one", h, err)
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
	}
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(seen) != 7 { // the calls of Servers: the ones of single have no recorder
		t.Fatalf("%d calls seen, want 7", len(seen))
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
