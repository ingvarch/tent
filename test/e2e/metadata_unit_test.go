package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl"
)

func okGroup(group string) groupResult {
	return groupResult{Group: group, Terminated: true, ExitCode: 0, Stderr: "wget: download timed out\n"}
}

func allOK() []groupResult {
	return []groupResult{okGroup("nomad-bridge"), okGroup("docker-bridge"), okGroup("host")}
}

// withGroup returns results with the one of its group replaced by r.
func withGroup(results []groupResult, r groupResult) []groupResult {
	out := slices.Clone(results)
	for i := range out {
		if out[i].Group == r.Group {
			out[i] = r
		}
	}
	return out
}

func TestMetadataProblemsPassOnlyWhenAllThreeGroupsExitedWith0(t *testing.T) {
	if got := metadataProblems(allOK()); len(got) != 0 {
		t.Errorf("metadataProblems = %q, want none", got)
	}
}

func TestMetadataProblemsNameTheGroupAndTheReason(t *testing.T) {
	cases := []struct {
		name string
		in   groupResult
		want string
	}{
		{"reached", groupResult{Group: "docker-bridge", Terminated: true, ExitCode: 10, Stderr: "reached\n"},
			"docker-bridge: reached the metadata service"},
		{"another failure quotes stderr", groupResult{Group: "host", Terminated: true, ExitCode: 11,
			Stderr: "wget: can't connect to remote host (169.254.169.254): Connection refused\n"},
			"host: the metadata probe failed in another way: " +
				"wget: can't connect to remote host (169.254.169.254): Connection refused"},
		{"no network", groupResult{Group: "nomad-bridge", Terminated: true, ExitCode: 12, Stderr: "control-failed\n"},
			"nomad-bridge: no network: the control URL failed, so a timeout would prove nothing"},
		{"another code", groupResult{Group: "host", Terminated: true, ExitCode: 137}, "host: exit 137"},
		{"no terminated event", groupResult{Group: "host"}, "host: did not run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := metadataProblems(withGroup(allOK(), c.in))
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("metadataProblems = %q, want [%q]", got, c.want)
			}
		})
	}
}

func TestMetadataProblemsFailAGroupThatIsMissing(t *testing.T) {
	cases := map[string][]groupResult{
		"no results at all":   nil,
		"one group is absent": {okGroup("nomad-bridge"), okGroup("host")},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := metadataProblems(in)
			if len(got) == 0 || !slices.Contains(got, "docker-bridge: did not run") {
				t.Errorf("metadataProblems = %q, want %q among them", got, "docker-bridge: did not run")
			}
		})
	}
}

func TestMetadataProblemsListEveryFailingGroupInTheOrderOfTheGroups(t *testing.T) {
	in := withGroup(withGroup(allOK(),
		groupResult{Group: "host", Terminated: true, ExitCode: 10}),
		groupResult{Group: "nomad-bridge", Terminated: true, ExitCode: 12})
	want := []string{
		"nomad-bridge: no network: the control URL failed, so a timeout would prove nothing",
		"host: reached the metadata service",
	}
	if got := metadataProblems(in); !slices.Equal(got, want) {
		t.Errorf("metadataProblems = %q, want %q", got, want)
	}
}

func TestMetadataProblemsQuoteTheTailOfALongStderr(t *testing.T) {
	var lines []string
	for i := range stderrTailLines + 5 {
		lines = append(lines, "line "+string(rune('a'+i)))
	}
	in := withGroup(allOK(), groupResult{Group: "host", Terminated: true, ExitCode: 11, Stderr: strings.Join(lines, "\n")})
	got := metadataProblems(in)
	if len(got) != 1 || strings.Contains(got[0], "line a\n") || !strings.HasSuffix(got[0], lines[len(lines)-1]) {
		t.Errorf("metadataProblems = %q, want only the last %d lines", got, stderrTailLines)
	}
}

