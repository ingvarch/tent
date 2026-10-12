package app_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// undrained returns what is wrong with the node n of the machine id when the roll does verb to the machine, or
// nothing when the node is ineligible, not draining, and its last drain is complete and was for the machine.
func undrained(verb, id string, n nomadops.Node) string {
	if !n.Eligible && !n.Draining && n.LastDrain.Status == "complete" && n.LastDrain.Meta["tent_machine"] == id {
		return ""
	}
	return fmt.Sprintf("%s of %s: its node reads %s, eligible %v, draining %v, last drain %q for %q; "+
		"want a node that is down, or ineligible with a drain that is complete for the machine",
		verb, id, n.Status, n.Eligible, n.Draining, n.LastDrain.Status, n.LastDrain.Meta["tent_machine"])
}

// shrinkInvariants checks, before every call to Vultr and to Nomad, what the scale-down of a client group keeps: the
// group never has more machines than when the watch began, and no create names it; the machines of the group that
// are listed and whose node is ready, eligible and not draining are at least its size less its maxUnavailable; a
// machine of the group goes only after its drain; and a purge names a node that reads down and that no listed machine
// has.
type shrinkInvariants struct {
	tb          testing.TB
	f           *vultrfake.Fake
	w           *nomadWorld
	group       string            // the name of the node group
	size        int               // the size that the group shrinks to
	unavailable int               // the maxUnavailable of the group
	most        int               // the machines of the group when the watch began
	purges      *serverInvariants // checks a purge as the watch of a combined group does
}

// watchShrink makes every call to f and to w check the invariants of the client group called group, which shrinks to
// size, before it runs, and takes the hooks of both. It reads the group's maxUnavailable from the cluster's specs.
func watchShrink(tb testing.TB, f *vultrfake.Fake, w *nomadWorld, group string, size int) *shrinkInvariants {
	tb.Helper()
	objs, err := w.svc.Get(context.Background(), w.name, true)
	if err != nil {
		tb.Fatalf("Get: %v", err)
	}
	inv := &shrinkInvariants{
		tb: tb, f: f, w: w, group: group, size: size, unavailable: -1,
		purges: &serverInvariants{tb: tb, f: f, w: w, group: group},
	}
	for _, g := range objs.NodeGroups {
		if g.Metadata.Name == group && g.Spec.RollingUpdate.MaxUnavailable != nil {
			inv.unavailable = *g.Spec.RollingUpdate.MaxUnavailable
		}
	}
	if inv.unavailable < 0 {
		tb.Fatalf("the specs have no client group %q", group)
	}
	inv.most = len(inv.machineIDs())
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		inv.check("Vultr " + c.Name + " " + c.Arg)
		switch c.Name {
		case "CreateInstance":
			inv.checkCreate(c.Arg)
		case "DeleteInstance":
			inv.checkDrained(c.Arg)
		}
		return next(ctx)
	})
	w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		inv.check("Nomad " + c.Name + " " + c.Arg)
		if c.Name == "Purge" {
			inv.purges.checkPurge(c.Arg)
		}
		return next(ctx)
	})
	return inv
}

// machineIDs returns the instance IDs of the machines of the group that the cloud lists.
func (inv *shrinkInvariants) machineIDs() []string {
	var ids []string
	for _, in := range inv.f.Instances() {
		if strings.HasPrefix(in.Hostname, machinePrefix(inv.group)) {
			ids = append(ids, in.ID)
		}
	}
	return ids
}

// check fails the test when the group has more machines than when the watch began, or too few nodes that can take
// work.
func (inv *shrinkInvariants) check(call string) {
	inv.tb.Helper()
	ids := inv.machineIDs()
	if len(ids) > inv.most {
		inv.tb.Errorf("before %s: the group has %d machines, want at most %d, as when the watch began",
			strings.TrimSpace(call), len(ids), inv.most)
	}
	nodes := slices.Collect(maps.Values(inv.w.nodeViews()))
	if got, want := availableWorkers(ids, nodes), inv.size-inv.unavailable; got < want {
		inv.tb.Errorf("before %s: %d nodes of the group can take work, want at least %d",
			strings.TrimSpace(call), got, want)
	}
}

