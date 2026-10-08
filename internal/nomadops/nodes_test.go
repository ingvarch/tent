package nomadops_test

import (
	"net/http"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// nodesRequest is the request of Nodes.
var nodesRequest = gotRequest{Method: http.MethodGet, Path: "/v1/nodes", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// nodesJSON is how Nomad lists five client nodes: one that registers and was never drained ("LastDrain": null), one
// that drains, one that is ready after a drain that completed with meta, one of the same name that went down before
// it, at another address, and one whose drain was canceled.
const nodesJSON = `[
{"ID":"4b1e","Name":"prod-workers-2","Address":"10.64.0.8","NodePool":"default","Status":"initializing",
 "SchedulingEligibility":"eligible","Drain":false,"Version":"2.0.7","LastDrain":null,"CreateIndex":40},
{"ID":"3c2d","Name":"prod-workers-1","Address":"10.64.0.7","NodePool":"default","Status":"ready",
 "SchedulingEligibility":"ineligible","Drain":true,"Version":"2.0.7","CreateIndex":30,
 "LastDrain":{"StartedAt":"2026-10-08T01:14:10Z","UpdatedAt":"2026-10-08T01:14:10Z","Status":"draining",
  "AccessorID":"a-1","Meta":{"tent_machine":"m-1"}}},
{"ID":"2d3c","Name":"prod-workers-0","Address":"10.64.0.6","NodePool":"default","Status":"ready",
 "SchedulingEligibility":"ineligible","Drain":false,"Version":"2.0.7","CreateIndex":20,
 "LastDrain":{"StartedAt":"2026-10-08T01:10:00Z","UpdatedAt":"2026-10-08T01:10:14Z","Status":"complete",
  "AccessorID":"a-1","Meta":{"tent_machine":"m-0","tent_run":"r-9"}}},
{"ID":"1e4b","Name":"prod-workers-0","Address":"fd00::6","NodePool":"default","Status":"down",
 "SchedulingEligibility":"eligible","Drain":false,"Version":"2.0.7","LastDrain":null,"CreateIndex":10},
{"ID":"0f5a","Name":"prod-workers-3","Address":"10.64.0.9","NodePool":"default","Status":"ready",
 "SchedulingEligibility":"eligible","Drain":false,"Version":"2.0.7","CreateIndex":5,
 "LastDrain":{"Status":"canceled"}}
]`

func TestNodes(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, nodesJSON))
	got, err := newClient(t, srv, p, token).Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %s", show(t, err, clientTokens(token)))
	}
	want := []nomadops.Node{
		{ID: "4b1e", Name: "prod-workers-2", Status: "initializing", Eligible: true,
			Address: netip.MustParseAddr("10.64.0.8"), Version: "2.0.7"},
		{ID: "3c2d", Name: "prod-workers-1", Status: "ready", Eligible: false, Draining: true,
			LastDrain: nomadops.LastDrain{Status: "draining", Meta: map[string]string{"tent_machine": "m-1"}},
			Address:   netip.MustParseAddr("10.64.0.7"), Version: "2.0.7"},
		{ID: "2d3c", Name: "prod-workers-0", Status: "ready", Eligible: false,
			LastDrain: nomadops.LastDrain{Status: "complete",
				Meta: map[string]string{"tent_machine": "m-0", "tent_run": "r-9"}},
			Address: netip.MustParseAddr("10.64.0.6"), Version: "2.0.7"},
		{ID: "1e4b", Name: "prod-workers-0", Status: "down", Eligible: true, Address: netip.MustParseAddr("fd00::6"),
			Version: "2.0.7"},
		{ID: "0f5a", Name: "prod-workers-3", Status: "ready", Eligible: true,
			LastDrain: nomadops.LastDrain{Status: "canceled"}, Address: netip.MustParseAddr("10.64.0.9"),
			Version: "2.0.7"},
	}
	if diff := cmp.Diff(want, got, equateAddrs); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
	checkRequests(t, srv, clientTokens(token), nodesRequest)
}