func TestMetadataOutcomeFailsOnAWaitErrorEvenWhenTheFinishedGroupsPass(t *testing.T) {
	waitErr := errors.New("waiting for host")
	if err := metadataOutcome(allOK(), nil); err != nil {
		t.Errorf("metadataOutcome = %v, want nil", err)
	}
	if err := metadataOutcome(allOK(), waitErr); !errors.Is(err, waitErr) {
		t.Errorf("metadataOutcome = %v, want the wait error", err)
	}
}

func TestMetadataOutcomeHoldsTheProblemsAndTheWaitError(t *testing.T) {
	in := withGroup(allOK(), groupResult{Group: "host"})
	err := metadataOutcome(in, errors.New("waiting for host"))
	if err == nil || !strings.Contains(err.Error(), "host: did not run") ||
		!strings.Contains(err.Error(), "waiting for host") {
		t.Errorf("metadataOutcome = %v, want the problem and the wait error", err)
	}
}

// labelledBlock returns the block of kind with the given label under parent: HCL decodes blocks that share a kind
// into a list with one entry for each label.
func labelledBlock(t *testing.T, parent map[string]any, kind, label string) map[string]any {
	t.Helper()
	list, _ := parent[kind].([]map[string]any)
	for _, entry := range list {
		if _, ok := entry[label]; ok {
			return block(t, entry, label)
		}
	}
	t.Fatalf("no %s %q in the job file", kind, label)
	return nil
}

const (
	allocNomadBridge  = "aaaa1111-0000"
	allocDockerBridge = "bbbb2222-0000"
	allocHost         = "cccc3333-0000"
)

func allocationsJSON(statuses map[string]string) string {
	ids := map[string]string{
		"nomad-bridge": allocNomadBridge, "docker-bridge": allocDockerBridge, "host": allocHost,
	}
	var parts []string
	for _, g := range metadataGroups {
		if status, ok := statuses[g]; ok {
			parts = append(parts, `{"ID":"`+ids[g]+`","TaskGroup":"`+g+`","ClientStatus":"`+status+`"}`)
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestFinishedGroupsTakesCompleteAndFailedAllocationsByGroup(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, allocationsJSON(map[string]string{
			"nomad-bridge": "complete", "docker-bridge": "failed", "host": "running",
		})),
	})
	allocs, err := readMetadataAllocations(t.Context(), n)
	if err != nil {
		t.Fatalf("readMetadataAllocations: %v", err)
	}
	got := finishedGroups(allocs)
	want := map[string]string{"nomad-bridge": allocNomadBridge, "docker-bridge": allocDockerBridge}
	if len(got) != len(want) || got["nomad-bridge"] != want["nomad-bridge"] ||
		got["docker-bridge"] != want["docker-bridge"] {
		t.Errorf("finishedGroups = %v, want %v", got, want)
	}
}

func TestFinishedGroupsPrefersAFinishedAllocationOfAGroup(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200,
			`[{"ID":"p1","TaskGroup":"host","ClientStatus":"pending"},{"ID":"f1","TaskGroup":"host","ClientStatus":"failed"}]`),
	})
	allocs, err := readMetadataAllocations(t.Context(), n)
	if got := finishedGroups(allocs); err != nil || got["host"] != "f1" {
		t.Errorf("finishedGroups = %v, %v, want host f1", got, err)
	}
}

func TestFinishedGroupsKeepsTheFirstOfSeveralFinishedAllocationsOfAGroup(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200,
			`[{"ID":"f1","TaskGroup":"host","ClientStatus":"failed"},{"ID":"c2","TaskGroup":"host","ClientStatus":"complete"}]`),
	})
	allocs, err := readMetadataAllocations(t.Context(), n)
	if got := finishedGroups(allocs); err != nil || got["host"] != "f1" {
		t.Errorf("finishedGroups = %v, %v, want host f1", got, err)
	}
}

