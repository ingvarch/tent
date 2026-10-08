package app_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// workersMetaYAML is the test workers with a meta, which changes their spec hash.
const workersMetaYAML = workersYAML + "  nomad:\n    meta: {team: web}\n"

// rollWorld is the test cluster after an update, with a Nomad that follows it.
func rollWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t)
	mustUpdate(t, svc)
	return svc, f, w
}

// outdatedWorld is rollWorld whose workers' spec then changed and was applied by a second update: the update writes
// the completed spec and replaces no node, so both workers carry the old hash.
func outdatedWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := rollWorld(t)
	mustReplace(t, svc, workersMetaYAML)
	mustUpdate(t, svc)
	return svc, f, w
}

// tolerant is a Nomad API whose autopilot reports that the servers can lose one voter, as the Nomad of the test world
// does not say.
type tolerant struct{ nomadops.API }

func (t tolerant) Health(ctx context.Context) (nomadops.Health, error) {
	h, err := t.API.Health(ctx)
	h.FailureTolerance = 1
	return h, err
}

// withTolerance makes the Nomad of svc report that the servers can lose one voter, so that the decisions give a step to
// a server group instead of refusing it for the want of one.
func withTolerance(svc *app.Service) {
	inner := svc.Nomad
	svc.Nomad = func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return tolerant{api}, err
	}
}

// instanceNamed returns the ID of the instance of f whose hostname is name.
func instanceNamed(t *testing.T, f *vultrfake.Fake, name string) string {
	t.Helper()
	for _, in := range f.Instances() {
		if in.Hostname == name {
			return in.ID
		}
	}
	t.Fatalf("the fake has no instance %s", name)
	return ""
}

// nomadReads are the calls of the Nomad fake that read.
var nomadReads = []string{"Leader", "Peers", "Health", "Members", "Nodes", "KeyringReady"}

// untouched checks that what a call does to the store, the cloud and Nomad is only reading, and that it takes no lock:
// the test holds the cluster's lock, which makes a call that takes it fail.
type untouched struct {
	svc          *app.Service
	f            *vultrfake.Fake
	w            *nomadWorld
	store        map[string]string
	cloud, nomad int
}

// watch starts to check the calls that follow, with the cluster's lock held.
func watch(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld) untouched {
	t.Helper()
	holdLock(t, svc.Store)
	return untouched{svc: svc, f: f, w: w, store: snapshot(t, svc.Store), cloud: len(f.Calls()), nomad: len(w.Log())}
}

// check fails the test unless the calls since watch only read.
func (u untouched) check(t *testing.T) {
	t.Helper()
	wantSnapshot(t, u.svc.Store, u.store)
	wantNoWrites(t, u.f.Calls()[u.cloud:])
	for _, name := range nomadCallNames(u.w, u.nomad) {
		if !slices.Contains(nomadReads, name) {
			t.Errorf("the Nomad fake took the call %s, which writes", name)
		}
	}
}

// rollingUpdate runs a rolling update of the test cluster without apply.
func rollingUpdate(svc *app.Service, opts app.RollOptions) (app.RollPlan, error) {
	return svc.RollingUpdate(context.Background(), "prod", opts)
}

// TestRollingUpdatePlansTheOutdatedWorkers lists both workers as outdated by their hash, leaves the servers up to
// date and names the first step, a create, with the plan as text and as JSON. It reads the machines once and Nomad in
// the order of the observation, and writes nothing.
func TestRollingUpdatePlansTheOutdatedWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		u := watch(t, svc, f, w)
		listed := countCalls(f, "ListInstances")

		plan, err := rollingUpdate(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		u.check(t)
		if got := countCalls(f, "ListInstances") - listed; got != 1 {
			t.Errorf("RollingUpdate listed the machines %d times, want once", got)
		}
		if diff := cmp.Diff([]string{"Peers", "Health", "Members", "Nodes"}, nomadCallNames(w, u.nomad)); diff != "" {
			t.Errorf("the calls of Nomad (-want +got):\n%s", diff)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		checkGolden(t, "roll_plan.golden", text.String())
		checkGolden(t, "roll_plan.json.golden", encodeJSON(t, plan, "  "))
		if plan.Applied {
			t.Error("the plan of a run without apply says it applied")
		}
	})
}

