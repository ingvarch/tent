package buildconfig_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"
)

// repoFile reads a file by its path from the repository root.
func repoFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

// workflow is the part of a GitHub Actions workflow the tests look at.
type workflow struct {
	text []byte // the file as written, for the comments the parser drops

	Permissions map[string]string `json:"permissions"`
	Jobs        map[string]job    `json:"jobs"`
}

type job struct {
	file, name string // where the job is, for failure messages

	RunsOn   string `json:"runs-on"`
	Strategy struct {
		Matrix map[string]any `json:"matrix"`
	} `json:"strategy"`
	Steps []step `json:"steps"`
}

type step struct {
	Name string            `json:"name"`
	If   string            `json:"if"`
	Uses string            `json:"uses"`
	Run  string            `json:"run"`
	With map[string]any    `json:"with"`
	Env  map[string]string `json:"env"`
}

// with returns an input of the step as text, or "" when the step does not set it.
func (s step) with(key string) string {
	v, ok := s.With[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

// usesAction reports whether the step uses the action at any version.
func (s step) usesAction(action string) bool {
	return strings.HasPrefix(s.Uses, action+"@")
}

// where names the job in failure messages.
func (j job) where() string {
	return fmt.Sprintf("%s: job %s", j.file, j.name)
}

// uses reports whether a step of the job uses the action.
func (j job) uses(action string) bool {
	return slices.ContainsFunc(j.Steps, func(s step) bool { return s.usesAction(action) })
}

// runsGo reports whether a run step of the job calls the go command.
func (j job) runsGo() bool {
	for _, s := range j.Steps {
		for line := range strings.Lines(s.Run) {
			if strings.HasPrefix(strings.TrimSpace(line), "go ") {
				return true
			}
		}
	}
	return false
}

// workflowNames lists the workflow files.
func workflowNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", ".github", "workflows"))
	if err != nil {
		t.Fatalf("reading the workflows: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("there are no workflows")
	}
	return names
}

// loadWorkflow parses one workflow file.
func loadWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	return parseWorkflow(t, name, repoFile(t, ".github/workflows/"+name))
}

// parseWorkflow parses the text of the workflow file name.
func parseWorkflow(t *testing.T, name string, text []byte) workflow {
	t.Helper()
	w := workflow{text: text}
	if err := yaml.Unmarshal(text, &w); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	for jobName, j := range w.Jobs {
		j.file, j.name = name, jobName
		w.Jobs[jobName] = j
	}
	return w
}

// workflowJob returns a job of a workflow file, which must exist.
func workflowJob(t *testing.T, file, name string) job {
	t.Helper()
	j, ok := loadWorkflow(t, file).Jobs[name]
	if !ok {
		t.Fatalf("%s has no job %q", file, name)
	}
	return j
}

// matrixValues returns a list from a job's matrix.
func matrixValues(t *testing.T, j job, key string) []string {
	t.Helper()
	raw, ok := j.Strategy.Matrix[key].([]any)
	if !ok {
		t.Fatalf("%s: the matrix has no list %q", j.where(), key)
	}
	values := make([]string, 0, len(raw))
	for _, v := range raw {
		values = append(values, fmt.Sprint(v))
	}
	return values
}

// matrixEntries returns the maps under include or exclude in a job's matrix, or none when the key is absent.
func matrixEntries(t *testing.T, j job, key string) []map[string]any {
	t.Helper()
	raw, ok := j.Strategy.Matrix[key]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s: the matrix %s is not a list", j.where(), key)
	}
	entries := make([]map[string]any, 0, len(list))
	for _, e := range list {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s: the matrix %s holds %v, not a map", j.where(), key, e)
		}
		entries = append(entries, entry)
	}
	return entries
}

// fits reports whether a matrix entry sets no os or go other than the combination's, as GitHub matches include and
// exclude entries.
func fits(entry, combination map[string]any) bool {
	for _, k := range []string{"os", "go"} {
		if v, ok := entry[k]; ok && fmt.Sprint(v) != fmt.Sprint(combination[k]) {
			return false
		}
	}
	return true
}