func TestFinishedGroupsIgnoresAnAllocationOfAnotherGroup(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200,
			`[{"ID":"x1","TaskGroup":"other","ClientStatus":"complete"}]`),
	})
	allocs, err := readMetadataAllocations(t.Context(), n)
	if got := finishedGroups(allocs); err != nil || len(got) != 0 {
		t.Errorf("finishedGroups = %v, %v, want none", got, err)
	}
}

func TestReadMetadataAllocationsReturnsTheErrorOfTheCall(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(503, "no leader"),
	})
	if _, err := readMetadataAllocations(t.Context(), n); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("readMetadataAllocations error = %v, want the 503", err)
	}
}

func TestWaitMetadataFinishedPollsUntilAllThreeGroupsHaveFinished(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": func() (int, string) {
			mu.Lock()
			defer mu.Unlock()
			asked++
			if asked < 3 {
				return 200, allocationsJSON(map[string]string{"nomad-bridge": "complete", "host": "running"})
			}
			return 200, allocationsJSON(map[string]string{
				"nomad-bridge": "complete", "docker-bridge": "complete", "host": "failed",
			})
		},
	})
	got, err := waitMetadataFinished(t.Context(), n, time.Millisecond, time.Minute)
	if err != nil || len(got) != 3 || got["host"] != allocHost {
		t.Errorf("waitMetadataFinished = %v, %v, want 3 groups after 3 asks (asked %d)", got, err, asked)
	}
}

func TestWaitMetadataFinishedNamesTheGroupsStillWaitedForAndKeepsTheFinishedOnes(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, allocationsJSON(map[string]string{
			"nomad-bridge": "complete", "host": "running",
		})),
	})
	got, err := waitMetadataFinished(t.Context(), n, time.Millisecond, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "docker-bridge (no allocation)") ||
		!strings.Contains(err.Error(), "host (running)") || strings.Contains(err.Error(), "nomad-bridge") {
		t.Errorf("waitMetadataFinished error = %v, want the unfinished groups and not the finished one", err)
	}
	if len(got) != 1 || got["nomad-bridge"] != allocNomadBridge {
		t.Errorf("waitMetadataFinished = %v, want the finished group kept", got)
	}
}

func TestWaitMetadataFinishedFailsAtOnceWhenAGroupHasMoreThanOneAllocation(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, `[`+
			`{"ID":"`+allocNomadBridge+`","TaskGroup":"nomad-bridge","ClientStatus":"complete"},`+
			`{"ID":"h1","TaskGroup":"host","ClientStatus":"failed"},`+
			`{"ID":"h2","TaskGroup":"host","ClientStatus":"pending"}]`),
	})
	start := time.Now()
	got, err := waitMetadataFinished(t.Context(), n, time.Millisecond, time.Minute)
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("waitMetadataFinished took %v, want it to end at once", took)
	}
	if err == nil || err.Error() != "group host has 2 allocations, want 1" {
		t.Errorf("waitMetadataFinished error = %v, want the group with two allocations", err)
	}
	if len(got) != 2 || got["nomad-bridge"] != allocNomadBridge || got["host"] != "h1" {
		t.Errorf("waitMetadataFinished = %v, want the groups that had ended kept", got)
	}
}

func TestWaitMetadataFinishedNamesEveryGroupWithMoreThanOneAllocation(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, `[`+
			`{"ID":"a1","TaskGroup":"nomad-bridge","ClientStatus":"complete"},`+
			`{"ID":"a2","TaskGroup":"nomad-bridge","ClientStatus":"complete"},`+
			`{"ID":"a3","TaskGroup":"nomad-bridge","ClientStatus":"complete"},`+
			`{"ID":"h1","TaskGroup":"host","ClientStatus":"complete"},`+
			`{"ID":"h2","TaskGroup":"host","ClientStatus":"complete"}]`),
	})
	_, err := waitMetadataFinished(t.Context(), n, time.Millisecond, 5*time.Second)
	want := "group nomad-bridge has 3 allocations, want 1; group host has 2 allocations, want 1"
	if err == nil || err.Error() != want {
		t.Errorf("waitMetadataFinished error = %v, want %q", err, want)
	}
}