// TestRollingUpdateWithNothingOutdated has no next step, and says so.
func TestRollingUpdateWithNothingOutdated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		u.check(t)
		if plan.Next != nil {
			t.Errorf("Next = %+v, want none", plan.Next)
		}
		want := []app.RollGroup{
			{Name: "servers", Role: v1alpha1.RoleServer, Size: 3},
			{Name: "workers", Role: v1alpha1.RoleClient, Size: 2},
		}
		if diff := cmp.Diff(want, plan.Groups); diff != "" {
			t.Errorf("groups (-want +got):\n%s", diff)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		if !strings.HasSuffix(text.String(), "\n\nNothing to roll.\n") {
			t.Errorf("text = %q, want it to end with Nothing to roll.", text.String())
		}
	})
}

// TestRollingUpdateRefusalOfAnUpToDateClusterIsNotNothingToRoll ends the text of a refused plan, whose groups have
// nothing outdated, after the group lines: it does not say that there is nothing to roll. Each case is a refusal of
// another check: the rollout (a newer Nomad), the join check and the role check.
func TestRollingUpdateRefusalOfAnUpToDateClusterIsNotNothingToRoll(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		world func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld)
		later bool   // the clock is past the time that a client has to join
		cause string // a part of the refusal, which tells the check that refused
	}{
		{"newer Nomad", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Version = "9.9.9" })
			return svc, f, w
		}, false, "tent never moves a node to an older Nomad"},
		{"unjoined client", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			return unjoinedWorld(t, func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") })
		}, true, "has not joined within 31 minutes"},
		{"unjoined server", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			dropJoinedTag(t, f, instanceNamed(t, f, "prod-servers-0"))
			return svc, f, w
		}, false, "tent cannot roll server and combined groups yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, _ := tc.world(t)
				if tc.later {
					svc.Now = func() time.Time { return time.Now().Add(32 * time.Minute) }
				}

				plan, err := rollingUpdate(svc, app.RollOptions{})

				if err == nil || !strings.Contains(err.Error(), tc.cause) {
					t.Fatalf("RollingUpdate error = %v, want a refusal that says %q", err, tc.cause)
				}
				want := "node group servers (server, size 3): up to date\nnode group workers (client, size 2): up to date\n"
				if got := rollText(t, plan); got != want {
					t.Errorf("text = %q, want only the group lines %q", got, want)
				}
			})
		})
	}
}

// TestRollingUpdateForceMarksTheSelectedWorkers forces every worker of a cluster whose machines are up to date, and
// the groups that the selection leaves out are not in the plan.
func TestRollingUpdateForceMarksTheSelectedWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{NodeGroups: []string{"workers"}, Force: true})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		u.check(t)
		if len(plan.Groups) != 1 || plan.Groups[0].Name != "workers" {
			t.Fatalf("groups = %+v, want the workers alone", plan.Groups)
		}
		var got []string
		for _, n := range plan.Groups[0].Outdated {
			got = append(got, n.Name+" "+n.Reason)
		}
		if want := []string{"prod-workers-0 forced", "prod-workers-1 forced"}; !slices.Equal(want, got) {
			t.Errorf("outdated = %q, want %q", got, want)
		}
		if plan.Next == nil || plan.Next.Action != "create" || plan.Next.Node != "prod-workers-2" {
			t.Errorf("Next = %+v, want the create of prod-workers-2", plan.Next)
		}
	})
}

// TestRollingUpdateSelectsGroups takes every group for an empty list, each name once and the groups by name, and fails
// for a name that the specs lack before it calls the cloud or Nomad.
func TestRollingUpdateSelectsGroups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		for _, tc := range []struct {
			name string
			list []string
			want []string
		}{
			{"an empty list", []string{}, []string{"servers", "workers"}},
			{"no list", nil, []string{"servers", "workers"}},
			{"a name twice", []string{"workers", "workers"}, []string{"workers"}},
			{"names out of order", []string{"workers", "servers"}, []string{"servers", "workers"}},
		} {
			plan, err := rollingUpdate(svc, app.RollOptions{NodeGroups: tc.list})
			if err != nil {
				t.Fatalf("%s: RollingUpdate: %v", tc.name, err)
			}
			var got []string
			for _, g := range plan.Groups {
				got = append(got, g.Name)
			}
			if !slices.Equal(tc.want, got) {
				t.Errorf("%s: groups = %q, want %q", tc.name, got, tc.want)
			}
		}

		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		_, err := rollingUpdate(svc, app.RollOptions{NodeGroups: []string{"workers", "db", "cache"}})

		wantError(t, err, "node group db is not in the specs of cluster prod; its node groups are servers and workers")
		if len(f.Calls()) != cloudCalls || len(w.Log()) != nomadCalls {
			t.Error("a name that the specs lack was found out after a call to the cloud or Nomad")
		}
	})
}

