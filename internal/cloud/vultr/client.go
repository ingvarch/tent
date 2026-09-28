package vultr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/vultr/govultr/v3"
)

// API is the part of the Vultr API that tent uses. Lists are whole: the client follows the cursors.
type API interface {
	// ListSSHKeys returns every SSH key of the account.
	ListSSHKeys(ctx context.Context) ([]govultr.SSHKey, error)
	// CreateSSHKey adds an SSH key to the account.
	CreateSSHKey(ctx context.Context, req *govultr.SSHKeyReq) (*govultr.SSHKey, error)
	// DeleteSSHKey removes an SSH key.
	DeleteSSHKey(ctx context.Context, id string) error
	// ListVPCs returns every VPC of the account, in all regions.
	ListVPCs(ctx context.Context) ([]govultr.VPC, error)
	// CreateVPC creates a VPC.
	CreateVPC(ctx context.Context, req *govultr.VPCReq) (*govultr.VPC, error)
	// DeleteVPC deletes a VPC.
	DeleteVPC(ctx context.Context, id string) error
	// ListFirewallGroups returns every firewall group of the account.
	ListFirewallGroups(ctx context.Context) ([]govultr.FirewallGroup, error)
	// CreateFirewallGroup creates a firewall group without rules.
	CreateFirewallGroup(ctx context.Context, req *govultr.FirewallGroupReq) (*govultr.FirewallGroup, error)
	// DeleteFirewallGroup deletes a firewall group.
	DeleteFirewallGroup(ctx context.Context, id string) error
	// ListFirewallRules returns every rule of a firewall group.
	ListFirewallRules(ctx context.Context, groupID string) ([]govultr.FirewallRule, error)
	// CreateFirewallRule adds a rule to a firewall group.
	CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (*govultr.FirewallRule, error)
	// DeleteFirewallRule removes a rule from a firewall group.
	DeleteFirewallRule(ctx context.Context, groupID string, ruleID int) error
	// AvailablePlans returns the ids of the plans of a type, such as vc2, that a region can deploy now; every type's
	// when planType is empty.
	AvailablePlans(ctx context.Context, region, planType string) ([]string, error)
	// ListPlans returns every plan of a type, such as vc2; every type's when planType is empty.
	ListPlans(ctx context.Context, planType string) ([]govultr.Plan, error)
	// ListOS returns every operating system image.
	ListOS(ctx context.Context) ([]govultr.OS, error)
	// ListInstances returns every instance with the tag, which Vultr matches exactly but in any case. The tag must not
	// be empty.
	ListInstances(ctx context.Context, tag string) ([]govultr.Instance, error)
	// GetInstance returns an instance.
	GetInstance(ctx context.Context, id string) (*govultr.Instance, error)
	// CreateInstance creates an instance. The answer, and only it, holds the instance's root password.
	CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error)
	// DeleteInstance destroys an instance at once, even a running one.
	DeleteInstance(ctx context.Context, id string) error
	// HaltInstance powers an instance off hard, without a shutdown. Halting an instance twice is harmless.
	HaltInstance(ctx context.Context, id string) error
	// UpdateInstance changes an instance. Vultr keeps its tags when req.Tags is nil.
	UpdateInstance(ctx context.Context, id string, req *govultr.InstanceUpdateReq) error
	// ListInstanceVPCs returns the VPCs an instance is attached to, each with the instance's address and MAC in it.
	ListInstanceVPCs(ctx context.Context, id string) ([]govultr.VPCInfo, error)
}

// Client is the API over govultr. It sends its requests through the transport, which spaces them, gives each attempt
// at most 2 minutes and retries them.
//
// A call whose answer is not a success, or that got no answer, fails with an *APIError; when the context ended, the
// error matches the context's error too. A success that the client cannot read fails with an error that names the
// request; for a POST it is an *APIError that matches ErrUnavailable, since the object may exist. A list answer
// without Vultr's meta field is such a success. A call with an id in its path that is empty or holds anything but
// ASCII letters, digits and "-" fails without sending a request, and so does ListInstances with an empty tag.
type Client struct {
	gv *govultr.Client
}

var _ API = (*Client)(nil)

