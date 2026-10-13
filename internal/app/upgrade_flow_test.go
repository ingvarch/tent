package app_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// upgradeFlowDocs are the specs of the cluster of three servers and two workers, and upgradeFlowCombinedDocs those of
// the cluster of three combined nodes and two workers.
var (
	upgradeFlowDocs         = []string{keyedClusterYAML, serversYAML, workersYAML}
	upgradeFlowCombinedDocs = []string{keyedClusterYAML, combinedDrainYAML, workersYAML}
)

// versionInvariants checks, after every create of a machine, what an upgrade keeps: a machine of a client group gets
// a Nomad no newer than any server of the Raft configuration reports, and no machine gets an older Nomad than a
// server, or a node that is not down, reports. A server or a node that reports no version fails the test too.
type versionInvariants struct {
	tb      testing.TB
	f       *vultrfake.Fake
	w       *nomadWorld
	mu      sync.Mutex
	creates int // how many creates were checked
}

// hook is the Vultr hook that checks each create after the fake has carried it out.
func (v *versionInvariants) hook(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
	err := next(ctx)
	if c.Name == "CreateInstance" && err == nil {
		v.check(c.Arg)
	}
	return err
}

// check fails the test for each rule that the machine labelled label breaks.
func (v *versionInvariants) check(label string) {
	v.tb.Helper()
	var in struct{ ID, Hostname string }
	found := false
	for _, candidate := range v.f.Instances() {
		if candidate.Label == label {
			in.ID, in.Hostname, found = candidate.ID, candidate.Hostname, true
		}
	}
	if !found {
		v.tb.Errorf("the create of %s left no machine", label)
		return
	}
	version := nomadAssetVersion(v.w.createdConfig(in.ID))
	if version == "" {
		v.tb.Errorf("the create request of %s has no Nomad version", in.Hostname)
		return
	}
	v.mu.Lock()
	v.creates++
	v.mu.Unlock()
	client := strings.HasPrefix(in.Hostname, machinePrefix("workers"))
	for name, reported := range v.reported() {
		switch {
		case reported == "":
			v.tb.Errorf("%s reports no Nomad version at the create of %s", name, in.Hostname)
		case channels.Compare(version, reported) < 0:
			v.tb.Errorf("%s gets Nomad %s, older than the %s that %s reports", in.Hostname, version, reported, name)
		}
	}
	if !client {
		return
	}
	for name, reported := range v.servers() {
		if reported != "" && channels.Compare(version, reported) > 0 {
			v.tb.Errorf("the client %s gets Nomad %s, newer than the %s of server %s", in.Hostname, version, reported,
				name)
		}
	}
}

// servers returns the version that each server of the Raft configuration reports, as the test's edits shape it, by
// the server's name.
func (v *versionInvariants) servers() map[string]string {
	var health nomadops.Health
	v.w.mu.Lock()
	for _, r := range v.w.raft.servers {
		if !r.removed {
			health.Servers = append(health.Servers, nomadops.ServerHealth{Name: r.name, Version: v.w.versionOf(r.id)})
		}
	}
	v.w.mu.Unlock()
	out := map[string]string{}
	for _, s := range v.w.shapeHealth(health).Servers {
		out[s.Name] = s.Version
	}
	return out
}

// reported returns the version that each server of the Raft configuration and each node that is not down reports, by
// name.
func (v *versionInvariants) reported() map[string]string {
	out := map[string]string{}
	for name, version := range v.servers() {
		out["server "+name] = version
	}
	for _, n := range v.w.nodeViews() {
		if n.Status != "down" {
			out["node "+n.Name] = n.Version
		}
	}
	return out
}

// wantChecked fails the test unless the invariants checked n creates.
func (v *versionInvariants) wantChecked(n int) {
	v.tb.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.creates != n {
		v.tb.Errorf("the version invariants checked %d creates, want %d", v.creates, n)
	}
}

