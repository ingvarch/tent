package app_test

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/secret"
)

// liveWorld is the test cluster after an update, with a client of its Nomad.
type liveWorld struct {
	f   *vultrfake.Fake
	w   *nomadWorld
	api nomadops.API
}

// newLiveWorld updates the test cluster in the bubble of t, and returns its fakes and a Nomad client.
func newLiveWorld(t *testing.T) liveWorld { return newLiveWorldWith(t, nil) }

// newLiveWorldWith is newLiveWorld with the world set by setup, when it is not nil, before the update.
func newLiveWorldWith(t *testing.T, setup func(*nomadWorld)) liveWorld {
	t.Helper()
	svc, f, w := newRelease(t)
	if setup != nil {
		setup(w)
	}
	mustUpdate(t, svc)
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	return liveWorld{f: f, w: w, api: api}
}

// nodes returns the nodes that the Nomad of the world lists.
func (b liveWorld) nodes(t *testing.T) []nomadops.Node {
	t.Helper()
	nodes, err := b.api.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	return nodes
}

// node returns the listed node with the ID, and whether the Nomad lists it.
func (b liveWorld) node(t *testing.T, id string) (nomadops.Node, bool) {
	t.Helper()
	for _, n := range b.nodes(t) {
		if n.ID == id {
			return n, true
		}
	}
	return nomadops.Node{}, false
}

// workerIDs returns the node IDs of the clients that the Nomad lists, in its order.
func (b liveWorld) workerIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, n := range b.nodes(t) {
		ids = append(ids, n.ID)
	}
	return ids
}

// instanceOf returns the ID of the instance whose node has the ID id.
func instanceOf(id string) string { return strings.TrimPrefix(id, "n-") }

// deleteMachineOf deletes the instance of the node id at the Vultr fake.
func (b liveWorld) deleteMachineOf(t *testing.T, id string) {
	t.Helper()
	if err := b.f.DeleteInstance(t.Context(), instanceOf(id)); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

// wantStatus fails the test unless the node id is listed with the status.
func (b liveWorld) wantStatus(t *testing.T, id, status string) nomadops.Node {
	t.Helper()
	n, ok := b.node(t, id)
	if !ok || n.Status != status {
		t.Fatalf("node %s = %+v (listed %v), want status %q", id, n, ok, status)
	}
	return n
}

// TestWorldRegistersAReadyClientOnceWithItsID checks that each ready client has the ID n-<instance id>, and that a
// mark, a drain and a purge through a client of the world stay at the next call.
func TestWorldRegistersAReadyClientOnceWithItsID(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		ids := b.workerIDs(t)
		if len(ids) != 2 {
			t.Fatalf("the Nomad lists %v, want two workers", ids)
		}
		for _, in := range b.f.Instances() {
			if tagOf(in.Tags, cloud.LabelRole) != "client" {
				continue
			}
			if !slices.Contains(ids, "n-"+in.ID) {
				t.Errorf("the Nomad lists %v, want the node n-%s of %s", ids, in.ID, in.Hostname)
			}
		}
		marked, drained := ids[0], ids[1]
		b.w.SetDrainReads(5)

		if err := b.api.MarkIneligible(t.Context(), marked); err != nil {
			t.Fatalf("MarkIneligible: %v", err)
		}
		req := nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": "m-1"}}
		if err := b.api.Drain(t.Context(), drained, req); err != nil {
			t.Fatalf("Drain: %v", err)
		}

		for range 2 {
			if n, _ := b.node(t, marked); n.Eligible || n.Status != "ready" {
				t.Errorf("the marked node = %+v, want ready and ineligible", n)
			}
			n, _ := b.node(t, drained)
			if want := map[string]string{"tent_machine": "m-1"}; !n.Draining || n.Eligible ||
				cmp.Diff(want, n.LastDrain.Meta) != "" {
				t.Errorf("the drained node = %+v, want draining, ineligible, with the meta of the drain", n)
			}
		}

		if err := b.api.Purge(t.Context(), marked); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		for range 2 {
			if _, listed := b.node(t, marked); listed {
				t.Errorf("the purged node of a live machine is listed again")
			}
		}
	})
}

// TestWorldDownsTheNodeOfAGoneMachineAfterTheDelay checks that the node of a deleted or halted machine reads ready
// until 20 s after the world saw the machine gone, and down after, with its drain complete and its ID and meta kept;
// and that a mark of a down node stays.
func TestWorldDownsTheNodeOfAGoneMachineAfterTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		ids := b.workerIDs(t)
		b.w.SetDrainReads(100)
		req := nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": "m-1"}}
		if err := b.api.Drain(t.Context(), ids[0], req); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		b.deleteMachineOf(t, ids[0])
		if err := b.f.HaltInstance(t.Context(), instanceOf(ids[1])); err != nil {
			t.Fatalf("HaltInstance: %v", err)
		}

		b.wantStatus(t, ids[0], "ready")
		time.Sleep(19 * time.Second)
		b.wantStatus(t, ids[0], "ready")
		time.Sleep(2 * time.Second)
		gone := b.wantStatus(t, ids[0], "down")
		if gone.Draining || gone.Eligible || gone.LastDrain.Status != "complete" ||
			cmp.Diff(req.Meta, gone.LastDrain.Meta) != "" {
			t.Errorf("the down node = %+v, want its drain complete, with the meta, and ineligible", gone)
		}
		if halted := b.wantStatus(t, ids[1], "down"); !halted.Eligible || halted.Draining {
			t.Errorf("the down node of a halted machine = %+v, want it eligible, as Nomad keeps it", halted)
		}
		if err := b.api.MarkIneligible(t.Context(), ids[1]); err != nil {
			t.Fatalf("MarkIneligible: %v", err)
		}
		if n := b.wantStatus(t, ids[1], "down"); n.Eligible {
			t.Errorf("the mark of the down node was undone: %+v", n)
		}
	})
}

