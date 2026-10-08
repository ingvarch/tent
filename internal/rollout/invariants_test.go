package rollout_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/rollout"
)

// stepsThen returns a decision function that gives the steps in turn, then done.
func stepsThen(steps ...rollout.Step) decider {
	return func(rollout.State, rollout.Mode) (rollout.Step, error) {
		if len(steps) == 0 {
			return rollout.Step{Action: rollout.Done}, nil
		}
		step := steps[0]
		steps = steps[1:]
		return step, nil
	}
}

// machineStep names the machine and the node of a worker of the world for a step.
func (w *world) machineStep(action rollout.Action, name string) rollout.Step {
	m := w.machines[w.machineIndexByName(name)]
	step := rollout.Step{Action: action, Group: m.Group, Machine: m.Machine}
	if i := w.nodeIndexByOwner(m.ID); i >= 0 {
		step.Node = w.nodes[i].Node
	}
	return step
}

// serverStep names the machine, the server and the member of a server of the world for a step. A step that needs the
// server of a machine without one gets an empty server.
func (w *world) serverStep(action rollout.Action, name string) rollout.Step {
	m := w.machines[w.machineIndexByName(name)]
	member := rollout.Member{Name: name + ".global"}
	step := rollout.Step{Action: action, Group: m.Group, Machine: m.Machine, Member: member}
	if i := w.serverIndexByMachine(m.ID); i >= 0 {
		step.Server = rollout.Server{ID: w.servers[i].id, Name: name + ".global"}
	}
	return step
}

