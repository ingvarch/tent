package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
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
