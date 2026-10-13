package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
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

		b := liveWorld{f: f, w: w}
		b.api, err = svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
		if err != nil {
			t.Fatal(err)
		}
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

// TestUpdateStopsBeforeAClientWhenNomadCannotBeAsked fails the read of the servers' versions in the apply, after the
// plans have read them, and the making of the client of Nomad: the run stops before the client with the cause,
// nothing is created, and the next update works.
func TestUpdateStopsBeforeAClientWhenNomadCannotBeAsked(t *testing.T) {
	t.Parallel()
	const stops = "update stops before node prod-workers-2: tent could not ask Nomad which versions its servers run: "
	for _, tc := range []struct {
		name string
		fail func(*testing.T, *nomadWorld)
		want string
	}{
		{"Peers", func(t *testing.T, w *nomadWorld) { w.Fail(t, "Peers", errBoom) }, stops + "boom"},
		{"Health", func(t *testing.T, w *nomadWorld) { w.Fail(t, "Health", errBoom) }, stops + "boom"},
		{"the client", func(_ *testing.T, w *nomadWorld) {
			w.svc.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errBoom }
		}, stops + "reach server prod-servers-0: boom"},
	} {
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

				wantError(t, err, tc.want)
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
