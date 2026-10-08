package app_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// workerReplaceLine is the line of the plan that names both workers of the test cluster as outdated.
const workerReplaceLine = "Outdated: prod-workers-0 and prod-workers-1; tent rolling-update cluster replaces them.\n"

// allOutdatedLine is the line of the plan that names every machine of the built test cluster as outdated.
const allOutdatedLine = "Outdated: prod-servers-0, prod-servers-1, prod-servers-2, prod-workers-0 and " +
	"prod-workers-1; tent rolling-update cluster replaces them.\n"

// couldNotTell is the start of the warning of a report that the release files make impossible.
const couldNotTell = "tent could not tell which nodes are outdated: "

// outdatedWorkers returns the outdated machines that outdatedWorld has, named by the fake's instances.
func outdatedWorkers(t *testing.T, f *vultrfake.Fake) []app.OutdatedNode {
	t.Helper()
	var out []app.OutdatedNode
	for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
		out = append(out, app.OutdatedNode{
			Name: name, ID: instanceNamed(t, f, name), Group: "workers", Reason: "spec hash",
		})
	}
	return out
}

// told records the warnings that svc tells.
func told(svc *app.Service) *[]string {
	var warnings []string
	svc.OnWarning = func(w string) { warnings = append(warnings, w) }
	return &warnings
}

// withCount returns how many of the warnings start with prefix.
func withCount(warnings []string, prefix string) int {
	return len(slices.DeleteFunc(slices.Clone(warnings), func(w string) bool { return !strings.HasPrefix(w, prefix) }))
}

// cancelingSites is a release site that ends the context of its run when it is asked, and answers no request.
type cancelingSites struct{ cancel context.CancelFunc }

func (s cancelingSites) RoundTrip(req *http.Request) (*http.Response, error) {
	s.cancel()
	return nil, req.Context().Err()
}

// TestUpdatePlanListsTheOutdatedMachinesWithoutChangingThem plans an update of workers that carry the old hash: the
// plan names them with the reason, says so in its text and its JSON, has no change, and only reads.
func TestUpdatePlanListsTheOutdatedMachinesWithoutChangingThem(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := outdatedWorld(t)
		u := watch(t, svc, f, w)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if diff := cmp.Diff(outdatedWorkers(t, f), plan.Outdated); diff != "" {
			t.Errorf("Outdated (-want +got):\n%s", diff)
		}
		if plan.HasChanges() {
			t.Errorf("the plan has changes:\n%s", planText(t, plan))
		}
		if got, want := planText(t, plan), "No changes.\n"+workerReplaceLine; got != want {
			t.Errorf("the plan reads %q, want %q", got, want)
		}
		if got := encodeJSON(t, plan, ""); !strings.Contains(got, `"outdated":[{"name":"prod-workers-0","id":"`) {
			t.Errorf("the JSON of the plan lacks the outdated machines: %s", got)
		}
		u.check(t)
	})
}

// TestUpdateAppliedWithOnlyOutdatedMachinesReplacesNothing applies the update of a cluster whose workers are outdated:
// it is applied at once, keeps the report and creates and deletes no machine.
func TestUpdateAppliedWithOnlyOutdatedMachinesReplacesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		before := slices.Clone(f.Instances())

		plan := mustUpdate(t, svc)

		if !plan.Applied || len(plan.Outdated) != 2 {
			t.Errorf("the applied plan is Applied=%t with %d outdated machines, want true and 2", plan.Applied,
				len(plan.Outdated))
		}
		if diff := cmp.Diff(before, f.Instances()); diff != "" {
			t.Errorf("the instances changed (-before +after):\n%s", diff)
		}
	})
}

