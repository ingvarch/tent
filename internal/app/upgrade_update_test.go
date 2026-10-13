package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// upgradedWorld is the test cluster built on Nomad 2.0.6 and then updated to 2.0.7 in its specs: the pin is 2.0.7,
// every node still reports 2.0.6 and is outdated.
func upgradedWorld(t *testing.T) liveWorld {
	t.Helper()
	b := olderWorld(t)
	b.moveToNewer(t, 3, 2)
	wantPinned(t, b.w.svc, newerNomad)
	return b
}

// specsAtVersion stores the test cluster's Cluster with the Nomad version v in its spec.
func specsAtVersion(t *testing.T, svc *app.Service, v string) {
	t.Helper()
	mustReplace(t, svc, withVersion(t, keyedClusterYAML, v))
}

// reportsNewer makes the server called name report Nomad 2.0.7.
func reportsNewer(w *nomadWorld, name string) {
	w.ChangeServer(name, func(h *nomadops.ServerHealth) { h.Version = newerNomad })
}

// nodeReportsNewer makes the node of the machine called name report Nomad 2.0.7.
func nodeReportsNewer(w *nomadWorld, name string) {
	w.ChangeNode(name, func(n *nomadops.Node) { n.Version = newerNomad })
}

// wantUpdateRefused fails the test unless Update fails with want, with and without apply, and changes neither the
// store nor the cloud.
func wantUpdateRefused(t *testing.T, b liveWorld, want string) {
	t.Helper()
	svc := b.w.svc
	stored, calls := snapshot(t, svc.Store), len(b.f.Calls())
	for _, apply := range []bool{false, true} {
		plan, err := svc.Update(t.Context(), "prod", apply)
		wantError(t, err, want)
		if plan.Applied {
			t.Errorf("Update(apply %t) applied a plan", apply)
		}
	}
	wantSnapshot(t, svc.Store, stored)
	wantNoWrites(t, b.f.Calls()[calls:])
}

// lowering is the refusal of a pin that goes from 2.0.7 to 2.0.6 beside a server or a node that runs 2.0.7.
func lowering(who string) string {
	return "update would lower the Nomad version of cluster prod from 2.0.7 to 2.0.6: tent never moves a node to an " +
		"older Nomad: the cluster's version is 2.0.6, and " + who + " runs 2.0.7; set spec.nomad.version to 2.0.7 " +
		"again and finish the roll with tent rolling-update cluster"
}

// TestUpdateTakesAnUpgradeBack puts the old version into the spec of a cluster that was updated to a new one and has
// no node on it: the plan writes the completed spec alone, nothing is created or deleted, and no node is outdated
// after it. With the version left out of the spec, the pin stays.
func TestUpdateTakesAnUpgradeBack(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := upgradedWorld(t)
		svc := b.w.svc
		mustReplace(t, svc, keyedClusterYAML)
		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil || plan.HasChanges() {
			t.Fatalf("Update with the version left out = %v, %v; want no change", plan.HasChanges(), err)
		}

		specsAtVersion(t, svc, olderNomad)
		calls := len(b.f.Calls())
		plan, err = svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if got := planText(t, plan); got != completedOnly {
			t.Errorf("the plan is:\n%s\nwant the completed spec alone:\n%s", got, completedOnly)
		}
		mustUpdate(t, svc)
		wantPinned(t, svc, olderNomad)
		wantNoWrites(t, b.f.Calls()[calls:])
		wantConverged(t, svc)
	})
}

// TestUpdateDoesNotLowerThePinUnderANode refuses to write an older Nomad version into the completed spec while a
// server, or a node that is not down, reports the newer one; a node that is down does not count.
func TestUpdateDoesNotLowerThePinUnderANode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(*nomadWorld)
		refuse string // "" when the plan stands
	}{
		{"a server", func(w *nomadWorld) { reportsNewer(w, "prod-servers-0") }, lowering("server prod-servers-0.global")},
		{"a node", func(w *nomadWorld) { nodeReportsNewer(w, "prod-workers-0") }, lowering("node prod-workers-0")},
		{"a node that is down", func(w *nomadWorld) {
			w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Version, n.Status = newerNomad, "down" })
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := upgradedWorld(t)
				tc.setup(b.w)
				specsAtVersion(t, b.w.svc, olderNomad)
				if tc.refuse != "" {
					wantUpdateRefused(t, b, tc.refuse)
					return
				}
				mustUpdate(t, b.w.svc)
				wantPinned(t, b.w.svc, olderNomad)
			})
		})
	}
}

// TestUpdateDoesNotBootANodeBelowANode refuses a plan that creates a node with an older Nomad than a server or a node
// that is not down reports, for a client and for a server, and lets it stand beside a node that is down.
func TestUpdateDoesNotBootANodeBelowANode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(*nomadWorld)
		docs   func(*testing.T) []string
		refuse string // "" when the plan stands
	}{
		{"a client beside a server", func(w *nomadWorld) { reportsNewer(w, "prod-servers-0") }, biggerWorkers,
			"update would create node prod-workers-2: tent never moves a node to an older Nomad: the cluster's " +
				"version is 2.0.6, and server prod-servers-0.global runs 2.0.7; set spec.nomad.version to 2.0.7 or newer"},
		{"a client beside a node", func(w *nomadWorld) { nodeReportsNewer(w, "prod-workers-1") }, biggerWorkers,
			"update would create node prod-workers-2: tent never moves a node to an older Nomad: the cluster's " +
				"version is 2.0.6, and node prod-workers-1 runs 2.0.7; set spec.nomad.version to 2.0.7 or newer"},
		{"a server beside a server", func(w *nomadWorld) { reportsNewer(w, "prod-servers-1") }, fiveServers,
			"update would create node prod-servers-3: tent never moves a node to an older Nomad: the cluster's " +
				"version is 2.0.6, and server prod-servers-1.global runs 2.0.7; set spec.nomad.version to 2.0.7 or newer"},
		{"a client beside a node that is down", func(w *nomadWorld) {
			w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Version, n.Status = newerNomad, "down" })
		}, biggerWorkers, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := olderWorld(t)
				tc.setup(b.w)
				mustReplace(t, b.w.svc, tc.docs(t)...)
				if tc.refuse != "" {
					wantUpdateRefused(t, b, tc.refuse)
					return
				}
				if _, err := b.w.svc.Update(t.Context(), "prod", false); err != nil {
					t.Fatalf("Update without apply: %v", err)
				}
			})
		})
	}
}

