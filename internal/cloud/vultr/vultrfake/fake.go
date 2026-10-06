// Package vultrfake is an in-memory Vultr API for tests. Its Fake implements vultr.API with the client's types and
// errors, so provider code runs on it as on the real client. A test seeds it with objects and makes chosen calls
// fail, get throttled, or lose their answer after the fake carried them out.
package vultrfake

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// Fake is an in-memory vultr.API. It is safe for concurrent use.
//
// A call first checks the ids that the client would put into its path, and fails with the client's error for a bad
// one. Then a call whose context has ended fails as the client's does: with an ErrUnavailable *vultr.APIError that
// matches the context's error. Every other call reaches the fake's API: Calls logs it, and it is carried out unless a
// fault applies. Its errors are *vultr.APIError values with the method, path, status and class that the client
// gives; a missing id, for example, is vultr.ErrNotFound. Lists are whole and in creation order, and every value the
// fake returns is a copy. New objects get ids such as ssh-key-1, vpc-1, firewall-1 and instance-1; no id is given
// out twice. A new instance boots as GetInstance reads it: see SetBootReads.
//
// The fake is simpler than Vultr in these ways:
//   - ListInstanceVPCs lists nothing until the instance shows as active. Vultr listed the address 6–7 s after the
//     create, long before the instance was active.
//   - A deleted instance is gone from every call at once.
//   - DeleteVPC deletes a VPC as soon as no instance is attached to it. Vultr refuses for 14–20 s more.
//   - CreateVPC assigns no subnet when the request has none.
//
// Faults change the outcome of the next calls of a vultr.API method, named as in the interface, such as CreateVPC:
// see Fail, LoseResponse and Throttle. They apply in the order they were set: a call takes the first fault set for
// its method, and each fault applies to as many calls as it was set for. A Hook set with SetHook wraps every call, so
// a test can act at a chosen call, such as end the context of the run that makes it.
//
// Seeding with AddSSHKey, AddVPC, AddFirewallGroup, AddFirewallRule and AddInstance stores objects as if they had
// been created before, without a call. An empty id gets a new one, and an empty date_created the clock's time.
// Seeding checks no other field, no limit and no second copy of a rule, and returns the object as stored.
// SetInstanceTags and SetInstanceUserData change an instance without a call. SSHKeys, VPCs, FirewallGroups,
// FirewallRules, Instances, UserData, InstanceVPCs and CreateRequest read the objects back without a call: Calls does
// not log them, and no fault applies to them.
//
// The fault and seeding methods take the test's testing.TB. They fail the test on a bug of the test, such as a name
// that is not a vultr.API method or an id that is taken, at the line of the wrong call, rather than return an error
// that every test would have to check. Call them from the test's goroutine.
type Fake struct {
	now func() time.Time

	mu        sync.Mutex
	ids       map[string]bool // every id given out, so that none is given twice
	next      map[string]int  // the number in the last id given out of each kind
	sshKeys   []govultr.SSHKey
	vpcs      []govultr.VPC
	groups    []*firewallGroup
	plans     []govultr.Plan
	images    []govultr.OS
	available map[string][]string // the plans each known region can deploy now
	instances []*instance
	faults    []*fault // in the order they were set
	calls     []Call
	hook      Hook // wraps every call when set

	activeAfter, okAfter int // the boot reads of new instances
	macs                 int // how many MACs were given out
	mainIPs              int // how many main IPs were given out
}

var _ vultr.API = (*Fake)(nil)

// Option changes a default of New.
type Option func(*Fake)

// WithClock makes the fake read the time, for the date_created of new objects, from now. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(f *Fake) { f.now = now }
}

// New returns a fake without SSH keys, VPCs and firewall groups. Its catalog holds the smallest plans that Vultr
// listed on 2026-09-25, with their monthly costs; the images Ubuntu 24.04 (2284) and 26.04 (2760) and Debian 12
// (2136) and 13 (2625); and one region, ams, which can deploy every one of those plans.
func New(opts ...Option) *Fake {
	plans := defaultPlans()
	var ids []string
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	f := &Fake{
		now:         time.Now,
		ids:         map[string]bool{},
		next:        map[string]int{},
		plans:       plans,
		images:      defaultOS(),
		available:   map[string][]string{defaultRegion: ids},
		activeAfter: defaultActiveAfter,
		okAfter:     defaultOKAfter,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Call is a call that reached the fake's API.
type Call struct {
	Name string // the vultr.API method, such as CreateVPC
	// Arg is the call's main argument:
	//   - for a create, the name of the SSH key or the label of the instance, or the description of the VPC or
	//     firewall group;
	//   - for CreateFirewallRule, the group's id and the rule: ip_type, protocol, subnet/subnet_size, then the port
	//     and source=<source> when they are set, such as "firewall-1 v4 tcp 0.0.0.0/0 22";
	//   - for a delete, the id; for a firewall rule, the group's id and "/" and the rule's id, such as firewall-1/3;
	//   - the instance's id for GetInstance, HaltInstance, UpdateInstance and ListInstanceVPCs;
	//   - the group's id for ListFirewallRules, the region for AvailablePlans, the type for ListPlans, and the tag for
	//     ListInstances.
	// It is empty for the other lists.
	Arg string
}

// Calls returns the calls that reached the fake's API, in order, whatever their outcome. Calls with a bad id or an
// ended context are not among them, since the client sends no request for them.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// SSHKeys returns every SSH key, as ListSSHKeys does, without a call.
func (f *Fake) SSHKeys() []govultr.SSHKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.sshKeys)
}

