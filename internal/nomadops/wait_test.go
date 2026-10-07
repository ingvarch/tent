package nomadops_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/pki"
)

// leaderAddr is the RPC address of the leader of the waits' cluster.
const leaderAddr = "10.0.0.5:4647"

// newFake returns a cluster without a leader whose ACL system is bootstrapped, and a client of it.
func newFake() (*nomadfake.Fake, nomadops.API) {
	f := nomadfake.New()
	f.SetBootstrapped(pki.NewBootstrapSecret())
	return f, f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
}

// bounded returns the test's context, which ends after a minute, so that a wait that never ends fails the test at once
// instead of polling until the test times out.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// after runs change after d, while a wait polls.
func after(d time.Duration, change func()) {
	go func() {
		time.Sleep(d)
		change()
	}()
}

// wantCalls checks the names of the calls that reached the fake.
func wantCalls(t *testing.T, f *nomadfake.Fake, want ...string) {
	t.Helper()
	var got []string
	for _, c := range f.Calls() {
		got = append(got, c.Name)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// wantTook fails t unless the time since start is d.
func wantTook(t *testing.T, start time.Time, d time.Duration) {
	t.Helper()
	if got := time.Since(start); got != d {
		t.Errorf("the wait took %v, want %v", got, d)
	}
}

// checkErr is checkCallErr for an error that no secret went into.
func checkErr(t *testing.T, err error, want string, notReady bool) {
	t.Helper()
	checkCallErr(t, err, want, notReady, nil)
}

// TestWaitLeader checks that WaitLeader waits 2 seconds after each call through lost answers and a cluster without a
// leader.
func TestWaitLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := newFake()
		f.LoseResponse(t, "Leader")
		after(3*time.Second, func() { f.SetLeader(leaderAddr) })
		start := time.Now()

		got, err := nomadops.WaitLeader(bounded(t), a)

		if err != nil || got != leaderAddr {
			t.Errorf("WaitLeader() = %q, %v; want %s", got, err, leaderAddr)
		}
		// The answer at 0 s was lost, and at 2 s there was no leader.
		wantTook(t, start, 4*time.Second)
		wantCalls(t, f, "Leader", "Leader", "Leader")
	})
}

// addr is the private address of the node that the waits wait for.
var addr = netip.MustParseAddr("10.64.0.6")

// other is the address of another machine.
var other = netip.MustParseAddr("10.64.0.7")

// workers0 returns a node called prod-workers-0 at a with the status, eligible for work when eligible is set.
func workers0(a netip.Addr, status string, eligible bool) nomadops.Node {
	return nomadops.Node{Name: "prod-workers-0", Status: status, Eligible: eligible, Address: a}
}

// TestWaitNode checks that WaitNode goes on until the node of the name at the address is ready and eligible: a node
// that went down, a node that registers, a ready node that is not eligible, another ready node and a ready node of the
// name at another address do not count.
func TestWaitNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := newFake()
		f.SetLeader(leaderAddr)
		f.Register(workers0(addr, "down", true))
		f.Register(nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true, Address: addr})
		f.Register(workers0(other, "ready", true))
		after(time.Second, func() { f.Register(workers0(addr, "initializing", false)) })
		after(3*time.Second, func() { f.Register(workers0(addr, "ready", false)) })
		want := workers0(addr, "ready", true)
		after(5*time.Second, func() { f.Register(want) })
		start := time.Now()

		got, err := nomadops.WaitNode(bounded(t), a, "prod-workers-0", addr)

		if err != nil || got != want {
			t.Errorf("WaitNode() = %+v, %v; want %+v", got, err, want)
		}
		wantTook(t, start, 6*time.Second)
		wantCalls(t, f, "Nodes", "Nodes", "Nodes", "Nodes")
	})
}

// listed is an API whose Nodes lists the same nodes every time, for the nodes of the same name that the fake does
// not list. The waits call nothing else of it.
type listed struct {
	nomadops.API
	nodes []nomadops.Node
}

func (l listed) Nodes(context.Context) ([]nomadops.Node, error) { return l.nodes, nil }

// TestWaitNodeAmongNodesOfTheSameName checks that any ready node of the name at the address ends the wait, whatever
// the others of that name are, and that a ready node at another address never does.
func TestWaitNodeAmongNodesOfTheSameName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := workers0(addr, "ready", true)
		for _, nodes := range [][]nomadops.Node{
			{workers0(addr, "down", true), want},
			{workers0(other, "ready", true), want},
			{want, workers0(addr, "initializing", true)},
		} {
			start := time.Now()
			got, err := nomadops.WaitNode(bounded(t), listed{nodes: nodes}, "prod-workers-0", addr)
			if err != nil || got != want {
				t.Errorf("WaitNode() among %+v = %+v, %v; want %+v", nodes, got, err, want)
			}
			wantTook(t, start, 0)
		}
	})
}

