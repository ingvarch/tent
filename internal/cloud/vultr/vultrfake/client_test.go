package vultrfake_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// The tests in this file hold the fake to the real client: the same requests, and the same errors where the client
// makes its own.

// request is the method and path of a request.
type request struct{ Method, Path string }

// newClient returns a real client of a test server that answers every request with 404 and logs it.
func newClient(t *testing.T) (*vultr.Client, func() []request) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, request{r.Method, r.URL.Path})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Not found.","status":404}`))
	}))
	t.Cleanup(srv.Close)
	c, err := vultr.NewClient("key-5e0b7a", vultr.WithBaseURL(srv.URL), vultr.WithRoundTripper(srv.Client().Transport),
		vultr.WithInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return reqs
	}
}

// apiErrorOf returns err as a *vultr.APIError, and fails the test when it is none.
func apiErrorOf(t *testing.T, err error) *vultr.APIError {
	t.Helper()
	e, ok := errors.AsType[*vultr.APIError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *vultr.APIError", err, err)
	}
	return e
}

func TestSameRequestsAsTheClient(t *testing.T) {
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			client, requests := newClient(t)
			sent := apiErrorOf(t, c.call(t.Context(), client))
			want := []request{{c.method, c.path}}
			if diff := cmp.Diff(want, requests()); diff != "" {
				t.Errorf("the client's requests (-want +got):\n%s", diff)
			}
			f := newSeeded(t)
			f.Throttle(t, c.name, 0, 1)
			faked := apiErrorOf(t, c.call(t.Context(), f))
			if got := (request{faked.Method, faked.Path}); got != (request{sent.Method, sent.Path}) {
				t.Errorf("the fake's error names %v, the client's %s %s", got, sent.Method, sent.Path)
			}
		})
	}
}

func TestSameContextErrorsAsTheClient(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			client, requests := newClient(t)
			want := c.call(canceled, client)
			got := c.call(canceled, newSeeded(t))
			if got == nil || want == nil || got.Error() != want.Error() {
				t.Errorf("the fake's error %v, the client's %v", got, want)
			}
			wantAPIError(t, got, vultr.ErrUnavailable, want.Error())
			if !errors.Is(got, context.Canceled) {
				t.Errorf("err = %v, want it to match context.Canceled", got)
			}
			if n := len(requests()); n != 0 {
				t.Errorf("the client sent %d requests, want none", n)
			}
		})
	}
}

func TestSameIDErrorsAsTheClient(t *testing.T) {
	type idCall func(ctx context.Context, a vultr.API, id string) error
	calls := map[string]idCall{
		"DeleteSSHKey":        func(ctx context.Context, a vultr.API, id string) error { return a.DeleteSSHKey(ctx, id) },
		"DeleteVPC":           func(ctx context.Context, a vultr.API, id string) error { return a.DeleteVPC(ctx, id) },
		"DeleteFirewallGroup": func(ctx context.Context, a vultr.API, id string) error { return a.DeleteFirewallGroup(ctx, id) },
		"ListFirewallRules": func(ctx context.Context, a vultr.API, id string) error {
			_, err := a.ListFirewallRules(ctx, id)
			return err
		},
		"CreateFirewallRule": func(ctx context.Context, a vultr.API, id string) error {
			_, err := a.CreateFirewallRule(ctx, id, &sshRule)
			return err
		},
		"DeleteFirewallRule": func(ctx context.Context, a vultr.API, id string) error {
			return a.DeleteFirewallRule(ctx, id, 1)
		},
		"DeleteFirewallRule with a bad rule id": func(ctx context.Context, a vultr.API, _ string) error {
			return a.DeleteFirewallRule(ctx, "firewall-1", 0)
		},
		"AvailablePlans": func(ctx context.Context, a vultr.API, id string) error {
			_, err := a.AvailablePlans(ctx, id, "vc2")
			return err
		},
		"ListInstances without a tag": func(ctx context.Context, a vultr.API, _ string) error {
			_, err := a.ListInstances(ctx, "")
			return err
		},
		"GetInstance": func(ctx context.Context, a vultr.API, id string) error {
			_, err := a.GetInstance(ctx, id)
			return err
		},
		"DeleteInstance": func(ctx context.Context, a vultr.API, id string) error { return a.DeleteInstance(ctx, id) },
		"HaltInstance":   func(ctx context.Context, a vultr.API, id string) error { return a.HaltInstance(ctx, id) },
		"UpdateInstance": func(ctx context.Context, a vultr.API, id string) error {
			return a.UpdateInstance(ctx, id, &govultr.InstanceUpdateReq{Label: "l"})
		},
		"ListInstanceVPCs": func(ctx context.Context, a vultr.API, id string) error {
			_, err := a.ListInstanceVPCs(ctx, id)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			client, requests := newClient(t)
			f := newSeeded(t)
			for _, id := range []string{"", "a/b", "..", "é"} {
				want := call(t.Context(), client, id)
				got := call(t.Context(), f, id)
				if got == nil || want == nil || got.Error() != want.Error() {
					t.Errorf("id %q: the fake's error %v, the client's %v", id, got, want)
				}
			}
			if n := len(requests()); n != 0 {
				t.Errorf("the client sent %d requests, want none", n)
			}
			wantCalls(t, f)
		})
	}
}