// biggerWorkers is the spec of a worker group of three; fiveServers that of a server group of five.
func biggerWorkers(t *testing.T) []string {
	return []string{edit(t, workersYAML, "size: 2", "size: 3")}
}
func fiveServers(t *testing.T) []string {
	return []string{edit(t, serversYAML, "size: 3", "size: 5")}
}

// TestUpdateAsksNothingOfAClusterThatCannotAnswer plans a boot and a pin that goes down with no check and no read
// when the store holds no mark of the bootstrap, and when no server of the cluster runs.
func TestUpdateAsksNothingOfAClusterThatCannotAnswer(t *testing.T) {
	t.Parallel()
	t.Run("a first build", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t)
			w.SetHook(func(_ context.Context, c nomadfake.Call, _ func(context.Context) error) error {
				t.Errorf("the plan called Nomad: %v", c)
				return errBoom
			})

			if _, err := svc.Update(t.Context(), "prod", false); err != nil {
				t.Fatalf("Update without apply: %v", err)
			}
		})
	})
	t.Run("the mark is gone", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := upgradedWorld(t)
			reportsNewer(b.w, "prod-servers-0")
			if err := b.w.svc.Store.Delete(t.Context(), markPath); err != nil {
				t.Fatal(err)
			}
			specsAtVersion(t, b.w.svc, olderNomad)
			before := len(b.w.Log())

			plan, err := b.w.svc.Update(t.Context(), "prod", false)

			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}
			if !plan.Completed {
				t.Errorf("the plan does not write the completed spec: %s", planText(t, plan))
			}
			if calls := nomadNames(b.w, before); len(calls) != 0 {
				t.Errorf("the plan called Nomad: %v", calls)
			}
		})
	})
	t.Run("no server runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := upgradedWorld(t)
			reportsNewer(b.w, "prod-servers-0")
			for _, name := range serverNames {
				b.haltInstance(t, b.serverOf(t, instanceNamed(t, b.f, name)))
			}
			specsAtVersion(t, b.w.svc, olderNomad)
			before := len(b.w.Log())

			plan, err := b.w.svc.Update(t.Context(), "prod", false)

			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}
			if !plan.Completed {
				t.Errorf("the plan does not write the completed spec: %s", planText(t, plan))
			}
			if calls := nomadNames(b.w, before); len(calls) != 0 {
				t.Errorf("the plan called Nomad: %v", calls)
			}
		})
	})
}

// TestUpdateFailsWhenItCannotAskNomadWhichVersionsRun ends the plan of a boot, of a pin that goes down and of a pin
// that goes up when the read of Nomad fails, before anything is written.
func TestUpdateFailsWhenItCannotAskNomadWhichVersionsRun(t *testing.T) {
	t.Parallel()
	const cause = ", and tent could not ask Nomad which versions run: read the Raft configuration: boom"
	for _, tc := range []struct {
		name  string
		world func(*testing.T) liveWorld
		setup func(*testing.T, liveWorld)
		want  string
	}{
		{"a boot", olderWorld, func(t *testing.T, b liveWorld) { mustReplace(t, b.w.svc, biggerWorkers(t)...) },
			"update would create node prod-workers-2" + cause},
		{"a pin that goes down", upgradedWorld, func(t *testing.T, b liveWorld) { specsAtVersion(t, b.w.svc, olderNomad) },
			"update would lower the Nomad version of cluster prod from 2.0.7 to 2.0.6" + cause},
		{"a pin that goes up", olderWorld, func(t *testing.T, b liveWorld) {
			b.w.svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
			specsAtVersion(t, b.w.svc, newerNomad)
		}, "update would change the Nomad version of cluster prod from 2.0.6 to 2.0.7" + cause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := tc.world(t)
				tc.setup(t, b)
				stored, calls := snapshot(t, b.w.svc.Store), len(b.f.Calls())
				for _, apply := range []bool{false, true} {
					b.w.Fail(t, "Peers", errBoom)
					_, err := b.w.svc.Update(t.Context(), "prod", apply)
					wantError(t, err, tc.want)
					if !errors.Is(err, errBoom) {
						t.Errorf("Update(apply %t) = %v, want an error that matches %v", apply, err, errBoom)
					}
				}
				wantSnapshot(t, b.w.svc.Store, stored)
				wantNoWrites(t, b.f.Calls()[calls:])
			})
		})
	}
}

