package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// TestKeyOfKeysACreateByItsName tells two creates of one group apart by the name of their machines, which have no ID
// yet, and a step on a machine by its ID.
func TestKeyOfKeysACreateByItsName(t *testing.T) {
	t.Parallel()
	create := func(name string) rollout.Step {
		return rollout.Step{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{Name: name}}
	}
	if keyOf(create("prod-workers-2")) == keyOf(create("prod-workers-3")) {
		t.Error("two creates of different machines have one key")
	}
	first, again := keyOf(create("prod-workers-2")), keyOf(create("prod-workers-2"))
	if first != again {
		t.Error("two creates of one machine have different keys")
	}
	other := create("prod-workers-2")
	other.Group = "db"
	if keyOf(create("prod-workers-2")) == keyOf(other) {
		t.Error("two creates of different groups have one key")
	}
	drain := func(id string, node string) rollout.Step {
		return rollout.Step{
			Action: rollout.Drain, Group: "workers", Machine: rollout.Machine{ID: id, Name: "prod-workers-0"},
			Node: rollout.Node{ID: node},
		}
	}
	if keyOf(drain("i-1", "n-1")) == keyOf(drain("i-2", "n-1")) {
		t.Error("two steps on different machines have one key")
	}
	if keyOf(drain("i-1", "n-1")) == keyOf(drain("i-1", "n-2")) {
		t.Error("two steps on different nodes have one key")
	}
}

// TestKeyOfKeysAServerStepByItsTarget tells the steps of one victim apart by the server that takes the leadership, the
// peer that is removed and the member that is forced out, and a wait by its victim.
func TestKeyOfKeysAServerStepByItsTarget(t *testing.T) {
	t.Parallel()
	victim := func(id string) rollout.Machine { return rollout.Machine{ID: id, Name: "prod-servers-" + id} }
	transfer := func(id, to string) rollout.Step {
		return rollout.Step{Action: rollout.TransferLeadership, Group: "servers", Machine: victim(id),
			Server: rollout.Server{ID: to}}
	}
	removal := func(id, peer string) rollout.Step {
		return rollout.Step{
			Action: rollout.RemovePeer, Group: "servers", Machine: victim(id), Server: rollout.Server{ID: peer},
		}
	}
	force := func(id, member string) rollout.Step {
		return rollout.Step{
			Action: rollout.ForceLeave, Group: "servers", Machine: victim(id), Member: rollout.Member{Name: member},
		}
	}
	wait := func(id string) rollout.Step {
		return rollout.Step{Action: rollout.WaitStable, Group: "servers", Machine: victim(id)}
	}
	for _, tc := range []struct {
		name       string
		one, other rollout.Step
	}{
		{"leaders to take over", transfer("0", "r-1"), transfer("0", "r-2")},
		{"peers to remove", removal("0", "r-1"), removal("0", "r-2")},
		{"members to force out", force("0", "a.global"), force("0", "b.global")},
		{"victims of a wait", wait("0"), wait("1")},
	} {
		if keyOf(tc.one) == keyOf(tc.other) {
			t.Errorf("two steps of %s have one key", tc.name)
		}
	}
	first, again := keyOf(transfer("0", "r-1")), keyOf(transfer("0", "r-1"))
	if first != again {
		t.Error("two equal transfers have different keys")
	}
}

// stableUntil is the end of a stability window in the tests.
var stableUntil = time.Date(2026, 10, 8, 12, 4, 20, 0, time.UTC)

