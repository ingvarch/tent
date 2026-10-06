package app_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// Ages of a client's machine, by the clock of the fake: before and after the lifetime of its intro token, 31 minutes.
const (
	youngClient = 30 * time.Minute
	oldClient   = 32 * time.Minute
)

// neverRegistered builds the test cluster in which the first machine of prod-workers-1, instance-5, never registers:
// the build stops at its registration, with the machine ready and without the joined label. The world registers the
// machine that replaces it.
func neverRegistered(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t)
	stopAtWorker1(t, svc, f, w)
	return svc, f, w
}

// stopAtWorker1 runs the build on svc, f and w with the machine instance-5 withheld, and stops the test unless the
// build fails at its registration.
func stopAtWorker1(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld) {
	t.Helper()
	w.WithholdInstance("instance-5")
	if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the build: %v, want a deadline", err)
	}
	wantJoined(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0")
}

// sleepUntilAge sleeps until the machine id is age old, by the clock of the fake, which is the service's: the bubble's.
func sleepUntilAge(t *testing.T, f *vultrfake.Fake, id string, age time.Duration) {
	t.Helper()
	for _, in := range f.Instances() {
		if in.ID != id {
			continue
		}
		created, err := time.Parse(time.RFC3339, in.DateCreated)
		if err != nil {
			t.Fatalf("the creation date of %s: %v", id, err)
		}
		time.Sleep(time.Until(created.Add(age)))
		return
	}
	t.Fatalf("the fake has no instance %s", id)
}

// waitForWorker1 is the wait for the machine instance-5 of prod-workers-1, which is ready and has not joined.
var waitForWorker1 = app.NodeChange{
	Action: app.NodeWait, Name: "prod-workers-1", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
	MachineType: "vc2-2c-4gb", Image: v1alpha1.DefaultImage, ID: "instance-5",
}

// replaceWorker1 are the changes that replace the machine instance-5 of prod-workers-1.
func replaceWorker1() []app.NodeChange {
	return []app.NodeChange{deleteOf("prod-workers-1", "instance-5", "not registered"), createOf("workers",
		v1alpha1.RoleClient, 1)}
}

// wantWorker1Cause is the start of the error of a plan that cannot ask Nomad about prod-workers-1.
const wantWorker1Cause = "node prod-workers-1 (ID instance-5) did not join within 31 minutes of its creation, and " +
	"tent could not ask Nomad whether it registered: "

// writesOf returns the calls of the fake from the n-th on that write, as lines. nm names the instances that it had
// before them, which it may have deleted.
func writesOf(f *vultrfake.Fake, n int, nm map[string]string) []string {
	var out []string
	maps.Copy(nm, names(f))
	for _, c := range f.Calls()[n:] {
		if slices.Contains([]string{"CreateInstance", "DeleteInstance", "UpdateInstance"}, c.Name) {
			out = append(out, callKey(line(c, nm)))
		}
	}
	return out
}

