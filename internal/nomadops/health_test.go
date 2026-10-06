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

// healthRequest is the request of Health.
var healthRequest = gotRequest{Method: http.MethodGet, Path: "/v1/operator/autopilot/health", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// Autopilot's health of three servers, as Nomad answers: all healthy, and after the third one has failed. Nomad
// answers the second with 429.
const (
	healthyJSON = `{"Healthy":true,"FailureTolerance":1,"Leader":"s1","Voters":["s1","s2","s3"],"Servers":[
{"ID":"s1","Name":"prod-servers-0.eu","Address":"10.0.0.5:4647","SerfStatus":"alive","Version":"2.0.7","Leader":true,
 "Healthy":true,"Voter":true},
{"ID":"s2","Name":"prod-servers-1.eu","Address":"10.0.0.6:4647","SerfStatus":"alive","Version":"2.0.7","Healthy":true,
 "Voter":true},
{"ID":"s3","Name":"prod-servers-2.eu","Address":"10.0.0.7:4647","SerfStatus":"alive","Version":"2.0.7","Healthy":true,
 "Voter":true}]}`
	unhealthyJSON = `{"Healthy":false,"FailureTolerance":0,"Leader":"s1","Voters":["s1","s2","s3"],"Servers":[
{"ID":"s1","Name":"prod-servers-0.eu","Address":"10.0.0.5:4647","SerfStatus":"alive","Version":"2.0.7","Leader":true,
 "Healthy":true,"Voter":true},
{"ID":"s2","Name":"prod-servers-1.eu","Address":"10.0.0.6:4647","SerfStatus":"alive","Version":"2.0.7","Healthy":true,
 "Voter":true},
{"ID":"s3","Name":"prod-servers-2.eu","Address":"10.0.0.7:4647","SerfStatus":"failed","Version":"2.0.7",
 "Healthy":false,"Voter":true}]}`
)

// equateAddrs lets cmp compare the addresses of a Health.
var equateAddrs = cmpopts.EquateComparable(netip.Addr{}, netip.AddrPort{})

// threeServers is what Health reports for healthyJSON.
func threeServers() []nomadops.ServerHealth {
	return []nomadops.ServerHealth{
		{Name: "prod-servers-0.eu", Address: netip.MustParseAddrPort("10.0.0.5:4647"), Serf: "alive", Healthy: true,
			Voter: true, Leader: true, Version: "2.0.7"},
		{Name: "prod-servers-1.eu", Address: netip.MustParseAddrPort("10.0.0.6:4647"), Serf: "alive", Healthy: true,
			Voter: true, Version: "2.0.7"},
		{Name: "prod-servers-2.eu", Address: netip.MustParseAddrPort("10.0.0.7:4647"), Serf: "alive", Healthy: true,
			Voter: true, Version: "2.0.7"},
	}
}

func TestHealth(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	failed := threeServers()
	failed[2].Serf, failed[2].Healthy = "failed", false
	left := threeServers()
	left[1].Serf, left[1].Healthy, left[1].Voter, left[1].Version = "left", false, false, ""
	cases := []struct {
		name   string
		status int
		body   string
		want   nomadops.Health
	}{
		{"healthy", http.StatusOK, healthyJSON, nomadops.Health{Healthy: true, Voters: 3, Servers: threeServers()}},
		{"unhealthy, answered with 429", http.StatusTooManyRequests, unhealthyJSON,
			nomadops.Health{Healthy: false, Voters: 3, Servers: failed}},
		{"one voter", http.StatusOK, `{"Healthy":true,"Voters":["s1"]}`, nomadops.Health{Healthy: true, Voters: 1}},
		{"a server that left", http.StatusTooManyRequests, `{"Healthy":false,"Voters":["s1","s3"],"Servers":[
{"Name":"prod-servers-0.eu","Address":"10.0.0.5:4647","SerfStatus":"alive","Version":"2.0.7","Leader":true,
 "Healthy":true,"Voter":true},
{"Name":"prod-servers-1.eu","Address":"10.0.0.6:4647","SerfStatus":"left","Healthy":false},
{"Name":"prod-servers-2.eu","Address":"10.0.0.7:4647","SerfStatus":"alive","Version":"2.0.7","Healthy":true,
 "Voter":true}]}`, nomadops.Health{Voters: 2, Servers: left}},
		{"an address that does not parse", http.StatusOK, `{"Healthy":true,"Voters":["s1"],"Servers":[
{"Name":"a","Address":"not-an-address","SerfStatus":"alive","Healthy":true,"Voter":true},
{"Name":"b","Address":"","SerfStatus":"alive"},
{"Name":"c","Address":"10.0.0.5","SerfStatus":"alive"}]}`, nomadops.Health{Healthy: true, Voters: 1,
			Servers: []nomadops.ServerHealth{{Name: "a", Serf: "alive", Healthy: true, Voter: true},
				{Name: "b", Serf: "alive"}, {Name: "c", Serf: "alive"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).Health(t.Context())
			if err != nil {
				t.Fatalf("Health: %s", show(t, err, clientTokens(token)))
			}
			if diff := cmp.Diff(tc.want, got, equateAddrs); diff != "" {
				t.Errorf("Health() (-want +got):\n%s", diff)
			}
			checkRequests(t, srv, clientTokens(token), healthRequest)
		})
	}
}

func TestHealthErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"429 without a report", http.StatusTooManyRequests, "too many requests",
			"nomad: GET /v1/operator/autopilot/health: 429: too many requests", true},
		{"429 with JSON that is no report", http.StatusTooManyRequests, `{"error":"rate limited"}`,
			`nomad: GET /v1/operator/autopilot/health: 429: {"error":"rate limited"}`, true},
		{"429 with an empty object", http.StatusTooManyRequests, `{}`,
			"nomad: GET /v1/operator/autopilot/health: 429: {}", true},
		{"no leader", http.StatusInternalServerError, "No cluster leader",
			"nomad: GET /v1/operator/autopilot/health: 500: No cluster leader", true},
		{"no permission", http.StatusForbidden, "Permission denied",
			"nomad: GET /v1/operator/autopilot/health: 403: Permission denied", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).Health(t.Context())
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if diff := cmp.Diff(nomadops.Health{}, got, equateAddrs); diff != "" {
				t.Errorf("Health() with the error (-want +got):\n%s", diff)
			}
			checkRequests(t, srv, clientTokens(token), healthRequest)
		})
	}
}
