package app_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// serverGroupSize is the size of the test cluster's server group.
const serverGroupSize = 3

// stopWindow is how long the servers must have been unchanged before the roll stops one: the window of a minute and
// ten seconds, less the second that the whole seconds of StableSince can take off.
const stopWindow = time.Minute + 10*time.Second - time.Second

// serverView is what the invariants of a roll of servers look at in the cluster at one call.
type serverView struct {
	machines   int    // the machines of the server group that the cloud lists
	voters     []bool // for each voter in the Raft configuration, whether its machine runs
	leader     string // the instance ID of the server that leads; empty when none does
	leaderRuns bool   // the leader's machine is listed and not halted
}

// broken returns what the view breaks of the invariants of a group of the given size: the group has at most one
// machine beyond its size, the cluster has a leader whose machine runs, and the voters that run are a quorum.
func (v serverView) broken(size int) []string {
	var out []string
	if v.machines > size+1 {
		out = append(out, fmt.Sprintf("the server group has %d machines, want at most %d", v.machines, size+1))
	}
	if !v.leaderRuns {
		out = append(out, fmt.Sprintf("the cluster has the leader %q, want one whose machine runs", v.leader))
	}
	live := 0
	for _, runs := range v.voters {
		if runs {
			live++
		}
	}
	if quorum := len(v.voters)/2 + 1; live < quorum {
		out = append(out, fmt.Sprintf("%d of %d voters run, want at least %d", live, len(v.voters), quorum))
	}
	return out
}

// raftVoters returns the instance IDs of the servers that vote in the Raft configuration as the world last saw it,
// the instance ID of the leader, and when the last server joined as a nonvoter, which is zero if none did.
func (w *nomadWorld) raftVoters() (voters []string, leader string, joined time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for _, r := range w.raft.servers {
		if !r.removed && r.votes(now, w.delays) {
			voters = append(voters, r.id)
		}
		if !r.bootstrap && r.joined.After(joined) {
			joined = r.joined
		}
	}
	return voters, w.raft.leader, joined
}

// nomadAt is a call to Nomad and when it was made.
type nomadAt struct {
	name string
	at   time.Time
}

// serverInvariants checks, before every call to Vultr and to Nomad, what a roll of servers keeps (serverView.broken),
// and that the roll stops a server only once the servers have not changed for stopWindow: a change is a server that
// joins the Raft configuration as a nonvoter, or a transfer of the leadership.
type serverInvariants struct {
	tb testing.TB
	f  *vultrfake.Fake
	w  *nomadWorld

	mu          sync.Mutex
	outside     map[string]bool // the machines that the test halted itself, by ID
	transferred time.Time       // when the last transfer of the leadership was carried out
	nomad       []nomadAt       // the calls to Nomad
	checked     int             // how many calls were checked
	cutBefore   int             // how many checked calls a cut ended the context before, so that no fake logged them
	vultr, seen int             // how many calls had reached the Vultr fake and Nomad when the watch began
}

// watchServers makes every call to f and to w check the invariants before it runs, and takes the hooks of both.
func watchServers(tb testing.TB, f *vultrfake.Fake, w *nomadWorld) *serverInvariants {
	tb.Helper()
	inv := &serverInvariants{
		tb: tb, f: f, w: w, outside: map[string]bool{}, vultr: len(f.Calls()), seen: len(w.Log()),
	}
	inv.watch(nil, nil)
	return inv
}

// watch makes every call to the Vultr fake and to Nomad check the invariants before it runs and then go through the
// hook for it, which a nil hook leaves out. A transfer of the leadership that the fake carried out counts as one
// whatever the hook makes of its answer. It takes the hooks of both fakes.
func (inv *serverInvariants) watch(onVultr vultrfake.Hook, onNomad nomadHook) {
	inv.f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		inv.check(ctx, "Vultr "+c.Name+" "+c.Arg)
		if c.Name == "HaltInstance" {
			inv.checkStop(c.Arg)
		}
		if onVultr == nil {
			return next(ctx)
		}
		return onVultr(ctx, c, next)
	})
	inv.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		inv.check(ctx, "Nomad "+c.Name+" "+c.Arg)
		inv.mu.Lock()
		inv.nomad = append(inv.nomad, nomadAt{c.Name, time.Now()})
		inv.mu.Unlock()
		carried := func(ctx context.Context) error {
			err := next(ctx)
			if c.Name == "TransferLeadership" && err == nil {
				inv.mu.Lock()
				inv.transferred = time.Now()
				inv.mu.Unlock()
			}
			return err
		}
		if onNomad == nil {
			return carried(ctx)
		}
		return onNomad(ctx, c, carried)
	})
}