// workerLimits checks the limits of the workers that the roll of client groups keeps with the default shape (maxSurge
// 1, maxUnavailable 0): at most one machine more than the size, and no fewer machines whose node is ready, eligible
// and not draining than the size. It reads the nodes as the world last saw them, so it advances no drain and meets no
// fault.
type workerLimits struct {
	tb testing.TB
	f  *vultrfake.Fake
	w  *nomadWorld
}

// check fails the test for each limit that the workers break before the call.
func (l workerLimits) check(call string) {
	l.tb.Helper()
	ids := workerIDs(l.f)
	if len(ids) > workerCount+1 {
		l.tb.Errorf("before %s the workers have %d machines, want at most %d", call, len(ids), workerCount+1)
	}
	if got := availableWorkers(ids, slices.Collect(maps.Values(l.w.nodeViews()))); got < workerCount {
		l.tb.Errorf("before %s %d workers are available, want at least %d", call, got, workerCount)
	}
}

// vultr is the Vultr hook that checks the limits before a call and then passes it to next.
func (l workerLimits) vultr(next vultrfake.Hook) vultrfake.Hook {
	return func(ctx context.Context, c vultrfake.Call, do func(context.Context) error) error {
		l.check("Vultr " + c.Name + " " + c.Arg)
		return next(ctx, c, do)
	}
}

// nomad is the Nomad hook that checks the limits before a call.
func (l workerLimits) nomad(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
	l.check("Nomad " + c.Name + " " + c.Arg)
	return next(ctx)
}

// upgradeFlowWorld builds the cluster of docs on Nomad 2.0.6, in a world whose machines report the version of their
// create request, and makes the channel recommend 2.0.7. The invariants of the group called group watch, and so do
// the version invariants and the limits of the workers. setup sets the world before the cluster is built.
func upgradeFlowWorld(
	t *testing.T, group string, docs []string, setup func(*nomadWorld),
) (*app.Service, *vultrfake.Fake, *nomadWorld, *serverInvariants, *versionInvariants) {
	t.Helper()
	svc, f, w := newRelease(t, docs...)
	w.NodeVersions()
	w.ServersOverTime()
	if setup != nil {
		setup(w)
	}
	svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
	mustUpdate(t, svc)
	svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
	watch := watchServers
	if group == combinedGroup {
		watch = watchCombined
	}
	inv := watch(t, f, w, group, serverGroupSize)
	vi := &versionInvariants{tb: t, f: f, w: w}
	limits := workerLimits{tb: t, f: f, w: w}
	inv.watch(limits.vultr(vi.hook), limits.nomad)
	return svc, f, w, inv, vi
}

// liveOf returns the liveWorld of a service and its fakes.
func liveOf(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld) liveWorld {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	return liveWorld{f: f, w: w, api: api}
}

// upgradeAndUpdate writes the new version with UpgradeCluster, plans and applies the update that moves the pin, and
// checks that the update changes the completed spec alone: no machine is created, stopped or deleted.
func upgradeAndUpdate(t *testing.T, svc *app.Service, f *vultrfake.Fake, updatePlan string) {
	t.Helper()
	up, err := svc.UpgradeCluster(t.Context(), "prod", true)
	if err != nil || !up.Applied || up.Current != olderNomad || up.Target != newerNomad {
		t.Fatalf("UpgradeCluster = %+v, %v; want the move from %s to %s applied", up, err, olderNomad, newerNomad)
	}
	plan, err := svc.Update(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("Update without apply: %v", err)
	}
	if updatePlan != "" {
		checkGolden(t, updatePlan, planText(t, plan))
	}
	calls := len(f.Calls())
	if _, err := svc.Update(t.Context(), "prod", true); err != nil {
		t.Fatalf("Update: %v", err)
	}
	wantNoWrites(t, f.Calls()[calls:])
	wantPinned(t, svc, newerNomad)
	if len(plan.Outdated) != serverGroupSize+workerCount {
		t.Errorf("the update lists %d outdated nodes, want %d", len(plan.Outdated), serverGroupSize+workerCount)
	}
}