// TestUpdateReplacesAClientThatNeverRegistered replaces a client that did not register within 31 minutes of its
// creation. Until then the plan waits for it without a call to Nomad; after it the plan asks Nomad once, shows the
// delete and the create, and the apply checks the delete, deletes the machine, makes a token and a machine under a
// new operation id, waits for its registration and scrubs it.
func TestUpdateReplacesAClientThatNeverRegistered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		oldOp := opOf(f.Instances()[4].Tags)
		sleepUntilAge(t, f, "instance-5", youngClient)
		before := len(w.Log())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, waitForWorker1)
		if diff := cmp.Diff([]string(nil), nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of a plan within 31 minutes (-want +got):\n%s", diff)
		}

		sleepUntilAge(t, f, "instance-5", oldClient)
		before = len(w.Log())

		plan, err = svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, replaceWorker1()...)
		if diff := cmp.Diff([]string{"Nodes"}, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of a plan after 31 minutes (-want +got):\n%s", diff)
		}
		if plan.Nomad != nil {
			t.Errorf("the plan has the Nomad step %+v, want none", plan.Nomad)
		}

		before, cloudBefore, nm := len(w.Log()), len(f.Calls()), names(f)
		lines := recordProgress(svc)
		applied := mustUpdate(t, svc)

		wantNodeChanges(t, applied, replaceWorker1()...)
		wantNomad := []string{"Nodes", "Nodes", "Nodes", "Peers", "IntroToken", "Nodes"}
		if diff := cmp.Diff(wantNomad, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of the apply: two plans, the check of the delete, the token and the registration "+
				"(-want +got):\n%s", diff)
		}
		if got := w.Log()[before+4]; got.Arg != "prod-workers-1 default 30m0s" {
			t.Errorf("the intro token was asked for %q, want prod-workers-1 default 30m0s", got.Arg)
		}
		wantWrites := []string{"DeleteInstance <prod-workers-1>", "CreateInstance prod-workers-1",
			"UpdateInstance <prod-workers-1>"}
		if diff := cmp.Diff(wantWrites, writesOf(f, cloudBefore, nm)); diff != "" {
			t.Errorf("the calls that write (-want +got):\n%s", diff)
		}
		wantLines(t, onlyNomadAndNodes(*lines), slices.Concat(
			nodeSteps("delete", "prod-workers-1"), nodeSteps("create", "prod-workers-1"),
			registerSteps("prod-workers-1"), nodeSteps("scrub", "prod-workers-1"),
		))
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, allNames...)
		if ids := instanceIDs(f); !slices.Equal(ids, []string{"instance-1", "instance-2", "instance-3", "instance-4",
			"instance-6"}) {
			t.Errorf("the instances are %v, want the new machine instance-6 in the place of instance-5", ids)
		}
		if op := opOf(f.Instances()[4].Tags); op == oldOp || op == "" {
			t.Errorf("the new machine has the operation id %q, want one that differs from %q", op, oldOp)
		}
		wantClaims := map[string]string{"nomad_node_name": "prod-workers-1", "nomad_node_pool": "default"}
		if diff := cmp.Diff(wantClaims, introClaims(t, configOf(t, f, "prod-workers-1"))); diff != "" {
			t.Errorf("the intro token of the new machine (-want +got):\n%s", diff)
		}
		wantConverged(t, svc)
	})
}

// TestUpdateReplacesAClientWhenTheLimitStillCountsItsDeletedMachine replaces a client that never registered in an
// account that is at its instance limit: Vultr refuses the first create after the delete, and the run still ends with
// the new machine, in one run.
func TestUpdateReplacesAClientWhenTheLimitStillCountsItsDeletedMachine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := neverRegistered(t)
		sleepUntilAge(t, f, "instance-5", oldClient)
		f.Fail(t, "CreateInstance", vultr.NewAPIError(http.MethodPost, "/v2/instances", 400,
			"Server add failed: You have reached the maximum number of active instances for this account.", 0), 2)
		cloudBefore, nm := len(f.Calls()), names(f)

		applied := mustUpdate(t, svc)

		wantNodeChanges(t, applied, replaceWorker1()...)
		wantWrites := []string{"DeleteInstance <prod-workers-1>", "CreateInstance prod-workers-1",
			"CreateInstance prod-workers-1", "CreateInstance prod-workers-1", "UpdateInstance <prod-workers-1>"}
		if diff := cmp.Diff(wantWrites, writesOf(f, cloudBefore, nm)); diff != "" {
			t.Errorf("the calls that write: the delete, two refused creates, the accepted one and the scrub "+
				"(-want +got):\n%s", diff)
		}
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, allNames...)
		wantConverged(t, svc)
	})
}

// instanceIDs returns the IDs of the fake's instances in creation order.
func instanceIDs(f *vultrfake.Fake) []string {
	var ids []string
	for _, in := range f.Instances() {
		ids = append(ids, in.ID)
	}
	return ids
}