// TestNodesAddress checks that a node whose address is missing or does not parse lists the invalid address.
func TestNodesAddress(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `[
{"Name":"a","Status":"ready","SchedulingEligibility":"eligible"},
{"Name":"b","Address":"not-an-address","Status":"ready","SchedulingEligibility":"eligible"},
{"Name":"c","Address":"","Status":"ready","SchedulingEligibility":"eligible"}]`))
	got, err := newClient(t, srv, p, token).Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %s", show(t, err, clientTokens(token)))
	}
	if len(got) != 3 {
		t.Fatalf("Nodes() = %v, want 3 nodes", got)
	}
	for _, n := range got {
		if n.Address.IsValid() {
			t.Errorf("node %s has the address %s, want the invalid one", n.Name, n.Address)
		}
	}
}

func TestNodeIs(t *testing.T) {
	addr := netip.MustParseAddr("10.64.0.6")
	node := nomadops.Node{Name: "prod-workers-0", Address: addr}
	for _, tc := range []struct {
		name string
		node nomadops.Node
		arg  string
		addr netip.Addr
		want bool
	}{
		{"same name and address", node, "prod-workers-0", addr, true},
		{"another address", node, "prod-workers-0", netip.MustParseAddr("10.64.0.7"), false},
		{"another name", node, "prod-workers-1", addr, false},
		{"an invalid address never matches", nomadops.Node{Name: "prod-workers-0"}, "prod-workers-0", netip.Addr{}, false},
		{"a node without an address", nomadops.Node{Name: "prod-workers-0"}, "prod-workers-0", addr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.Is(tc.arg, tc.addr); got != tc.want {
				t.Errorf("%+v.Is(%q, %v) = %v, want %v", tc.node, tc.arg, tc.addr, got, tc.want)
			}
		})
	}
}

// TestNodesSkipsNull checks that a list with null in it gives the nodes that it holds.
func TestNodesSkipsNull(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for name, body := range map[string]string{
		"null alone": `[null]`,
		"null around nodes": `[null,{"Name":"prod-workers-0","Status":"ready","SchedulingEligibility":"eligible",` +
			`"CreateIndex":20},null,{"Name":"prod-workers-1","Status":"initializing","SchedulingEligibility":"eligible",` +
			`"CreateIndex":30}]`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, body))
			got, err := newClient(t, srv, p, token).Nodes(t.Context())
			if err != nil {
				t.Fatalf("Nodes: %s", show(t, err, clientTokens(token)))
			}
			var want []nomadops.Node
			if name != "null alone" {
				want = []nomadops.Node{
					{Name: "prod-workers-0", Status: "ready", Eligible: true},
					{Name: "prod-workers-1", Status: "initializing", Eligible: true},
				}
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty(), equateAddrs); diff != "" {
				t.Errorf("Nodes() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNodeReady(t *testing.T) {
	for _, tc := range []struct {
		node nomadops.Node
		want bool
	}{
		{nomadops.Node{Status: "ready", Eligible: true}, true},
		{nomadops.Node{Status: "ready", Eligible: false}, false},
		{nomadops.Node{Status: "initializing", Eligible: true}, false},
		{nomadops.Node{Status: "down", Eligible: true}, false},
		{nomadops.Node{Status: "disconnected", Eligible: true}, false},
	} {
		if got := tc.node.Ready(); got != tc.want {
			t.Errorf("%+v.Ready() = %v, want %v", tc.node, got, tc.want)
		}
	}
}

func TestNodesErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no leader", http.StatusInternalServerError, "No cluster leader", "nomad: GET /v1/nodes: 500: No cluster leader",
			true},
		{"no permission", http.StatusForbidden, "Permission denied", "nomad: GET /v1/nodes: 403: Permission denied", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).Nodes(t.Context())
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if got != nil {
				t.Errorf("Nodes() = %v with the error, want nil", got)
			}
			checkRequests(t, srv, clientTokens(token), nodesRequest)
		})
	}
}
