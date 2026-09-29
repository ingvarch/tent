package nomadfake_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// leader is the RPC address of the leader of the tests' cluster.
const leader = "10.0.0.5:4647"

// redacted is how a bootstrap secret, a UUID, prints.
const redacted = "[secret, 36 bytes]"

// bootstrapSecret is the secret that the calls of apiCalls bootstrap with.
var bootstrapSecret = pki.NewBootstrapSecret()

// introRequest asks for an introduction token for prod-workers-1 in the pool default, for 30 minutes.
var introRequest = nomadops.IntroRequest{NodeName: "prod-workers-1", NodePool: "default", TTL: 30 * time.Minute}

// newAPI returns a fake that has a leader, and a client of it with a new token.
func newAPI() (*nomadfake.Fake, nomadops.API) {
	f := nomadfake.New()
	f.SetLeader(leader)
	return f, f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
}

// wantCalls checks the fake's log of calls.
func wantCalls(t *testing.T, f *nomadfake.Fake, want ...nomadfake.Call) {
	t.Helper()
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// checkErr fails t unless err is want, as text, and matches nomadops.ErrNotReady exactly when notReady is set.
func checkErr(t *testing.T, err error, want string, notReady bool) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatalf("no error, want %q", want)
	case err.Error() != want:
		t.Errorf("error = %v, want %q", err, want)
	}
	if errors.Is(err, nomadops.ErrNotReady) != notReady {
		t.Errorf("errors.Is(%v, ErrNotReady) = %v, want %v", err, !notReady, notReady)
	}
}

// apiCall is a call of one nomadops.API method that succeeds on a fake with a leader.
type apiCall struct {
	name string // the method
	call func(context.Context, nomadops.API) error
}

// apiCalls holds an apiCall for every nomadops.API method.
var apiCalls = []apiCall{
	{"Leader", func(ctx context.Context, a nomadops.API) error {
		_, err := a.Leader(ctx)
		return err
	}},
	{"Bootstrap", func(ctx context.Context, a nomadops.API) error { return a.Bootstrap(ctx, bootstrapSecret) }},
	{"IntroToken", func(ctx context.Context, a nomadops.API) error {
		_, err := a.IntroToken(ctx, introRequest)
		return err
	}},
	{"Nodes", func(ctx context.Context, a nomadops.API) error {
		_, err := a.Nodes(ctx)
		return err
	}},
	{"Health", func(ctx context.Context, a nomadops.API) error {
		_, err := a.Health(ctx)
		return err
	}},
}

func TestAPICallsCoverTheAPI(t *testing.T) {
	var want, got []string
	api := reflect.TypeFor[nomadops.API]()
	for i := range api.NumMethod() {
		want = append(want, api.Method(i).Name)
	}
	for _, c := range apiCalls {
		got = append(got, c.name)
	}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("apiCalls (-the API +the table):\n%s", diff)
	}
}

func TestLeader(t *testing.T) {
	f := nomadfake.New()
	a := f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
	got, err := a.Leader(t.Context())
	checkErr(t, err, "nomadfake: Leader: no leader", true)
	if got != "" {
		t.Errorf("Leader() = %q with the error, want none", got)
	}
	f.SetLeader(leader)
	if got, err := a.Leader(t.Context()); err != nil || got != leader {
		t.Errorf("Leader() = %q, %v; want %s", got, err, leader)
	}
	f.SetLeader("") // the cluster lost its leader
	_, err = a.Leader(t.Context())
	checkErr(t, err, "nomadfake: Leader: no leader", true)
	wantCalls(t, f, nomadfake.Call{Name: "Leader"}, nomadfake.Call{Name: "Leader"}, nomadfake.Call{Name: "Leader"})
}

// TestEveryCallNeedsALeader checks that a cluster without a leader answers no call, as Nomad's servers answer 500 "No
// cluster leader".
func TestEveryCallNeedsALeader(t *testing.T) {
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			f := nomadfake.New()
			a := f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
			checkErr(t, c.call(t.Context(), a), "nomadfake: "+c.name+": no leader", true)
			wantCalls(t, f, nomadfake.Call{Name: c.name, Arg: argOf(c.name)})
			// Nothing was done: a bootstrap with another secret is the first.
			f.SetLeader(leader)
			if err := a.Bootstrap(t.Context(), pki.NewBootstrapSecret()); err != nil {
				t.Errorf("Bootstrap after the call: %v", err)
			}
		})
	}
}

// argOf returns the Arg that Calls logs for the call of apiCalls with the name.
func argOf(name string) string {
	switch name {
	case "Bootstrap":
		return redacted
	case "IntroToken":
		return "prod-workers-1 default 30m0s"
	}
	return ""
}