// VPCs returns every VPC, as ListVPCs does, without a call.
func (f *Fake) VPCs() []govultr.VPC {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.vpcs)
}

// FirewallGroups returns every firewall group, as ListFirewallGroups does, without a call.
func (f *Fake) FirewallGroups() []govultr.FirewallGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firewallGroups()
}

// FirewallRules returns every rule of a firewall group, as ListFirewallRules does, without a call. It returns nil
// for an unknown group, as for a group without rules, so that a test can check that a group is gone; FirewallGroups
// tells the two apart.
func (f *Fake) FirewallRules(groupID string) []govultr.FirewallRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.group(groupID); g != nil {
		return clone(g.rules)
	}
	return nil
}

// Instances returns every instance, as GetInstance shows it now, in creation order, without a call and without
// counting a read.
func (f *Fake) Instances() []govultr.Instance {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []govultr.Instance
	for _, in := range f.instances {
		out = append(out, in.view())
	}
	return out
}

// UserData returns the user data of an instance, base64 as sent, without a call. It returns "" for an unknown
// instance, as for one without user data.
func (f *Fake) UserData(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instance(id); in != nil {
		return in.userData
	}
	return ""
}

// InstanceVPCs returns the VPCs an instance is attached to, with its address and MAC in each, without a call and also
// before the instance shows as active. It returns nil for an unknown instance.
func (f *Fake) InstanceVPCs(id string) []govultr.VPCInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instance(id); in != nil {
		return clone(in.vpcs)
	}
	return nil
}

// CreateRequest returns the request that created an instance, without a call. It reports false for an unknown or a
// seeded instance.
func (f *Fake) CreateRequest(id string) (govultr.InstanceCreateReq, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instance(id); in != nil && in.req != nil {
		return cloneCreateReq(*in.req), true
	}
	return govultr.InstanceCreateReq{}, false
}

// request is a call as the client would send it.
type request struct {
	name, arg    string // for Calls
	method, path string // of the HTTP request, for errors
}

// fail returns the error of an answer to r with status and message.
func (r request) fail(status int, message string) error {
	return vultr.NewAPIError(r.method, r.path, status, message, 0)
}

// Hook wraps every call to the fake's API once a test sets it with SetHook. It gets the call's context, the call as
// Calls would log it, and next, which carries the call out as the fake does without a hook: next checks the context,
// logs the call, applies a fault and returns the call's error. What the hook returns is the call's error, and a call
// with an error returns no value. So a hook can end the context before or after next, or lose the answer. Each call of
// next is a new request: after the context ended, next fails as the client does and reaches nothing. A hook may
// return nil only when a call of next returned nil. It runs on the call's goroutine, outside the fake's lock, so it
// may read the fake's objects.
type Hook func(ctx context.Context, c Call, next func(context.Context) error) error

// SetHook makes every later call go through hook once the ids that the client would put into its path pass
// vultr.CheckID; a call with a bad id fails before the hook. nil removes the hook.
func (f *Fake) SetHook(hook Hook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// run carries out r with do through the hook, when one is set, as carryOut does.
func (f *Fake) run(ctx context.Context, r request, do func() error) error {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook == nil {
		return f.carryOut(ctx, r, do)
	}
	return hook(ctx, Call{Name: r.name, Arg: r.arg}, func(ctx context.Context) error { return f.carryOut(ctx, r, do) })
}

// carryOut carries out r with do, under the lock, and returns do's error. When ctx has ended it does nothing and
// returns the client's error for that. Otherwise it logs r, and a fault for r may replace do's outcome.
func (f *Fake) carryOut(ctx context.Context, r request, do func() error) error {
	if err := ctx.Err(); err != nil {
		return vultr.NewNoAnswerError(r.method, r.path, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Name: r.name, Arg: r.arg})
	ft, ok := f.takeFault(r.name)
	if !ok {
		return do()
	}
	switch ft.kind {
	case failCall:
		return ft.err
	case throttle:
		return vultr.NewAPIError(r.method, r.path, http.StatusTooManyRequests, "Rate limit exceeded", ft.retryAfter)
	default: // loseAnswer
		_ = do() // carried out; its answer, whatever it was, is lost
		return vultr.NewNoAnswerError(r.method, r.path, errLost)
	}
}

// newID returns a new id of a kind of object, such as vpc-3. The caller holds the lock.
func (f *Fake) newID(kind string) string {
	for {
		f.next[kind]++
		id := kind + "-" + strconv.Itoa(f.next[kind])
		if !f.ids[id] {
			f.ids[id] = true
			return id
		}
	}
}

// dateLayout is how Vultr writes a date_created: RFC 3339 in UTC, with +00:00.
const dateLayout = "2006-01-02T15:04:05-07:00"

// date returns the clock's time as a date_created.
func (f *Fake) date() string {
	return f.now().UTC().Format(dateLayout)
}

// result returns v, or the zero value and err when err is not nil.
func result[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// clone returns a copy of s, or nil when s is empty, as the client returns an empty list.
func clone[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

// deref returns *p, or the zero value when p is nil.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