// TestWaitOfGivesEachWaitItsLimitAndItsEvent holds each wait of a roll to its limit and tells Nomad's event
// what the wait waits for.
func TestWaitOfGivesEachWaitItsLimitAndItsEvent(t *testing.T) {
	t.Parallel()
	r := &rollRun{groups: []rollout.Group{{Name: "workers", DrainTimeout: 10 * time.Minute}}}
	node := rollout.Node{ID: "n-1", Name: "prod-workers-0", Address: netip.MustParseAddr("10.64.0.6")}
	for _, tc := range []struct {
		name string
		step rollout.Step
		want rollWait
	}{
		{"join", rollout.Step{Action: rollout.WaitJoined, Group: "workers", Machine: rollout.Machine{Name: "prod-workers-2"}},
			rollWait{NomadEvent{Action: NomadRegister, Node: "prod-workers-2"}, 10 * time.Minute, "node prod-workers-2",
				"join"}},
		{"drained", rollout.Step{Action: rollout.WaitDrained, Group: "workers", Node: node},
			rollWait{NomadEvent{Action: NomadDrained, Node: "prod-workers-0"}, 15 * time.Minute, "node prod-workers-0",
				"finish draining"}},
		{"down", rollout.Step{Action: rollout.WaitNodeDown, Group: "workers", Node: node},
			rollWait{NomadEvent{Action: NomadDown, Node: "prod-workers-0", Address: "10.64.0.6"}, 6 * time.Minute,
				"node prod-workers-0 (10.64.0.6)", "go down"}},
		{"healthy", rollout.Step{Action: rollout.WaitHealthy, Group: "workers", Voters: 3},
			rollWait{NomadEvent{Action: NomadHealthy, Voters: 3}, 10 * time.Minute, "the servers", "become healthy and vote"}},
		{"a server voting", rollout.Step{Action: rollout.WaitJoined, Group: "servers", Machine: rollout.Machine{
			Name: "prod-servers-3", Role: v1alpha1.RoleServer}},
			rollWait{NomadEvent{Action: NomadVote, Node: "prod-servers-3"}, 10 * time.Minute, "node prod-servers-3", "vote"}},
		{"the window", rollout.Step{Action: rollout.WaitStable, Group: "servers", Until: stableUntil,
			Machine: rollout.Machine{Name: "prod-servers-0"}},
			rollWait{NomadEvent{Action: NomadStable, Until: stableUntil}, 5 * time.Minute, "the servers", "become stable"}},
		{"a stopped server", rollout.Step{Action: rollout.WaitServerDown, Group: "servers",
			Machine: rollout.Machine{Name: "prod-servers-0"}},
			rollWait{NomadEvent{Action: NomadServerDown, Node: "prod-servers-0"}, 5 * time.Minute, "autopilot",
				"stop counting prod-servers-0 as a healthy voter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.waitOf(tc.step)
			if err != nil {
				t.Fatalf("waitOf: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(rollWait{})); diff != "" {
				t.Errorf("the wait (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWaitOfFailsForAStepThatIsNoWait refuses to poll a step that has no limit, so that a wait added later cannot run
// for ever.
func TestWaitOfFailsForAStepThatIsNoWait(t *testing.T) {
	t.Parallel()
	r := &rollRun{}
	step := rollout.Step{Action: rollout.Stop, Machine: rollout.Machine{Name: "prod-servers-0", ID: "i-1"}}

	_, err := r.waitOf(step)

	if want := "no deadline for the wait " + step.String(); err == nil || err.Error() != want {
		t.Errorf("waitOf error = %v, want %q", err, want)
	}
}

// TestCarryFailsForAWait refuses a step that the loop polls and never carries out.
func TestCarryFailsForAWait(t *testing.T) {
	t.Parallel()
	r := &rollRun{}
	step := rollout.Step{Action: rollout.WaitStable, Machine: rollout.Machine{Name: "prod-servers-0", ID: "i-1"}}

	err := r.carry(t.Context(), step)

	if want := "no way to carry out the step " + step.String(); err == nil || err.Error() != want {
		t.Errorf("carry error = %v, want %q", err, want)
	}
}

// TestTryAgainOnlyAfterAGoneNodeOrNoAnswer tries a write again when the node is gone or no server answered, a create
// when it failed before it sent anything because no server answered, and nothing else.
func TestTryAgainOnlyAfterAGoneNodeOrNoAnswer(t *testing.T) {
	t.Parallel()
	notReady := fmt.Errorf("no server: %w", nomadops.ErrNotReady)
	gone := fmt.Errorf("no node: %w", nomadops.ErrGone)
	denied := errors.New("denied")
	mark, create := rollout.Step{Action: rollout.MarkIneligible}, rollout.Step{Action: rollout.Create}
	for _, tc := range []struct {
		name string
		step rollout.Step
		err  error
		want bool
	}{
		{"a write that no server answered", mark, notReady, true},
		{"a write on a node that is gone", mark, gone, true},
		{"a write that failed otherwise", mark, denied, false},
		{"a create that no server answered before it sent anything", create, notSentError{notReady}, true},
		{"a create that failed before it sent anything otherwise", create, notSentError{denied}, false},
		{"a create that failed after it sent its request", create, notReady, false},
		{"a delete", rollout.Step{Action: rollout.Delete}, notReady, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tryAgain(tc.step, tc.err); got != tc.want {
				t.Errorf("tryAgain = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShowingSaysWhatNomadListsOfTheWait says what Nomad lists of what each wait waits for.
func TestShowingSaysWhatNomadListsOfTheWait(t *testing.T) {
	t.Parallel()
	addr := netip.MustParseAddr("10.64.0.6")
	nodes := []nomadops.Node{
		{ID: "n-1", Name: "prod-workers-0", Address: addr, Status: "ready"},
		{ID: "n-2", Name: "prod-workers-1", Address: netip.MustParseAddr("10.64.0.7"), Status: "ready", Draining: true},
	}
	reading := nomadReading{nodes: nodes, health: nomadops.Health{Healthy: true, Voters: 3}}
	join := rollout.Step{Action: rollout.WaitJoined, Machine: rollout.Machine{Name: "prod-workers-0", PrivateIP: addr}}
	for _, tc := range []struct {
		name    string
		step    rollout.Step
		reading nomadReading
		want    string
	}{
		{"a node that joined", join, reading, "Nomad lists it ready"},
		{"a node at another address", rollout.Step{Action: rollout.WaitJoined, Machine: rollout.Machine{
			Name: "prod-workers-0", PrivateIP: netip.MustParseAddr("10.64.0.9")}}, reading,
			"Nomad lists no node of that name at its address"},
		{"a node that drains", rollout.Step{Action: rollout.WaitDrained, Node: rollout.Node{ID: "n-2"}}, reading,
			"Nomad lists it draining"},
		{"a node that is down", rollout.Step{Action: rollout.WaitNodeDown, Node: rollout.Node{ID: "n-1"}}, reading,
			"Nomad lists it ready"},
		{"a node that is gone", rollout.Step{Action: rollout.WaitDrained, Node: rollout.Node{ID: "n-9"}}, reading,
			"Nomad does not list it"},
		{"healthy servers", rollout.Step{Action: rollout.WaitHealthy}, reading,
			"autopilot reports 3 voters and the servers healthy"},
		{"servers that are not healthy", rollout.Step{Action: rollout.WaitHealthy},
			nomadReading{health: nomadops.Health{Voters: 2}}, "autopilot reports 2 voters and the servers not healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := showing(tc.step, tc.reading); got != tc.want {
				t.Errorf("showing = %q, want %q", got, tc.want)
			}
		})
	}
}

// readsNomad is a Nomad API whose reads fail while fail is true, as when no server answers.
type readsNomad struct {
	nomadops.API
	fail *bool
}

func (n readsNomad) Peers(context.Context) ([]nomadops.Peer, error) {
	if *n.fail {
		return nil, fmt.Errorf("peers: %w", nomadops.ErrNotReady)
	}
	return nil, nil
}

func (readsNomad) Health(context.Context) (nomadops.Health, error)    { return nomadops.Health{}, nil }
func (readsNomad) Members(context.Context) ([]nomadops.Member, error) { return nil, nil }
func (readsNomad) Nodes(context.Context) ([]nomadops.Node, error)     { return nil, nil }

// TestObserveCountsTheFailuresOfNomadInARow ends the run when the reads fail for ten minutes in a row, and counts again
// from the next failure after an answer.
func TestObserveCountsTheFailuresOfNomadInARow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fail := true
		r := &rollRun{api: readsNomad{fail: &fail}, rollLoop: newRollLoop()}
		observeFor := func(d time.Duration, when string) {
			t.Helper()
			for start := time.Now(); time.Since(start) < d; time.Sleep(rollPoll) {
				if _, ok, err := r.observe(t.Context()); ok || err != nil {
					t.Fatalf("%s: observe = %v, %v while the reads fail, want false, nil", when, ok, err)
				}
			}
		}

		observeFor(6*time.Minute, "the first six minutes")
		fail = false
		if _, ok, err := r.observe(t.Context()); !ok || err != nil {
			t.Fatalf("observe after an answer = %v, %v, want true, nil", ok, err)
		}
		fail = true
		observeFor(6*time.Minute, "the second six minutes, a new run of failures and not 12 minutes of one")
		time.Sleep(5 * time.Minute)
		_, ok, err := r.observe(t.Context())
		if ok || !errors.Is(err, nomadops.ErrNotReady) {
			t.Errorf("observe after 11 minutes of failures = %v, %v, want the error of the read", ok, err)
		}
	})
}

// TestMachinesListsThePendingOnesAfterTheListedOnes adds the machines that no list showed yet to those of the last
// list, by operation id.
func TestMachinesListsThePendingOnesAfterTheListedOnes(t *testing.T) {
	t.Parallel()
	r := &rollRun{rollLoop: newRollLoop(), listed: []cloud.Instance{{ID: "i-1", Name: "prod-workers-0"}}}
	r.pending["op-b"] = pendingMachine{in: cloud.Instance{ID: "i-3", Name: "prod-workers-3"}}
	r.pending["op-a"] = pendingMachine{in: cloud.Instance{ID: "i-2", Name: "prod-workers-2"}}

	got := r.machines()

	want := []cloud.Instance{{ID: "i-1", Name: "prod-workers-0"}, {ID: "i-2", Name: "prod-workers-2"},
		{ID: "i-3", Name: "prod-workers-3"}}
	if diff := cmp.Diff(want, got, equateNetip); diff != "" {
		t.Errorf("machines (-want +got):\n%s", diff)
	}
}

// listsNodes is a cloud that lists the same machines at every call.
type listsNodes struct {
	cloud.Nodes
	listed []cloud.Instance
}

func (n listsNodes) List(context.Context, string) ([]cloud.Instance, error) { return n.listed, nil }

// TestListDropsWhatTheCloudShows drops the pending machines that the list shows and the deleted machines that it does
// not, and keeps the others.
func TestListDropsWhatTheCloudShows(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		nodes := listsNodes{listed: []cloud.Instance{{ID: "i-2"}, {ID: "i-5"}}}
		r := &rollRun{kit: nodeKit{nodes: nodes}, rollLoop: newRollLoop()}
		r.pending["op-a"] = pendingMachine{in: cloud.Instance{ID: "i-2"}, since: time.Now()}
		r.pending["op-b"] = pendingMachine{in: cloud.Instance{ID: "i-3"}, since: time.Now()}
		r.deleting["i-5"] = time.Now()
		r.deleting["i-6"] = time.Now()
		r.relist = true

		if err := r.list(t.Context()); err != nil {
			t.Fatalf("list: %v", err)
		}

		if _, ok := r.pending["op-a"]; ok {
			t.Error("the machine that the list shows is still pending")
		}
		if _, ok := r.pending["op-b"]; !ok {
			t.Error("the machine that the list misses is not pending any more")
		}
		if _, ok := r.deleting["i-5"]; !ok {
			t.Error("the machine that the list still shows is not among the deleted ones any more")
		}
		if _, ok := r.deleting["i-6"]; ok {
			t.Error("the machine that the list does not show is still among the deleted ones")
		}
		if r.relist {
			t.Error("the next observation lists again")
		}
		if len(r.listed) != 2 {
			t.Errorf("the run holds %d machines of the list, want 2", len(r.listed))
		}
	})
}

// TestListFailsForAMachineThatItCreatedAndThatTheListsMissForAMinute names the machine that no list showed within the
// pending time, the first of them by operation id.
func TestListFailsForAMachineThatItCreatedAndThatTheListsMissForAMinute(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := &rollRun{kit: nodeKit{nodes: listsNodes{}}, rollLoop: newRollLoop()}
		r.pending["op-b"] = pendingMachine{in: cloud.Instance{ID: "i-3", Name: "prod-workers-3"}, since: time.Now()}
		r.pending["op-a"] = pendingMachine{in: cloud.Instance{ID: "i-2", Name: "prod-workers-2"}, since: time.Now()}
		time.Sleep(pendingTimeout - time.Second)
		if err := r.list(t.Context()); err != nil {
			t.Fatalf("list after a little less than a minute: %v", err)
		}
		time.Sleep(time.Second)

		want := "the cloud does not list node prod-workers-2 (ID i-2), which this run created"
		for range 30 { // map order is random: a list that does not sort fails one of these
			if err := r.list(t.Context()); err == nil || err.Error() != want {
				t.Fatalf("list after a minute = %v, want %q", err, want)
			}
		}
	})
}

// TestRepeatCreateCarriesTheGroupsHashAndTheMachinesOperationId repeats the create of a machine that is not ready with
// its operation id and with the hash of its group, which a create makes the machine with when the cloud lacks it, and
// lists the machines at the next observation.
func TestRepeatCreateCarriesTheGroupsHashAndTheMachinesOperationId(t *testing.T) {
	t.Parallel()
	const op = "4f6a2d5e-8c3b-4d1e-9a7f-0b2c3d4e5f60"
	var steps []string
	nodes := &recordingNodes{}
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer},
		{Name: "workers", Role: v1alpha1.RoleClient, MachineType: "vc2-4c-8gb", Image: "ubuntu-24.04"},
	}}
	r := &rollRun{
		s: testService(&steps), kit: testKit(t, nodes), model: m, api: &introStub{}, rollLoop: newRollLoop(),
		groups: []rollout.Group{{Name: "workers", SpecHash: "group-hash"}},
		listed: []cloud.Instance{{Name: "prod-servers-0", Group: "servers", PrivateIP: netip.MustParseAddr("10.64.0.3")}},
	}
	in := cloud.Instance{
		ID: "i-9", Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams", Op: op,
	}

	if err := r.repeatCreate(t.Context(), in); err != nil {
		t.Fatalf("repeatCreate: %v", err)
	}

	if len(nodes.creates) != 1 {
		t.Fatalf("the cloud was asked for %d machines, want 1", len(nodes.creates))
	}
	got := nodes.creates[0]
	if got.Op != op || got.SpecHash != "group-hash" || got.MachineType != "vc2-4c-8gb" || got.Image != "ubuntu-24.04" ||
		got.Name != in.Name || got.Zone != "ams" || got.Role != v1alpha1.RoleClient {
		t.Errorf("the create request is %+v, want the machine's operation id, name and zone and the group's hash, "+
			"machine type and image", got)
	}
	if !r.relist {
		t.Error("the next observation does not list the machines")
	}
}

