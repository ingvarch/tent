package cli

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// outdatedWorkersYAML is the test workers with a meta, which changes their spec hash.
const outdatedWorkersYAML = workersYAML + "  nomad:\n    meta: {team: web}\n"

// slowServersYAML is the test cluster with a Nomad setting that only the servers take, which changes their spec hash.
const slowServersYAML = clusterYAML + "  nomad:\n    extraConfig:\n      server: 'raft_multiplier = 3'\n"

// roll returns the arguments that roll the test cluster in the store s, followed by more.
func roll(s state, more ...string) []string {
	return append([]string{"rolling-update", "cluster", "prod", "--state", s.url}, more...)
}

// rollRunner returns what executes tent with args against the Vultr fake f and one Nomad that follows f for the whole
// test, as the Nomad of a real cluster outlives each command.
func rollRunner(t *testing.T, f *vultrfake.Fake) func(args ...string) result {
	t.Helper()
	_, nomad := followedNomad(f)
	return func(args ...string) result {
		t.Helper()
		return runWithNomad(t, onVultr(f), nomad, args...)
	}
}

// outdatedCluster returns the built test cluster whose workers' spec changed and was applied by a second update,
// which replaces no node: the three workers carry the old spec hash. Call it in a synctest bubble.
func outdatedCluster(t *testing.T) (state, *vultrfake.Fake) {
	t.Helper()
	s, f := builtCluster(t)
	s.put(t, workersPath, outdatedWorkersYAML)
	if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
		t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
	}
	return s, f
}

const (
	upToDateServersLine = "node group servers (server, size 3): up to date\n"
	outdatedWorkersLine = "node group workers (client, size 3): 3 outdated: prod-workers-0 (ID instance-4), " +
		"prod-workers-1 (ID instance-5) and prod-workers-2 (ID instance-6)\n"
	firstStep = "\nNext: create node prod-workers-3 (client of workers, ams).\n"

	// rollPlan is the plan of the roll of the three outdated workers.
	rollPlan = upToDateServersLine + outdatedWorkersLine + firstStep
	// rollHint is what rolling-update prints on stderr after a plan with a next step.
	rollHint = "run with --yes to roll the nodes\n"
)

func TestRollingUpdateClusterPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)
		before, calls := s.objects(t), len(f.Calls())

		wantResult(t, tent(roll(s)...), 0, rollPlan, rollHint)

		wantNoWritesIn(t, f.Calls()[calls:])
		s.want(t, before)
	})
}

func TestRollingUpdateClusterPlanJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)

		got := tent(roll(s, "-o", "json")...)

		const want = `{
  "groups": [
    {
      "name": "servers",
      "role": "server",
      "size": 3,
      "outdated": []
    },
    {
      "name": "workers",
      "role": "client",
      "size": 3,
      "outdated": [
        {
          "name": "prod-workers-0",
          "id": "instance-4",
          "group": "workers",
          "reason": "spec hash"
        },
        {
          "name": "prod-workers-1",
          "id": "instance-5",
          "group": "workers",
          "reason": "spec hash"
        },
        {
          "name": "prod-workers-2",
          "id": "instance-6",
          "group": "workers",
          "reason": "spec hash"
        }
      ]
    }
  ],
  "next": {
    "action": "create",
    "group": "workers",
    "node": "prod-workers-3",
    "text": "create node prod-workers-3 (client of workers, ams)"
  }
}
`
		wantResult(t, got, 0, want, rollHint)
	})
}

func TestRollingUpdateClusterPlanYAML(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)

		got := tent(roll(s, "-o", "yaml")...)

		const want = `groups:
- name: servers
  outdated: []
  role: server
  size: 3
- name: workers
  outdated:
  - group: workers
    id: instance-4
    name: prod-workers-0
    reason: spec hash
  - group: workers
    id: instance-5
    name: prod-workers-1
    reason: spec hash
  - group: workers
    id: instance-6
    name: prod-workers-2
    reason: spec hash
  role: client
  size: 3
next:
  action: create
  group: workers
  node: prod-workers-3
  text: create node prod-workers-3 (client of workers, ams)
`
		wantResult(t, got, 0, want, rollHint)
	})
}