// TestUpdateKeepsTheWarningOfAPlanThatNeedsNoVersions asks Nomad for the nodes alone, once, when a plan neither boots
// a node nor changes the version, and goes on with a warning when that read fails.
func TestUpdateKeepsTheWarningOfAPlanThatNeedsNoVersions(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		mustReplace(t, svc, edit(t, keyedClusterYAML, "sshKeys:", "access:\n    api: [203.0.113.0/24]\n  sshKeys:"))
		before := len(b.w.Log())
		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if diff := cmp.Diff([]string{"Nodes"}, nomadNames(b.w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}

		var warned []string
		svc.OnWarning = func(msg string) { warned = append(warned, msg) }
		b.w.Fail(t, "Nodes", errBoom)
		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		want := []string{"tent could not ask Nomad whether a node of a deleted machine is left: boom"}
		if diff := cmp.Diff(want, warned); diff != "" {
			t.Errorf("the warnings (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateReadsWhatARollObservesOnceForARemovalAndAMove plans a pin that goes up together with the removal of a
// client: the reading that the removal needs serves the version checks, and nothing is read twice.
func TestUpdateReadsWhatARollObservesOnceForARemovalAndAMove(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		mustReplace(t, svc, withVersion(t, keyedClusterYAML, newerNomad), edit(t, workersYAML, "size: 2", "size: 1"))
		before := len(b.w.Log())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if !slices.ContainsFunc(plan.Nodes, func(c app.NodeChange) bool { return c.Drain != "" }) {
			t.Fatalf("the plan removes no client: %s", planText(t, plan))
		}
		want := []string{"Peers", "Health", "Members", "Nodes"}
		if diff := cmp.Diff(want, nomadNames(b.w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateHoldsAVersionWithoutAStoredPinAgainstNomad refuses to write an older Nomad version into the completed
// spec of a built cluster whose store lacks it, while a server reports a newer one.
func TestUpdateHoldsAVersionWithoutAStoredPinAgainstNomad(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := upgradedWorld(t)
		reportsNewer(b.w, "prod-servers-0")
		if err := b.w.svc.Store.Delete(t.Context(), completedPath); err != nil {
			t.Fatal(err)
		}
		specsAtVersion(t, b.w.svc, olderNomad)
		wantUpdateRefused(t, b, "update would set the Nomad version of cluster prod to 2.0.6: tent never moves a node "+
			"to an older Nomad: the cluster's version is 2.0.6, and server prod-servers-0.global runs 2.0.7; "+
			"set spec.nomad.version to 2.0.7 or newer")
	})
}

// aheadStop is the error of an apply that stops before the client node called node, beside servers on Nomad 2.0.6.
func aheadStop(node string) string {
	return "update stops before node " + node + ": a new node would run Nomad 2.0.7, newer than the 2.0.6 of server " +
		"prod-servers-0.global; run tent rolling-update cluster, then tent update cluster again"
}

// aheadWarning is the warning of a plan that creates the client node called node, beside servers on Nomad 2.0.6.
func aheadWarning(node string) string {
	return "node " + node + " waits for the servers: a new node would run Nomad 2.0.7, newer than the 2.0.6 of " +
		"server prod-servers-0.global; this update stops before it: run tent rolling-update cluster, then tent " +
		"update cluster again"
}

// waitWarnings makes svc keep the warnings that say a client waits for the servers, and returns the pointer to them.
func waitWarnings(svc *app.Service) *[]string {
	var warned []string
	svc.OnWarning = func(msg string) {
		if strings.Contains(msg, " waits for the servers: ") {
			warned = append(warned, msg)
		}
	}
	return &warned
}

// wantAllOn fails the test unless every server and every node of the world reports Nomad v and every group has its
// machines, and the three commands have nothing left to do.
func wantAllOn(t *testing.T, b liveWorld, v string) {
	t.Helper()
	for name, got := range b.serverVersions(t) {
		if got != v {
			t.Errorf("server %s reports Nomad %s, want %s", name, got, v)
		}
	}
	for name, got := range b.listedVersions(t) {
		if got != v {
			t.Errorf("node %s reports Nomad %s, want %s", name, got, v)
		}
	}
	wantConverged(t, b.w.svc)
	plan, err := b.w.svc.RollingUpdate(t.Context(), "prod", app.RollOptions{})
	if err != nil || plan.Next != nil {
		t.Errorf("RollingUpdate without apply = %+v, %v; want no next step", plan, err)
	}
}

// TestUpdateStopsBeforeAClientBesideAnOlderServer follows the first sequence of the design: a server machine and a
// worker machine are lost after an upgrade was applied. The plan creates both and warns once that the worker waits;
// the apply creates the server, joins it, and stops before the worker. The roll then ends the upgrade, and one more
// update has no change.
func TestUpdateStopsBeforeAClientBesideAnOlderServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := upgradedWorld(t)
		svc := b.w.svc
		for _, name := range []string{"prod-servers-2", "prod-workers-1"} {
			if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, name)); err != nil {
				t.Fatal(err)
			}
		}
		warned := waitWarnings(svc)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if got := plan.Nodes; len(got) != 2 || got[0].Name != "prod-servers-3" || got[1].Name != "prod-workers-1" {
			t.Fatalf("the plan is:\n%s\nwant the creates of prod-servers-3 and prod-workers-1", planText(t, plan))
		}
		if diff := cmp.Diff([]string{aheadWarning("prod-workers-1")}, *warned); diff != "" {
			t.Errorf("the warnings of the plan (-want +got):\n%s", diff)
		}

		*warned = nil
		_, err = svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-1"))
		if diff := cmp.Diff([]string{aheadWarning("prod-workers-1")}, *warned); diff != "" {
			t.Errorf("the warnings of the apply (-want +got):\n%s", diff)
		}
		wantJoined(t, b.f, "prod-servers-0", "prod-servers-1", "prod-servers-3", "prod-workers-0")
		if got := machinesOf(b.f, "workers"); !slices.Equal(got, []string{"prod-workers-0"}) {
			t.Errorf("the workers are %v, want prod-workers-0 alone", got)
		}

		if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// TestUpdateStopsBeforeTheCreateOfAClientThatNeverRegistered follows the second sequence of the design: a worker that
// never registered is older than 31 minutes when the upgrade is written. The apply writes the completed spec and
// deletes that machine, then stops before the create that replaces it. The roll then ends the upgrade.
func TestUpdateStopsBeforeTheCreateOfAClientThatNeverRegistered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		w.NodeVersions()
		svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
		stopAtWorker1(t, svc, f, w)
		sleepUntilAge(t, f, "instance-5", oldClient)
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		mustReplace(t, svc, withVersion(t, keyedClusterYAML, newerNomad))
		warned := waitWarnings(svc)

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-1"))
		if !plan.Completed || len(plan.Nodes) != 2 || plan.Nodes[0].Action != app.NodeDelete {
			t.Errorf("the plan is:\n%s\nwant the completed spec, the delete of prod-workers-1 and its create",
				planText(t, plan))
		}
		if diff := cmp.Diff([]string{aheadWarning("prod-workers-1")}, *warned); diff != "" {
			t.Errorf("the warnings (-want +got):\n%s", diff)
		}
		wantPinned(t, svc, newerNomad)
		if got := machinesOf(f, "workers"); !slices.Equal(got, []string{"prod-workers-0"}) {
			t.Errorf("the workers are %v, want prod-workers-0 alone: the delete stands and the create does not", got)
		}

		b := liveOf(t, svc, f, w)
		if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// TestUpdateStopsBeforeANewWorkerOfAnUpgradeAndALargerGroup writes an upgrade and a larger worker group in one update:
// the completed spec is written, the run stops before the first new worker and creates none, and the roll fills the
// group on the new version.
func TestUpdateStopsBeforeANewWorkerOfAnUpgradeAndALargerGroup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		mustReplace(t, svc, withVersion(t, keyedClusterYAML, newerNomad), edit(t, workersYAML, "size: 2", "size: 4"))
		creates := countCalls(b.f, "CreateInstance")

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-2"))
		if !plan.Completed {
			t.Errorf("the plan does not write the completed spec: %s", planText(t, plan))
		}
		wantPinned(t, svc, newerNomad)
		if n := countCalls(b.f, "CreateInstance"); n != creates {
			t.Errorf("the update sent %d create requests, want none", n-creates)
		}

		if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if got := machinesOf(b.f, "workers"); len(got) != 4 {
			t.Errorf("the workers are %v, want four", got)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// TestUpdateCreatesServersOfAnUpgradeThatIsNotRolled grows the server group from 3 to 5 in an upgrade that was not
// rolled: update creates two servers on the new version, which join the three old ones, and the roll then replaces the
// three old servers.
func TestUpdateCreatesServersOfAnUpgradeThatIsNotRolled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		b.moveToNewer(t, 5, 2)
		want := map[string]string{
			"prod-servers-0": olderNomad, "prod-servers-1": olderNomad, "prod-servers-2": olderNomad,
			"prod-servers-3": newerNomad, "prod-servers-4": newerNomad,
		}
		if diff := cmp.Diff(want, b.serverVersions(t)); diff != "" {
			t.Errorf("the versions of the servers after the update (-want +got):\n%s", diff)
		}

		if _, err := b.w.svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// TestUpdateStopsBeforeAClientWhenAServerReportsNoVersion stops the apply, after a plan that stands and warns, while
// a server reports no Nomad version yet. The next update creates the client.
func TestUpdateStopsBeforeAClientWhenAServerReportsNoVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		b.w.ChangeServer("prod-servers-1", func(h *nomadops.ServerHealth) { h.Version = "" })
		mustReplace(t, svc, biggerWorkers(t)...)
		const reason = "a server reports no Nomad version yet"
		warned := waitWarnings(svc)

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "update stops before node prod-workers-2: "+reason+"; run tent update cluster again")
		want := "node prod-workers-2 waits for the servers: " + reason +
			"; this update stops before it: run tent update cluster again"
		if diff := cmp.Diff([]string{want}, *warned); diff != "" {
			t.Errorf("the warnings (-want +got):\n%s", diff)
		}
		if got := machinesOf(b.f, "workers"); len(got) != 2 {
			t.Errorf("the workers are %v, want the two that stood", got)
		}

		b.w.ChangeServer("prod-servers-1", func(h *nomadops.ServerHealth) { h.Version = olderNomad })
		mustUpdate(t, svc)
		wantConverged(t, svc)
	})
}

