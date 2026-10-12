package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// scaleWorkers returns the workers of the example cluster with size nodes that drain within ten minutes.
func scaleWorkers(t *testing.T, size int) string {
	t.Helper()
	workers := edit(t, exampleWorkersYAML, "size: 2", fmt.Sprintf("size: %d", size))
	return workers + "  rollingUpdate:\n    drainTimeout: 10m\n"
}

// scaleWorld builds the example cluster with three workers on an empty cloud, and gives its drains three reads. The
// specs then ask for size workers.
func scaleWorld(t *testing.T, size int) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	return builtScaleWorld(t, 3, size)
}

// builtScaleWorld is scaleWorld with built workers.
func builtScaleWorld(t *testing.T, built, size int) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newExample(t, exampleClusterYAML, exampleServersYAML, scaleWorkers(t, built))
	w.SetDrainReads(drainReads)
	mustUpdate(t, svc)
	mustReplace(t, svc, scaleWorkers(t, size))
	return svc, f, w
}

// wantScaledDown fails the test unless the run deleted the machines of the plan's node deletes, the cluster is valid,
// a plan of an update has no change, Nomad lists a node of no machine that is gone, and every machine that remains has
// the joined label and the stub.
func wantScaledDown(t *testing.T, svc *app.Service, f *vultrfake.Fake, plan app.UpdatePlan) {
	t.Helper()
	wantRemoved(t, plan, f)
	wantConverged(t, svc)
	v := mustValidate(t, svc)
	if !v.Valid() {
		t.Errorf("the cluster is not valid after the update: %+v", v.Failures)
	}
	var listed, names []string
	for _, in := range f.Instances() {
		listed = append(listed, nodeIDOf(in.ID))
		names = append(names, in.Hostname)
	}
	for _, n := range nomadNodes(t, svc) {
		if !slices.Contains(listed, n.ID) {
			t.Errorf("Nomad lists the node %s (%s) of a machine that is gone", n.Name, n.ID)
		}
	}
	wantJoined(t, f, names...)
}

// TestScaleFlowDrainsTheSurplusClients scales the workers of the built example cluster from 3 to 1 with a drain of ten
// minutes. The plan and the calls to Vultr and Nomad match golden files; the clients keep to the invariants of a
// scale-down at every call; the summary counts two drained and two purged; and one worker is left, ready and eligible.
func TestScaleFlowDrainsTheSurplusClients(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 1)
		watchShrink(t, f, w, "workers", 1)

		plan, calls := updateFlow(t, svc, f, w)

		checkGolden(t, "flow_shrink_clients.plan.golden", planText(t, plan))
		checkGolden(t, "flow_shrink_clients.calls.golden", callsText(calls))
		var applied strings.Builder
		if err := plan.WriteApplied(&applied); err != nil {
			t.Fatalf("WriteApplied: %v", err)
		}
		const want = "Nodes: 0 created, 0 waited for, 2 deleted. Removed: 2 drained, 0 stopped, 2 purged. " +
			"Wrote cluster.completed.yaml.\n"
		if applied.String() != want {
			t.Errorf("the summary is %q, want %q", applied.String(), want)
		}
		wantScaledDown(t, svc, f, plan)
		nodes := nomadNodes(t, svc)
		var workers []string
		for _, n := range nodes {
			if strings.HasPrefix(n.Name, "prod-workers-") {
				workers = append(workers, fmt.Sprintf("%s %s eligible=%v", n.Name, n.Status, n.Eligible))
			}
		}
		if want := []string{"prod-workers-0 ready eligible=true"}; !slices.Equal(workers, want) {
			t.Errorf("Nomad lists the workers %q, want %q", workers, want)
		}
	})
}

// TestScaleFlowReportsEachStepOfTheScaleDown scales the workers from 3 to 1 and checks that the progress lines match
// a golden file: the marks of both nodes, the drain and the delete of each, and the purges.
func TestScaleFlowReportsEachStepOfTheScaleDown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 1)
		watchShrink(t, f, w, "workers", 1)
		lines := recordProgress(svc)

		mustUpdate(t, svc)

		checkGolden(t, "flow_shrink_clients.steps.golden", strings.Join(*lines, "\n")+"\n")
	})
}

