package vultrfake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// now is the time of the tests' clock, and date its date_created.
var now = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

const date = "2026-09-27T10:00:00+00:00"

// newFake returns a fake whose clock always reads now.
func newFake() *vultrfake.Fake {
	return vultrfake.New(vultrfake.WithClock(func() time.Time { return now }))
}

// sentinels are the classes of a *vultr.APIError.
var sentinels = []error{
	vultr.ErrNotFound, vultr.ErrRateLimited, vultr.ErrLimitReached, vultr.ErrInUse, vultr.ErrInvalid,
	vultr.ErrForbidden, vultr.ErrUnavailable,
}

// wantAPIError checks that err is a *vultr.APIError with the text that matches kind and no other class.
func wantAPIError(t *testing.T, err, kind error, text string) {
	t.Helper()
	if err == nil || err.Error() != text {
		t.Errorf("err = %v, want %q", err, text)
	}
	if _, ok := errors.AsType[*vultr.APIError](err); !ok {
		t.Errorf("err = %v (%T), want a *vultr.APIError", err, err)
	}
	for _, s := range sentinels {
		if got, want := errors.Is(err, s), errors.Is(s, kind); got != want {
			t.Errorf("errors.Is(%v, %v) = %v, want %v", err, s, got, want)
		}
	}
}

// wantCalls checks the fake's log of calls.
func wantCalls(t *testing.T, f *vultrfake.Fake, want ...vultrfake.Call) {
	t.Helper()
	if diff := cmp.Diff(want, f.Calls()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// mustCreateSSHKey creates an SSH key with name and fails the test on an error.
func mustCreateSSHKey(t *testing.T, f *vultrfake.Fake, name string) *govultr.SSHKey {
	t.Helper()
	k, err := f.CreateSSHKey(t.Context(), &govultr.SSHKeyReq{Name: name, SSHKey: "ssh-ed25519 AAAA"})
	if err != nil {
		t.Fatalf("CreateSSHKey %q: %v", name, err)
	}
	return k
}

func TestDefaultClock(t *testing.T) {
	before := time.Now().Truncate(time.Second)
	k, err := vultrfake.New().CreateSSHKey(t.Context(), &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
	after := time.Now()
	if err != nil {
		t.Fatalf("CreateSSHKey: %v", err)
	}
	at, err := time.Parse(time.RFC3339, k.DateCreated)
	if err != nil || at.Before(before) || at.After(after) {
		t.Errorf("date_created %q (%v), want the time of the call as RFC 3339", k.DateCreated, err)
	}
}

func TestClockIsWrittenInUTC(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	f := vultrfake.New(vultrfake.WithClock(func() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 5, cest) }))
	if k := mustCreateSSHKey(t, f, "k"); k.DateCreated != date {
		t.Errorf("date_created %q, want %q", k.DateCreated, date)
	}
}

func TestCalls(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	mustCreateSSHKey(t, f, "a")
	if _, err := f.ListSSHKeys(ctx); err != nil {
		t.Fatalf("ListSSHKeys: %v", err)
	}
	_ = f.DeleteSSHKey(ctx, "ssh-key-9") // a failed call is logged too
	calls := f.Calls()
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateSSHKey", Arg: "a"},
		vultrfake.Call{Name: "ListSSHKeys"},
		vultrfake.Call{Name: "DeleteSSHKey", Arg: "ssh-key-9"},
	)
	calls[0].Name = "changed"
	if got := f.Calls()[0].Name; got != "CreateSSHKey" {
		t.Errorf("changing the returned log changed the fake's: %q", got)
	}
}