func TestPinnedIDErrors(t *testing.T) {
	f := newFake()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{f.DeleteVPC(t.Context(), ""), `vultr: DELETE /v2/vpcs/{id}: invalid id ""`},
		{f.DeleteFirewallRule(t.Context(), "firewall-1", 0),
			`vultr: DELETE /v2/firewalls/{groupID}/rules/{ruleID}: invalid ruleID 0`},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("err = %v, want %q", tc.err, tc.want)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	const workers, rounds = 10, 5 // 50 rules fill one group, and 5 VPCs a region
	f := newFake()
	group := f.AddFirewallGroup(t, govultr.FirewallGroup{}).ID
	regions := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9"}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			ctx := t.Context()
			for i := range rounds {
				if _, err := f.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"}); err != nil {
					t.Errorf("CreateSSHKey: %v", err)
				}
				if _, err := f.CreateVPC(ctx, &govultr.VPCReq{Region: regions[w]}); err != nil {
					t.Errorf("CreateVPC: %v", err)
				}
				rule := portRule(w*rounds + i + 1) // the group takes no second copy of a rule
				if _, err := f.CreateFirewallRule(ctx, group, &rule); err != nil {
					t.Errorf("CreateFirewallRule: %v", err)
				}
				if _, err := f.ListSSHKeys(ctx); err != nil {
					t.Errorf("ListSSHKeys: %v", err)
				}
				_ = f.Calls()
			}
		})
	}
	wg.Wait()

	keys, err := f.ListSSHKeys(t.Context())
	if err != nil {
		t.Fatalf("ListSSHKeys: %v", err)
	}
	vpcs, err := f.ListVPCs(t.Context())
	if err != nil {
		t.Fatalf("ListVPCs: %v", err)
	}
	rules, err := f.ListFirewallRules(t.Context(), group)
	if err != nil {
		t.Fatalf("ListFirewallRules: %v", err)
	}
	ids := map[string]bool{}
	for _, k := range keys {
		ids[k.ID] = true
	}
	for _, v := range vpcs {
		ids[v.ID] = true
	}
	ruleIDs := map[int]bool{}
	for _, r := range rules {
		ruleIDs[r.ID] = true
	}
	const n = workers * rounds
	if len(keys) != n || len(vpcs) != n || len(rules) != n || len(ids) != 2*n || len(ruleIDs) != n {
		t.Errorf("%d keys, %d VPCs, %d rules, %d distinct ids and %d distinct rule ids; want %d of each kind",
			len(keys), len(vpcs), len(rules), len(ids), len(ruleIDs), n)
	}
	if got, want := len(f.Calls()), 4*n+3; got != want {
		t.Errorf("%d calls, want %d", got, want)
	}
}