// cutBeforeFake notes that a cut ended the context of the call that the hooks checked last, before it reached the
// fake, which logged nothing of it.
func (inv *serverInvariants) cutBeforeFake() {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.cutBefore++
}

// halt halts the machine id as a fault would, outside the roll: the roll's invariants about its own stops leave it be.
func (inv *serverInvariants) halt(id string) {
	inv.mu.Lock()
	inv.outside[id] = true
	inv.mu.Unlock()
	if err := inv.f.HaltInstance(context.Background(), id); err != nil {
		inv.tb.Errorf("HaltInstance: %v", err)
	}
}

// view returns the cluster as it is now.
func (inv *serverInvariants) view() serverView {
	var v serverView
	for _, in := range inv.f.Instances() {
		if strings.HasPrefix(in.Hostname, "prod-servers-") {
			v.machines++
		}
	}
	voters, leader, _ := inv.w.raftVoters()
	runs := func(id string) bool { return hasInstance(inv.f, id) && !inv.f.Halted(id) }
	for _, id := range voters {
		v.voters = append(v.voters, runs(id))
	}
	v.leader, v.leaderRuns = leader, leader != "" && runs(leader)
	return v
}

// check fails the test for each invariant that the cluster breaks before the call. A call whose context has ended is
// checked and not counted, since the fakes log no such call.
func (inv *serverInvariants) check(ctx context.Context, call string) {
	inv.tb.Helper()
	if ctx.Err() == nil {
		inv.mu.Lock()
		inv.checked++
		inv.mu.Unlock()
	}
	for _, msg := range inv.view().broken(serverGroupSize) {
		inv.tb.Errorf("before %s: %s", strings.TrimSpace(call), msg)
	}
}

// checkStop fails the test when the roll halts the machine id less than stopWindow after the servers last changed.
func (inv *serverInvariants) checkStop(id string) {
	inv.tb.Helper()
	inv.mu.Lock()
	outside, transferred := inv.outside[id], inv.transferred
	inv.mu.Unlock()
	if outside {
		return
	}
	_, _, joined := inv.w.raftVoters()
	changed := joined
	if transferred.After(changed) {
		changed = transferred
	}
	if since := time.Since(changed); since < stopWindow {
		inv.tb.Errorf("the roll halted %s %v after the servers last changed, want at least %v", id, since, stopWindow)
	}
}

// reads returns when the roll called the Nomad method name.
func (inv *serverInvariants) reads(name string) []time.Time {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	var out []time.Time
	for _, c := range inv.nomad {
		if c.name == name {
			out = append(out, c.at)
		}
	}
	return out
}

// wantKept fails the test unless the hooks checked every call to the Vultr fake and to Nomad since the watch began,
// and no call reached a server that the roll had halted. A server that the test halted itself may be reached until the
// roll lists the cloud again, and is left out.
func (inv *serverInvariants) wantKept() {
	inv.tb.Helper()
	inv.mu.Lock()
	checked, cutBefore := inv.checked, inv.cutBefore
	inv.mu.Unlock()
	want := len(inv.f.Calls()) - inv.vultr + len(inv.w.Log()) - inv.seen + cutBefore
	if checked != want || want == 0 {
		inv.tb.Errorf("the invariants checked %d calls, want %d: the calls that reached the fakes and those that a cut "+
			"ended before them", checked, want)
	}
	inv.mu.Lock()
	outside := maps.Clone(inv.outside)
	inv.mu.Unlock()
	stopped := map[string]string{} // the name of each stopped server, by its public address
	inv.w.mu.Lock()
	for _, id := range callsOf(inv.f, "HaltInstance") {
		if r := inv.w.raft.find(id); r != nil && !outside[id] {
			stopped[r.address] = r.name
		}
	}
	inv.w.mu.Unlock()
	for _, c := range inv.w.HaltedCalls() {
		host, _, _ := strings.Cut(c.Server, ":")
		if name, ok := stopped[host]; ok {
			inv.tb.Errorf("the roll called %s %s on %s, which it had stopped", c.Name, c.Arg, name)
		}
	}
}