// Defaults of NewClient.
const (
	defaultBaseURL = "https://api.vultr.com"
	perPage        = 500  // the most items a page of a list may hold
	maxPages       = 1000 // the most pages the client gets of one list: 500,000 items

	// Bounds of each attempt of the default round tripper.
	dialTimeout   = 30 * time.Second
	tlsTimeout    = 30 * time.Second
	headerTimeout = time.Minute // the wait for the answer's header once the request is sent
)

// options are the settings of NewClient.
type options struct {
	baseURL   string
	base      http.RoundTripper
	transport []transportOption
}

// Option changes a default of NewClient.
type Option func(*options)

// WithBaseURL sends the requests to the scheme and host of u, such as a proxy or a test server, instead of
// https://api.vultr.com. The paths of the requests start with /v2 whatever the path of u.
func WithBaseURL(u string) Option {
	return func(o *options) { o.baseURL = u }
}

// WithInterval sets the least time between the starts of two requests. The default, 100 ms, keeps to 10 requests per
// second.
func WithInterval(d time.Duration) Option {
	return withTransportOptions(withInterval(d))
}

// WithRoundTripper sends the requests through rt. The default is a copy of http.DefaultTransport that bounds the
// waits of each attempt: 30 s to connect, 30 s for the TLS handshake and a minute for the answer's header.
func WithRoundTripper(rt http.RoundTripper) Option {
	return func(o *options) { o.base = rt }
}

// withTransportOptions passes options to the transport.
func withTransportOptions(opts ...transportOption) Option {
	return func(o *options) { o.transport = append(o.transport, opts...) }
}

// NewClient returns a client that calls the Vultr API with the API key. It fails when the key is empty or has
// whitespace or control characters, and when the base URL is not an http or https URL with a host. An http URL must
// name a loopback host, since the key would travel in clear text.
func NewClient(key string, opts ...Option) (*Client, error) {
	switch {
	case key == "":
		return nil, errors.New("vultr: no API key")
	case strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return nil, errors.New("vultr: the API key has whitespace or control characters")
	}
	o := options{baseURL: defaultBaseURL}
	for _, opt := range opts {
		opt(&o)
	}
	if err := checkBaseURL(o.baseURL); err != nil {
		return nil, err
	}
	if o.base == nil {
		o.base = newHTTPTransport()
	}
	gv := govultr.NewClient(&http.Client{
		// No Timeout: it would cover the retries and pauses of a call too. The context bounds a call, and the
		// transport each attempt.
		Transport: newTransport(key, o.base, o.transport...),
		// Following a redirect would send the key to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	gv.SetRetryLimit(0) // govultr would send a POST again; the transport retries instead
	if err := gv.SetBaseURL(o.baseURL); err != nil {
		return nil, fmt.Errorf("vultr: set the base URL: %w", err)
	}
	return &Client{gv: gv}, nil
}

// checkBaseURL fails when u is not an https URL with a host, or an http URL with a loopback host: localhost,
// 127.0.0.0/8 or ::1.
func checkBaseURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return fmt.Errorf("vultr: the base URL %q is not an http or https URL with a host", u)
	}
	if p.Scheme == "http" && !isLoopback(p.Hostname()) {
		return fmt.Errorf("vultr: the base URL %q uses http, which only localhost, 127.0.0.0/8 and ::1 may use", u)
	}
	return nil
}

// isLoopback reports whether host is localhost or a loopback address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// newHTTPTransport returns a copy of http.DefaultTransport that bounds the waits for a connection, a TLS handshake
// and an answer's header, so that a hung attempt fails and the transport may send it again.
func newHTTPTransport() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok { // a program replaced it
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}
	t = t.Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = tlsTimeout
	t.ResponseHeaderTimeout = headerTimeout
	return t
}

// ListSSHKeys gets GET /v2/ssh-keys, every page.
func (c *Client) ListSSHKeys(ctx context.Context) ([]govultr.SSHKey, error) {
	return list(ctx, c.gv.SSHKey.List)
}

// CreateSSHKey sends POST /v2/ssh-keys.
func (c *Client) CreateSSHKey(ctx context.Context, req *govultr.SSHKeyReq) (*govultr.SSHKey, error) {
	return object(ctx, req, c.gv.SSHKey.Create)
}

// DeleteSSHKey sends DELETE /v2/ssh-keys/{id}.
func (c *Client) DeleteSSHKey(ctx context.Context, id string) error {
	if err := CheckID("DELETE /v2/ssh-keys/{id}", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.SSHKey.Delete(ctx, id) })
	return err
}