// checkCreate fails the test when the create is of a machine of the group.
func (inv *shrinkInvariants) checkCreate(label string) {
	inv.tb.Helper()
	if strings.HasPrefix(label, machinePrefix(inv.group)) {
		inv.tb.Errorf("create of %s, a machine of the group that shrinks", label)
	}
}

// checkDrained fails the test when the roll deletes a machine of the group whose node Nomad lists as up and that is
// not ineligible with a drain that is complete for the machine, whatever labels the machine has. A node that is down
// or not listed is left alone.
func (inv *shrinkInvariants) checkDrained(id string) {
	inv.tb.Helper()
	if !slices.Contains(inv.machineIDs(), id) {
		return
	}
	n, listed := inv.w.nodeViews()[nodeIDOf(id)]
	if !listed || n.Status == "down" {
		return
	}
	if msg := undrained("delete", id, n); msg != "" {
		inv.tb.Errorf("%s", msg)
	}
}

// shrinking limits the group to the machines it has now, in place of its size and one more, since a scale-down adds
// none, and returns inv.
func (inv *serverInvariants) shrinking() *serverInvariants {
	inv.most = inv.view().machines
	return inv
}

// wantRemoved fails the test unless the machines that the run deleted at the Vultr fake are the machines of the
// plan's node deletes, and those of the groups called ordered, which are server or combined groups, went in the
// order of the plan.
func wantRemoved(tb testing.TB, plan app.UpdatePlan, f *vultrfake.Fake, ordered ...string) {
	tb.Helper()
	var planned []string
	names := map[string]string{}
	for _, c := range plan.Nodes {
		if c.Action == app.NodeDelete {
			planned = append(planned, c.ID)
			names[c.ID] = c.Name
		}
	}
	deleted := callsOf(f, "DeleteInstance")
	if diff := cmp.Diff(slices.Sorted(slices.Values(planned)), slices.Sorted(slices.Values(deleted))); diff != "" {
		tb.Errorf("the machines the run deleted differ from the plan's node deletes (-plan +run):\n%s", diff)
	}
	inOrder := func(ids []string) []string {
		return slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
			return !slices.ContainsFunc(ordered, func(g string) bool { return strings.HasPrefix(names[id], machinePrefix(g)) })
		})
	}
	if diff := cmp.Diff(inOrder(planned), inOrder(deleted)); diff != "" {
		tb.Errorf("the run deleted the machines of %v out of the plan's order (-plan +run):\n%s", ordered, diff)
	}
}

// shrinkKit is the test cluster after an update, with a client of its Nomad.
type shrinkKit struct{ combinedKit }

// create creates a machine from the create request of prod-workers-0, with the label and the hostname.
func (k shrinkKit) create(t *testing.T, label, hostname string) {
	t.Helper()
	req, ok := k.f.CreateRequest(k.id(t, "prod-workers-0"))
	if !ok {
		t.Fatal("the fake has no create request for prod-workers-0")
	}
	req.Label, req.Hostname = label, hostname
	if _, err := k.f.CreateInstance(t.Context(), &req); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
}

// list lists the machines of the cluster.
func (k shrinkKit) list(t *testing.T) {
	t.Helper()
	if _, err := k.f.ListInstances(t.Context(), cloud.LabelCluster+"=prod"); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
}

