package app_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
)

// The two Nomad versions that the world's tests move between.
const (
	olderNomad = assetstest.OlderNomadVersion
	newerNomad = assetstest.NomadVersion
)

// olderWorld is the test cluster built on Nomad 2.0.6 by a service whose channel recommends it, in a world where each
// machine reports the Nomad version of its create request.
func olderWorld(t *testing.T) liveWorld {
	t.Helper()
	return newLiveWorldWith(t, func(w *nomadWorld) {
		w.NodeVersions()
		w.svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
	})
}

// moveToNewer makes the specs of the test cluster ask for Nomad 2.0.7 under a channel that recommends it, with
// servers machines in the server group and workers in the client group, and applies the update.
func (b liveWorld) moveToNewer(t *testing.T, servers, workers int) {
	t.Helper()
	svc := b.w.svc
	svc.Channels = stableChannel("2.0.0", newerNomad, olderNomad, newerNomad)
	mustReplace(t, svc, withVersion(t, keyedClusterYAML, newerNomad),
		edit(t, serversYAML, "size: 3", fmt.Sprintf("size: %d", servers)),
		edit(t, workersYAML, "size: 2", fmt.Sprintf("size: %d", workers)))
	mustUpdate(t, svc)
}

// serverVersions returns the version that autopilot's report shows for each server, by machine name.
func (b liveWorld) serverVersions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, s := range b.health(t).Servers {
		out[strings.TrimSuffix(s.Name, ".global")] = s.Version
	}
	return out
}

// listedVersions returns the version that the list of nodes shows for each node, by name.
func (b liveWorld) listedVersions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range b.nodes(t) {
		out[n.Name] = n.Version
	}
	return out
}

// nodeUserDataWith returns the user data, base64 as a create request carries it, of the NodeConfig of prod-servers-0
// with its name set to name, changed by change.
func (b liveWorld) nodeUserDataWith(t *testing.T, name string, change func(*nodeconfig.NodeConfig)) string {
	t.Helper()
	nc := configOf(t, b.f, "prod-servers-0")
	nc.Name = name
	change(nc)
	nc.SpecHash = nodeconfig.SpecHash(nc)
	data, err := nodeconfig.UserData(nc)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

// nodeUserDataFor is nodeUserDataWith with the version of the nomad asset set to version.
func (b liveWorld) nodeUserDataFor(t *testing.T, name, version string) string {
	t.Helper()
	return b.nodeUserDataWith(t, name, func(nc *nodeconfig.NodeConfig) {
		i := slices.IndexFunc(nc.Assets, isNomadAsset)
		if i < 0 {
			t.Fatal("the NodeConfig has no nomad asset")
		}
		nc.Assets[i].Version = version
	})
}

// TestWorldNodeVersionsKeepsOldMachinesAndGivesNewOnesTheirVersion checks that, once the specs ask for 2.0.7, the
// servers and nodes that were created on 2.0.6 still report 2.0.6, and a client that update creates reports 2.0.7 in
// the node list.
func TestWorldNodeVersionsKeepsOldMachinesAndGivesNewOnesTheirVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		b.moveToNewer(t, 5, 3)

		wantNodes := map[string]string{
			"prod-workers-0": olderNomad, "prod-workers-1": olderNomad, "prod-workers-2": newerNomad,
		}
		if diff := cmp.Diff(wantNodes, b.listedVersions(t)); diff != "" {
			t.Errorf("the versions of the nodes (-want +got):\n%s", diff)
		}
		wantServers := map[string]string{
			"prod-servers-0": olderNomad, "prod-servers-1": olderNomad, "prod-servers-2": olderNomad,
			"prod-servers-3": newerNomad, "prod-servers-4": newerNomad,
		}
		if diff := cmp.Diff(wantServers, b.serverVersions(t)); diff != "" {
			t.Errorf("the versions of the servers (-want +got):\n%s", diff)
		}
	})
}

