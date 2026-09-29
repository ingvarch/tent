package nodeconfig_test

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// seeds are the servers that the join goldens show: two IPv4 addresses and an IPv6 one.
var seeds = []netip.Addr{
	netip.MustParseAddr("10.64.0.5"), netip.MustParseAddr("10.64.0.9"), netip.MustParseAddr("fd00:64::c"),
}

func TestRenderJoinGolden(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		t.Run(string(role), func(t *testing.T) {
			got, err := nodeconfig.RenderJoin(role, seeds)
			if err != nil {
				t.Fatalf("RenderJoin(%s): %v", role, err)
			}
			checkGolden(t, string(role)+"_05-join.hcl.golden", string(got.Content))
		})
	}
}

// joinFile is what the tests read back from 05-join.hcl with HCL1, as Nomad reads it. It has the retry_join that
// Nomad 2.1 removes from the server block, to prove that the file never sets it.
type joinFile struct {
	Server *struct {
		RetryJoin  []string    `hcl:"retry_join"`
		ServerJoin *serverJoin `hcl:"server_join"`
	} `hcl:"server"`
	Client *struct {
		ServerJoin *serverJoin `hcl:"server_join"`
	} `hcl:"client"`
	RetryJoin []string `hcl:"retry_join"`
}

type serverJoin struct {
	RetryJoin []string `hcl:"retry_join"`
}

// TestRenderJoinReadsBack reads 05-join.hcl back with HCL1: servers join the servers' serf port in server.server_join,
// clients their RPC port in client.server_join, and combined nodes as servers do, since Nomad gives their client the
// server of the same agent. No file sets the retry_join that Nomad 2.1 removes.
func TestRenderJoinReadsBack(t *testing.T) {
	serf := []string{"10.64.0.5:4648", "10.64.0.9:4648", "[fd00:64::c]:4648"}
	rpc := []string{"10.64.0.5:4647", "10.64.0.9:4647", "[fd00:64::c]:4647"}
	for _, tc := range []struct {
		role                 v1alpha1.Role
		server, client       []string
		hasServer, hasClient bool
	}{
		{v1alpha1.RoleServer, serf, nil, true, false},
		{v1alpha1.RoleClient, nil, rpc, false, true},
		{v1alpha1.RoleCombined, serf, nil, true, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			got := decodeJoin(t, tc.role, seeds)
			if (got.Server != nil) != tc.hasServer || (got.Client != nil) != tc.hasClient {
				t.Fatalf("server block %t, client block %t; want %t, %t",
					got.Server != nil, got.Client != nil, tc.hasServer, tc.hasClient)
			}
			if got.Server != nil {
				if got.Server.RetryJoin != nil || got.RetryJoin != nil {
					t.Errorf("the file sets the retry_join that Nomad 2.1 removes: %q, %q",
						got.Server.RetryJoin, got.RetryJoin)
				}
				if diff := cmp.Diff(tc.server, got.Server.ServerJoin.RetryJoin); diff != "" {
					t.Errorf("server.server_join.retry_join differs (-want +got):\n%s", diff)
				}
			}
			if got.Client != nil {
				if diff := cmp.Diff(tc.client, got.Client.ServerJoin.RetryJoin); diff != "" {
					t.Errorf("client.server_join.retry_join differs (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestRenderJoinWithoutServers checks that a node with no servers to join, such as the first server of a new
// cluster, gets an empty list, which Nomad ignores.
func TestRenderJoinWithoutServers(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		got := decodeJoin(t, role, nil)
		join := got.Server
		if role == v1alpha1.RoleClient {
			join = nil
			if got.Client == nil || got.Client.ServerJoin == nil || len(got.Client.ServerJoin.RetryJoin) != 0 {
				t.Errorf("RenderJoin(%s, none): client = %+v, want an empty retry_join", role, got.Client)
			}
			continue
		}
		if join == nil || join.ServerJoin == nil || len(join.ServerJoin.RetryJoin) != 0 {
			t.Errorf("RenderJoin(%s, none): server = %+v, want an empty retry_join", role, join)
		}
	}
}

// decodeJoin renders 05-join.hcl and reads it back with HCL1, and fails the test on an error.
func decodeJoin(t *testing.T, role v1alpha1.Role, servers []netip.Addr) joinFile {
	t.Helper()
	f, err := nodeconfig.RenderJoin(role, servers)
	if err != nil {
		t.Fatalf("RenderJoin(%s): %v", role, err)
	}
	var got joinFile
	if err := hcl.Decode(&got, string(f.Content)); err != nil {
		t.Fatalf("HCL1 cannot read 05-join.hcl of %s: %v", role, err)
	}
	return got
}

func TestRenderJoinErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    v1alpha1.Role
		servers []netip.Addr
		want    string
	}{
		{"role", "worker", seeds, `05-join.hcl: role "worker" is not server, client or combined`},
		{"invalid address", v1alpha1.RoleClient, []netip.Addr{seeds[0], {}},
			"05-join.hcl: servers[1]: not an address"},
		{"zone with a line end", v1alpha1.RoleServer, []netip.Addr{netip.MustParseAddr("fe80::1").WithZone("a\nb")},
			"05-join.hcl: servers[0] has the control character U+000A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := nodeconfig.RenderJoin(tc.role, tc.servers)
			if got := errText(err); got != tc.want {
				t.Errorf("RenderJoin() error = %q, want %q", got, tc.want)
			}
			if f.Path != "" || f.Content != nil {
				t.Errorf("RenderJoin() = %v, want no file with the error", f)
			}
		})
	}
}