// TestRollingUpdateRefusesAtTheStart fails or refuses, in the order of the checks of a run, with only reads, no lock
// and no change of the store. A refusal of the decisions comes with the plan of the groups, a failed check without.
func TestRollingUpdateRefusesAtTheStart(t *testing.T) {
	const server = "node group servers: tent cannot roll server and combined groups yet; " +
		"select client groups with --nodegroups"
	for _, tc := range []struct {
		name   string
		world  func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld)
		opts   app.RollOptions
		want   string
		groups bool
	}{
		{"specs that changed since the last update", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			mustReplace(t, svc, workersMetaYAML)
			return svc, f, w
		}, app.RollOptions{}, "the specs of cluster prod changed since the last tent update cluster; run it first", false},
		{"no completed spec", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			if err := svc.Store.Delete(t.Context(), completedPath); err != nil {
				t.Fatal(err)
			}
			return svc, f, w
		}, app.RollOptions{}, "cluster prod has no completed spec; run tent update cluster first", false},
		{"no Nomad yet", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			if err := svc.Store.Delete(t.Context(), "prod/nomad/bootstrapped"); err != nil {
				t.Fatal(err)
			}
			return svc, f, w
		}, app.RollOptions{}, "cluster prod has no Nomad yet; run tent update cluster first", false},
		{"no Nomad and no completed spec: the mark is checked first", func(
			t *testing.T,
		) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			for _, p := range []string{"prod/nomad/bootstrapped", completedPath} {
				if err := svc.Store.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			return svc, f, w
		}, app.RollOptions{}, "cluster prod has no Nomad yet; run tent update cluster first", false},
		{"no completed spec and a missing secret: the spec is checked first", func(
			t *testing.T,
		) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			for _, p := range []string{"prod/secrets/gossip.key", completedPath} {
				if err := svc.Store.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			return svc, f, w
		}, app.RollOptions{}, "cluster prod has no completed spec; run tent update cluster first", false},
		{"a missing secret", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			if err := svc.Store.Delete(t.Context(), "prod/secrets/gossip.key"); err != nil {
				t.Fatal(err)
			}
			return svc, f, w
		}, app.RollOptions{}, "the state store lacks secrets/gossip.key; run tent update cluster first", false},
		{"servers that never joined", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			for _, name := range []string{"prod-servers-0", "prod-servers-1", "prod-servers-2"} {
				dropJoinedTag(t, f, instanceNamed(t, f, name))
			}
			return svc, f, w
		}, app.RollOptions{}, "cluster prod has no server that joined; run tent update cluster first", false},
		{"outdated servers in the default selection", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			withTolerance(svc)
			mustReplace(t, svc, keyedClusterYAML+"  nomad:\n    extraConfig:\n      server: 'raft_multiplier = 3'\n")
			mustUpdate(t, svc)
			return svc, f, w
		}, app.RollOptions{}, server, true},
		{"force with the default selection", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			withTolerance(svc)
			return svc, f, w
		}, app.RollOptions{Force: true}, server, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := tc.world(t)
				u := watch(t, svc, f, w)

				plan, err := rollingUpdate(svc, tc.opts)

				wantError(t, err, tc.want)
				u.check(t)
				if tc.groups != (len(plan.Groups) > 0) {
					t.Errorf("groups = %+v, want some: %v", plan.Groups, tc.groups)
				}
				if plan.Next != nil {
					t.Errorf("Next = %+v, want none for a refused run", plan.Next)
				}
			})
		})
	}
}

