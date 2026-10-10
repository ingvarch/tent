package app_test

import (
	"bytes"
	"encoding/base64"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
)

// nodeUserData returns the user data, base64 as a create request carries it, of the NodeConfig of the server
// prod-servers-0 with its 10-node.hcl rendered for the node called name with bootstrap_expect = expect, which 0 leaves
// out.
func (b liveWorld) nodeUserData(t *testing.T, name string, expect int) string {
	t.Helper()
	nc := configOf(t, b.f, "prod-servers-0")
	file, err := nodeconfig.RenderNode(name, "ams", nc.Role, expect)
	if err != nil {
		t.Fatalf("RenderNode: %v", err)
	}
	f := fileOf(nc, nodeHCL)
	if f == nil {
		t.Fatalf("the NodeConfig has no %s", nodeHCL)
	}
	*f, nc.Name = file, name
	data, err := nodeconfig.UserData(nc)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

// createNode creates a machine through the create path of the Vultr fake, with the create request of prod-servers-0
// but for its name, its role tag and its user data, and returns it. The machine is ready at once. An empty userData
// sends none.
func (b liveWorld) createNode(t *testing.T, name, role, userData string) worldServer {
	t.Helper()
	req, ok := b.f.CreateRequest(instanceNamed(t, b.f, "prod-servers-0"))
	if !ok {
		t.Fatal("the fake has no create request for prod-servers-0")
	}
	req.Hostname, req.Label, req.UserData = name, name, userData
	req.Tags = slices.DeleteFunc(slices.Clone(req.Tags), func(tag string) bool {
		return tagOf([]string{tag}, cloud.LabelRole) != ""
	})
	req.Tags = append(req.Tags, cloud.LabelRole+"="+role)
	b.f.SetBootReads(t, 0, 0) // ready at once, as the machines that tests add
	in, err := b.f.CreateInstance(t.Context(), &req)
	if err != nil {
		t.Fatalf("CreateInstance %s: %v", name, err)
	}
	return b.serverOf(t, in.ID)
}

// TestWorldServerCreatedToBootstrapAloneStaysOutsideTheCluster checks that a server machine whose create request
// carried bootstrap_expect = 1, and that is ready while the cluster has a leader, starts a cluster of its own: it is
// not in the Raft configuration or the report, its member reads alive, and the cluster is as it was.
func TestWorldServerCreatedToBootstrapAloneStaysOutsideTheCluster(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		leader := b.leader(t)
		added := b.createNode(t, "prod-servers-3", "server", b.nodeUserData(t, "prod-servers-3", 1))

		st := b.state(t, added)
		if st.peer != nil || st.entry != nil {
			t.Errorf("the server is in the Raft configuration %+v or the report %+v, want neither", st.peer, st.entry)
		}
		if st.member == nil || st.member.Status != "alive" {
			t.Errorf("the member of the server is %+v, want it alive", st.member)
		}
		if got := b.leader(t); got != leader || len(b.peers(t)) != 3 || st.health.Voters != 3 || !st.health.Healthy {
			t.Errorf("leader %v, %d peers, report %+v; want the leader %v, three peers and a healthy cluster of three",
				got, len(b.peers(t)), st.health, leader)
		}
	})
}

// TestWorldServerCreatedToJoinJoinsTheCluster checks that a server machine ready while the cluster has a leader joins
// it as a peer when its create request carried bootstrap_expect = 3, when it carried none, and when it carried no
// NodeConfig.
func TestWorldServerCreatedToJoinJoinsTheCluster(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		expect int  // bootstrap_expect of the NodeConfig; 0 leaves it out
		none   bool // no NodeConfig in the request
	}{
		{"bootstrap_expect 3", 3, false},
		{"no bootstrap_expect", 0, false},
		{"no NodeConfig", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorld(t)
				var data string
				if !tc.none {
					data = b.nodeUserData(t, "prod-servers-3", tc.expect)
				}
				added := b.createNode(t, "prod-servers-3", "server", data)

				st := b.state(t, added)
				if st.peer == nil || st.entry == nil || !st.peer.Voter || st.health.Voters != 4 {
					t.Errorf("peer %+v, entry %+v, %d voters; want the server in the Raft configuration, voting, "+
						"as the fourth", st.peer, st.entry, st.health.Voters)
				}
			})
		})
	}
}

// TestWorldCombinedNodeCreatedToBootstrapAloneNeverRegisters checks that a combined machine that starts a cluster of
// its own never registers its node, while one that joins the cluster does.
func TestWorldCombinedNodeCreatedToBootstrapAloneNeverRegisters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		expect     int
		registered bool
	}{
		{"alone", 1, false},
		{"joining", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := newLiveWorld(t)
				added := b.createNode(t, "prod-servers-3", "combined", b.nodeUserData(t, "prod-servers-3", tc.expect))

				if _, found := b.node(t, nodeIDOf(added.id)); found != tc.registered {
					t.Errorf("the node of the combined machine is listed: %v, want %v", found, tc.registered)
				}
			})
		})
	}
}

// TestWorldFirstServerOfAClusterOfOneLeads checks that the server of a cluster of one, created with bootstrap_expect
// = 1 before the cluster has a leader, is in the Raft configuration and leads.
func TestWorldFirstServerOfAClusterOfOneLeads(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, edit(t, combinedYAML, "size: 3", "size: 1"))
		mustUpdate(t, svc)
		api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
		if err != nil {
			t.Fatalf("Nomad: %v", err)
		}
		b := liveWorld{f: f, w: w, api: api}

		if got := b.leader(t); got.name != "prod-all-0" {
			t.Errorf("the leader is %v, want prod-all-0", got)
		}
		if peers := b.peers(t); len(peers) != 1 || !peers[0].Voter {
			t.Errorf("the Raft configuration is %+v, want one voter", peers)
		}
		got := fileOf(configOf(t, f, "prod-all-0"), nodeHCL)
		if !bytes.Contains(got.Content, []byte("bootstrap_expect = 1\n")) {
			t.Errorf("the first server does not bootstrap with 1 server:\n%s", got.Content)
		}
	})
}