// wantUpgradeEnded fails the test unless the roll ended the upgrade: every server and node reports 2.0.7, the machines
// that the roll replaced are gone, the cluster is valid with Nomad 2.0.7, the specs hold the version, UpgradeCluster
// and RollingUpdate have nothing to do, and Update lists no outdated node.
func wantUpgradeEnded(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld, old []string) {
	t.Helper()
	wantRollEnded(t, svc, f, old)
	wantAllOn(t, liveOf(t, svc, f, w), newerNomad)
	v, err := svc.ValidateCluster(t.Context(), "prod")
	if err != nil || !v.Valid() || v.NomadVersion != newerNomad {
		t.Errorf("ValidateCluster = %+v, %v; want a valid cluster on Nomad %s", v, err, newerNomad)
	}
	for _, apply := range []bool{false, true} {
		up, err := svc.UpgradeCluster(t.Context(), "prod", apply)
		if err != nil || up.Due() || up.Current != newerNomad {
			t.Errorf("UpgradeCluster(apply %t) = %+v, %v; want nothing to upgrade from Nomad %s", apply, up, err,
				newerNomad)
		}
	}
	wantPinned(t, svc, newerNomad)
	if plan, err := svc.Update(t.Context(), "prod", false); err != nil || len(plan.Outdated) != 0 {
		t.Errorf("Update lists the outdated nodes %+v and returns %v, want neither", plan.Outdated, err)
	}
	if got := decode(t, string(get(t, svc.Store, clusterPath))).Cluster.Spec.Nomad.Version; got != newerNomad {
		t.Errorf("cluster.yaml sets Nomad %q, want %q", got, newerNomad)
	}
}

// wantNoVersionNamed fails the test for each progress line that names one of the two Nomad versions.
func wantNoVersionNamed(t *testing.T, lines []string) {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, newerNomad) || strings.Contains(line, olderNomad) {
			t.Errorf("the progress line %q names a version", line)
		}
	}
}

// TestUpgradeFlowRollsTheServersBeforeTheWorkers upgrades the cluster of three servers and two workers: UpgradeCluster
// writes the version, Update moves the pin and changes no machine, and the roll replaces the servers and then the
// workers, with the version invariants watching every create. The plans of the update and of the roll and the
// progress lines of the roll match golden files.
func TestUpgradeFlowRollsTheServersBeforeTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv, vi := upgradeFlowWorld(t, serversGroup, upgradeFlowDocs, nil)
		old := instanceIDs(f)
		upgradeAndUpdate(t, svc, f, "flow_upgrade.update.plan.golden")
		planned, err := rollingUpdate(svc, app.RollOptions{})
		if err != nil {
			t.Fatalf("RollingUpdate without apply: %v", err)
		}
		checkGolden(t, "flow_upgrade.roll.plan.golden", planText(t, planned))
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 5, Drained: 2, Stopped: 3, Deleted: 5, Purged: 2}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		checkGolden(t, "flow_upgrade.steps.golden", strings.Join(*lines, "\n")+"\n")
		wantInOrder(t, *lines, "node done delete prod-servers-0", "node started create prod-workers-2")
		wantNoVersionNamed(t, *lines)
		vi.wantChecked(5)
		wantUpgradeEnded(t, svc, f, w, old)
		inv.wantKept()
	})
}

// TestUpgradeFlowRollsTheCombinedNodesBeforeTheWorkers is the same upgrade of a cluster of three combined nodes and
// two workers.
func TestUpgradeFlowRollsTheCombinedNodesBeforeTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv, vi := upgradeFlowWorld(t, combinedGroup, upgradeFlowCombinedDocs,
			func(w *nomadWorld) { w.SetDrainReads(drainReads) })
		old := instanceIDs(f)
		upgradeAndUpdate(t, svc, f, "")
		lines := recordProgress(svc)

		plan, err := applyRoll(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 5, Drained: 5, Stopped: 3, Deleted: 5, Purged: 5}); plan.Rolled != want {
			t.Errorf("the roll did %+v, want %+v", plan.Rolled, want)
		}
		checkGolden(t, "flow_upgrade_combined.steps.golden", strings.Join(*lines, "\n")+"\n")
		wantInOrder(t, *lines, "nomad done purge prod-all-0", "node started create prod-workers-2")
		wantNoVersionNamed(t, *lines)
		vi.wantChecked(5)
		wantUpgradeEnded(t, svc, f, w, old)
		wantCombinedNodesReady(t, svc, f)
		inv.wantKept()
	})
}