// failedVersionReads are the ways in which the apply cannot ask Nomad for the servers' versions: fail breaks one of
// them, and cause ends the error of the run.
var failedVersionReads = []struct {
	name  string
	fail  func(*testing.T, *nomadWorld)
	cause string
}{
	{"Peers", func(t *testing.T, w *nomadWorld) { w.Fail(t, "Peers", errBoom) }, "boom"},
	{"Health", func(t *testing.T, w *nomadWorld) { w.Fail(t, "Health", errBoom) }, "boom"},
	{"the client", func(_ *testing.T, w *nomadWorld) {
		w.svc.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errBoom }
	}, "reach server prod-servers-0: boom"},
}

// cannotAskStop is the error of an apply that could not ask Nomad for the servers' versions before the client node
// called node, because of cause.
func cannotAskStop(node, cause string) string {
	return "update stops before node " + node + ": tent could not ask Nomad which versions its servers run: " + cause
}

// TestUpdateStopsBeforeAClientWhenNomadCannotBeAsked fails the read of the servers' versions in the apply, after the
// plans have read them, and the making of the client of Nomad: the run stops before the client with the cause,
// nothing is created, and the next update works.
func TestUpdateStopsBeforeAClientWhenNomadCannotBeAsked(t *testing.T) {
	t.Parallel()
	for _, tc := range failedVersionReads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := olderWorld(t)
				svc := b.w.svc
				nomad := svc.Nomad
				mustReplace(t, svc, biggerWorkers(t)...)
				svc.OnUpdatePlan = func(app.UpdatePlan) error {
					tc.fail(t, b.w)
					return nil
				}
				creates := countCalls(b.f, "CreateInstance")

				_, err := svc.Update(t.Context(), "prod", true)

				wantError(t, err, cannotAskStop("prod-workers-2", tc.cause))
				if !errors.Is(err, errBoom) {
					t.Errorf("Update = %v, want an error that matches %v", err, errBoom)
				}
				if n := countCalls(b.f, "CreateInstance"); n != creates {
					t.Errorf("the update sent %d create requests, want none", n-creates)
				}
				svc.OnUpdatePlan, svc.Nomad = nil, nomad
				mustUpdate(t, svc)
				wantConverged(t, svc)
			})
		})
	}
}

// applyNomadCalls returns the names of the Nomad calls that the apply of svc.Update makes, which come after the two
// plans of an applied update.
func applyNomadCalls(t *testing.T, w *nomadWorld) []string {
	t.Helper()
	var from int
	w.svc.OnUpdatePlan = func(app.UpdatePlan) error {
		from = len(w.Log())
		return nil
	}
	if _, err := w.svc.Update(t.Context(), "prod", true); err != nil {
		t.Fatalf("Update: %v", err)
	}
	return nomadNames(w, from)
}