// TestRollingUpdateClusterWithNothingToRoll prints the groups and that nothing is left, with no hint, with and
// without --yes; --yes takes no lock, which the test holds.
func TestRollingUpdateClusterWithNothingToRoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		tent := rollRunner(t, f)
		holdLock(t, s)
		const plan = upToDateServersLine + "node group workers (client, size 3): up to date\n\nNothing to roll.\n"
		calls := len(f.Calls())

		wantResult(t, tent(roll(s)...), 0, plan, "")
		wantResult(t, tent(roll(s, "--yes", "--lock-timeout", "0")...), 0, "cluster prod has nothing to roll\n", "")

		wantNoWritesIn(t, f.Calls()[calls:])
	})
}

// rolledNode is a node of the roll of the three outdated workers, in the order of the roll: a new node is created
// (replaces is empty), or the machine that replaces names is deleted and its node purged. A roll with the default
// shape, maxSurge 1 and maxUnavailable 0, creates one extra worker first and then replaces each outdated one in turn,
// reusing its name once Nomad no longer lists it.
type rolledNode struct{ name, replaces, ip, oldIP string }

// rolledOrder is the order of the roll of the outdated workers instance-4 to instance-6, at 10.64.0.6 to 10.64.0.8.
var rolledOrder = []rolledNode{
	{name: "prod-workers-3", ip: "10.64.0.9"},
	{replaces: "instance-4", name: "prod-workers-0", oldIP: "10.64.0.6"},
	{name: "prod-workers-0", ip: "10.64.0.6"},
	{replaces: "instance-5", name: "prod-workers-1", oldIP: "10.64.0.7"},
	{name: "prod-workers-1", ip: "10.64.0.7"},
	{replaces: "instance-6", name: "prod-workers-2", oldIP: "10.64.0.8"},
}

// rolledLines returns the progress lines of the roll of rolledOrder as -o table prints them.
func rolledLines() []string {
	var lines []string
	for _, n := range rolledOrder {
		if n.replaces != "" {
			node := n.name + " (" + n.oldIP + ")"
			lines = append(lines,
				"marking node "+n.name+" ineligible", "node "+n.name+" is ineligible",
				"draining node "+n.name+" within 1h0m0s", "node "+n.name+" is draining",
				"deleting node "+n.name+" (ID "+n.replaces+")", "deleted node "+n.name+" (ID "+n.replaces+")",
				"purging node "+node+" from Nomad", "purged node "+node+" from Nomad")
			continue
		}
		lines = append(lines,
			"creating node "+n.name, "created node "+n.name+" ("+n.ip+")",
			"waiting for node "+n.name+" to register", "node "+n.name+" registered",
			"scrubbing the user data of node "+n.name, "scrubbed the user data of node "+n.name)
	}
	return lines
}

// rolledActions returns the action and the node of each step of the roll of rolledOrder that is done, as -o json
// prints them.
func rolledActions() []string {
	var done []string
	for _, n := range rolledOrder {
		for _, a := range []string{"create", "register", "scrub"} {
			if n.replaces == "" {
				done = append(done, a+" "+n.name)
			}
		}
		for _, a := range []string{"ineligible", "drain", "delete", "purge"} {
			if n.replaces != "" {
				done = append(done, a+" "+n.name)
			}
		}
	}
	return done
}

// rolledSummary is what rolling-update --yes prints after it rolled the three outdated workers.
const rolledSummary = "\nRolled: 3 created, 3 drained, 0 stopped, 3 deleted, 3 purged.\n"

// TestRollingUpdateClusterApply prints the plan made under the lock, each step on stderr and the summary, replaces
// the machines, and then finds nothing to roll.
func TestRollingUpdateClusterApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)

		got := tent(roll(s, "--yes")...)

		wantResult(t, got, 0, rollPlan+rolledSummary,
			openAPIWarning+strings.Join(rolledLines(), "\n")+"\n")
		wantInstances(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-3", "prod-workers-0",
			"prod-workers-1")
		wantResult(t, tent(roll(s, "--yes")...), 0, "cluster prod has nothing to roll\n", "")
	})
}

