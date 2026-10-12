package app

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// removalVersion is the Nomad version that the test clusters are pinned to.
const removalVersion = "2.0.7"

// removalNow is the time of the plans of these tests.
var removalNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// removalModel returns the test cluster prod with the groups.
func removalModel(groups ...model.NodeGroup) *model.Cluster {
	return &model.Cluster{Name: "prod", Groups: groups}
}

// specGroup returns the group g as the decisions see a group of the specs, with the drain timeout drain.
func specGroup(g model.NodeGroup, drain time.Duration) rollout.Group {
	return rollout.Group{
		Name: g.Name, Role: g.Role, Size: g.Size, Zones: g.Zones, SpecHash: "hash-" + g.Name, DrainTimeout: drain,
	}
}

// removedGroup returns a group that the specs lack as the decisions see it: a client group of size 0 with the default
// drain.
func removedGroup(name string) rollout.Group {
	return rollout.Group{Name: name, Role: v1alpha1.RoleClient, DrainTimeout: time.Hour}
}

// withPublicAddress returns in with a public address, so that a roll can call its Nomad API.
func withPublicAddress(in cloud.Instance) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.PublicIP = netip.MustParseAddr("192.0.2.1") })
}

// readyNode returns the node that Nomad lists for the client machine in: ready and eligible.
func readyNode(in cloud.Instance) nomadops.Node {
	return nomadops.Node{ID: "n-" + in.ID, Name: in.Name, Status: "ready", Eligible: true, Address: in.PrivateIP}
}

// downNode returns a node of the name that Nomad lists as down at the private address of the test instance id.
func downNode(name, id string) nomadops.Node {
	return nomadops.Node{ID: "n-" + id, Name: name, Status: "down", Address: addressFor(id)}
}

// healthyServers returns what Nomad reports of the servers ms when all are voters and the first leads; unhealthy names
// the machines that autopilot does not count healthy.
func healthyServers(ms []cloud.Instance, unhealthy ...string) nomadReading {
	var r nomadReading
	r.health.Healthy = len(unhealthy) == 0
	r.health.FailureTolerance = (len(ms) - 1) / 2
	r.health.Voters = len(ms)
	for i, in := range ms {
		name := in.Name + ".global"
		r.peers = append(r.peers, nomadops.Peer{
			ID: "r-" + in.ID, Name: name, Address: netip.AddrPortFrom(in.PrivateIP, raftPort), Voter: true, Leader: i == 0,
		})
		r.health.Servers = append(r.health.Servers, nomadops.ServerHealth{
			ID: "r-" + in.ID, Name: name, Healthy: !slices.Contains(unhealthy, in.Name), Voter: true, Leader: i == 0,
			Version: removalVersion, StableSince: removalNow.Add(-time.Hour),
		})
		r.members = append(r.members, nomadops.Member{Name: name, Address: in.PrivateIP, Status: "alive"})
	}
	return r
}

