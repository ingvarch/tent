package app_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
)

// failureTB records what a test would fail with, so that a test can check that the world fails the test that it
// would, without failing its own.
type failureTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (f *failureTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}

// failures returns what the test would have failed with, in order.
func (f *failureTB) failures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.errs)
}

// atHalfSecond sleeps to the next half second, so that a time that the test notes has a part of a second to cut off.
func atHalfSecond() {
	time.Sleep(time.Second + 500*time.Millisecond - time.Duration(time.Now().Nanosecond()))
}

// serverNames are the servers of the test cluster, in the order they join.
var serverNames = []string{"prod-servers-0", "prod-servers-1", "prod-servers-2"}

// worldServer is a server machine of the world, as a test names it: a machine that is gone keeps its name and address.
type worldServer struct{ id, name, address string }

func (s worldServer) String() string { return s.name }

// raftID returns the Raft ID of the server.
func (s worldServer) raftID() string { return raftIDOf(s.id) }

// member returns the name of the server in the gossip pool.
func (s worldServer) member() string { return s.name + ".global" }

// serverState is what the Nomad of the world shows of one server at one moment.
type serverState struct {
	peer   *nomadops.Peer
	entry  *nomadops.ServerHealth
	member *nomadops.Member
	health nomadops.Health
}

// state reads the peers, the report and the members, and returns what they show of s.
func (b liveWorld) state(t *testing.T, s worldServer) serverState {
	t.Helper()
	health := b.health(t)
	members, err := b.api.Members(t.Context())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	st := serverState{health: health}
	for _, p := range b.peers(t) {
		if p.ID == s.raftID() {
			st.peer = &p
		}
	}
	for _, e := range health.Servers {
		if e.ID == s.raftID() {
			st.entry = &e
		}
	}
	for _, m := range members {
		if m.Name == s.member() {
			st.member = &m
		}
	}
	return st
}

// peers returns the Raft configuration.
func (b liveWorld) peers(t *testing.T) []nomadops.Peer {
	t.Helper()
	peers, err := b.api.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	return peers
}

// health returns autopilot's report.
func (b liveWorld) health(t *testing.T) nomadops.Health {
	t.Helper()
	health, err := b.api.Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return health
}

// serverOf returns the server machine with the instance ID id.
func (b liveWorld) serverOf(t *testing.T, id string) worldServer {
	t.Helper()
	for _, in := range b.f.Instances() {
		if in.ID == id {
			return worldServer{id: id, name: in.Hostname, address: in.MainIP}
		}
	}
	t.Fatalf("the fake has no instance %s", id)
	return worldServer{}
}

// leader returns the server that leads.
func (b liveWorld) leader(t *testing.T) worldServer {
	t.Helper()
	for _, p := range b.peers(t) {
		if p.Leader {
			return b.serverOf(t, strings.TrimPrefix(p.ID, "r-"))
		}
	}
	t.Fatal("no peer leads")
	return worldServer{}
}

// followers returns the servers of the test cluster that do not lead, in the order they joined.
func (b liveWorld) followers(t *testing.T) []worldServer {
	t.Helper()
	lead := b.leader(t)
	var out []worldServer
	for _, name := range serverNames {
		if s := b.serverOf(t, instanceNamed(t, b.f, name)); s != lead {
			out = append(out, s)
		}
	}
	return out
}

// serverMaker returns a function that adds a ready server machine, with the name it is given and a public address of
// its own, like the first server of the cluster: its tags, in its VPC. It adds machines also after that server is
// deleted.
func (b liveWorld) serverMaker(t *testing.T) func(name string) worldServer {
	t.Helper()
	for _, in := range b.f.Instances() {
		if tagOf(in.Tags, cloud.LabelRole) != "server" {
			continue
		}
		vpcs := b.f.InstanceVPCs(in.ID)
		if len(vpcs) == 0 {
			t.Fatalf("the server %s has no VPC", in.ID)
		}
		return func(name string) worldServer {
			in.ID, in.Hostname, in.Label = "", name, name
			in.MainIP = fmt.Sprintf("198.19.0.%d", len(b.f.Instances())+1)
			return b.serverOf(t, b.f.AddInstance(t, in, vpcs[0].ID).ID)
		}
	}
	t.Fatal("the fake has no server machine")
	return nil
}

// addServer adds a ready server machine called name, like the first server of the cluster.
func (b liveWorld) addServer(t *testing.T, name string) worldServer {
	t.Helper()
	return b.serverMaker(t)(name)
}

// haltInstance halts the machine of s at the Vultr fake.
func (b liveWorld) haltInstance(t *testing.T, s worldServer) {
	t.Helper()
	if err := b.f.HaltInstance(t.Context(), s.id); err != nil {
		t.Fatalf("HaltInstance: %v", err)
	}
}

// deleteInstance deletes the machine of s at the Vultr fake.
func (b liveWorld) deleteInstance(t *testing.T, s worldServer) {
	t.Helper()
	if err := b.f.DeleteInstance(t.Context(), s.id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}