// TestUpdateReadsTheServersVersionsOnceBeforeTheFirstClient reads the Raft configuration and autopilot's report once
// in a run that boots two clients, before the first of them, and makes no such read in a run that boots no client.
func TestUpdateReadsTheServersVersionsOnceBeforeTheFirstClient(t *testing.T) {
	t.Parallel()
	t.Run("two clients", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := olderWorld(t)
			mustReplace(t, b.w.svc, edit(t, workersYAML, "size: 2", "size: 4"))
			warned := waitWarnings(b.w.svc)

			calls := applyNomadCalls(t, b.w)

			if len(calls) < 2 || calls[0] != "Peers" || calls[1] != "Health" {
				t.Errorf("the Nomad calls of the apply are %v, want Peers and Health first", calls)
			}
			for _, name := range []string{"Peers", "Health"} {
				if n := count(calls, name); n != 1 {
					t.Errorf("the apply made %d %s calls, want 1: %v", n, name, calls)
				}
			}
			if len(*warned) != 0 {
				t.Errorf("a plan beside servers of its version warned: %q", *warned)
			}
		})
	})
	t.Run("no client", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := olderWorld(t)
			mustReplace(t, b.w.svc, edit(t, serversYAML, "size: 3", "size: 5"))

			calls := applyNomadCalls(t, b.w)

			if n := count(calls, "Peers"); n != 1 {
				t.Errorf("the apply made %d Peers calls, want the one of the Nomad step: %v", n, calls)
			}
		})
	})
}

// TestUpdateWaitsForAClientThatExistsBesideOlderServers waits for a client whose machine exists and labels it, in an
// upgrade that is not rolled: the run boots no client, so it does not read the servers' versions and does not stop.
func TestUpdateWaitsForAClientThatExistsBesideOlderServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		w.NodeVersions()
		svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
		w.Withhold("prod-workers-1")
		if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the build: %v, want a deadline", err)
		}
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		specsAtVersion(t, svc, newerNomad)
		w.Release("prod-workers-1")

		calls := applyNomadCalls(t, w)

		if slices.Contains(calls, "Peers") || slices.Contains(calls, "Health") {
			t.Errorf("the Nomad calls of the apply are %v, want no read of the servers' versions", calls)
		}
		wantJoined(t, f, allNames...)
		wantPinned(t, svc, newerNomad)
	})
}

// TestUpdateStopsBeforeTheWaitThatRepeatsTheCreateOfAClient cuts the create of a third worker after its request and
// then moves the specs to the newer Nomad: the next apply stops before the wait that repeats that create, the machine
// stays without the joined label, and the roll ends the upgrade with three workers.
func TestUpdateStopsBeforeTheWaitThatRepeatsTheCreateOfAClient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		mustReplace(t, svc, biggerWorkers(t)...)
		b.f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name != "CreateInstance" {
				return next(ctx)
			}
			_ = next(ctx)
			return errCutRun
		})
		if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, errCutRun) {
			t.Fatalf("the update returned %v, want an error that matches %v", err, errCutRun)
		}
		b.f.SetHook(nil)
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		specsAtVersion(t, svc, newerNomad)

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-2"))
		if got := plan.Nodes; len(got) != 1 || got[0].Action != app.NodeWait || got[0].Op == "" {
			t.Fatalf("the plan is:\n%s\nwant the wait that repeats the create of prod-workers-2", planText(t, plan))
		}
		wantJoined(t, b.f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0", "prod-workers-1")

		if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if got := machinesOf(b.f, "workers"); len(got) != 3 {
			t.Errorf("the workers are %v, want three", got)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// neverJoinedServerWorld is the cluster of an upgrade that update applied (the pin is 2.0.7, every node runs 2.0.6),
// after a roll that gave up because its new server, prod-servers-3, never came up: that machine is a surplus one that
// never joined. It returns the world and the ID of that machine.
func neverJoinedServerWorld(t *testing.T) (liveWorld, string) {
	t.Helper()
	svc, f, w := newRelease(t)
	w.NodeVersions()
	w.ServersOverTime()
	svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
	mustUpdate(t, svc)
	b := liveOf(t, svc, f, w)
	svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
	if _, err := svc.UpgradeCluster(t.Context(), "prod", true); err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	mustUpdate(t, svc)
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		err := next(ctx)
		if c.Name != "CreateInstance" {
			return err
		}
		for _, in := range f.Instances() {
			if in.Hostname == "prod-servers-3" && !f.Halted(in.ID) {
				_ = f.HaltInstance(ctx, in.ID)
			}
		}
		return err
	})
	if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err == nil {
		t.Fatal("the roll whose new server never comes up did not fail")
	}
	f.SetHook(nil)
	return b, instanceNamed(t, f, "prod-servers-3")
}

// TestUpdateDeletesTheMachineThatNeverJoinedWhenItStopsBeforeAClient loses a worker machine beside a server that never
// joined. The plan creates the worker before it deletes that server; the apply stops before the worker, deletes the
// server's machine, and returns the stop. The roll then ends the upgrade, and one more update has no change.
func TestUpdateDeletesTheMachineThatNeverJoinedWhenItStopsBeforeAClient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b, broken := neverJoinedServerWorld(t)
		svc := b.w.svc
		if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, "prod-workers-1")); err != nil {
			t.Fatal(err)
		}

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-1"))
		if slices.Contains(instanceIDs(b.f), broken) {
			t.Errorf("the machine of prod-servers-3 that never joined (%s) is still there", broken)
		}
		if got := machinesOf(b.f, "workers"); !slices.Equal(got, []string{"prod-workers-0"}) {
			t.Errorf("the workers are %v, want prod-workers-0 alone: the create is held back", got)
		}

		if _, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		wantAllOn(t, b, newerNomad)
	})
}

// TestUpdateStopsBeforeEveryClientAndReadsTheVersionsOnce loses both workers beside a server that never joined: the
// plan has two creates of clients and the delete after them. The apply creates neither client, makes the delete, and
// reads the servers' versions once.
func TestUpdateStopsBeforeEveryClientAndReadsTheVersionsOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b, broken := neverJoinedServerWorld(t)
		svc := b.w.svc
		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, name)); err != nil {
				t.Fatal(err)
			}
		}
		creates := countCalls(b.f, "CreateInstance")
		var from int
		svc.OnUpdatePlan = func(app.UpdatePlan) error {
			from = len(b.w.Log())
			return nil
		}

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-0"))
		if n := countCalls(b.f, "CreateInstance"); n != creates {
			t.Errorf("the update sent %d create requests, want none", n-creates)
		}
		if slices.Contains(instanceIDs(b.f), broken) {
			t.Errorf("the machine of prod-servers-3 that never joined (%s) is still there", broken)
		}
		// The guard of the delete reads the Raft configuration too, so Health counts the reads of the versions.
		if calls := nomadNames(b.w, from); count(calls, "Health") != 1 {
			t.Errorf("the apply made %d Health calls, want 1: %v", count(calls, "Health"), calls)
		}
	})
}

