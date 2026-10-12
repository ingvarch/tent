package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// growthWorld returns the test cluster with one server and two workers after an update, in a world whose servers
// change over time.
func growthWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t, keyedClusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
	w.ServersOverTime()
	mustUpdate(t, svc)
	return svc, f, w
}

// resizeServers replaces the specs of the test cluster with ones whose server group has the given size.
func resizeServers(t *testing.T, svc *app.Service, size int) {
	t.Helper()
	mustReplace(t, svc, keyedClusterYAML, edit(t, serversYAML, "size: 3", fmt.Sprintf("size: %d", size)), workersYAML)
}

// listedServers returns the names of the machines of the server group that f lists, sorted.
func listedServers(f *vultrfake.Fake) []string { return machinesOf(f, serversGroup) }

// raftAddresses returns the address of each server in the Raft configuration, by its machine's name, and fails the
// test for a server that does not vote.
func raftAddresses(t *testing.T, svc *app.Service) map[string]netip.Addr {
	t.Helper()
	api, err := svc.Nomad(nomadops.Config{Address: "198.51.100.1:4646"})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	peers, err := api.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	out := map[string]netip.Addr{}
	for _, p := range peers {
		name := strings.TrimSuffix(p.Name, ".global")
		if !p.Voter {
			t.Errorf("the server %s does not vote", name)
		}
		out[name] = p.Address.Addr()
	}
	return out
}

// wantGrown fails the test unless the server group has exactly the machines called want, Nomad lists them as voters,
// the cluster is valid and the store holds the highest index of the names. A machine that the group did not have
// before carries the new size in bootstrap_expect and a seed of the servers that had a machine before it.
func wantGrown(t *testing.T, svc *app.Service, f *vultrfake.Fake, before, want []string, size int) {
	t.Helper()
	if diff := cmp.Diff(want, listedServers(f)); diff != "" {
		t.Errorf("the servers (-want +got):\n%s", diff)
	}
	addresses := raftAddresses(t, svc)
	var voters []string
	for name := range addresses {
		voters = append(voters, name)
	}
	slices.Sort(voters)
	if diff := cmp.Diff(want, voters); diff != "" {
		t.Errorf("the voters in the Raft configuration (-want +got):\n%s", diff)
	}
	existing := slices.Clone(before)
	for _, name := range want {
		if slices.Contains(before, name) {
			continue
		}
		seed := seedOf(configOf(t, f, name))
		var wantSeed []string
		for _, e := range existing {
			wantSeed = append(wantSeed, addresses[e].String())
		}
		slices.Sort(seed)
		slices.Sort(wantSeed)
		if diff := cmp.Diff(wantSeed, seed); diff != "" {
			t.Errorf("the seed of %s (-want +got):\n%s", name, diff)
		}
		wantExpect := fmt.Sprintf("bootstrap_expect = %d", size)
		if got := fileOf(configOf(t, f, name), "/etc/nomad.d/10-node.hcl"); got == nil ||
			!strings.Contains(string(got.Content), wantExpect) {
			t.Errorf("the 10-node.hcl of %s lacks %q", name, wantExpect)
		}
		existing = append(existing, name)
	}
	wantJoined(t, f, append(slices.Clone(want), "prod-workers-0", "prod-workers-1")...)
	wantFailures(t, mustValidate(t, svc))
	highest := slices.Max(indexesOf(t, want))
	wantStored(t, svc.Store, namesPath, []byte(fmt.Sprintf("%d\n", highest)))
	wantLockFree(t, svc.Store)
}

// indexesOf returns the index in each of the server names.
func indexesOf(t *testing.T, names []string) []int {
	t.Helper()
	var out []int
	for _, name := range names {
		var index int
		if _, err := fmt.Sscanf(strings.TrimPrefix(name, machinePrefix(serversGroup)), "%d", &index); err != nil {
			t.Fatalf("the name %q has no index: %v", name, err)
		}
		out = append(out, index)
	}
	return out
}

// TestScaleGrowServers grows the server group of a built cluster from one server to three and to five through update.
// The plan of the first growth matches a golden file. After each growth the new servers vote, carry the joined label
// and the stub, have the new size in bootstrap_expect and a seed of the servers that existed, and the cluster is
// valid. At every call a leader runs, and no machine stops or goes.
func TestScaleGrowServers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := growthWorld(t)
		before := listedServers(f)
		if diff := cmp.Diff([]string{"prod-servers-0"}, before); diff != "" {
			t.Fatalf("the servers of the built cluster (-want +got):\n%s", diff)
		}
		for step, tc := range []struct {
			size int
			want []string
		}{
			{3, []string{"prod-servers-0", "prod-servers-1", "prod-servers-2"}},
			{5, []string{"prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-servers-3", "prod-servers-4"}},
		} {
			resizeServers(t, svc, tc.size)
			inv := watchServers(t, f, w, serversGroup, tc.size)
			stops := countCalls(f, "HaltInstance") + countCalls(f, "DeleteInstance")

			plan := mustUpdate(t, svc)

			if step == 0 {
				checkGolden(t, "flow_grow_servers.plan.golden", planText(t, plan))
			}
			wantGrown(t, svc, f, before, tc.want, tc.size)
			if got := countCalls(f, "HaltInstance") + countCalls(f, "DeleteInstance"); got != stops {
				t.Errorf("the growth to %d servers stopped or deleted %d machines", tc.size, got-stops)
			}
			inv.wantKept()
			wantConverged(t, svc)
			before = listedServers(f)
		}
	})
}