func TestWaitMetadataFinishedIgnoresSeveralAllocationsOfAnotherGroup(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, `[`+
			`{"ID":"x1","TaskGroup":"other","ClientStatus":"complete"},`+
			`{"ID":"x2","TaskGroup":"other","ClientStatus":"complete"},`+
			`{"ID":"`+allocNomadBridge+`","TaskGroup":"nomad-bridge","ClientStatus":"complete"},`+
			`{"ID":"`+allocDockerBridge+`","TaskGroup":"docker-bridge","ClientStatus":"complete"},`+
			`{"ID":"`+allocHost+`","TaskGroup":"host","ClientStatus":"complete"}]`),
	})
	got, err := waitMetadataFinished(t.Context(), n, time.Millisecond, 5*time.Second)
	if err != nil || len(got) != 3 {
		t.Errorf("waitMetadataFinished = %v, %v, want the three groups and no error", got, err)
	}
}

func terminatedJSON(code string) string {
	return `{"TaskStates":{"probe":{"Events":[{"Type":"Started"},{"Type":"Terminated","ExitCode":` + code + `}]}}}`
}

func logsPath(id string) string {
	return "GET /v1/client/fs/logs/" + id + "?task=probe&type=stderr&plain=true&origin=start"
}

func TestReadMetadataResultsTakesTheExitCodeOfTheTerminatedEventAndTheStderr(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/allocation/" + allocNomadBridge:  fixed(200, terminatedJSON("0")),
		logsPath(allocNomadBridge):                fixed(200, "wget: download timed out\n"),
		"GET /v1/allocation/" + allocDockerBridge: fixed(200, terminatedJSON("10")),
		logsPath(allocDockerBridge):               fixed(200, "reached\n"),
		"GET /v1/allocation/" + allocHost:         fixed(200, terminatedJSON("11")),
		logsPath(allocHost):                       fixed(200, "boom\n"),
	})
	got, err := readMetadataResults(t.Context(), n, map[string]string{
		"nomad-bridge": allocNomadBridge, "docker-bridge": allocDockerBridge, "host": allocHost,
	})
	if err != nil {
		t.Fatalf("readMetadataResults: %v", err)
	}
	want := []groupResult{
		{Group: "nomad-bridge", Terminated: true, ExitCode: 0, Stderr: "wget: download timed out\n"},
		{Group: "docker-bridge", Terminated: true, ExitCode: 10, Stderr: "reached\n"},
		{Group: "host", Terminated: true, ExitCode: 11, Stderr: "boom\n"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("readMetadataResults = %+v, want %+v (requests %q)", got, want, f.requests)
	}
}

func TestReadMetadataResultsMarksAGroupWithoutAllocationOrTerminatedEventAsNotRun(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/allocation/" + allocHost: fixed(200,
			`{"TaskStates":{"probe":{"Events":[{"Type":"Driver Failure"}]}}}`),
	})
	got, err := readMetadataResults(t.Context(), n, map[string]string{"host": allocHost})
	if err != nil {
		t.Fatalf("readMetadataResults: %v", err)
	}
	for _, r := range got {
		if r.Terminated || r.ExitCode != 0 || r.Stderr != "" {
			t.Errorf("result %+v, want a group that did not run", r)
		}
	}
	if len(got) != 3 || got[0].Group != "nomad-bridge" || got[2].Group != "host" {
		t.Errorf("readMetadataResults = %+v, want the three groups in order", got)
	}
	if slices.Contains(f.requests, logsPath(allocHost)) {
		t.Errorf("requests = %q, read the log of a task that never ended", f.requests)
	}
}