// serversFlowWorld is serversWorld whose servers change over time, with the invariants watching, after the setups.
func serversFlowWorld(
	t *testing.T, setups ...func(*nomadWorld),
) (*app.Service, *vultrfake.Fake, *nomadWorld, *serverInvariants) {
	t.Helper()
	svc, f, w := serversWorld(t, func(w *nomadWorld) {
		w.ServersOverTime()
		for _, setup := range setups {
			setup(w)
		}
	})
	return svc, f, w, watchServers(t, f, w)
}

// serverMachine is a server machine of the cluster as a roll sees it.
type serverMachine struct{ id, name, hash string }

// serverMachines returns the machines of the server group that f lists, in creation order.
func serverMachines(f *vultrfake.Fake) []serverMachine {
	var out []serverMachine
	for _, in := range f.Instances() {
		if strings.HasPrefix(in.Hostname, "prod-servers-") {
			out = append(out, serverMachine{id: in.ID, name: in.Hostname, hash: tagOf(in.Tags, cloud.LabelSpecHash)})
		}
	}
	return out
}

// idsOf returns the IDs of the machines.
func idsOf(machines []serverMachine) []string {
	var out []string
	for _, m := range machines {
		out = append(out, m.id)
	}
	return out
}

// wantServersRolled fails the test unless the servers are three, none of them an old one or with an old one's name,
// each with a spec hash that differs from the old one's when hashChanged and equals it otherwise, and Nomad lists
// exactly them as voters and as alive members, and no peer or member of an old machine.
func wantServersRolled(t *testing.T, svc *app.Service, f *vultrfake.Fake, old []serverMachine, hashChanged bool) {
	t.Helper()
	now := serverMachines(f)
	if len(now) != serverGroupSize {
		t.Errorf("the server group has %d machines, want %d", len(now), serverGroupSize)
	}
	var wantPeers, wantMembers []string
	for _, m := range now {
		if slices.ContainsFunc(old, func(o serverMachine) bool { return o.name == m.name || o.id == m.id }) {
			t.Errorf("the server %s (ID %s) has the name or the ID of an old server", m.name, m.id)
		}
		if m.hash == "" || (m.hash != old[0].hash) != hashChanged {
			t.Errorf("the server %s has the spec hash %q and the old servers had %q, want the hash changed: %v", m.name,
				m.hash, old[0].hash, hashChanged)
		}
		wantPeers = append(wantPeers, raftIDOf(m.id)+" "+m.name+".global voter")
		wantMembers = append(wantMembers, m.name+".global alive")
	}
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	peers, err := api.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	members, err := api.Members(t.Context())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	var gotPeers, gotMembers []string
	for _, p := range peers {
		role := "nonvoter"
		if p.Voter {
			role = "voter"
		}
		gotPeers = append(gotPeers, p.ID+" "+p.Name+" "+role)
	}
	for _, m := range members {
		gotMembers = append(gotMembers, m.Name+" "+m.Status)
	}
	for _, list := range [][]string{wantPeers, gotPeers, wantMembers, gotMembers} {
		slices.Sort(list)
	}
	if diff := cmp.Diff(wantPeers, gotPeers); diff != "" {
		t.Errorf("the Raft configuration (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantMembers, gotMembers); diff != "" {
		t.Errorf("the gossip pool (-want +got):\n%s", diff)
	}
}

// maxPollBlock is the longest block of calls that collapsePolls takes for one poll of a wait.
const maxPollBlock = 6

// collapsePolls shortens the calls of a roll that waits: a block of up to maxPollBlock lines that follows itself
// unchanged, as the reads of a wait do at each poll, shows once, followed by a line that says how many times more it
// comes. The rest of the calls stays as it is.
func collapsePolls(calls []string) []string {
	var out []string
	for i := 0; i < len(calls); {
		n, times := 0, 0
		for size := 1; size <= maxPollBlock && i+2*size <= len(calls); size++ {
			again := 0
			for slices.Equal(calls[i:i+size], calls[i+(again+1)*size:min(len(calls), i+(again+2)*size)]) {
				again++
			}
			if again > 0 {
				n, times = size, again
				break
			}
		}
		if n == 0 {
			out = append(out, calls[i])
			i++
			continue
		}
		out = append(out, calls[i:i+n]...)
		out = append(out, fmt.Sprintf("... the block above, %d more times", times))
		i += n * (times + 1)
	}
	return out
}

// TestCollapsePollsShortensAWaitAndKeepsTheRest shows a block that repeats once with the count of its repeats, leaves
// a block that does not repeat, and takes the shortest block of a run.
func TestCollapsePollsShortensAWaitAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	got := collapsePolls([]string{"a", "b", "c", "b", "c", "b", "c", "d", "d", "d", "e", "b", "c", "b"})

	want := []string{
		"a", "b", "c", "... the block above, 2 more times", "d", "... the block above, 2 more times", "e", "b", "c",
		"b",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("collapsePolls (-want +got):\n%s", diff)
	}
}