// TestWorldSetDownAfterMovesTheDelay checks that SetDownAfter sets how long a gone machine's node reads ready.
func TestWorldSetDownAfterMovesTheDelay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		id := b.workerIDs(t)[0]
		b.w.SetDownAfter(5 * time.Minute)
		b.deleteMachineOf(t, id)

		b.wantStatus(t, id, "ready")
		time.Sleep(time.Minute)
		b.wantStatus(t, id, "ready")
		time.Sleep(4*time.Minute + time.Second)
		b.wantStatus(t, id, "down")
	})
}

// TestWorldKeepsAPurgedNodeGone checks that a purged node does not come back when its machine goes: not when the node
// was down at the purge, and not when its machine was live and nothing read the nodes since.
func TestWorldKeepsAPurgedNodeGone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		ids := b.workerIDs(t)

		b.deleteMachineOf(t, ids[0])
		b.wantStatus(t, ids[0], "ready")
		time.Sleep(21 * time.Second)
		b.wantStatus(t, ids[0], "down")
		if err := b.api.Purge(t.Context(), ids[0]); err != nil {
			t.Fatalf("Purge: %v", err)
		}

		// No read of the nodes between the purge and the end of the delay: only the purge itself tells the world.
		if err := b.api.Purge(t.Context(), ids[1]); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		b.deleteMachineOf(t, ids[1])
		if _, err := b.api.Peers(t.Context()); err != nil {
			t.Fatalf("Peers: %v", err)
		}
		time.Sleep(time.Minute)
		if left := b.workerIDs(t); len(left) != 0 {
			t.Errorf("the Nomad lists %v after both purges, want no node", left)
		}
	})
}

// TestWorldNewClusterRegistersTheNodesAgain checks that the nodes of the live machines register once more, with the
// same IDs, in a cluster that NewCluster made.
func TestWorldNewClusterRegistersTheNodesAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		ids := b.workerIDs(t)
		if err := b.api.Purge(t.Context(), ids[0]); err != nil {
			t.Fatalf("Purge: %v", err)
		}

		b.w.NewCluster()
		b.w.SetBootstrapped(secret.Secret("test-token"))

		if diff := cmp.Diff(ids, b.workerIDs(t)); diff != "" {
			t.Errorf("the nodes of the new cluster (-want +got):\n%s", diff)
		}
	})
}

// TestWorldGivesServersRaftIDsAndMembers checks that the peers and the autopilot report share one Raft ID per server,
// r-<instance id>, that one peer leads, that a server's stable time is the time the world first saw it ready, and that
// the members are the ready servers, alive, at their private addresses.
func TestWorldGivesServersRaftIDsAndMembers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorld(t)
		peers, err := b.api.Peers(t.Context())
		if err != nil {
			t.Fatalf("Peers: %v", err)
		}
		health, err := b.api.Health(t.Context())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		time.Sleep(time.Minute)
		later, err := b.api.Health(t.Context())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		members, err := b.api.Members(t.Context())
		if err != nil {
			t.Fatalf("Members: %v", err)
		}

		servers := map[string]vultrServer{}
		for _, in := range b.f.Instances() {
			if tagOf(in.Tags, cloud.LabelRole) == "server" {
				servers["r-"+in.ID] = vultrServer{name: in.Hostname + ".global", private: b.f.InstanceVPCs(in.ID)[0].IPAddress}
			}
		}
		if len(peers) != 3 || len(health.Servers) != 3 || len(members) != 3 {
			t.Fatalf("peers %d, report %d, members %d, want 3 each", len(peers), len(health.Servers), len(members))
		}
		leaders := 0
		for _, p := range peers {
			want, ok := servers[p.ID]
			if !ok || p.Name != want.name {
				t.Errorf("peer %+v, want an ID and a name from %v", p, servers)
			}
			if p.Leader {
				leaders++
			}
		}
		if leaders != 1 {
			t.Errorf("%d peers lead, want 1", leaders)
		}
		for i, s := range health.Servers {
			if s.Name != servers[s.ID].name || s.StableSince.IsZero() || s.Leader != leaderOf(peers, s.ID) {
				t.Errorf("report entry %+v, want the name and leader of its peer and a stable time", s)
			}
			if !s.StableSince.Equal(later.Servers[i].StableSince) {
				t.Errorf("the stable time of %s moved from %v to %v", s.ID, s.StableSince, later.Servers[i].StableSince)
			}
		}
		privateOf := map[string]string{}
		for _, v := range servers {
			privateOf[v.name] = v.private
		}
		for _, m := range members {
			if want, ok := privateOf[m.Name]; !ok || m.Status != "alive" || m.Address.String() != want {
				t.Errorf("member %+v, want it alive at the private address in %v", m, privateOf)
			}
		}
	})
}

// vultrServer is what a test knows of a server machine.
type vultrServer struct{ name, private string }

// leaderOf reports whether the peer with the Raft ID leads.
func leaderOf(peers []nomadops.Peer, id string) bool {
	for _, p := range peers {
		if p.ID == id {
			return p.Leader
		}
	}
	return false
}