// TestRemovesJoined is true for a group above its size with a machine that joined and for a client that joined in a
// group the specs lack, and false for every other cluster.
func TestRemovesJoined(t *testing.T) {
	t.Parallel()
	w1, s1 := workers(1), servers(1)
	old, db := nodeGroup("old", v1alpha1.RoleClient, 1), nodeGroup("db", v1alpha1.RoleServer, 1)
	m := removalModel(w1, s1)
	for _, tc := range []struct {
		name   string
		listed []cloud.Instance
		want   bool
	}{
		{"no machine", nil, false},
		{"groups at their size", []cloud.Instance{member(w1, 0, "i-1", minutes(1)), member(s1, 0, "i-2", minutes(1))}, false},
		{"a client group above its size with machines that joined",
			[]cloud.Instance{member(w1, 0, "i-1", minutes(1)), member(w1, 1, "i-2", minutes(2))}, true},
		{"a server group above its size with a machine that joined",
			[]cloud.Instance{member(s1, 0, "i-1", minutes(1)), unjoined(member(s1, 1, "i-2", minutes(2)))}, true},
		{"a group above its size with no machine that joined",
			[]cloud.Instance{unjoined(member(w1, 0, "i-1", minutes(1))), unjoined(member(w1, 1, "i-2", minutes(2)))}, false},
		{"a client that joined in a group the specs lack", []cloud.Instance{member(old, 0, "i-1", minutes(1))}, true},
		{"a client that never joined in a group the specs lack",
			[]cloud.Instance{unjoined(member(old, 0, "i-1", minutes(1)))}, false},
		{"a server that joined in a group the specs lack", []cloud.Instance{member(db, 0, "i-1", minutes(1))}, false},
		{"a client that joined and has no group label",
			[]cloud.Instance{changed(member(old, 0, "i-1", minutes(1)), func(in *cloud.Instance) { in.Group = "" })}, false},
		{"machines of another cluster",
			[]cloud.Instance{
				changed(member(w1, 0, "i-1", minutes(1)), func(in *cloud.Instance) { in.Cluster = "dev" }),
				changed(member(w1, 1, "i-2", minutes(2)), func(in *cloud.Instance) { in.Cluster = "dev" }),
				changed(member(old, 0, "i-3", minutes(1)), func(in *cloud.Instance) { in.Cluster = "dev" }),
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := removesJoined(m, tc.listed); got != tc.want {
				t.Errorf("removesJoined = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadForRemoval reads the Raft configuration, autopilot's report, the members and the nodes, in that order,
// through the API over the servers that joined and run, and returns the reading.
func TestReadForRemoval(t *testing.T) {
	t.Parallel()
	s3, w1 := servers(3), workers(1)
	machines := []cloud.Instance{
		withPublicAddress(member(s3, 1, "i-2", minutes(1))), withPublicAddress(member(s3, 0, "i-1", minutes(1))),
		withPublicAddress(unjoined(member(s3, 2, "i-3", minutes(1)))), member(s3, 2, "i-4", minutes(1)),
		withPublicAddress(changed(member(s3, 2, "i-5", minutes(1)), func(in *cloud.Instance) { in.Ready = false })),
		withPublicAddress(member(w1, 0, "i-6", minutes(1))),
	}
	stub := &readStub{}
	var asked []string
	got, err := readForRemoval(t.Context(), removalModel(s3, w1), machines, nil,
		func(servers []cloud.Instance) (nomadops.API, error) {
			for _, in := range servers {
				asked = append(asked, in.ID)
			}
			return stub, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"i-1", "i-2"}, asked); diff != "" {
		t.Errorf("the servers that the API is made over (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"Peers", "Health", "Members", "Nodes"}, stub.calls); diff != "" {
		t.Errorf("the reads (-want +got):\n%s", diff)
	}
	want := nomadReading{
		peers:   []nomadops.Peer{{ID: "r-1"}},
		health:  nomadops.Health{FailureTolerance: 1},
		members: []nomadops.Member{{Name: "m.global"}},
		nodes:   []nomadops.Node{{ID: "n-1"}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(nomadReading{}), equateNetip); diff != "" {
		t.Errorf("the reading (-want +got):\n%s", diff)
	}
}

// TestReadForRemovalWithoutAServerToAsk fails when no machine of a server or combined group joined, runs, and has a
// public address, and names the nodes that joined among the removals of the plan, and no other change.
func TestReadForRemovalWithoutAServerToAsk(t *testing.T) {
	t.Parallel()
	s3, w1 := servers(3), workers(1)
	w1b := member(w1, 1, "i-5", minutes(2))
	other := []NodeChange{wantCreate(s3, 0, "ams"), wantDelete(unjoined(member(w1, 3, "i-7", minutes(4))), "surplus")}
	one := slices.Concat(other, []NodeChange{wantRemoval(w1b, "surplus", "1h0m0s", false)})
	two := append(slices.Clone(one), wantRemoval(member(w1, 2, "i-6", minutes(3)), "surplus", "1h0m0s", false))
	for _, tc := range []struct {
		name   string
		listed []cloud.Instance
		plan   []NodeChange
		want   string
	}{
		{
			"no server machine at all", []cloud.Instance{w1b}, one,
			"update would remove a node that joined Nomad: prod-workers-1 (ID i-5, surplus); cluster prod has no " +
				"server that joined and runs for tent to ask: keep this node in the specs until the servers run, or " +
				"delete the whole cluster with tent delete cluster",
		},
		{
			"servers that do not run, did not join or have no public address, and two nodes",
			[]cloud.Instance{
				w1b, withPublicAddress(changed(member(s3, 0, "i-1", minutes(1)), func(in *cloud.Instance) { in.Ready = false })),
				withPublicAddress(unjoined(member(s3, 1, "i-2", minutes(1)))), member(s3, 2, "i-3", minutes(1)),
			},
			two,
			"update would remove nodes that joined Nomad: prod-workers-1 (ID i-5, surplus) and prod-workers-2 " +
				"(ID i-6, surplus); cluster prod has no server that joined and runs for tent to ask: keep these nodes " +
				"in the specs until the servers run, or delete the whole cluster with tent delete cluster",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := readForRemoval(t.Context(), removalModel(s3, w1), tc.listed, tc.plan,
				func([]cloud.Instance) (nomadops.API, error) {
					t.Error("the API is made with no server to ask")
					return nil, nil
				})
			if err == nil || err.Error() != tc.want || !errors.Is(err, errNoServerToAsk) {
				t.Errorf("readForRemoval error = %v, want %q matching errNoServerToAsk", err, tc.want)
			}
		})
	}
}

// TestReadForRemovalWithoutAServerToAskOrARemoval fails with the error that the caller matches and a text that names
// no node, when the changes hold no removal.
func TestReadForRemovalWithoutAServerToAskOrARemoval(t *testing.T) {
	t.Parallel()
	s3, w1 := servers(3), workers(1)
	plan := []NodeChange{wantCreate(s3, 0, "ams"), wantDelete(unjoined(member(w1, 1, "i-5", minutes(2))), "surplus")}
	_, err := readForRemoval(t.Context(), removalModel(s3, w1), []cloud.Instance{member(w1, 0, "i-4", minutes(1))}, plan,
		func([]cloud.Instance) (nomadops.API, error) {
			t.Error("the API is made with no server to ask")
			return nil, nil
		})
	want := "cluster prod has no server that joined and runs for tent to ask"
	if err == nil || err.Error() != want || !errors.Is(err, errNoServerToAsk) {
		t.Errorf("readForRemoval error = %v, want %q matching errNoServerToAsk", err, want)
	}
}

// TestReadForRemovalFailures returns the error of the API's maker as it is, and the error of a read with the name of
// the read, and no reading with either. A read that no server answers adds the advice to start a stopped server.
func TestReadForRemovalFailures(t *testing.T) {
	t.Parallel()
	s3 := servers(3)
	running := withPublicAddress(member(s3, 0, "i-1", minutes(1)))
	stopped := changed(member(s3, 1, "i-2", minutes(1)), func(in *cloud.Instance) { in.Ready = false })
	errMaker, errRead := errors.New("no certificate"), errors.New("connection refused")
	for _, tc := range []struct {
		name      string
		listed    []cloud.Instance
		maker     error
		failRead  error
		want      string
		wantMatch error
	}{
		{"the API cannot be made", []cloud.Instance{running}, errMaker, nil, "no certificate", errMaker},
		{"a read fails", []cloud.Instance{running}, nil, errRead, "read the Raft configuration: connection refused",
			errRead},
		{
			"no server answers while one is stopped", []cloud.Instance{running, stopped}, nil, nomadops.ErrNotReady,
			"read the Raft configuration: " + nomadops.ErrNotReady.Error() + "; node prod-servers-1 (ID i-2) is " +
				"stopped: if the servers have lost their quorum, start that instance again and run the command again",
			nomadops.ErrNotReady,
		},
		{
			"no server answers and none is stopped", []cloud.Instance{running}, nil, nomadops.ErrNotReady,
			"read the Raft configuration: " + nomadops.ErrNotReady.Error(), nomadops.ErrNotReady,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &readStub{failOn: "Peers", err: tc.failRead}
			got, err := readForRemoval(t.Context(), removalModel(s3), tc.listed, nil,
				func([]cloud.Instance) (nomadops.API, error) {
					if tc.maker != nil {
						return nil, tc.maker
					}
					return stub, nil
				})
			if err == nil || err.Error() != tc.want || !errors.Is(err, tc.wantMatch) {
				t.Errorf("readForRemoval error = %v, want %q matching %v", err, tc.want, tc.wantMatch)
			}
			if diff := cmp.Diff(nomadReading{}, got, cmp.AllowUnexported(nomadReading{})); diff != "" {
				t.Errorf("the reading with an error (-want +got):\n%s", diff)
			}
			if tc.maker == nil && !slices.Equal(stub.calls, []string{"Peers"}) {
				t.Errorf("the reads = %v, want the failed one alone", stub.calls)
			}
		})
	}
}

// removalCluster is the test cluster of the decisions below: a server group, a client group with a drain timeout of
// its own, and the group "old" that the specs lack.
type removalCluster struct {
	m      *model.Cluster
	groups []rollout.Group
}

func newRemovalCluster(sizeOfServers, sizeOfWorkers int) removalCluster {
	s, w := servers(sizeOfServers), workers(sizeOfWorkers)
	return removalCluster{
		m:      removalModel(s, w),
		groups: []rollout.Group{specGroup(s, 0), specGroup(w, 30*time.Minute)},
	}
}

// input returns the input of the decisions for the listed machines and the changes of the plan.
func (c removalCluster) input(listed []cloud.Instance, changes ...NodeChange) removalInput {
	return removalInput{
		m: c.m, groups: c.groups, listed: listed, changes: changes, version: removalVersion, now: removalNow,
	}
}

// clientsReading returns a reading with a ready node for each client machine.
func clientsReading(ms ...cloud.Instance) nomadReading {
	var r nomadReading
	for _, in := range ms {
		r.nodes = append(r.nodes, readyNode(in))
	}
	return r
}

// wantPlan checks the part of the removal plan that the apply uses.
func wantPlan(t *testing.T, got removalPlan, c removalCluster, groups []rollout.Group, planned ...string) {
	t.Helper()
	if got.model != c.m {
		t.Errorf("model = %p, want the model of the cluster %p", got.model, c.m)
	}
	if got.version != removalVersion {
		t.Errorf("version = %q, want %q", got.version, removalVersion)
	}
	if diff := cmp.Diff(groups, got.groups); diff != "" {
		t.Errorf("groups (-want +got):\n%s", diff)
	}
	want := map[string]bool{}
	for _, id := range planned {
		want[id] = true
	}
	if diff := cmp.Diff(want, got.planned); diff != "" {
		t.Errorf("planned machines (-want +got):\n%s", diff)
	}
}

// TestDecideRemovesAClientThatJoined puts the group of the node that the plan drains into the removal plan, with the
// settings of its spec, and the node's machine among the planned machines.
func TestDecideRemovesAClientThatJoined(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	in := c.input([]cloud.Instance{w0, w1}, wantRemoval(w1, "surplus", "30m0s", false))
	plan, purges, err := in.decide(clientsReading(w0, w1))
	if err != nil {
		t.Fatal(err)
	}
	wantPlan(t, plan, c, []rollout.Group{c.groups[1]}, "i-2")
	if len(purges) != 0 {
		t.Errorf("purges = %v, want none", purges)
	}
}

// TestDecideRemovesAClientOfAGroupThatTheSpecsLack makes a group of its name for the decisions: a client group of size
// 0 with no limit on unavailable nodes and the default drain timeout.
func TestDecideRemovesAClientOfAGroupThatTheSpecsLack(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	old := nodeGroup("old", v1alpha1.RoleClient, 0)
	o0, o1 := member(old, 0, "i-7", minutes(1)), member(old, 1, "i-8", minutes(1))
	in := c.input([]cloud.Instance{o0, o1},
		wantRemoval(o0, "not in the spec", "1h0m0s", false), wantRemoval(o1, "not in the spec", "1h0m0s", false))
	plan, _, err := in.decide(clientsReading(o0, o1))
	if err != nil {
		t.Fatal(err)
	}
	wantPlan(t, plan, c, []rollout.Group{removedGroup("old")}, "i-7", "i-8")
}

// TestDecideLeavesOutTheDeletesOfMachinesThatNeverJoined asks the decisions without the machines that the plan
// deletes as before: a twin of the same name would make them refuse the group.
func TestDecideLeavesOutTheDeletesOfMachinesThatNeverJoined(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	twin := unjoined(member(workers(1), 1, "i-3", minutes(3)))
	in := c.input([]cloud.Instance{w0, w1, twin},
		wantDelete(twin, "duplicate"), wantRemoval(w1, "surplus", "30m0s", false))
	plan, _, err := in.decide(clientsReading(w0, w1))
	if err != nil {
		t.Fatalf("decide with a twin that the plan deletes: %v", err)
	}
	wantPlan(t, plan, c, []rollout.Group{c.groups[1]}, "i-2")
}

// TestDecideAsksTheDecisionsToShrink refuses a twin that the plan does not delete with the text of a shrink, which
// asks to run update again: a roll would say first.
func TestDecideAsksTheDecisionsToShrink(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	twin := member(workers(1), 1, "i-3", minutes(3))
	in := c.input([]cloud.Instance{w0, w1, twin}, wantRemoval(w1, "surplus", "30m0s", false))
	_, _, err := in.decide(clientsReading(w0, w1))
	want := "update cannot remove prod-workers-1: node group workers: machines i-2 and i-3 share the name " +
		"prod-workers-1; run tent update cluster again"
	if err == nil || err.Error() != want || !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("decide error = %v, want %q matching ErrRefused", err, want)
	}
}

// TestDecideRefusesWhatTheDecisionsRefuse fails the plan with the refusal of rollout after the names of the nodes that
// it removes, in the order of the plan.
func TestDecideRefusesWhatTheDecisionsRefuse(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(3, 1)
	var machines []cloud.Instance
	for i := range 5 {
		machines = append(machines, member(servers(5), i, "i-"+string(rune('1'+i)), minutes(i)))
	}
	in := c.input(machines, wantRemoval(machines[4], "surplus", "", true), wantRemoval(machines[3], "surplus", "", true))
	_, _, err := in.decide(healthyServers(machines, "prod-servers-1"))
	want := "update cannot remove prod-servers-4 and prod-servers-3: node group servers: autopilot reports the " +
		"servers unhealthy (prod-servers-1); tent removes a server only while every server is healthy"
	if err == nil || err.Error() != want || !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("decide error = %v, want %q matching ErrRefused", err, want)
	}
	if _, _, err := in.decide(healthyServers(machines)); err != nil {
		t.Errorf("decide with healthy servers: %v", err)
	}
}

// TestDecideRefusesAClientGroupWhoseRoleChanged fails the plan with the refusal of a role that changed, in a group of
// the specs.
func TestDecideRefusesAClientGroupWhoseRoleChanged(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	in := c.input([]cloud.Instance{labelled(w0, v1alpha1.RoleCombined), w1}, wantRemoval(w1, "surplus", "30m0s", false))
	_, _, err := in.decide(clientsReading(w0, w1))
	want := "update cannot remove prod-workers-1: node group workers: node prod-workers-0 was created as a combined " +
		"node and the group is now client; tent does not replace or remove the nodes of a group whose role changed; " +
		"a node group keeps its role, and a cluster moves by creating a new one"
	if err == nil || err.Error() != want {
		t.Errorf("decide error = %v, want %q", err, want)
	}
}

// TestDecideLeavesOutAGroupThatLosesNoNode asks the decisions about the groups that lose a node alone: a server group
// that they would refuse, and that loses none, does not fail the removal of a worker.
func TestDecideLeavesOutAGroupThatLosesNoNode(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	s0 := labelled(member(servers(1), 0, "i-9", minutes(1)), v1alpha1.RoleCombined)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	in := c.input([]cloud.Instance{s0, w0, w1}, wantRemoval(w1, "surplus", "30m0s", false))
	plan, _, err := in.decide(clientsReading(w0, w1))
	if err != nil {
		t.Fatalf("decide beside a group that loses no node and that the decisions refuse: %v", err)
	}
	wantPlan(t, plan, c, []rollout.Group{c.groups[1]}, "i-2")
}

// TestDecideRefusesAGroupThatTheSpecsLack asks the decisions about the group that stands for one that the specs lack:
// the cloud lists no private address for its only machine, so its node cannot be told and the group cannot go on.
func TestDecideRefusesAGroupThatTheSpecsLack(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	o0 := changed(member(nodeGroup("old", v1alpha1.RoleClient, 0), 0, "i-7", minutes(1)),
		func(in *cloud.Instance) { in.PrivateIP = netip.Addr{} })
	in := c.input([]cloud.Instance{o0}, wantRemoval(o0, "not in the spec", "1h0m0s", false))
	_, _, err := in.decide(nomadReading{})
	want := "update cannot remove prod-old-0: node group old: cannot go on: prod-old-0 has no node in Nomad: the cloud " +
		"reports no private address for it; with maxUnavailable 0 no surplus node can be removed"
	if err == nil || err.Error() != want || !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("decide error = %v, want %q matching ErrRefused", err, want)
	}
}

// TestDecideStateOfTheDecisions builds the state that the decisions and the loop see: the cluster's machines but the
// deletes of the plan and the machines of other clusters, so that a machine that the plan waits for stays; the reading,
// the pinned version, the time, and the machines that carry the replace label as forced.
func TestDecideStateOfTheDecisions(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	w0 = changed(w0, func(in *cloud.Instance) { in.Replace = true })
	gone := unjoined(member(workers(1), 1, "i-3", minutes(3)))
	other := changed(member(workers(1), 0, "i-4", minutes(1)), func(in *cloud.Instance) { in.Cluster = "dev" })
	waiting := unjoined(member(workers(1), 2, "i-5", minutes(4)))
	in := c.input([]cloud.Instance{w0, w1, gone, other, waiting},
		wantDelete(gone, "duplicate"), wantJoinWait(workers(1), waiting))
	reading := clientsReading(w0, w1)
	got := in.stateOf([]rollout.Group{c.groups[1]}, reading)
	want := rollout.State{
		Cluster: "prod", Groups: []rollout.Group{c.groups[1]}, Machines: rolloutMachines([]cloud.Instance{w0, w1, waiting}),
		Nomad: reading.state(), Version: removalVersion, Forced: map[string]bool{"i-1": true},
		Refresh: joinRefresh, Now: removalNow,
	}
	if diff := cmp.Diff(want, got, equateNetip); diff != "" {
		t.Errorf("state (-want +got):\n%s", diff)
	}
}

// TestDecideWithNothingToDo returns no group, no machine and no purge.
func TestDecideWithNothingToDo(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0 := member(workers(1), 0, "i-1", minutes(1))
	plan, purges, err := c.input([]cloud.Instance{w0}).decide(clientsReading(w0))
	if err != nil {
		t.Fatal(err)
	}
	if plan.groups != nil || plan.planned != nil || len(purges) != 0 {
		t.Errorf("decide = %+v, %v, want no group, no planned machine and no purge", plan, purges)
	}
}

// purgeCluster is a cluster with a server group, a client group and a combined group, and the machines of the client
// and combined group that stay.
func purgeCluster() (removalCluster, []cloud.Instance) {
	c := newRemovalCluster(1, 1)
	pool := nodeGroup("pool", v1alpha1.RoleCombined, 1)
	c.m.Groups = append(c.m.Groups, pool)
	c.groups = append(c.groups, specGroup(pool, 2*time.Hour))
	return c, []cloud.Instance{
		member(servers(1), 0, "i-1", minutes(1)), member(workers(1), 0, "i-2", minutes(1)),
		member(pool, 0, "i-3", minutes(1)),
	}
}

// TestDecidePurges lists the down nodes that no machine has: those of a client or combined group of the specs and of a
// client group that the specs lack, by name and then ID. A purge alone puts its group into the plan with no planned
// machine.
func TestDecidePurges(t *testing.T) {
	t.Parallel()
	c, machines := purgeCluster()
	workersGroup, poolGroup := c.groups[1], c.groups[2]
	for _, tc := range []struct {
		name       string
		nodes      []nomadops.Node
		wantPurges []NodePurge
		wantGroups []rollout.Group
	}{
		{
			name:  "the purges by name where the groups sort the other way",
			nodes: []nomadops.Node{downNode("prod-web-9", "i-10"), downNode("prod-web-2-0", "i-11")},
			wantPurges: []NodePurge{
				{Name: "prod-web-2-0", Address: "10.64.0.11", ID: "n-i-11", Group: "web-2"},
				{Name: "prod-web-9", Address: "10.64.0.10", ID: "n-i-10", Group: "web"},
			},
			wantGroups: []rollout.Group{removedGroup("web"), removedGroup("web-2")},
		},
		{
			name:       "a down node of a client group of the specs",
			nodes:      []nomadops.Node{downNode("prod-workers-1", "i-5")},
			wantPurges: []NodePurge{{Name: "prod-workers-1", Address: "10.64.0.5", ID: "n-i-5", Group: "workers"}},
			wantGroups: []rollout.Group{workersGroup},
		},
		{
			name:       "a down node of a combined group of the specs",
			nodes:      []nomadops.Node{downNode("prod-pool-1", "i-5")},
			wantPurges: []NodePurge{{Name: "prod-pool-1", Address: "10.64.0.5", ID: "n-i-5", Group: "pool"}},
			wantGroups: []rollout.Group{poolGroup},
		},
		{
			name:       "a down node of a group that the specs lack, with a dash in its name",
			nodes:      []nomadops.Node{downNode("prod-web-workers-4", "i-5")},
			wantPurges: []NodePurge{{Name: "prod-web-workers-4", Address: "10.64.0.5", ID: "n-i-5", Group: "web-workers"}},
			wantGroups: []rollout.Group{removedGroup("web-workers")},
		},
		{
			name: "the purges by name, then ID, and the groups by name",
			nodes: []nomadops.Node{
				downNode("prod-workers-2", "i-7"), downNode("prod-old-1", "i-6"), downNode("prod-workers-1", "i-9"),
				downNode("prod-workers-1", "i-8"),
			},
			wantPurges: []NodePurge{
				{Name: "prod-old-1", Address: "10.64.0.6", ID: "n-i-6", Group: "old"},
				{Name: "prod-workers-1", Address: "10.64.0.8", ID: "n-i-8", Group: "workers"},
				{Name: "prod-workers-1", Address: "10.64.0.9", ID: "n-i-9", Group: "workers"},
				{Name: "prod-workers-2", Address: "10.64.0.7", ID: "n-i-7", Group: "workers"},
			},
			wantGroups: []rollout.Group{removedGroup("old"), workersGroup},
		},
		{
			name:       "a down node with no address",
			nodes:      []nomadops.Node{{ID: "n-5", Name: "prod-workers-1", Status: "down"}},
			wantPurges: []NodePurge{{Name: "prod-workers-1", ID: "n-5", Group: "workers"}},
			wantGroups: []rollout.Group{workersGroup},
		},
		{
			name:       "a down node beside the ready node of a machine, which reports no version",
			nodes:      []nomadops.Node{readyNode(machines[1]), downNode("prod-workers-1", "i-5")},
			wantPurges: []NodePurge{{Name: "prod-workers-1", Address: "10.64.0.5", ID: "n-i-5", Group: "workers"}},
			wantGroups: []rollout.Group{workersGroup},
		},
		{name: "a node that is ready", nodes: []nomadops.Node{readyNode(member(workers(2), 1, "i-5", minutes(1)))}},
		{
			name: "a node that is disconnected",
			nodes: []nomadops.Node{
				changed2(downNode("prod-workers-1", "i-5"), func(n *nomadops.Node) { n.Status = "disconnected" }),
			},
		},
		{
			name:  "a down node whose machine is listed at its name and address",
			nodes: []nomadops.Node{downNode("prod-workers-0", "i-2")},
		},
		{
			name:       "a down node of the name of a machine, at another address",
			nodes:      []nomadops.Node{downNode("prod-workers-0", "i-6")},
			wantPurges: []NodePurge{{Name: "prod-workers-0", Address: "10.64.0.6", ID: "n-i-6", Group: "workers"}},
			wantGroups: []rollout.Group{workersGroup},
		},
		{name: "a down node under the name of a server group", nodes: []nomadops.Node{downNode("prod-servers-3", "i-5")}},
		{name: "a down node of no group's pattern", nodes: []nomadops.Node{
			downNode("prod-workers", "i-5"), downNode("prod-workers-x", "i-6"), downNode("dev-workers-3", "i-7"),
			downNode("laptop", "i-8"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, purges, err := c.input(machines).decide(nomadReading{nodes: tc.nodes})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.wantPurges, purges); diff != "" {
				t.Errorf("purges (-want +got):\n%s", diff)
			}
			if len(tc.wantPurges) == 0 {
				if plan.groups != nil || plan.planned != nil {
					t.Errorf("plan = %+v, want no group and no planned machine", plan)
				}
				return
			}
			wantPlan(t, plan, c, tc.wantGroups)
		})
	}
}

// changed2 returns the node n after change has changed it.
func changed2(n nomadops.Node, change func(*nomadops.Node)) nomadops.Node {
	change(&n)
	return n
}

// TestDecidePurgeBesidesARemoval lists the purge of a group that also loses a machine, and the group once.
func TestDecidePurgeBesidesARemoval(t *testing.T) {
	t.Parallel()
	c := newRemovalCluster(1, 1)
	w0, w1 := member(workers(1), 0, "i-1", minutes(1)), member(workers(1), 1, "i-2", minutes(2))
	reading := clientsReading(w0, w1)
	reading.nodes = append(reading.nodes, downNode("prod-workers-2", "i-5"))
	plan, purges, err := c.input([]cloud.Instance{w0, w1}, wantRemoval(w1, "surplus", "30m0s", false)).decide(reading)
	if err != nil {
		t.Fatal(err)
	}
	wantPlan(t, plan, c, []rollout.Group{c.groups[1]}, "i-2")
	want := []NodePurge{{Name: "prod-workers-2", Address: "10.64.0.5", ID: "n-i-5", Group: "workers"}}
	if diff := cmp.Diff(want, purges); diff != "" {
		t.Errorf("purges (-want +got):\n%s", diff)
	}
}

// TestDecideLeavesTheNodesOfAGroupThatTheDecisionsRefuse lists no purge for a group that rollout refuses, and keeps the
// purges of the other groups: a node that is left over must not fail an update that has other work.
func TestDecideLeavesTheNodesOfAGroupThatTheDecisionsRefuse(t *testing.T) {
	t.Parallel()
	c, machines := purgeCluster()
	twin := member(workers(1), 0, "i-4", minutes(2))
	nodes := []nomadops.Node{downNode("prod-workers-1", "i-5"), downNode("prod-pool-1", "i-6")}
	for _, tc := range []struct {
		name   string
		listed []cloud.Instance
	}{
		{"two machines of one name", append(slices.Clone(machines), twin)},
		{"a role that changed", slices.Concat(machines[:1], []cloud.Instance{labelled(machines[1], v1alpha1.RoleCombined)},
			machines[2:])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, purges, err := c.input(tc.listed).decide(nomadReading{nodes: nodes})
			if err != nil {
				t.Fatalf("decide: %v, want the refused group left alone", err)
			}
			want := []NodePurge{{Name: "prod-pool-1", Address: "10.64.0.6", ID: "n-i-6", Group: "pool"}}
			if diff := cmp.Diff(want, purges); diff != "" {
				t.Errorf("purges (-want +got):\n%s", diff)
			}
			wantPlan(t, plan, c, []rollout.Group{c.groups[2]})
		})
	}
}

// TestDecideLeavesTheNodesOfARemovedGroupThatTheDecisionsRefuse lists no purge for a group that the specs lack and
// that rollout refuses.
func TestDecideLeavesTheNodesOfARemovedGroupThatTheDecisionsRefuse(t *testing.T) {
	t.Parallel()
	c, machines := purgeCluster()
	old := nodeGroup("old", v1alpha1.RoleClient, 0)
	twins := []cloud.Instance{
		unjoined(member(old, 0, "i-7", minutes(1))), unjoined(member(old, 0, "i-8", minutes(2))),
	}
	plan, purges, err := c.input(append(slices.Clone(machines), twins...)).decide(
		nomadReading{nodes: []nomadops.Node{downNode("prod-old-1", "i-9")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(purges) != 0 || plan.groups != nil {
		t.Errorf("decide = %+v, %v, want no purge and no group for a group that is refused", plan, purges)
	}
}

// TestDecideAsksAboutEachGroupThatLosesANode refuses a plan when a later group cannot start, though the first group
// has a step: the cloud lists no private address for a worker, so the workers cannot lose a node.
func TestDecideAsksAboutEachGroupThatLosesANode(t *testing.T) {
	t.Parallel()
	apps, w2 := nodeGroup("apps", v1alpha1.RoleClient, 1), workers(2)
	c := removalCluster{
		m:      removalModel(servers(1), apps, w2),
		groups: []rollout.Group{specGroup(servers(1), 0), specGroup(apps, 0), specGroup(w2, 0)},
	}
	a0, a1 := member(apps, 0, "i-1", minutes(1)), member(apps, 1, "i-2", minutes(2))
	w0, w1 := member(w2, 0, "i-3", minutes(1)), member(w2, 1, "i-4", minutes(2))
	hidden := changed(member(w2, 2, "i-5", minutes(3)), func(in *cloud.Instance) { in.PrivateIP = netip.Addr{} })
	in := c.input([]cloud.Instance{a0, a1, w0, w1, hidden},
		wantRemoval(a1, "surplus", "1h0m0s", false), wantRemoval(w1, "surplus", "1h0m0s", false))
	_, _, err := in.decide(clientsReading(a0, a1, w0, w1))
	want := "update cannot remove prod-apps-1 and prod-workers-1: node group workers: cannot go on: prod-workers-2 has " +
		"no node in Nomad: the cloud reports no private address for it; with maxUnavailable 0 no surplus node can be " +
		"removed"
	if err == nil || err.Error() != want || !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("decide error = %v, want %q matching ErrRefused", err, want)
	}
}