// TestRollServersFlowReplacesTheServers applies the roll of the three outdated servers, with the invariants watching:
// the plan that it returns and the calls to Vultr and Nomad match golden files, and the cluster ends as
// wantServersRolled and wantRollEnded say.
func TestRollServersFlowReplacesTheServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := serversFlowWorld(t)
		old := serverMachines(f)
		var plan app.RollPlan
		var err error

		calls := flowCalls(t, svc, f, w, func() *engine.Plan {
			plan, err = applyRoll(svc, app.RollOptions{})
			return nil
		})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Stopped: 3, Deleted: 3}); !plan.Applied || plan.Rolled != want {
			t.Errorf("the plan says applied %v and rolled %+v, want applied and %+v", plan.Applied, plan.Rolled, want)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		checkGolden(t, "flow_roll_servers.plan.golden", text.String())
		checkGolden(t, "flow_roll_servers.calls.golden", callsText(collapsePolls(calls)))
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, old, true)
		inv.wantKept()
	})
}

// TestRollServersFlowReportsEachStepOfTheRoll applies the same roll and matches its progress lines to a golden file.
func TestRollServersFlowReportsEachStepOfTheRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := serversFlowWorld(t)
		lines := recordProgress(svc)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		checkGolden(t, "flow_roll_servers.steps.golden", strings.Join(*lines, "\n")+"\n")
	})
}

// clusterFlowWorld is serversFlowWorld whose workers are outdated too.
func clusterFlowWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld, *serverInvariants) {
	t.Helper()
	svc, f, w, _ := serversFlowWorld(t)
	mustReplace(t, svc, workersMetaYAML)
	mustUpdate(t, svc)
	return svc, f, w, watchServers(t, f, w)
}

// TestRollServersFlowRollsTheServersBeforeTheWorkers applies the roll of a cluster whose servers and workers are
// outdated: the servers are replaced first, and the progress lines match a golden file.
func TestRollServersFlowRollsTheServersBeforeTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := clusterFlowWorld(t)
		oldServers, oldWorkers := serverMachines(f), workerIDs(f)
		before := workerHashes(f)
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 5, Drained: 2, Stopped: 3, Deleted: 5, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		checkGolden(t, "flow_roll_cluster.steps.golden", strings.Join(*lines, "\n")+"\n")
		wantInOrder(t, *lines, "node done delete prod-servers-0", "node started create prod-workers-2")
		wantWorkersRolled(t, svc, f, before, oldWorkers)
		wantServersRolled(t, svc, f, oldServers, true)
		inv.wantKept()
	})
}

// TestRollServersFlowMovesTheLeadershipOnceAndSettlesAfterTheBlip rolls servers whose leader is the oldest and reads
// unhealthy for 2 s after a transfer, as Nomad does after some: the roll waits for one settle after the transfer and
// goes on, and moves the leadership once.
func TestRollServersFlowMovesTheLeadershipOnceAndSettlesAfterTheBlip(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := serversFlowWorld(t, func(w *nomadWorld) { w.SetTransferBlip(2 * time.Second) })
		old := serverMachines(f)
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Stopped: 3, Deleted: 3}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantInOrder(t, *lines, "nomad started transfer prod-servers-0", "nomad done transfer prod-servers-0",
			"nomad started settle", "nomad done settle", "node started stop prod-servers-0")
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, old, true)
		inv.wantKept()
	})
}