// TestUpdatePlanOfAnUpToDateClusterHasNoOutdatedMachines shows no line and an empty list when every machine has its
// group's hash.
func TestUpdatePlanOfAnUpToDateClusterHasNoOutdatedMachines(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := rollWorld(t)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if len(plan.Outdated) != 0 {
			t.Errorf("Outdated = %+v, want none", plan.Outdated)
		}
		if got := planText(t, plan); got != "No changes.\n" {
			t.Errorf("the plan reads %q, want %q", got, "No changes.\n")
		}
		if got := encodeJSON(t, plan, ""); !strings.Contains(got, `"outdated":[]`) {
			t.Errorf("the JSON of the plan is %s, want an empty list of outdated machines", got)
		}
	})
}

// TestUpdatePlanReportsAMachineWithoutAHash names a machine that carries no spec hash with that reason.
func TestUpdatePlanReportsAMachineWithoutAHash(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		id := instanceNamed(t, f, "prod-workers-1")
		for _, in := range f.Instances() {
			if in.ID == id {
				f.SetInstanceTags(t, id, slices.DeleteFunc(slices.Clone(in.Tags), func(tag string) bool {
					return strings.HasPrefix(tag, cloud.LabelSpecHash+"=")
				})...)
			}
		}

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		want := []app.OutdatedNode{{Name: "prod-workers-1", ID: id, Group: "workers", Reason: "no spec hash"}}
		if diff := cmp.Diff(want, plan.Outdated); diff != "" {
			t.Errorf("Outdated (-want +got):\n%s", diff)
		}
	})
}

// TestUpdatePlanKeepsTheMachineItWaitsForAmongTheOutdated plans the wait for a client that has not joined yet and
// carries the old hash: the machine stays and is reported with the one that joined.
func TestUpdatePlanKeepsTheMachineItWaitsForAmongTheOutdated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := neverRegistered(t)
		mustReplace(t, svc, workersMetaYAML)
		sleepUntilAge(t, f, "instance-5", youngClient)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, waitForWorker1)
		want := []app.OutdatedNode{
			{Name: "prod-workers-0", ID: "instance-4", Group: "workers", Reason: "spec hash"},
			{Name: "prod-workers-1", ID: "instance-5", Group: "workers", Reason: "spec hash"},
		}
		if diff := cmp.Diff(want, plan.Outdated); diff != "" {
			t.Errorf("Outdated (-want +got):\n%s", diff)
		}
	})
}

// TestUpdatePlanLeavesOutTheMachinesThatItDeletes plans the replacement of a client that never registered and that
// carries the old hash: only the machine that stays is reported.
func TestUpdatePlanLeavesOutTheMachinesThatItDeletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := neverRegistered(t)
		mustReplace(t, svc, workersMetaYAML)
		sleepUntilAge(t, f, "instance-5", oldClient)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantNodeChanges(t, plan, replaceWorker1()...)
		want := []app.OutdatedNode{{Name: "prod-workers-0", ID: "instance-4", Group: "workers", Reason: "spec hash"}}
		if diff := cmp.Diff(want, plan.Outdated); diff != "" {
			t.Errorf("Outdated (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateGoesOnWhenTheReleaseFilesCannotBeRead plans and applies an update that creates no node while the release
// files cannot be read: it warns once that it cannot tell which nodes are outdated, reports none and goes on.
func TestUpdateGoesOnWhenTheReleaseFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := outdatedWorld(t)
		mustReplace(t, svc, workersMetaYAML+"  rollingUpdate:\n    maxSurge: 2\n")
		svc.Assets.Client = &http.Client{Transport: brokenSites{}}
		warnings := told(svc)

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if len(plan.Outdated) != 0 || !plan.HasChanges() {
			t.Errorf("the plan reports %d outdated machines and has changes %t, want none and true",
				len(plan.Outdated), plan.HasChanges())
		}
		if n := withCount(*warnings, couldNotTell); n != 1 || !strings.Contains(strings.Join(*warnings, "\n"),
			errSitesDown.Error()) {
			t.Errorf("warnings = %q, want one that starts with %q and names %v", *warnings, couldNotTell, errSitesDown)
		}

		*warnings = nil
		applied, err := svc.Update(t.Context(), "prod", true)

		if err != nil || !applied.Applied || !applied.Completed {
			t.Errorf("Update = Applied %t, Completed %t, error %v, want the completed spec written", applied.Applied,
				applied.Completed, err)
		}
		if n := withCount(*warnings, couldNotTell); n != 1 {
			t.Errorf("the apply told %d warnings that start with %q, want 1: %q", n, couldNotTell, *warnings)
		}
		if got := len(f.Instances()); got != 5 {
			t.Errorf("the fake has %d instances, want the 5 it had", got)
		}
	})
}