// ListVPCs gets GET /v2/vpcs, every page.
func (c *Client) ListVPCs(ctx context.Context) ([]govultr.VPC, error) {
	return list(ctx, c.gv.VPC.List)
}

// CreateVPC sends POST /v2/vpcs.
func (c *Client) CreateVPC(ctx context.Context, req *govultr.VPCReq) (*govultr.VPC, error) {
	return object(ctx, req, c.gv.VPC.Create)
}

// DeleteVPC sends DELETE /v2/vpcs/{id}.
func (c *Client) DeleteVPC(ctx context.Context, id string) error {
	if err := CheckID("DELETE /v2/vpcs/{id}", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.VPC.Delete(ctx, id) })
	return err
}

// ListFirewallGroups gets GET /v2/firewalls, every page.
func (c *Client) ListFirewallGroups(ctx context.Context) ([]govultr.FirewallGroup, error) {
	return list(ctx, c.gv.FirewallGroup.List)
}

// CreateFirewallGroup sends POST /v2/firewalls.
func (c *Client) CreateFirewallGroup(ctx context.Context, req *govultr.FirewallGroupReq) (*govultr.FirewallGroup,
	error) {
	return object(ctx, req, c.gv.FirewallGroup.Create)
}

// DeleteFirewallGroup sends DELETE /v2/firewalls/{id}.
func (c *Client) DeleteFirewallGroup(ctx context.Context, id string) error {
	if err := CheckID("DELETE /v2/firewalls/{id}", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.FirewallGroup.Delete(ctx, id) })
	return err
}

// ListFirewallRules gets GET /v2/firewalls/{groupID}/rules, every page.
func (c *Client) ListFirewallRules(ctx context.Context, groupID string) ([]govultr.FirewallRule, error) {
	if err := CheckID("GET /v2/firewalls/{groupID}/rules", "groupID", groupID); err != nil {
		return nil, err
	}
	return list(ctx, func(ctx context.Context, o *govultr.ListOptions) ([]govultr.FirewallRule, *govultr.Meta,
		*http.Response, error) {
		return c.gv.FirewallRule.List(ctx, groupID, o)
	})
}

// CreateFirewallRule sends POST /v2/firewalls/{groupID}/rules.
func (c *Client) CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (
	*govultr.FirewallRule, error) {
	if err := CheckID("POST /v2/firewalls/{groupID}/rules", "groupID", groupID); err != nil {
		return nil, err
	}
	return object(ctx, req, func(ctx context.Context, req *govultr.FirewallRuleReq) (*govultr.FirewallRule,
		*http.Response, error) {
		return c.gv.FirewallRule.Create(ctx, groupID, req)
	})
}

// DeleteFirewallRule sends DELETE /v2/firewalls/{groupID}/rules/{ruleID}.
func (c *Client) DeleteFirewallRule(ctx context.Context, groupID string, ruleID int) error {
	const route = "DELETE /v2/firewalls/{groupID}/rules/{ruleID}"
	if err := CheckID(route, "groupID", groupID); err != nil {
		return err
	}
	if err := CheckRuleID(route, ruleID); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.FirewallRule.Delete(ctx, groupID, ruleID) })
	return err
}

// AvailablePlans gets GET /v2/regions/{region}/availability?type={planType}.
func (c *Client) AvailablePlans(ctx context.Context, region, planType string) ([]string, error) {
	if err := CheckID("GET /v2/regions/{region}/availability", "region", region); err != nil {
		return nil, err
	}
	var a *govultr.PlanAvailability
	_, err := call(ctx, func(ctx context.Context) (err error) {
		a, _, err = c.gv.Region.Availability(ctx, region, planType)
		return err
	})
	if err != nil {
		return nil, err
	}
	return a.AvailablePlans, nil
}

// ListPlans gets GET /v2/plans?type={planType}, every page.
func (c *Client) ListPlans(ctx context.Context, planType string) ([]govultr.Plan, error) {
	return list(ctx, func(ctx context.Context, o *govultr.ListOptions) ([]govultr.Plan, *govultr.Meta,
		*http.Response, error) {
		return c.gv.Plan.List(ctx, planType, o)
	})
}