// haltDuringStable halts the machine called name from outside the roll 40 s after the nth start of the wait for the
// window: autopilot still counts the halted server healthy when the window ends, and the list that the run took before
// the wait shows it running.
func haltDuringStable(t *testing.T, svc *app.Service, f *vultrfake.Fake, inv *serverInvariants, name string, nth int) {
	t.Helper()
	starts := 0
	inner := svc.OnProgress
	svc.OnProgress = func(p app.Progress) {
		if inner != nil {
			inner(p)
		}
		if p.Step != app.NodeStarted || p.Nomad == nil || p.Nomad.Action != app.NomadStable {
			return
		}
		if starts++; starts != nth {
			return
		}
		id := instanceNamed(t, f, name)
		time.AfterFunc(40*time.Second, func() { inv.halt(id) })
	}
}

// TestRollServersFlowStopsNoServerWhenTheNewServerIsHaltedDuringTheWindow ends the roll with the refusal after the
// settle when the new server is halted from outside while the roll waits for the window: the roll halts no server.
func TestRollServersFlowStopsNoServerWhenTheNewServerIsHaltedDuringTheWindow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := serversFlowWorld(t)
		lines := recordProgress(svc)
		haltDuringStable(t, svc, f, inv, "prod-servers-3", 1)

		plan, err := applyRoll(svc, app.RollOptions{})

		const refusal = "node group servers: node prod-servers-3 is not running; " +
			"run tent update cluster or tent validate cluster first"
		wantError(t, err, refusal)
		if halted := instanceNamed(t, f, "prod-servers-3"); !slices.Equal(callsOf(f, "HaltInstance"), []string{halted}) {
			t.Errorf("the cloud halted %v, want only the new server %s that the test halted", callsOf(f, "HaltInstance"), halted)
		}
		if len(*lines) < 4 {
			t.Fatalf("the progress has %d lines, want at least 4:\n%s", len(*lines), strings.Join(*lines, "\n"))
		}
		wantLines(t, (*lines)[len(*lines)-4:], []string{
			"nomad started stable", "nomad started settle", "nomad failed settle: " + refusal,
			"nomad failed stable: " + refusal,
		})
		if plan.Applied || plan.Rolled != (app.RollCounts{Created: 1}) {
			t.Errorf("the plan says applied %v and rolled %+v, want one create and not applied", plan.Applied, plan.Rolled)
		}
		inv.wantKept()
	})
}

// TestRollServersFlowRemovesAnOldServerThatWasHaltedDuringTheWindow makes the old server that is halted from outside
// while the roll waits for the window its victim: the roll removes it before it stops another server, halts no server
// that it did not plan to, and replaces every server.
func TestRollServersFlowRemovesAnOldServerThatWasHaltedDuringTheWindow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := serversFlowWorld(t)
		old := serverMachines(f)
		lines := recordProgress(svc)
		haltDuringStable(t, svc, f, inv, "prod-servers-2", 1)
		halted := instanceNamed(t, f, "prod-servers-2")

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Stopped: 2, Deleted: 3}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v: it stopped the two servers that were running", plan.Rolled, want)
		}
		if got := count(*lines, "node started stop prod-servers-2"); got != 0 {
			t.Errorf("the roll stopped the halted server %d times, want none", got)
		}
		wantInOrder(t, *lines, "nomad started server-down prod-servers-2", "node done delete prod-servers-2",
			"node started stop prod-servers-1")
		halts := callsOf(f, "HaltInstance")
		if len(halts) != 3 || halts[0] != halted {
			t.Errorf("the cloud halted %v, want the halted server %s and then the two that the roll stops", halts, halted)
		}
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, old, true)
		inv.wantKept()
	})
}

