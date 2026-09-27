package vultrfake_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// errBoom is an error that a test makes a call return.
var errBoom = errors.New("boom")

// apiCall is a call of one vultr.API method that succeeds on a fake from newSeeded.
type apiCall struct {
	name   string // the method
	method string // of the HTTP request
	path   string // of the HTTP request
	call   func(context.Context, vultr.API) error
}

// apiCalls holds an apiCall for every vultr.API method.
var apiCalls = []apiCall{
	{"ListSSHKeys", "GET", "/v2/ssh-keys", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListSSHKeys(ctx)
		return err
	}},
	{"CreateSSHKey", "POST", "/v2/ssh-keys", func(ctx context.Context, a vultr.API) error {
		_, err := a.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
		return err
	}},
	{"DeleteSSHKey", "DELETE", "/v2/ssh-keys/ssh-key-1", func(ctx context.Context, a vultr.API) error {
		return a.DeleteSSHKey(ctx, "ssh-key-1")
	}},
	{"ListVPCs", "GET", "/v2/vpcs", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListVPCs(ctx)
		return err
	}},
	{"CreateVPC", "POST", "/v2/vpcs", func(ctx context.Context, a vultr.API) error {
		_, err := a.CreateVPC(ctx, &govultr.VPCReq{Region: "ams", Description: "d"})
		return err
	}},
	{"DeleteVPC", "DELETE", "/v2/vpcs/vpc-1", func(ctx context.Context, a vultr.API) error {
		return a.DeleteVPC(ctx, "vpc-1")
	}},
	{"ListFirewallGroups", "GET", "/v2/firewalls", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListFirewallGroups(ctx)
		return err
	}},
	{"CreateFirewallGroup", "POST", "/v2/firewalls", func(ctx context.Context, a vultr.API) error {
		_, err := a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "d"})
		return err
	}},
	{"DeleteFirewallGroup", "DELETE", "/v2/firewalls/firewall-1", func(ctx context.Context, a vultr.API) error {
		return a.DeleteFirewallGroup(ctx, "firewall-1")
	}},
	{"ListFirewallRules", "GET", "/v2/firewalls/firewall-1/rules", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListFirewallRules(ctx, "firewall-1")
		return err
	}},
	{"CreateFirewallRule", "POST", "/v2/firewalls/firewall-1/rules", func(ctx context.Context, a vultr.API) error {
		_, err := a.CreateFirewallRule(ctx, "firewall-1", &sshRule)
		return err
	}},
	{"DeleteFirewallRule", "DELETE", "/v2/firewalls/firewall-1/rules/1", func(ctx context.Context, a vultr.API) error {
		return a.DeleteFirewallRule(ctx, "firewall-1", 1)
	}},
	{"AvailablePlans", "GET", "/v2/regions/ams/availability", func(ctx context.Context, a vultr.API) error {
		_, err := a.AvailablePlans(ctx, "ams", "vc2")
		return err
	}},
	{"ListPlans", "GET", "/v2/plans", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListPlans(ctx, "vc2")
		return err
	}},
	{"ListOS", "GET", "/v2/os", func(ctx context.Context, a vultr.API) error {
		_, err := a.ListOS(ctx)
		return err
	}},
}