// TestUpdateWaitsForAClientThatExistsAfterTheStop plans the wait that repeats the create of prod-workers-0 and, after
// it, the wait for prod-workers-1, whose machine exists: the run stops before the first, carries out the second, and
// returns the stop.
func TestUpdateWaitsForAClientThatExistsAfterTheStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, "prod-workers-0")); err != nil {
			t.Fatal(err)
		}
		b.f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name != "CreateInstance" {
				return next(ctx)
			}
			_ = next(ctx)
			return errCutRun
		})
		if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, errCutRun) {
			t.Fatalf("the update returned %v, want an error that matches %v", err, errCutRun)
		}
		b.f.SetHook(nil)
		worker1 := instanceNamed(t, b.f, "prod-workers-1")
		tags, _ := tagsOf(b.f, worker1)
		b.f.SetInstanceTags(t, worker1, withoutJoined(tags)...)
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		specsAtVersion(t, svc, newerNomad)

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-0"))
		if got := plan.Nodes; len(got) != 2 || got[0].Name != "prod-workers-0" || got[0].Op == "" ||
			got[1].Name != "prod-workers-1" || got[1].Op != "" {
			t.Fatalf("the plan is:\n%s\nwant the wait that repeats the create of prod-workers-0, then the wait for "+
				"prod-workers-1", planText(t, plan))
		}
		wantJoined(t, b.f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-1")
	})
}

// TestUpdateEndsTheRunAtOnceWhenItCannotReadTheVersionsBeforeTheDeletes fails the read of the servers' versions in the
// apply, and the making of the client of Nomad: the run ends before the delete of the machine that never joined.
func TestUpdateEndsTheRunAtOnceWhenItCannotReadTheVersionsBeforeTheDeletes(t *testing.T) {
	t.Parallel()
	for _, tc := range failedVersionReads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b, broken := neverJoinedServerWorld(t)
				svc := b.w.svc
				if err := b.f.DeleteInstance(t.Context(), instanceNamed(t, b.f, "prod-workers-1")); err != nil {
					t.Fatal(err)
				}
				svc.OnUpdatePlan = func(app.UpdatePlan) error {
					tc.fail(t, b.w)
					return nil
				}

				_, err := svc.Update(t.Context(), "prod", true)

				wantError(t, err, cannotAskStop("prod-workers-1", tc.cause))
				if !slices.Contains(instanceIDs(b.f), broken) {
					t.Errorf("the machine of prod-servers-3 (%s) was deleted, want the run to end before the deletes", broken)
				}
			})
		})
	}
}

// TestUpdateDoesNotRemoveNodesOrDeleteInfrastructureAfterTheStop loses a worker beside a joined worker of a group that
// the specs lack and an SSH key that the specs lack: the run stops before the worker, and neither removes that node
// nor deletes that key.
func TestUpdateDoesNotRemoveNodesOrDeleteInfrastructureAfterTheStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		batch := edit(t, edit(t, workersYAML, "name: workers", "name: batch"), "size: 2", "size: 1")
		svc, f, w := newRelease(t, keyedClusterYAML, serversYAML, workersYAML, batch)
		w.NodeVersions()
		svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
		mustUpdate(t, svc)
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		mustReplace(t, svc, withVersion(t, edit(t, keyedClusterYAML, "  - "+devKey+"\n", ""), newerNomad))
		if err := svc.Store.Delete(t.Context(), "prod/nodegroups/batch.yaml"); err != nil {
			t.Fatal(err)
		}
		if err := f.DeleteInstance(t.Context(), instanceNamed(t, f, "prod-workers-1")); err != nil {
			t.Fatal(err)
		}
		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil || len(plan.Nodes) != 2 {
			t.Fatalf("the plan is %v, %v\nwant the create of prod-workers-1 and the removal of prod-batch-0",
				planText(t, plan), err)
		}
		keys, machines := len(f.SSHKeys()), len(instanceIDs(f))

		_, err = svc.Update(t.Context(), "prod", true)

		wantError(t, err, aheadStop("prod-workers-1"))
		if got := len(f.SSHKeys()); got != keys {
			t.Errorf("the account has %d SSH keys, want the %d it had: the delete of one does not run", got, keys)
		}
		if got := len(instanceIDs(f)); got != machines {
			t.Errorf("the account has %d machines, want the %d it had: no node is removed", got, machines)
		}
	})
}

// answersOnceTheFirewallHasTheRule makes the Nomad API of w refuse every call with a "not ready" error until the cloud
// f has created a firewall rule, and returns a function that tells whether it was created. The rule stands for the
// operator's new address in spec.access.api. When atRule is not nil, it runs just before the request that creates the
// rule.
func answersOnceTheFirewallHasTheRule(f *vultrfake.Fake, w *nomadWorld, atRule func()) (reachable func() bool) {
	var open atomic.Bool
	w.SetHook(func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
		if !open.Load() {
			return errNotReady
		}
		return next(ctx)
	})
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		if c.Name == "CreateFirewallRule" && atRule != nil {
			atRule()
		}
		err := next(ctx)
		if c.Name == "CreateFirewallRule" && err == nil {
			open.Store(true)
		}
		return err
	})
	return open.Load
}

// withNewAddress is the test cluster's Cluster with the operator's address range in spec.access.api.
func withNewAddress(t *testing.T, doc string) string {
	t.Helper()
	return edit(t, doc, "sshKeys:", "access:\n    api: [203.0.113.0/24]\n  sshKeys:")
}

// loseWorker deletes the machine of prod-workers-1.
func loseWorker(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	if err := f.DeleteInstance(t.Context(), instanceNamed(t, f, "prod-workers-1")); err != nil {
		t.Fatal(err)
	}
}

// unaskedWarnings makes svc keep the warnings that say the plan could not ask Nomad which versions run, and returns
// the pointer to them.
func unaskedWarnings(svc *app.Service) *[]string {
	var warned []string
	svc.OnWarning = func(msg string) {
		if strings.HasPrefix(msg, "tent could not ask Nomad which versions run: ") {
			warned = append(warned, msg)
		}
	}
	return &warned
}