// ListOS gets GET /v2/os, every page.
func (c *Client) ListOS(ctx context.Context) ([]govultr.OS, error) {
	return list(ctx, c.gv.OS.List)
}

// ListInstances gets GET /v2/instances?tag={tag}, every page.
func (c *Client) ListInstances(ctx context.Context, tag string) ([]govultr.Instance, error) {
	if err := CheckTag(tag); err != nil {
		return nil, err
	}
	return list(ctx, func(ctx context.Context, o *govultr.ListOptions) ([]govultr.Instance, *govultr.Meta,
		*http.Response, error) {
		opts := *o
		opts.Tag = tag
		return c.gv.Instance.List(ctx, &opts)
	})
}

// GetInstance gets GET /v2/instances/{id}.
func (c *Client) GetInstance(ctx context.Context, id string) (*govultr.Instance, error) {
	if err := CheckID("GET /v2/instances/{id}", "id", id); err != nil {
		return nil, err
	}
	return object(ctx, id, c.gv.Instance.Get)
}

// CreateInstance sends POST /v2/instances.
func (c *Client) CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	return object(ctx, req, c.gv.Instance.Create)
}

// DeleteInstance sends DELETE /v2/instances/{id}.
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	if err := CheckID("DELETE /v2/instances/{id}", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.Instance.Delete(ctx, id) })
	return err
}

// HaltInstance sends POST /v2/instances/{id}/halt. The POST has no body, so Go 1.26's HTTP/2 client may send it
// again after the server resets the stream; a second halt of a stopped instance answers 204, so that is harmless.
func (c *Client) HaltInstance(ctx context.Context, id string) error {
	if err := CheckID("POST /v2/instances/{id}/halt", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error { return c.gv.Instance.Halt(ctx, id) })
	return err
}

// UpdateInstance sends PATCH /v2/instances/{id}. govultr sends "tags" and "ddos_protection" as null when they are
// unset, and omits every other empty field, so an empty firewall group id, label or user data changes nothing.
func (c *Client) UpdateInstance(ctx context.Context, id string, req *govultr.InstanceUpdateReq) error {
	if err := CheckID("PATCH /v2/instances/{id}", "id", id); err != nil {
		return err
	}
	_, err := call(ctx, func(ctx context.Context) error {
		_, _, err := c.gv.Instance.Update(ctx, id, req)
		return err
	})
	return err
}

// ListInstanceVPCs gets GET /v2/instances/{id}/vpcs, every page.
func (c *Client) ListInstanceVPCs(ctx context.Context, id string) ([]govultr.VPCInfo, error) {
	if err := CheckID("GET /v2/instances/{id}/vpcs", "id", id); err != nil {
		return nil, err
	}
	return list(ctx, func(ctx context.Context, o *govultr.ListOptions) ([]govultr.VPCInfo, *govultr.Meta,
		*http.Response, error) {
		return c.gv.Instance.ListVPCInfo(ctx, id, o)
	})
}

// idPattern matches an id that goes into a path as it is: Vultr's ids are UUIDs, and its regions are like ams.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// CheckID fails when id is empty or holds anything but ASCII letters, digits and "-", with the error the client
// gives for such an id. Such an id could change the path of the call, and a delete sent to another path may answer
// 404, which a caller takes for success. route and name say which call and which id, for the error, such as
// "DELETE /v2/vpcs/{id}" and "id".
func CheckID(route, name, id string) error {
	if idPattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf("vultr: %s: invalid %s %q", route, name, id)
}

// CheckTag fails when a tag to list instances by is empty, with the error the client gives for it: such a list would
// hold every instance of the account.
func CheckTag(tag string) error {
	if tag != "" {
		return nil
	}
	return errors.New(`vultr: GET /v2/instances: invalid tag ""`)
}

// CheckRuleID fails when a firewall rule id is not positive, with the error the client gives for such an id. route
// says which call, for the error.
func CheckRuleID(route string, id int) error {
	if id > 0 {
		return nil
	}
	return fmt.Errorf("vultr: %s: invalid ruleID %d", route, id)
}

