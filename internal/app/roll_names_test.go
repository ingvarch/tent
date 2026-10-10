package app_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// serverIndex returns the index in the name of a server machine, such as 3 for prod-servers-3.
func serverIndex(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimPrefix(name, "prod-servers-"))
	if err != nil {
		t.Fatalf("%q is no name of a server machine: %v", name, err)
	}
	return n
}

// createdNames returns the names of the machines that the progress lines show as created, in order.
func createdNames(lines []string) []string {
	var names []string
	for _, l := range lines {
		if name, ok := strings.CutPrefix(l, "node started create "); ok {
			names = append(names, name)
		}
	}
	return names
}

// wantFreshServerNames fails the test unless the machines that the runs created took names that no machine of the
// world had had, one after the other above the old server's, the one server that is left has the highest name, and the
// store holds that index. The voters that run were a quorum at every call.
func (s *singleWorld) wantFreshServerNames(t *testing.T, lines []string) {
	t.Helper()
	s.inv.wantQuorumFor(quorumSpan)
	s.inv.wantKept()
	seen := map[string]bool{s.old[0].name: true}
	highest := serverIndex(t, s.old[0].name)
	for _, name := range createdNames(lines) {
		if seen[name] {
			t.Errorf("a roll created %s, a name that a machine of the world had had; the creates were %v", name,
				createdNames(lines))
		}
		seen[name] = true
		highest = max(highest, serverIndex(t, name))
	}
	servers := serverMachines(s.f)
	if len(servers) != 1 {
		t.Fatalf("the server group has %d machines after the rolls, want 1: %v", len(servers), servers)
	}
	if want := "prod-servers-" + strconv.Itoa(highest); servers[0].name != want {
		t.Errorf("the one server is %s, want %s, above every earlier name; the creates were %v", servers[0].name, want,
			createdNames(lines))
	}
	wantStored(t, s.svc.Store, "prod/names/servers", []byte(strconv.Itoa(highest)+"\n"))
}

// The options of a roll of the server group alone, plain and forced.
var (
	serversOnly   = app.RollOptions{NodeGroups: []string{"servers"}}
	serversForced = app.RollOptions{NodeGroups: []string{"servers"}, Force: true}
)

// TestRollNamesNoServerTakesANameThatAMachineHad cuts a roll of one server right after the new server joined and
// before the leadership moves, and runs the roll again. Both servers are outdated in the second run, and the old one
// leads, so the new one goes first: the run creates a server under a new name, above the one that went, where the
// names of the listed machines alone give the name that went. A forced run labels each machine for replacement and
// cuts the same way; so does a newer tent between two plain runs, whose spec hash differs.
func TestRollNamesNoServerTakesANameThatAMachineHad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// first is the options of the run that is cut, and between changes the world before the next run.
		first, again app.RollOptions
		between      func(t *testing.T, s *singleWorld)
	}{
		{
			name:  "a forced run, again",
			first: serversForced, again: serversForced,
		}, {
			name: "a newer tent between two plain runs", first: serversOnly, again: serversOnly,
			// Its tent-node is another asset, so every node group has another spec hash.
			between: func(_ *testing.T, s *singleWorld) { s.svc.Version = "v0.5.1" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newSingleWorld(t, time.Minute, 10*time.Second)
				lines := recordProgress(s.svc)

				s.cutRunOf(t, tc.first, "TransferLeadership", 1, false)
				if tc.between != nil {
					tc.between(t, s)
				}
				if _, err := applyRoll(s.svc, tc.again); err != nil {
					t.Fatalf("the run after the cut failed: %v", err)
				}

				s.wantFreshServerNames(t, *lines)
			})
		})
	}
}

// TestForcedRollOfAStoreWithoutTheNamesTakesNoNameAgain cuts a forced roll of one server after the create of the new
// server, deletes the object of the server names, as a cluster that an earlier tent built has none, and runs the roll
// again: the run takes the index that it stored itself as its floor, so no server gets a name that a machine had.
func TestForcedRollOfAStoreWithoutTheNamesTakesNoNameAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newSingleWorld(t, time.Minute, 10*time.Second)
		lines := recordProgress(s.svc)

		s.cutRunOf(t, serversForced, "TransferLeadership", 1, false)
		if err := s.svc.Store.Delete(t.Context(), namesPath); err != nil {
			t.Fatalf("delete %s: %v", namesPath, err)
		}
		if _, err := applyRoll(s.svc, serversForced); err != nil {
			t.Fatalf("the run after the cut failed: %v", err)
		}

		s.wantFreshServerNames(t, *lines)
	})
}

// eventLog is the order of the writes to the store and of the creates that reach the cloud.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

// watch makes the store of svc and the fake f add their writes and creates to the log, as "put <path>" and "create".
// The lock is left out. Label writes are logged as "label".
func (l *eventLog) watch(svc *app.Service, f *vultrfake.Fake) {
	add := func(event string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.events = append(l.events, event)
	}
	svc.Store = &writeLog{Store: svc.Store, onWrite: func(p string) {
		if p != lockPath {
			add("put " + p)
		}
	}}
	f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		switch c.Name {
		case "CreateInstance":
			add("create")
		case "UpdateInstance":
			add("label")
		}
		return next(ctx)
	})
}