// TestRollServersFlowRefusesAnUnhealthyClusterWithoutALock applies a roll of a cluster whose servers autopilot reports
// unhealthy: it returns the refusal with the groups, only reads, and does not take the lock that the test holds.
func TestRollServersFlowRefusesAnUnhealthyClusterWithoutALock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := serversWorld(t, nil)
		w.Unhealthy()
		u := watch(t, svc, f, w)

		plan, err := applyRoll(svc, app.RollOptions{})

		wantError(t, err, "node group servers: autopilot reports the servers unhealthy; "+
			"tent replaces a server only while every server is healthy")
		u.check(t)
		if len(plan.Groups) == 0 || plan.Next != nil || plan.Applied {
			t.Errorf("plan = %+v, want the groups, no next step and not applied", plan)
		}
	})
}

// failureWindows returns, for each server whose machine the world saw stop, when the world counts the server failed
// and when autopilot's cleanup would remove its peer.
func (w *nomadWorld) failureWindows() [][2]time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out [][2]time.Time
	for _, r := range w.raft.servers {
		if failed, stopped := r.failedAt(w.delays); stopped {
			out = append(out, [2]time.Time{failed, failed.Add(w.delays.cleanupAfter)})
		}
	}
	return out
}

// nomadArgs returns the arguments of the calls of the Nomad method name that reached the fake, sorted.
func nomadArgs(w *nomadWorld, name string) []string {
	var out []string
	for _, c := range w.Log() {
		if c.Name == name {
			out = append(out, c.Arg)
		}
	}
	slices.Sort(out)
	return out
}

// TestRollServersFlowEndsTheSameWhoeverRemovesThePeer rolls the servers with three timings of the failure of a
// stopped server and of autopilot's cleanup, which fall between the polls of the roll so that who removes the peer
// does not depend on the run: autopilot removes it before the roll looks (the roll sends no RemovePeer), the roll
// looks first and removes it, or autopilot never does. The calls of the world show who removed each peer and where
// the failure and the cleanup fell among the reads of autopilot's report. The cluster ends the same in all three.
func TestRollServersFlowEndsTheSameWhoeverRemovesThePeer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		setup     func(*nomadWorld)
		tentFirst bool // the roll removes the peers
		// between says if a read of autopilot's report falls between the failure and the cleanup of each server; it is
		// only checked when there is a cleanup.
		between, cleanup bool
	}{
		{"autopilot first", func(w *nomadWorld) {
			w.SetFailAfter(41 * time.Second)
			w.SetCleanupAfter(500 * time.Millisecond)
		}, false, false, true},
		{"the roll first", func(w *nomadWorld) {
			w.SetFailAfter(41 * time.Second)
			w.SetCleanupAfter(3 * time.Second)
		}, true, true, true},
		{"no cleanup", func(w *nomadWorld) { w.NoCleanup() }, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, inv := serversFlowWorld(t, tc.setup)
				old := serverMachines(f)

				plan, err := applyRoll(svc, app.RollOptions{})

				if err != nil {
					t.Fatalf("RollingUpdate: %v", err)
				}
				if want := (app.RollCounts{Created: 3, Stopped: 3, Deleted: 3}); plan.Rolled != want {
					t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
				}
				var wantRemoved, wantForced []string
				if tc.tentFirst {
					for _, m := range old {
						wantRemoved = append(wantRemoved, raftIDOf(m.id))
					}
				}
				for _, m := range old {
					wantForced = append(wantForced, m.name+".global")
				}
				slices.Sort(wantRemoved)
				slices.Sort(wantForced)
				if diff := cmp.Diff(wantRemoved, nomadArgs(w, "RemovePeer")); diff != "" {
					t.Errorf("the peers that the roll removed (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(wantForced, nomadArgs(w, "ForceLeave")); diff != "" {
					t.Errorf("the members that the roll forced out (-want +got):\n%s", diff)
				}
				windows := w.failureWindows()
				if len(windows) != len(old) {
					t.Fatalf("the world saw %d servers stop, want %d", len(windows), len(old))
				}
				if tc.cleanup {
					for _, window := range windows {
						between := slices.ContainsFunc(inv.reads("Health"), func(at time.Time) bool {
							return !at.Before(window[0]) && !at.After(window[1])
						})
						if between != tc.between {
							t.Errorf("a read of autopilot's report between the failure at %v and the cleanup at %v: %v, want %v",
								window[0], window[1], between, tc.between)
						}
					}
				}
				wantRollEnded(t, svc, f, idsOf(old))
				wantServersRolled(t, svc, f, old, true)
				inv.wantKept()
			})
		})
	}
}