// TestScrubMarksOnlyThePendingCopyOfItsMachine labels the machine and replaces its user data once, reports the wait as
// done, makes the pending copy of that machine joined and no other, and lists at the next observation.
func TestScrubMarksOnlyThePendingCopyOfItsMachine(t *testing.T) {
	t.Parallel()
	var steps []string
	nodes := &recordingNodes{}
	svc := &Service{Now: func() time.Time { return testNow }, OnProgress: func(p Progress) {
		if p.Nomad != nil {
			steps = append(steps, "nomad "+p.Nomad.Action.String()+" "+p.Step.String())
		} else {
			steps = append(steps, p.Node.Action.String()+" "+p.Node.Name+" "+p.Step.String())
		}
	}}
	r := &rollRun{s: svc, kit: testKit(t, nodes), rollLoop: newRollLoop()}
	a, b := cloud.Instance{ID: "i-2", Name: "prod-workers-2"}, cloud.Instance{ID: "i-3", Name: "prod-workers-3"}
	r.pending["op-a"], r.pending["op-b"] = pendingMachine{in: a}, pendingMachine{in: b}
	r.open = &openWait{event: NomadEvent{Action: NomadRegister, Node: a.Name}}

	if err := r.scrub(t.Context(), a); err != nil {
		t.Fatalf("scrub: %v", err)
	}

	if len(nodes.joined) != 1 || nodes.joined[0].ID != a.ID {
		t.Errorf("the cloud was asked to label %+v, want only %s", nodes.joined, a.ID)
	}
	if !r.pending["op-a"].in.Joined || r.pending["op-b"].in.Joined {
		t.Errorf("the pending copies are joined %v and %v, want true and false", r.pending["op-a"].in.Joined,
			r.pending["op-b"].in.Joined)
	}
	if !r.relist {
		t.Error("the next observation does not list the machines")
	}
	if r.open != nil {
		t.Error("the wait for the node is still open")
	}
	want := []string{"nomad register done", "scrub prod-workers-2 started", "scrub prod-workers-2 done"}
	if diff := cmp.Diff(want, steps); diff != "" {
		t.Errorf("the progress (-want +got):\n%s", diff)
	}
}