// askAgainSuffix ends the warning of a plan that could not ask Nomad which versions run and changes the infrastructure.
const askAgainSuffix = "; update asks again once it has applied the infrastructure's changes, before it writes the " +
	"completed spec or creates a node"

// neverAnswers makes the Nomad API of w refuse every call with a "not ready" error.
func neverAnswers(w *nomadWorld) {
	w.SetHook(func(context.Context, nomadfake.Call, func(context.Context) error) error { return errNotReady })
}

// wantUnasked fails the test unless warned holds one warning that ends the way a plan ends that could not ask.
func wantUnasked(t *testing.T, warned []string) {
	t.Helper()
	if len(warned) != 1 || !strings.HasSuffix(warned[0], askAgainSuffix) ||
		!strings.Contains(warned[0], "no server is ready") {
		t.Errorf("the warnings = %q, want one that names the cause (no server is ready) and ends with %q", warned,
			askAgainSuffix)
	}
}

// TestUpdateAppliesTheFirewallBeforeAskingNomadWhenNoServerAnswers loses a worker and gives spec.access.api a new
// address, with a Nomad API that answers only once the firewall has the rule. The plan lists both changes and warns
// once; the apply creates the rule, asks Nomad, creates the worker and ends applied; the next plan has no change.
func TestUpdateAppliesTheFirewallBeforeAskingNomadWhenNoServerAnswers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		loseWorker(t, b.f)
		reachable := answersOnceTheFirewallHasTheRule(b.f, b.w, nil)
		mustReplace(t, svc, withNewAddress(t, keyedClusterYAML))
		warned := unaskedWarnings(svc)

		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil || !plan.Infra.HasChanges() || len(plan.Nodes) != 1 || plan.Nodes[0].Name != "prod-workers-1" {
			t.Fatalf("Update without apply = %v, %v\nwant the rule and the create of prod-workers-1", planText(t, plan), err)
		}
		wantUnasked(t, *warned)
		*warned = nil

		plan, err = svc.Update(t.Context(), "prod", true)

		if err != nil || !plan.Applied {
			t.Fatalf("Update = applied %t, %v; want an applied plan", plan.Applied, err)
		}
		wantUnasked(t, *warned)
		if !reachable() {
			t.Error("the firewall has no rule for the new address")
		}
		if got := machinesOf(b.f, "workers"); !slices.Equal(got, []string{"prod-workers-0", "prod-workers-1"}) {
			t.Errorf("the workers are %v, want both", got)
		}
		wantConverged(t, svc)
	})
}

// TestUpdateWritesThePinAfterTheFirewallWhenNoServerAnswers is the same with an upgrade in place of the lost worker:
// the rule is in the firewall before the completed spec moves to the new version.
func TestUpdateWritesThePinAfterTheFirewallWhenNoServerAnswers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
		if _, err := svc.UpgradeCluster(t.Context(), "prod", true); err != nil {
			t.Fatalf("UpgradeCluster: %v", err)
		}
		var completedAtTheRule []byte
		answersOnceTheFirewallHasTheRule(b.f, b.w, func() {
			completedAtTheRule, _, _ = svc.Store.Get(t.Context(), completedPath)
		})
		mustReplace(t, svc, withNewAddress(t, withVersion(t, keyedClusterYAML, newerNomad)))
		warned := unaskedWarnings(svc)

		plan, err := svc.Update(t.Context(), "prod", true)

		if err != nil || !plan.Applied {
			t.Fatalf("Update = applied %t, %v; want an applied plan", plan.Applied, err)
		}
		wantUnasked(t, *warned)
		if got := decode(t, string(completedAtTheRule)).Cluster.Spec.Nomad.Version; got != olderNomad {
			t.Errorf("the completed spec held %q when the rule was made, want %q", got, olderNomad)
		}
		wantPinned(t, svc, newerNomad)
		wantConverged(t, svc)
	})
}

// loseWorkerAndGiveANewAddress is the cluster of the tests below: a worker is lost, spec.access.api has a new address,
// and the Nomad API answers only once the firewall has the rule. It returns the world and the function that tells
// whether the rule exists.
func loseWorkerAndGiveANewAddress(t *testing.T) (b liveWorld, reachable func() bool) {
	t.Helper()
	b = olderWorld(t)
	loseWorker(t, b.f)
	reachable = answersOnceTheFirewallHasTheRule(b.f, b.w, nil)
	mustReplace(t, b.w.svc, withNewAddress(t, keyedClusterYAML))
	return b, reachable
}

// onlyWorkerZero fails the test unless prod-workers-0 is the one machine of the worker group.
func onlyWorkerZero(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	if got := machinesOf(f, "workers"); !slices.Equal(got, []string{"prod-workers-0"}) {
		t.Errorf("the workers are %v, want prod-workers-0 alone", got)
	}
}

// unaskedStart starts the error of a plan or an apply that boots prod-workers-1 and could not read Nomad.
const unaskedStart = "update would create node prod-workers-1, and tent could not ask Nomad which versions run: "

// wantUnaskedError fails the test unless err starts the way an unread Nomad starts it, ends with the hint and matches
// ErrNotReady.
func wantUnaskedError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), unaskedStart) || !strings.HasSuffix(err.Error(), hintOfAccess) ||
		!errors.Is(err, nomadops.ErrNotReady) {
		t.Errorf("error = %v\nwant one that starts with %q, ends with %q and matches %v", err, unaskedStart,
			hintOfAccess, nomadops.ErrNotReady)
	}
}