func TestBootstrap(t *testing.T) {
	f, a := newAPI()
	ctx := t.Context()
	s, other := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	if err := a.Bootstrap(ctx, s); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// A repeat with the same secret finds the bootstrap done by it, from any client.
	if err := a.Bootstrap(ctx, s); err != nil {
		t.Errorf("Bootstrap again: %v", err)
	}
	if err := f.Client(nomadops.Config{Token: s}).Bootstrap(ctx, s); err != nil {
		t.Errorf("Bootstrap from another client: %v", err)
	}
	err := a.Bootstrap(ctx, other)
	checkErr(t, err, "nomadfake: Bootstrap: the ACL system was bootstrapped with a secret other than the one in "+
		"secrets/acl-bootstrap-token", false)
	if !errors.Is(err, nomadops.ErrBootstrapMismatch) {
		t.Errorf("errors.Is(%v, ErrBootstrapMismatch) = false, want true", err)
	}
	secrettest.CheckHidden(t, map[string]string{"the error": err.Error()},
		map[string][]byte{"the secret": s, "the other secret": other}, "")
	// The first secret stays.
	if err := a.Bootstrap(ctx, s); err != nil {
		t.Errorf("Bootstrap with the first secret after the mismatch: %v", err)
	}
	bootstrap := nomadfake.Call{Name: "Bootstrap", Arg: redacted}
	wantCalls(t, f, bootstrap, bootstrap, bootstrap, bootstrap, bootstrap)
}

// TestBootstrapKeepsACopy checks that a change to the caller's secret after a bootstrap does not change the secret
// that the ACL system was bootstrapped with.
func TestBootstrapKeepsACopy(t *testing.T) {
	_, a := newAPI()
	s := pki.NewBootstrapSecret()
	if err := a.Bootstrap(t.Context(), s); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	orig := slices.Clone(s)
	s[0]++
	if err := a.Bootstrap(t.Context(), orig); err != nil {
		t.Errorf("Bootstrap with the secret as it was: %v", err)
	}
	s = orig
	if err := a.Bootstrap(t.Context(), s); err != nil {
		t.Errorf("Bootstrap with the secret restored: %v", err)
	}
}

// TestBootstrapRefusesABadSecret checks that the fake refuses a secret that is not a lower-case UUID of version 4
// before any call, as the client does: Nomad refuses such a secret for good, or makes one that nobody knows for an
// empty one.
func TestBootstrapRefusesABadSecret(t *testing.T) {
	f, a := newAPI()
	for _, s := range []secret.Secret{
		nil, {}, secret.Secret("root"), secret.Secret(strings.ToUpper(string(pki.NewBootstrapSecret()))),
	} {
		err := a.Bootstrap(t.Context(), s)
		checkErr(t, err, "nomadfake: Bootstrap: ACL bootstrap secret: not a lower-case UUID of version 4", false)
		secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, map[string][]byte{"the secret": s}, "")
	}
	wantCalls(t, f)
	if err := a.Bootstrap(t.Context(), pki.NewBootstrapSecret()); err != nil {
		t.Errorf("Bootstrap after the refusals: %v", err)
	}
}

// claims decodes the payload of a JWT without a signature, as the fake makes them, and fails t for anything else.
// Its messages never show the JWT.
func claims(t *testing.T, jwt secret.Secret) map[string]string {
	t.Helper()
	parts := strings.Split(string(jwt), ".")
	if len(parts) != 3 || parts[2] != "" {
		t.Fatalf("the JWT has %d parts or a signature, want a header, claims and no signature", len(parts))
	}
	var header, payload map[string]string
	for i, v := range []*map[string]string{&header, &payload} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("part %d of the JWT is not base64url: %v", i+1, err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("part %d of the JWT is not a JSON object of strings: %v", i+1, err)
		}
	}
	if want := map[string]string{"alg": "none", "typ": "JWT"}; !cmp.Equal(header, want) {
		t.Errorf("the JWT's header = %v, want %v", header, want)
	}
	return payload
}

