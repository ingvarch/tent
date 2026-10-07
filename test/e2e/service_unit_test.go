package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl"
)

const (
	allocRunning = "11111111-aaaa"
	allocOther   = "22222222-bbbb"
)

func passingCheck() map[string]allocationCheck {
	return map[string]allocationCheck{"c1": {Service: "e2e-web", Status: "success"}}
}

func TestServiceMissing(t *testing.T) {
	running := jobAllocation{ID: allocRunning, ClientStatus: "running"}
	cases := []struct {
		name string
		in   serviceAnswers
		want []string
	}{
		{"ready", serviceAnswers{
			Allocations:   []jobAllocation{running},
			Checks:        map[string]map[string]allocationCheck{allocRunning: passingCheck()},
			Registrations: []serviceRegistration{{AllocID: allocRunning}},
		}, nil},
		{"no allocation", serviceAnswers{}, []string{"a running allocation"}},
		{"allocation not running", serviceAnswers{
			Allocations: []jobAllocation{{ID: allocRunning, ClientStatus: "pending"}},
		}, []string{"a running allocation"}},
		{"nothing but a running allocation", serviceAnswers{Allocations: []jobAllocation{running}},
			[]string{"a successful check of e2e-web", "a registration of e2e-web"}},
		{"the check of another service", serviceAnswers{
			Allocations:   []jobAllocation{running},
			Checks:        map[string]map[string]allocationCheck{allocRunning: {"c1": {Service: "other", Status: "success"}}},
			Registrations: []serviceRegistration{{AllocID: allocRunning}},
		}, []string{"a successful check of e2e-web"}},
		{"a check that is not successful yet", serviceAnswers{
			Allocations:   []jobAllocation{running},
			Checks:        map[string]map[string]allocationCheck{allocRunning: {"c1": {Service: "e2e-web", Status: "pending"}}},
			Registrations: []serviceRegistration{{AllocID: allocRunning}},
		}, []string{"a successful check of e2e-web"}},
		{"a registration of another allocation", serviceAnswers{
			Allocations:   []jobAllocation{running},
			Checks:        map[string]map[string]allocationCheck{allocRunning: passingCheck()},
			Registrations: []serviceRegistration{{AllocID: allocOther}},
		}, []string{"a registration of e2e-web"}},
		{"an old allocation that is complete beside a ready one", serviceAnswers{
			Allocations: []jobAllocation{{ID: allocOther, ClientStatus: "complete"}, running},
			Checks:      map[string]map[string]allocationCheck{allocRunning: passingCheck()},
			Registrations: []serviceRegistration{
				{AllocID: allocOther}, {AllocID: allocRunning},
			},
		}, nil},
		{"the allocation with the fewest gaps is named, not the first", serviceAnswers{
			Allocations:   []jobAllocation{running, {ID: allocOther, ClientStatus: "running"}},
			Checks:        map[string]map[string]allocationCheck{allocOther: passingCheck()},
			Registrations: nil,
		}, []string{"a registration of e2e-web"}},
		{"the check and the registration belong to different allocations", serviceAnswers{
			Allocations: []jobAllocation{running, {ID: allocOther, ClientStatus: "running"}},
			Checks:      map[string]map[string]allocationCheck{allocRunning: passingCheck()},
			Registrations: []serviceRegistration{
				{AllocID: allocOther},
			},
		}, []string{"a registration of e2e-web"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.missing("e2e-web"); !slices.Equal(got, c.want) {
				t.Errorf("missing = %q, want %q", got, c.want)
			}
		})
	}
}