// TestRollingUpdateClusterApplyJSON prints each step as a JSON object on stderr and the plan that it applied on
// stdout.
func TestRollingUpdateClusterApplyJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)

		got := tent(roll(s, "--yes", "-o", "json")...)

		if got.code != 0 {
			t.Fatalf("exit code %d, stderr\n%s", got.code, got.errOut)
		}
		var plan struct {
			Applied bool
			Groups  []struct{ Name string }
			Next    *struct{ Action string }
			Rolled  map[string]int
		}
		if err := json.Unmarshal([]byte(got.out), &plan); err != nil {
			t.Fatalf("stdout is not the plan: %v\n%s", err, got.out)
		}
		stopped, hasStopped := plan.Rolled["stopped"]
		if !plan.Applied || len(plan.Groups) != 2 || plan.Next == nil ||
			plan.Rolled["created"] != 3 || plan.Rolled["drained"] != 3 || plan.Rolled["deleted"] != 3 ||
			plan.Rolled["purged"] != 3 || !hasStopped || stopped != 0 {
			t.Errorf("the plan = %+v, want it applied, with both groups, the step it began with, 3 of each and "+
				"no stopped machine", plan)
		}
		var done []string
		for _, e := range decodeProgress(t, got.errOut) {
			if e.Step == "done" {
				done = append(done, e.Action+" "+e.Name)
			}
		}
		if diff := cmp.Diff(rolledActions(), done); diff != "" {
			t.Errorf("the steps that are done (-want +got):\n%s", diff)
		}
	})
}

// TestRollingUpdateClusterSelectsNodeGroups takes the groups of --nodegroups: separated by commas or given more than
// once, each once, by name.
func TestRollingUpdateClusterSelectsNodeGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"one group", []string{"--nodegroups", "workers"}, outdatedWorkersLine + firstStep},
		{"commas", []string{"--nodegroups", "servers,workers"}, rollPlan},
		{"repeated", []string{"--nodegroups", "servers", "--nodegroups", "workers"}, rollPlan},
		{"twice", []string{"--nodegroups", "workers,workers"}, outdatedWorkersLine + firstStep},
		{"out of order", []string{"--nodegroups", "workers,servers"}, rollPlan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := outdatedCluster(t)

				wantResult(t, rollRunner(t, f)(roll(s, tc.args...)...), 0, tc.want, rollHint)
			})
		})
	}
}

// TestRollingUpdateClusterRefusesAnUnknownNodeGroup before any call to the cloud.
func TestRollingUpdateClusterRefusesAnUnknownNodeGroup(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()
	tent := rollRunner(t, f)

	wantError(t, tent(roll(s, "--nodegroups", "workers,db")...),
		"Error: node group db is not in the specs of cluster prod; its node groups are servers and workers\n")

	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}

// TestRollingUpdateClusterForce marks every machine of the selected groups forced, whatever its hash.
func TestRollingUpdateClusterForce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		tent := rollRunner(t, f)

		wantResult(t, tent(roll(s, "--force", "--nodegroups", "workers")...), 0,
			"node group workers (client, size 3): 3 outdated: prod-workers-0 (ID instance-4, forced), "+
				"prod-workers-1 (ID instance-5, forced) and prod-workers-2 (ID instance-6, forced)\n"+firstStep,
			rollHint)
	})
}

// serverRefusal is why tent does not roll the servers yet.
const serverRefusal = "node group servers: tent cannot roll server and combined groups yet; " +
	"select client groups with --nodegroups"

// TestRollingUpdateClusterRefusesToRollServers prints the plan, then the error, and exits with 1, with and without
// --yes and without a lock: for outdated servers, and for --force with the default selection. The client groups roll
// when they are selected.
func TestRollingUpdateClusterRefusesToRollServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		tent := rollRunner(t, f)
		s.put(t, clusterPath, slowServersYAML)
		if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
			t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
		}
		holdLock(t, s)
		calls := len(f.Calls())
		const plan = "node group servers (server, size 3): 3 outdated: prod-servers-0 (ID instance-1), " +
			"prod-servers-1 (ID instance-2) and prod-servers-2 (ID instance-3)\n" +
			"node group workers (client, size 3): up to date\n"

		for _, more := range [][]string{nil, {"--yes", "--lock-timeout", "0"}} {
			wantResult(t, tent(roll(s, more...)...), 1, plan, "Error: "+serverRefusal+"\n")
		}
		forced := tent(roll(s, "--force")...)
		if forced.code != 1 || !strings.HasSuffix(forced.errOut, "Error: "+serverRefusal+"\n") {
			t.Errorf("--force: exit code = %d, stderr\n%s\nwant 1 and the refusal", forced.code, forced.errOut)
		}

		wantNoWritesIn(t, f.Calls()[calls:])
	})
}