// TestRollServersFlowForceReplacesEveryServerAndWorkerOnce rolls a cluster that is up to date because it is forced,
// with the default selection: the servers and the workers are each replaced once, the new servers have the hash of the
// old ones, and the machines that the roll makes are not forced, so it ends.
func TestRollServersFlowForceReplacesEveryServerAndWorkerOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		w.ServersOverTime()
		mustUpdate(t, svc)
		inv := watchServers(t, f, w)
		oldServers, oldWorkers := serverMachines(f), workerIDs(f)

		plan, err := applyRoll(svc, app.RollOptions{Force: true})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 5, Drained: 2, Stopped: 3, Deleted: 5, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		wantRollEnded(t, svc, f, append(idsOf(oldServers), oldWorkers...))
		wantServersRolled(t, svc, f, oldServers, false)
		if got := len(workerIDs(f)); got != workerCount {
			t.Errorf("the workers have %d machines, want %d", got, workerCount)
		}
		inv.wantKept()
	})
}

// TestRollServersFlowRefusesAGroupOfOneServerAndRollsTheWorkers refuses the default selection of a cluster of one
// server whose servers and workers are outdated, with only reads, and rolls the workers when they are selected.
func TestRollServersFlowRefusesAGroupOfOneServerAndRollsTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
		mustUpdate(t, svc)
		mustReplace(t, svc, keyedClusterYAML+serversExtraYAML, workersMetaYAML)
		mustUpdate(t, svc)
		server := instanceNamed(t, f, "prod-servers-0")
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())

		plan, err := applyRoll(svc, app.RollOptions{})

		wantError(t, err, "node group servers: a group of one server cannot roll: its failure tolerance is 0")
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if got := nomadWrites(w, nomadCalls); len(got) > 0 {
			t.Errorf("the roll wrote to Nomad: %q", got)
		}
		wantLockFree(t, svc.Store)
		if len(plan.Groups) != 2 || plan.Next != nil || plan.Applied {
			t.Errorf("plan = %+v, want the two groups, no next step and not applied", plan)
		}

		plan, err = applyRoll(svc, app.RollOptions{NodeGroups: []string{"workers"}})

		if err != nil {
			t.Fatalf("RollingUpdate of the workers: %v", err)
		}
		if want := (app.RollCounts{Created: 2, Drained: 2, Deleted: 2, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		if !hasInstance(f, server) {
			t.Errorf("the machine %s of the one server is gone", server)
		}
	})
}

// TestRollServersFlowCountsTheWaitForTheServersOfEachVictimFromItsOwnStart rolls three servers whose removed peer
// the report keeps for six minutes, so that each wait for the servers to be healthy lasts six minutes and the three
// together last longer than the ten minutes that one wait may: the roll ends, since each wait counts from its own
// start.
func TestRollServersFlowCountsTheWaitForTheServersOfEachVictimFromItsOwnStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, inv := serversFlowWorld(t, func(w *nomadWorld) { w.SetReportLag(6 * time.Minute) })
		old := serverMachines(f)
		lines := recordProgress(svc)
		start := time.Now()

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		if got := count(*lines, "nomad started healthy"); got != serverGroupSize {
			t.Errorf("the roll waited for the servers %d times, want %d", got, serverGroupSize)
		}
		if got := time.Since(start); got < 3*6*time.Minute {
			t.Errorf("the roll took %v, want at least the three waits of 6 minutes", got)
		}
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, old, true)
		inv.wantKept()
	})
}

