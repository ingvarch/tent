package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var probeBase = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// steadyRecords returns n polls, one second apart, that are all led and served.
func steadyRecords(n int) []probeRecord {
	records := make([]probeRecord, n)
	for i := range records {
		records[i] = probeRecord{
			Start:   probeBase.Add(time.Duration(i) * time.Second),
			Leader:  probeLed,
			Service: probeServed,
		}
	}
	return records
}

func TestDefaultProbeLimitsAreThoseOfTheScenarios(t *testing.T) {
	want := probeLimits{
		Leaderless: 10 * time.Second,
		Unserved:   10 * time.Second,
		Unreached:  30 * time.Second,
		Gap:        30 * time.Second,
		MinPolls:   10,
	}
	if defaultProbeLimits != want {
		t.Errorf("defaultProbeLimits = %+v, want %+v", defaultProbeLimits, want)
	}
}

func TestProbeVerdictPassesASteadyRunAndCountsIt(t *testing.T) {
	summary, err := probeVerdict(steadyRecords(20), defaultProbeLimits)
	if err != nil {
		t.Fatalf("probeVerdict: %v", err)
	}
	want := "polls: led 20, leaderless 0, leader unreached 0, served 20, unserved 0, service unreached 0; " +
		"longest stretch: leaderless 0s, leader unreached 0s, unserved 0s, service unreached 0s; longest gap 1s"
	if got := summary.String(); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

func TestProbeVerdictSummaryNamesTheLongestStretchOfEachKind(t *testing.T) {
	records := steadyRecords(30)
	for i := 3; i < 6; i++ {
		records[i].Leader = probeLeaderless
	}
	for i := 10; i < 14; i++ {
		records[i].Service = probeUnserved
	}
	for i := 20; i < 25; i++ {
		records[i].Leader = probeUnreached
		records[i].Service = probeUnreached
	}
	summary, err := probeVerdict(records, defaultProbeLimits)
	if err != nil {
		t.Fatalf("probeVerdict: %v", err)
	}
	want := "polls: led 22, leaderless 3, leader unreached 5, served 21, unserved 4, service unreached 5; " +
		"longest stretch: leaderless 3s, leader unreached 5s, unserved 4s, service unreached 5s; longest gap 1s"
	if got := summary.String(); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

// TestProbeVerdictStretchLimits runs a bad stretch of each kind that is exactly as long as its limit and one second
// longer: the first passes, the second fails and the error names the kind.
func TestProbeVerdictStretchLimits(t *testing.T) {
	cases := []struct {
		name  string
		limit time.Duration
		set   func(*probeRecord)
	}{
		{"leaderless", defaultProbeLimits.Leaderless, func(r *probeRecord) { r.Leader = probeLeaderless }},
		{"leader unreached", defaultProbeLimits.Unreached, func(r *probeRecord) { r.Leader = probeUnreached }},
		{"unserved", defaultProbeLimits.Unserved, func(r *probeRecord) { r.Service = probeUnserved }},
		{"service unreached", defaultProbeLimits.Unreached, func(r *probeRecord) { r.Service = probeUnreached }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The stretch lasts from its first poll to the first good poll after it.
			polls := int(tc.limit / time.Second)
			at := func(n int) error {
				records := steadyRecords(5 + n + 20)
				for i := 5; i < 5+n; i++ {
					tc.set(&records[i])
				}
				_, err := probeVerdict(records, defaultProbeLimits)
				return err
			}
			if err := at(polls); err != nil {
				t.Errorf("a stretch of %s: %v, want a pass", tc.limit, err)
			}
			err := at(polls + 1)
			if err == nil {
				t.Fatalf("a stretch of %s passed, want a failure", tc.limit+time.Second)
			}
			for _, want := range []string{tc.name + " for " + (tc.limit + time.Second).String(), "12:00:05.000"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestProbeVerdictKindsHaveTheirOwnLimits(t *testing.T) {
	// 20 s is within the unreached limit and past the leaderless and unserved limits.
	records := steadyRecords(50)
	for i := 5; i < 25; i++ {
		records[i].Leader = probeUnreached
		records[i].Service = probeUnreached
	}
	if _, err := probeVerdict(records, defaultProbeLimits); err != nil {
		t.Errorf("20 s unreached: %v, want a pass", err)
	}
	records = steadyRecords(50)
	for i := 5; i < 25; i++ {
		records[i].Leader = probeLeaderless
		records[i].Service = probeUnserved
	}
	err := func() error { _, err := probeVerdict(records, defaultProbeLimits); return err }()
	if err == nil || !strings.Contains(err.Error(), "leaderless for 20s") ||
		!strings.Contains(err.Error(), "unserved for 20s") {
		t.Errorf("20 s leaderless and unserved: %v, want both named", err)
	}
}

func TestProbeVerdictFailsOnAGapBetweenPolls(t *testing.T) {
	withGap := func(gap time.Duration) error {
		records := steadyRecords(20)
		for i := 10; i < len(records); i++ {
			records[i].Start = records[i].Start.Add(gap - time.Second)
		}
		_, err := probeVerdict(records, defaultProbeLimits)
		return err
	}
	if err := withGap(30 * time.Second); err != nil {
		t.Errorf("a gap of 30s: %v, want a pass", err)
	}
	err := withGap(31 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "no poll started for 31s after 12:00:09.000") {
		t.Errorf("a gap of 31s: %v, want it named with its start", err)
	}
}

func TestProbeVerdictFailsWhenTheFirstOrTheLastPollIsNotLedAndServed(t *testing.T) {
	cases := []struct {
		name string
		edit func(records []probeRecord)
		want string
	}{
		{"first leaderless", func(r []probeRecord) { r[0].Leader = probeLeaderless }, "the first poll"},
		{"first unserved", func(r []probeRecord) { r[0].Service = probeUnserved }, "the first poll"},
		{"last leaderless", func(r []probeRecord) { r[len(r)-1].Leader = probeLeaderless }, "the last poll"},
		{"last unserved", func(r []probeRecord) { r[len(r)-1].Service = probeUnserved }, "the last poll"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := steadyRecords(20)
			tc.edit(records)
			_, err := probeVerdict(records, defaultProbeLimits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestProbeVerdictMeasuresABadStretchAtTheStartAndAtTheEnd(t *testing.T) {
	records := steadyRecords(30)
	for i := 0; i < 12; i++ {
		records[i].Leader = probeLeaderless
	}
	for i := 18; i < 30; i++ {
		records[i].Service = probeUnserved
	}
	_, err := probeVerdict(records, defaultProbeLimits)
	if err == nil {
		t.Fatal("probeVerdict passed, want a failure")
	}
	for _, want := range []string{"leaderless for 12s from 12:00:00.000", "unserved for 11s from 12:00:18.000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestProbeVerdictNeedsTenPolls(t *testing.T) {
	if _, err := probeVerdict(steadyRecords(10), defaultProbeLimits); err != nil {
		t.Errorf("10 polls: %v, want a pass", err)
	}
	_, err := probeVerdict(steadyRecords(9), defaultProbeLimits)
	if err == nil || !strings.Contains(err.Error(), "9 polls, at least 10 are needed") {
		t.Errorf("9 polls: %v, want the count named", err)
	}
	if _, err := probeVerdict(nil, defaultProbeLimits); err == nil {
		t.Error("no polls passed, want a failure")
	}
}

func TestProbeVerdictNamesEveryBreach(t *testing.T) {
	records := steadyRecords(30)
	records[0].Leader = probeUnreached
	for i := 5; i < 20; i++ {
		records[i].Service = probeUnserved
	}
	_, err := probeVerdict(records, defaultProbeLimits)
	if err == nil {
		t.Fatal("probeVerdict passed, want a failure")
	}
	for _, want := range []string{"the first poll", "unserved for 15s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestProbeLineHasTheColumnsOfTheHeader(t *testing.T) {
	r := probeRecord{
		Start:      time.Date(2026, 10, 13, 12, 0, 1, 234_000_000, time.UTC),
		Took:       1500 * time.Millisecond,
		Leader:     probeLed,
		LeaderAddr: "10.0.0.1:4647",
		Service:    probeServed,
		Detail:     "a",
	}
	want := "2026-10-13T12:00:01.234Z\t1500\tled\t10.0.0.1:4647\tserved\ta\n"
	if got := probeLine(r); got != want {
		t.Errorf("probeLine = %q, want %q", got, want)
	}
	if got, want := probeHeader, "start\ttook_ms\tleader\tleader_address\tservice\tdetail\n"; got != want {
		t.Errorf("probeHeader = %q, want %q", got, want)
	}
}

func TestProbeLineKeepsTheDetailOnOneLine(t *testing.T) {
	r := probeRecord{Start: probeBase, Detail: "one\ttwo\nthree\r\nfour"}
	got := probeLine(r)
	if strings.Count(got, "\n") != 1 || strings.Count(got, "\t") != 5 {
		t.Errorf("probeLine = %q, want one line of six columns", got)
	}
	if !strings.HasSuffix(got, "\tone two three four\n") {
		t.Errorf("probeLine = %q, want the detail with spaces", got)
	}
}

func TestNewProbeConfigHasTheScenarioValues(t *testing.T) {
	var out strings.Builder
	cfg := newProbeConfig(&nomadAPI{}, nil, "job", "svc", &out)
	if cfg.Every != time.Second || cfg.Refresh != 10*time.Second || cfg.RefreshTimeout != 5*time.Second ||
		cfg.LeaderTimeout != 3*time.Second || cfg.ServiceTimeout != 7*time.Second {
		t.Errorf("config = %+v, want every 1s, refresh 10s in 5s, leader reads 3s, service reads 7s", cfg)
	}
	if cfg.Job != "job" || cfg.Service != "svc" || cfg.Out != &out {
		t.Errorf("config = %+v, want the job, the service and the writer", cfg)
	}
}

// newNode starts a fake server with answers.
func newNode(t *testing.T, answers map[string]func() (int, string)) *fakeNomad {
	t.Helper()
	f, _ := newFakeNomad(t, answers)
	return f
}

// openOnCleanup opens gate when the test ends. A test calls it after it has started the servers, so a blocked answer
// goes before the servers close.
func openOnCleanup(t *testing.T, gate chan struct{}) {
	t.Helper()
	t.Cleanup(func() { close(gate) })
}

func leaderAnswers(leader string) map[string]func() (int, string) {
	answers := readyAnswers()
	answers["GET /v1/status/leader?stale=true"] = fixed(200, `"`+leader+`"`)
	return answers
}

func noServiceAnswers(leader func() (int, string)) map[string]func() (int, string) {
	return map[string]func() (int, string){"GET /v1/status/leader?stale=true": leader}
}

// deadAddress is the address of a server that was closed.
func deadAddress(t *testing.T) string {
	t.Helper()
	f := newNode(t, nil)
	f.srv.Close()
	return f.srv.URL
}

func testProbeConfig(n *nomadAPI, addresses ...string) probeConfig {
	return probeConfig{
		Nomad: n,
		Addresses: func(context.Context) ([]string, error) {
			return addresses, nil
		},
		Job:            webJob,
		Service:        webService,
		Every:          5 * time.Millisecond,
		Refresh:        5 * time.Millisecond,
		RefreshTimeout: time.Second,
		LeaderTimeout:  40 * time.Millisecond,
		ServiceTimeout: 40 * time.Millisecond,
		Out:            &syncBuffer{},
	}
}

// pollOnce makes one poll of the servers at addresses.
func pollOnce(t *testing.T, addresses ...string) probeRecord {
	t.Helper()
	p := &probe{cfg: testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}}), addresses: addresses}
	return p.poll(t.Context())
}

func TestPollLeaderIsLedWhenAnyServerNamesOne(t *testing.T) {
	dead := deadAddress(t)
	empty := newNode(t, noServiceAnswers(fixed(200, `""`)))
	leading := newNode(t, leaderAnswers("10.0.0.1:4647"))
	got := pollOnce(t, dead, empty.srv.URL, leading.srv.URL)
	if got.Leader != probeLed || got.LeaderAddr != "10.0.0.1:4647" {
		t.Errorf("leader = %q at %q, want led at 10.0.0.1:4647", got.Leader, got.LeaderAddr)
	}
	if !strings.Contains(got.Detail, dead) {
		t.Errorf("detail = %q, want the failed server named", got.Detail)
	}
}

func TestPollServiceIsReadThroughTheServerThatNamedTheLeader(t *testing.T) {
	// The first server leads nothing and answers 404 to every service read.
	empty := newNode(t, noServiceAnswers(fixed(200, `""`)))
	leading := newNode(t, leaderAnswers("10.0.0.1:4647"))
	got := pollOnce(t, empty.srv.URL, leading.srv.URL)
	if got.Service != probeServed {
		t.Errorf("service = %q (%s), want served", got.Service, got.Detail)
	}
	for _, r := range empty.requests {
		if strings.Contains(r, "/v1/job/") {
			t.Errorf("requests of the server without a leader = %q, want no service read", empty.requests)
		}
	}
}

func TestPollLeaderlessWhenNoServerNamesALeader(t *testing.T) {
	cases := map[string]func() (int, string){
		"empty answer":      fixed(200, `""`),
		"no cluster leader": fixed(500, "No cluster leader"),
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			a := newNode(t, noServiceAnswers(answer))
			b := newNode(t, noServiceAnswers(answer))
			got := pollOnce(t, a.srv.URL, b.srv.URL)
			if got.Leader != probeLeaderless || got.LeaderAddr != "" {
				t.Errorf("leader = %q at %q, want leaderless", got.Leader, got.LeaderAddr)
			}
		})
	}
}

func TestPollServiceIsReadThroughAServerThatAnsweredWhenNoneNamedALeader(t *testing.T) {
	answers := readyAnswers()
	answers["GET /v1/status/leader?stale=true"] = fixed(200, `""`)
	got := pollOnce(t, deadAddress(t), newNode(t, answers).srv.URL)
	if got.Leader != probeLeaderless || got.Service != probeServed {
		t.Errorf("leader = %q, service = %q (%s), want leaderless and served", got.Leader, got.Service, got.Detail)
	}
}

func TestPollLeaderIsUnreachedWhenNoServerAnswers(t *testing.T) {
	failing := newNode(t, noServiceAnswers(fixed(500, "boom")))
	got := pollOnce(t, deadAddress(t), failing.srv.URL)
	if got.Leader != probeUnreached || got.Service != probeUnreached {
		t.Errorf("leader = %q, service = %q, want both unreached", got.Leader, got.Service)
	}
	if !strings.Contains(got.Detail, "boom") || strings.Count(got.Detail, "; ") != 1 {
		t.Errorf("detail = %q, want the errors of the two servers and no service read", got.Detail)
	}
}

func TestPollWithoutAddressesIsUnreached(t *testing.T) {
	got := pollOnce(t)
	if got.Leader != probeUnreached || got.Service != probeUnreached || got.Detail != "no server address is known" {
		t.Errorf("poll = %+v, want unreached with the reason", got)
	}
}

func TestPollLeaderSlowServerRunsIntoItsTimeoutBesideALeader(t *testing.T) {
	gate := make(chan struct{})
	slow := newNode(t, noServiceAnswers(func() (int, string) { <-gate; return 200, `"x"` }))
	openOnCleanup(t, gate)
	leading := newNode(t, leaderAnswers("10.0.0.1:4647"))
	got := pollOnce(t, slow.srv.URL, leading.srv.URL)
	if got.Leader != probeLed || !strings.Contains(got.Detail, slow.srv.URL) {
		t.Errorf("leader = %q (%s), want led with the slow server named", got.Leader, got.Detail)
	}
}

func TestPollLeaderOfOnlyASlowServerIsUnreached(t *testing.T) {
	gate := make(chan struct{})
	slow := newNode(t, noServiceAnswers(func() (int, string) { <-gate; return 200, `"x"` }))
	openOnCleanup(t, gate)
	got := pollOnce(t, slow.srv.URL)
	if got.Leader != probeUnreached || !strings.Contains(got.Detail, "deadline exceeded") {
		t.Errorf("leader = %q (%s), want unreached by the timeout", got.Leader, got.Detail)
	}
}

func allocationsOf(ids ...string) func() (int, string) {
	var items []string
	for _, id := range ids {
		items = append(items, `{"ID":"`+id+`","ClientStatus":"running"}`)
	}
	return fixed(200, "["+strings.Join(items, ",")+"]")
}

func checksPath(id string) string { return "GET /v1/allocation/" + id + "/checks" }

func serviceNode(t *testing.T, edit func(answers map[string]func() (int, string))) string {
	t.Helper()
	answers := leaderAnswers("10.0.0.1:4647")
	edit(answers)
	return newNode(t, answers).srv.URL
}

func TestPollServiceOutcomes(t *testing.T) {
	const registered = `[{"AllocID":"` + allocRunning + `"},{"AllocID":"` + allocOther + `"}]`
	cases := []struct {
		name string
		edit func(answers map[string]func() (int, string))
		want string
		note string
	}{
		{"one healthy allocation", func(map[string]func() (int, string)) {}, probeServed, ""},
		{"no running allocation", func(a map[string]func() (int, string)) {
			a["GET /v1/job/e2e-web/allocations"] = fixed(200, `[{"ID":"`+allocRunning+`","ClientStatus":"pending"}]`)
		}, probeUnserved, "allocations: 11111111 pending; checks: none"},
		{"check fails", func(a map[string]func() (int, string)) {
			a[checksPath(allocRunning)] = fixed(200, `{"c1":{"Service":"e2e-web","Status":"failure"}}`)
		}, probeUnserved, "checks: 11111111 e2e-web failure"},
		{"no registration", func(a map[string]func() (int, string)) {
			a["GET /v1/service/e2e-web"] = fixed(200, `[]`)
		}, probeUnserved, "registrations: none"},
		{"checks read of one of two fails", func(a map[string]func() (int, string)) {
			a["GET /v1/job/e2e-web/allocations"] = allocationsOf(allocRunning, allocOther)
			a["GET /v1/service/e2e-web"] = fixed(200, registered)
			a[checksPath(allocOther)] = fixed(500, "no route to client")
		}, probeServed, ""},
		{"checks read of one of two fails and the other has no check", func(a map[string]func() (int, string)) {
			a["GET /v1/job/e2e-web/allocations"] = allocationsOf(allocRunning, allocOther)
			a["GET /v1/service/e2e-web"] = fixed(200, registered)
			a[checksPath(allocRunning)] = fixed(200, `{}`)
			a[checksPath(allocOther)] = fixed(500, "no route to client")
		}, probeUnserved, "allocations:"},
		{"checks reads of every allocation fail", func(a map[string]func() (int, string)) {
			a["GET /v1/job/e2e-web/allocations"] = allocationsOf(allocRunning, allocOther)
			a["GET /v1/service/e2e-web"] = fixed(200, registered)
			a[checksPath(allocRunning)] = fixed(500, "no route to client")
			a[checksPath(allocOther)] = fixed(500, "no route to client")
		}, probeUnreached, "no route to client"},
		{"allocations read fails", func(a map[string]func() (int, string)) {
			a["GET /v1/job/e2e-web/allocations"] = fixed(500, "allocations down")
		}, probeUnreached, "allocations down"},
		{"registrations read fails", func(a map[string]func() (int, string)) {
			a["GET /v1/service/e2e-web"] = fixed(500, "registrations down")
		}, probeUnreached, "registrations down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pollOnce(t, serviceNode(t, tc.edit))
			if got.Service != tc.want {
				t.Errorf("service = %q (%s), want %q", got.Service, got.Detail, tc.want)
			}
			if !strings.Contains(got.Detail, tc.note) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tc.note)
			}
			if tc.want == probeServed && strings.Contains(got.Detail, "allocations:") {
				t.Errorf("detail = %q, want the answers quoted only when the service is not served", got.Detail)
			}
		})
	}
}

func TestPollServiceReadThatIsSlowIsUnreached(t *testing.T) {
	gate := make(chan struct{})
	address := serviceNode(t, func(a map[string]func() (int, string)) {
		a["GET /v1/job/e2e-web/allocations"] = func() (int, string) { <-gate; return 200, "[]" }
	})
	openOnCleanup(t, gate)
	got := pollOnce(t, address)
	if got.Service != probeUnreached || !strings.Contains(got.Detail, "deadline exceeded") {
		t.Errorf("service = %q (%s), want unreached by the timeout", got.Service, got.Detail)
	}
}

// syncBuffer is a writer that a test can read while the probe writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eventually waits until cond holds; it fails the test after 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func leadingProbeConfig(t *testing.T) probeConfig {
	t.Helper()
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	return testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}}, address)
}