// TestRollingUpdateClusterRefusesWhatTheDecisionsRefuse prints the plan, then the refusal of the decisions, and exits
// with 1, with and without --yes and without a lock.
func TestRollingUpdateClusterRefusesWhatTheDecisionsRefuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		world, nomad := followedNomad(f)
		world.Register(nomadops.Node{ID: "n-instance-4", Name: "prod-workers-0", Status: "ready", Eligible: true,
			Address: netip.MustParseAddr("10.64.0.6"), Version: "9.9.9"})
		holdLock(t, s)
		calls := len(f.Calls())
		for _, more := range [][]string{nil, {"--yes", "--lock-timeout", "0"}} {
			wantResult(t, runWithNomad(t, onVultr(f), nomad, roll(s, more...)...), 1,
				upToDateServersLine+outdatedWorkersLine,
				"Error: tent never moves a node to an older Nomad: the cluster is pinned to "+assetstest.NomadVersion+
					", and node prod-workers-0 runs 9.9.9\n")
		}
		wantNoWritesIn(t, f.Calls()[calls:])
	})
}

// TestRollingUpdateClusterAllowsASingleServer takes --allow-single-server as update does: without it the specs of a
// cluster of one server are invalid, with it the command goes on to its next check.
func TestRollingUpdateClusterAllowsASingleServer(t *testing.T) {
	s := newState(t)
	s.put(t, clusterPath, clusterYAML)
	s.put(t, serversPath, replaced(t, serversYAML, "size: 3", "size: 1"))
	tent := rollRunner(t, vultrfake.New())

	wantError(t, tent(roll(s)...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: size 1 needs --allow-single-server\n")
	wantError(t, tent(roll(s, "--allow-single-server")...),
		"Error: cluster prod has no Nomad yet; run tent update cluster first\n")
}

// TestRollingUpdateClusterFailsBeforeItPlans prints only the error for a check that fails, as update does.
func TestRollingUpdateClusterFailsBeforeItPlans(t *testing.T) {
	s := withCluster(t)
	tent := rollRunner(t, vultrfake.New())

	wantError(t, tent(roll(s)...),
		"Error: cluster prod has no Nomad yet; run tent update cluster first\n")
	wantError(t, tent(roll(s, "--yes")...),
		"Error: cluster prod has no Nomad yet; run tent update cluster first\n")
}

// TestRollingUpdateClusterWaitsForTheLock prints update's notice and waits, as every change does.
func TestRollingUpdateClusterWaitsForTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)
		held := holdLock(t, s)

		got := tent(roll(s, "--yes", "--lock-timeout", "200ms")...)

		locked := "cluster prod is locked by " + held.Lease().String()
		const waiting = "; waiting up to 200ms (--lock-timeout)\n"
		if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, locked+waiting) ||
			!strings.Contains(got.errOut, "\nError: "+locked) {
			t.Errorf("exit code = %d, stdout\n%s\nstderr\n%s\nwant 1, no output, the notice and the error",
				got.code, got.out, got.errOut)
		}
	})
}

// TestRollingUpdateClusterStopsOnCtrlC ends a roll that waits for a drain on the first Ctrl-C: it prints the plan,
// the steps so far, the wait that failed and the error interrupted, exits with 1, deletes no machine and releases the
// lock.
func TestRollingUpdateClusterStopsOnCtrlC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		world, nomad := followedNomad(f)
		world.SetDrainReads(1 << 30)
		sigs, ex := make(chan os.Signal), make(exits, 1)
		r := startWithSignalsAndOptions(t, sigs, ex, strings.NewReader(""), roll(s, "--yes"),
			WithProviders(onVultr(f)), WithAssets(testAssets()), WithNomad(nomad))
		const draining = "waiting for node prod-workers-0 to drain"
		for range 1000 {
			if strings.Contains(r.errOut.String(), draining) {
				break
			}
			time.Sleep(time.Second)
			synctest.Wait()
		}
		sigs <- syscall.SIGINT

		got := r.wait()

		steps := strings.Join(append(rolledLines()[:10], draining,
			"failed to wait for node prod-workers-0 to drain: context canceled"), "\n")
		wantResult(t, got, 1, rollPlan, openAPIWarning+steps+"\nError: interrupted\n")
		ex.wantNoExit(t)
		wantInstances(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0", "prod-workers-1",
			"prod-workers-2", "prod-workers-3")
		holdLock(t, s)
	})
}

// lastPurgeLosesTheLock is a nomadops.API that calls lose once the clients of its Nomad made last purges together.
type lastPurgeLosesTheLock struct {
	nomadops.API
	purges *atomic.Int32
	last   int32
	lose   func()
}