func TestServiceAnswersStringIsShortAndInOrder(t *testing.T) {
	a := serviceAnswers{
		Allocations: []jobAllocation{{ID: allocRunning, ClientStatus: "running"}, {ID: allocOther, ClientStatus: "pending"}},
		Checks: map[string]map[string]allocationCheck{allocRunning: {
			"zz": {Service: "e2e-web", Status: "failure"}, "aa": {Service: "e2e-web", Status: "success"},
		}},
		Registrations: []serviceRegistration{{AllocID: allocRunning}},
	}
	want := "allocations: 11111111 running, 22222222 pending; " +
		"checks: 11111111 e2e-web success, 11111111 e2e-web failure; registrations: 11111111"
	if got := a.String(); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestServiceAnswersStringSaysNone(t *testing.T) {
	want := "allocations: none; checks: none; registrations: none"
	if got := (serviceAnswers{}).String(); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

// fakeNomad answers the paths of a job's service from a table and records the requests.
type fakeNomad struct {
	mu       sync.Mutex
	answers  map[string]func() (int, string)
	requests []string
	bodies   map[string]string
	srv      *httptest.Server
}

func newFakeNomad(t *testing.T, answers map[string]func() (int, string)) (*fakeNomad, *nomadAPI) {
	t.Helper()
	f := &fakeNomad{answers: answers, bodies: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.RequestURI()
		f.mu.Lock()
		f.requests = append(f.requests, key)
		f.bodies[key] = string(body)
		answer, ok := f.answers[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no answer for "+key, http.StatusNotFound)
			return
		}
		code, text := answer()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(f.srv.Close)
	return f, &nomadAPI{base: f.srv.URL, token: "t", client: f.srv.Client()}
}

func fixed(code int, text string) func() (int, string) {
	return func() (int, string) { return code, text }
}

func TestReadServiceAnswersAsksOnlyRunningAllocationsForChecks(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations": fixed(200, `[{"ID":"`+allocRunning+`","ClientStatus":"running"},`+
			`{"ID":"`+allocOther+`","ClientStatus":"complete"}]`),
		"GET /v1/allocation/" + allocRunning + "/checks": fixed(200,
			`{"c1":{"Service":"e2e-web","Status":"success","Mode":"healthiness"}}`),
		"GET /v1/service/e2e-web": fixed(200, `[{"ServiceName":"e2e-web","AllocID":"`+allocRunning+`"}]`),
	})
	got, err := readServiceAnswers(t.Context(), n, "e2e-web", "e2e-web")
	if err != nil {
		t.Fatalf("readServiceAnswers: %v", err)
	}
	if missing := got.missing("e2e-web"); len(missing) != 0 {
		t.Errorf("missing = %q, want none in %v", missing, got)
	}
	if slices.Contains(f.requests, "GET /v1/allocation/"+allocOther+"/checks") {
		t.Errorf("requests = %q, asked the checks of an allocation that is not running", f.requests)
	}
}

func TestReadServiceAnswersKeepsWhatItReadWhenALaterCallFails(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations":                fixed(200, `[{"ID":"`+allocRunning+`","ClientStatus":"running"}]`),
		"GET /v1/allocation/" + allocRunning + "/checks": fixed(200, `{}`),
		"GET /v1/service/e2e-web":                        fixed(500, "down"),
	})
	got, err := readServiceAnswers(t.Context(), n, "e2e-web", "e2e-web")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("readServiceAnswers error = %v, want the 500", err)
	}
	if len(got.Allocations) != 1 {
		t.Errorf("answers = %v, want the allocations that were read", got)
	}
}

func TestReadServiceAnswersFailsWhenTheChecksOfARunningAllocationFail(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations":                fixed(200, `[{"ID":"`+allocRunning+`","ClientStatus":"running"}]`),
		"GET /v1/allocation/" + allocRunning + "/checks": fixed(500, "no checks"),
	})
	if _, err := readServiceAnswers(t.Context(), n, "e2e-web", "e2e-web"); err == nil ||
		!strings.Contains(err.Error(), "500") {
		t.Errorf("readServiceAnswers error = %v, want the 500", err)
	}
}

func readyAnswers() map[string]func() (int, string) {
	return map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations": fixed(200, `[{"ID":"`+allocRunning+`","ClientStatus":"running"}]`),
		"GET /v1/allocation/" + allocRunning + "/checks": fixed(200,
			`{"c1":{"Service":"e2e-web","Status":"success"}}`),
		"GET /v1/service/e2e-web": fixed(200, `[{"AllocID":"`+allocRunning+`"}]`),
	}
}

func TestWaitServicePassesWhenTheThreeAnswersAgree(t *testing.T) {
	_, n := newFakeNomad(t, readyAnswers())
	if err := waitService(t.Context(), n, "e2e-web", "e2e-web", time.Millisecond, time.Minute); err != nil {
		t.Errorf("waitService: %v", err)
	}
}

func TestWaitServicePollsUntilTheCheckPasses(t *testing.T) {
	answers := readyAnswers()
	var mu sync.Mutex
	asked := 0
	answers["GET /v1/allocation/"+allocRunning+"/checks"] = func() (int, string) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		if asked < 3 {
			return 200, `{"c1":{"Service":"e2e-web","Status":"pending"}}`
		}
		return 200, `{"c1":{"Service":"e2e-web","Status":"success"}}`
	}
	_, n := newFakeNomad(t, answers)
	if err := waitService(t.Context(), n, "e2e-web", "e2e-web", time.Millisecond, time.Minute); err != nil {
		t.Errorf("waitService: %v", err)
	}
}

func TestServiceProblemNamesWhatIsMissingAndQuotesTheAnswers(t *testing.T) {
	answers := readyAnswers()
	answers["GET /v1/service/e2e-web"] = fixed(200, `[]`)
	_, n := newFakeNomad(t, answers)
	got := serviceProblem(t.Context(), n, "e2e-web", "e2e-web")
	for _, want := range []string{"missing a registration of e2e-web", "allocations: 11111111 running"} {
		if !strings.Contains(got, want) {
			t.Errorf("serviceProblem = %q, want it to hold %q", got, want)
		}
	}
	if strings.Contains(got, "successful check") {
		t.Errorf("serviceProblem = %q, names a check that passed", got)
	}
}