func TestIntroToken(t *testing.T) {
	f, a := newAPI()
	ctx := t.Context()
	jwt, err := a.IntroToken(ctx, introRequest)
	if err != nil {
		t.Fatalf("IntroToken: %v", err)
	}
	want := map[string]string{"nomad_node_name": "prod-workers-1", "nomad_node_pool": "default"}
	if got := claims(t, jwt); !cmp.Equal(got, want) {
		t.Errorf("the JWT's claims = %v, want %v", got, want)
	}
	secrettest.CheckHidden(t, secrettest.Printed(t, jwt), map[string][]byte{"the JWT": jwt}, "[secret, ")

	// The token depends on the node and the pool alone.
	longer := introRequest
	longer.TTL = time.Minute
	if again, err := a.IntroToken(ctx, longer); err != nil || !bytes.Equal(again, jwt) {
		t.Errorf("IntroToken for the same node and pool gave another token (error %v), want the same", err)
	}
	for _, change := range []func(*nomadops.IntroRequest){
		func(r *nomadops.IntroRequest) { r.NodeName = "prod-workers-2" },
		func(r *nomadops.IntroRequest) { r.NodePool = "gpu" },
	} {
		req := introRequest
		change(&req)
		other, err := a.IntroToken(ctx, req)
		if err != nil {
			t.Fatalf("IntroToken(%+v): %v", req, err)
		}
		if bytes.Equal(other, jwt) {
			t.Errorf("IntroToken(%+v) gave the token of %+v", req, introRequest)
		}
		want := map[string]string{"nomad_node_name": req.NodeName, "nomad_node_pool": req.NodePool}
		if got := claims(t, other); !cmp.Equal(got, want) {
			t.Errorf("the claims of IntroToken(%+v) = %v, want %v", req, got, want)
		}
	}
	wantCalls(t, f,
		nomadfake.Call{Name: "IntroToken", Arg: "prod-workers-1 default 30m0s"},
		nomadfake.Call{Name: "IntroToken", Arg: "prod-workers-1 default 1m0s"},
		nomadfake.Call{Name: "IntroToken", Arg: "prod-workers-2 default 30m0s"},
		nomadfake.Call{Name: "IntroToken", Arg: "prod-workers-1 gpu 30m0s"},
	)
}

// TestIntroTokenRefusesABadRequest checks that the fake checks the request before any call, as the client does.
func TestIntroTokenRefusesABadRequest(t *testing.T) {
	f, a := newAPI()
	for _, tc := range []struct {
		change func(*nomadops.IntroRequest)
		want   string
	}{
		{func(r *nomadops.IntroRequest) { r.NodeName = "" }, "nomadfake: IntroToken: intro token: no node name"},
		{func(r *nomadops.IntroRequest) { r.NodePool = "" }, "nomadfake: IntroToken: intro token: no node pool"},
		{func(r *nomadops.IntroRequest) { r.TTL = 0 },
			"nomadfake: IntroToken: intro token: TTL 0s is not above zero and at most 30m0s"},
		{func(r *nomadops.IntroRequest) { r.TTL = nomadops.MaxIntroTTL + time.Second },
			"nomadfake: IntroToken: intro token: TTL 30m1s is not above zero and at most 30m0s"},
	} {
		req := introRequest
		tc.change(&req)
		jwt, err := a.IntroToken(t.Context(), req)
		checkErr(t, err, tc.want, false)
		if jwt != nil {
			t.Errorf("IntroToken(%+v) gave a token of %d bytes with the error", req, len(jwt))
		}
	}
	wantCalls(t, f)
}

func TestNodes(t *testing.T) {
	f, a := newAPI()
	ctx := t.Context()
	got, err := a.Nodes(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Nodes() of a new fake = %#v, %v; want an empty list, as the client gives", got, err)
	}
	f.Register(nomadops.Node{Name: "prod-workers-0", Status: "initializing"})
	f.Register(nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true})
	f.Register(nomadops.Node{Name: "prod-workers-0", Status: "ready", Eligible: true}) // replaces the first
	want := []nomadops.Node{
		{Name: "prod-workers-0", Status: "ready", Eligible: true},
		{Name: "prod-workers-1", Status: "ready", Eligible: true},
	}
	got, err = a.Nodes(ctx)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
	got[0].Status = "down"
	if got, _ := a.Nodes(ctx); got[0].Status != "ready" {
		t.Errorf("changing a returned list changed the fake: %+v", got)
	}
	wantCalls(t, f, nomadfake.Call{Name: "Nodes"}, nomadfake.Call{Name: "Nodes"}, nomadfake.Call{Name: "Nodes"})
}

func TestHealth(t *testing.T) {
	f, a := newAPI()
	if got, err := a.Health(t.Context()); err != nil || got != (nomadops.Health{}) {
		t.Errorf("Health() of a new fake = %+v, %v; want the zero Health", got, err)
	}
	want := nomadops.Health{Healthy: true, Voters: 3}
	f.SetHealth(want)
	if got, err := a.Health(t.Context()); err != nil || got != want {
		t.Errorf("Health() = %+v, %v; want %+v", got, err, want)
	}
	wantCalls(t, f, nomadfake.Call{Name: "Health"}, nomadfake.Call{Name: "Health"})
}

