package app

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
)

// TestRefuseJoinedDeletes fails a plan that deletes a machine with the joined label and neither drains its node nor
// makes its server leave: a machine of a group that the specs lack whose role is not client, a machine of a group that
// the specs lack with no group label, and machines of one name that joined. It names each such machine in one error,
// with the advice for each cause. It accepts a plan whose deletes have never joined or are removals.
func TestRefuseJoinedDeletes(t *testing.T) {
	t.Parallel()
	joined := func(id, name string, role v1alpha1.Role, group string) cloud.Instance {
		return cloud.Instance{ID: id, Name: name, Role: role, Group: group, Joined: true}
	}
	idle := func(id, name string) cloud.Instance { return cloud.Instance{ID: id, Name: name} }
	db0 := joined("instance-1", "prod-db-0", v1alpha1.RoleServer, "db")
	db1 := joined("instance-2", "prod-db-1", v1alpha1.RoleServer, "db")
	cache := joined("instance-14", "prod-cache-0", v1alpha1.RoleCombined, "cache")
	noGroup := joined("instance-15", "prod-lost-0", v1alpha1.RoleClient, "")
	noGroup2 := joined("instance-16", "prod-lost-1", "", "")
	unknown := joined("instance-17", "prod-odd-0", "", "odd")
	w1 := joined("instance-5", "prod-workers-1", v1alpha1.RoleClient, "workers")
	free := idle("instance-9", "prod-workers-3")
	keeper, twin := joined("instance-3", "prod-workers-0", v1alpha1.RoleClient, "workers"),
		joined("instance-8", "prod-workers-0", v1alpha1.RoleClient, "workers")
	keeper2, twin2 := joined("instance-4", "prod-workers-2", v1alpha1.RoleClient, "workers"),
		joined("instance-10", "prod-workers-2", v1alpha1.RoleClient, "workers")
	// Two clients of a group that the specs lack, with one name.
	old0, old0b := joined("instance-7", "prod-old-0", v1alpha1.RoleClient, "old"),
		joined("instance-11", "prod-old-0", v1alpha1.RoleClient, "old")
	listed := []cloud.Instance{
		db0, db1, cache, noGroup, noGroup2, unknown, w1, free, keeper, twin, keeper2, twin2, old0, old0b,
	}
	const orCluster = ", or delete the whole cluster with tent delete cluster"
	const wholeGroup = "tent removes a whole node group only when its nodes are clients: "
	const twins = "tent does not choose between machines of one name that joined: "
	removal := func(in cloud.Instance, reason string) NodeChange {
		c := deleteNode(in, reason)
		c.Drain = "1h0m0s"
		return c
	}
	for _, tc := range []struct {
		name    string
		changes []NodeChange
		want    string
	}{
		{name: "no changes"},
		{name: "a delete of a node that never joined", changes: []NodeChange{deleteNode(free, reasonSurplus)}},
		{
			name: "creates and waits are no deletes",
			changes: []NodeChange{
				{Action: NodeCreate, Name: "prod-workers-4"}, {Action: NodeWait, Name: "prod-workers-3", ID: "instance-9"},
			},
		},
		{name: "a removal with a drain", changes: []NodeChange{removal(w1, reasonSurplus)}},
		{
			name: "a removal with the leaving of a server",
			changes: []NodeChange{
				func() NodeChange { c := deleteNode(db0, reasonSurplus); c.Server = true; return c }(),
			},
		},
		{
			name:    "one server of a group that the specs lack",
			changes: []NodeChange{deleteNode(db0, reasonNotInSpec)},
			want: "update would delete a node that joined Nomad: prod-db-0 (ID instance-1, not in the spec); " + wholeGroup +
				"keep node group db in the specs" + orCluster,
		},
		{
			name:    "two servers of one group that the specs lack",
			changes: []NodeChange{deleteNode(db0, reasonNotInSpec), deleteNode(db1, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-db-0 (ID instance-1, not in the spec) and " +
				"prod-db-1 (ID instance-2, not in the spec); " + wholeGroup + "keep node group db in the specs" + orCluster,
		},
		{
			name:    "nodes of two groups that the specs lack",
			changes: []NodeChange{deleteNode(db0, reasonNotInSpec), deleteNode(cache, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-db-0 (ID instance-1, not in the spec) and " +
				"prod-cache-0 (ID instance-14, not in the spec); " + wholeGroup + "keep node groups cache and db in the specs" +
				orCluster,
		},
		{
			name:    "a node with no group label",
			changes: []NodeChange{deleteNode(noGroup, reasonNotInSpec)},
			want: "update would delete a node that joined Nomad: prod-lost-0 (ID instance-15, not in the spec); " + wholeGroup +
				"keep it in a node group of the specs" + orCluster,
		},
		{
			name:    "nodes with no group label",
			changes: []NodeChange{deleteNode(noGroup, reasonNotInSpec), deleteNode(noGroup2, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-lost-0 (ID instance-15, not in the spec) and " +
				"prod-lost-1 (ID instance-16, not in the spec); " + wholeGroup + "keep them in a node group of the specs" +
				orCluster,
		},
		{
			name:    "a node with no group label beside a node of a group that the specs lack",
			changes: []NodeChange{deleteNode(db0, reasonNotInSpec), deleteNode(noGroup, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-db-0 (ID instance-1, not in the spec) and " +
				"prod-lost-0 (ID instance-15, not in the spec); " + wholeGroup +
				"keep node group db in the specs, and keep it in a node group of the specs" + orCluster,
		},
		{
			name:    "a node of a group that the specs lack with no role",
			changes: []NodeChange{deleteNode(unknown, reasonNotInSpec)},
			want: "update would delete a node that joined Nomad: prod-odd-0 (ID instance-17, not in the spec); " + wholeGroup +
				"keep node group odd in the specs" + orCluster,
		},
		{
			name:    "a duplicate",
			changes: []NodeChange{deleteNode(twin, reasonDuplicate)},
			want: "update would delete a node that joined Nomad: prod-workers-0 (ID instance-8, duplicate of ID " +
				"instance-3); " + twins + "remove one of the two machines called prod-workers-0 from Nomad and delete it " +
				"in the cloud" + orCluster,
		},
		{
			name:    "two duplicates",
			changes: []NodeChange{deleteNode(twin, reasonDuplicate), deleteNode(twin2, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-workers-0 (ID instance-8, duplicate of ID " +
				"instance-3) and prod-workers-2 (ID instance-10, duplicate of ID instance-4); " + twins +
				"of the machines that share a name, remove one from Nomad and delete it in the cloud" + orCluster,
		},
		{
			name:    "a duplicate whose keeper is removed",
			changes: []NodeChange{removal(keeper, reasonSurplus), deleteNode(twin, reasonDuplicate)},
			want: "update would delete a node that joined Nomad: prod-workers-0 (ID instance-8, duplicate of ID " +
				"instance-3); " + twins + "remove one of the two machines called prod-workers-0 from Nomad and delete it " +
				"in the cloud" + orCluster,
		},
		{
			name:    "two clients of a group that the specs lack with one name",
			changes: []NodeChange{deleteNode(old0, reasonNotInSpec), deleteNode(old0b, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-old-0 (ID instance-7, not in the spec) and " +
				"prod-old-0 (ID instance-11, not in the spec); " + twins +
				"remove one of the two machines called prod-old-0 from Nomad and delete it in the cloud" + orCluster,
		},
		{
			name: "twins of a group of the specs and of a group that the specs lack",
			changes: []NodeChange{
				deleteNode(old0, reasonNotInSpec), deleteNode(old0b, reasonNotInSpec), deleteNode(twin, reasonDuplicate),
			},
			want: "update would delete nodes that joined Nomad: prod-old-0 (ID instance-7, not in the spec), " +
				"prod-old-0 (ID instance-11, not in the spec) and prod-workers-0 (ID instance-8, duplicate of ID instance-3); " +
				twins + "of the machines that share a name, remove one from Nomad and delete it in the cloud" + orCluster,
		},
		{
			name:    "a server of a group that the specs lack and a duplicate",
			changes: []NodeChange{deleteNode(db0, reasonNotInSpec), deleteNode(twin, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-db-0 (ID instance-1, not in the spec) and " +
				"prod-workers-0 (ID instance-8, duplicate of ID instance-3); " + wholeGroup + "keep node group db in the specs; " +
				twins + "remove one of the two machines called prod-workers-0 from Nomad and delete it in the cloud" +
				orCluster,
		},
		{
			name:    "a removal beside a node that stays refused",
			changes: []NodeChange{removal(w1, reasonSurplus), deleteNode(db0, reasonNotInSpec)},
			want: "update would delete a node that joined Nomad: prod-db-0 (ID instance-1, not in the spec); " + wholeGroup +
				"keep node group db in the specs" + orCluster,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseJoinedDeletes(tc.changes, listed)

			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refuseJoinedDeletes = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("refuseJoinedDeletes = %v\nwant %s", err, tc.want)
			}
		})
	}
}

// TestRefuseJoinedDeletesIgnoresAMachineThatTheCloudDoesNotList accepts a delete whose ID no listed machine has.
func TestRefuseJoinedDeletesIgnoresAMachineThatTheCloudDoesNotList(t *testing.T) {
	t.Parallel()
	changes := []NodeChange{{Action: NodeDelete, Name: "prod-workers-1", ID: "instance-5", Reason: reasonSurplus}}
	listed := []cloud.Instance{{ID: "instance-6", Name: "prod-workers-1", Joined: true}}

	if err := refuseJoinedDeletes(changes, listed); err != nil {
		t.Errorf("refuseJoinedDeletes = %v, want nil", err)
	}
}

// TestRefuseJoinedDeletesNamesTheStayerOfTheSameCluster names the machine of the duplicate's own cluster as the one
// that stays, also when a machine of another cluster with the name is listed first.
func TestRefuseJoinedDeletesNamesTheStayerOfTheSameCluster(t *testing.T) {
	t.Parallel()
	other := cloud.Instance{ID: "instance-1", Cluster: "other", Name: "prod-workers-0", Joined: true}
	keeper := cloud.Instance{ID: "instance-3", Cluster: "prod", Name: "prod-workers-0", Joined: true}
	twin := cloud.Instance{ID: "instance-8", Cluster: "prod", Name: "prod-workers-0", Joined: true}

	err := refuseJoinedDeletes([]NodeChange{deleteNode(twin, reasonDuplicate)}, []cloud.Instance{other, keeper, twin})

	if want := "(ID instance-8, duplicate of ID instance-3)"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("refuseJoinedDeletes = %v, want an error with %q", err, want)
	}
}

// nomadStub is a Nomad API that answers Nodes and Peers as set and counts the calls.
type nomadStub struct {
	nomadops.API
	nodes                 []nomadops.Node
	peers                 []nomadops.Peer
	nodesErr, peersErr    error
	nodesCalls, peerCalls int
}

func (s *nomadStub) Nodes(context.Context) ([]nomadops.Node, error) {
	s.nodesCalls++
	return s.nodes, s.nodesErr
}

func (s *nomadStub) Peers(context.Context) ([]nomadops.Peer, error) {
	s.peerCalls++
	return s.peers, s.peersErr
}

// TestHasJoined tells a machine that joined Nomad from one that did not, by the node's name and address and by the
// peer's address, and passes on the error of either call.
func TestHasJoined(t *testing.T) {
	t.Parallel()
	addr := netip.MustParseAddr("10.64.0.9")
	machine := cloud.Instance{ID: "instance-9", Name: "prod-workers-1", PrivateIP: addr}
	as := func(role v1alpha1.Role) cloud.Instance {
		in := machine
		in.Role = role
		return in
	}
	node := func(name, a, status string) nomadops.Node {
		return nomadops.Node{Name: name, Address: netip.MustParseAddr(a), Status: status, Eligible: true}
	}
	peer := func(a string, voter bool) nomadops.Peer {
		return nomadops.Peer{Name: "x.global", Address: netip.MustParseAddrPort(a + ":4647"), Voter: voter}
	}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		in        cloud.Instance
		stub      nomadStub
		want      string
		wantErr   error
		wantCalls [2]int // Nodes, Peers
	}{
		{
			name: "a registered client, whatever the machine's role label", in: as(v1alpha1.RoleServer),
			stub: nomadStub{nodes: []nomadops.Node{node("prod-workers-1", "10.64.0.9", "ready")}},
			want: "a registered client at 10.64.0.9", wantCalls: [2]int{1, 0},
		},
		{
			name: "a client that is initializing", in: machine,
			stub: nomadStub{nodes: []nomadops.Node{node("prod-workers-1", "10.64.0.9", "initializing")}},
			want: "a registered client at 10.64.0.9", wantCalls: [2]int{1, 0},
		},
		{
			name: "a client that is down", in: machine,
			stub:      nomadStub{nodes: []nomadops.Node{node("prod-workers-1", "10.64.0.9", "down")}},
			wantCalls: [2]int{1, 1},
		},
		{
			name: "a node of the name at another address", in: machine,
			stub:      nomadStub{nodes: []nomadops.Node{node("prod-workers-1", "10.64.0.8", "ready")}},
			wantCalls: [2]int{1, 1},
		},
		{
			name: "a node at the address under another name", in: machine,
			stub:      nomadStub{nodes: []nomadops.Node{node("prod-workers-2", "10.64.0.9", "ready")}},
			wantCalls: [2]int{1, 1},
		},
		{
			name: "a voting server, whatever the machine's role label", in: as(v1alpha1.RoleClient),
			stub: nomadStub{peers: []nomadops.Peer{peer("10.64.0.8", true), peer("10.64.0.9", true)}},
			want: "a server at 10.64.0.9:4647", wantCalls: [2]int{1, 1},
		},
		{
			name: "a server that does not vote", in: machine,
			stub: nomadStub{peers: []nomadops.Peer{peer("10.64.0.9", false)}},
			want: "a server at 10.64.0.9:4647", wantCalls: [2]int{1, 1},
		},
		{
			name: "nothing", in: machine, stub: nomadStub{peers: []nomadops.Peer{peer("10.64.0.8", true)}},
			wantCalls: [2]int{1, 1},
		},
		{
			name: "no private address", in: cloud.Instance{ID: "instance-9", Name: "prod-workers-1"},
			stub: nomadStub{nodes: []nomadops.Node{node("prod-workers-1", "10.64.0.9", "ready")}},
		},
		{name: "Nodes fails", in: machine, stub: nomadStub{nodesErr: boom}, wantErr: boom, wantCalls: [2]int{1, 0}},
		{name: "Peers fails", in: machine, stub: nomadStub{peersErr: boom}, wantErr: boom, wantCalls: [2]int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &applier{api: &tc.stub}

			got, err := a.hasJoined(t.Context(), tc.in)

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("hasJoined error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("hasJoined = %q, want %q", got, tc.want)
			}
			if calls := [2]int{tc.stub.nodesCalls, tc.stub.peerCalls}; calls != tc.wantCalls {
				t.Errorf("hasJoined made [Nodes Peers] = %v calls, want %v", calls, tc.wantCalls)
			}
		})
	}
}