// TestWorldNodeVersionsGivesAServerCreatedLaterItsVersion checks that a server machine created with a NodeConfig whose
// nomad asset is 2.0.7 reports 2.0.7 in autopilot's report, beside servers on 2.0.6.
func TestWorldNodeVersionsGivesAServerCreatedLaterItsVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		b.createNode(t, "prod-servers-3", "server", b.nodeUserDataFor(t, "prod-servers-3", newerNomad))

		want := map[string]string{
			"prod-servers-0": olderNomad, "prod-servers-1": olderNomad, "prod-servers-2": olderNomad,
			"prod-servers-3": newerNomad,
		}
		if diff := cmp.Diff(want, b.serverVersions(t)); diff != "" {
			t.Errorf("the versions of the servers (-want +got):\n%s", diff)
		}
	})
}

// errCutRun is why the update of a test stopped after a create.
var errCutRun = errors.New("the test cut the run")

// TestWorldNodeVersionsKeepsTheVersionOfTheFirstCreate checks that a create whose answer was lost, and that the next
// update finds by its operation id after the specs moved to 2.0.7, sends no second create request, and that the
// machine reports the version of the request that created it.
func TestWorldNodeVersionsKeepsTheVersionOfTheFirstCreate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		mustReplace(t, b.w.svc, keyedClusterYAML, serversYAML, edit(t, workersYAML, "size: 2", "size: 3"))
		b.f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name != "CreateInstance" {
				return next(ctx)
			}
			_ = next(ctx)
			return errCutRun
		})
		if _, err := b.w.svc.Update(t.Context(), "prod", true); !errors.Is(err, errCutRun) {
			t.Fatalf("the update returned %v, want an error that matches %v", err, errCutRun)
		}
		b.f.SetHook(nil)
		creates, sent := countCalls(b.f, "CreateInstance"), len(b.f.Calls())

		b.moveToNewer(t, 3, 3)
		byOp := func(c vultrfake.Call) bool {
			return c.Name == "ListInstances" && strings.HasPrefix(c.Arg, cloud.LabelOp+"=")
		}
		if !slices.ContainsFunc(b.f.Calls()[sent:], byOp) {
			t.Error("the update after the cut did not search for the machine by its operation id")
		}

		if n := countCalls(b.f, "CreateInstance"); n != creates {
			t.Errorf("%d create requests were sent in all, want %d: the repeated create must find its machine", n, creates)
		}
		if got := b.listedVersions(t)["prod-workers-2"]; got != olderNomad {
			t.Errorf("the machine of the repeated create reports %q, want %q, the version of the request that made it",
				got, olderNomad)
		}
	})
}

// TestWorldNodeVersionsReportsTheWorldsVersionWithoutARequest checks that a machine that has no create request, and
// one whose request has no NodeConfig or a NodeConfig without a nomad asset, report the world's version, and follow
// SetVersion.
func TestWorldNodeVersionsReportsTheWorldsVersionWithoutARequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		b.addServer(t, "prod-servers-3")
		b.createNode(t, "prod-servers-4", "server", "")
		b.createNode(t, "prod-servers-5", "server", b.nodeUserDataWith(t, "prod-servers-5",
			func(nc *nodeconfig.NodeConfig) {
				nc.Assets = slices.DeleteFunc(nc.Assets, isNomadAsset)
			}))

		for _, version := range []string{newerNomad, "2.0.5"} {
			b.w.SetVersion(version)
			got := b.serverVersions(t)
			for _, name := range []string{"prod-servers-3", "prod-servers-4", "prod-servers-5"} {
				if got[name] != version {
					t.Errorf("after SetVersion(%q) the server %s reports %q, want %q", version, name, got[name], version)
				}
			}
			if got["prod-servers-0"] != olderNomad {
				t.Errorf("after SetVersion(%q) prod-servers-0 reports %q, want %q", version, got["prod-servers-0"], olderNomad)
			}
		}
	})
}