// unjoinedScaleWorld is scaleWorld with the worker prod-workers-2 built but never registered, as the specs ask for size
// workers: the build stops at its registration, with its machine ready and without the joined label.
func unjoinedScaleWorld(t *testing.T, size int) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newExample(t, exampleClusterYAML, exampleServersYAML, scaleWorkers(t, 3))
	w.SetDrainReads(drainReads)
	w.Withhold("prod-workers-2")
	if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the build: %v, want a deadline", err)
	}
	mustReplace(t, svc, scaleWorkers(t, size))
	return svc, f, w
}

// TestScaleFlowDeletesTheMachineThatNeverJoinedFirst scales three workers, one of which never registered, down to one:
// the machine that never joined goes without a drain, after the update asked Nomad whether its node exists, and one
// worker that joined is drained and removed.
func TestScaleFlowDeletesTheMachineThatNeverJoinedFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedScaleWorld(t, 1)
		before := len(w.Log())

		plan := mustUpdate(t, svc)

		wantNodeChanges(t, plan, deleteOf("prod-workers-2", "instance-6", "surplus"),
			removalOf("prod-workers-1", "instance-5", "surplus", "10m0s"))
		if want := (app.RemovalCounts{Drained: 1, Purged: 1}); plan.Removed != want {
			t.Errorf("the update removed %+v, want %+v", plan.Removed, want)
		}
		calls := nomadNames(w, before)
		reading := []string{"Peers", "Health", "Members", "Nodes"}
		want := slices.Concat(reading, reading, []string{"Nodes", "Peers"})
		if diff := cmp.Diff(want, calls[:min(len(calls), len(want))]); diff != "" {
			t.Errorf("the Nomad calls before the first delete (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"MarkIneligible", "Drain", "Purge"}, nomadWrites(w, before)); diff != "" {
			t.Errorf("the writes to Nomad (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"instance-6", "instance-5"}, callsOf(f, "DeleteInstance")); diff != "" {
			t.Errorf("the deleted machines (-want +got):\n%s", diff)
		}
		wantNodes(t, f, server(0), server(1), server(2), worker(0))
		wantConverged(t, svc)
	})
}

// TestScaleFlowDeletesAMachineOnceThatTheCloudStillLists makes the cloud list a deleted machine at its next two reads.
// The delete of the machine that never joined comes before the drain of the other worker, and the removals start
// from a list that still shows it: the run sends one delete for it in all, waits until the cloud stops listing it,
// and goes on.
func TestScaleFlowDeletesAMachineOnceThatTheCloudStillLists(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedScaleWorld(t, 1)
		f.SetDeleteReads(t, 2)
		watchShrink(t, f, w, "workers", 1)

		plan := mustUpdate(t, svc)

		if diff := cmp.Diff([]string{"instance-6", "instance-5"}, callsOf(f, "DeleteInstance")); diff != "" {
			t.Errorf("the deleted machines (-want +got):\n%s", diff)
		}
		if want := (app.RemovalCounts{Drained: 1, Purged: 1}); plan.Removed != want {
			t.Errorf("the update removed %+v, want %+v", plan.Removed, want)
		}
		wantNodes(t, f, server(0), server(1), server(2), worker(0))
		wantConverged(t, svc)
	})
}

// TestScaleFlowDrainsAMachineWithoutTheLabelOnTheNextRun scales three workers down to two when the newest has no
// joined label but is registered. The delete finds its node, labels the machine and ends with the advice to update
// again; the next update drains the node and removes the machine.
func TestScaleFlowDrainsAMachineWithoutTheLabelOnTheNextRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 2)
		dropJoinedTag(t, f, "instance-6")
		addr := privateOf(t, f, "instance-6")

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "delete node prod-workers-2 (instance-6): the node has joined Nomad (a registered client at "+
			addr.String()+"); run tent update cluster again, which removes it as a node that joined or says why it cannot")
		wantNodes(t, f, append(slices.Clone(allNodes), worker(2))...)
		wantJoined(t, f, append(slices.Clone(allNames), "prod-workers-2")...)
		wantLockFree(t, svc.Store)
		watchShrink(t, f, w, "workers", 2)

		plan := mustUpdate(t, svc)

		wantNodeChanges(t, plan, removalOf("prod-workers-2", "instance-6", "surplus", "10m0s"))
		wantScaledDown(t, svc, f, plan)
	})
}

