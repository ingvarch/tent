package nomadops_test

import (
	"net/http"
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

// nodesJSON is how Nomad lists four client nodes: one that registers, one that is drained, one that is ready, and one
// of the same name that went down before it.
const nodesJSON = `[
{"ID":"4b1e","Name":"prod-workers-2","NodePool":"default","Status":"initializing","SchedulingEligibility":"eligible",
 "Drain":false,"Version":"2.0.7","CreateIndex":40},
{"ID":"3c2d","Name":"prod-workers-1","NodePool":"default","Status":"ready","SchedulingEligibility":"ineligible",
 "Drain":true,"Version":"2.0.7","CreateIndex":30},
{"ID":"2d3c","Name":"prod-workers-0","NodePool":"default","Status":"ready","SchedulingEligibility":"eligible",
 "Drain":false,"Version":"2.0.7","CreateIndex":20},
{"ID":"1e4b","Name":"prod-workers-0","NodePool":"default","Status":"down","SchedulingEligibility":"eligible",
 "Drain":false,"Version":"2.0.7","CreateIndex":10}
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
		{Name: "prod-workers-2", Status: "initializing", Eligible: true},
		{Name: "prod-workers-1", Status: "ready", Eligible: false},
		{Name: "prod-workers-0", Status: "ready", Eligible: true},
		{Name: "prod-workers-0", Status: "down", Eligible: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
	checkRequests(t, srv, clientTokens(token), nodesRequest)
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
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
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