// before returns the events up to the first event.
func (l *eventLog) before(event string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := slices.Index(l.events, event)
	if i < 0 {
		return slices.Clone(l.events)
	}
	return slices.Clone(l.events[:i])
}

// TestUpdateStoresTheHighestNameBeforeItChangesANode applies an update to a cluster whose store lacks the object of the
// server names, as one that an earlier tent built does, and whose only change is a new worker. The store holds the
// highest index of the listed servers right after the completed spec, and before the first machine is created.
func TestUpdateStoresTheHighestNameBeforeItChangesANode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := newRelease(t)
		mustUpdate(t, svc)
		if err := svc.Store.Delete(t.Context(), namesPath); err != nil {
			t.Fatalf("delete %s: %v", namesPath, err)
		}
		mustReplace(t, svc, keyedClusterYAML, serversYAML, edit(t, workersYAML, "size: 2", "size: 3"))
		var log eventLog
		log.watch(svc, f)

		mustUpdate(t, svc)

		if diff := cmp.Diff([]string{"put " + completedPath, "put " + namesPath}, log.before("create")); diff != "" {
			t.Errorf("the writes before the first create (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, namesPath, []byte("2\n"))
	})
}

// TestRollingUpdateStoresTheVersionAndTheHighestNameBeforeItChangesANode applies a roll of the servers of a store that
// lacks the object of the server names, with a tent newer than the one that built the cluster: under the lock it
// raises the version first, then stores the highest index of the listed servers, and then creates, each create after
// the write of its own index. A plan without apply writes nothing.
func TestRollingUpdateStoresTheVersionAndTheHighestNameBeforeItChangesANode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		if err := svc.Store.Delete(t.Context(), namesPath); err != nil {
			t.Fatalf("delete %s: %v", namesPath, err)
		}
		svc.Version = "v0.5.1"
		stored := snapshot(t, svc.Store)
		if _, err := rollingUpdate(svc, serversOnly); err != nil {
			t.Fatalf("the plan failed: %v", err)
		}
		wantSnapshot(t, svc.Store, stored)
		var log eventLog
		log.watch(svc, f)

		if _, err := applyRoll(svc, serversOnly); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		// The second write of the names is the first create's own index.
		want := []string{"put " + versionPath, "put " + namesPath, "put " + namesPath}
		if diff := cmp.Diff(want, log.before("create")); diff != "" {
			t.Errorf("the writes before the first create (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, versionPath, []byte("v0.5.1\n"))
		wantStored(t, svc.Store, namesPath, []byte("5\n"))
	})
}

// TestRollingUpdateStoresTheVersionAndTheHighestNameBeforeItLabelsAMachine applies a forced roll of the servers of a
// store that lacks the object of the server names, with a tent newer than the one that built the cluster: the version
// and the highest index of the listed servers are in the store before the first machine gets the replace label.
func TestRollingUpdateStoresTheVersionAndTheHighestNameBeforeItLabelsAMachine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := serversWorld(t, (*nomadWorld).ServersOverTime)
		if err := svc.Store.Delete(t.Context(), namesPath); err != nil {
			t.Fatalf("delete %s: %v", namesPath, err)
		}
		svc.Version = "v0.5.1"
		var log eventLog
		log.watch(svc, f)

		if _, err := applyRoll(svc, serversForced); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		want := []string{"put " + versionPath, "put " + namesPath}
		if diff := cmp.Diff(want, log.before("label")); diff != "" {
			t.Errorf("the writes before the first label (-want +got):\n%s", diff)
		}
	})
}

// TestRollingUpdateLeavesTheNamesOfGroupsItDoesNotRoll rolls the workers of a store that lacks the object of the server
// names: the roll writes no object of names, since it creates no server.
func TestRollingUpdateLeavesTheNamesOfGroupsItDoesNotRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		if err := svc.Store.Delete(t.Context(), namesPath); err != nil {
			t.Fatalf("delete %s: %v", namesPath, err)
		}

		plan, err := applyRoll(svc, app.RollOptions{NodeGroups: []string{"workers"}})

		if err != nil || plan.Rolled.Created == 0 {
			t.Fatalf("RollingUpdate rolled %+v and ended with %v, want a roll of the workers", plan.Rolled, err)
		}
		if got := list(t, svc.Store, namesPath); len(got) != 0 {
			t.Errorf("the roll of the workers wrote %v", got)
		}
	})
}

// TestPlansFailForAnObjectOfNamesThatIsNoIndex fails the plan of an update and that of a rolling update, which write
// nothing, when the object of the server names holds no index.
func TestPlansFailForAnObjectOfNamesThatIsNoIndex(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		put(t, svc.Store, namesPath, []byte("x\n"))
		stored := snapshot(t, svc.Store)
		const want = `prod/names/servers holds "x", which is no index of a node name; it must hold the highest index ` +
			"that a machine name of node group servers has had"

		_, updateErr := svc.Update(t.Context(), "prod", false)
		_, rollErr := rollingUpdate(svc, app.RollOptions{})

		wantError(t, updateErr, want)
		wantError(t, rollErr, want)
		wantSnapshot(t, svc.Store, stored)
	})
}