// TestUpdateStopsWhenInterruptedWhileItReadsTheReleaseFiles returns the interruption, not a warning.
func TestUpdateStopsWhenInterruptedWhileItReadsTheReleaseFiles(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := rollWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		svc.Assets.Client = &http.Client{Transport: cancelingSites{cancel}}
		warnings := told(svc)

		_, err := svc.Update(ctx, "prod", false)

		if err == nil || err.Error() != "interrupted" {
			t.Errorf("Update error = %v, want an interruption", err)
		}
		if len(*warnings) != 0 {
			t.Errorf("warnings = %q, want none", *warnings)
		}
	})
}

// TestUpdateStillFailsWhenANodeNeedsTheReleaseFiles fails an update that creates a node while the release files cannot
// be read, as it did before the report.
func TestUpdateStillFailsWhenANodeNeedsTheReleaseFiles(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		mustReplace(t, svc, edit(t, workersMetaYAML, "size: 2", "size: 3"))
		svc.Assets.Client = &http.Client{Transport: brokenSites{}}
		warnings := told(svc)
		before := len(f.Instances())

		_, err := svc.Update(t.Context(), "prod", true)

		if err == nil || !strings.Contains(err.Error(), errSitesDown.Error()) {
			t.Errorf("Update error = %v, want one that names %v", err, errSitesDown)
		}
		if n := withCount(*warnings, couldNotTell); n != 0 || len(f.Instances()) != before {
			t.Errorf("warned %d times and have %d instances, want none and %d", n, len(f.Instances()), before)
		}
	})
}

// TestValidateWarnsAboutTheOutdatedNodes validates a cluster whose workers are outdated: it stays valid and warns, by
// the count, and tells the warning too.
func TestValidateWarnsAboutTheOutdatedNodes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		warnings := told(svc)

		v := mustValidate(t, svc)

		want := "2 nodes are outdated: prod-workers-0 and prod-workers-1; tent rolling-update cluster replaces them"
		if !v.Valid() || !slices.Contains(v.Warnings, want) || !slices.Contains(*warnings, want) {
			t.Errorf("valid %t, warnings %q, told %q, want a valid cluster and the warning %q", v.Valid(), v.Warnings,
				*warnings, want)
		}
	})
}

// TestUpdatePlanListsAMachineWithTheReplaceLabel plans an update of up-to-date workers, one of which carries the
// replace label: the plan names it as forced.
func TestUpdatePlanListsAMachineWithTheReplaceLabel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		labelReplace(t, f, "prod-workers-0")

		plan, err := svc.Update(t.Context(), "prod", false)

		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		want := []app.OutdatedNode{
			{Name: "prod-workers-0", ID: instanceNamed(t, f, "prod-workers-0"), Group: "workers", Reason: "forced"},
		}
		if diff := cmp.Diff(want, plan.Outdated); diff != "" {
			t.Errorf("Outdated (-want +got):\n%s", diff)
		}
	})
}

// TestValidateWarnsAboutAMachineWithTheReplaceLabel validates a cluster of up-to-date workers, one of which carries
// the replace label: it warns that a rolling update replaces it.
func TestValidateWarnsAboutAMachineWithTheReplaceLabel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		labelReplace(t, f, "prod-workers-0")

		v := mustValidate(t, svc)

		want := "1 node is outdated: prod-workers-0; tent rolling-update cluster replaces it"
		if !slices.Contains(v.Warnings, want) {
			t.Errorf("warnings = %q, want %q", v.Warnings, want)
		}
	})
}