// TestScaleFlowPurgesTheNodeThatADeletedMachineLeft cuts a scale-down right after the delete of the machine, whose
// node reads ready for a while. An update then has no change; once Nomad lists the node down, the plan lists its
// purge and exits with changes, the update purges it, and the next one has no change.
func TestScaleFlowPurgesTheNodeThatADeletedMachineLeft(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := scaleWorld(t, 2)
		addr := privateOf(t, f, "instance-6")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.SetHook(atCall(f, "DeleteInstance <prod-workers-2>", 1,
			func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
				_ = next(ctx) // the cloud takes the delete; its answer is lost
				cancel()
				return next(ctx)
			}))
		if _, err := svc.Update(ctx, "prod", true); !errors.Is(err, context.Canceled) {
			t.Fatalf("the cut update returned %v, want an error that matches context.Canceled", err)
		}
		f.SetHook(nil)
		wantLockFree(t, svc.Store)

		for _, apply := range []bool{false, true} {
			plan, err := svc.Update(t.Context(), "prod", apply)
			if err != nil || plan.HasChanges() {
				t.Fatalf("Update(apply %v) while the node reads ready = %+v, %v; want no change", apply, plan.Purges, err)
			}
		}
		time.Sleep(time.Minute)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		want := []app.NodePurge{{Name: "prod-workers-2", Address: addr.String(), ID: "n-instance-6", Group: "workers"}}
		if diff := cmp.Diff(want, plan.Purges); diff != "" {
			t.Errorf("the purges (-want +got):\n%s", diff)
		}
		if got := planText(t, plan); !strings.Contains(got, "- Nomad node prod-workers-2 ("+addr.String()+
			", down, no machine)\n") || !strings.Contains(got, "Nomad nodes: 1 to purge.\n") {
			t.Errorf("the plan is\n%s\nwant the purge of prod-workers-2 and the count", got)
		}
		if !plan.HasChanges() || len(plan.Nodes) != 0 {
			t.Errorf("the plan has changes %v and node changes %+v, want a purge alone", plan.HasChanges(), plan.Nodes)
		}

		plan = mustUpdate(t, svc)

		if want := (app.RemovalCounts{Purged: 1}); plan.Removed != want {
			t.Errorf("the update removed %+v, want %+v", plan.Removed, want)
		}
		var applied strings.Builder
		const summary = "Removed: 0 drained, 0 stopped, 1 purged.\n"
		if err := plan.WriteApplied(&applied); err != nil || applied.String() != summary {
			t.Errorf("the summary is %q, %v; want %q", applied.String(), err, summary)
		}
		wantConverged(t, svc)
		for _, n := range nomadNodes(t, svc) {
			if n.ID == "n-instance-6" {
				t.Errorf("Nomad still lists the node %s", n.ID)
			}
		}
	})
}

// TestScaleFlowLeavesNodesThatItDoesNotKnowAlone plans no purge for a node that is ready and has no machine, for a down
// node whose machine is listed, and for a down node under the name of a server group.
func TestScaleFlowLeavesNodesThatItDoesNotKnowAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		node nomadops.Node
	}{
		{"a ready node without a machine", nomadops.Node{
			ID: "n-ghost", Name: "prod-workers-7", Status: "ready", Eligible: true, Address: netip.MustParseAddr("10.64.0.99"),
		}},
		{"a down node of a server group", nomadops.Node{
			ID: "n-ghost", Name: "prod-servers-9", Status: "down", Address: netip.MustParseAddr("10.64.0.99"),
		}},
		{"a down node whose name is no group's", nomadops.Node{
			ID: "n-ghost", Name: "other-workers-0", Status: "down", Address: netip.MustParseAddr("10.64.0.99"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := scaleWorld(t, 3)
				w.Register(tc.node)
				before, asked := len(f.Calls()), len(w.Log())

				plan, err := svc.Update(t.Context(), "prod", true)

				if err != nil || plan.HasChanges() || len(plan.Purges) != 0 {
					t.Errorf("Update = %+v, %v; want no change and no purge", plan.Purges, err)
				}
				wantNoWrites(t, f.Calls()[before:])
				if got := nomadWrites(w, asked); len(got) != 0 {
					t.Errorf("the update wrote %v to Nomad", got)
				}
			})
		})
	}
}