// TestContextEnded checks that a call whose context has ended fails as the client's does, reaches nothing and is not
// logged.
func TestContextEnded(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now())
	defer stop()
	for _, c := range apiCalls {
		for _, tc := range []struct {
			ctx   context.Context
			cause error
		}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}} {
			f, a := newAPI()
			err := c.call(tc.ctx, a)
			checkErr(t, err, "nomadfake: "+c.name+": "+tc.cause.Error(), false)
			if !errors.Is(err, tc.cause) {
				t.Errorf("%s: errors.Is(%v, %v) = false, want true", c.name, err, tc.cause)
			}
			wantCalls(t, f)
			if err := a.Bootstrap(t.Context(), pki.NewBootstrapSecret()); err != nil {
				t.Errorf("%s: Bootstrap after the call: %v", c.name, err)
			}
		}
	}
}

func TestTokens(t *testing.T) {
	f := nomadfake.New()
	if got := f.Tokens(); len(got) != 0 {
		t.Errorf("a new fake has %d tokens, want none", len(got))
	}
	first, second := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	a := f.Client(nomadops.Config{Address: "203.0.113.5:4646", Region: "eu", Token: first})
	f.Client(nomadops.Config{Token: second})
	f.Client(nomadops.Config{})
	check := func() {
		t.Helper()
		got := f.Tokens()
		if len(got) != 3 || !bytes.Equal(got[0], first) || !bytes.Equal(got[1], second) || len(got[2]) != 0 {
			t.Errorf("Tokens() gives %d tokens, want the first, the second and an empty one, in order", len(got))
		}
	}
	check()
	// The fake keeps copies.
	orig := slices.Clone(first)
	first[0]++
	f.Tokens()[1][0]++
	first = orig
	check()

	// Neither the client nor the fake prints a token.
	f.SetLeader(leader)
	if err := a.Bootstrap(t.Context(), first); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	secrets := map[string][]byte{"the first token": first, "the second token": second}
	secrettest.CheckHidden(t, secrettest.Printed(t, a), secrets, "")
	secrettest.CheckHidden(t, secrettest.Printed(t, f), secrets, "")
}

// TestCallsHideSecrets checks that the log of calls shows no secret: neither a client's token, nor a bootstrap
// secret, nor a JWT.
func TestCallsHideSecrets(t *testing.T) {
	f := nomadfake.New()
	f.SetLeader(leader)
	token, s := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	a := f.Client(nomadops.Config{Token: token})
	if err := a.Bootstrap(t.Context(), s); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	jwt, err := a.IntroToken(t.Context(), introRequest)
	if err != nil {
		t.Fatalf("IntroToken: %v", err)
	}
	wantCalls(t, f,
		nomadfake.Call{Name: "Bootstrap", Arg: redacted},
		nomadfake.Call{Name: "IntroToken", Arg: "prod-workers-1 default 30m0s"},
	)
	secrettest.CheckHidden(t, secrettest.Printed(t, f.Calls()),
		map[string][]byte{"the token": token, "the bootstrap secret": s, "the JWT": jwt}, "")
}

func TestConcurrentUse(t *testing.T) {
	const workers, rounds = 10, 5
	f := nomadfake.New()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			ctx := t.Context()
			for i := range rounds {
				f.SetLeader(leader)
				f.Register(nomadops.Node{Name: "prod-workers-" + strconv.Itoa(w), Status: "ready", Eligible: true})
				f.SetHealth(nomadops.Health{Healthy: true, Voters: i})
				a := f.Client(nomadops.Config{Token: pki.NewBootstrapSecret()})
				for _, c := range apiCalls {
					if err := c.call(ctx, a); err != nil {
						t.Errorf("%s: %v", c.name, err)
					}
				}
				_, _ = f.Calls(), f.Tokens()
			}
		})
	}
	wg.Wait()

	if got, want := len(f.Calls()), workers*rounds*len(apiCalls); got != want {
		t.Errorf("%d calls logged, want %d", got, want)
	}
	if got, want := len(f.Tokens()), workers*rounds; got != want {
		t.Errorf("%d tokens recorded, want %d", got, want)
	}
	if nodes, err := f.Client(nomadops.Config{}).Nodes(t.Context()); err != nil || len(nodes) != workers {
		t.Errorf("Nodes() = %d nodes, %v; want %d", len(nodes), err, workers)
	}
}
