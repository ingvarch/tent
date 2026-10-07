package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	// metadataJob and metadataTask are the job and the task that testdata/metadata.nomad.hcl defines.
	metadataJob  = "e2e-metadata"
	metadataTask = "probe"
)

// metadataGroups are the groups of the metadata job, one for each network a container can use.
var metadataGroups = []string{"nomad-bridge", "docker-bridge", "host"}

// The exit codes of the probe script of the metadata job.
const (
	probeReached   = 10
	probeOtherFail = 11
	probeNoNetwork = 12
)

// groupResult is what one group of the metadata job did: whether its task ended, its exit code and its stderr.
type groupResult struct {
	Group      string
	Terminated bool
	ExitCode   int
	Stderr     string
}

// metadataProblems returns one line for each group of the metadata job that did not end with exit code 0, in the
// order of metadataGroups. A group that has no result counts as one that did not run.
func metadataProblems(results []groupResult) []string {
	var problems []string
	for _, group := range metadataGroups {
		res := groupResult{Group: group}
		for _, r := range results {
			if r.Group == group {
				res = r
			}
		}
		if reason := probeProblem(res); reason != "" {
			problems = append(problems, group+": "+reason)
		}
	}
	return problems
}

// probeProblem returns why a group failed, or "" when its task exited with 0.
func probeProblem(res groupResult) string {
	switch {
	case !res.Terminated:
		return "did not run"
	case res.ExitCode == 0:
		return ""
	case res.ExitCode == probeReached:
		return "reached the metadata service"
	case res.ExitCode == probeOtherFail:
		return "the metadata probe failed in another way: " + lastLines(res.Stderr, stderrTailLines)
	case res.ExitCode == probeNoNetwork:
		return "no network: the control URL failed, so a timeout would prove nothing"
	default:
		return fmt.Sprintf("exit %d", res.ExitCode)
	}
}

// metadataOutcome is nil when the wait ended without an error and every group's task exited with 0.
func metadataOutcome(results []groupResult, waitErr error) error {
	var errs []error
	if waitErr != nil {
		errs = append(errs, waitErr)
	}
	if problems := metadataProblems(results); len(problems) > 0 {
		errs = append(errs, errors.New(strings.Join(problems, "; ")))
	}
	return errors.Join(errs...)
}

// metadataAllocation is an allocation of the metadata job with its group.
type metadataAllocation struct {
	jobAllocation
	TaskGroup string
}

// readMetadataAllocations lists the allocations of the metadata job.
func readMetadataAllocations(ctx context.Context, n *nomadAPI) ([]metadataAllocation, error) {
	var allocs []metadataAllocation
	err := n.get(ctx, "/v1/job/"+url.PathEscape(metadataJob)+"/allocations", &allocs)
	return allocs, err
}

// finishedGroups returns the allocation of each group of the metadata job that has ended, complete or failed, by
// group. Of several allocations of a group it takes the first that has ended.
func finishedGroups(allocs []metadataAllocation) map[string]string {
	finished := map[string]string{}
	for _, a := range allocs {
		if _, done := finished[a.TaskGroup]; done || !slices.Contains(metadataGroups, a.TaskGroup) {
			continue
		}
		if a.ClientStatus == "complete" || a.ClientStatus == "failed" {
			finished[a.TaskGroup] = a.ID
		}
	}
	return finished
}

// crowdedGroups returns one line for each group of the metadata job that has more than one allocation, in the order
// of metadataGroups. The job never restarts or reschedules, so a second allocation is a fault.
func crowdedGroups(allocs []metadataAllocation) []string {
	count := map[string]int{}
	for _, a := range allocs {
		count[a.TaskGroup]++
	}
	var crowded []string
	for _, g := range metadataGroups {
		if count[g] > 1 {
			crowded = append(crowded, fmt.Sprintf("group %s has %d allocations, want 1", g, count[g]))
		}
	}
	return crowded
}

