package vultr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"
)

// gotRequest is a request that an apiServer got.
type gotRequest struct {
	Method, Path, Query, Auth string
	Body                      string // the JSON body in canonical form; empty when there is none
}

// apiServer is a test server in the place of the Vultr API. It logs the requests it gets.
type apiServer struct {
	*httptest.Server

	mu   sync.Mutex
	reqs []gotRequest
}

// newAPIServer starts an apiServer that answers with h.
func newAPIServer(t *testing.T, h http.HandlerFunc) *apiServer {
	t.Helper()
	s := &apiServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, gotRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			Body: canonJSON(t, string(body)),
		})
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// requests returns the requests the server got so far.
func (s *apiServer) requests() []gotRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// canonJSON returns a JSON text in one form, so that two texts of the same value compare equal. It returns "" for
// "".
func canonJSON(t *testing.T, s string) string {
	t.Helper()
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Errorf("not JSON: %q: %v", s, err)
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshal %v: %v", v, err)
	}
	return string(out)
}

// writeJSON answers with status and a JSON body, with the Content-Type that the Vultr API sends.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// answer returns a handler that always answers with status and a JSON body, or with no body when body is "".
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if body == "" {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, body)
	}
}

// newTestClient returns a client of srv that waits 1ms between requests and before retries.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(testKey, WithBaseURL(srv.URL), WithRoundTripper(srv.Client().Transport),
		WithInterval(time.Millisecond), withTransportOptions(withBackoff(time.Millisecond)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// auth is the Authorization header of every request.
const auth = "Bearer " + testKey

// An instance as Vultr lists it once it is ready, and the create request of a node.
const (
	instanceJSON = `{"id":"i1","region":"ams","plan":"vc2-1c-1gb","label":"prod-servers-0",` +
		`"hostname":"prod-servers-0","tags":["tent/cluster=prod"],"os_id":2284,"firewall_group_id":"g1",` +
		`"status":"active","power_status":"running","server_status":"ok","main_ip":"198.51.100.7",` +
		`"internal_ip":"","date_created":"2026-09-27T10:00:00+00:00"}`
	password = "x7#Qv9pL2m" // the root password that only the create answer holds
)

var (
	readyInstance = govultr.Instance{
		ID: "i1", Region: "ams", Plan: "vc2-1c-1gb", Label: "prod-servers-0", Hostname: "prod-servers-0",
		Tags: []string{"tent/cluster=prod"}, OsID: 2284, FirewallGroupID: "g1", Status: "active",
		PowerStatus: "running", ServerStatus: "ok", MainIP: "198.51.100.7", DateCreated: "2026-09-27T10:00:00+00:00",
	}
	createInstanceReq = govultr.InstanceCreateReq{
		Region: "ams", Plan: "vc2-1c-1gb", Label: "prod-servers-0", Hostname: "prod-servers-0",
		Tags: []string{"tent/cluster=prod", "tent/op=op-1"}, OsID: 2284, FirewallGroupID: "g1",
		AttachVPC: []string{"v1"}, SSHKeys: []string{"k1"}, Backups: "disabled", UserData: "I2Nsb3VkLWNvbmZpZwo=",
	}
)

func TestClientCalls(t *testing.T) {
	const (
		created = "2026-09-27T10:00:00+00:00"
		marker  = "tent:cluster=prod;kind=vpc"
		noNext  = `"meta":{"total":1,"links":{"next":"","prev":""}}`
	)
	for _, tc := range []struct {
		name   string
		call   func(context.Context, API) (any, error)
		want   gotRequest // Auth is always the key
		status int
		answer string // a JSON body, or "" for none
		result any
	}{
		{
			name:   "ListSSHKeys",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListSSHKeys(ctx) },
			want:   gotRequest{Method: "GET", Path: "/v2/ssh-keys", Query: "per_page=500"},
			status: 200,
			answer: `{"ssh_keys":[{"id":"k1","name":"tent:cluster=prod;kind=ssh-key","ssh_key":"ssh-ed25519 AAAA",` +
				`"date_created":"` + created + `"}],` + noNext + `}`,
			result: []govultr.SSHKey{
				{ID: "k1", Name: "tent:cluster=prod;kind=ssh-key", SSHKey: "ssh-ed25519 AAAA", DateCreated: created},
			},
		},
		{
			name: "CreateSSHKey",
			call: func(ctx context.Context, c API) (any, error) {
				return c.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: "tent:cluster=prod;kind=ssh-key", SSHKey: "ssh-ed25519 AAAA"})
			},
			want: gotRequest{
				Method: "POST", Path: "/v2/ssh-keys",
				Body: `{"name":"tent:cluster=prod;kind=ssh-key","ssh_key":"ssh-ed25519 AAAA"}`,
			},
			status: 201,
			answer: `{"ssh_key":{"id":"k1","name":"tent:cluster=prod;kind=ssh-key","ssh_key":"ssh-ed25519 AAAA",` +
				`"date_created":"` + created + `"}}`,
			result: &govultr.SSHKey{
				ID: "k1", Name: "tent:cluster=prod;kind=ssh-key", SSHKey: "ssh-ed25519 AAAA", DateCreated: created,
			},
		},
		{
			name:   "DeleteSSHKey",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.DeleteSSHKey(ctx, "k1") },
			want:   gotRequest{Method: "DELETE", Path: "/v2/ssh-keys/k1"},
			status: 204,
		},
		{
			name:   "ListVPCs",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListVPCs(ctx) },
			want:   gotRequest{Method: "GET", Path: "/v2/vpcs", Query: "per_page=500"},
			status: 200,
			answer: `{"vpcs":[{"id":"v1","region":"ams","description":"` + marker + `","v4_subnet":"10.64.0.0",` +
				`"v4_subnet_mask":16,"date_created":"` + created + `"}],` + noNext + `}`,
			result: []govultr.VPC{{
				ID: "v1", Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16,
				DateCreated: created,
			}},
		},
		{
			name: "CreateVPC",
			call: func(ctx context.Context, c API) (any, error) {
				return c.CreateVPC(ctx, &govultr.VPCReq{
					Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16,
				})
			},
			want: gotRequest{
				Method: "POST", Path: "/v2/vpcs",
				Body: `{"region":"ams","description":"` + marker + `","v4_subnet":"10.64.0.0","v4_subnet_mask":16}`,
			},
			status: 201,
			answer: `{"vpc":{"id":"v1","region":"ams","description":"` + marker + `","v4_subnet":"10.64.0.0",` +
				`"v4_subnet_mask":16,"date_created":"` + created + `"}}`,
			result: &govultr.VPC{
				ID: "v1", Region: "ams", Description: marker, V4Subnet: "10.64.0.0", V4SubnetMask: 16,
				DateCreated: created,
			},
		},
		{
			name:   "DeleteVPC",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.DeleteVPC(ctx, "v1") },
			want:   gotRequest{Method: "DELETE", Path: "/v2/vpcs/v1"},
			status: 204,
		},
		{
			name:   "ListFirewallGroups",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListFirewallGroups(ctx) },
			want:   gotRequest{Method: "GET", Path: "/v2/firewalls", Query: "per_page=500"},
			status: 200,
			answer: `{"firewall_groups":[{"id":"g1","description":"tent:cluster=prod;kind=firewall;role=server",` +
				`"date_created":"` + created + `","date_modified":"` + created + `","instance_count":3,` +
				`"rule_count":2,"max_rule_count":50}],` + noNext + `}`,
			result: []govultr.FirewallGroup{{
				ID: "g1", Description: "tent:cluster=prod;kind=firewall;role=server", DateCreated: created,
				DateModified: created, InstanceCount: 3, RuleCount: 2, MaxRuleCount: 50,
			}},
		},
		{
			name: "CreateFirewallGroup",
			call: func(ctx context.Context, c API) (any, error) {
				return c.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "tent:cluster=prod;kind=firewall"})
			},
			want: gotRequest{
				Method: "POST", Path: "/v2/firewalls", Body: `{"description":"tent:cluster=prod;kind=firewall"}`,
			},
			status: 201,
			answer: `{"firewall_group":{"id":"g1","description":"tent:cluster=prod;kind=firewall","max_rule_count":50}}`,
			result: &govultr.FirewallGroup{ID: "g1", Description: "tent:cluster=prod;kind=firewall", MaxRuleCount: 50},
		},
		{
			name:   "DeleteFirewallGroup",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.DeleteFirewallGroup(ctx, "g1") },
			want:   gotRequest{Method: "DELETE", Path: "/v2/firewalls/g1"},
			status: 204,
		},
		{
			name:   "ListFirewallRules",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListFirewallRules(ctx, "g1") },
			want:   gotRequest{Method: "GET", Path: "/v2/firewalls/g1/rules", Query: "per_page=500"},
			status: 200,
			answer: `{"firewall_rules":[{"id":1,"action":"accept","ip_type":"v4","protocol":"tcp","port":"22",` +
				`"subnet":"0.0.0.0","subnet_size":0,"source":"","notes":"ssh"}],` + noNext + `}`,
			result: []govultr.FirewallRule{{
				ID: 1, Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "0.0.0.0", Notes: "ssh",
			}},
		},
		{
			name: "CreateFirewallRule",
			call: func(ctx context.Context, c API) (any, error) {
				return c.CreateFirewallRule(ctx, "g1", &govultr.FirewallRuleReq{
					IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", SubnetSize: 0, Port: "22", Notes: "ssh",
				})
			},
			want: gotRequest{
				Method: "POST", Path: "/v2/firewalls/g1/rules",
				Body: `{"ip_type":"v4","protocol":"tcp","subnet":"0.0.0.0","subnet_size":0,"port":"22","notes":"ssh"}`,
			},
			status: 201,
			answer: `{"firewall_rule":{"id":1,"action":"accept","ip_type":"v4","protocol":"tcp","port":"22",` +
				`"subnet":"0.0.0.0","subnet_size":0,"source":"","notes":"ssh"}}`,
			result: &govultr.FirewallRule{
				ID: 1, Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "0.0.0.0", Notes: "ssh",
			},
		},
		{
			name:   "DeleteFirewallRule",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.DeleteFirewallRule(ctx, "g1", 3) },
			want:   gotRequest{Method: "DELETE", Path: "/v2/firewalls/g1/rules/3"},
			status: 204,
		},
		{
			name:   "AvailablePlans",
			call:   func(ctx context.Context, c API) (any, error) { return c.AvailablePlans(ctx, "ams", "vc2") },
			want:   gotRequest{Method: "GET", Path: "/v2/regions/ams/availability", Query: "type=vc2"},
			status: 200,
			answer: `{"available_plans":["vc2-1c-1gb","vc2-2c-4gb"],"available_vpc_only_plans":[]}`,
			result: []string{"vc2-1c-1gb", "vc2-2c-4gb"},
		},
		{
			name:   "ListPlans",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListPlans(ctx, "vc2") },
			want:   gotRequest{Method: "GET", Path: "/v2/plans", Query: "per_page=500&type=vc2"},
			status: 200,
			answer: `{"plans":[{"id":"vc2-1c-1gb","vcpu_count":1,"ram":1024,"disk":25,"disk_count":1,` +
				`"bandwidth":1024,"monthly_cost":5,"type":"vc2","locations":["ams","fra"]}],` + noNext + `}`,
			result: []govultr.Plan{{
				ID: "vc2-1c-1gb", VCPUCount: 1, RAM: 1024, Disk: 25, DiskCount: 1, Bandwidth: 1024, MonthlyCost: 5,
				Type: "vc2", Locations: []string{"ams", "fra"},
			}},
		},
		{
			name:   "ListOS",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListOS(ctx) },
			want:   gotRequest{Method: "GET", Path: "/v2/os", Query: "per_page=500"},
			status: 200,
			answer: `{"os":[{"id":2284,"name":"Ubuntu 24.04 LTS x64","arch":"x64","family":"ubuntu"}],` + noNext + `}`,
			result: []govultr.OS{{ID: 2284, Name: "Ubuntu 24.04 LTS x64", Arch: "x64", Family: "ubuntu"}},
		},
		{
			name:   "ListInstances",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListInstances(ctx, "tent/cluster=prod") },
			want:   gotRequest{Method: "GET", Path: "/v2/instances", Query: "per_page=500&tag=tent%2Fcluster%3Dprod"},
			status: 200,
			answer: `{"instances":[` + instanceJSON + `],` + noNext + `}`,
			result: []govultr.Instance{readyInstance},
		},
		{
			name:   "GetInstance",
			call:   func(ctx context.Context, c API) (any, error) { return c.GetInstance(ctx, "i1") },
			want:   gotRequest{Method: "GET", Path: "/v2/instances/i1"},
			status: 200,
			answer: `{"instance":` + instanceJSON + `}`,
			result: &readyInstance,
		},
		{
			name: "CreateInstance",
			call: func(ctx context.Context, c API) (any, error) { return c.CreateInstance(ctx, &createInstanceReq) },
			want: gotRequest{
				Method: "POST", Path: "/v2/instances",
				Body: `{"region":"ams","plan":"vc2-1c-1gb","label":"prod-servers-0","hostname":"prod-servers-0",` +
					`"tags":["tent/cluster=prod","tent/op=op-1"],"os_id":2284,"firewall_group_id":"g1",` +
					`"attach_vpc":["v1"],"sshkey_id":["k1"],"backups":"disabled","user_data":"I2Nsb3VkLWNvbmZpZwo=",` +
					`"block_devices":null}`,
			},
			status: 202,
			answer: `{"instance":{"id":"i1","region":"ams","plan":"vc2-1c-1gb","label":"prod-servers-0",` +
				`"hostname":"prod-servers-0","tags":["tent/cluster=prod","tent/op=op-1"],"os_id":2284,` +
				`"firewall_group_id":"g1","status":"pending","power_status":"stopped","server_status":"none",` +
				`"main_ip":"0.0.0.0","internal_ip":"","date_created":"` + created + `","default_password":"` + password +
				`"}}`,
			result: &govultr.Instance{
				ID: "i1", Region: "ams", Plan: "vc2-1c-1gb", Label: "prod-servers-0", Hostname: "prod-servers-0",
				Tags: []string{"tent/cluster=prod", "tent/op=op-1"}, OsID: 2284, FirewallGroupID: "g1", Status: "pending",
				PowerStatus: "stopped", ServerStatus: "none", MainIP: "0.0.0.0", DateCreated: created,
				DefaultPassword: password,
			},
		},
		{
			name:   "DeleteInstance",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.DeleteInstance(ctx, "i1") },
			want:   gotRequest{Method: "DELETE", Path: "/v2/instances/i1"},
			status: 204,
		},
		{
			name:   "HaltInstance",
			call:   func(ctx context.Context, c API) (any, error) { return nil, c.HaltInstance(ctx, "i1") },
			want:   gotRequest{Method: "POST", Path: "/v2/instances/i1/halt"},
			status: 204,
		},
		{
			name: "UpdateInstance",
			call: func(ctx context.Context, c API) (any, error) {
				return nil, c.UpdateInstance(ctx, "i1", &govultr.InstanceUpdateReq{UserData: "c3R1Ygo="})
			},
			// govultr sends tags and ddos_protection even when they are unset; Vultr then keeps them.
			want: gotRequest{
				Method: "PATCH", Path: "/v2/instances/i1",
				Body: `{"tags":null,"ddos_protection":null,"user_data":"c3R1Ygo="}`,
			},
			status: 202,
			answer: `{"job_ids":["j1"]}`,
		},
		{
			name:   "ListInstanceVPCs",
			call:   func(ctx context.Context, c API) (any, error) { return c.ListInstanceVPCs(ctx, "i1") },
			want:   gotRequest{Method: "GET", Path: "/v2/instances/i1/vpcs", Query: "per_page=500"},
			status: 200,
			answer: `{"vpcs":[{"id":"v1","mac_address":"5a:00:04:aa:bb:cc","ip_address":"10.64.0.3"}],` + noNext + `}`,
			result: []govultr.VPCInfo{{ID: "v1", MacAddress: "5a:00:04:aa:bb:cc", IPAddress: "10.64.0.3"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, answer(tc.status, tc.answer))
			got, err := tc.call(t.Context(), newTestClient(t, srv.Server))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if diff := cmp.Diff(tc.result, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
			want := tc.want
			want.Auth, want.Body = auth, canonJSON(t, want.Body)
			if diff := cmp.Diff([]gotRequest{want}, srv.requests()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

// pagedList answers a list in three pages, one item on each: the cursors are "", "c1" and "c2".
func pagedList(t *testing.T, path, field string, items [3]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			writeJSON(w, http.StatusNotFound, `{"error":"no such path","status":404}`)
			return
		}
		pages := map[string]struct{ item, next string }{"": {items[0], "c1"}, "c1": {items[1], "c2"}, "c2": {items[2], ""}}
		cursor := r.URL.Query().Get("cursor")
		p, ok := pages[cursor]
		if !ok {
			t.Errorf("unknown cursor %q", cursor)
			writeJSON(w, http.StatusBadRequest, `{"error":"Invalid cursor.","status":400}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"`+field+`":[`+p.item+`],"meta":{"total":3,"links":{"next":"`+p.next+
			`","prev":""}}}`)
	}
}

func TestClientListsFollowCursors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		list  func(context.Context, API) (any, error)
		path  string
		query url.Values // besides per_page and cursor
		field string     // the list's field in the answer
		items [3]string  // the JSON of each page's item
		want  any
	}{
		{
			"ListSSHKeys", func(ctx context.Context, c API) (any, error) { return c.ListSSHKeys(ctx) },
			"/v2/ssh-keys", nil, "ssh_keys", [3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.SSHKey{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
		{
			"ListVPCs", func(ctx context.Context, c API) (any, error) { return c.ListVPCs(ctx) },
			"/v2/vpcs", nil, "vpcs", [3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.VPC{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
		{
			"ListFirewallGroups", func(ctx context.Context, c API) (any, error) { return c.ListFirewallGroups(ctx) },
			"/v2/firewalls", nil, "firewall_groups", [3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.FirewallGroup{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
		{
			"ListFirewallRules", func(ctx context.Context, c API) (any, error) { return c.ListFirewallRules(ctx, "g1") },
			"/v2/firewalls/g1/rules", nil, "firewall_rules", [3]string{`{"id":1}`, `{"id":2}`, `{"id":3}`},
			[]govultr.FirewallRule{{ID: 1}, {ID: 2}, {ID: 3}},
		},
		{
			"ListPlans", func(ctx context.Context, c API) (any, error) { return c.ListPlans(ctx, "vc2") },
			"/v2/plans", url.Values{"type": {"vc2"}}, "plans", [3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.Plan{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
		{
			"ListOS", func(ctx context.Context, c API) (any, error) { return c.ListOS(ctx) },
			"/v2/os", nil, "os", [3]string{`{"id":1}`, `{"id":2}`, `{"id":3}`},
			[]govultr.OS{{ID: 1}, {ID: 2}, {ID: 3}},
		},
		{
			"ListInstances", func(ctx context.Context, c API) (any, error) { return c.ListInstances(ctx, "tent/op=op-1") },
			"/v2/instances", url.Values{"tag": {"tent/op=op-1"}}, "instances",
			[3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.Instance{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
		{
			"ListInstanceVPCs", func(ctx context.Context, c API) (any, error) { return c.ListInstanceVPCs(ctx, "i1") },
			"/v2/instances/i1/vpcs", nil, "vpcs", [3]string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`},
			[]govultr.VPCInfo{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, pagedList(t, tc.path, tc.field, tc.items))
			got, err := tc.list(t.Context(), newTestClient(t, srv.Server))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
			var want []gotRequest
			for _, cursor := range []string{"", "c1", "c2"} {
				q := url.Values{"per_page": {"500"}}
				for k, v := range tc.query {
					q[k] = v
				}
				if cursor != "" {
					q.Set("cursor", cursor)
				}
				want = append(want, gotRequest{Method: "GET", Path: tc.path, Query: q.Encode(), Auth: auth})
			}
			if diff := cmp.Diff(want, srv.requests()); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClientListRepeatedCursor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		next  map[string]string // the next cursor after each cursor
		calls int
	}{
		{"the same cursor again", map[string]string{"": "c1", "c1": "c1"}, 2},
		{"a cycle", map[string]string{"": "c1", "c1": "c2", "c2": "c1"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				next := tc.next[r.URL.Query().Get("cursor")]
				writeJSON(w, http.StatusOK, `{"vpcs":[{"id":"v1"}],"meta":{"total":1,"links":{"next":"`+next+
					`","prev":""}}}`)
			})
			vpcs, err := newTestClient(t, srv.Server).ListVPCs(t.Context())
			noKey(t, err)
			const want = `vultr: GET /v2/vpcs: the cursor "c1" repeats`
			if err == nil || err.Error() != want || vpcs != nil {
				t.Errorf("ListVPCs = %v, %v; want the error %q", vpcs, err, want)
			}
			if n := len(srv.requests()); n != tc.calls {
				t.Errorf("the server got %d requests, want %d", n, tc.calls)
			}
		})
	}
}

func TestClientListNeedsMeta(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
	}{
		{"no body", ""},
		{"an empty object", `{}`},
		{"an error", `{"error":"x"}`},
		{"a list without meta", `{"vpcs":[{"id":"v1"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, answer(200, tc.answer))
			vpcs, err := newTestClient(t, srv.Server).ListVPCs(t.Context())
			const want = "vultr: GET /v2/vpcs: the answer holds no list"
			if err == nil || err.Error() != want || vpcs != nil {
				t.Errorf("ListVPCs = %v, %v; want the error %q", vpcs, err, want)
			}
			wantClass(t, err, nil)
		})
	}
	t.Run("an empty list", func(t *testing.T) {
		srv := newAPIServer(t, answer(200, `{"vpcs":[],"meta":{"total":0,"links":{"next":"","prev":""}}}`))
		vpcs, err := newTestClient(t, srv.Server).ListVPCs(t.Context())
		if err != nil || len(vpcs) != 0 {
			t.Errorf("ListVPCs = %v, %v; want an empty list", vpcs, err)
		}
	})
}

func TestClientListPageLimit(t *testing.T) {
	var pages int
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) { // a new cursor on every page
		pages++
		body := fmt.Sprintf(`{"vpcs":[],"meta":{"total":0,"links":{"next":"c%d","prev":""}}}`, pages)
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(body)),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})
	c, err := NewClient(testKey, WithRoundTripper(base), WithInterval(0))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	vpcs, err := c.ListVPCs(t.Context())
	const want = "vultr: GET /v2/vpcs: more than 1000 pages"
	if err == nil || err.Error() != want || vpcs != nil {
		t.Errorf("ListVPCs = %v, %v; want the error %q", vpcs, err, want)
	}
	if pages != 1000 {
		t.Errorf("the client got %d pages, want 1000", pages)
	}
}

// answers returns a handler that answers the requests in turn with hs, and with the last one after that.
func answers(hs ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := hs[min(n, len(hs)-1)]
		n++
		mu.Unlock()
		h(w, r)
	}
}

func TestClientErrors(t *testing.T) {
	var (
		deleteVPC = func(ctx context.Context, c API) error { return c.DeleteVPC(ctx, "v1") }
		createVPC = func(ctx context.Context, c API) error {
			_, err := c.CreateVPC(ctx, &govultr.VPCReq{Region: "ams", V4Subnet: "10.64.0.0", V4SubnetMask: 8})
			return err
		}
		createKey = func(ctx context.Context, c API) error {
			_, err := c.CreateSSHKey(ctx, &govultr.SSHKeyReq{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
			return err
		}
		listKeys = func(ctx context.Context, c API) error {
			_, err := c.ListSSHKeys(ctx)
			return err
		}
		deleteInstance = func(ctx context.Context, c API) error { return c.DeleteInstance(ctx, "i1") }
		createInstance = func(ctx context.Context, c API) error {
			_, err := c.CreateInstance(ctx, &createInstanceReq)
			return err
		}
		getInstance = func(ctx context.Context, c API) error {
			_, err := c.GetInstance(ctx, "i1")
			return err
		}
		haltInstance   = func(ctx context.Context, c API) error { return c.HaltInstance(ctx, "i1") }
		updateInstance = func(ctx context.Context, c API) error {
			return c.UpdateInstance(ctx, "i1", &govultr.InstanceUpdateReq{UserData: "c3R1Ygo="})
		}
	)
	for _, tc := range []struct {
		name    string
		call    func(context.Context, API) error
		handler http.HandlerFunc
		kind    error     // the class the error matches; nil for none
		api     *APIError // the error as an *APIError; nil when it is none
		text    string    // the error's text
		calls   []string  // the methods of the requests the server gets
		as      any       // a pointer to a type that the error wraps, or nil
	}{
		{
			name:    "404 on a delete",
			call:    deleteVPC,
			handler: answer(404, `{"error":"Invalid VPC ID.","status":404}`),
			kind:    ErrNotFound,
			api:     &APIError{Method: "DELETE", Path: "/v2/vpcs/v1", Status: 404, Message: "Invalid VPC ID."},
			text:    "vultr: DELETE /v2/vpcs/v1: 404 Not Found: Invalid VPC ID.",
			calls:   []string{"DELETE"},
		},
		{
			name:    "400 with a message",
			call:    createVPC,
			handler: answer(400, `{"error":"Invalid subnet mask: must be 16 to 29.","status":400}`),
			kind:    ErrInvalid,
			api: &APIError{
				Method: "POST", Path: "/v2/vpcs", Status: 400, Message: "Invalid subnet mask: must be 16 to 29.",
			},
			text:  "vultr: POST /v2/vpcs: 400 Bad Request: Invalid subnet mask: must be 16 to 29.",
			calls: []string{"POST"},
		},
		{
			name:    "500 on a POST is sent once",
			call:    createKey,
			handler: answer(500, `{"error":"Internal error.","status":500}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "POST", Path: "/v2/ssh-keys", Status: 500, Message: "Internal error."},
			text:    "vultr: POST /v2/ssh-keys: 500 Internal Server Error: Internal error.",
			calls:   []string{"POST"},
		},
		{
			name:    "503 on every GET is sent 4 times",
			call:    listKeys,
			handler: answer(503, `{"error":"Try again.","status":503}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "GET", Path: "/v2/ssh-keys", Status: 503, Message: "Try again."},
			text:    "vultr: GET /v2/ssh-keys: 503 Service Unavailable: Try again.",
			calls:   []string{"GET", "GET", "GET", "GET"},
		},
		{
			name: "429 on a GET is retried",
			call: listKeys,
			handler: answers(
				answer(429, `{"error":"Rate limit reached.","status":429}`),
				answer(200, `{"ssh_keys":[],"meta":{"total":0,"links":{"next":"","prev":""}}}`),
			),
			calls: []string{"GET", "GET"},
		},
		{
			name:    "a success govultr cannot decode",
			call:    listKeys,
			handler: answer(200, `{"ssh_keys":[{"id":`),
			text:    "vultr: GET /v2/ssh-keys: unexpected end of JSON input",
			calls:   []string{"GET"},
			as:      new(*json.SyntaxError),
		},
		{
			name:    "a success status that govultr does not read",
			call:    listKeys,
			handler: answer(207, `{"ssh_keys":[],"meta":{"total":0,"links":{"next":"","prev":""}}}`),
			text:    "vultr: GET /v2/ssh-keys: a success with the status 207, which the client does not read",
			calls:   []string{"GET"},
		},
		{
			name: "a success that is not JSON",
			call: listKeys,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = io.WriteString(w, `{"ssh_keys":[{"id":"k1"}],"meta":{"total":1,"links":{"next":"","prev":""}}}`)
			},
			text:  `vultr: GET /v2/ssh-keys: an answer of type "application/json; charset=utf-8", not application/json`,
			calls: []string{"GET"},
		},
		{
			name:    "a create without the object",
			call:    createVPC,
			handler: answer(201, `{}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "POST", Path: "/v2/vpcs", Status: 201},
			text:    "vultr: POST /v2/vpcs: the answer holds no object",
			calls:   []string{"POST"},
		},
		{
			name:    "a create answer that does not decode",
			call:    createVPC,
			handler: answer(201, `{"vpc":{"id":`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "POST", Path: "/v2/vpcs", Status: 201},
			text:    "vultr: POST /v2/vpcs: unexpected end of JSON input",
			calls:   []string{"POST"},
			as:      new(*json.SyntaxError),
		},
		{
			name: "a create answer that is not JSON",
			call: createVPC,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"vpc":{"id":"v1"}}`)
			},
			kind:  ErrUnavailable,
			api:   &APIError{Method: "POST", Path: "/v2/vpcs", Status: 201},
			text:  `vultr: POST /v2/vpcs: an answer of type "text/html", not application/json`,
			calls: []string{"POST"},
		},
		{
			name:    "404 on an instance delete",
			call:    deleteInstance,
			handler: answer(404, `{"error":"Invalid instance ID.","status":404}`),
			kind:    ErrNotFound,
			api:     &APIError{Method: "DELETE", Path: "/v2/instances/i1", Status: 404, Message: "Invalid instance ID."},
			text:    "vultr: DELETE /v2/instances/i1: 404 Not Found: Invalid instance ID.",
			calls:   []string{"DELETE"},
		},
		{
			name:    "500 on an instance create is sent once",
			call:    createInstance,
			handler: answer(500, `{"error":"Internal error.","status":500}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "POST", Path: "/v2/instances", Status: 500, Message: "Internal error."},
			text:    "vultr: POST /v2/instances: 500 Internal Server Error: Internal error.",
			calls:   []string{"POST"},
		},
		{
			name:    "500 on a halt is sent once",
			call:    haltInstance,
			handler: answer(500, `{"error":"Internal error.","status":500}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "POST", Path: "/v2/instances/i1/halt", Status: 500, Message: "Internal error."},
			text:    "vultr: POST /v2/instances/i1/halt: 500 Internal Server Error: Internal error.",
			calls:   []string{"POST"},
		},
		{
			name:    "503 on every PATCH is sent 4 times",
			call:    updateInstance,
			handler: answer(503, `{"error":"Try again.","status":503}`),
			kind:    ErrUnavailable,
			api:     &APIError{Method: "PATCH", Path: "/v2/instances/i1", Status: 503, Message: "Try again."},
			text:    "vultr: PATCH /v2/instances/i1: 503 Service Unavailable: Try again.",
			calls:   []string{"PATCH", "PATCH", "PATCH", "PATCH"},
		},
		{
			name:    "an instance answer without the instance",
			call:    getInstance,
			handler: answer(200, `{}`),
			text:    "vultr: GET /v2/instances/i1: the answer holds no object",
			calls:   []string{"GET"},
		},
		{
			name: "a redirect",
			call: listKeys,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://api.example.com/v2/ssh-keys")
				w.WriteHeader(http.StatusFound)
			},
			api:   &APIError{Method: "GET", Path: "/v2/ssh-keys", Status: 302},
			text:  "vultr: GET /v2/ssh-keys: 302 Found",
			calls: []string{"GET"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, tc.handler)
			err := tc.call(t.Context(), newTestClient(t, srv.Server))
			noKey(t, err)
			wantClass(t, err, tc.kind)
			if tc.text == "" {
				if err != nil {
					t.Errorf("err = %v, want none", err)
				}
			} else if err == nil || err.Error() != tc.text {
				t.Errorf("err = %v, want %q", err, tc.text)
			}
			e, ok := errors.AsType[*APIError](err)
			switch {
			case tc.api == nil && ok:
				t.Errorf("err is the *APIError %+v, want none", e)
			case tc.api != nil && !ok:
				t.Errorf("err = %v, want an *APIError", err)
			case ok:
				opts := cmpopts.IgnoreUnexported(APIError{})
				if diff := cmp.Diff(tc.api, e, opts); diff != "" {
					t.Errorf("*APIError (-want +got):\n%s", diff)
				}
			}
			if tc.as != nil && !errors.As(err, tc.as) {
				t.Errorf("err = %v, want it to wrap a %T", err, tc.as)
			}
			var methods []string
			for _, r := range srv.requests() {
				methods = append(methods, r.Method)
			}
			if diff := cmp.Diff(tc.calls, methods); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClientDeleteReadsNoAnswer checks that a delete, which reads nothing from its answer, succeeds whatever the
// type of the answer's body.
func TestClientDeleteReadsNoAnswer(t *testing.T) {
	srv := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "deleted")
	})
	if err := newTestClient(t, srv.Server).DeleteVPC(t.Context(), "v1"); err != nil {
		t.Errorf("DeleteVPC = %v, want nil", err)
	}
}

func TestClientContextErrors(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		srv := newAPIServer(t, answer(204, ""))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := newTestClient(t, srv.Server).DeleteVPC(ctx, "v1")
		noKey(t, err)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("DeleteVPC = %v, want context.Canceled and ErrUnavailable", err)
		}
		if n := len(srv.requests()); n != 0 {
			t.Errorf("the server got %d requests, want none", n)
		}
	})
	t.Run("canceled while the server has the request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv := newAPIServer(t, func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		})
		_, err := newTestClient(t, srv.Server).CreateVPC(ctx, &govultr.VPCReq{Region: "ams"})
		noKey(t, err)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("CreateVPC = %v, want context.Canceled and ErrUnavailable", err)
		}
		if n := len(srv.requests()); n != 1 {
			t.Errorf("the server got %d requests, want 1", n)
		}
	})
}

func TestCallNoRequestSent(t *testing.T) {
	cause := errors.New(`parse "/v2/vpcs/%zz": invalid URL escape "%zz"`)
	_, err := call(t.Context(), func(context.Context) error { return cause })
	const want = `vultr: no request sent: parse "/v2/vpcs/%zz": invalid URL escape "%zz"`
	if err == nil || err.Error() != want || !errors.Is(err, cause) {
		t.Errorf("call = %v, want %q wrapping the cause", err, want)
	}
	wantClass(t, err, nil)
}

func TestClientChecksIDs(t *testing.T) {
	const good = "ok-1f2e"
	// One answer that every call below can read: a list, a rule, the plans and an instance.
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, `{"firewall_rules":[],"firewall_rule":{"id":1},"available_plans":[],`+
			`"instance":{"id":"i1"},"meta":{"total":0,"links":{"next":"","prev":""}}}`)
	})
	c := newTestClient(t, srv.Server)
	for _, tc := range []struct {
		route string // the call as errors name it
		name  string // the id's name in errors
		call  func(ctx context.Context, id string) error
		path  string // the path of the request with the good id
	}{
		{"DELETE /v2/ssh-keys/{id}", "id", c.DeleteSSHKey, "/v2/ssh-keys/" + good},
		{"DELETE /v2/vpcs/{id}", "id", c.DeleteVPC, "/v2/vpcs/" + good},
		{"DELETE /v2/firewalls/{id}", "id", c.DeleteFirewallGroup, "/v2/firewalls/" + good},
		{
			"GET /v2/firewalls/{groupID}/rules", "groupID",
			func(ctx context.Context, id string) error {
				_, err := c.ListFirewallRules(ctx, id)
				return err
			},
			"/v2/firewalls/" + good + "/rules",
		},
		{
			"POST /v2/firewalls/{groupID}/rules", "groupID",
			func(ctx context.Context, id string) error {
				_, err := c.CreateFirewallRule(ctx, id, &govultr.FirewallRuleReq{IPType: "v4", Protocol: "icmp"})
				return err
			},
			"/v2/firewalls/" + good + "/rules",
		},
		{
			"DELETE /v2/firewalls/{groupID}/rules/{ruleID}", "groupID",
			func(ctx context.Context, id string) error { return c.DeleteFirewallRule(ctx, id, 1) },
			"/v2/firewalls/" + good + "/rules/1",
		},
		{
			"GET /v2/regions/{region}/availability", "region",
			func(ctx context.Context, id string) error {
				_, err := c.AvailablePlans(ctx, id, "vc2")
				return err
			},
			"/v2/regions/" + good + "/availability",
		},
		{
			"GET /v2/instances/{id}", "id",
			func(ctx context.Context, id string) error {
				_, err := c.GetInstance(ctx, id)
				return err
			},
			"/v2/instances/" + good,
		},
		{"DELETE /v2/instances/{id}", "id", c.DeleteInstance, "/v2/instances/" + good},
		{"POST /v2/instances/{id}/halt", "id", c.HaltInstance, "/v2/instances/" + good + "/halt"},
		{
			"PATCH /v2/instances/{id}", "id",
			func(ctx context.Context, id string) error {
				return c.UpdateInstance(ctx, id, &govultr.InstanceUpdateReq{Label: "l"})
			},
			"/v2/instances/" + good,
		},
		{
			"GET /v2/instances/{id}/vpcs", "id",
			func(ctx context.Context, id string) error {
				_, err := c.ListInstanceVPCs(ctx, id)
				return err
			},
			"/v2/instances/" + good + "/vpcs",
		},
	} {
		t.Run(tc.route, func(t *testing.T) {
			before := len(srv.requests())
			for _, id := range []string{"", "/", "..", "a/b", "%zz", "a b", "é"} {
				err := tc.call(t.Context(), id)
				noKey(t, err)
				want := fmt.Sprintf("vultr: %s: invalid %s %q", tc.route, tc.name, id)
				if err == nil || err.Error() != want {
					t.Errorf("id %q: err = %v, want %q", id, err, want)
				}
			}
			if got := srv.requests()[before:]; len(got) != 0 {
				t.Errorf("bad ids sent %d requests, want none", len(got))
			}
			if err := tc.call(t.Context(), good); err != nil {
				t.Errorf("id %q: %v", good, err)
			}
			if got := srv.requests()[before:]; len(got) != 1 || got[0].Path != tc.path {
				t.Errorf("the id %q sent %d requests, want one to %s", good, len(got), tc.path)
			}
		})
	}

	t.Run("the literal forms", func(t *testing.T) {
		before := len(srv.requests())
		for _, tc := range []struct {
			err  error
			want string
		}{
			{c.DeleteVPC(t.Context(), ""), `vultr: DELETE /v2/vpcs/{id}: invalid id ""`},
			{c.DeleteSSHKey(t.Context(), "../x"), `vultr: DELETE /v2/ssh-keys/{id}: invalid id "../x"`},
			{c.DeleteFirewallRule(t.Context(), good, 0),
				`vultr: DELETE /v2/firewalls/{groupID}/rules/{ruleID}: invalid ruleID 0`},
			{c.DeleteFirewallRule(t.Context(), good, -1),
				`vultr: DELETE /v2/firewalls/{groupID}/rules/{ruleID}: invalid ruleID -1`},
			// A list of instances without a tag would hold every instance of the account.
			{listInstances(t, c, ""), `vultr: GET /v2/instances: invalid tag ""`},
		} {
			if tc.err == nil || tc.err.Error() != tc.want {
				t.Errorf("err = %v, want %q", tc.err, tc.want)
			}
		}
		if n := len(srv.requests()) - before; n != 0 {
			t.Errorf("the server got %d requests, want none", n)
		}
	})
}

// listInstances returns the error of c.ListInstances with the tag, and fails the test when it lists instances.
func listInstances(t *testing.T, c API, tag string) error {
	t.Helper()
	got, err := c.ListInstances(t.Context(), tag)
	if got != nil {
		t.Errorf("ListInstances(%q) = %v, want none", tag, got)
	}
	return err
}

func TestCheckID(t *testing.T) {
	const route = "DELETE /v2/vpcs/{id}"
	for _, id := range []string{"a", "vpc-1", "cb676a46-66fd-4dfb-b839-443f2e6c0b60", "AMS"} {
		if err := CheckID(route, "id", id); err != nil {
			t.Errorf("CheckID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", "/", "..", "a/b", "%zz", "a b", "a_b", "é", "a?b"} {
		want := fmt.Sprintf("vultr: DELETE /v2/vpcs/{id}: invalid id %q", id)
		if err := CheckID(route, "id", id); err == nil || err.Error() != want {
			t.Errorf("CheckID(%q) = %v, want %q", id, err, want)
		}
	}
}

func TestCheckTag(t *testing.T) {
	for _, tag := range []string{"tent/cluster=prod", "TENT/op=1", " "} {
		if err := CheckTag(tag); err != nil {
			t.Errorf("CheckTag(%q) = %v, want nil", tag, err)
		}
	}
	const want = `vultr: GET /v2/instances: invalid tag ""`
	if err := CheckTag(""); err == nil || err.Error() != want {
		t.Errorf(`CheckTag("") = %v, want %q`, err, want)
	}
}

// TestClientCreateInstanceHidesThePassword checks that no error of an instance create shows the root password that
// the create answer holds, whatever is wrong with the answer.
func TestClientCreateInstanceHidesThePassword(t *testing.T) {
	body := `{"instance":{"id":"i1","default_password":"` + password + `"}}`
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			"a success that is not JSON",
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, body)
			},
		},
		{"a success that does not decode", answer(202, `{"instance":{"default_password":"`+password+`","ram":"x"}}`)},
		{"a cut success", answer(202, strings.TrimSuffix(body, "}}"))},
		{"a success status that govultr does not read", answer(207, body)},
		{"an unknown success status", answer(299, body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAPIServer(t, tc.handler)
			in, err := newTestClient(t, srv.Server).CreateInstance(t.Context(), &createInstanceReq)
			if err == nil || in != nil {
				t.Fatalf("CreateInstance = %+v, %v; want an error", in, err)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), password) {
					t.Errorf("the error shows the password: %v", e)
				}
			}
			wantClass(t, err, ErrUnavailable)
			if n := len(srv.requests()); n != 1 {
				t.Errorf("the server got %d requests, want 1", n)
			}
		})
	}
}

func TestCheckRuleID(t *testing.T) {
	const route = "DELETE /v2/firewalls/{groupID}/rules/{ruleID}"
	for _, id := range []int{1, 50} {
		if err := CheckRuleID(route, id); err != nil {
			t.Errorf("CheckRuleID(%d) = %v, want nil", id, err)
		}
	}
	for _, id := range []int{0, -1} {
		want := fmt.Sprintf("vultr: %s: invalid ruleID %d", route, id)
		if err := CheckRuleID(route, id); err == nil || err.Error() != want {
			t.Errorf("CheckRuleID(%d) = %v, want %q", id, err, want)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	other := newAPIServer(t, answer(200, `{"ssh_keys":[],"meta":{"total":0,"links":{"next":"","prev":""}}}`))
	srv := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	})
	keys, err := newTestClient(t, srv.Server).ListSSHKeys(t.Context())
	noKey(t, err)
	if e, ok := errors.AsType[*APIError](err); !ok || e.Status != http.StatusFound || keys != nil {
		t.Errorf("ListSSHKeys = %v, %v; want the 302 as an *APIError", keys, err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("the server got %d requests, want 1", n)
	}
	if n := len(other.requests()); n != 0 {
		t.Errorf("the other server got %d requests, want none", n)
	}
}

func TestNewClient(t *testing.T) {
	t.Run("no key", func(t *testing.T) {
		if c, err := NewClient(""); err == nil || c != nil {
			t.Errorf(`NewClient("") = %v, %v; want an error`, c, err)
		}
	})

	t.Run("bad base URL", func(t *testing.T) {
		for _, u := range []string{"", "api.vultr.com", "ftp://api.vultr.com", "https://", "https://api.vultr.com/%zz"} {
			c, err := NewClient(testKey, WithBaseURL(u))
			noKey(t, err)
			if err == nil || c != nil {
				t.Errorf("NewClient with the base URL %q = %v, %v; want an error", u, c, err)
			}
		}
	})

	t.Run("http only to a loopback host", func(t *testing.T) {
		for _, u := range []string{
			"http://localhost", "http://LocalHost:8080", "http://127.0.0.1:8080", "http://127.9.9.9", "http://[::1]:8080",
			"https://api.vultr.com", "https://10.0.0.1",
		} {
			if _, err := NewClient(testKey, WithBaseURL(u)); err != nil {
				t.Errorf("NewClient with the base URL %q: %v", u, err)
			}
		}
		for _, u := range []string{
			"http://api.vultr.com", "http://10.0.0.1", "http://128.0.0.1", "http://[::2]", "http://localhost.example.com",
		} {
			c, err := NewClient(testKey, WithBaseURL(u))
			noKey(t, err)
			want := fmt.Sprintf("vultr: the base URL %q uses http, which only localhost, 127.0.0.0/8 and ::1 may use", u)
			if err == nil || err.Error() != want || c != nil {
				t.Errorf("NewClient with the base URL %q = %v, %v; want the error %q", u, c, err, want)
			}
		}
	})

	t.Run("key with whitespace or control characters", func(t *testing.T) {
		for _, key := range []string{
			" " + testKey, testKey + " ", "key 1", testKey + "\n", testKey + "\r", "\t" + testKey, testKey + "\x00",
			testKey + "\x7f", testKey + "\u00a0", testKey + "\u2028",
		} {
			c, err := NewClient(key)
			noKey(t, err)
			const want = "vultr: the API key has whitespace or control characters"
			if err == nil || err.Error() != want || c != nil {
				t.Errorf("NewClient with the key %q = %v, %v; want the error %q", strings.ReplaceAll(key, testKey, "<key>"),
					c, err, want)
			}
		}
	})

	t.Run("default round tripper", func(t *testing.T) {
		srv := newAPIServer(t, answer(200, `{"os":[{"id":2284}],"meta":{"total":1,"links":{"next":"","prev":""}}}`))
		c, err := NewClient(testKey, WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		got, err := c.ListOS(t.Context())
		if err != nil || !slices.Equal(got, []govultr.OS{{ID: 2284}}) {
			t.Errorf("ListOS = %v, %v; want [2284]", got, err)
		}
		if r := srv.requests(); len(r) != 1 || r[0].Auth != auth {
			t.Errorf("the server got %+v, want one request with the key", r)
		}
	})
}

func TestNewHTTPTransport(t *testing.T) {
	tr := newHTTPTransport()
	if tr.TLSHandshakeTimeout != 30*time.Second || tr.ResponseHeaderTimeout != time.Minute {
		t.Errorf("TLSHandshakeTimeout %v, ResponseHeaderTimeout %v; want 30s and 1m", tr.TLSHandshakeTimeout,
			tr.ResponseHeaderTimeout)
	}
	if tr.DialContext == nil || tr.Proxy == nil {
		t.Error("the transport has no dialer or no proxy setting of http.DefaultTransport")
	}
	if tr == http.DefaultTransport {
		t.Error("the transport is http.DefaultTransport itself, want a copy")
	}
}

func TestNewHTTPTransportReplacedDefault(t *testing.T) {
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("the replaced default transport")
	})
	tr := newHTTPTransport()
	if tr.TLSHandshakeTimeout != 30*time.Second || tr.ResponseHeaderTimeout != time.Minute || tr.DialContext == nil ||
		tr.Proxy == nil {
		t.Errorf("newHTTPTransport = %+v, want the bounded waits and the proxy from the environment", tr)
	}
}