// TestScaleGrowTakesNamesAboveTheRememberedIndex grows the group from one server to three when the store remembers a
// higher index than any listed machine has: the new servers take the names above it.
func TestScaleGrowTakesNamesAboveTheRememberedIndex(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := growthWorld(t)
		if _, err := svc.Store.Put(t.Context(), namesPath, []byte("6\n"), statestore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		resizeServers(t, svc, 3)

		mustUpdate(t, svc)

		want := []string{"prod-servers-0", "prod-servers-7", "prod-servers-8"}
		if diff := cmp.Diff(want, listedServers(f)); diff != "" {
			t.Errorf("the servers (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, namesPath, []byte("8\n"))
		wantConverged(t, svc)
	})
}

// TestScaleGrowNamesAServerAboveTheRememberedIndexBesideAClientThatNeverRegistered plans a new server above the
// remembered index in the plan that also replaces a client that never registered.
func TestScaleGrowNamesAServerAboveTheRememberedIndexBesideAClientThatNeverRegistered(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := neverRegistered(t)
		sleepUntilAge(t, f, "instance-5", oldClient)
		if err := f.DeleteInstance(t.Context(), "instance-3"); err != nil {
			t.Fatal(err)
		}

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, append([]app.NodeChange{createOf("servers", v1alpha1.RoleServer, 3)},
			replaceWorker1()...)...)
	})
}

// cutUpdateAt applies the update of svc and ends its context at the first call to f that callKey gives as key, just
// after the fake carried it out when after is set and just before it otherwise. It fails the test unless the run ends
// with that interruption.
func cutUpdateAt(t *testing.T, svc *app.Service, f *vultrfake.Fake, key string, after bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cut atomic.Bool
	f.SetHook(atCall(f, key, 1, func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
		cut.Store(true)
		if after {
			_ = next(ctx)
		}
		cancel()
		return next(ctx)
	}))
	_, err := svc.Update(ctx, "prod", true)
	f.SetHook(nil)
	if !cut.Load() || !errors.Is(err, context.Canceled) {
		t.Fatalf("the run cut at %s (reached: %v) returned %v, want an error that matches %v", key, cut.Load(), err,
			context.Canceled)
	}
}

// TestScaleCutGrowth cuts the growth from one server to three at the first create, the first scrub and the first
// write of the index of the names, just before it and just after it. The next run ends with three servers, the names
// have a gap when the index was set aside for a server that was never created, and the store holds the highest index.
func TestScaleCutGrowth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		key   string // the call that ends the run; empty for a cut at the first write of the index
		after bool
		want  []string
	}{
		{
			"before the first create", "CreateInstance prod-servers-1", false,
			[]string{"prod-servers-0", "prod-servers-2", "prod-servers-3"},
		},
		{
			"after the first create", "CreateInstance prod-servers-1", true,
			[]string{"prod-servers-0", "prod-servers-1", "prod-servers-2"},
		},
		{
			"before the first scrub", "UpdateInstance <prod-servers-1>", false,
			[]string{"prod-servers-0", "prod-servers-1", "prod-servers-2"},
		},
		{
			"after the first scrub", "UpdateInstance <prod-servers-1>", true,
			[]string{"prod-servers-0", "prod-servers-1", "prod-servers-2"},
		},
		{"before the first write of the index", "", false, []string{"prod-servers-0", "prod-servers-1", "prod-servers-2"}},
		{"after the first write of the index", "", true, []string{"prod-servers-0", "prod-servers-2", "prod-servers-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f, _ := growthWorld(t)
				resizeServers(t, svc, 3)
				store := svc.Store
				if tc.key == "" {
					svc.Store = &cutStore{Store: store, path: namesPath, nth: 1, after: tc.after}
					if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, errCut) {
						t.Fatalf("the cut run returned %v, want an error that matches %v", err, errCut)
					}
					svc.Store = store
				} else {
					cutUpdateAt(t, svc, f, tc.key, tc.after)
				}

				mustUpdate(t, svc)

				wantGrown(t, svc, f, []string{"prod-servers-0"}, tc.want, 3)
				wantConverged(t, svc)
			})
		})
	}
}