func TestReadMetadataResultsTakesTheLastTerminatedEvent(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/allocation/" + allocHost: fixed(200, `{"TaskStates":{"probe":{"Events":[`+
			`{"Type":"Terminated","ExitCode":0},{"Type":"Restarting"},{"Type":"Terminated","ExitCode":10}]}}}`),
		logsPath(allocHost): fixed(200, ""),
	})
	got, err := readMetadataResults(t.Context(), n, map[string]string{"host": allocHost})
	if err != nil || got[2].ExitCode != 10 {
		t.Errorf("readMetadataResults = %+v, %v, want exit code 10 for host", got, err)
	}
}

func TestReadMetadataResultsFailsWhenACallFails(t *testing.T) {
	cases := map[string]map[string]func() (int, string){
		"allocation": {"GET /v1/allocation/" + allocHost: fixed(500, "down")},
		"log": {
			"GET /v1/allocation/" + allocHost: fixed(200, terminatedJSON("0")),
			logsPath(allocHost):               fixed(500, "no log"),
		},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			_, n := newFakeNomad(t, answers)
			if _, err := readMetadataResults(t.Context(), n, map[string]string{"host": allocHost}); err == nil ||
				!strings.Contains(err.Error(), "500") {
				t.Errorf("readMetadataResults error = %v, want the 500", err)
			}
		})
	}
}

func TestReadMetadataResultsReturnsTheResultsReadBeforeAFailure(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/allocation/" + allocNomadBridge:  fixed(200, terminatedJSON("0")),
		logsPath(allocNomadBridge):                fixed(200, "wget: download timed out\n"),
		"GET /v1/allocation/" + allocDockerBridge: fixed(500, "down"),
	})
	got, err := readMetadataResults(t.Context(), n, map[string]string{
		"nomad-bridge": allocNomadBridge, "docker-bridge": allocDockerBridge,
	})
	if err == nil || !strings.Contains(err.Error(), "group docker-bridge") {
		t.Errorf("readMetadataResults error = %v, want it to name group docker-bridge", err)
	}
	if len(got) != 1 || got[0].Group != "nomad-bridge" || !got[0].Terminated {
		t.Errorf("readMetadataResults = %+v, want the result of nomad-bridge", got)
	}
}

func TestCollectMetadataKeepsWhatFinishedWhenTheWaitTimesOut(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, allocationsJSON(map[string]string{"host": "complete"})),
		"GET /v1/allocation/" + allocHost:      fixed(200, terminatedJSON("0")),
		logsPath(allocHost):                    fixed(200, "wget: download timed out\n"),
	})
	results, err := collectMetadata(t.Context(), n, time.Millisecond, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "docker-bridge (no allocation)") {
		t.Errorf("collectMetadata error = %v, want the groups that did not finish", err)
	}
	if len(results) != 3 || !results[2].Terminated || results[0].Terminated {
		t.Errorf("collectMetadata results = %+v, want host read and the others not run", results)
	}
	if outcome := metadataOutcome(results, err); outcome == nil {
		t.Error("metadataOutcome = nil for a wait that timed out")
	}
}

func TestCollectMetadataReturnsTheErrorOfAReadWhenAllGroupsFinished(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, allocationsJSON(map[string]string{
			"nomad-bridge": "complete", "docker-bridge": "complete", "host": "complete",
		})),
		"GET /v1/allocation/" + allocNomadBridge: fixed(500, "down"),
	})
	_, err := collectMetadata(t.Context(), n, time.Millisecond, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("collectMetadata error = %v, want the 500", err)
	}
}

