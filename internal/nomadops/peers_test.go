package nomadops_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// peersRequest is the request of Peers.
var peersRequest = gotRequest{Method: http.MethodGet, Path: "/v1/operator/raft/configuration",
	Query: "region=" + region, Token: clientToken, Peer: "cli." + region + ".nomad"}

// peersJSON is how Nomad shows the Raft configuration of three servers: one that votes, one that does not, and one
// that Nomad does not know by name.
const peersJSON = `{"Index":12,"Servers":[
{"ID":"6d1f1e2a-9c1b-4d53-8a0e-1b2c3d4e5f60","Node":"prod-servers-0.eu","Address":"10.64.0.3:4647",
 "Leader":true,"Voter":true,"RaftProtocol":"3"},
{"ID":"7e2a2f3b-0d2c-4e64-9b1f-2c3d4e5f6071","Node":"prod-servers-1.eu","Address":"10.64.0.4:4647",
 "Leader":false,"Voter":false,"RaftProtocol":"3"},
{"ID":"8f3b3a4c-1e3d-4f75-ac20-3d4e5f607182","Node":"(unknown)","Address":"10.64.0.5:4647",
 "Leader":false,"Voter":true,"RaftProtocol":"3"}]}`

func TestPeers(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name string
		body string
		want []nomadops.Peer
	}{
		{"three servers", peersJSON, []nomadops.Peer{
			{Name: "prod-servers-0.eu", Address: netip.MustParseAddrPort("10.64.0.3:4647"), Voter: true},
			{Name: "prod-servers-1.eu", Address: netip.MustParseAddrPort("10.64.0.4:4647"), Voter: false},
			{Name: "(unknown)", Address: netip.MustParseAddrPort("10.64.0.5:4647"), Voter: true},
		}},
		{"an address that does not parse", `{"Servers":[{"Node":"s","Address":"nonsense","Voter":true},null]}`,
			[]nomadops.Peer{{Name: "s", Voter: true}}},
		{"an IPv6 address", `{"Servers":[{"Node":"s","Address":"[fd00::3]:4647","Voter":true}]}`,
			[]nomadops.Peer{{Name: "s", Address: netip.MustParseAddrPort("[fd00::3]:4647"), Voter: true}}},
		{"no server", `{"Servers":[]}`, []nomadops.Peer{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, tc.body))
			got, err := newClient(t, srv, p, token).Peers(t.Context())
			if err != nil {
				t.Fatalf("Peers: %s", show(t, err, clientTokens(token)))
			}
			if diff := cmp.Diff(tc.want, got, equateAddrs); diff != "" {
				t.Errorf("Peers() (-want +got):\n%s", diff)
			}
			checkRequests(t, srv, clientTokens(token), peersRequest)
		})
	}
}

func TestPeersErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	const path = "nomad: GET /v1/operator/raft/configuration: "
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no leader", http.StatusInternalServerError, "No cluster leader", path + "500: No cluster leader", true},
		{"rate limited", http.StatusTooManyRequests, "slow down", path + "429: slow down", true},
		{"no permission", http.StatusForbidden, "Permission denied", path + "403: Permission denied", false},
		{"a bad request", http.StatusBadRequest, "bad", path + "400: bad", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).Peers(t.Context())
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if got != nil {
				t.Errorf("Peers() = %v with the error, want nil", got)
			}
			checkRequests(t, srv, clientTokens(token), peersRequest)
		})
	}
}

func TestFindPeer(t *testing.T) {
	a := nomadops.Peer{Name: "s0", Address: netip.MustParseAddrPort("10.64.0.3:4647"), Voter: true}
	b := nomadops.Peer{Name: "s1", Address: netip.MustParseAddrPort("10.64.0.4:4648")}
	bad := nomadops.Peer{Name: "s2"}
	peers := []nomadops.Peer{bad, a, b}
	for _, tc := range []struct {
		name string
		addr netip.Addr
		want nomadops.Peer
		ok   bool
	}{
		{"by address", netip.MustParseAddr("10.64.0.3"), a, true},
		{"whatever the port", netip.MustParseAddr("10.64.0.4"), b, true},
		{"an unlisted address", netip.MustParseAddr("10.64.0.9"), nomadops.Peer{}, false},
		{"an invalid address", netip.Addr{}, nomadops.Peer{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nomadops.FindPeer(peers, tc.addr)
			if ok != tc.ok || got != tc.want {
				t.Errorf("FindPeer(%v) = %+v, %v; want %+v, %v", tc.addr, got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := nomadops.FindPeer(nil, netip.MustParseAddr("10.64.0.3")); ok {
		t.Error("FindPeer(nil) found a peer")
	}
}

func TestPeersOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
	c := newClient(t, srv, p, token)
	c.SetTimeout(50 * time.Millisecond)
	_, err := c.Peers(t.Context())
	checkCallErr(t, err, "nomad: GET /v1/operator/raft/configuration: no answer within 50ms", true, clientTokens(token))
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("the error matches context.DeadlineExceeded, which a caller would take for the end of its own context")
	}
}