// testCombinations returns the os/go pairs a job runs: os x go without the exclude entries, plus include entries
// that extend none of those combinations.
func testCombinations(t *testing.T, j job) []string {
	t.Helper()
	excludes := matrixEntries(t, j, "exclude")
	var combinations []map[string]any
	for _, osName := range matrixValues(t, j, "os") {
		for _, goRelease := range matrixValues(t, j, "go") {
			c := map[string]any{"os": osName, "go": goRelease}
			if !slices.ContainsFunc(excludes, func(e map[string]any) bool { return fits(e, c) }) {
				combinations = append(combinations, c)
			}
		}
	}
	// GitHub matches an include entry only against os x go, never against an earlier include entry.
	n := len(combinations)
	for _, e := range matrixEntries(t, j, "include") {
		if !slices.ContainsFunc(combinations[:n], func(c map[string]any) bool { return fits(e, c) }) {
			combinations = append(combinations, e)
		}
	}
	pairs := make([]string, 0, len(combinations))
	for _, c := range combinations {
		pairs = append(pairs, fmt.Sprintf("%v/%v", c["os"], c["go"]))
	}
	return pairs
}

// stringsIn returns every string in a parsed YAML value, at any depth.
func stringsIn(v any) []string {
	var found []string
	switch v := v.(type) {
	case string:
		found = append(found, v)
	case []any:
		for _, e := range v {
			found = append(found, stringsIn(e)...)
		}
	case map[string]any:
		for _, e := range v {
			found = append(found, stringsIn(e)...)
		}
	}
	return found
}

// commitSHA matches a full commit id.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// actionVersions lists the action of every step as action@version. A commit ref is named by the "# v..." comment on
// its line, or by nothing.
func (w workflow) actionVersions() []string {
	var versions []string
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if s.Uses == "" {
				continue
			}
			action, ref, _ := strings.Cut(s.Uses, "@")
			if commitSHA.MatchString(ref) {
				ref = w.versionComment(s.Uses)
			}
			versions = append(versions, action+"@"+ref)
		}
	}
	return versions
}

// versionComment returns the "# v..." comment that follows uses on its line, or "".
func (w workflow) versionComment(uses string) string {
	m := regexp.MustCompile(regexp.QuoteMeta(uses) + `[ \t]+#[ \t]*(v\S+)`).FindSubmatch(w.text)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// stepUsing returns the job's step that uses the action, which must exist.
func stepUsing(t *testing.T, j job, action string) step {
	t.Helper()
	for _, s := range j.Steps {
		if s.usesAction(action) {
			return s
		}
	}
	t.Fatalf("%s: no step uses %s", j.where(), action)
	return step{}
}

func TestActionVersionsReadEveryStep(t *testing.T) {
	// The retired-actions and pin checks see only what this returns, so no step may drop out.
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	cases := []struct {
		name, steps string
		want        []string
	}{
		{"tag", "- uses: actions/checkout@v7", []string{"actions/checkout@v7"}},
		{"commit with a version", "- uses: actions/checkout@" + sha + " # v7.0.1", []string{"actions/checkout@v7.0.1"}},
		{
			"comment of several words", "- uses: actions/upload-artifact@v3 # keep until the runner moves",
			[]string{"actions/upload-artifact@v3"},
		},
		{"quoted", `- uses: "actions/checkout@v4"`, []string{"actions/checkout@v4"}},
		{
			"commit without a version", "- uses: actions/checkout@" + sha + "\n      # v4.2.0\n      - uses: actions/setup-go@v7",
			[]string{"actions/checkout@", "actions/setup-go@v7"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := "jobs:\n  build:\n    steps:\n      " + c.steps + "\n"
			got := parseWorkflow(t, c.name, []byte(text)).actionVersions()
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("actionVersions of\n%s(-want +got):\n%s", text, diff)
			}
		})
	}
}

func TestWorkflowsPinTheRunner(t *testing.T) {
	// A floating label changes the machine under the build on a date somebody else picks.
	for _, name := range workflowNames(t) {
		for _, j := range loadWorkflow(t, name).Jobs {
			runners := []string{j.RunsOn}
			if strings.Contains(j.RunsOn, "${{") {
				runners = append(runners, stringsIn(j.Strategy.Matrix)...)
			}
			for _, r := range runners {
				if strings.Contains(r, "-latest") {
					t.Errorf("%s runs on the floating label %s", j.where(), r)
				}
			}
		}
	}
}