// TestUpdateStopsAtTheOwedCheckWhenTheSpecsVersionIsOlderThanAServer reads Nomad after the firewall has the rule and
// finds a server on a newer Nomad than the specs ask for: the run ends there with the refusal of a plan, the rule is
// in the firewall, and the completed spec and the nodes are as before. The next plan refuses with the same text and
// writes nothing.
func TestUpdateStopsAtTheOwedCheckWhenTheSpecsVersionIsOlderThanAServer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b, reachable := loseWorkerAndGiveANewAddress(t)
		svc := b.w.svc
		reportsNewer(b.w, "prod-servers-0")
		if err := svc.Store.Delete(t.Context(), namesPath); err != nil { // a raise before the check would write it again
			t.Fatalf("delete %s: %v", namesPath, err)
		}
		stored := snapshot(t, svc.Store)
		want := "update would create node prod-workers-1: tent never moves a node to an older Nomad: the cluster's " +
			"version is 2.0.6, and server prod-servers-0.global runs 2.0.7; set spec.nomad.version to 2.0.7 or newer"

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, want)
		if plan.Applied {
			t.Error("Update applied the plan")
		}
		if !reachable() {
			t.Error("the firewall has no rule for the new address")
		}
		wantSnapshot(t, svc.Store, stored)
		onlyWorkerZero(t, b.f)
		wantUpdateRefused(t, b, want)
	})
}

// TestUpdateStopsAtTheOwedCheckWhenNoServerAnswersAfterTheFirewall keeps Nomad closed after the rule is made: the run
// ends with the error of a read that fails and the hint, and the completed spec and the nodes are as before.
func TestUpdateStopsAtTheOwedCheckWhenNoServerAnswersAfterTheFirewall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		loseWorker(t, b.f)
		neverAnswers(b.w)
		mustReplace(t, svc, withNewAddress(t, keyedClusterYAML))
		stored := snapshot(t, svc.Store)
		rules := countCalls(b.f, "CreateFirewallRule")

		_, err := svc.Update(t.Context(), "prod", true)

		wantUnaskedError(t, err)
		if countCalls(b.f, "CreateFirewallRule") == rules {
			t.Error("the firewall got no rule before the check")
		}
		wantSnapshot(t, svc.Store, stored)
		onlyWorkerZero(t, b.f)

		calls := len(b.f.Calls())
		_, err = svc.Update(t.Context(), "prod", true)
		wantUnaskedError(t, err)
		wantNoWrites(t, b.f.Calls()[calls:])
		wantSnapshot(t, svc.Store, stored)
		b.w.SetHook(nil)
		mustUpdate(t, svc)
		if got := machinesOf(b.f, "workers"); !slices.Equal(got, []string{"prod-workers-0", "prod-workers-1"}) {
			t.Errorf("the workers are %v, want both", got)
		}
		wantConverged(t, svc)
	})
}

// TestUpdateFailsWithTheHintWhenNoServerAnswersAndTheInfrastructureHasNoChange fails the plan of a lost worker when
// no server answers and the infrastructure stays as it is: the error carries the hint of spec.access.api, and nothing
// is written.
func TestUpdateFailsWithTheHintWhenNoServerAnswersAndTheInfrastructureHasNoChange(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		loseWorker(t, b.f)
		neverAnswers(b.w)
		stored := snapshot(t, b.w.svc.Store)
		for _, apply := range []bool{false, true} {
			_, err := b.w.svc.Update(t.Context(), "prod", apply)
			wantUnaskedError(t, err)
		}
		wantSnapshot(t, b.w.svc.Store, stored)
		onlyWorkerZero(t, b.f)
	})
}

// TestUpdateFailsWhenOnlyDeletesChangeTheInfrastructureAndNoServerAnswers does not let the plan stand when the
// infrastructure's changes are deletes alone, which run last: it fails with the hint, as it does for no change.
func TestUpdateFailsWhenOnlyDeletesChangeTheInfrastructureAndNoServerAnswers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		svc := b.w.svc
		loseWorker(t, b.f)
		mustReplace(t, svc, edit(t, keyedClusterYAML, "  - "+devKey+"\n", ""))
		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if s := plan.Infra.Summary(); s.Create+s.Update+s.Replace != 0 || s.Delete == 0 {
			t.Fatalf("the infrastructure's changes are %v, want deletes alone", s)
		}
		neverAnswers(b.w)
		stored := snapshot(t, svc.Store)
		keys := len(b.f.SSHKeys())
		for _, apply := range []bool{false, true} {
			_, err := svc.Update(t.Context(), "prod", apply)
			wantUnaskedError(t, err)
		}
		wantSnapshot(t, svc.Store, stored)
		if got := len(b.f.SSHKeys()); got != keys {
			t.Errorf("the account has %d SSH keys, want the %d it had", got, keys)
		}
	})
}

// TestUpdateFailsWhenTheReadFailsForAnotherReasonAlthoughTheInfrastructureChanges keeps the plan failing, without the
// hint, for a read that fails with an error other than a Nomad that is not ready.
func TestUpdateFailsWhenTheReadFailsForAnotherReasonAlthoughTheInfrastructureChanges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		loseWorker(t, b.f)
		mustReplace(t, b.w.svc, withNewAddress(t, keyedClusterYAML))
		stored := snapshot(t, b.w.svc.Store)
		rules := countCalls(b.f, "CreateFirewallRule")
		const want = unaskedStart + "read the Raft configuration: boom"
		for _, apply := range []bool{false, true} {
			b.w.Fail(t, "Peers", errBoom)
			_, err := b.w.svc.Update(t.Context(), "prod", apply)
			wantError(t, err, want)
		}
		wantSnapshot(t, b.w.svc.Store, stored)
		if countCalls(b.f, "CreateFirewallRule") != rules {
			t.Error("the firewall got a rule, want none")
		}
	})
}

// TestUpdateKeepsThePinWhenTheOwedCheckRefusesALowerPin puts the old version back into the specs of an upgraded
// cluster together with a new address, beside a server that runs the new version: the rule reaches the firewall, the
// owed check refuses to lower the pin, and the completed spec still holds the new version.
func TestUpdateKeepsThePinWhenTheOwedCheckRefusesALowerPin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := upgradedWorld(t)
		svc := b.w.svc
		reportsNewer(b.w, "prod-servers-0")
		reachable := answersOnceTheFirewallHasTheRule(b.f, b.w, nil)
		mustReplace(t, svc, withNewAddress(t, withVersion(t, keyedClusterYAML, olderNomad)))
		stored := snapshot(t, svc.Store)

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, lowering("server prod-servers-0.global"))
		if !reachable() {
			t.Error("the firewall has no rule for the new address")
		}
		wantSnapshot(t, svc.Store, stored)
		wantPinned(t, svc, newerNomad)
	})
}