// TestRollRunSeedHoldsTheServersByName seeds a new node with the private addresses of the listed servers and combined
// nodes, by name, without the clients.
func TestRollRunSeedHoldsTheServersByName(t *testing.T) {
	t.Parallel()
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer}, {Name: "all", Role: v1alpha1.RoleCombined},
		{Name: "workers", Role: v1alpha1.RoleClient},
	}}
	addr := netip.MustParseAddr
	r := &rollRun{model: m, kit: nodeKit{cluster: "prod"}, listed: []cloud.Instance{
		{Name: "prod-workers-0", Group: "workers", PrivateIP: addr("10.64.0.9")},
		{Name: "prod-servers-1", Group: "servers", PrivateIP: addr("10.64.0.4")},
		{Name: "prod-all-0", Group: "all", PrivateIP: addr("10.64.0.2")},
		{Name: "prod-servers-0", Group: "servers", PrivateIP: addr("10.64.0.3")},
	}}

	got, err := r.seed("prod-workers-2", true)

	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := []netip.Addr{addr("10.64.0.2"), addr("10.64.0.3"), addr("10.64.0.4")}
	if diff := cmp.Diff(want, got, equateNetip); diff != "" {
		t.Errorf("seed (-want +got):\n%s", diff)
	}
}

// TestSeedOfFailsForServersWithoutAddresses names the servers that have no private address, and has an empty seed for a
// server that is the first.
func TestSeedOfFailsForServersWithoutAddresses(t *testing.T) {
	t.Parallel()
	servers := []cloud.Instance{
		{Name: "prod-servers-0", PrivateIP: netip.MustParseAddr("10.64.0.3")}, {Name: "prod-servers-1"},
	}
	for _, tc := range []struct {
		name    string
		servers []cloud.Instance
		node    string
		client  bool
		want    string
	}{
		{"a seed with one address", servers, "prod-workers-0", true, ""},
		{"no address at all", servers[1:], "prod-workers-0", true,
			"node prod-workers-0: no server of cluster prod has a private address yet (prod-servers-1); " +
				"run the command again"},
		{"a client and no server", nil, "prod-workers-0", true,
			"node prod-workers-0: no server of cluster prod has a private address yet; run the command again"},
		{"the first server", nil, "prod-servers-0", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := seedOf("prod", tc.servers, tc.node, tc.client)
			if got := fmt.Sprint(err); (tc.want == "") != (err == nil) || err != nil && got != tc.want {
				t.Errorf("seedOf error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestOpenWaitIsOverWhenTheReadingShowsWhatItWaitsFor ends each wait only when Nomad or the cloud show its end, not
// when the decisions give another step.
func TestOpenWaitIsOverWhenTheReadingShowsWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	m := rollout.Machine{ID: "i-1", Name: "prod-workers-0"}
	node := rollout.Node{ID: "n-1"}
	drain := func(status, meta string) nomadops.Node {
		return nomadops.Node{ID: "n-1", Status: "ready", Draining: status == "draining",
			LastDrain: nomadops.LastDrain{Status: status, Meta: map[string]string{drainMeta: meta}}}
	}
	one := func(n nomadops.Node) nomadReading { return nomadReading{nodes: []nomadops.Node{n}} }
	servers := func(versions ...string) nomadReading {
		var r nomadReading
		for i, v := range versions {
			id := fmt.Sprint("s-", i)
			r.peers = append(r.peers, nomadops.Peer{ID: id})
			r.health.Servers = append(r.health.Servers, nomadops.ServerHealth{ID: id, Version: v})
		}
		return r
	}
	for _, tc := range []struct {
		name     string
		step     rollout.Step
		joined   bool
		reading  nomadReading
		wantOver bool
	}{
		{"a machine that has not joined", rollout.Step{Action: rollout.WaitJoined, Machine: m}, false, nomadReading{}, false},
		{"a machine that joined", rollout.Step{Action: rollout.WaitJoined, Machine: m}, true, nomadReading{}, true},
		{"a node that still drains", rollout.Step{Action: rollout.WaitDrained, Machine: m, Node: node}, false,
			one(drain("draining", "i-1")), false},
		{"a node whose drain is complete", rollout.Step{Action: rollout.WaitDrained, Machine: m, Node: node}, false,
			one(drain("complete", "i-1")), true},
		{"a drain complete for another machine", rollout.Step{Action: rollout.WaitDrained, Machine: m, Node: node}, false,
			one(drain("complete", "i-9")), false},
		{"a draining node that went down", rollout.Step{Action: rollout.WaitDrained, Machine: m, Node: node}, false,
			one(nomadops.Node{ID: "n-1", Status: "down", Draining: true}), true},
		{"a draining node that is not listed", rollout.Step{Action: rollout.WaitDrained, Machine: m, Node: node}, false,
			nomadReading{}, true},
		{"a node that is ready", rollout.Step{Action: rollout.WaitNodeDown, Node: node}, false,
			one(nomadops.Node{ID: "n-1", Status: "ready"}), false},
		{"a node that is down", rollout.Step{Action: rollout.WaitNodeDown, Node: node}, false,
			one(nomadops.Node{ID: "n-1", Status: "down"}), true},
		{"a node that is not listed", rollout.Step{Action: rollout.WaitNodeDown, Node: node}, false, nomadReading{}, true},
		{"a server without a version", rollout.Step{Action: rollout.WaitHealthy}, false, servers("2.0.7", ""), false},
		{"servers that all report a version", rollout.Step{Action: rollout.WaitHealthy}, false,
			servers("2.0.7", "2.0.7"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rollRun{rollLoop: newRollLoop(), listed: []cloud.Instance{{ID: "i-1", Joined: tc.joined}}}
			w := openWait{step: tc.step}
			if got := w.over(r, tc.reading); got != tc.wantOver {
				t.Errorf("over = %v, want %v", got, tc.wantOver)
			}
		})
	}
}

// TestCreateChangeCarriesTheGroupsHashAndANewOperationID makes the create of a step from the machine, the group's plan
// and image, and the group's spec hash, with an operation id of its own each time.
func TestCreateChangeCarriesTheGroupsHashAndANewOperationID(t *testing.T) {
	t.Parallel()
	r := &rollRun{
		model: &model.Cluster{Groups: []model.NodeGroup{{Name: "workers", MachineType: "vc2-4c-8gb", Image: "ubuntu-24.04"}}},
	}
	r.groups = []rollout.Group{{Name: "workers", SpecHash: "group-hash"}}
	step := rollout.Step{Action: rollout.Create, Machine: rollout.Machine{
		Name: "prod-workers-2", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
	}}
	want := NodeChange{
		Action: NodeCreate, Name: "prod-workers-2", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
		MachineType: "vc2-4c-8gb", Image: "ubuntu-24.04", SpecHash: "group-hash",
	}

	first, second := r.createChange(step), r.createChange(step)

	for _, got := range []NodeChange{first, second} {
		if !cloud.ValidOpID(got.Op) {
			t.Errorf("Op = %q, want an operation id", got.Op)
		}
		got.Op = ""
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("createChange (-want +got):\n%s", diff)
		}
	}
	if first.Op == second.Op {
		t.Errorf("two creates have the operation id %q", first.Op)
	}
}

// TestPollFailsTheWaitThatRanOutAndNotTheOpenOne ends a wait whose limit passed while the wait for a node to join is
// the open one, and reports the failure on the wait that ran out. It reports no start for it.
func TestPollFailsTheWaitThatRanOutAndNotTheOpenOne(t *testing.T) {
	t.Parallel()
	var got []Progress
	r := &rollRun{s: &Service{OnProgress: func(p Progress) { got = append(got, p) }}, rollLoop: newRollLoop()}
	joining := rollout.Step{Action: rollout.WaitJoined, Machine: rollout.Machine{ID: "i-3", Name: "prod-workers-3"}}
	healthy := rollout.Step{Action: rollout.WaitHealthy, Group: "workers", Voters: 3}
	joined := NomadEvent{Action: NomadRegister, Node: "prod-workers-3"}
	r.open = &openWait{key: keyOf(joining), step: joining, event: joined}
	r.seen[keyOf(healthy)] = seenWait{step: healthy, since: time.Now().Add(-nomadTimeout)}

	err := r.poll(t.Context(), healthy, nomadReading{})
	r.endWait(err)

	if err == nil {
		t.Fatal("poll succeeded, want the error of the wait that ran out")
	}
	if len(got) != 1 || got[0].Step != NodeFailed || got[0].Nomad == nil || got[0].Nomad.Action != NomadHealthy ||
		!errors.Is(got[0].Err, err) {
		t.Errorf("the progress is %+v, want one failure of the wait for the servers with the error of the poll", got)
	}
}

// TestRepeatCreateCountsOnlyAMachineThatItMakes counts the machine of a repeated create when the cloud made a new one,
// and not when the cloud found the machine that the create names.
func TestRepeatCreateCountsOnlyAMachineThatItMakes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		id   string // the ID of the machine that the repeated create names; the cloud's create answers with id-1
		want []string
	}{
		{"the cloud has no machine with the operation id", "i-9", []string{"id-1"}},
		{"the cloud found the machine", "id-1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var steps []string
			m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
				{Name: "servers", Role: v1alpha1.RoleServer}, {Name: "workers", Role: v1alpha1.RoleClient},
			}}
			r := &rollRun{
				s: testService(&steps), kit: testKit(t, &recordingNodes{}), model: m, api: &introStub{}, rollLoop: newRollLoop(),
				groups: []rollout.Group{{Name: "workers"}},
				listed: []cloud.Instance{{Name: "prod-servers-0", Group: "servers", PrivateIP: netip.MustParseAddr("10.64.0.3")}},
			}
			in := cloud.Instance{
				ID: tc.id, Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
				Op: "4f6a2d5e-8c3b-4d1e-9a7f-0b2c3d4e5f60",
			}

			if err := r.repeatCreate(t.Context(), in); err != nil {
				t.Fatalf("repeatCreate: %v", err)
			}

			if p, ok := r.pending[in.Op]; ok != (tc.want != nil) || ok && p.in.ID != "id-1" {
				t.Errorf("r.pending[%q] = %+v (held %t), want the machine id-1 only when the create made it", in.Op, p, ok)
			}
			got := slices.Sorted(maps.Keys(r.rolled.created))
			if !slices.Equal(got, tc.want) {
				t.Errorf("the roll counts the created machines %q, want %q", got, tc.want)
			}
		})
	}
}