func TestCollectMetadataPassesWhenAllThreeFinishedWithExit0(t *testing.T) {
	answers := map[string]func() (int, string){
		"GET /v1/job/e2e-metadata/allocations": fixed(200, allocationsJSON(map[string]string{
			"nomad-bridge": "complete", "docker-bridge": "complete", "host": "complete",
		})),
	}
	for _, id := range []string{allocNomadBridge, allocDockerBridge, allocHost} {
		answers["GET /v1/allocation/"+id] = fixed(200, terminatedJSON("0"))
		answers[logsPath(id)] = fixed(200, "wget: download timed out\n")
	}
	_, n := newFakeNomad(t, answers)
	results, err := collectMetadata(t.Context(), n, time.Millisecond, time.Minute)
	if outcome := metadataOutcome(results, err); outcome != nil {
		t.Errorf("metadataOutcome = %v, want nil", outcome)
	}
}

func TestMetadataJobFileDefinesThreeGroupsOfOneProbeEach(t *testing.T) {
	data, err := os.ReadFile("testdata/metadata.nomad.hcl")
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	var doc map[string]any
	if err := hcl.Decode(&doc, string(data)); err != nil {
		t.Fatalf("decode the job file: %v", err)
	}
	job := block(t, doc, "job", metadataJob)
	if job["type"] != "batch" {
		t.Errorf("job type = %v, want batch", job["type"])
	}
	wantMode := map[string]string{"nomad-bridge": "", "docker-bridge": "bridge", "host": "host"}
	for _, g := range metadataGroups {
		group := labelledBlock(t, job, "group", g)
		task := labelledBlock(t, group, "task", metadataTask)
		cfg := block(t, task, "config")
		if task["driver"] != "docker" || cfg["image"] != "busybox:1.38" || cfg["command"] != "sh" {
			t.Errorf("group %s: task = %v, want a docker busybox:1.38 task that runs sh", g, task)
		}
		if args, _ := cfg["args"].([]any); len(args) != 1 || args[0] != "/local/probe.sh" {
			t.Errorf("group %s: args = %v, want [/local/probe.sh]", g, cfg["args"])
		}
		if mode, _ := cfg["network_mode"].(string); mode != wantMode[g] {
			t.Errorf("group %s: network_mode = %q, want %q", g, mode, wantMode[g])
		}
		if res := block(t, task, "resources"); res["cpu"] != 50 || res["memory"] != 32 {
			t.Errorf("group %s: resources = %v, want cpu 50 and memory 32", g, res)
		}
		restart := block(t, group, "restart")
		if restart["attempts"] != 0 || restart["mode"] != "fail" {
			t.Errorf("group %s: restart = %v, want attempts 0 and mode fail", g, restart)
		}
		reschedule := block(t, group, "reschedule")
		if reschedule["attempts"] != 0 || reschedule["unlimited"] != false {
			t.Errorf("group %s: reschedule = %v, want attempts 0 and unlimited false", g, reschedule)
		}
		if tpl := block(t, task, "template"); tpl["destination"] != "local/probe.sh" {
			t.Errorf("group %s: template destination = %v, want local/probe.sh", g, tpl["destination"])
		}
	}
	nomadBridge := labelledBlock(t, job, "group", "nomad-bridge")
	if net := block(t, nomadBridge, "network"); net["mode"] != "bridge" {
		t.Errorf("nomad-bridge: network = %v, want mode bridge", net)
	}
	for _, g := range []string{"docker-bridge", "host"} {
		if _, ok := labelledBlock(t, job, "group", g)["network"]; ok {
			t.Errorf("group %s has a network block, want none", g)
		}
	}
}

// probeScript returns the script the job file gives to the probe task of group.
func probeScript(t *testing.T, group string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/metadata.nomad.hcl")
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	var doc map[string]any
	if err := hcl.Decode(&doc, string(data)); err != nil {
		t.Fatalf("decode the job file: %v", err)
	}
	job := block(t, doc, "job", metadataJob)
	tpl := block(t, labelledBlock(t, labelledBlock(t, job, "group", group), "task", metadataTask), "template")
	script, ok := tpl["data"].(string)
	if !ok {
		t.Fatalf("template data of group %s is %T, want a string", group, tpl["data"])
	}
	return script
}

