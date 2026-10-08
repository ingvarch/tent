package rollout_test

import (
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

func TestStepText(t *testing.T) {
	machine := rollout.Machine{ID: "m-1", Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams"}
	node := rollout.Node{ID: "n-1", Name: "prod-workers-0", Address: ip(3)}
	tests := []struct {
		step rollout.Step
		want string
	}{
		{rollout.Step{Action: rollout.Done}, "done"},
		{rollout.Step{Action: rollout.Create, Machine: machine}, "create node prod-workers-0 (client of workers, ams)"},
		{rollout.Step{Action: rollout.WaitJoined, Machine: machine}, "wait until node prod-workers-0 joins"},
		{rollout.Step{Action: rollout.MarkIneligible, Machine: machine, Node: node}, "mark node prod-workers-0 ineligible"},
		{rollout.Step{Action: rollout.Drain, Machine: machine, Node: node, Deadline: time.Hour},
			"drain node prod-workers-0 within 1h0m0s"},
		{rollout.Step{Action: rollout.WaitDrained, Machine: machine, Node: node},
			"wait until node prod-workers-0 is drained"},
		{rollout.Step{Action: rollout.Delete, Machine: machine}, "delete node prod-workers-0 (ID m-1)"},
		{rollout.Step{Action: rollout.WaitNodeDown, Node: node},
			"wait until Nomad lists node prod-workers-0 at 10.64.0.3 as down"},
		{rollout.Step{Action: rollout.Purge, Node: node}, "purge node prod-workers-0 at 10.64.0.3 from Nomad"},
		{rollout.Step{}, "unknown action 0"},
	}
	for _, tt := range tests {
		if got := tt.step.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestActionWaits(t *testing.T) {
	waits := map[rollout.Action]bool{
		rollout.Done: false, rollout.Create: false, rollout.WaitJoined: true, rollout.MarkIneligible: false,
		rollout.Drain: false, rollout.WaitDrained: true, rollout.Delete: false, rollout.WaitNodeDown: true,
		rollout.Purge: false,
	}
	for action, want := range waits {
		if got := action.Waits(); got != want {
			t.Errorf("%d.Waits() = %v, want %v", action, got, want)
		}
	}
}