// TestUpgradeFlowFinishesARollThatWasCutBetweenTheServersAndTheWorkers ends the run of the roll at the first progress
// event of the worker group, before that step's call. Update then has no change and lists the two workers as outdated,
// ValidateCluster fails the check nomad-version for the two workers alone, and a second roll ends the upgrade.
func TestUpgradeFlowFinishesARollThatWasCutBetweenTheServersAndTheWorkers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv, vi := upgradeFlowWorld(t, serversGroup, upgradeFlowDocs, nil)
		old := instanceIDs(f)
		upgradeAndUpdate(t, svc, f, "")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc.OnProgress = func(p app.Progress) {
			if strings.HasPrefix(p.Node.Name, machinePrefix("workers")) {
				cancel()
			}
		}
		_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})
		svc.OnProgress = nil
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cut roll returned %v, want an error that matches context.Canceled", err)
		}

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil || plan.HasChanges() {
			t.Fatalf("Update after the cut has changes %v and the error %v, want neither", plan.HasChanges(), err)
		}
		var outdated []string
		for _, n := range plan.Outdated {
			outdated = append(outdated, n.Name)
		}
		slices.Sort(outdated)
		if diff := cmp.Diff([]string{"prod-workers-0", "prod-workers-1"}, outdated); diff != "" {
			t.Errorf("the outdated nodes after the cut (-want +got):\n%s", diff)
		}
		v, err := svc.ValidateCluster(t.Context(), "prod")
		if err != nil {
			t.Fatalf("ValidateCluster: %v", err)
		}
		var failed []string
		for _, fail := range v.Failures {
			if fail.Check != "nomad-version" {
				t.Errorf("the validation has the failure %+v, want the check nomad-version alone", fail)
			}
			failed = append(failed, fail.Node)
		}
		slices.Sort(failed)
		if diff := cmp.Diff([]string{"prod-workers-0", "prod-workers-1"}, failed); diff != "" {
			t.Errorf("the nodes of the failure nomad-version (-want +got):\n%s", diff)
		}
		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("the second RollingUpdate: %v", err)
		}
		vi.wantChecked(5)
		wantUpgradeEnded(t, svc, f, w, old)
		inv.wantKept()
	})
}

// TestUpgradeFlowRollsTheServersGroupBeforeTheWorkersGroupWhenTheGroupsAreSelected refuses to roll the workers alone
// while the servers are outdated, with and without apply, with no lock and only reads; rolling the servers and then
// the workers ends the upgrade.
func TestUpgradeFlowRollsTheServersGroupBeforeTheWorkersGroupWhenTheGroupsAreSelected(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv, vi := upgradeFlowWorld(t, serversGroup, upgradeFlowDocs, nil)
		old := instanceIDs(f)
		upgradeAndUpdate(t, svc, f, "")
		const refusal = "node group workers: a new node would run Nomad 2.0.7, newer than the 2.0.6 of server " +
			"prod-servers-0.global; roll the servers first"
		workers := app.RollOptions{NodeGroups: []string{"workers"}}
		lock := holdLock(t, svc.Store)
		u := untouched{svc: svc, f: f, w: w, store: snapshot(t, svc.Store), cloud: len(f.Calls()), nomad: len(w.Log())}

		_, planErr := rollingUpdate(svc, workers)
		_, applyErr := applyRoll(svc, workers)

		wantError(t, planErr, refusal)
		wantError(t, applyErr, refusal)
		u.check(t)
		release(t, lock)
		if _, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{serversGroup}}); err != nil {
			t.Fatalf("RollingUpdate of the servers: %v", err)
		}
		if _, err := applyRoll(svc, workers); err != nil {
			t.Fatalf("RollingUpdate of the workers: %v", err)
		}
		vi.wantChecked(5)
		wantUpgradeEnded(t, svc, f, w, old)
		inv.wantKept()
	})
}