// TestValidateReportsOnlyTheOutdatedNodesThatStay validates a cluster that has one worker too many: the surplus worker
// is a failure and is not named as outdated, and the warning is in the singular.
func TestValidateReportsOnlyTheOutdatedNodesThatStay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		mustReplace(t, svc, edit(t, workersMetaYAML, "size: 2", "size: 1"))

		v := mustValidate(t, svc)

		want := "1 node is outdated: prod-workers-0; tent rolling-update cluster replaces it"
		if v.Valid() || !slices.Contains(v.Warnings, want) {
			t.Errorf("valid %t, warnings %q, want an invalid cluster and the warning %q", v.Valid(), v.Warnings, want)
		}
	})
}

// TestValidateWithoutOutdatedNodesDoesNotWarn adds no warning for a cluster of the hashes of its groups.
func TestValidateWithoutOutdatedNodesDoesNotWarn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := rollWorld(t)

		v := mustValidate(t, svc)

		for _, prefix := range []string{"1 node ", "2 nodes ", couldNotTell} {
			if n := withCount(v.Warnings, prefix); n != 0 {
				t.Errorf("warnings = %q, want none that start with %q", v.Warnings, prefix)
			}
		}
	})
}

// TestValidateGoesOnWhenTheReleaseFilesCannotBeRead warns that it cannot tell which nodes are outdated and finds the
// cluster valid.
func TestValidateGoesOnWhenTheReleaseFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := outdatedWorld(t)
		svc.Assets.Client = &http.Client{Transport: brokenSites{}}
		warnings := told(svc)

		v := mustValidate(t, svc)

		if n := withCount(v.Warnings, couldNotTell); !v.Valid() || n != 1 || withCount(*warnings, couldNotTell) != 1 {
			t.Errorf("valid %t, warnings %q, told %q, want a valid cluster and one warning that starts with %q",
				v.Valid(), v.Warnings, *warnings, couldNotTell)
		}
		if !strings.Contains(strings.Join(v.Warnings, "\n"), errSitesDown.Error()) {
			t.Errorf("warnings = %q, want one that names %v", v.Warnings, errSitesDown)
		}
	})
}

// TestValidateStopsWhenInterruptedWhileItReadsTheReleaseFiles returns the interruption, not a warning.
func TestValidateStopsWhenInterruptedWhileItReadsTheReleaseFiles(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := rollWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		svc.Assets.Client = &http.Client{Transport: cancelingSites{cancel}}

		_, err := svc.ValidateCluster(ctx, "prod")

		if err == nil || err.Error() != "interrupted" {
			t.Errorf("ValidateCluster error = %v, want an interruption", err)
		}
	})
}

// TestValidateReadsNoReleaseFilesWithoutMachines asks no release site for a cluster whose machines are all gone.
func TestValidateReadsNoReleaseFilesWithoutMachines(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := rollWorld(t)
		for _, in := range f.Instances() {
			if err := f.DeleteInstance(t.Context(), in.ID); err != nil {
				t.Fatalf("delete %s: %v", in.ID, err)
			}
		}
		var asked atomic.Int32
		svc.Assets.Client = &http.Client{Transport: countingSites{&asked}}

		v, err := svc.ValidateCluster(t.Context(), "prod")

		if err != nil || asked.Load() != 0 || v.Valid() {
			t.Errorf("ValidateCluster error = %v, valid %t after %d release requests, want none, an invalid cluster "+
				"and 0", err, v.Valid(), asked.Load())
		}
	})
}

// countingSites is a release site that counts its requests and answers none.
type countingSites struct{ n *atomic.Int32 }

func (s countingSites) RoundTrip(*http.Request) (*http.Response, error) {
	s.n.Add(1)
	return nil, errSitesDown
}