// TestScaleFlowWarnsOnceWhenItCannotReadTheNodes fails every call to Nomad on a cluster at rest. A plan has no change
// and tells the warning once, with and without apply. An update whose only change is the firewall rule of the Nomad
// API is applied and tells it once. A stale client to judge fails the plan, as before.
func TestScaleFlowWarnsOnceWhenItCannotReadTheNodes(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	fail := func(w *nomadWorld) {
		w.SetHook(func(context.Context, nomadfake.Call, func(context.Context) error) error { return boom })
	}
	const prefix = "tent could not ask Nomad whether a node of a deleted machine is left: "
	wantWarned := func(t *testing.T, warned []string) {
		t.Helper()
		if len(warned) != 1 || !strings.HasPrefix(warned[0], prefix) || !strings.Contains(warned[0], "boom") {
			t.Errorf("the warnings are %q, want one that starts with %q and holds boom", warned, prefix)
		}
	}
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprintf("no change, apply %v", apply), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w := scaleWorld(t, 3)
				var warned []string
				svc.OnWarning = func(msg string) { warned = append(warned, msg) }
				fail(w)

				plan, err := svc.Update(t.Context(), "prod", apply)

				if err != nil || plan.HasChanges() {
					t.Errorf("Update = %v, %v; want no change", plan.HasChanges(), err)
				}
				wantWarned(t, warned)
			})
		})
	}
	t.Run("a change that needs no Nomad", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := scaleWorld(t, 3)
			mustReplace(t, svc, edit(t, exampleClusterYAML, "api: [0.0.0.0/0]", "api: [203.0.113.0/24]"))
			var warned []string
			svc.OnWarning = func(msg string) { warned = append(warned, msg) }
			fail(w)

			plan, err := svc.Update(t.Context(), "prod", true)

			if err != nil || !plan.Applied || !plan.HasChanges() {
				t.Fatalf("Update = applied %v, changes %v, %v; want an applied change", plan.Applied, plan.HasChanges(), err)
			}
			wantWarned(t, slices.DeleteFunc(warned, func(msg string) bool { return !strings.HasPrefix(msg, prefix) }))
			wantNodes(t, f, append(slices.Clone(allNodes), worker(2))...)
		})
	})
	t.Run("a client to judge", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := unjoinedScaleWorld(t, 3)
			sleepUntilAge(t, f, "instance-6", oldClient)
			fail(w)

			_, err := svc.Update(t.Context(), "prod", false)

			const judged = "tent could not ask Nomad whether it registered"
			if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), judged) {
				t.Errorf("Update = %v, want the error that a stale client could not be judged", err)
			}
		})
	})
}

// wantRefusedAtThePlan fails the test unless an update of the test cluster, with and without apply, fails with an error
// that starts with prefix, and leaves the cloud, the store and Nomad as they are: it only reads, and takes no lock.
func wantRefusedAtThePlan(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld, prefix string) {
	t.Helper()
	cloudBefore, storeBefore := cloudView(f), snapshot(t, svc.Store)
	calls, asked := len(f.Calls()), len(w.Log())
	for _, apply := range []bool{false, true} {
		_, err := svc.Update(t.Context(), "prod", apply)
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Update(apply %v) = %v, want an error that starts with %q", apply, err, prefix)
		}
	}
	wantNoWrites(t, f.Calls()[calls:])
	if got := nomadWrites(w, asked); len(got) != 0 {
		t.Errorf("the update wrote %v to Nomad", got)
	}
	if diff := cmp.Diff(cloudBefore, cloudView(f)); diff != "" {
		t.Errorf("the cloud changed (-before +after):\n%s", diff)
	}
	wantSnapshot(t, svc.Store, storeBefore)
}

