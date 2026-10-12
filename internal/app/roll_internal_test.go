package app

import (
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// TestRollActionNames names every action of rollout from Done to WaitStable, and an action outside that range
// unknown.
func TestRollActionNames(t *testing.T) {
	t.Parallel()
	want := map[rollout.Action]string{
		rollout.Done: "done", rollout.Create: "create", rollout.WaitJoined: "wait-joined",
		rollout.MarkIneligible: "mark-ineligible", rollout.Drain: "drain", rollout.WaitDrained: "wait-drained",
		rollout.Delete: "delete", rollout.WaitNodeDown: "wait-node-down", rollout.Purge: "purge",
		rollout.TransferLeadership: "transfer-leadership", rollout.Stop: "stop",
		rollout.WaitServerDown: "wait-server-down", rollout.RemovePeer: "remove-peer",
		rollout.ForceLeave: "force-leave", rollout.WaitHealthy: "wait-healthy", rollout.WaitStable: "wait-stable",
	}
	if len(want) != int(rollout.WaitStable-rollout.Done)+1 {
		t.Fatalf("the test names %d actions, rollout has %d", len(want), rollout.WaitStable-rollout.Done+1)
	}
	for a := rollout.Done; a <= rollout.WaitStable; a++ {
		if got := rollActionName(a); got != want[a] {
			t.Errorf("rollActionName(%d) = %q, want %q", int(a), got, want[a])
		}
	}
	for _, tc := range []struct {
		action rollout.Action
		want   string
	}{{0, "unknown action 0"}, {rollout.WaitStable + 1, "unknown action 17"}} {
		if got := rollActionName(tc.action); got != tc.want {
			t.Errorf("rollActionName(%d) = %q, want %q", int(tc.action), got, tc.want)
		}
	}
}

// TestRollStepOf names the machine of a step, else its node, and keeps the machine's ID and the step's text.
func TestRollStepOf(t *testing.T) {
	t.Parallel()
	machine := rollout.Machine{ID: "instance-6", Name: "prod-workers-0", Group: "workers"}
	node := rollout.Node{ID: "n-6", Name: "prod-workers-7", Address: netip.MustParseAddr("10.64.0.6")}
	for _, tc := range []struct {
		name string
		step rollout.Step
		want RollStep
	}{
		{"a create names the new machine and has no ID", rollout.Step{
			Action: rollout.Create, Group: "workers",
			Machine: rollout.Machine{Name: "prod-workers-2", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams"},
		}, RollStep{
			Action: "create", Group: "workers", Node: "prod-workers-2",
			Text: "create node prod-workers-2 (client of workers, ams)",
		}},
		{"a delete names the machine, whatever the node's name", rollout.Step{
			Action: rollout.Delete, Group: "workers", Machine: machine, Node: node,
		}, RollStep{
			Action: "delete", Group: "workers", Node: "prod-workers-0", ID: "instance-6",
			Text: "delete node prod-workers-0 (ID instance-6)",
		}},
		{"a purge of an orphan names the node and has no ID", rollout.Step{
			Action: rollout.Purge, Group: "workers", Node: node,
		}, RollStep{
			Action: "purge", Group: "workers", Node: "prod-workers-7",
			Text: "purge node prod-workers-7 at 10.64.0.6 from Nomad",
		}},
		{"a wait for the servers names none", rollout.Step{
			Action: rollout.WaitHealthy, Group: "workers", Voters: 3,
		}, RollStep{Action: "wait-healthy", Group: "workers", Text: "wait until 3 healthy servers vote"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, rollStepOf(tc.step)); diff != "" {
				t.Errorf("rollStepOf (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOutdatedMachines lists the machines of the group whose hash is not the group's, that carry none or that the run
// forces, by name and ID, each with its reason: the hash comes before the force.
func TestOutdatedMachines(t *testing.T) {
	t.Parallel()
	g := rollout.Group{Name: "workers", SpecHash: "new"}
	machine := func(id, name, group, hash string) cloud.Instance {
		return cloud.Instance{ID: id, Name: name, Group: group, SpecHash: hash}
	}
	listed := []cloud.Instance{
		machine("i-9", "prod-workers-1", "workers", "old"),
		machine("i-8", "prod-workers-1", "workers", ""),
		machine("i-7", "prod-workers-0", "workers", "new"),
		machine("i-6", "prod-workers-3", "workers", "new"),
		machine("i-5", "prod-workers-2", "workers", "old"),
		machine("i-4", "prod-servers-0", "servers", "old"),
	}
	forced := map[string]bool{"i-6": true, "i-5": true, "i-4": true}
	want := []OutdatedNode{
		{Name: "prod-workers-1", ID: "i-8", Group: "workers", Reason: "no spec hash"},
		{Name: "prod-workers-1", ID: "i-9", Group: "workers", Reason: "spec hash"},
		{Name: "prod-workers-2", ID: "i-5", Group: "workers", Reason: "spec hash"},
		{Name: "prod-workers-3", ID: "i-6", Group: "workers", Reason: "forced"},
	}
	if diff := cmp.Diff(want, outdatedOf(listed, g, forced)); diff != "" {
		t.Errorf("outdated machines (-want +got):\n%s", diff)
	}
	if got := outdatedOf(listed[2:4], g, nil); len(got) != 0 {
		t.Errorf("outdated machines of up to date, unforced ones = %v, want none", got)
	}
}

// TestJoinCheck gives what each rule of the wait for a new node says, in the order of the rules: repeat the create of
// a machine that is not ready, fail without a private address, scrub a machine that has joined as its role means
// (a client's node is ready and eligible, a server votes and is healthy, a combined node does both), refuse a client
// that is older than its intro token's life, and else wait.
func TestJoinCheck(t *testing.T) {
	t.Parallel()
	now := minutes(59)
	addr := netip.MustParseAddr("10.64.0.6")
	machine := func(change func(*cloud.Instance)) cloud.Instance {
		in := unjoined(member(workers(2), 2, "i-6", now.Add(-time.Minute)))
		in.PrivateIP = addr
		if change != nil {
			change(&in)
		}
		return in
	}
	node := func(change func(*nomadops.Node)) nomadReading {
		n := nomadops.Node{ID: "n-6", Name: "prod-workers-2", Address: addr, Status: "ready", Eligible: true}
		if change != nil {
			change(&n)
		}
		return nomadReading{nodes: []nomadops.Node{n}}
	}
	both := func(voter, healthy bool, change func(*nomadops.Node)) nomadReading {
		r := serversReading(healthy, serverEntry{"s-6", "10.64.0.6", voter, healthy})
		r.nodes = node(change).nodes
		return r
	}
	role := func(role v1alpha1.Role, change func(*cloud.Instance)) cloud.Instance {
		return machine(func(in *cloud.Instance) {
			in.Role = role
			if change != nil {
				change(in)
			}
		})
	}
	old := func(in *cloud.Instance) { in.Created = now.Add(-introLifetime - time.Nanosecond) }
	const noAddress = "node prod-workers-2: the cloud reports no private address for it yet; run the command again"
	const tooOld = "node prod-workers-2 has not joined within 31 minutes of its creation; " +
		"run tent update cluster, which deletes it and creates it again"
	for _, tc := range []struct {
		name    string
		in      cloud.Instance
		reading nomadReading
		want    joinStep
		wantErr string
	}{
		{"a machine that is not ready is created again with its operation id", machine(func(in *cloud.Instance) {
			in.Ready, in.PrivateIP = false, netip.Addr{}
			old(in)
		}), nomadReading{}, joinCreate, ""},
		{"a machine that is not ready and has no valid operation id has no address", machine(func(in *cloud.Instance) {
			in.Ready, in.Op, in.PrivateIP = false, "", netip.Addr{}
		}), nomadReading{}, 0, noAddress},
		{"a ready machine without an address", machine(func(in *cloud.Instance) { in.PrivateIP = netip.Addr{} }),
			nomadReading{}, 0, noAddress},
		{"a node that is ready and eligible is scrubbed", machine(nil), node(nil), joinScrub, ""},
		{"a node that is not eligible is waited for", machine(nil), node(func(n *nomadops.Node) { n.Eligible = false }),
			joinWait, ""},
		{"a node that is not ready is waited for", machine(nil),
			node(func(n *nomadops.Node) { n.Status = "initializing" }), joinWait, ""},
		{"a node of another address is waited for", machine(nil),
			node(func(n *nomadops.Node) { n.Address = netip.MustParseAddr("10.64.0.7") }), joinWait, ""},
		{"a node of another name is waited for", machine(nil),
			node(func(n *nomadops.Node) { n.Name = "prod-workers-3" }), joinWait, ""},
		{"a client within the life of its intro token is waited for", machine(func(in *cloud.Instance) {
			in.Created = now.Add(-introLifetime)
		}), nomadReading{}, joinWait, ""},
		{"a client past the life of its intro token is refused", machine(old), nomadReading{}, 0, tooOld},
		{"an old client whose node is ready is scrubbed", machine(old), node(nil), joinScrub, ""},
		{"a client whose address holds a healthy voter and no node is waited for", machine(nil),
			serversReading(true, serverEntry{"s-6", "10.64.0.6", true, true}), joinWait, ""},
		{"a machine with no creation time is never old", machine(func(in *cloud.Instance) {
			in.Created = time.Time{}
		}), nomadReading{}, joinWait, ""},
		{"a server without an address fails the wait", role(v1alpha1.RoleServer, func(in *cloud.Instance) {
			in.PrivateIP = netip.Addr{}
		}), nomadReading{}, 0, noAddress},
		{"a server that is not ready is created again with its operation id",
			role(v1alpha1.RoleServer, func(in *cloud.Instance) { in.Ready, in.PrivateIP = false, netip.Addr{} }),
			nomadReading{}, joinCreate, ""},
		{"an old server is waited for", role(v1alpha1.RoleServer, old), nomadReading{}, joinWait, ""},
		{"a server that votes and is healthy is scrubbed", role(v1alpha1.RoleServer, nil),
			both(true, true, nil), joinScrub, ""},
		{"a server that votes and is not healthy is waited for", role(v1alpha1.RoleServer, nil),
			both(true, false, nil), joinWait, ""},
		{"a server that does not vote is waited for", role(v1alpha1.RoleServer, nil),
			both(false, true, nil), joinWait, ""},
		{"a server whose node is ready and that has no peer is waited for", role(v1alpha1.RoleServer, nil),
			node(nil), joinWait, ""},
		{"a combined node whose server votes and whose node is ready is scrubbed", role(v1alpha1.RoleCombined, nil),
			both(true, true, nil), joinScrub, ""},
		{"a combined node whose node is ready and whose server does not vote is waited for",
			role(v1alpha1.RoleCombined, nil), both(false, true, nil), joinWait, ""},
		{"a combined node whose node is ready and whose server is not healthy is waited for",
			role(v1alpha1.RoleCombined, nil), both(true, false, nil), joinWait, ""},
		{"a combined node whose server votes and that has no node is waited for", role(v1alpha1.RoleCombined, nil),
			serversReading(true, serverEntry{"s-6", "10.64.0.6", true, true}), joinWait, ""},
		{"a combined node whose server votes and whose node is not eligible is waited for",
			role(v1alpha1.RoleCombined, nil), both(true, true, func(n *nomadops.Node) { n.Eligible = false }),
			joinWait, ""},
		{"a combined node is never refused for its age", role(v1alpha1.RoleCombined, old), nomadReading{},
			joinWait, ""},
		{"a combined node without an address fails the wait", role(v1alpha1.RoleCombined, func(in *cloud.Instance) {
			in.PrivateIP = netip.Addr{}
		}), nomadReading{}, 0, noAddress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := joinCheck(tc.in, tc.reading, now)
			if (err == nil) != (tc.wantErr == "") || err != nil && err.Error() != tc.wantErr {
				t.Fatalf("joinCheck error = %v, want %q", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("joinCheck = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRollRunState gives the decisions the run's groups, machines, Nomad, version and forced machines, the interval at
// which nodes refresh their servers, and the time of the service.
func TestRollRunState(t *testing.T) {
	t.Parallel()
	now := minutes(5)
	r := &rollRun{
		s:       &Service{Now: func() time.Time { return now }},
		kit:     nodeKit{cluster: "prod"},
		groups:  []rollout.Group{{Name: "workers", Role: v1alpha1.RoleClient, Size: 2}},
		version: "2.0.7",
		forced:  map[string]bool{"i-1": true},
		listed:  []cloud.Instance{member(workers(2), 0, "i-1", minutes(0))},
	}
	reading := nomadReading{nodes: []nomadops.Node{{ID: "n-1", Name: "prod-workers-0"}}}

	want := rollout.State{
		Cluster: "prod",
		Groups:  []rollout.Group{{Name: "workers", Role: v1alpha1.RoleClient, Size: 2}},
		Machines: []rollout.Machine{{
			ID: "i-1", Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams", Ready: true,
			Joined: true, Created: minutes(0), PrivateIP: addressFor("i-1"),
		}},
		Nomad:   rollout.Nomad{Nodes: []rollout.Node{{ID: "n-1", Name: "prod-workers-0"}}},
		Version: "2.0.7", Forced: map[string]bool{"i-1": true}, Refresh: time.Minute, Now: now,
	}
	if diff := cmp.Diff(want, r.state(reading), equateNetip); diff != "" {
		t.Errorf("state (-want +got):\n%s", diff)
	}
}