// TestRollingUpdateServerRefusalWithoutClientGroups leaves the advice about --nodegroups out when the specs have no
// client group.
func TestRollingUpdateServerRefusalWithoutClientGroups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, serversYAML)
		mustUpdate(t, svc)
		withTolerance(svc)
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{Force: true})

		wantError(t, err, "node group servers: tent cannot roll server and combined groups yet")
		u.check(t)
		if len(plan.Groups) != 1 {
			t.Errorf("groups = %+v, want the servers", plan.Groups)
		}
	})
}

// TestRollingUpdateRefusesACombinedGroup refuses a step of a combined group, without the advice about --nodegroups
// when the specs have no client group.
func TestRollingUpdateRefusesACombinedGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t, keyedClusterYAML, combinedYAML)
		mustUpdate(t, svc)
		withTolerance(svc)
		u := watch(t, svc, f, w)

		_, err := rollingUpdate(svc, app.RollOptions{Force: true})

		wantError(t, err, "node group all: tent cannot roll server and combined groups yet")
		u.check(t)
	})
}

// unjoinedWorld is rollWorld in which the machine of prod-workers-1 lost its joined label, as an unfinished roll leaves
// a new machine, and the specs ask for the same nodes: the next step waits for it to join.
func unjoinedWorld(
	t *testing.T, edit func(f *vultrfake.Fake, w *nomadWorld),
) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := rollWorld(t)
	dropJoinedTag(t, f, instanceNamed(t, f, "prod-workers-1"))
	edit(f, w)
	return svc, f, w
}

// TestRollingUpdateShowsTheWaitForANodeToJoin plans the wait for a machine that has not joined, as long as the run
// would carry it out: its node is not listed yet, or is listed and ready.
func TestRollingUpdateShowsTheWaitForANodeToJoin(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *vultrfake.Fake, w *nomadWorld)
	}{
		{"its node is ready, so the run scrubs the machine", func(*vultrfake.Fake, *nomadWorld) {}},
		{"its node is not listed yet", func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := unjoinedWorld(t, tc.edit)
				u := watch(t, svc, f, w)

				plan, err := rollingUpdate(svc, app.RollOptions{})

				if err != nil {
					t.Fatalf("RollingUpdate: %v", err)
				}
				u.check(t)
				want := &app.RollStep{
					Action: "wait-joined", Group: "workers", Node: "prod-workers-1",
					ID: instanceNamed(t, f, "prod-workers-1"), Text: "wait until node prod-workers-1 joins",
				}
				if diff := cmp.Diff(want, plan.Next); diff != "" {
					t.Errorf("Next (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestRollingUpdateRefusesAWaitThatTheRunWouldRefuse fails for a wait of which the run would refuse the first poll,
// with the plan of the groups: a client that has not joined within the life of its intro token and that Nomad does
// not list.
func TestRollingUpdateRefusesAWaitThatTheRunWouldRefuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedWorld(t, func(_ *vultrfake.Fake, w *nomadWorld) { w.DropNode("prod-workers-1") })
		svc.Now = func() time.Time { return time.Now().Add(32 * time.Minute) }
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{})

		wantError(t, err, "node prod-workers-1 has not joined within 31 minutes of its creation; "+
			"run tent update cluster, which deletes it and creates it again")
		u.check(t)
		if len(plan.Groups) != 2 || plan.Next != nil {
			t.Errorf("plan = %+v, want the two groups and no next step", plan)
		}
	})
}

// TestRollingUpdateRefusesAMachineWithoutAnAddress fails for a wait for a machine that the cloud reports without a
// private address.
func TestRollingUpdateRefusesAMachineWithoutAnAddress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		id := instanceNamed(t, f, "prod-workers-1")
		for _, in := range f.Instances() {
			if in.ID != id {
				continue
			}
			if err := f.DeleteInstance(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			in.ID, in.MainIP = "", ""
			in.Tags = withoutJoined(in.Tags)
			f.AddInstance(t, in) // no VPC: the cloud reports no private address
		}
		w.DropNode("prod-workers-1")
		u := watch(t, svc, f, w)

		plan, err := rollingUpdate(svc, app.RollOptions{})

		wantError(t, err, "node prod-workers-1: the cloud reports no private address for it yet; run the command again")
		u.check(t)
		if len(plan.Groups) != 2 || plan.Next != nil {
			t.Errorf("plan = %+v, want the two groups and no next step", plan)
		}
	})
}