// TestServerViewBreaksTheInvariants names each invariant that a view breaks, and none for a view that keeps them.
func TestServerViewBreaksTheInvariants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		view serverView
		want []string
	}{
		{"four machines, a leader, a full quorum", serverView{
			machines: 4, voters: []bool{true, true, true, true}, leader: "i-1", leaderRuns: true}, nil},
		{"two of three voters run", serverView{
			machines: 3, voters: []bool{true, true, false}, leader: "i-1", leaderRuns: true}, nil},
		{"three of four voters run", serverView{
			machines: 4, voters: []bool{true, true, true, false}, leader: "i-1", leaderRuns: true}, nil},
		{"five machines", serverView{
			machines: 5, voters: []bool{true, true, true}, leader: "i-1", leaderRuns: true},
			[]string{"the server group has 5 machines, want at most 4"}},
		{"no leader", serverView{machines: 3, voters: []bool{true, true, true}},
			[]string{`the cluster has the leader "", want one whose machine runs`}},
		{"a leader whose machine halted", serverView{
			machines: 3, voters: []bool{true, true, true}, leader: "i-1"},
			[]string{`the cluster has the leader "i-1", want one whose machine runs`}},
		{"two of four voters run", serverView{
			machines: 4, voters: []bool{true, true, false, false}, leader: "i-1", leaderRuns: true},
			[]string{"2 of 4 voters run, want at least 3"}},
		{"one of three voters runs", serverView{
			machines: 3, voters: []bool{true, false, false}, leader: "i-1", leaderRuns: true},
			[]string{"1 of 3 voters run, want at least 2"}},
		{"everything", serverView{machines: 9, voters: []bool{false}}, []string{
			"the server group has 9 machines, want at most 4",
			`the cluster has the leader "", want one whose machine runs`,
			"0 of 1 voters run, want at least 1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, tc.view.broken(serverGroupSize)); diff != "" {
				t.Errorf("broken (-want +got):\n%s", diff)
			}
		})
	}
}

// TestServerInvariantsFailTheTestForWhatTheClusterBreaks makes each invariant fail on the world, so that the flows
// cannot pass because their hooks look at nothing: a lost quorum, a leader that halted, a fifth server machine,
// and a stop shortly after a transfer of the leadership. A stop once the window has passed fails nothing.
func TestServerInvariantsFailTheTestForWhatTheClusterBreaks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, b liveWorld, inv *serverInvariants)
		want    string // a part of the failure, empty for none
	}{
		{"a lost quorum", func(t *testing.T, b liveWorld, inv *serverInvariants) {
			for _, s := range b.followers(t) {
				inv.halt(s.id)
			}
		}, "1 of 3 voters run, want at least 2"},
		{"a leader that halted", func(t *testing.T, b liveWorld, inv *serverInvariants) {
			inv.halt(b.leader(t).id)
		}, `want one whose machine runs`},
		{"too many machines", func(t *testing.T, b liveWorld, _ *serverInvariants) {
			b.addServer(t, "prod-servers-3")
			b.addServer(t, "prod-servers-4")
		}, "the server group has 5 machines, want at most 4"},
		{"a stop just before the window ends", func(t *testing.T, b liveWorld, _ *serverInvariants) {
			stopAfterTransfer(t, b, stopWindow-time.Second)
		}, "the roll halted"},
		{"a stop when the window ends", func(t *testing.T, b liveWorld, _ *serverInvariants) {
			stopAfterTransfer(t, b, stopWindow)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ftb := &failureTB{TB: t}
				b := newLiveWorldWith(t, func(w *nomadWorld) { w.ServersOverTime(); w.FailOnLeaderLoss(ftb) })
				inv := watchServers(ftb, b.f, b.w)
				b.peers(t)
				if got := ftb.failures(); len(got) > 0 {
					t.Fatalf("the invariants failed for a healthy cluster: %q", got)
				}

				tc.breakIt(t, b, inv)
				if _, err := b.f.ListInstances(t.Context(), cloud.LabelCluster+"=prod"); err != nil {
					t.Fatalf("ListInstances: %v", err)
				}
				b.peers(t)

				got := strings.Join(ftb.failures(), "\n")
				if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
					t.Errorf("the invariants failed with %q, want a failure that says %q", got, tc.want)
				}
			})
		})
	}
}

// stopAfterTransfer moves the leadership to a follower, waits for d and halts another follower through the Vultr fake,
// as the roll does.
func stopAfterTransfer(t *testing.T, b liveWorld, d time.Duration) {
	t.Helper()
	followers := b.followers(t)
	if err := b.api.TransferLeadership(t.Context(), followers[0].raftID()); err != nil {
		t.Fatalf("TransferLeadership: %v", err)
	}
	time.Sleep(d)
	b.haltInstance(t, followers[1])
}