func TestProbeScriptIsTheSameInEveryGroup(t *testing.T) {
	first := probeScript(t, metadataGroups[0])
	for _, g := range metadataGroups[1:] {
		if got := probeScript(t, g); got != first {
			t.Errorf("group %s: the probe script differs from the one of %s", g, metadataGroups[0])
		}
	}
}

// fakeWget is a wget that answers by the URL it is asked for and by the environment of the test.
const fakeWget = `#!/bin/sh
echo "$*" >>"$FAKE_LOG"
for url; do :; done
case "$url" in
*deb.debian.org* | *detectportal.firefox.com* | *captive.apple.com*)
	case "$url" in *"$FAKE_CONTROL"*) [ -n "$FAKE_CONTROL" ] && exit 0 ;; esac
	echo "wget: download timed out" >&2
	exit 1 ;;
*169.254.169.254*)
	case "$FAKE_METADATA" in
	reach) exit 0 ;;
	timeout) echo "wget: download timed out" >&2; exit 1 ;;
	*) echo "wget: can't connect to remote host (169.254.169.254): Connection refused" >&2; exit 1 ;;
	esac ;;
esac
echo "unexpected url $url" >&2
exit 99
`

// The calls of wget that the probe script makes: the control URLs with 10 seconds each, in order, until one answers,
// then the metadata service with 5.
const (
	debianCall   = "-q -T 10 -O /dev/null http://deb.debian.org/debian/"
	firefoxCall  = "-q -T 10 -O /dev/null http://detectportal.firefox.com/success.txt"
	appleCall    = "-q -T 10 -O /dev/null http://captive.apple.com/hotspot-detect.html"
	metadataCall = "-q -T 5 -O /dev/null http://169.254.169.254/v1.json"
)

func TestProbeScriptExitsWithTheCodesTheVerdictReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe script and the fake wget are sh scripts")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wget"), []byte(fakeWget), 0o700); err != nil {
		t.Fatalf("write the fake wget: %v", err)
	}
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte(probeScript(t, "host")), 0o600); err != nil {
		t.Fatalf("write the probe script: %v", err)
	}
	cases := []struct {
		name, control, metadata string
		code                    int
		stderr                  string
		calls                   []string
	}{
		{"the metadata service times out", "deb.debian.org", "timeout", 0, "timed out",
			[]string{debianCall, metadataCall}},
		{"the metadata service answers", "deb.debian.org", "reach", 10, "reached", []string{debianCall, metadataCall}},
		{"the metadata service refuses", "deb.debian.org", "refused", 11, "Connection refused",
			[]string{debianCall, metadataCall}},
		{"a later control URL answers", "captive.apple.com", "timeout", 0, "timed out",
			[]string{debianCall, firefoxCall, appleCall, metadataCall}},
		{"the second control URL answers", "detectportal.firefox.com", "timeout", 0, "timed out",
			[]string{debianCall, firefoxCall, metadataCall}},
		{"no control URL answers", "", "timeout", 12, "control-failed", []string{debianCall, firefoxCall, appleCall}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), sh, script)
			calls := filepath.Join(t.TempDir(), "calls")
			cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + "/usr/bin:/bin",
				"FAKE_CONTROL=" + c.control, "FAKE_METADATA=" + c.metadata, "FAKE_LOG=" + calls}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("run the probe script: %v", err)
				}
				code = exit.ExitCode()
			}
			if code != c.code || !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("probe script = exit %d, stderr %q, want exit %d and stderr with %q",
					code, stderr.String(), c.code, c.stderr)
			}
			log, _ := os.ReadFile(calls)
			if got := strings.Split(strings.TrimSpace(string(log)), "\n"); !slices.Equal(got, c.calls) {
				t.Errorf("wget calls = %q, want %q", got, c.calls)
			}
		})
	}
}