func TestProbeWritesTheHeaderFirstAndEachRecordAsItComes(t *testing.T) {
	cfg := leadingProbeConfig(t)
	out := cfg.Out.(*syncBuffer)
	p := startProbe(t.Context(), cfg)
	eventually(t, "two lines", func() bool { return strings.Count(out.String(), "\n") >= 3 })
	records := p.stop()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if lines[0]+"\n" != probeHeader {
		t.Errorf("first line = %q, want the header", lines[0])
	}
	polls := 0
	for _, l := range lines[1:] {
		if strings.Contains(l, "\tled\t10.0.0.1:4647\tserved\t") {
			polls++
		}
	}
	if polls != len(records) || len(records) < 2 {
		t.Errorf("%d poll lines for %d records, want the same number, at least 2:\n%s", polls, len(records), out)
	}
}

func TestProbeStopReturnsEveryRecordAndEndsTheGoroutines(t *testing.T) {
	var polls, lists atomic.Int32
	address := newNode(t, map[string]func() (int, string){
		"GET /v1/status/leader?stale=true": func() (int, string) { polls.Add(1); return 200, `"x"` },
	}).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	cfg.Addresses = func(context.Context) ([]string, error) { lists.Add(1); return []string{address}, nil }
	p := startProbe(t.Context(), cfg)
	out := cfg.Out.(*syncBuffer)
	eventually(t, "three records", func() bool { return strings.Count(out.String(), "\n") >= 4 })
	records := p.stop()
	if len(records) < 3 {
		t.Errorf("stop returned %d records, want at least 3", len(records))
	}
	polled, listed := polls.Load(), lists.Load()
	time.Sleep(30 * time.Millisecond)
	if polls.Load() != polled || lists.Load() != listed {
		t.Errorf("polls %d to %d, lists %d to %d after stop, want no more", polled, polls.Load(), listed, lists.Load())
	}
}