// TestUpdateWaitsForAClientThatRegisteredLate waits for a client that is older than 31 minutes and has registered
// without the label: the plan asks Nomad, finds the node, and keeps the wait, so the run labels the machine and
// deletes nothing.
func TestUpdateWaitsForAClientThatRegisteredLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		sleepUntilAge(t, f, "instance-5", oldClient)
		w.Register(nomadops.Node{
			Name: "prod-workers-1", Status: "ready", Eligible: true, Address: privateOf(t, f, "instance-5"),
		})
		cloudBefore, before, nm := len(f.Calls()), len(w.Log()), names(f)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, waitForWorker1)
		if diff := cmp.Diff([]string{"Nodes"}, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}

		mustUpdate(t, svc)

		if diff := cmp.Diff([]string{"UpdateInstance <prod-workers-1>"}, writesOf(f, cloudBefore, nm)); diff != "" {
			t.Errorf("the calls that write (-want +got):\n%s", diff)
		}
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, allNames...)
		wantConverged(t, svc)
	})
}

// TestUpdateAsksWhichNodeRegistered finds a client that never registered although Nomad lists a node of its name: the
// node is down, or it advertises another address. The plan replaces the machine.
func TestUpdateAsksWhichNodeRegistered(t *testing.T) {
	for _, tc := range []struct {
		name string
		node func(machine netip.Addr) nomadops.Node
	}{
		{"a down node at its address", func(a netip.Addr) nomadops.Node {
			return nomadops.Node{Name: "prod-workers-1", Status: "down", Address: a}
		}},
		{"a ready node at another address", func(netip.Addr) nomadops.Node {
			return nomadops.Node{
				Name: "prod-workers-1", Status: "ready", Eligible: true, Address: netip.MustParseAddr("10.64.0.99"),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := neverRegistered(t)
				w.Register(tc.node(privateOf(t, f, "instance-5")))
				sleepUntilAge(t, f, "instance-5", oldClient)

				plan, err := svc.Update(t.Context(), "prod", false)

				if err != nil {
					t.Fatalf("Update without apply: %v", err)
				}
				wantNodeChanges(t, plan, replaceWorker1()...)
			})
		})
	}
}

// TestUpdateReplacesAClientThatIsNotReady replaces a machine that the cloud still reports not ready 31 minutes after
// its creation: the plan holds its delete, not the repeat of its create.
func TestUpdateReplacesAClientThatIsNotReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		pendingWorker(t, svc, f) // instance-6 replaces instance-5 and is not ready
		sleepUntilAge(t, f, "instance-6", youngClient)
		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil || len(plan.Nodes) != 1 || plan.Nodes[0].Action != app.NodeWait || plan.Nodes[0].Op == "" {
			t.Fatalf("Update without apply = %+v, %v, want the wait that repeats the create", plan.Nodes, err)
		}
		sleepUntilAge(t, f, "instance-6", oldClient)

		plan, err = svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, deleteOf("prod-workers-1", "instance-6", "not registered"),
			createOf("workers", v1alpha1.RoleClient, 1))
	})
}