// TestWaitHealthy checks that WaitHealthy goes on until autopilot reports healthy servers and at least the voters
// asked for.
func TestWaitHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := newFake()
		f.SetLeader(leaderAddr)
		f.SetHealth(nomadops.Health{Healthy: false, Voters: 3})
		after(time.Second, func() { f.SetHealth(nomadops.Health{Healthy: true, Voters: 2}) })
		want := nomadops.Health{Healthy: true, Voters: 3}
		after(3*time.Second, func() { f.SetHealth(want) })
		start := time.Now()

		got, err := nomadops.WaitHealthy(bounded(t), a, 3)

		if err != nil || !cmp.Equal(got, want, equateAddrs) {
			t.Errorf("WaitHealthy() = %+v, %v; want %+v", got, err, want)
		}
		wantTook(t, start, 4*time.Second)
		wantCalls(t, f, "Health", "Health", "Health")

		// More voters than asked for will do.
		more := nomadops.Health{Healthy: true, Voters: 5}
		f.SetHealth(more)
		if got, err := nomadops.WaitHealthy(bounded(t), a, 3); err != nil || !cmp.Equal(got, more, equateAddrs) {
			t.Errorf("WaitHealthy() = %+v, %v; want %+v", got, err, more)
		}
	})
}

// TestWaitKeyring checks that WaitKeyring goes on until the keyring has an active key, and calls once when it has.
func TestWaitKeyring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := newFake()
		f.SetLeader(leaderAddr)
		f.SetKeyringDelay(2)
		start := time.Now()

		if err := nomadops.WaitKeyring(bounded(t), a); err != nil {
			t.Errorf("WaitKeyring() = %v, want nil", err)
		}
		wantTook(t, start, 4*time.Second)
		wantCalls(t, f, "KeyringReady", "KeyringReady", "KeyringReady")

		start = time.Now()
		if err := nomadops.WaitKeyring(bounded(t), a); err != nil {
			t.Errorf("WaitKeyring() with a ready keyring = %v, want nil", err)
		}
		wantTook(t, start, 0)
		wantCalls(t, f, "KeyringReady", "KeyringReady", "KeyringReady", "KeyringReady")
	})
}

// wait is a call of one of the waits, on a cluster that it waits for.
type wait struct {
	name string
	call string // the API method that the wait calls
	run  func(context.Context, nomadops.API) error
}

// A wait of each kind: for the leader, for prod-workers-0, for healthy servers with 3 voters, and for the keyring.
var (
	waitLeader = wait{"WaitLeader", "Leader", func(ctx context.Context, a nomadops.API) error {
		got, err := nomadops.WaitLeader(ctx, a)
		return noValue(got, err)
	}}
	waitNode = wait{"WaitNode", "Nodes", func(ctx context.Context, a nomadops.API) error {
		got, err := nomadops.WaitNode(ctx, a, "prod-workers-0", addr)
		return noValue(got, err)
	}}
	waitHealthy = wait{"WaitHealthy", "Health", func(ctx context.Context, a nomadops.API) error {
		got, err := nomadops.WaitHealthy(ctx, a, 3)
		return noValue(got, err)
	}}
	waitKeyring = wait{"WaitKeyring", "KeyringReady", nomadops.WaitKeyring}
	waits       = []wait{waitLeader, waitNode, waitHealthy, waitKeyring}
)

// errValue is the error of a wait that returned a value with its error.
var errValue = errors.New("a value came with the error")

// noValue returns err, or errValue when v is not the zero value although err is set.
func noValue[T any](v T, err error) error {
	var zero T
	if err != nil && !cmp.Equal(v, zero, equateAddrs) {
		return errors.Join(err, errValue)
	}
	return err
}

// TestWaitsStopOnAPermanentError checks that a wait returns an error that does not match ErrNotReady as it is, after
// it retried the errors that do.
func TestWaitsStopOnAPermanentError(t *testing.T) {
	boom := errors.New("nomad: GET /v1/x: 403: Permission denied")
	for _, w := range waits {
		t.Run(w.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, a := newFake()
				f.SetLeader(leaderAddr)
				f.LoseResponse(t, w.call)
				f.Fail(t, w.call, boom)
				start := time.Now()
				err := w.run(bounded(t), a)
				if !errors.Is(err, boom) || err.Error() != boom.Error() {
					t.Errorf("%s() error = %v, want %v as it is", w.name, err, boom)
				}
				wantTook(t, start, 2*time.Second)
				wantCalls(t, f, w.call, w.call)
			})
		})
	}
}