// TestVersionInvariantsFailTheTestForTheVersionsThatTheyForbid makes each rule fail on the world, so that the flows
// cannot pass because their hook looks at nothing: a client newer than a server, a server older than a server, and a
// server older than a node. A client that is no newer than the servers, and a server beside a node that is down and
// newer, fail nothing.
func TestVersionInvariantsFailTheTestForTheVersionsThatTheyForbid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, machine, role, version string
		setup                        func(*nomadWorld)
		want                         string // "" when nothing fails
	}{
		{"a client newer than a server", "prod-workers-2", "client", newerNomad, nil,
			"the client prod-workers-2 gets Nomad 2.0.7, newer than the 2.0.6 of server prod-servers-0"},
		{"a client as old as the servers", "prod-workers-2", "client", olderNomad, nil, ""},
		{"a server older than a server", "prod-servers-3", "server", olderNomad,
			func(w *nomadWorld) { reportsNewer(w, "prod-servers-0") },
			"prod-servers-3 gets Nomad 2.0.6, older than the 2.0.7 that server prod-servers-0 reports"},
		{"a server older than a node", "prod-servers-3", "server", olderNomad,
			func(w *nomadWorld) { nodeReportsNewer(w, "prod-workers-1") },
			"prod-servers-3 gets Nomad 2.0.6, older than the 2.0.7 that node prod-workers-1 reports"},
		{"a server that reports no version", "prod-servers-3", "server", olderNomad,
			func(w *nomadWorld) {
				w.ChangeServer("prod-servers-1", func(h *nomadops.ServerHealth) { h.Version = "" })
			}, "prod-servers-1 reports no Nomad version at the create of prod-servers-3"},
		{"a client beside a server that reports no version", "prod-workers-2", "client", olderNomad,
			func(w *nomadWorld) {
				w.ChangeServer("prod-servers-1", func(h *nomadops.ServerHealth) { h.Version = "" })
			}, "prod-servers-1 reports no Nomad version at the create of prod-workers-2"},
		{"a server beside a node that is down", "prod-servers-3", "server", olderNomad,
			func(w *nomadWorld) {
				w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Version, n.Status = newerNomad, "down" })
			}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := olderWorld(t)
				if tc.setup != nil {
					tc.setup(b.w)
				}
				ftb := &failureTB{TB: t}
				vi := &versionInvariants{tb: ftb, f: b.f, w: b.w}
				b.createNode(t, tc.machine, tc.role, b.nodeUserDataFor(t, tc.machine, tc.version))

				vi.check(tc.machine)

				got := strings.Join(ftb.failures(), "\n")
				if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
					t.Errorf("the invariants failed with %q, want a failure that says %q", got, tc.want)
				}
			})
		})
	}
}

// TestWorkerLimitsFailTheTestForWorkersThatBreakThem makes each limit fail on the world: one worker machine too many,
// and a worker whose node is ineligible. The cluster as it is fails nothing.
func TestWorkerLimitsFailTheTestForWorkersThatBreakThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, liveWorld)
		want  string // "" when nothing fails
	}{
		{"the cluster as it is", func(*testing.T, liveWorld) {}, ""},
		{"one worker machine too many", func(t *testing.T, b liveWorld) {
			ud := b.nodeUserDataFor(t, "prod-workers-2", olderNomad)
			b.createNode(t, "prod-workers-2", "client", ud)
			b.createNode(t, "prod-workers-3", "client", ud)
		}, "the workers have 4 machines, want at most 3"},
		{"a worker that is ineligible", func(_ *testing.T, b liveWorld) {
			b.w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Eligible = false })
		}, "1 workers are available, want at least 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := olderWorld(t)
				tc.setup(t, b)
				ftb := &failureTB{TB: t}

				workerLimits{tb: ftb, f: b.f, w: b.w}.check("a call")

				got := strings.Join(ftb.failures(), "\n")
				if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
					t.Errorf("the limits failed with %q, want a failure that says %q", got, tc.want)
				}
			})
		})
	}
}