// TestScaleFlowRefusesAtThePlan fails the plan of a scale-down, before any change, when the decisions refuse the
// removals, when no server runs for tent to ask, when the service has no way to reach Nomad, when the release
// files cannot be read, when the specs no longer have a server group and when twins that joined are in a client group
// that the specs no longer have.
func TestScaleFlowRefusesAtThePlan(t *testing.T) {
	t.Parallel()
	type world func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld)
	scale := func(built, size int) world {
		return func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			return builtScaleWorld(t, built, size)
		}
	}
	for _, tc := range []struct {
		name   string
		world  world
		damage func(t *testing.T, svc *app.Service, f *vultrfake.Fake)
		want   string
	}{
		{"a worker that no node of Nomad can stand for", scale(2, 2), func(t *testing.T, _ *app.Service, f *vultrfake.Fake) {
			in := f.Instances()[3] // prod-workers-0, with its labels
			in.ID, in.Hostname, in.Label, in.MainIP = "", "prod-workers-2", "prod-workers-2", "198.51.100.30"
			f.AddInstance(t, in) // joined, and the cloud lists no private address for it
		}, "update cannot remove prod-workers-1: node group workers: cannot go on: prod-workers-2 has no node in Nomad: " +
			"the cloud reports no private address for it; with maxUnavailable 0 no surplus node can be removed"},
		{"no server runs", scale(3, 1), func(t *testing.T, _ *app.Service, f *vultrfake.Fake) {
			for _, id := range []string{"instance-1", "instance-2", "instance-3"} {
				if err := f.HaltInstance(t.Context(), id); err != nil {
					t.Fatalf("HaltInstance: %v", err)
				}
			}
		}, "update would remove nodes that joined Nomad: prod-workers-1 (ID instance-5, surplus) and prod-workers-2 " +
			"(ID instance-6, surplus); cluster prod has no server that joined and runs for tent to ask: keep these nodes " +
			"in the specs until the servers run, or delete the whole cluster with tent delete cluster"},
		{"no way to reach Nomad", scale(3, 1), func(_ *testing.T, svc *app.Service, _ *vultrfake.Fake) {
			svc.Nomad = nil
		}, "no Nomad client is set up"},
		{"release files that cannot be read", scale(3, 1), func(_ *testing.T, svc *app.Service, _ *vultrfake.Fake) {
			svc.Assets.Client = &http.Client{Transport: brokenSites{}}
		}, "find Nomad 2.0.7: get https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_SHA256SUMS: " +
			errSitesDown.Error()},
		{"a server group that the specs no longer have", scale(3, 3),
			func(t *testing.T, svc *app.Service, _ *vultrfake.Fake) {
				if err := svc.Store.Delete(t.Context(), serversPath); err != nil {
					t.Fatal(err)
				}
				put(t, svc.Store, "prod/nodegroups/db.yaml",
					encode(t, edit(t, exampleServersYAML, "name: servers", "name: db")))
			}, "update would delete nodes that joined Nomad: prod-servers-0 (ID instance-1, not in the spec), " +
				"prod-servers-1 (ID instance-2, not in the spec) and prod-servers-2 (ID instance-3, not in the spec); tent " +
				"removes a whole node group only when its nodes are clients: keep node group servers in the specs, or " +
				"delete the whole cluster with tent delete cluster"},
		{"twins that joined in a client group that the specs no longer have", scale(3, 3),
			func(t *testing.T, svc *app.Service, f *vultrfake.Fake) {
				in := f.Instances()[3] // prod-workers-0, with its labels
				in.ID, in.MainIP = "", "198.51.100.30"
				f.AddInstance(t, in)
				if err := svc.Store.Delete(t.Context(), workersPath); err != nil {
					t.Fatal(err)
				}
			}, "update would delete nodes that joined Nomad: prod-workers-0 (ID instance-4, not in the spec) and " +
				"prod-workers-0 (ID instance-7, not in the spec); tent does not choose between machines of one name that " +
				"joined: remove one of the two machines called prod-workers-0 from Nomad and delete it in the cloud, or " +
				"delete the whole cluster with tent delete cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := tc.world(t)
				tc.damage(t, svc, f)

				wantRefusedAtThePlan(t, svc, f, w, tc.want)
			})
		})
	}
}

// TestScaleFlowLeavesTheNodesOfAGroupWhoseRoleChangedAlone plans no purge for a down node without a machine under the
// name of a group that the stored specs turned from server to combined: the decisions refuse such a group.
func TestScaleFlowLeavesTheNodesOfAGroupWhoseRoleChangedAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cluster := strings.Replace(exampleClusterYAML, "clientIntroduction: strict", "clientIntroduction: warn", 1)
		svc, f, w := newExample(t, cluster, exampleServersYAML, scaleWorkers(t, 3))
		mustUpdate(t, svc)
		put(t, svc.Store, serversPath, encode(t, edit(t, exampleServersYAML, "role: server", "role: combined")))
		w.Register(nomadops.Node{
			ID: "n-ghost", Name: "prod-servers-9", Status: "down", Address: netip.MustParseAddr("10.64.0.99"),
		})
		before := len(f.Calls())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil || len(plan.Purges) != 0 {
			t.Errorf("Update = %+v, %v; want no purge", plan.Purges, err)
		}
		wantNoWrites(t, f.Calls()[before:])
	})
}