func TestServiceProblemQuotesTheErrorOfACall(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/job/e2e-web/allocations": fixed(503, "no leader"),
	})
	got := serviceProblem(t.Context(), n, "e2e-web", "e2e-web")
	if !strings.Contains(got, "503: no leader") || !strings.Contains(got, "last answers: allocations: none") {
		t.Errorf("serviceProblem = %q, want the 503 and the last answers", got)
	}
}

func TestSubmitJobParsesTheTextThenSubmitsTheParsedJob(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"POST /v1/jobs/parse": fixed(200, `{"ID":"e2e-web","Extra":{"a":[1,2]}}`),
		"POST /v1/jobs":       fixed(200, `{"EvalID":"e1","Warnings":""}`),
	})
	const jobText = `job "e2e-web" {}`
	if err := submitJob(t.Context(), n, jobText); err != nil {
		t.Fatalf("submitJob: %v", err)
	}
	if want := []string{"POST /v1/jobs/parse", "POST /v1/jobs"}; !slices.Equal(f.requests, want) {
		t.Fatalf("requests = %q, want %q", f.requests, want)
	}
	var parse struct {
		JobHCL       string
		Canonicalize bool
	}
	if err := json.Unmarshal([]byte(f.bodies["POST /v1/jobs/parse"]), &parse); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parse.JobHCL != jobText || !parse.Canonicalize {
		t.Errorf("parse body = %+v, want the text and Canonicalize", parse)
	}
	if got, want := f.bodies["POST /v1/jobs"], `{"Job":{"ID":"e2e-web","Extra":{"a":[1,2]}}}`; got != want {
		t.Errorf("submit body = %s, want %s", got, want)
	}
}

func TestSubmitJobNamesTheCallThatFailed(t *testing.T) {
	cases := map[string]map[string]func() (int, string){
		"parse": {"POST /v1/jobs/parse": fixed(400, "bad hcl")},
		"submit": {
			"POST /v1/jobs/parse": fixed(200, `{}`),
			"POST /v1/jobs":       fixed(500, "boom"),
		},
	}
	for step, answers := range cases {
		t.Run(step, func(t *testing.T) {
			_, n := newFakeNomad(t, answers)
			err := submitJob(t.Context(), n, "job {}")
			if err == nil || !strings.Contains(err.Error(), step+" the job") {
				t.Errorf("submitJob = %v, want %q", err, step+" the job")
			}
		})
	}
}

func TestPurgeJobDeletesTheJobWithPurge(t *testing.T) {
	f, n := newFakeNomad(t, map[string]func() (int, string){
		"DELETE /v1/job/e2e-web?purge=true": fixed(200, `{}`),
	})
	if err := purgeJob(t.Context(), n, "e2e-web"); err != nil {
		t.Fatalf("purgeJob: %v", err)
	}
	if want := []string{"DELETE /v1/job/e2e-web?purge=true"}; !slices.Equal(f.requests, want) {
		t.Errorf("requests = %q, want %q", f.requests, want)
	}
}

// block walks an HCL document decoded by hcl.Decode: at each name it takes the first block under it.
func block(t *testing.T, doc any, path ...string) map[string]any {
	t.Helper()
	cur := doc
	for _, name := range path {
		if list, ok := cur.([]map[string]any); ok {
			cur = list[0]
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("at %q: %T is not a block", name, cur)
		}
		if cur, ok = m[name]; !ok {
			t.Fatalf("no %q in the job file", name)
		}
	}
	if list, ok := cur.([]map[string]any); ok {
		return list[0]
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a block", path)
	}
	return m
}

func TestWebJobFileDefinesTheJobAndTheServiceTheStepWaitsFor(t *testing.T) {
	data, err := os.ReadFile("testdata/web.nomad.hcl")
	if err != nil {
		t.Fatalf("read the job file: %v", err)
	}
	var doc map[string]any
	if err := hcl.Decode(&doc, string(data)); err != nil {
		t.Fatalf("decode the job file: %v", err)
	}
	group := block(t, doc, "job", webJob, "group", "web")
	svc := block(t, group, "service")
	if svc["name"] != webService || svc["provider"] != "nomad" || svc["port"] != "http" {
		t.Errorf("service = %v, want name %q, provider nomad, port http", svc, webService)
	}
	if check := block(t, svc, "check"); check["type"] != "http" || check["path"] != "/" {
		t.Errorf("check = %v, want an http check of /", check)
	}
	res := block(t, group, "task", "web", "resources")
	if res["cpu"] != 50 || res["memory"] != 32 {
		t.Errorf("resources = %v, want cpu 50 and memory 32", res)
	}
}