// waitingGroups names the groups of the metadata job that have no allocation that has ended, each with the status
// of its first allocation or "no allocation".
func waitingGroups(allocs []metadataAllocation) []string {
	finished := finishedGroups(allocs)
	var waiting []string
	for _, g := range metadataGroups {
		if _, ok := finished[g]; ok {
			continue
		}
		status := "no allocation"
		for _, a := range allocs {
			if a.TaskGroup == g {
				status = a.ClientStatus
				break
			}
		}
		waiting = append(waiting, fmt.Sprintf("%s (%s)", g, status))
	}
	return waiting
}

// waitMetadataFinished asks every `every` until all groups of the metadata job have an allocation that has ended,
// or until timeout. It returns the allocations that had ended, also when the wait fails; the error names the
// groups it still waited for. A group with more than one allocation ends the wait at once with an error.
func waitMetadataFinished(ctx context.Context, n *nomadAPI, every, timeout time.Duration) (map[string]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := map[string]string{}
	var fault error
	err := pollUntil(ctx, every, timeout, func(ctx context.Context) string {
		allocs, err := readMetadataAllocations(ctx, n)
		if err != nil {
			return err.Error()
		}
		finished = finishedGroups(allocs)
		if crowded := crowdedGroups(allocs); len(crowded) > 0 {
			fault = errors.New(strings.Join(crowded, "; "))
			cancel()
			return fault.Error()
		}
		if waiting := waitingGroups(allocs); len(waiting) > 0 {
			return "waiting for " + strings.Join(waiting, ", ")
		}
		return ""
	})
	if fault != nil {
		return finished, fault
	}
	return finished, err
}

// taskEvent is an event of a task of an allocation.
type taskEvent struct {
	Type     string
	ExitCode int
}

// readGroupResult reads from the allocation of a group how the probe task ended and, when it ended, its stderr.
func readGroupResult(ctx context.Context, n *nomadAPI, group, allocID string) (groupResult, error) {
	res := groupResult{Group: group}
	var alloc struct {
		TaskStates map[string]struct{ Events []taskEvent }
	}
	id := url.PathEscape(allocID)
	if err := n.get(ctx, "/v1/allocation/"+id, &alloc); err != nil {
		return res, err
	}
	for _, e := range alloc.TaskStates[metadataTask].Events {
		if e.Type == "Terminated" {
			res.Terminated, res.ExitCode = true, e.ExitCode
		}
	}
	if !res.Terminated {
		return res, nil
	}
	stderr, err := n.getText(ctx, "/v1/client/fs/logs/"+id+"?task="+metadataTask+"&type=stderr&plain=true&origin=start")
	res.Stderr = stderr
	return res, err
}

// readMetadataResults returns a result for each group of the metadata job, in the order of metadataGroups, from the
// allocations in finished. A group that is not in finished has a result of a group that did not run. On an error it
// returns the results read until then.
func readMetadataResults(ctx context.Context, n *nomadAPI, finished map[string]string) ([]groupResult, error) {
	results := make([]groupResult, 0, len(metadataGroups))
	for _, g := range metadataGroups {
		id, ok := finished[g]
		if !ok {
			results = append(results, groupResult{Group: g})
			continue
		}
		res, err := readGroupResult(ctx, n, g, id)
		if err != nil {
			return results, fmt.Errorf("read the result of group %s: %w", g, err)
		}
		results = append(results, res)
	}
	return results, nil
}

// collectMetadata waits for the groups of the metadata job to end and reads their results. When the wait fails it
// still reads the groups that ended, and returns their results with the wait's error.
func collectMetadata(ctx context.Context, n *nomadAPI, every, timeout time.Duration) ([]groupResult, error) {
	finished, waitErr := waitMetadataFinished(ctx, n, every, timeout)
	results, err := readMetadataResults(ctx, n, finished)
	return results, errors.Join(waitErr, err)
}