func TestAPICallsCoverTheAPI(t *testing.T) {
	var want, got []string
	api := reflect.TypeFor[vultr.API]()
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

// newSeeded returns a fake with the SSH key ssh-key-1, the VPC vpc-1, and the firewall group firewall-1 with the
// rule 1.
func newSeeded(t *testing.T) *vultrfake.Fake {
	t.Helper()
	f := newFake()
	f.AddSSHKey(t, govultr.SSHKey{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
	f.AddVPC(t, govultr.VPC{Region: "ams"})
	f.AddFirewallGroup(t, govultr.FirewallGroup{})
	f.AddFirewallRule(t, "firewall-1", govultr.FirewallRule{IPType: "v4", Protocol: "icmp"})
	return f
}

func TestEveryCallTakesFaults(t *testing.T) {
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			f := newSeeded(t)
			f.Fail(t, c.name, errBoom, 1)
			f.Throttle(t, c.name, time.Second, 1)
			f.LoseResponse(t, c.name, 1)
			if err := c.call(ctx, f); !errors.Is(err, errBoom) || err.Error() != errBoom.Error() {
				t.Errorf("with Fail: %v, want %v as it is", err, errBoom)
			}
			wantAPIError(t, c.call(ctx, f), vultr.ErrRateLimited,
				"vultr: "+c.method+" "+c.path+": 429 Too Many Requests: Rate limit exceeded")
			wantAPIError(t, c.call(ctx, f), vultr.ErrUnavailable,
				"vultr: "+c.method+" "+c.path+": vultrfake: the answer was lost")
			// The faults are used up. A delete finds its object gone: the call whose answer was lost deleted it.
			err := c.call(ctx, f)
			gone := strings.HasPrefix(c.name, "Delete") && errors.Is(err, vultr.ErrNotFound)
			if err != nil && !gone {
				t.Errorf("after the faults: %v, want success", err)
			}
			want := slices.Repeat([]string{c.name}, 4)
			var got []string
			for _, call := range f.Calls() {
				got = append(got, call.Name)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFailDoesNothing(t *testing.T) {
	f := newFake()
	f.Fail(t, "CreateVPC", errBoom, 2)
	for range 2 {
		if v, err := f.CreateVPC(t.Context(), &govultr.VPCReq{Region: "ams"}); !errors.Is(err, errBoom) || v != nil {
			t.Errorf("CreateVPC = %+v, %v; want %v", v, err, errBoom)
		}
	}
	wantVPCs(t, f)
	if v, err := f.CreateVPC(t.Context(), &govultr.VPCReq{Region: "ams"}); err != nil || v.ID != "vpc-1" {
		t.Errorf("CreateVPC after the faults = %+v, %v; want vpc-1", v, err)
	}
}

func TestLoseResponseCarriesTheCallOut(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	f.LoseResponse(t, "CreateVPC", 1)
	v, err := f.CreateVPC(ctx, &govultr.VPCReq{Region: "ams", Description: "tent:cluster=prod;kind=vpc;op=1"})
	wantAPIError(t, err, vultr.ErrUnavailable, "vultr: POST /v2/vpcs: vultrfake: the answer was lost")
	if v != nil {
		t.Errorf("CreateVPC returned %+v with the error", v)
	}
	// The VPC exists: a search for the operation finds it.
	wantVPCs(t, f, govultr.VPC{
		ID: "vpc-1", Region: "ams", Description: "tent:cluster=prod;kind=vpc;op=1", DateCreated: date,
	})

	f.LoseResponse(t, "DeleteVPC", 2)
	wantAPIError(t, f.DeleteVPC(ctx, "vpc-1"), vultr.ErrUnavailable,
		"vultr: DELETE /v2/vpcs/vpc-1: vultrfake: the answer was lost")
	wantVPCs(t, f)
	// A lost answer hides the call's own error too.
	wantAPIError(t, f.DeleteVPC(ctx, "vpc-1"), vultr.ErrUnavailable,
		"vultr: DELETE /v2/vpcs/vpc-1: vultrfake: the answer was lost")
}

func TestThrottleDoesNothing(t *testing.T) {
	f := newFake()
	f.Throttle(t, "CreateSSHKey", 3*time.Second, 1)
	k, err := f.CreateSSHKey(t.Context(), &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
	wantAPIError(t, err, vultr.ErrRateLimited, "vultr: POST /v2/ssh-keys: 429 Too Many Requests: Rate limit exceeded")
	if e, ok := errors.AsType[*vultr.APIError](err); !ok || e.RetryAfter != 3*time.Second || e.Status != 429 {
		t.Errorf("err = %+v, want a 429 that asks to wait 3s", err)
	}
	if k != nil {
		t.Errorf("CreateSSHKey returned %+v with the error", k)
	}
	wantSSHKeys(t, f)
}

func TestFaultsApplyInOrder(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	other := errors.New("other")
	f.Fail(t, "CreateVPC", errBoom, 2)
	f.Throttle(t, "CreateVPC", 0, 1)
	f.LoseResponse(t, "ListVPCs", 1)
	f.Fail(t, "CreateVPC", other, 1)
	createVPC := func() error {
		_, err := f.CreateVPC(ctx, &govultr.VPCReq{Region: "ams"})
		return err
	}
	listVPCs := func() error {
		_, err := f.ListVPCs(ctx)
		return err
	}
	for i, step := range []struct {
		call func() error
		want func(error) bool
	}{
		{createVPC, func(err error) bool { return errors.Is(err, errBoom) }},
		{listVPCs, func(err error) bool { return errors.Is(err, vultr.ErrUnavailable) }},
		{createVPC, func(err error) bool { return errors.Is(err, errBoom) }},
		{createVPC, func(err error) bool { return errors.Is(err, vultr.ErrRateLimited) }},
		{createVPC, func(err error) bool { return errors.Is(err, other) }},
		{createVPC, func(err error) bool { return err == nil }},
		{listVPCs, func(err error) bool { return err == nil }},
	} {
		if err := step.call(); !step.want(err) {
			t.Errorf("step %d: unexpected %v", i+1, err)
		}
	}
	wantVPCs(t, f, govultr.VPC{ID: "vpc-1", Region: "ams", DateCreated: date})
}

func TestFaultMisuse(t *testing.T) {
	f := newFake()
	for _, tc := range []struct {
		name  string
		fault func(testing.TB)
		want  string
	}{
		{
			"Fail with an unknown call", func(tb testing.TB) { f.Fail(tb, "CreateVpc", errBoom, 1) },
			`vultrfake: Fail: "CreateVpc" is not a method of vultr.API`,
		},
		{
			"LoseResponse with an unknown call", func(tb testing.TB) { f.LoseResponse(tb, "AddVPC", 1) },
			`vultrfake: LoseResponse: "AddVPC" is not a method of vultr.API`,
		},
		{
			"Throttle with an unknown call", func(tb testing.TB) { f.Throttle(tb, "", time.Second, 1) },
			`vultrfake: Throttle: "" is not a method of vultr.API`,
		},
		{
			"Fail without an error", func(tb testing.TB) { f.Fail(tb, "CreateVPC", nil, 1) },
			"vultrfake: Fail CreateVPC: no error",
		},
		{
			"Fail for no call", func(tb testing.TB) { f.Fail(tb, "CreateVPC", errBoom, 0) },
			"vultrfake: Fail CreateVPC: times is 0, want 1 or more",
		},
		{
			"LoseResponse for no call", func(tb testing.TB) { f.LoseResponse(tb, "CreateVPC", -1) },
			"vultrfake: LoseResponse CreateVPC: times is -1, want 1 or more",
		},
		{
			"Throttle with a negative wait", func(tb testing.TB) { f.Throttle(tb, "CreateVPC", -time.Second, 1) },
			"vultrfake: Throttle CreateVPC: retryAfter is -1s, want 0 or more",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			tc.fault(tb)
			wantFatal(t, tb, tc.want)
		})
	}
	// None of them was set.
	if _, err := f.CreateVPC(t.Context(), &govultr.VPCReq{Region: "ams"}); err != nil {
		t.Errorf("CreateVPC: %v, want no fault", err)
	}
}