// TestScaleFlowHoldsTheLockWhileItDrains drains the surplus workers while another update, started from inside the
// drain, finds the lock taken by an update and gives up; the lock is free when the update ends.
func TestScaleFlowHoldsTheLockWhileItDrains(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := scaleWorld(t, 1)
		var held *statestore.Lease
		var other error
		svc.OnProgress = func(p app.Progress) {
			if p.Nomad != nil && p.Nomad.Action == app.NomadDrain && p.Step == app.NodeStarted && held == nil {
				held = holder(t, svc.Store)
				_, other = svc.Update(t.Context(), "prod", true)
			}
		}

		mustUpdate(t, svc)

		if held == nil || held.Operation != "update" {
			t.Errorf("the lock at the first drain is %+v, want one held by an update", held)
		}
		if !errors.Is(other, statestore.ErrLocked) {
			t.Errorf("the update started from inside the drain returned %v, want an error that matches ErrLocked", other)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestScaleFlowGoesOnByTheLabelsWhenNoServerRunsAndNoNodeThatJoinedGoes halts the servers of a cluster whose group of
// workers is a machine above its size, the surplus one never having joined. The plan cannot read what a roll
// observes, but it removes no node that joined, so it goes on by the labels: the machine that never joined is the
// delete, and nothing is warned about.
func TestScaleFlowGoesOnByTheLabelsWhenNoServerRunsAndNoNodeThatJoinedGoes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := unjoinedScaleWorld(t, 2)
		for _, id := range []string{"instance-1", "instance-2", "instance-3"} {
			if err := f.HaltInstance(t.Context(), id); err != nil {
				t.Fatalf("HaltInstance: %v", err)
			}
		}
		var warned []string
		svc.OnWarning = func(msg string) { warned = append(warned, msg) }

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, deleteOf("prod-workers-2", "instance-6", "surplus"))
		if len(plan.Purges) != 0 || len(warned) != 0 {
			t.Errorf("the plan has the purges %+v and the warnings %q, want none", plan.Purges, warned)
		}
	})
}

// TestScaleFlowListsNoPurgeWhenTheReleaseFilesCannotBeRead leaves a node without a machine, as the cut scale-down of
// TestScaleFlowPurgesTheNodeThatADeletedMachineLeft does, and breaks the release files. The plan, which creates
// nothing, goes on without the purge and warns once that it cannot tell which nodes are outdated.
func TestScaleFlowListsNoPurgeWhenTheReleaseFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := scaleWorld(t, 2)
		if err := f.DeleteInstance(t.Context(), "instance-6"); err != nil {
			t.Fatalf("DeleteInstance: %v", err)
		}
		if plan, err := svc.Update(t.Context(), "prod", false); err != nil || len(plan.Purges) != 0 {
			t.Fatalf("Update = %+v, %v; want no purge while the node reads ready", plan.Purges, err)
		}
		time.Sleep(time.Minute)
		svc.Assets.Client = &http.Client{Transport: brokenSites{}}
		warned := told(svc)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if len(plan.Purges) != 0 {
			t.Errorf("the plan purges %+v, want none: it has no release files to build the groups with", plan.Purges)
		}
		if n := withCount(*warned, couldNotTell); n != 1 {
			t.Errorf("the plan told %d warnings that start with %q, want 1: %q", n, couldNotTell, *warned)
		}
	})
}

// retag replaces the tag called label of the instance id of f with value.
func retag(t *testing.T, f *vultrfake.Fake, id, label, value string) {
	t.Helper()
	for _, in := range f.Instances() {
		if in.ID == id {
			tags := slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool { return strings.HasPrefix(tag, label+"=") })
			f.SetInstanceTags(t, id, append(tags, label+"="+value)...)
			return
		}
	}
	t.Fatalf("the fake has no instance %s", id)
}