// TestWaitsEndWithTheContext checks that a wait whose context ends returns an error that matches the context's error,
// names what it waited for and why the last call did not end the wait, and does not match ErrNotReady.
func TestWaitsEndWithTheContext(t *testing.T) {
	const ended = ": context deadline exceeded; last: "
	cases := []struct {
		name  string
		setup func(*nomadfake.Fake)
		run   func(context.Context, nomadops.API) error
		want  string
	}{
		{"no leader", func(*nomadfake.Fake) {}, waitLeader.run,
			"nomad: wait for a leader" + ended + "nomadfake: Leader: no leader"},
		{"a node that is not listed", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.Register(nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true, Address: addr})
		}, waitNode.run, "nomad: wait for node prod-workers-0" + ended + "node prod-workers-0 is not listed at 10.64.0.6"},
		{"a node at another address", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.Register(workers0(other, "ready", true))
		}, waitNode.run, "nomad: wait for node prod-workers-0" + ended + "node prod-workers-0 is not listed at 10.64.0.6"},
		{"a node that is initializing", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.Register(workers0(addr, "initializing", false))
		}, waitNode.run, "nomad: wait for node prod-workers-0" + ended + "node prod-workers-0 is initializing"},
		{"a node that went down", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.Register(workers0(addr, "down", true))
		}, waitNode.run, "nomad: wait for node prod-workers-0" + ended + "node prod-workers-0 is down"},
		{"a node that is not eligible", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.Register(workers0(addr, "ready", false))
		}, waitNode.run, "nomad: wait for node prod-workers-0" + ended + "node prod-workers-0 is ready but not eligible"},
		{"nodes of the same name", nil, func(ctx context.Context, _ nomadops.API) error {
			_, err := nomadops.WaitNode(ctx, listed{nodes: []nomadops.Node{
				workers0(addr, "down", true), workers0(addr, "initializing", true), workers0(other, "ready", true),
			}}, "prod-workers-0", addr)
			return err
		}, "nomad: wait for node prod-workers-0" + ended +
			"the nodes named prod-workers-0 at 10.64.0.6 are down and initializing"},
		{"unhealthy servers", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.SetHealth(nomadops.Health{Healthy: false, Voters: 3})
		}, waitHealthy.run, "nomad: wait for healthy servers with at least 3 voters" + ended +
			"the servers are unhealthy, with 3 voters"},
		{"too few voters", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.SetHealth(nomadops.Health{Healthy: true, Voters: 2})
		}, waitHealthy.run, "nomad: wait for healthy servers with at least 3 voters" + ended +
			"the servers are healthy, with 2 voters"},
		{"a keyring without an active key", func(f *nomadfake.Fake) {
			f.SetLeader(leaderAddr)
			f.SetKeyringDelay(100)
		}, waitKeyring.run, "nomad: wait for the keyring" + ended + "the keyring has no active key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, a := newFake()
				if tc.setup != nil {
					tc.setup(f)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				start := time.Now()
				err := tc.run(ctx, a)
				checkErr(t, err, tc.want, false)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false, want true", err)
				}
				wantTook(t, start, 5*time.Second)
			})
		})
	}
}

// TestWaitWithAnEndedContext checks that a wait whose context has ended before its first call names what it waited
// for and nothing else.
func TestWaitWithAnEndedContext(t *testing.T) {
	for _, tc := range []struct {
		w    wait
		want string
	}{
		{waitLeader, "nomad: wait for a leader: context canceled"},
		{waitNode, "nomad: wait for node prod-workers-0: context canceled"},
		{waitHealthy, "nomad: wait for healthy servers with at least 3 voters: context canceled"},
		{waitKeyring, "nomad: wait for the keyring: context canceled"},
	} {
		f, a := newFake()
		f.SetLeader(leaderAddr)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := tc.w.run(ctx, a)
		checkErr(t, err, tc.want, false)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false, want true", err)
		}
		wantCalls(t, f)
	}
}

// leaderFunc is an API whose Leader is the function. The waits call nothing else of it.
type leaderFunc struct {
	nomadops.API
	leader func(context.Context) (string, error)
}

func (l leaderFunc) Leader(ctx context.Context) (string, error) { return l.leader(ctx) }

// TestWaitEndsDuringACall checks that the end of the context wins over the error of the call that it ended, and that
// the error names why the call before did not end the wait.
func TestWaitEndsDuringACall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, fake := newFake()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		a := leaderFunc{leader: func(ctx context.Context) (string, error) {
			if calls++; calls == 2 {
				cancel() // while the call runs
			}
			return fake.Leader(ctx)
		}}
		start := time.Now()
		_, err := nomadops.WaitLeader(ctx, a)
		checkErr(t, err, "nomad: wait for a leader: context canceled; last: nomadfake: Leader: no leader", false)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false, want true", err)
		}
		wantTook(t, start, 2*time.Second)
		wantCalls(t, f, "Leader") // the second call found its context ended
	})
}
