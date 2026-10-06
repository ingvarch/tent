package app

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
)

// texts returns the addresses as text.
func texts(addrs []netip.Addr) []string {
	var out []string
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// TestLastAddresses gives the addresses of an IPv4 prefix from its last one down, and the prefix's address, repeated,
// for a prefix that has no more room and for IPv6.
func TestLastAddresses(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		n      int
		want   []string
	}{
		{"10.64.0.0/16", 3, []string{"10.64.255.255", "10.64.255.254", "10.64.255.253"}},
		{"10.64.16.0/20", 2, []string{"10.64.31.255", "10.64.31.254"}},
		{"192.168.1.128/25", 1, []string{"192.168.1.255"}},
		{"10.0.0.0/8", 1, []string{"10.255.255.255"}},
		{"10.64.0.5/32", 3, []string{"10.64.0.5", "10.64.0.5", "10.64.0.5"}},
		{"fd00::/64", 2, []string{"fd00::", "fd00::"}},
		{"10.64.0.0/16", 0, nil},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			got := texts(lastAddresses(netip.MustParsePrefix(tc.prefix), tc.n))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("lastAddresses(%s, %d) (-want +got):\n%s", tc.prefix, tc.n, diff)
			}
		})
	}
}

// TestStandInIntroToken makes a token of 2048 bytes in three dot-separated parts, the same every time.
func TestStandInIntroToken(t *testing.T) {
	got := standInIntroToken()
	if len(got) != standInIntroTokenSize || standInIntroTokenSize != 2048 {
		t.Errorf("the stand-in token has %d bytes, want 2048", len(got))
	}
	if parts := strings.Split(string(got), "."); len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Errorf("the stand-in token has the parts %d, want 3 that are not empty", len(parts))
	}
	if again := standInIntroToken(); string(again) != string(got) {
		t.Error("the stand-in token differs between calls")
	}
}

// TestSeed gives the private addresses of the known servers other than the node, by name, and fails when servers are
// known and none has an address, or when a client has no server to join.
func TestSeed(t *testing.T) {
	in := func(name, private string) cloud.Instance {
		c := cloud.Instance{Name: name}
		if private != "" {
			c.PrivateIP = netip.MustParseAddr(private)
		}
		return c
	}
	for _, tc := range []struct {
		name   string
		known  []cloud.Instance
		node   string
		client bool
		want   []string
		err    string
	}{
		{name: "the first server has no seed", node: "prod-servers-0"},
		{name: "a server joins the others", node: "prod-servers-1",
			known: []cloud.Instance{in("prod-servers-0", "10.64.0.3"), in("prod-servers-1", "10.64.0.4"),
				in("prod-servers-2", "10.64.0.5")},
			want: []string{"10.64.0.3", "10.64.0.5"}},
		{name: "a server that has no address is skipped when another has one", node: "prod-servers-2",
			known: []cloud.Instance{in("prod-servers-0", "10.64.0.3"), in("prod-servers-1", "")},
			want:  []string{"10.64.0.3"}},
		{name: "no server has an address", node: "prod-servers-2",
			known: []cloud.Instance{in("prod-servers-0", ""), in("prod-servers-1", "")},
			err: "node prod-servers-2: no server of cluster prod has a private address yet " +
				"(prod-servers-0, prod-servers-1); run the command again"},
		{name: "a client without any server", node: "prod-workers-0", client: true,
			err: "node prod-workers-0: no server of cluster prod has a private address yet; run the command again"},
		{name: "a client with a server that has no address", node: "prod-workers-0", client: true,
			known: []cloud.Instance{in("prod-servers-0", "")},
			err: "node prod-workers-0: no server of cluster prod has a private address yet (prod-servers-0); " +
				"run the command again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &applier{u: updateRun{cluster: "prod"}, known: tc.known}

			got, err := a.seed(tc.node, tc.client)

			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Errorf("seed error = %v\nwant       %s", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			if diff := cmp.Diff(tc.want, texts(got)); diff != "" {
				t.Errorf("seed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClusterServers counts the machines of the server and combined groups, and ignores clients and a negative size.
func TestClusterServers(t *testing.T) {
	m := &model.Cluster{Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer, Size: 3},
		{Name: "both", Role: v1alpha1.RoleCombined, Size: 2},
		{Name: "workers", Role: v1alpha1.RoleClient, Size: 5},
		{Name: "odd", Role: v1alpha1.RoleServer, Size: -4},
	}}
	if got := clusterServers(m); got != 5 {
		t.Errorf("clusterServers = %d, want 5", got)
	}
}

// TestRegisterNeedsAPrivateAddress fails the registration wait of a machine that has no private address before any
// call: a node cannot be told from its twin without it. The service has no Nomad client, so a Nomad call would fail
// with another error.
func TestRegisterNeedsAPrivateAddress(t *testing.T) {
	err := (&applier{s: &Service{}}).register(t.Context(), cloud.Instance{Name: "prod-workers-1"})

	const want = "node prod-workers-1: the cloud reports no private address for it yet; run the command again"
	if err == nil || err.Error() != want {
		t.Errorf("register = %v, want %q", err, want)
	}
}

// TestCheckVote accepts a machine whose address a voter holds in the Raft configuration, and fails a machine without a
// private address, one that no peer holds and one whose peer does not vote, naming the voters of the cluster.
func TestCheckVote(t *testing.T) {
	peer := func(a string, voter bool) nomadops.Peer {
		return nomadops.Peer{Address: netip.MustParseAddrPort(a + ":4647"), Voter: voter}
	}
	const noVote = "node prod-servers-1: the servers are healthy with %s, but none votes at its address 10.64.0.4"
	machine := cloud.Instance{Name: "prod-servers-1", PrivateIP: netip.MustParseAddr("10.64.0.4")}
	other, voting, idle := peer("10.64.0.3", true), peer("10.64.0.4", true), peer("10.64.0.4", false)
	for _, tc := range []struct {
		name    string
		in      cloud.Instance
		peers   []nomadops.Peer
		voters  int
		wantErr string
	}{
		{name: "a voter at its address", in: machine, peers: []nomadops.Peer{other, voting}, voters: 2},
		{
			name: "no private address", in: cloud.Instance{Name: "prod-servers-1"}, peers: []nomadops.Peer{voting}, voters: 1,
			wantErr: "node prod-servers-1: the cloud reports no private address for it yet; run the command again",
		},
		{
			name: "a peer that does not vote", in: machine, peers: []nomadops.Peer{other, idle}, voters: 3,
			wantErr: fmt.Sprintf(noVote, "3 voters"),
		},
		{
			name: "no peer at its address", in: machine, peers: []nomadops.Peer{other}, voters: 2,
			wantErr: fmt.Sprintf(noVote, "2 voters"),
		},
		{name: "one voter", in: machine, peers: []nomadops.Peer{other}, voters: 1, wantErr: fmt.Sprintf(noVote, "1 voter")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkVote(tc.peers, tc.in, tc.voters)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("checkVote = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Errorf("checkVote = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