// del deletes the machine called name.
func (k shrinkKit) del(t *testing.T, name string) {
	t.Helper()
	if err := k.f.DeleteInstance(t.Context(), k.id(t, name)); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

// spareYAML is a second client group of the test cluster, which no scale-down touches.
const spareYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: spare
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  size: 1
`

// shrinkCase is a world that breaks one invariant of a scale-down, or none.
type shrinkCase struct {
	name    string
	workers string // lines that follow the spec of the workers
	breakIt func(t *testing.T, k shrinkKit)
	want    string // a part of the failure; empty when the case expects none
}

// wantShrinkInvariants runs each case on a healthy cluster with two workers, one spare client and the invariants
// watching the workers as a group of one, which fail the test only for what the case breaks. A drain takes three
// reads.
func wantShrinkInvariants(t *testing.T, cases []shrinkCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := newRelease(t, keyedClusterYAML, serversYAML, workersYAML+tc.workers, spareYAML)
				w.SetDrainReads(drainReads)
				mustUpdate(t, svc)
				api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
				if err != nil {
					t.Fatalf("Nomad: %v", err)
				}
				ftb := &failureTB{TB: t}
				watchShrink(ftb, f, w, "workers", 1)
				k := shrinkKit{combinedKit{f: f, w: w, api: api}}
				if _, err := api.Nodes(t.Context()); err != nil {
					t.Fatalf("Nodes: %v", err)
				}
				if got := ftb.failures(); len(got) > 0 {
					t.Fatalf("the invariants failed for a healthy cluster: %q", got)
				}

				tc.breakIt(t, k)

				got := strings.Join(ftb.failures(), "\n")
				if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
					t.Errorf("the invariants failed with %q, want a failure that says %q", got, tc.want)
				}
			})
		})
	}
}

// TestShrinkInvariantsFailTheTestForADeleteOfAMachineThatIsNotDrained deletes a worker whose node is eligible, is
// being drained, was drained for another machine, or has no joined label, and fails the test for each. It fails
// nothing for a worker whose node was drained for it, is down or is not listed, and for a machine of another group.
func TestShrinkInvariantsFailTheTestForADeleteOfAMachineThatIsNotDrained(t *testing.T) {
	t.Parallel()
	const fails = "delete of "
	wantShrinkInvariants(t, []shrinkCase{
		{name: "a node that was not drained", breakIt: func(t *testing.T, k shrinkKit) {
			k.del(t, "prod-workers-1")
		}, want: fails},
		{name: "a node that drains", breakIt: func(t *testing.T, k shrinkKit) {
			k.mark(t, "prod-workers-1")
			k.drain(t, "prod-workers-1", "prod-workers-1", 1)
			k.del(t, "prod-workers-1")
		}, want: fails},
		{name: "a drain for another machine", breakIt: func(t *testing.T, k shrinkKit) {
			k.mark(t, "prod-workers-1")
			k.drain(t, "prod-workers-1", "prod-workers-0", 4)
			k.del(t, "prod-workers-1")
		}, want: fails},
		{name: "a machine without the joined label", breakIt: func(t *testing.T, k shrinkKit) {
			id := k.id(t, "prod-workers-1")
			for _, in := range k.f.Instances() {
				if in.ID == id {
					k.f.SetInstanceTags(t, id, slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
						return strings.HasPrefix(tag, cloud.LabelJoined+"=")
					})...)
				}
			}
			k.del(t, "prod-workers-1")
		}, want: fails},
		{name: "a node that was drained for the machine", breakIt: func(t *testing.T, k shrinkKit) {
			k.retire(t, "prod-workers-1")
			k.del(t, "prod-workers-1")
		}},
		{name: "a node that is down", breakIt: func(t *testing.T, k shrinkKit) {
			k.w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Status = "down" })
			k.del(t, "prod-workers-1")
		}},
		{name: "a node that is not listed", breakIt: func(t *testing.T, k shrinkKit) {
			k.w.DropNode("prod-workers-1")
			k.del(t, "prod-workers-1")
		}},
		{name: "a machine of another group", breakIt: func(t *testing.T, k shrinkKit) {
			k.del(t, "prod-spare-0")
		}},
	})
}

// TestShrinkInvariantsFailTheTestForACreateInTheGroup creates a machine with a name of the group and fails the test at
// the create. A machine of another group is left alone.
func TestShrinkInvariantsFailTheTestForACreateInTheGroup(t *testing.T) {
	t.Parallel()
	create := func(name string) func(t *testing.T, k shrinkKit) {
		return func(t *testing.T, k shrinkKit) {
			k.create(t, name, name)
		}
	}
	wantShrinkInvariants(t, []shrinkCase{
		{name: "a machine of the group", breakIt: create("prod-workers-2"), want: "create of prod-workers-2"},
		{name: "a machine of another group", breakIt: create("prod-spare-1")},
	})
}

// TestShrinkInvariantsFailTheTestForAGroupThatGrows lets the group have more machines than when the watch began,
// by a create of one of another name, and fails the test; the same count fails nothing.
func TestShrinkInvariantsFailTheTestForAGroupThatGrows(t *testing.T) {
	t.Parallel()
	wantShrinkInvariants(t, []shrinkCase{
		{name: "a machine more", breakIt: func(t *testing.T, k shrinkKit) {
			k.create(t, "other", "prod-workers-9")
			k.list(t)
		}, want: "the group has 3 machines, want at most 2"},
		{name: "the same machines", breakIt: func(t *testing.T, k shrinkKit) {
			k.list(t)
		}},
	})
}

// TestShrinkInvariantsFailTheTestForTooFewNodesThatCanTakeWork counts the nodes of the group that are ready, eligible
// and not draining against the size, 1 here, less maxUnavailable, 0 unless the case says otherwise: it fails when both
// workers are ineligible, down or draining, and fails nothing when one can take work or when a maxUnavailable of 1
// lets both go.
func TestShrinkInvariantsFailTheTestForTooFewNodesThatCanTakeWork(t *testing.T) {
	t.Parallel()
	const fails = "nodes of the group can take work, want at least 1"
	wantShrinkInvariants(t, []shrinkCase{
		{name: "both ineligible, before a call to Nomad", breakIt: func(t *testing.T, k shrinkKit) {
			k.mark(t, "prod-workers-0")
			k.mark(t, "prod-workers-1")
			for range 2 {
				if _, err := k.api.Nodes(t.Context()); err != nil {
					t.Fatalf("Nodes: %v", err)
				}
			}
		}, want: "before Nomad Nodes: 0 " + fails},
		{name: "both ineligible, before a call to Vultr", breakIt: func(t *testing.T, k shrinkKit) {
			k.mark(t, "prod-workers-0")
			k.mark(t, "prod-workers-1")
			if _, err := k.api.Nodes(t.Context()); err != nil {
				t.Fatalf("Nodes: %v", err)
			}
			k.list(t)
		}, want: "before Vultr ListInstances"},
		{name: "one down, one draining", breakIt: func(t *testing.T, k shrinkKit) {
			k.w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Status = "down" })
			k.w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Draining = true })
			k.list(t)
		}, want: "0 " + fails},
		{name: "both ineligible, one may be unavailable", workers: "  rollingUpdate:\n    maxUnavailable: 1\n",
			breakIt: func(t *testing.T, k shrinkKit) {
				k.mark(t, "prod-workers-0")
				k.mark(t, "prod-workers-1")
				if _, err := k.api.Nodes(t.Context()); err != nil {
					t.Fatalf("Nodes: %v", err)
				}
				k.list(t)
			}},
		{name: "one ineligible", breakIt: func(t *testing.T, k shrinkKit) {
			k.mark(t, "prod-workers-0")
			if _, err := k.api.Nodes(t.Context()); err != nil {
				t.Fatalf("Nodes: %v", err)
			}
			k.list(t)
		}},
	})
}

// TestShrinkInvariantsFailTheTestForAPurgeOfANodeThatIsNotGone purges the node of a worker that Nomad lists as up,
// and the node of a worker whose machine is listed though the node is down, and fails the test for each. It fails
// nothing for a node that is down and has no machine.
func TestShrinkInvariantsFailTheTestForAPurgeOfANodeThatIsNotGone(t *testing.T) {
	t.Parallel()
	purge := func(t *testing.T, k shrinkKit, name string) {
		t.Helper()
		if err := k.api.Purge(t.Context(), k.node(t, name)); err != nil {
			t.Fatalf("Purge: %v", err)
		}
	}
	wantShrinkInvariants(t, []shrinkCase{
		{name: "a node that is up", breakIt: func(t *testing.T, k shrinkKit) {
			purge(t, k, "prod-workers-1")
		}, want: "Nomad lists the node ready, want down"},
		{name: "a node that is down and has a machine", breakIt: func(t *testing.T, k shrinkKit) {
			k.w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Status = "down" })
			purge(t, k, "prod-workers-1")
		}, want: "the machine "},
		{name: "a node that is down and has no machine", breakIt: func(t *testing.T, k shrinkKit) {
			k.retire(t, "prod-workers-1")
			node := k.node(t, "prod-workers-1")
			k.del(t, "prod-workers-1")
			k.w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Status = "down" })
			if err := k.api.Purge(t.Context(), node); err != nil {
				t.Fatalf("Purge: %v", err)
			}
		}},
	})
}

// TestServerInvariantsOfAScaleDownFailTheTestForAGroupThatGrows holds a server group to the machines it has when the
// watch begins, in place of its size and one more: a fourth machine fails it, and a group at its count does not.
func TestServerInvariantsOfAScaleDownFailTheTestForAGroupThatGrows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		watch func(testing.TB, *vultrfake.Fake, *nomadWorld) *serverInvariants
		add   bool
		want  string
	}{
		{"a roll with a fourth machine", func(tb testing.TB, f *vultrfake.Fake, w *nomadWorld) *serverInvariants {
			return watchServers(tb, f, w, serversGroup, serverGroupSize)
		}, true, ""},
		{"a scale-down with a fourth machine", func(tb testing.TB, f *vultrfake.Fake, w *nomadWorld) *serverInvariants {
			return watchServers(tb, f, w, serversGroup, 1).shrinking()
		}, true, "the server group has 4 machines, want at most 3"},
		{"a scale-down of a combined group with a fourth machine",
			func(tb testing.TB, f *vultrfake.Fake, w *nomadWorld) *serverInvariants {
				return watchCombined(tb, f, w, serversGroup, 1).shrinking()
			}, true, "the server group has 4 machines, want at most 3"},
		{"a scale-down at its count", func(tb testing.TB, f *vultrfake.Fake, w *nomadWorld) *serverInvariants {
			return watchServers(tb, f, w, serversGroup, 1).shrinking()
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ftb := &failureTB{TB: t}
				b := newLiveWorldWith(t, func(w *nomadWorld) { w.ServersOverTime() })
				tc.watch(ftb, b.f, b.w)
				if tc.add {
					b.addServer(t, "prod-servers-3")
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

// removedWorld is a cluster of three servers and two workers, and the IDs of its machines by name.
func removedWorld(t *testing.T) (*vultrfake.Fake, map[string]string) {
	t.Helper()
	svc, f, _ := newRelease(t)
	mustUpdate(t, svc)
	ids := map[string]string{}
	for _, in := range f.Instances() {
		ids[in.Hostname] = in.ID
	}
	return f, ids
}

// TestWantRemovedComparesTheDeletesWithThePlan fails when the run deleted a machine that the plan does not delete or
// kept one that it deletes, and when the machines of an ordered group went in another order than the plan's. The order
// of the other groups does not matter.
func TestWantRemovedComparesTheDeletesWithThePlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		plan    []string // the machines that the plan deletes, in order
		run     []string // the machines that the run deletes, in order
		ordered []string
		want    string
	}{
		{"the same machines, the same order", []string{"prod-servers-2", "prod-servers-1", "prod-workers-1"},
			[]string{"prod-servers-2", "prod-servers-1", "prod-workers-1"}, []string{"servers"}, ""},
		{"another order of the servers", []string{"prod-servers-2", "prod-servers-1"},
			[]string{"prod-servers-1", "prod-servers-2"}, []string{"servers"}, "out of the plan's order"},
		{"another order of the workers", []string{"prod-workers-1", "prod-workers-0"},
			[]string{"prod-workers-0", "prod-workers-1"}, []string{"servers"}, ""},
		{"a servers group among the workers", []string{"prod-servers-2", "prod-workers-1", "prod-servers-1"},
			[]string{"prod-workers-1", "prod-servers-2", "prod-servers-1"}, []string{"servers"}, ""},
		{"a machine more", []string{"prod-workers-1"}, []string{"prod-workers-1", "prod-workers-0"}, nil,
			"differ from the plan's node deletes"},
		{"a machine less", []string{"prod-workers-1", "prod-workers-0"}, []string{"prod-workers-1"}, nil,
			"differ from the plan's node deletes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f, ids := removedWorld(t)
				var plan app.UpdatePlan
				for _, name := range tc.plan {
					plan.Nodes = append(plan.Nodes, app.NodeChange{Action: app.NodeDelete, Name: name, ID: ids[name]})
				}
				plan.Nodes = append(plan.Nodes, app.NodeChange{Action: app.NodeCreate, Name: "prod-workers-7"})
				for _, name := range tc.run {
					if err := f.DeleteInstance(t.Context(), ids[name]); err != nil {
						t.Fatalf("DeleteInstance: %v", err)
					}
				}
				ftb := &failureTB{TB: t}

				wantRemoved(ftb, plan, f, tc.ordered...)

				got := strings.Join(ftb.failures(), "\n")
				if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
					t.Errorf("wantRemoved failed with %q, want a failure that says %q", got, tc.want)
				}
			})
		})
	}
}