// list returns every item of a list. It gets the pages with page, following the cursors. It fails when an answer
// holds no list, which Vultr's meta field marks, when a cursor repeats, and after maxPages pages.
func list[T any](ctx context.Context,
	page func(context.Context, *govultr.ListOptions) ([]T, *govultr.Meta, *http.Response, error)) ([]T, error) {
	var all []T
	seen := map[string]bool{}
	opts := &govultr.ListOptions{PerPage: perPage}
	for n := 1; ; n++ {
		var (
			items []T
			meta  *govultr.Meta
		)
		rec, err := call(ctx, func(ctx context.Context) (err error) {
			items, meta, _, err = page(ctx, opts)
			return err
		})
		switch {
		case err != nil:
			return nil, err
		case meta == nil: // an answer without a list would read as an empty one
			return nil, fmt.Errorf("vultr: %s %s: the answer holds no list", rec.method, rec.path)
		}
		all = append(all, items...)
		if meta.Links == nil || meta.Links.Next == "" {
			return all, nil
		}
		next := meta.Links.Next
		switch {
		case seen[next]:
			return nil, fmt.Errorf("vultr: %s %s: the cursor %q repeats", rec.method, rec.path, next)
		case n == maxPages:
			return nil, fmt.Errorf("vultr: %s %s: more than %d pages", rec.method, rec.path, maxPages)
		}
		seen[next] = true
		opts = &govultr.ListOptions{PerPage: perPage, Cursor: next}
	}
}

// object sends a request with f and returns the object that the answer holds. It fails when the answer holds none.
func object[R, T any](ctx context.Context, req R, f func(context.Context, R) (*T, *http.Response, error)) (*T, error) {
	var out *T
	rec, err := call(ctx, func(ctx context.Context) (err error) {
		out, _, err = f(ctx, req)
		return err
	})
	switch {
	case err != nil:
		return nil, err
	case out == nil:
		return nil, unreadable(rec, errors.New("the answer holds no object"))
	}
	return out, nil
}

// call runs f, which sends one request through govultr, with a fresh call record. It returns the record, and f's
// error turned into the client's by callError. A success with a body whose type is not application/json is an error
// too, except for a DELETE, which reads nothing from its answer: govultr reads no other type, and would leave the
// result empty.
func call(ctx context.Context, f func(context.Context) error) (*callRecord, error) {
	ctx, rec := withCallRecord(ctx)
	if err := f(ctx); err != nil {
		return rec, callError(rec, err)
	}
	if ct := rec.header.Get("Content-Type"); rec.method != http.MethodDelete && len(rec.body) > 0 &&
		ct != "application/json" {
		return rec, unreadable(rec, fmt.Errorf("an answer of type %q, not application/json", ct))
	}
	return rec, nil
}

// callError turns the error of a govultr call into the client's:
//   - the *APIError of the record when the call got an answer with an error status, or none;
//   - an *APIError without a class for an answer that is neither a success nor an error, such as a redirect;
//   - the error of unreadable for a success that govultr could not read. For a success status that govultr does not
//     read at all, such as 207, it names the status: govultr's error is then the body, which may hold a secret, such
//     as the root password in the answer to an instance create;
//   - err with "no request sent" when govultr failed before sending one.
func callError(rec *callRecord, err error) error {
	if rec.method == "" {
		return fmt.Errorf("vultr: no request sent: %w", err)
	}
	if e := newAPIError(rec); e != nil {
		return e
	}
	if rec.status < http.StatusOK || rec.status >= http.StatusMultipleChoices {
		return NewAPIError(rec.method, rec.path, rec.status, message(rec.body), 0)
	}
	if !govultrReads(rec.status) {
		return unreadable(rec, fmt.Errorf("a success with the status %d, which the client does not read", rec.status))
	}
	return unreadable(rec, err)
}

// govultrReads reports whether govultr reads a success with status. To any other success it gives the body as its
// error.
func govultrReads(status int) bool {
	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNonAuthoritativeInfo,
		http.StatusNoContent, http.StatusResetContent, http.StatusPartialContent:
		return true
	}
	return false
}

// unreadable returns the error of a success that the client cannot read because of cause. For a GET or DELETE it is
// cause with the request's method and path. For a POST it is an *APIError that matches ErrUnavailable and unwraps to
// cause: Vultr carried the call out, so the object may exist, but the answer does not say which.
func unreadable(rec *callRecord, cause error) error {
	if rec.method != http.MethodPost {
		return fmt.Errorf("vultr: %s %s: %w", rec.method, rec.path, cause)
	}
	e := NewNoAnswerError(rec.method, rec.path, cause)
	e.Status = rec.status
	return e
}