func TestCITestsTheModuleGoEverywhereAndTheNewestOnLinux(t *testing.T) {
	test := workflowJob(t, "ci.yml", "test")

	// The go.mod Go on every system, the newest too on Linux (macOS and Windows minutes are expensive on a private
	// repository). tent ships for all three systems and supports two Go releases (ADR-0013).
	want := []string{"ubuntu-26.04/go.mod", "macos-15/go.mod", "windows-2025/go.mod", "ubuntu-26.04/stable"}
	if diff := cmp.Diff(want, testCombinations(t, test), cmpopts.SortSlices(strings.Compare)); diff != "" {
		t.Errorf("%s: combinations (-want +got):\n%s", test.where(), diff)
	}
	// setup-go gets one input: it warns when it gets both.
	setup := stepUsing(t, test, "actions/setup-go")
	if got, want := setup.with("go-version-file"), "${{ matrix.go == 'go.mod' && 'go.mod' || '' }}"; got != want {
		t.Errorf("%s: go-version-file %q, want %q", test.where(), got, want)
	}
	if got, want := setup.with("go-version"), "${{ matrix.go != 'go.mod' && matrix.go || '' }}"; got != want {
		t.Errorf("%s: go-version %q, want %q", test.where(), got, want)
	}
}

func TestCITestsTheReleaseGoOnEverySystemItShipsFor(t *testing.T) {
	// The release builds with the go.mod Go, so every system it ships for runs the tests with that Go.
	runners := map[string]string{"linux": "ubuntu-26.04", "darwin": "macos-15", "windows": "windows-2025"}
	shipped := map[string]bool{}
	for _, b := range release(t).Builds {
		for _, target := range b.Targets {
			goos, _, _ := strings.Cut(target, "_")
			shipped[goos] = true
		}
	}
	if len(shipped) == 0 {
		t.Fatal("the release builds for no system")
	}
	combinations := testCombinations(t, workflowJob(t, "ci.yml", "test"))
	for goos := range shipped {
		runner, ok := runners[goos]
		if !ok {
			t.Errorf("the release ships for %s, which has no CI runner", goos)
			continue
		}
		if !slices.Contains(combinations, runner+"/go.mod") {
			t.Errorf("the release ships for %s, but CI does not test the go.mod Go on %s", goos, runner)
		}
	}
}

func TestWorkflowsTakeTheGoVersionFromTheModule(t *testing.T) {
	// Outside the test matrix, which has its own test, one place says which Go builds tent: go.mod.
	for _, name := range workflowNames(t) {
		for jobName, j := range loadWorkflow(t, name).Jobs {
			if name == "ci.yml" && jobName == "test" {
				continue
			}
			for _, s := range j.Steps {
				switch {
				case s.usesAction("actions/setup-go"):
					if s.with("go-version-file") != "go.mod" || s.with("go-version") != "" {
						t.Errorf("%s sets up Go without go.mod", j.where())
					}
				case s.usesAction("golang/govulncheck-action"):
					// go-version-input defaults to stable and wins over go-version-file.
					_, emptied := s.With["go-version-input"]
					if s.with("go-version-file") != "go.mod" || !emptied || s.with("go-version-input") != "" {
						t.Errorf("%s runs govulncheck without the go.mod version", j.where())
					}
				}
			}
		}
	}
}

func TestGoJobsSetUpGo(t *testing.T) {
	// Without setup-go a job builds with whatever Go the runner image carries. govulncheck-action sets up Go itself.
	for _, name := range workflowNames(t) {
		for _, j := range loadWorkflow(t, name).Jobs {
			needsGo := j.uses("golangci/golangci-lint-action") || j.uses("goreleaser/goreleaser-action") || j.runsGo()
			if needsGo && !j.uses("actions/setup-go") {
				t.Errorf("%s runs Go without actions/setup-go", j.where())
			}
		}
	}
}

func TestWorkflowsUseActionsThatAreStillThere(t *testing.T) {
	retired := []string{
		"actions/checkout@v3", "actions/checkout@v4",
		"actions/setup-go@v4", "actions/setup-go@v5",
		"actions/upload-artifact@v3", "actions/upload-artifact@v4",
		"actions/download-artifact@v3", "actions/download-artifact@v4",
	}
	for _, name := range workflowNames(t) {
		for _, used := range loadWorkflow(t, name).actionVersions() {
			for _, action := range retired {
				if used == action || strings.HasPrefix(used, action+".") {
					t.Errorf("%s uses the retired %s", name, used)
				}
			}
		}
	}
}