// TestRollingUpdateWithoutNomadClient fails when the service has no way to reach Nomad.
func TestRollingUpdateWithoutNomadClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		svc.Nomad = nil
		u := watch(t, svc, f, w)

		_, err := rollingUpdate(svc, app.RollOptions{})

		wantError(t, err, "no Nomad client is set up")
		u.check(t)
	})
}

// TestRollingUpdateRefusesWhatTheDecisionsRefuse returns the refusal of rollout with the plan of the groups.
func TestRollingUpdateRefusesWhatTheDecisionsRefuse(t *testing.T) {
	pinned := func(t *testing.T, svc *app.Service) string {
		t.Helper()
		return decode(t, string(get(t, svc.Store, completedPath))).Cluster.Spec.Nomad.Version
	}
	for _, tc := range []struct {
		name  string
		world func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld)
		want  func(t *testing.T, svc *app.Service, f *vultrfake.Fake) string
	}{
		{"two machines of one name", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := outdatedWorld(t)
			twinOf(t, f, instanceNamed(t, f, "prod-workers-0"))
			return svc, f, w
		}, func(_ *testing.T, _ *app.Service, f *vultrfake.Fake) string {
			ids := []string{}
			for _, in := range f.Instances() {
				if in.Hostname == "prod-workers-0" {
					ids = append(ids, in.ID)
				}
			}
			return "node group workers: machines " + ids[0] + " and " + ids[1] +
				" share the name prod-workers-0; run tent update cluster first"
		}},
		{"a node that runs a newer Nomad than the pinned one", func(
			t *testing.T,
		) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := outdatedWorld(t)
			w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Version = "9.9.9" })
			return svc, f, w
		}, func(t *testing.T, svc *app.Service, _ *vultrfake.Fake) string {
			return "tent never moves a node to an older Nomad: the cluster is pinned to " + pinned(t, svc) +
				", and node prod-workers-0 runs 9.9.9"
		}},
		{"a server that runs an older Nomad than a new node", func(
			t *testing.T,
		) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := outdatedWorld(t)
			w.ChangeServer("prod-servers-0", func(s *nomadops.ServerHealth) { s.Version = "1.0.0" })
			return svc, f, w
		}, func(t *testing.T, svc *app.Service, _ *vultrfake.Fake) string {
			return "node group workers: a new node would run Nomad " + pinned(t, svc) +
				", newer than the 1.0.0 of server prod-servers-0.global; roll the servers first"
		}},
		{"a group that cannot go on", func(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
			svc, f, w := rollWorld(t)
			grown := edit(t, workersMetaYAML, "size: 2", "size: 3")
			mustReplace(t, svc, grown+"  rollingUpdate:\n    maxSurge: 0\n    maxUnavailable: 1\n")
			mustUpdate(t, svc) // creates prod-workers-2 with the new hash
			w.ChangeNode("prod-workers-2", func(n *nomadops.Node) { n.Eligible = false })
			return svc, f, w
		}, func(*testing.T, *app.Service, *vultrfake.Fake) string {
			return "node group workers: cannot go on: prod-workers-2 is not eligible; " +
				"with maxSurge 0 and maxUnavailable 1 no outdated node can be replaced"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := tc.world(t)
				u := watch(t, svc, f, w)

				plan, err := rollingUpdate(svc, app.RollOptions{})

				wantError(t, err, tc.want(t, svc, f))
				u.check(t)
				if len(plan.Groups) == 0 || plan.Next != nil {
					t.Errorf("plan = %+v, want the groups and no next step", plan)
				}
			})
		})
	}
}