func (a lastPurgeLosesTheLock) Purge(ctx context.Context, nodeID string) error {
	err := a.API.Purge(ctx, nodeID)
	if err == nil && a.purges.Add(1) == a.last {
		a.lose()
	}
	return err
}

// TestRollingUpdateClusterLosesItsLock prints what the roll did, with -o table and with -o json, and fails with the
// error that says the roll is saved, when the lock is lost after the last step.
func TestRollingUpdateClusterLosesItsLock(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := outdatedCluster(t)
				_, nomad := followedNomad(f)
				var purges atomic.Int32
				losing := func(cfg nomadops.Config) (nomadops.API, error) {
					api, err := nomad(cfg)
					return lastPurgeLosesTheLock{API: api, purges: &purges, last: 3, lose: func() {
						if err := s.open(t).Delete(t.Context(), "prod/lock"); err != nil {
							t.Errorf("remove the lock: %v", err)
						}
					}}, err
				}

				opts := &globalOptions{
					openStore: wrapped(func(st statestore.Store) statestore.Store { return leaseStore{st} }),
					providers: onVultr(f), assets: testAssets(), nomad: losing,
				}
				got := runWith(t, opts, roll(s, "--yes", "-o", format)...)

				if got.code != 1 || !strings.HasSuffix(got.errOut, "\n"+lockLost+"\n") {
					t.Errorf("exit code = %d, stderr\n%s\nwant 1 and the error\n%s", got.code, got.errOut, lockLost)
				}
				if format == "table" && got.out != rollPlan+rolledSummary {
					t.Errorf("stdout\n%s\nwant the plan and what the roll did\n%s", got.out, rollPlan+rolledSummary)
				}
				if format == "json" && !strings.Contains(got.out, `"applied": true`) {
					t.Errorf("stdout\n%s\nwant the applied plan", got.out)
				}
			})
		})
	}
}

// TestRollingUpdateClusterHelp tells what the command does and which flags it takes.
func TestRollingUpdateClusterHelp(t *testing.T) {
	got := runOnCloud(t, "rolling-update", "cluster", "--help")
	for _, want := range []string{
		"Usage:\n  tent rolling-update cluster [NAME] [flags]\n",
		"--yes", "--nodegroups", "--force", "--exit-code", "--allow-single-server", "VULTR_API_KEY",
		"Replace the outdated nodes of the cluster named by NAME or --name.",
		"tent cannot roll server groups yet",
	} {
		if got.code != 0 || !strings.Contains(got.out, want) {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and it to hold %q", got.code, got.out, want)
		}
	}
}

// TestRollingUpdateClusterExitCode exits with 2 under --exit-code while a roll is due, and with 0 when nothing is left
// to roll; the plan prints as without the flag.
func TestRollingUpdateClusterExitCode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)
		tent := rollRunner(t, f)

		wantResult(t, tent(roll(s, "--exit-code")...), 2, rollPlan, rollHint)

		clean, g := builtCluster(t)
		const plan = upToDateServersLine + "node group workers (client, size 3): up to date\n\nNothing to roll.\n"
		wantResult(t, rollRunner(t, g)(roll(clean, "--exit-code")...), 0, plan, "")
	})
}

// TestRollingUpdateClusterExitCodeKeepsTheRefusalAnError exits with 1, not 2, when the decisions refuse the roll.
func TestRollingUpdateClusterExitCodeKeepsTheRefusalAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		tent := rollRunner(t, f)
		s.put(t, clusterPath, slowServersYAML)
		if got := runOn(t, f, update(s, "--yes")...); got.code != 0 {
			t.Fatalf("update --yes: exit code %d\n%s", got.code, got.errOut)
		}

		got := tent(roll(s, "--exit-code")...)

		if got.code != 1 || !strings.HasSuffix(got.errOut, "Error: "+serverRefusal+"\n") {
			t.Errorf("exit code = %d, stderr\n%s\nwant 1 and the refusal", got.code, got.errOut)
		}
	})
}

// TestRollingUpdateClusterExitCodeNeedsAPlan refuses --exit-code with --yes before it calls the cloud, as update does.
func TestRollingUpdateClusterExitCodeNeedsAPlan(t *testing.T) {
	s := withCluster(t)
	f := vultrfake.New()

	wantError(t, rollRunner(t, f)(roll(s, "--yes", "--exit-code")...), "Error: --exit-code works only without --yes\n")

	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("calls to the cloud: %v", calls)
	}
}