// TestWorldNodeVersionsSurvivesTheScrubOfUserData checks that servers and nodes whose user data tent replaced with a
// stub still report the version of the request that created them.
func TestWorldNodeVersionsSurvivesTheScrubOfUserData(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		for _, in := range b.f.Instances() {
			if !isScrubbed(b.f, in.ID) {
				t.Fatalf("the user data of %s is not the stub after the update", in.Hostname)
			}
		}

		wantServers := map[string]string{
			"prod-servers-0": olderNomad, "prod-servers-1": olderNomad, "prod-servers-2": olderNomad,
		}
		if diff := cmp.Diff(wantServers, b.serverVersions(t)); diff != "" {
			t.Errorf("the versions of the servers (-want +got):\n%s", diff)
		}
		wantNodes := map[string]string{"prod-workers-0": olderNomad, "prod-workers-1": olderNomad}
		if diff := cmp.Diff(wantNodes, b.listedVersions(t)); diff != "" {
			t.Errorf("the versions of the nodes (-want +got):\n%s", diff)
		}
	})
}

// TestWorldNodeVersionsLetsATestChangeTheAnswer checks that ChangeServer and ChangeNode still decide the version that
// a server and a node report.
func TestWorldNodeVersionsLetsATestChangeTheAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := olderWorld(t)
		b.w.ChangeServer("prod-servers-1", func(s *nomadops.ServerHealth) { s.Version = "2.0.1" })
		b.w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Version = "2.0.2" })

		if got := b.serverVersions(t); got["prod-servers-1"] != "2.0.1" || got["prod-servers-0"] != olderNomad {
			t.Errorf("the versions of the servers are %v, want prod-servers-1 at 2.0.1 and prod-servers-0 at %s",
				got, olderNomad)
		}
		if got := b.listedVersions(t); got["prod-workers-1"] != "2.0.2" || got["prod-workers-0"] != olderNomad {
			t.Errorf("the versions of the nodes are %v, want prod-workers-1 at 2.0.2 and prod-workers-0 at %s",
				got, olderNomad)
		}
	})
}

// TestWorldReportsTheWorldsVersionWithoutNodeVersions checks that, without the knob, every server and node reports the
// world's version, whatever the NodeConfig of its request says.
func TestWorldReportsTheWorldsVersionWithoutNodeVersions(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newLiveWorldWith(t, func(w *nomadWorld) {
			w.svc.Channels = stableChannel("2.0.0", olderNomad, olderNomad)
			w.SetVersion("2.0.5")
		})
		b.createNode(t, "prod-servers-3", "server", b.nodeUserDataFor(t, "prod-servers-3", newerNomad))

		wantServers := map[string]string{
			"prod-servers-0": "2.0.5", "prod-servers-1": "2.0.5", "prod-servers-2": "2.0.5", "prod-servers-3": "2.0.5",
		}
		if diff := cmp.Diff(wantServers, b.serverVersions(t)); diff != "" {
			t.Errorf("the versions of the servers (-want +got):\n%s", diff)
		}
		wantNodes := map[string]string{"prod-workers-0": "2.0.5", "prod-workers-1": "2.0.5"}
		if diff := cmp.Diff(wantNodes, b.listedVersions(t)); diff != "" {
			t.Errorf("the versions of the nodes (-want +got):\n%s", diff)
		}
	})
}

// TestWorldNodeVersionsKeepsTheVersionOfADeletedServer checks that the record of a server whose machine was deleted,
// which the report keeps while the server is failing and as a removed peer until the report lags, still shows the
// version of the request that created the machine.
func TestWorldNodeVersionsKeepsTheVersionOfADeletedServer(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]func(*nomadWorld){
		"failing": func(w *nomadWorld) { w.SetFailAfter(40 * time.Second) },
		"lagging": func(w *nomadWorld) { w.SetReportLag(30 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				b := olderWorld(t)
				hold(b.w)
				b.deleteInstance(t, b.followers(t)[0])

				got := b.serverVersions(t)
				want := map[string]string{
					"prod-servers-0": olderNomad, "prod-servers-1": olderNomad, "prod-servers-2": olderNomad,
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("the versions of the servers (-want +got):\n%s", diff)
				}
			})
		})
	}
}