// TestUpdateAsksNothingWithoutAJoinedServer waits for an old client when no server that stays carries the joined
// label, since the plan has no server to ask: the plan makes no Nomad call.
func TestUpdateAsksNothingWithoutAJoinedServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		for _, id := range serverIDs {
			unmark(t, f, id)
		}
		sleepUntilAge(t, f, "instance-5", oldClient)
		before := len(w.Log())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		var got []string
		for _, c := range plan.Nodes {
			got = append(got, c.Action.String()+" "+c.Name)
		}
		want := []string{"wait prod-servers-0", "wait prod-servers-1", "wait prod-servers-2", "wait prod-workers-1"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("the node changes (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string(nil), nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateAsksOnlyTheJoinedServers asks Nomad about an old client through the servers that carry the joined label
// alone: with one of three joined, the plan makes one Nomad client, at the public address of that server.
func TestUpdateAsksOnlyTheJoinedServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		unmark(t, f, "instance-2")
		unmark(t, f, "instance-3")
		sleepUntilAge(t, f, "instance-5", oldClient)
		before := len(w.Configs())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		var got []string
		for _, c := range plan.Nodes {
			got = append(got, c.Action.String()+" "+c.Name)
		}
		want := []string{"wait prod-servers-1", "wait prod-servers-2", "delete prod-workers-1", "create prod-workers-1"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("the node changes (-want +got):\n%s", diff)
		}
		var addresses []string
		for _, c := range w.Configs()[before:] {
			addresses = append(addresses, c.Address)
		}
		if diff := cmp.Diff([]string{"198.18.0.1:4646"}, addresses); diff != "" {
			t.Errorf("the servers the plan asked (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateMeasuresTheAgeByTheServiceClock decides that a client is old by the clock of the service, not by the
// clock of the process: a service clock an hour ahead of the cloud's makes the machine old at once.
func TestUpdateMeasuresTheAgeByTheServiceClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := neverRegistered(t)
		svc.Now = func() time.Time { return time.Now().Add(time.Hour) }
		before := len(w.Log())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, replaceWorker1()...)
		if diff := cmp.Diff([]string{"Nodes"}, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateFailsWhenItCannotAskNomadAboutAClient fails the plan of an old client before any write when Nomad cannot be
// asked, with and without apply, and when the service has no way to reach Nomad.
func TestUpdateFailsWhenItCannotAskNomadAboutAClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *app.Service, *nomadWorld)
		check func(*testing.T, error)
	}{
		{
			name:  "Nodes fails",
			setup: func(t *testing.T, _ *app.Service, w *nomadWorld) { w.Fail(t, "Nodes", errBoom) },
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != wantWorker1Cause+errBoom.Error() || !errors.Is(err, errBoom) {
					t.Errorf("Update = %v, want %q, wrapping boom", err, wantWorker1Cause+errBoom.Error())
				}
			},
		},
		{
			name:  "no Nomad client",
			setup: func(_ *testing.T, svc *app.Service, _ *nomadWorld) { svc.Nomad = nil },
			check: func(t *testing.T, err error) {
				wantError(t, err, wantWorker1Cause+"no Nomad client is set up")
			},
		},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(tc.name+" with apply "+map[bool]string{false: "off", true: "on"}[apply], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					svc, f, w := neverRegistered(t)
					sleepUntilAge(t, f, "instance-5", oldClient)
					tc.setup(t, svc, w)
					view, calls := cloudView(f), len(f.Calls())

					_, err := svc.Update(t.Context(), "prod", apply)

					tc.check(t, err)
					wantNoWrites(t, f.Calls()[calls:])
					if diff := cmp.Diff(view, cloudView(f)); diff != "" {
						t.Errorf("the cloud changed (-before +after):\n%s", diff)
					}
					wantLockFree(t, svc.Store)
				})
			})
		}
	}
}

// TestUpdateStopsWhenTheClientRegisteredMeanwhile does not delete a client that registered after the plan asked: the
// check of the delete finds its node, labels the machine, and fails the run.
func TestUpdateStopsWhenTheClientRegisteredMeanwhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		sleepUntilAge(t, f, "instance-5", oldClient)
		answers := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Nodes" {
				if answers++; answers == 3 { // the first two answers belong to the plans, the third to the check
					w.Register(nomadops.Node{
						Name: "prod-workers-1", Status: "ready", Eligible: true, Address: privateOf(t, f, "instance-5"),
					})
				}
			}
			return next(ctx)
		})

		_, err := svc.Update(t.Context(), "prod", true)

		const prefix = "delete node prod-workers-1 (instance-5): the node has joined Nomad"
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("Update = %v, want the refusal to delete a node that joined", err)
		}
		w.SetHook(nil)
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, allNames...)
		wantConverged(t, svc)
	})
}

