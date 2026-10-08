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

const (
	refusalCause = "update deletes only nodes that never joined"
	rollAdvice   = "run tent rolling-update cluster to finish a rolling update that stopped, "
)

// TestRefuseJoinedDeletes fails a plan that deletes a machine with the joined label, for every reason a delete has,
// and names each such machine in one error. It accepts a plan whose deletes have never joined.
func TestRefuseJoinedDeletes(t *testing.T) {
	joined := func(id, name string) cloud.Instance { return cloud.Instance{ID: id, Name: name, Joined: true} }
	idle := func(id, name string) cloud.Instance { return cloud.Instance{ID: id, Name: name} }
	old, w1, w2, twin := joined("instance-7", "prod-old-0"), joined("instance-5", "prod-workers-1"),
		joined("instance-6", "prod-workers-2"), joined("instance-8", "prod-workers-0")
	free := idle("instance-9", "prod-workers-3")
	keeper, twin2 := joined("instance-3", "prod-workers-0"), joined("instance-10", "prod-workers-2")
	// Three machines of one name: the first listed is outside the spec, the second stays, the third is its duplicate.
	stray, keeper4, twin4 := joined("instance-11", "prod-workers-4"), joined("instance-12", "prod-workers-4"),
		joined("instance-13", "prod-workers-4")
	listed := []cloud.Instance{old, w1, w2, twin, free, keeper, twin2, stray, keeper4, twin4}
	const tail = "; " + refusalCause + ": "
	const orCluster = ", or delete the whole cluster with tent delete cluster"
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
		{
			name:    "one node that is not in the spec",
			changes: []NodeChange{deleteNode(old, reasonNotInSpec)},
			want: "update would delete a node that joined Nomad: prod-old-0 (ID instance-7, not in the spec)" +
				"; " + refusalCause + ": keep this node in the specs, or delete the whole cluster with tent delete cluster",
		},
		{
			name:    "a surplus node among nodes that never joined",
			changes: []NodeChange{deleteNode(free, reasonSurplus), deleteNode(w1, reasonSurplus)},
			want: "update would delete a node that joined Nomad: prod-workers-1 (ID instance-5, surplus)" +
				"; " + refusalCause + ": " + rollAdvice + "keep this node in the specs, or delete the whole cluster with tent " +
				"delete cluster",
		},
		{
			name:    "a duplicate",
			changes: []NodeChange{deleteNode(twin, reasonDuplicate)},
			want: "update would delete a node that joined Nomad: prod-workers-0 (ID instance-8, duplicate of ID " +
				"instance-3)" + tail + "remove one of the two machines called prod-workers-0 from Nomad and delete it " +
				"in the cloud" + orCluster,
		},
		{
			name:    "two duplicates",
			changes: []NodeChange{deleteNode(twin, reasonDuplicate), deleteNode(twin2, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-workers-0 (ID instance-8, duplicate of ID " +
				"instance-3) and prod-workers-2 (ID instance-10, duplicate of ID instance-6)" + tail +
				"of the machines that share a name, remove one from Nomad and delete it in the cloud" + orCluster,
		},
		{
			name:    "a duplicate and a surplus node",
			changes: []NodeChange{deleteNode(w1, reasonSurplus), deleteNode(twin, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-workers-1 (ID instance-5, surplus) and " +
				"prod-workers-0 (ID instance-8, duplicate of ID instance-3)" + tail + rollAdvice + "keep this node in the specs, " +
				"and remove one of the two machines called prod-workers-0 from Nomad and delete it in the cloud" +
				orCluster,
		},
		{
			name:    "a duplicate whose keeper is surplus",
			changes: []NodeChange{deleteNode(keeper, reasonSurplus), deleteNode(twin, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-workers-0 (ID instance-3, surplus) and " +
				"prod-workers-0 (ID instance-8, duplicate of ID instance-3)" + tail + rollAdvice + "keep this node in the specs, " +
				"and remove one of the two machines called prod-workers-0 from Nomad and delete it in the cloud" +
				orCluster,
		},
		{
			name:    "a duplicate listed after a machine of its name that is not in the spec",
			changes: []NodeChange{deleteNode(stray, reasonNotInSpec), deleteNode(twin4, reasonDuplicate)},
			want: "update would delete nodes that joined Nomad: prod-workers-4 (ID instance-11, not in the spec) and " +
				"prod-workers-4 (ID instance-13, duplicate of ID instance-12)" + tail + "keep this node in the specs, " +
				"and remove one of the two machines called prod-workers-4 from Nomad and delete it in the cloud" +
				orCluster,
		},
		{
			name: "a duplicate beside two surplus nodes",
			changes: []NodeChange{
				deleteNode(w1, reasonSurplus), deleteNode(w2, reasonSurplus), deleteNode(twin, reasonDuplicate),
			},
			want: "update would delete nodes that joined Nomad: prod-workers-1 (ID instance-5, surplus), " +
				"prod-workers-2 (ID instance-6, surplus) and prod-workers-0 (ID instance-8, duplicate of ID instance-3)" +
				tail + rollAdvice + "keep these nodes in the specs, and remove one of the two machines called " +
				"prod-workers-0 from Nomad and delete it in the cloud" + orCluster,
		},
		{
			name:    "two nodes",
			changes: []NodeChange{deleteNode(old, reasonNotInSpec), deleteNode(w1, reasonSurplus)},
			want: "update would delete nodes that joined Nomad: prod-old-0 (ID instance-7, not in the spec) and " +
				"prod-workers-1 (ID instance-5, surplus)" +
				"; " + refusalCause + ": " + rollAdvice + "keep these nodes in the specs, or delete the whole cluster with tent " +
				"delete cluster",
		},
		{
			name:    "two nodes that are not in the spec",
			changes: []NodeChange{deleteNode(old, reasonNotInSpec), deleteNode(stray, reasonNotInSpec)},
			want: "update would delete nodes that joined Nomad: prod-old-0 (ID instance-7, not in the spec) and " +
				"prod-workers-4 (ID instance-11, not in the spec)" + tail + "keep these nodes in the specs" + orCluster,
		},
		{
			name: "three nodes",
			changes: []NodeChange{
				deleteNode(old, reasonNotInSpec), deleteNode(w1, reasonSurplus), deleteNode(w2, reasonSurplus),
			},
			want: "update would delete nodes that joined Nomad: prod-old-0 (ID instance-7, not in the spec), " +
				"prod-workers-1 (ID instance-5, surplus) and prod-workers-2 (ID instance-6, surplus)" +
				"; " + refusalCause + ": " + rollAdvice + "keep these nodes in the specs, or delete the whole cluster with tent " +
				"delete cluster",
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
	changes := []NodeChange{{Action: NodeDelete, Name: "prod-workers-1", ID: "instance-5", Reason: reasonSurplus}}
	listed := []cloud.Instance{{ID: "instance-6", Name: "prod-workers-1", Joined: true}}

	if err := refuseJoinedDeletes(changes, listed); err != nil {
		t.Errorf("refuseJoinedDeletes = %v, want nil", err)
	}
}

// TestRefuseJoinedDeletesNamesTheStayerOfTheSameCluster names the machine of the duplicate's own cluster as the one
// that stays, also when a machine of another cluster with the name is listed first.
func TestRefuseJoinedDeletesNamesTheStayerOfTheSameCluster(t *testing.T) {
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
