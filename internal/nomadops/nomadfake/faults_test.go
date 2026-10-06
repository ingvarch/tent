package nomadfake_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/pki"
)

// errBoom is an error that a test makes a call return.
var errBoom = errors.New("boom")

func TestEveryCallTakesFaults(t *testing.T) {
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			f, a := newBootstrappedAPI(t)
			f.Fail(t, c.name, errBoom)
			f.LoseResponse(t, c.name)
			if err := c.call(ctx, a); !errors.Is(err, errBoom) || err.Error() != errBoom.Error() {
				t.Errorf("with Fail: %v, want %v as it is", err, errBoom)
			}
			checkErr(t, c.call(ctx, a), "nomadfake: "+c.name+": the answer was lost", true)
			if err := c.call(ctx, a); err != nil {
				t.Errorf("after the faults: %v, want success", err)
			}
			call := nomadfake.Call{Name: c.name, Arg: argOf(c.name)}
			wantCalls(t, f, bootstrapCall, call, call, call)
		})
	}
}

// TestFaultedCallsReturnNoValue checks that a call that fails because of a fault returns no value with the error.
func TestFaultedCallsReturnNoValue(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	ctx := t.Context()
	f.Register(nomadops.Node{Name: "prod-workers-0", Status: "ready", Eligible: true})
	f.SetHealth(nomadops.Health{Healthy: true, Voters: 3})
	for _, fault := range []func(call string){
		func(call string) { f.Fail(t, call, errBoom) },
		func(call string) { f.LoseResponse(t, call) },
	} {
		for _, call := range []string{"Leader", "IntroToken", "CreateToken", "Nodes", "Health"} {
			fault(call)
		}
		if got, err := a.Leader(ctx); err == nil || got != "" {
			t.Errorf("Leader() = %q, %v; want an error and no leader", got, err)
		}
		if got, err := a.IntroToken(ctx, introRequest); err == nil || got != nil {
			t.Errorf("IntroToken() gave %d bytes and the error %v; want an error and no token", len(got), err)
		}
		if got, err := a.CreateToken(ctx, tokenRequest); err == nil || got.Secret != nil || got.Accessor != "" {
			t.Errorf("CreateToken() = %+v, %v; want an error and no token", got, err)
		}
		if got, err := a.Nodes(ctx); err == nil || got != nil {
			t.Errorf("Nodes() = %v, %v; want an error and no list", got, err)
		}
		if got, err := a.Health(ctx); err == nil || !cmp.Equal(got, nomadops.Health{}, equateAddrs) {
			t.Errorf("Health() = %+v, %v; want an error and the zero Health", got, err)
		}
	}
}

func TestFailDoesNothing(t *testing.T) {
	f, a := newAPI()
	f.Fail(t, "Bootstrap", errBoom)
	if err := a.Bootstrap(t.Context(), pki.NewBootstrapSecret()); !errors.Is(err, errBoom) {
		t.Errorf("Bootstrap = %v, want %v", err, errBoom)
	}
	// The secret was not stored: another one bootstraps.
	if err := a.Bootstrap(t.Context(), pki.NewBootstrapSecret()); err != nil {
		t.Errorf("Bootstrap after the fault: %v", err)
	}
}

func TestLoseResponseCarriesTheCallOut(t *testing.T) {
	f, a := newAPI()
	ctx := t.Context()
	s := pki.NewBootstrapSecret()
	f.LoseResponse(t, "Bootstrap")
	checkErr(t, a.Bootstrap(ctx, s), "nomadfake: Bootstrap: the answer was lost", true)
	// The bootstrap was done with s.
	if err := a.Bootstrap(ctx, pki.NewBootstrapSecret()); !errors.Is(err, nomadops.ErrBootstrapMismatch) {
		t.Errorf("Bootstrap with another secret = %v, want ErrBootstrapMismatch", err)
	}
	if err := a.Bootstrap(ctx, s); err != nil {
		t.Errorf("Bootstrap with the secret again: %v", err)
	}
	// A lost answer hides the call's own error too.
	f.LoseResponse(t, "Bootstrap")
	checkErr(t, a.Bootstrap(ctx, pki.NewBootstrapSecret()), "nomadfake: Bootstrap: the answer was lost", true)
}

func TestFaultsApplyInOrder(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	ctx := t.Context()
	other := errors.New("other")
	f.Fail(t, "Leader", errBoom)
	f.LoseResponse(t, "Nodes")
	f.Fail(t, "Leader", other)
	f.LoseResponse(t, "Leader")
	leaderErr := func() error {
		_, err := a.Leader(ctx)
		return err
	}
	nodesErr := func() error {
		_, err := a.Nodes(ctx)
		return err
	}
	for i, step := range []struct {
		call func() error
		want func(error) bool
	}{
		{nodesErr, func(err error) bool { return errors.Is(err, nomadops.ErrNotReady) }},
		{leaderErr, func(err error) bool { return errors.Is(err, errBoom) }},
		{leaderErr, func(err error) bool { return errors.Is(err, other) }},
		{leaderErr, func(err error) bool { return errors.Is(err, nomadops.ErrNotReady) }},
		{leaderErr, func(err error) bool { return err == nil }},
		{nodesErr, func(err error) bool { return err == nil }},
	} {
		if err := step.call(); !step.want(err) {
			t.Errorf("step %d: unexpected %v", i+1, err)
		}
	}
}

// fatalTB is a testing.TB that records Fatalf instead of ending the test.
type fatalTB struct {
	testing.TB
	msgs []string
}

func (tb *fatalTB) Helper() {}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.msgs = append(tb.msgs, fmt.Sprintf(format, args...))
}

func TestFaultMisuse(t *testing.T) {
	f, a := newAPI()
	for _, tc := range []struct {
		name  string
		fault func(testing.TB)
		want  string
	}{
		{
			"Fail with an unknown call", func(tb testing.TB) { f.Fail(tb, "leader", errBoom) },
			`nomadfake: Fail: "leader" is not a method of nomadops.API`,
		},
		{
			"LoseResponse with an unknown call", func(tb testing.TB) { f.LoseResponse(tb, "Register") },
			`nomadfake: LoseResponse: "Register" is not a method of nomadops.API`,
		},
		{
			"Fail without an error", func(tb testing.TB) { f.Fail(tb, "Leader", nil) },
			"nomadfake: Fail Leader: no error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			tc.fault(tb)
			if diff := cmp.Diff([]string{tc.want}, tb.msgs); diff != "" {
				t.Errorf("Fatalf messages (-want +got):\n%s", diff)
			}
		})
	}
	// None of them was set.
	if _, err := a.Leader(t.Context()); err != nil {
		t.Errorf("Leader: %v, want no fault", err)
	}
}