// TestUpdateResumesTheReplacementOfAClient cuts the replacement at the delete of the machine: before it, the next plan
// holds both changes; after it, the create alone. The next run ends with one machine per node and no change.
func TestUpdateResumesTheReplacementOfAClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after bool
		plan  []app.NodeChange
	}{
		{"before the delete", false, replaceWorker1()},
		{"after the delete", true, replaceWorker1()[1:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := neverRegistered(t)
				sleepUntilAge(t, f, "instance-5", oldClient)
				cut := cutCase{key: "DeleteInstance <prod-workers-1>", n: 1, after: tc.after}
				runCut(t, f, w, svc.Store, cut, func(ctx context.Context) error {
					_, err := svc.Update(ctx, "prod", true)
					return err
				})

				plan, err := svc.Update(t.Context(), "prod", false)

				if err != nil {
					t.Fatalf("Update without apply: %v", err)
				}
				wantNodeChanges(t, plan, tc.plan...)

				mustUpdate(t, svc)

				wantNodes(t, f, allNodes...)
				wantJoined(t, f, allNames...)
				wantConverged(t, svc)
			})
		})
	}
}

// TestUpdateKeepsTheTwinThatRegisteredOfAClientThatNeverRegistered finds a client that never registered beside a twin
// of its name that registered without the label, both older than 31 minutes. The twin stays and the other machine
// goes as a duplicate: no third machine is made, and the guard does not meet a joined twin.
func TestUpdateKeepsTheTwinThatRegisteredOfAClientThatNeverRegistered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := neverRegistered(t)
		twin := twinOf(t, f, "instance-5")
		sleepUntilAge(t, f, twin, oldClient)
		before := len(w.Log())

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		waitForTwin := waitForWorker1
		waitForTwin.ID = twin
		wantNodeChanges(t, plan, waitForTwin, deleteOf("prod-workers-1", "instance-5", "duplicate"))
		if diff := cmp.Diff([]string{"Nodes"}, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls of the plan (-want +got):\n%s", diff)
		}

		mustUpdate(t, svc)

		if ids := instanceIDs(f); !slices.Equal(ids, []string{"instance-1", "instance-2", "instance-3", "instance-4", twin}) {
			t.Errorf("the instances are %v, want the twin %s in the place of instance-5, and no third machine", ids, twin)
		}
		wantNodes(t, f, allNodes...)
		wantJoined(t, f, allNames...)
		wantConverged(t, svc)
	})
}

// nameOf returns the machine name of the instance id of f.
func nameOf(f *vultrfake.Fake, id string) string { return names(f)[id] }

// TestUpdateReplacesTwoClientsThatNeverRegisteredWhenTheirGroupShrinks scales a group of two clients that never
// registered down to one. Both are old and Nomad lists both as down: the plan deletes both as not registered and makes
// one, which registers and is scrubbed.
func TestUpdateReplacesTwoClientsThatNeverRegisteredWhenTheirGroupShrinks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		mustUpdate(t, svc)
		for _, id := range []string{"instance-4", "instance-5"} {
			dropJoinedTag(t, f, id)
			w.WithholdInstance(id)
			w.Register(nomadops.Node{Name: nameOf(f, id), Status: "down", Address: privateOf(t, f, id)})
		}
		sleepUntilAge(t, f, "instance-5", oldClient)
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 1"))

		plan := mustUpdate(t, svc)

		wantNodeChanges(t, plan, deleteOf("prod-workers-0", "instance-4", "not registered"),
			deleteOf("prod-workers-1", "instance-5", "not registered"), createOf("workers", v1alpha1.RoleClient, 0))
		wantNodes(t, f, server(0), server(1), server(2), worker(0))
		wantJoined(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0")
		wantConverged(t, svc)
	})
}