func TestRunBreaksOnInvariants(t *testing.T) {
	tests := []struct {
		name  string
		world func() *world
		steps func(w *world) []rollout.Step
		want  string
	}{
		{"the leader's machine is deleted", func() *world { return outdatedWorkers(1, 0).arm() },
			func(w *world) []rollout.Step { return []rollout.Step{w.machineStep(rollout.Delete, "prod-servers-0")} },
			"it is the machine of the leader"},
		{"a node that holds allocations is deleted", func() *world { return outdatedWorkers(1, 0).arm() },
			func(w *world) []rollout.Step { return []rollout.Step{w.machineStep(rollout.Delete, "prod-workers-0")} },
			"its node is up and holds 2 allocations"},
		{"more nodes go than maxUnavailable allows", func() *world { return outdatedWorkers(0, 1).arm() },
			func(w *world) []rollout.Step {
				return []rollout.Step{
					w.machineStep(rollout.MarkIneligible, "prod-workers-0"),
					w.machineStep(rollout.MarkIneligible, "prod-workers-1"),
				}
			},
			"group workers has 1 available nodes, fewer than its size 3 less 1 unavailable"},
		{"more machines exist than size plus surge", func() *world { return outdatedWorkers(1, 0).arm() },
			func(*world) []rollout.Step {
				create := rollout.Step{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
					Name: "prod-workers-3", Zone: "ams"}}
				second := create
				second.Machine.Name = "prod-workers-4"
				return []rollout.Step{create, second}
			},
			"group workers has 5 machines, more than the 4 it may have"},
		{"a node is created while a server runs an older Nomad", func() *world {
			w := outdatedWorkers(1, 0)
			w.version = "2.0.8"
			return w.arm()
		}, func(*world) []rollout.Step {
			return []rollout.Step{{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
				Name: "prod-workers-3", Zone: "ams"}}}
		}, "a server runs Nomad 2.0.7, older than the 2.0.8 of the new node"},
		{"the leader's machine is stopped", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step { return []rollout.Step{w.serverStep(rollout.Stop, "prod-servers-0")} },
			"it is the machine of the leader"},
		{"the leader's peer is removed", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step {
				return []rollout.Step{w.serverStep(rollout.RemovePeer, "prod-servers-0")}
			},
			"it is the leader"},
		{"a second server is stopped and the quorum is lost", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step {
				return []rollout.Step{
					w.serverStep(rollout.Stop, "prod-servers-1"), w.serverStep(rollout.Stop, "prod-servers-2"),
				}
			},
			"no quorum: 1 of 3 voters run"},
		{"a server is stopped right after another joined", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step {
				create := rollout.Step{Action: rollout.Create, Group: "servers", Machine: rollout.Machine{
					Name: "prod-servers-3", Zone: "ams"}}
				wait := rollout.Step{Action: rollout.WaitJoined}
				return []rollout.Step{create, wait, wait, w.serverStep(rollout.Stop, "prod-servers-1")}
			},
			"a server joined the Raft configuration 0s ago, less than the refresh interval of 1m0s"},
		{"a peer is removed right after another joined", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step {
				create := rollout.Step{Action: rollout.Create, Group: "servers", Machine: rollout.Machine{
					Name: "prod-servers-3", Zone: "ams"}}
				wait := rollout.Step{Action: rollout.WaitJoined}
				return []rollout.Step{create, wait, wait, w.serverStep(rollout.RemovePeer, "prod-servers-1")}
			},
			"a server joined the Raft configuration 0s ago, less than the refresh interval of 1m0s"},
		{"a machine is deleted while its server is in the Raft configuration", func() *world { return serverWorld(3) },
			func(w *world) []rollout.Step { return []rollout.Step{w.serverStep(rollout.Delete, "prod-servers-1")} },
			"its server is still in the Raft configuration"},
		{"a server group has more machines than size plus one", func() *world { return serverWorld(3) },
			func(*world) []rollout.Step {
				create := rollout.Step{Action: rollout.Create, Group: "servers", Machine: rollout.Machine{
					Name: "prod-servers-3", Zone: "ams"}}
				second := create
				second.Machine.Name = "prod-servers-4"
				return []rollout.Step{create, second}
			},
			"group servers has 5 machines, more than the 4 it may have"},
		{"a combined group has more machines than size plus one", func() *world { return combinedWorld(3) },
			func(*world) []rollout.Step {
				create := rollout.Step{Action: rollout.Create, Group: "control", Machine: rollout.Machine{
					Name: "prod-control-3", Zone: "ams"}}
				second := create
				second.Machine.Name = "prod-control-4"
				return []rollout.Step{create, second}
			},
			"group control has 5 machines, more than the 4 it may have"},
		{"a client step comes before the server group is done", func() *world {
			w := outdatedServers(3)
			w.addGroup(workersGroup(1, 0))
			w.addClients("workers", 3, oldHash, oldVersion)
			return w.arm()
		}, func(w *world) []rollout.Step {
			return []rollout.Step{w.machineStep(rollout.MarkIneligible, "prod-workers-0")}
		}, "group servers is not done"},
		{"two machines get one name", func() *world { return outdatedWorkers(1, 0).arm() },
			func(*world) []rollout.Step {
				return []rollout.Step{{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
					Name: "prod-workers-1", Zone: "ams"}}}
			},
			"two machines are called prod-workers-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.world()
			_, err := w.run(rollout.Roll, stepsThen(tt.steps(w)...), false)
			var v *violation
			if !errors.As(err, &v) || !strings.Contains(v.Error(), tt.want) {
				t.Errorf("run error = %v, want a violation containing %q", err, tt.want)
			}
		})
	}
}

func TestRunBreaksOnShrinkInvariants(t *testing.T) {
	tests := []struct {
		name  string
		world func() *world
		steps func(w *world) []rollout.Step
		want  string
	}{
		{"a machine is created", func() *world { return shrinkWorkers(4) },
			func(*world) []rollout.Step {
				return []rollout.Step{{Action: rollout.Create, Group: "workers", Machine: rollout.Machine{
					Name: "prod-workers-4", Zone: "ams"}}}
			},
			"a shrink creates no machine"},
		{"a machine is deleted from a group at its size", func() *world { return shrinkWorkers(2) },
			func(w *world) []rollout.Step { return []rollout.Step{w.machineStep(rollout.Delete, "prod-workers-1")} },
			"group workers has 2 machines, no more than its size 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tt.world()
			_, err := w.run(rollout.Shrink, stepsThen(tt.steps(w)...), false)
			var v *violation
			if !errors.As(err, &v) || !strings.Contains(v.Error(), tt.want) {
				t.Errorf("run error = %v, want a violation containing %q", err, tt.want)
			}
		})
	}
}