func TestProbeStopDropsThePollItInterrupted(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	address := newNode(t, noServiceAnswers(func() (int, string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return 200, `"x"`
	})).srv.URL
	openOnCleanup(t, gate)
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}}, address)
	cfg.LeaderTimeout = time.Minute
	cfg.Out = &syncBuffer{}
	p := startProbe(t.Context(), cfg)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the leader read")
	}
	if records := p.stop(); len(records) != 0 {
		t.Errorf("stop returned %+v, want no record of an interrupted poll", records)
	}
	if got := cfg.Out.(*syncBuffer).String(); got != probeHeader {
		t.Errorf("output = %q, want the header alone", got)
	}
}

func TestProbeFirstPollWaitsForTheFirstList(t *testing.T) {
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	cfg.Addresses = func(context.Context) ([]string, error) {
		time.Sleep(50 * time.Millisecond)
		return []string{address}, nil
	}
	p := startProbe(t.Context(), cfg)
	eventually(t, "a record", func() bool { return strings.Count(cfg.Out.(*syncBuffer).String(), "\n") >= 2 })
	records := p.stop()
	if records[0].Leader != probeLed {
		t.Errorf("first record = %+v, want led: the poll came before the first list", records[0])
	}
}

func TestProbeRefreshThatBlocksDoesNotDelayThePolls(t *testing.T) {
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	cfg.RefreshTimeout = time.Hour
	var calls atomic.Int32
	blocked := make(chan struct{})
	var once sync.Once
	cfg.Addresses = func(ctx context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			return []string{address}, nil
		}
		once.Do(func() { close(blocked) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p := startProbe(t.Context(), cfg)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the second list")
	}
	out := cfg.Out.(*syncBuffer)
	before := strings.Count(out.String(), "\tserved\t")
	eventually(t, "polls while the list blocks", func() bool {
		return strings.Count(out.String(), "\tserved\t") >= before+3
	})
	p.stop()
	if strings.Contains(out.String(), "the list of servers failed") {
		t.Errorf("output = %q, want no line for a list that the stop ended", out.String())
	}
}

func TestProbeRefreshEndsAtItsTimeoutAndIsWrittenAsALine(t *testing.T) {
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	cfg.RefreshTimeout = 10 * time.Millisecond
	out := cfg.Out.(*syncBuffer)
	var calls atomic.Int32
	cfg.Addresses = func(ctx context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		// The lists name no server until a poll has run without one.
		if !strings.Contains(out.String(), "no server address is known") {
			return nil, nil
		}
		return []string{address}, nil
	}
	p := startProbe(t.Context(), cfg)
	eventually(t, "a served poll", func() bool { return strings.Contains(out.String(), "\tserved\t") })
	records := p.stop()
	if !strings.Contains(out.String(), "\t-\t-\t-\tthe list of servers failed: context deadline exceeded\n") {
		t.Errorf("output = %q, want a line for the failed list", out.String())
	}
	if records[0].Leader != probeUnreached || records[0].Detail != "no server address is known" {
		t.Errorf("first record = %+v, want unreached: no list came before the first poll", records[0])
	}
	if got := len(records); strings.Count(out.String(), "\n") != got+2 {
		t.Errorf("output holds %d lines for %d records, want the header and the failed list besides them",
			strings.Count(out.String(), "\n"), got)
	}
}

func TestProbeKeepsTheLastAddressesWhenAListFails(t *testing.T) {
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	var calls atomic.Int32
	cfg.Addresses = func(context.Context) ([]string, error) {
		if calls.Add(1) == 1 {
			return []string{address}, nil
		}
		return nil, errors.New("vultr is down")
	}
	p := startProbe(t.Context(), cfg)
	out := cfg.Out.(*syncBuffer)
	eventually(t, "a failed list", func() bool { return strings.Contains(out.String(), "vultr is down") })
	before := strings.Count(out.String(), "\tserved\t")
	eventually(t, "polls after it", func() bool { return strings.Count(out.String(), "\tserved\t") >= before+3 })
	for _, r := range p.stop() {
		if r.Leader != probeLed {
			t.Errorf("record = %+v, want led: the last addresses are kept", r)
		}
	}
}

func TestProbeListThatSucceedsReplacesTheAddresses(t *testing.T) {
	gone := deadAddress(t)
	address := newNode(t, leaderAnswers("10.0.0.1:4647")).srv.URL
	cfg := testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}})
	out := cfg.Out.(*syncBuffer)
	cfg.Addresses = func(context.Context) ([]string, error) {
		// The lists name the dead server until a poll has asked it.
		if !strings.Contains(out.String(), "\tunreached\t") {
			return []string{gone}, nil
		}
		return []string{address}, nil
	}
	p := startProbe(t.Context(), cfg)
	eventually(t, "a served poll", func() bool { return strings.Contains(out.String(), "\tserved\t") })
	records := p.stop()
	if records[0].Leader != probeUnreached {
		t.Errorf("first record = %+v, want unreached: the first list names a dead server", records[0])
	}
}

// TestPollAsksTheServersForTheLeaderAtOnce gives each server a leader answer that waits until the other server has
// been asked too: asked one after the other, the first read would run into its timeout.
func TestPollAsksTheServersForTheLeaderAtOnce(t *testing.T) {
	gate := make(chan struct{})
	asked := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var once [2]sync.Once
	address := func(self, other int) string {
		answers := leaderAnswers("10.0.0.1:4647")
		answers["GET /v1/status/leader?stale=true"] = func() (int, string) {
			once[self].Do(func() { close(asked[self]) })
			select {
			case <-asked[other]:
				return 200, `"10.0.0.1:4647"`
			case <-gate:
				return 500, "the other server was never asked"
			}
		}
		return newNode(t, answers).srv.URL
	}
	addresses := []string{address(0, 1), address(1, 0)}
	openOnCleanup(t, gate)
	p := &probe{cfg: testProbeConfig(&nomadAPI{token: "t", client: &http.Client{}}), addresses: addresses}
	p.cfg.LeaderTimeout = 2 * time.Second
	got := p.poll(t.Context())
	if got.Leader != probeLed || got.Detail != "" {
		t.Errorf("leader = %q (%s), want led with no failed read", got.Leader, got.Detail)
	}
}