// TestRollingUpdateFailsForWhatIsNotThere fails for a cluster that the store lacks, a tent that is too old and a
// cloud that does not accept the specs, and writes nothing.
func TestRollingUpdateFailsForWhatIsNotThere(t *testing.T) {
	t.Run("a cluster that the store lacks", func(t *testing.T) {
		svc, root := newService(t)
		withCloud(svc)

		_, err := rollingUpdate(svc, app.RollOptions{})

		wantError(t, err, notFound(svc, "cluster prod"))
		wantNothingWritten(t, root)
	})
	t.Run("a cluster that a newer tent wrote", func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		withCloud(svc)
		put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
		svc.Version = "v0.4.0"

		_, err := rollingUpdate(svc, app.RollOptions{})

		if err == nil || !errors.Is(err, statestore.ErrTentTooOld) {
			t.Errorf("RollingUpdate error = %v, want one that matches statestore.ErrTentTooOld", err)
		}
	})
	t.Run("a provider that tent cannot reach", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := rollWorld(t)
			svc.Providers = nil
			u := watch(t, svc, f, w)

			_, err := rollingUpdate(svc, app.RollOptions{})

			wantError(t, err, "no cloud providers are set up")
			u.check(t)
		})
	})
	t.Run("a store that cannot read the mark of the bootstrap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := rollWorld(t)
			u := watch(t, svc, f, w)
			inner := svc.Store
			svc.Store = unreadable{inner, "prod/nomad/bootstrapped"}

			_, err := rollingUpdate(svc, app.RollOptions{})

			if !errors.Is(err, errUnreadable) {
				t.Errorf("RollingUpdate error = %v, want one that wraps %v", err, errUnreadable)
			}
			svc.Store = inner
			u.check(t)
		})
	})
	t.Run("a secret that does not load", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := rollWorld(t)
			put(t, svc.Store, "prod/pki/ca-bundle.pem", []byte("not a bundle\n"))
			u := watch(t, svc, f, w)

			_, err := rollingUpdate(svc, app.RollOptions{})

			wantError(t, err, "prod/pki/private/ca.key and prod/pki/ca-bundle.pem: CA bundle: no certificates")
			u.check(t)
		})
	})
	t.Run("release files that cannot be read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := rollWorld(t)
			svc.Assets.Client = &http.Client{Transport: brokenSites{}}
			u := watch(t, svc, f, w)

			_, err := rollingUpdate(svc, app.RollOptions{})

			if !errors.Is(err, errSitesDown) {
				t.Errorf("RollingUpdate error = %v, want one that wraps %v", err, errSitesDown)
			}
			u.check(t)
		})
	})
	t.Run("a machine type that the cloud lacks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := rollWorld(t)
			mustReplace(t, svc, edit(t, workersYAML, "vc2-2c-4gb", "no-such-plan"))
			u := watch(t, svc, f, w)

			_, err := rollingUpdate(svc, app.RollOptions{})

			if err == nil || !strings.Contains(err.Error(), "no-such-plan") {
				t.Errorf("RollingUpdate error = %v, want one that names the machine type", err)
			}
			u.check(t)
		})
	})
}

// errUnreadable is why an unreadable store fails.
var errUnreadable = errors.New("the store cannot read it")

// unreadable is a store that cannot read the object at path.
type unreadable struct {
	statestore.Store
	path string
}

func (s unreadable) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	if p == s.path {
		return nil, "", errUnreadable
	}
	return s.Store.Get(ctx, p)
}

// errSitesDown is why brokenSites fail.
var errSitesDown = errors.New("the release sites are down")

// brokenSites is a release site that answers no request.
type brokenSites struct{}

func (brokenSites) RoundTrip(*http.Request) (*http.Response, error) { return nil, errSitesDown }

// TestRollingUpdateReportsWhatFailsToRead fails for a list of the machines or a read of Nomad that fails, naming the
// read of Nomad.
func TestRollingUpdateReportsWhatFailsToRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := rollWorld(t)
		f.Fail(t, "ListInstances", errors.New("cloud down"), 1)
		if _, err := rollingUpdate(svc, app.RollOptions{}); err == nil || !strings.Contains(err.Error(), "cloud down") {
			t.Errorf("RollingUpdate error = %v, want the error of the list", err)
		}

		w.Fail(t, "Health", errors.New("autopilot down"))
		_, err := rollingUpdate(svc, app.RollOptions{})
		wantError(t, err, "read autopilot's report: autopilot down")
	})
}

// TestRollingUpdateStopsWhenInterrupted returns the interruption, which stands for the end of the context.
func TestRollingUpdateStopsWhenInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "ListInstances" {
				cancel()
			}
			return next(ctx)
		})

		_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{})

		if err == nil || err.Error() != "interrupted" || !errors.Is(app.Stands(err), context.Canceled) {
			t.Errorf("RollingUpdate error = %v (stands for %v), want an interruption", err, app.Stands(err))
		}
	})
}
