package rollout_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/rollout"
)

// downVoters returns the names of the machines whose servers vote and that are stopped.
func (w *world) downVoters() []string {
	var names []string
	for _, srv := range w.servers {
		if i := w.machineIndex(srv.machine); srv.voter && !w.up(srv.machine) {
			names = append(names, w.machines[i].Name)
		}
	}
	return names
}

// A step that stops a server or hands its leadership over never comes while another voter is down, whichever
// voter failed at whichever point of a roll, and whether or not autopilot has noticed it yet.
func TestNoStopOrTransferWhileAnotherVoterIsDown(t *testing.T) {
	failures := []struct {
		name string
		fail func(w *world, machine string)
	}{
		{"the machine stopped and autopilot has not noticed", (*world).stopMachine},
		{"autopilot has noticed", (*world).failServer},
	}
	for _, sc := range []scenario{
		{"combined3", rollout.Roll, func() *world { return outdatedCombined(3) }},
		{"servers3", rollout.Roll, func() *world { return outdatedServers(3) }},
		{"server1", rollout.Roll, func() *world { return outdatedServers(1) }},
	} {
		res, err := sc.build().run(sc.mode, rollout.Next, true)
		if err != nil {
			t.Fatalf("%s: %v", sc.name, err)
		}
		for _, failure := range failures {
			for i, snap := range res.snapshots {
				for _, m := range snap.world.machines {
					w := snap.world.clone()
					if w.serverIndexByMachine(m.ID) < 0 || !w.up(m.ID) {
						continue
					}
					failure.fail(w, m.Name)
					step, err := rollout.Next(w.observe(), sc.mode)
					if err != nil {
						continue
					}
					if step.Action != rollout.Stop && step.Action != rollout.TransferLeadership {
						continue
					}
					if down := w.downVoters(); len(down) > 0 && !slices.Equal(down, []string{step.Machine.Name}) {
						t.Errorf("%s, decision %d, %s on %s: %q while %s is down", sc.name, i, failure.name, m.Name, step,
							strings.Join(down, " and "))
					}
				}
			}
		}
	}
}

// failsAVoterDuringADrain returns a decider that stops a running voter, neither the leader nor the machine that
// drains, as soon as a drain is under way, and records the machine's name in failed.
func failsAVoterDuringADrain(w *world, failed *string) decider {
	return func(s rollout.State, mode rollout.Mode) (rollout.Step, error) {
		if i := slices.IndexFunc(w.nodes, func(n simNode) bool { return n.Draining }); i >= 0 && *failed == "" {
			for _, srv := range w.servers {
				m := w.machines[w.machineIndex(srv.machine)]
				if srv.voter && !srv.leader && m.ID != w.nodes[i].owner && w.up(m.ID) {
					w.stopMachine(m.Name)
					*failed = m.Name
					break
				}
			}
		}
		return rollout.Next(s, mode)
	}
}

func TestACombinedRollRefusesWhenAVoterFailsDuringTheDrain(t *testing.T) {
	w := outdatedCombined(3)
	var failed string
	res, err := w.run(rollout.Roll, failsAVoterDuringADrain(w, &failed), false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	last := res.lines[len(res.lines)-1]
	if want := "refused: node group control: node " + failed + " is not running"; failed == "" || !res.refused ||
		!strings.HasPrefix(last, want) {
		t.Errorf("the run ended with %q (refused %t), want the refusal %q... after %q failed", last, res.refused, want,
			failed)
	}
}