// serversReading is the reading of a Nomad whose servers are as the entries say, by Raft ID.
func serversReading(healthy bool, entries ...serverEntry) nomadReading {
	r := nomadReading{health: nomadops.Health{Healthy: healthy}}
	for _, e := range entries {
		addr := netip.AddrPortFrom(netip.MustParseAddr(e.address), 4647)
		r.peers = append(r.peers, nomadops.Peer{ID: e.id, Address: addr, Voter: e.voter})
		r.health.Servers = append(r.health.Servers, nomadops.ServerHealth{ID: e.id, Healthy: e.healthy})
		if e.voter {
			r.health.Voters++
		}
	}
	return r
}

// serverEntry is a server of a reading: its Raft ID, private address, vote and health.
type serverEntry struct {
	id, address    string
	voter, healthy bool
}

// TestShowingSaysWhatNomadListsOfAServerWait says what the Raft configuration and autopilot show of the server that a
// wait for a vote, for a stopped server and for the window waits on.
func TestShowingSaysWhatNomadListsOfAServerWait(t *testing.T) {
	t.Parallel()
	machine := rollout.Machine{
		Name: "prod-servers-3", Role: v1alpha1.RoleServer, PrivateIP: netip.MustParseAddr("10.64.0.6"),
	}
	join := rollout.Step{Action: rollout.WaitJoined, Machine: machine}
	down := rollout.Step{Action: rollout.WaitServerDown, Machine: machine}
	stable := rollout.Step{Action: rollout.WaitStable, Machine: machine, Until: stableUntil}
	at := func(voter, healthy bool) nomadReading {
		return serversReading(healthy, serverEntry{"s-3", "10.64.0.6", voter, healthy},
			serverEntry{"s-0", "10.64.0.3", true, true})
	}
	for _, tc := range []struct {
		name    string
		step    rollout.Step
		reading nomadReading
		want    string
	}{
		{"a vote that no server at the address has", join, serversReading(true, serverEntry{"s-0", "10.64.0.3", true, true}),
			"the Raft configuration lists no server at its address"},
		{"a vote of a server that has none yet", join, at(false, true), "its server does not vote yet"},
		{"a vote of a server that autopilot counts unhealthy", join, at(true, false),
			"autopilot does not count its server healthy"},
		{"a stopped server that autopilot counts a healthy voter", down, at(true, true),
			"autopilot counts it a healthy voter"},
		{"a stopped server that autopilot counts unhealthy", down, at(true, false),
			"autopilot does not count it a healthy voter"},
		{"a stopped server that is no voter", down, at(false, true), "autopilot does not count it a healthy voter"},
		{"a stopped server that has no peer", down, serversReading(true), "autopilot does not count it a healthy voter"},
		{"the window", stable, at(true, true), "the window ends at 12:04:20"},
		{"a vote of a machine without an address", rollout.Step{Action: rollout.WaitJoined, Machine: rollout.Machine{
			Name: "prod-servers-3", Role: v1alpha1.RoleServer}}, nomadReading{peers: []nomadops.Peer{{ID: "s-9"}}},
			"the Raft configuration lists no server at its address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := showing(tc.step, tc.reading); got != tc.want {
				t.Errorf("showing = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpenWaitOfAServerGroupIsOverWhenTheReadingShowsItsEnd ends the wait for a stopped server when autopilot no longer
// counts it a healthy voter, the wait for the servers of a server group when they are healthy and the group's voters
// vote, and the wait for the window as soon as the decisions give another step.
func TestOpenWaitOfAServerGroupIsOverWhenTheReadingShowsItsEnd(t *testing.T) {
	t.Parallel()
	victim := rollout.Machine{ID: "i-0", Name: "prod-servers-0", PrivateIP: netip.MustParseAddr("10.64.0.3")}
	servers := func(healthy bool, victimVoter, victimHealthy bool) nomadReading {
		return serversReading(healthy,
			serverEntry{"s-0", "10.64.0.3", victimVoter, victimHealthy},
			serverEntry{"s-1", "10.64.0.4", true, true}, serverEntry{"s-2", "10.64.0.5", true, true})
	}
	down := rollout.Step{Action: rollout.WaitServerDown, Group: "servers", Machine: victim}
	healthy := rollout.Step{Action: rollout.WaitHealthy, Group: "servers", Machine: victim, Voters: 2}
	for _, tc := range []struct {
		name     string
		step     rollout.Step
		reading  nomadReading
		wantOver bool
	}{
		{"a stopped server that is a healthy voter", down, servers(true, true, true), false},
		{"a stopped server that autopilot counts unhealthy", down, servers(false, true, false), true},
		{"a stopped server that is no voter", down, servers(true, false, true), true},
		{"a stopped server whose peer is gone", down,
			serversReading(true, serverEntry{"s-1", "10.64.0.4", true, true}), true},
		{"the servers of a group, unhealthy", healthy, servers(false, false, false), false},
		{"the servers of a group, with a voter too many", healthy, servers(true, true, true), false},
		{"the servers of a group, with a voter too few", healthy,
			serversReading(true, serverEntry{"s-1", "10.64.0.4", true, true}), false},
		{"the servers of a group that are healthy and vote", healthy, serversReading(true,
			serverEntry{"s-1", "10.64.0.4", true, true}, serverEntry{"s-2", "10.64.0.5", true, true}), true},
		{"the window", rollout.Step{Action: rollout.WaitStable, Group: "servers", Machine: victim},
			servers(true, true, true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &rollRun{rollLoop: newRollLoop(), groups: []rollout.Group{{Name: "servers", Role: v1alpha1.RoleServer}}}
			if got := (openWait{step: tc.step}).over(r, tc.reading); got != tc.wantOver {
				t.Errorf("over = %v, want %v", got, tc.wantOver)
			}
		})
	}
}

// TestRefuseRoleRefusesCombinedGroupsAndServerGroupsOnlyForThePlan refuses a combined group whatever the caller, and a
// server group only when the caller says that servers are refused too; a client group never.
func TestRefuseRoleRefusesCombinedGroupsAndServerGroupsOnlyForThePlan(t *testing.T) {
	t.Parallel()
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer}, {Name: "all", Role: v1alpha1.RoleCombined},
		{Name: "workers", Role: v1alpha1.RoleClient},
	}}
	withClients := &rollRun{model: m}
	serversOnly := &rollRun{model: &model.Cluster{Name: "prod", Groups: m.Groups[:2]}}
	step := func(group string) rollout.Step { return rollout.Step{Action: rollout.Create, Group: group} }
	for _, tc := range []struct {
		name    string
		r       *rollRun
		group   string
		servers bool
		want    string
	}{
		{"a combined group", withClients, "all", false,
			"node group all: tent cannot roll combined groups yet; select client groups with --nodegroups"},
		{"a combined group of a cluster without clients", serversOnly, "all", false,
			"node group all: tent cannot roll combined groups yet"},
		{"a server group", withClients, "servers", false, ""},
		{"a client group", withClients, "workers", false, ""},
		{"a combined group for the plan", withClients, "all", true,
			"node group all: tent cannot roll server and combined groups yet; select client groups with --nodegroups"},
		{"a server group for the plan", withClients, "servers", true,
			"node group servers: tent cannot roll server and combined groups yet; select client groups with --nodegroups"},
		{"a server group for the plan, without clients", serversOnly, "servers", true,
			"node group servers: tent cannot roll server and combined groups yet"},
		{"a client group for the plan", withClients, "workers", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.r.refuseRole(step(tc.group), tc.servers)
			if (err == nil) != (tc.want == "") || err != nil && err.Error() != tc.want {
				t.Errorf("refuseRole error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestNeedsListOnlyForAStopOrATransferDecidedWithoutAList wants a list for a stop and for a transfer of the leadership
// that were decided on an observation that did not list the machines, and for no other step or observation.
func TestNeedsListOnlyForAStopOrATransferDecidedWithoutAList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action  rollout.Action
		listing bool
		want    bool
	}{
		{rollout.Stop, false, true}, {rollout.TransferLeadership, false, true},
		{rollout.Stop, true, false}, {rollout.TransferLeadership, true, false},
		{rollout.Create, false, false}, {rollout.Delete, false, false}, {rollout.RemovePeer, false, false},
		{rollout.WaitStable, false, false},
	} {
		t.Run(fmt.Sprintf("%v listing %v", tc.action, tc.listing), func(t *testing.T) {
			t.Parallel()
			r := &rollRun{rollLoop: newRollLoop()}
			r.listing = tc.listing
			if got := r.needsList(rollout.Step{Action: tc.action}); got != tc.want {
				t.Errorf("needsList = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestObserveNotesWhetherItListed notes that it listed when the last step changed the machines, and that it did not
// when nothing asked for a list.
func TestObserveNotesWhetherItListed(t *testing.T) {
	t.Parallel()
	fail := false
	r := &rollRun{api: readsNomad{fail: &fail}, rollLoop: newRollLoop(), kit: nodeKit{nodes: listsNodes{}}}
	r.relist = true

	if _, ok, err := r.observe(t.Context()); !ok || err != nil || !r.listing {
		t.Fatalf("observe = %v, %v, listing %v; want an observation that listed", ok, err, r.listing)
	}
	if _, ok, err := r.observe(t.Context()); !ok || err != nil || r.listing {
		t.Errorf("observe = %v, %v, listing %v; want an observation that did not list", ok, err, r.listing)
	}
}

// TestCreateAndRepeatCreateBootAServerWithoutAnIntroTokenAndAClientWithOne asks the servers for an intro token only
// when the machine is a client, whether the loop creates the machine or repeats its create.
func TestCreateAndRepeatCreateBootAServerWithoutAnIntroTokenAndAClientWithOne(t *testing.T) {
	t.Parallel()
	const op = "4f6a2d5e-8c3b-4d1e-9a7f-0b2c3d4e5f60"
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer, MachineType: serverType, Image: testImage},
		{Name: "workers", Role: v1alpha1.RoleClient, MachineType: clientType, Image: testImage},
	}}
	for _, tc := range []struct {
		name   string
		group  string
		role   v1alpha1.Role
		tokens int
	}{
		{"a server", "servers", v1alpha1.RoleServer, 0},
		{"a client", "workers", v1alpha1.RoleClient, 1},
	} {
		machine := cloud.Instance{
			ID: "i-9", Name: "prod-" + tc.group + "-1", Group: tc.group, Role: tc.role, Zone: "ams", Op: op,
		}
		for _, b := range []struct {
			how  string
			boot func(*testing.T, *rollRun) error
		}{
			{"create", func(t *testing.T, r *rollRun) error {
				return r.create(t.Context(), rollout.Step{Action: rollout.Create, Group: tc.group, Machine: rollout.Machine{
					Name: machine.Name, Group: tc.group, Role: tc.role, Zone: "ams"}})
			}},
			{"repeat", func(t *testing.T, r *rollRun) error { return r.repeatCreate(t.Context(), machine) }},
		} {
			t.Run(tc.name+" "+b.how, func(t *testing.T) {
				t.Parallel()
				var steps []string
				nodes, intro := &recordingNodes{}, &introStub{}
				r := &rollRun{
					s: testService(&steps), kit: testKit(t, nodes), model: m, api: intro, rollLoop: newRollLoop(),
					groups: []rollout.Group{{Name: tc.group}},
					listed: []cloud.Instance{{
						Name: "prod-servers-0", Group: "servers", Role: v1alpha1.RoleServer, PrivateIP: netip.MustParseAddr("10.64.0.3"),
					}},
				}

				if err := b.boot(t, r); err != nil {
					t.Fatalf("%s: %v", b.how, err)
				}

				if len(nodes.creates) != 1 || nodes.creates[0].Role != tc.role {
					t.Errorf("the cloud was asked for %+v, want one machine of the role %s", nodes.creates, tc.role)
				}
				if len(intro.requests) != tc.tokens {
					t.Errorf("the servers were asked for %d intro tokens, want %d", len(intro.requests), tc.tokens)
				}
			})
		}
	}
}

// TestJoinPollScrubsAServerThatVotesAndNotACombinedMachine scrubs a server once a healthy voter runs at its address,
// and a combined machine not before its node has registered.
func TestJoinPollScrubsAServerThatVotesAndNotACombinedMachine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role      v1alpha1.Role
		wantScrub int
	}{{v1alpha1.RoleServer, 1}, {v1alpha1.RoleCombined, 0}} {
		t.Run(string(tc.role), func(t *testing.T) {
			t.Parallel()
			var steps []string
			nodes := &recordingNodes{}
			in := cloud.Instance{
				ID: "i-3", Name: "prod-servers-3", Role: tc.role, Ready: true, PrivateIP: netip.MustParseAddr("10.64.0.6"),
			}
			r := &rollRun{s: testService(&steps), kit: testKit(t, nodes), rollLoop: newRollLoop(), listed: []cloud.Instance{in}}
			step := rollout.Step{Action: rollout.WaitJoined, Machine: rollout.Machine{ID: in.ID, Name: in.Name, Role: tc.role}}
			reading := serversReading(true, serverEntry{"s-3", "10.64.0.6", true, true})

			if err := r.joinPoll(t.Context(), step, reading); err != nil {
				t.Fatalf("joinPoll: %v", err)
			}

			if len(nodes.joined) != tc.wantScrub {
				t.Errorf("the cloud was asked to label %d machines, want %d", len(nodes.joined), tc.wantScrub)
			}
		})
	}
}

// TestStopStopsTheMachineAndListsAtTheNextObservation sends the stop to the cloud and then lists the machines at the
// next observation; a stop that fails returns the cloud's error and leaves the list as it is.
func TestStopStopsTheMachineAndListsAtTheNextObservation(t *testing.T) {
	t.Parallel()
	errCloud := errors.New("the cloud refused")
	for _, tc := range []struct {
		name      string
		cloudErr  error
		wantList  bool
		wantAsked int
	}{{"the cloud stops it", nil, true, 1}, {"the cloud refuses", errCloud, false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var steps []string
			nodes := &stoppingNodes{err: tc.cloudErr}
			r := &rollRun{s: testService(&steps), kit: nodeKit{cluster: "prod", nodes: nodes}, rollLoop: newRollLoop()}

			err := r.stop(t.Context(), rollout.Machine{ID: "i-1", Name: "prod-servers-0"})

			if !errors.Is(err, tc.cloudErr) {
				t.Errorf("stop error = %v, want %v", err, tc.cloudErr)
			}
			if len(nodes.stopped) != tc.wantAsked || r.relist != tc.wantList {
				t.Errorf("the cloud was asked to stop %d machines and the run lists next: %v; want %d and %v",
					len(nodes.stopped), r.relist, tc.wantAsked, tc.wantList)
			}
		})
	}
}
