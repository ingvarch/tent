package app_test

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
)

// wantCombinedNodesReady fails the test unless Nomad lists a node for each combined machine that f lists, with the ID
// n-<instance ID>, the machine's name, ready and eligible, and lists no other node whose name starts with the group's.
func wantCombinedNodesReady(t *testing.T, svc *app.Service, f *vultrfake.Fake) {
	t.Helper()
	machines := serverMachines(f, combinedGroup)
	var want, got []string
	for _, m := range machines {
		want = append(want, nodeIDOf(m.id)+" "+m.name+" ready eligible")
	}
	for _, n := range nomadNodes(t, svc) {
		if !strings.HasPrefix(n.Name, machinePrefix(combinedGroup)) {
			continue
		}
		state := n.Status + " ineligible"
		if n.Eligible {
			state = n.Status + " eligible"
		}
		got = append(got, n.ID+" "+n.Name+" "+state)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("Nomad lists the combined nodes %q, want %q", got, want)
	}
}

// TestRollCombinedFlowReplacesTheNodes applies the roll of the three outdated combined nodes, with the invariants of
// a combined group watching: the plan that it returns and the calls to Vultr and Nomad match golden files, and the
// cluster ends as wantRollEnded, wantServersRolled and wantCombinedNodesReady say.
func TestRollCombinedFlowReplacesTheNodes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, inv := combinedFlowWorld(t)
		old := serverMachines(f, combinedGroup)
		var plan app.RollPlan
		var err error

		calls := flowCalls(t, svc, f, w, func() *engine.Plan {
			plan, err = applyRoll(svc, app.RollOptions{})
			return nil
		})

		if err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}
		if want := (app.RollCounts{Created: 3, Drained: 3, Stopped: 3, Deleted: 3, Purged: 3}); !plan.Applied ||
			plan.Rolled != want {
			t.Errorf("the plan says applied %v and rolled %+v, want applied and %+v", plan.Applied, plan.Rolled, want)
		}
		var text strings.Builder
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		checkGolden(t, "flow_roll_combined.plan.golden", text.String())
		checkGolden(t, "flow_roll_combined.calls.golden", callsText(collapsePolls(calls)))
		wantRollEnded(t, svc, f, idsOf(old))
		wantServersRolled(t, svc, f, combinedGroup, old, true)
		wantCombinedNodesReady(t, svc, f)
		inv.wantKept()
	})
}

// TestRollCombinedFlowReportsEachStepOfTheRoll applies the same roll and matches its progress lines to a golden file.
func TestRollCombinedFlowReportsEachStepOfTheRoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := combinedFlowWorld(t)
		lines := recordProgress(svc)

		if _, err := applyRoll(svc, app.RollOptions{}); err != nil {
			t.Fatalf("RollingUpdate: %v", err)
		}

		checkGolden(t, "flow_roll_combined.steps.golden", strings.Join(*lines, "\n")+"\n")
	})
}