// TestScaleFlowRemovesAnOutdatedNodeBeforeANewerOne scales three workers down to two when the oldest has another spec
// hash than its group, or carries the replace label: it goes in place of the newest, in the plan and in the run.
func TestScaleFlowRemovesAnOutdatedNodeBeforeANewerOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, label, value string }{
		{"a spec hash that is not the group's", cloud.LabelSpecHash, "0000000000000000"},
		{"the replace label", cloud.LabelReplace, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, _ := scaleWorld(t, 2)
				retag(t, f, "instance-4", tc.label, tc.value)

				plan := mustUpdate(t, svc)

				wantNodeChanges(t, plan, removalOf("prod-workers-0", "instance-4", "surplus", "10m0s"))
				if diff := cmp.Diff([]string{"instance-4"}, callsOf(f, "DeleteInstance")); diff != "" {
					t.Errorf("the deleted machines (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestScaleFlowDoesNotWarnWhenTheUpdateIsCut ends the context of an update at its first read of the nodes. The plan
// returns the end of the context, not a warning that it cannot read the nodes.
func TestScaleFlowDoesNotWarnWhenTheUpdateIsCut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := scaleWorld(t, 3)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Nodes" {
				cancel()
			}
			return next(ctx)
		})
		warned := told(svc)

		_, err := svc.Update(ctx, "prod", false)

		if !errors.Is(err, context.Canceled) || len(*warned) != 0 {
			t.Errorf("Update = %v with the warnings %q, want the end of the context and no warning", err, *warned)
		}
	})
}

// TestScaleFlowEndsWhenTheServersAreGoneBeforeTheRemovals halts the servers after the plans, when the update is
// about to apply them. The run ends before it drains anything, with the advice of a roll, and counts nothing.
func TestScaleFlowEndsWhenTheServersAreGoneBeforeTheRemovals(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := scaleWorld(t, 1)
		svc.OnUpdatePlan = func(app.UpdatePlan) error {
			for _, id := range []string{"instance-1", "instance-2", "instance-3"} {
				if err := f.HaltInstance(t.Context(), id); err != nil {
					t.Fatalf("HaltInstance: %v", err)
				}
			}
			return nil
		}
		before := len(w.Log())

		plan, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "cluster prod has no server that joined and runs; run tent validate cluster to see what is wrong")
		if got := nomadWrites(w, before); len(got) != 0 || plan.Removed != (app.RemovalCounts{}) || plan.Applied {
			t.Errorf("the update wrote %v to Nomad, removed %+v and applied %v, want nothing", got, plan.Removed, plan.Applied)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestScaleFlowCountsWhatTheRemovalsDidBeforeTheyFailed fails the purge of the first removed node. The plan that the
// update returns holds the nodes drained until then.
func TestScaleFlowCountsWhatTheRemovalsDidBeforeTheyFailed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := scaleWorld(t, 1)
		boom := errors.New("boom")
		svc.OnUpdatePlan = func(app.UpdatePlan) error {
			w.Fail(t, "Purge", boom)
			return nil
		}

		plan, err := svc.Update(t.Context(), "prod", true)

		if !errors.Is(err, boom) {
			t.Fatalf("Update = %v, want an error that wraps boom", err)
		}
		if want := (app.RemovalCounts{Drained: 2}); plan.Removed != want || plan.Applied {
			t.Errorf("the plan removed %+v and applied %v, want %+v and not applied", plan.Removed, plan.Applied, want)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestScaleFlowFailsThePlanWhenTheReadFailsThoughTheSurplusNeverJoined fails one read of what a roll observes on a
// cluster whose group of workers is a machine above its size, the surplus one by the labels never having joined. The
// plan fails with the read's error, with and without apply, changes nothing and warns about nothing.
func TestScaleFlowFailsThePlanWhenTheReadFailsThoughTheSurplusNeverJoined(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedScaleWorld(t, 2)
		boom := errors.New("boom")
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Health" {
				return boom
			}
			return next(ctx)
		})
		warned := told(svc)
		cloudBefore, storeBefore, calls := cloudView(f), snapshot(t, svc.Store), len(f.Calls())

		for _, apply := range []bool{false, true} {
			if _, err := svc.Update(t.Context(), "prod", apply); !errors.Is(err, boom) {
				t.Errorf("Update(apply %v) = %v, want an error that wraps boom", apply, err)
			}
		}

		wantNoWrites(t, f.Calls()[calls:])
		if diff := cmp.Diff(cloudBefore, cloudView(f)); diff != "" {
			t.Errorf("the cloud changed (-before +after):\n%s", diff)
		}
		wantSnapshot(t, svc.Store, storeBefore)
		if len(*warned) != 0 {
			t.Errorf("the update told the warnings %q, want none", *warned)
		}
	})
}