func TestLimitOfAGroupIsWhatItStartedWithWhenThatIsMore(t *testing.T) {
	w := shrinkWorkers(4)
	if err := w.check(); err != nil {
		t.Fatalf("check of a group that starts above its size: %v", err)
	}
	w.addClients("workers", 1, newHash, curVersion)
	var v *violation
	if err := w.check(); !errors.As(err, &v) || v.Error() != "group workers has 5 machines, more than the 4 it may have" {
		t.Errorf("check = %v, want the violation for a fifth machine", err)
	}
}

func TestCheckFindsAWorldWithoutALeader(t *testing.T) {
	w := outdatedWorkers(1, 0).arm()
	if err := w.check(); err != nil {
		t.Fatalf("check of a healthy world: %v", err)
	}
	w.servers[0].leader = false
	var v *violation
	if err := w.check(); !errors.As(err, &v) || v.Error() != "no leader" {
		t.Errorf("check = %v, want the violation %q", err, "no leader")
	}
	w.servers[0].leader = true
	w.stopMachine("prod-servers-0")
	if err := w.check(); !errors.As(err, &v) || v.Error() != "no leader" {
		t.Errorf("check with a stopped leader = %v, want the violation %q", err, "no leader")
	}
}

func TestBudgetInvariantHoldsOnlyForGroupsThatStartAvailable(t *testing.T) {
	w := outdatedWorkers(0, 0)
	w.makeIneligible("prod-workers-0")
	w.arm()
	if err := w.check(); err != nil {
		t.Errorf("check = %v, want no violation for a group that starts short", err)
	}
}

func TestRunStopsOnOtherErrors(t *testing.T) {
	tests := []struct {
		name   string
		decide func(w *world) decider
		want   string
	}{
		{"a decision that fails", func(*world) decider {
			return func(rollout.State, rollout.Mode) (rollout.Step, error) { return rollout.Step{}, errors.New("broken") }
		}, "broken"},
		{"a wait that never ends", func(*world) decider {
			return func(rollout.State, rollout.Mode) (rollout.Step, error) {
				return rollout.Step{Action: rollout.WaitJoined}, nil
			}
		}, "the run did not end in 1000 steps"},
		{"a step on a node that does not exist", func(*world) decider {
			return stepsThen(rollout.Step{Action: rollout.MarkIneligible, Node: rollout.Node{ID: "n-99"}})
		}, "no such node"},
		{"a step that the world does not apply", func(*world) decider {
			return stepsThen(rollout.Step{Action: rollout.WaitJoined, Group: "workers"}, rollout.Step{})
		}, "unknown action 0: the world does not apply this step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := outdatedWorkers(1, 0).arm()
			_, err := w.run(rollout.Roll, tt.decide(w), false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestResumeCheckFindsADecisionThatRemembers(t *testing.T) {
	sc := scenarios[0]
	problem, err := resumeProblem(sc, func() decider {
		calls := 0
		return func(s rollout.State, m rollout.Mode) (rollout.Step, error) {
			if calls++; calls > 4 {
				return rollout.Step{Action: rollout.Done}, nil
			}
			return rollout.Next(s, m)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(problem, "prints other lines") {
		t.Errorf("problem = %q, want a difference in the lines", problem)
	}
}

func TestShrinkLeavesNoDrainedNodeBehind(t *testing.T) {
	w := shrinkWorkers(4)
	for _, name := range []string{"prod-workers-0", "prod-workers-1", "prod-workers-2"} {
		w.makeIneligible(name)
	}
	w.arm()
	if _, err := w.run(rollout.Shrink, rollout.Next, false); err != nil {
		t.Fatal(err)
	}
	if got := w.countOf("workers"); got != 2 {
		t.Errorf("the group has %d machines, want 2", got)
	}
	for _, n := range w.observe().Nomad.Nodes {
		if n.Draining || n.DrainedFor != "" {
			t.Errorf("node %s is left draining or drained (draining %v, drained for %q)", n.Name, n.Draining,
				n.DrainedFor)
		}
	}
}