func TestContextEnded(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), now)
	defer stop()
	for _, tc := range []struct {
		ctx   context.Context
		cause error
		text  string
	}{
		{canceled, context.Canceled, "vultr: POST /v2/ssh-keys: context canceled"},
		{expired, context.DeadlineExceeded, "vultr: POST /v2/ssh-keys: context deadline exceeded"},
	} {
		f := newFake()
		k, err := f.CreateSSHKey(tc.ctx, &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
		wantAPIError(t, err, vultr.ErrUnavailable, tc.text)
		if !errors.Is(err, tc.cause) || k != nil {
			t.Errorf("CreateSSHKey = %v, %v; want no key and an error that matches %v", k, err, tc.cause)
		}
		// Nothing was done, and the API got no call.
		wantCalls(t, f)
		if keys, err := f.ListSSHKeys(t.Context()); err != nil || len(keys) != 0 {
			t.Errorf("ListSSHKeys = %v, %v; want none", keys, err)
		}
	}
}

func TestBadIDFailsLikeTheClient(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the id is checked first, as the client does
	err := f.DeleteSSHKey(ctx, "../x")
	const want = `vultr: DELETE /v2/ssh-keys/{id}: invalid id "../x"`
	if err == nil || err.Error() != want {
		t.Errorf("DeleteSSHKey = %v, want %q", err, want)
	}
	wantCalls(t, f)
}

func TestListsReturnCopies(t *testing.T) {
	f := newFake()
	k := mustCreateSSHKey(t, f, "a")
	k.Name = "changed"
	keys, err := f.ListSSHKeys(t.Context())
	if err != nil || len(keys) != 1 || keys[0].Name != "a" {
		t.Fatalf("ListSSHKeys = %v, %v; want the key named a", keys, err)
	}
	keys[0].Name = "changed"
	if keys, _ := f.ListSSHKeys(t.Context()); keys[0].Name != "a" {
		t.Errorf("changing a returned list changed the fake: %v", keys)
	}
}

func TestGetters(t *testing.T) {
	f := newFake()
	keys, vpcs, groups, rules := f.SSHKeys(), f.VPCs(), f.FirewallGroups(), f.FirewallRules("firewall-1")
	if keys != nil || vpcs != nil || groups != nil || rules != nil {
		t.Errorf("a new fake has %v, %v, %v and %v; want nothing", keys, vpcs, groups, rules)
	}
	k := f.AddSSHKey(t, govultr.SSHKey{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
	v := f.AddVPC(t, govultr.VPC{Region: "ams", Description: "d"})
	g := f.AddFirewallGroup(t, govultr.FirewallGroup{Description: "g"})
	empty := f.AddFirewallGroup(t, govultr.FirewallGroup{Description: "empty"})
	r := f.AddFirewallRule(t, g.ID, govultr.FirewallRule{IPType: "v4", Protocol: "icmp"})
	g.RuleCount = 1
	// The getters make no call, so faults do not touch them.
	for _, name := range []string{"ListSSHKeys", "ListVPCs", "ListFirewallGroups", "ListFirewallRules"} {
		f.Fail(t, name, errBoom, 1)
	}

	check := func() {
		t.Helper()
		if diff := cmp.Diff([]govultr.SSHKey{k}, f.SSHKeys()); diff != "" {
			t.Errorf("SSHKeys (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]govultr.VPC{v}, f.VPCs()); diff != "" {
			t.Errorf("VPCs (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]govultr.FirewallGroup{g, empty}, f.FirewallGroups()); diff != "" {
			t.Errorf("FirewallGroups (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]govultr.FirewallRule{r}, f.FirewallRules(g.ID)); diff != "" {
			t.Errorf("FirewallRules (-want +got):\n%s", diff)
		}
		if rules := f.FirewallRules(empty.ID); rules != nil {
			t.Errorf("FirewallRules of a group without rules = %v, want nil", rules)
		}
		if rules := f.FirewallRules("firewall-9"); rules != nil {
			t.Errorf("FirewallRules of an unknown group = %v, want nil", rules)
		}
	}
	check()
	// They return copies.
	f.SSHKeys()[0].Name = "changed"
	f.VPCs()[0].Region = "changed"
	f.FirewallGroups()[0].Description = "changed"
	f.FirewallRules(g.ID)[0].Port = "changed"
	check()
	wantCalls(t, f)
	if _, err := f.ListSSHKeys(t.Context()); !errors.Is(err, errBoom) {
		t.Errorf("ListSSHKeys = %v, want the fault that the getters left", err)
	}
}
