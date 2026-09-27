package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/statestore"
)

func TestEditorCommand(t *testing.T) {
	fallback := "vi"
	if runtime.GOOS == "windows" {
		fallback = "notepad"
	}
	for _, tc := range []struct {
		name                    string
		tentEditor, visual, env string
		want                    string
	}{
		{"TENT_EDITOR first", "code -w", "vim", "nano", "code -w"},
		{"then VISUAL", "", "vim", "nano", "vim"},
		{"then EDITOR", " ", "", "nano", "nano"},
		{"else the fallback", "", "", "", fallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TENT_EDITOR", tc.tentEditor)
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.env)
			if got := editorCommand(); got != tc.want {
				t.Errorf("editorCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"vi", []string{"vi"}},
		{"  code   --wait\t-n ", []string{"code", "--wait", "-n"}},
		{`"C:\Program Files\Editor\editor.exe" --wait`, []string{`C:\Program Files\Editor\editor.exe`, "--wait"}},
		{`emacs --eval="(setq a 1)"`, []string{"emacs", "--eval=(setq a 1)"}},
		{`ed "" -s`, []string{"ed", "", "-s"}},
	} {
		got, err := splitCommand(tc.line)
		if err != nil {
			t.Errorf("splitCommand(%q): %v", tc.line, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("splitCommand(%q) (-want +got):\n%s", tc.line, diff)
		}
	}
	const unterminated = `"C:\Program Files\Editor\editor.exe --wait`
	_, err := splitCommand(unterminated)
	if want := "the editor command has an unterminated quote: " + unterminated; err == nil || err.Error() != want {
		t.Errorf("splitCommand(%q) returned %v, want %q", unterminated, err, want)
	}
}

// editorDirEnv names the directory of the fake editor: its steps and the record of its runs.
const editorDirEnv = "TENT_TEST_EDITOR_DIR"

// editStep is what the fake editor does on one run. The zero step saves the file unchanged.
type editStep struct {
	Old, New string    // replace the first Old in the file with New
	Content  *string   // write Content over the file
	Put      *storePut // write an object to a state store, as another tent would
	Lock     bool      // make the file's directory read-only, so that tent cannot remove the file
	CRLF     bool      // end the file's lines with CR LF
	BOM      bool      // start the file with a UTF-8 byte order mark
	Wait     bool      // create the file waiting in the editor's directory, then wait for the file go
	Print    string    // a line to write to stdout and stderr
	ReadLine bool      // read a line from stdin
	Exit     int       // the exit code
}

// storePut is an object that the fake editor writes to the state store at URL.
type storePut struct{ URL, Path, Data string }

// editorRun is what the fake editor was given on one run.
type editorRun struct {
	Args    []string    // its arguments, the file last
	Content string      // what the file held
	Mode    os.FileMode // the file's permissions
	Stdin   string      // the line it read from stdin
}

// fakeEditor is the test binary run again as the editor of tent edit.
type fakeEditor struct {
	dir string // the steps and the runs
	tmp string // the temporary directory of tent edit, which holds the file it edits
}

// newFakeEditor makes the test binary the editor, with args before the file, doing one step per run. tent edit gets
// a temporary directory of its own.
func newFakeEditor(t *testing.T, args string, steps ...editStep) fakeEditor {
	t.Helper()
	e := fakeEditor{dir: t.TempDir(), tmp: t.TempDir()}
	data, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "steps.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TENT_EDITOR", `"`+exe+`" -test.run=^TestEditorHelper$ --`+args)
	t.Setenv(editorDirEnv, e.dir)
	t.Setenv("GORACE", "atexit_sleep_ms=0") // a race-enabled editor would wait a second before it exits
	for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(env, e.tmp)
	}
	return e
}

// TestEditorHelper is the fake editor of the edit tests, not a test: they run the test binary again as the editor.
func TestEditorHelper(t *testing.T) {
	dir := os.Getenv(editorDirEnv)
	if dir == "" {
		t.Skip("the fake editor of the edit tests")
	}
	code, err := fakeEdit(dir, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake editor:", err)
		code = 3
	}
	os.Exit(code) // before the test binary writes PASS to the output of tent
}

// fakeEdit records its run in dir, does the step for it on the file, the last of args, and returns the step's exit
// code.
func fakeEdit(dir string, args []string) (int, error) {
	data, err := os.ReadFile(filepath.Join(dir, "steps.json"))
	if err != nil {
		return 0, err
	}
	var steps []editStep
	if err := json.Unmarshal(data, &steps); err != nil {
		return 0, err
	}
	n := 1
	for fileExists(runFile(dir, n)) {
		n++
	}
	if n > len(steps) || len(args) == 0 {
		return 0, fmt.Errorf("run %d with arguments %q has no step", n, args)
	}
	path := args[len(args)-1]
	if data, err = os.ReadFile(path); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	step := steps[n-1]
	var line string
	if step.ReadLine {
		if line, err = readLine(os.Stdin); err != nil {
			return 0, err
		}
	}
	record, err := json.Marshal(editorRun{Args: args, Content: string(data), Mode: info.Mode().Perm(), Stdin: line})
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(runFile(dir, n), record, 0o600); err != nil {
		return 0, err
	}
	if step.Print != "" {
		fmt.Println(step.Print)
		fmt.Fprintln(os.Stderr, step.Print)
	}
	if step.Wait {
		if err := waitForGo(dir); err != nil {
			return 0, err
		}
	}
	if p := step.Put; p != nil {
		store, err := statestore.Open(context.Background(), p.URL)
		if err != nil {
			return 0, err
		}
		if _, err := store.Put(context.Background(), p.Path, []byte(p.Data), statestore.PutOptions{}); err != nil {
			return 0, err
		}
	}
	switch {
	case step.Content != nil:
		data = []byte(*step.Content)
	case step.Old != "":
		if !bytes.Contains(data, []byte(step.Old)) {
			return 0, fmt.Errorf("%q is not in the file:\n%s", step.Old, data)
		}
		data = bytes.Replace(data, []byte(step.Old), []byte(step.New), 1)
	}
	if step.CRLF {
		data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
	}
	if step.BOM {
		data = append([]byte("\ufeff"), data...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return 0, err
	}
	if step.Lock {
		if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
			return 0, err
		}
	}
	return step.Exit, nil
}

// readLine reads r up to a line end, one byte at a time, so that what follows stays for the next reader of the file.
func readLine(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 && b[0] != '\n' {
			line = append(line, b[0])
		}
		switch {
		case n == 1 && b[0] == '\n', errors.Is(err, io.EOF):
			return string(line), nil
		case err != nil:
			return "", err
		}
	}
}

// waitForGo creates the file waiting in dir and waits for the file go.
func waitForGo(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "waiting"), nil, 0o600); err != nil {
		return err
	}
	for deadline := time.Now().Add(time.Minute); !fileExists(filepath.Join(dir, "go")); {
		if time.Now().After(deadline) {
			return errors.New("the file go did not come")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func runFile(dir string, n int) string { return filepath.Join(dir, fmt.Sprintf("run-%d.json", n)) }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// wantRuns fails the test unless the editor ran once for each of contents, given that content in the file tent edit
// made for it.
func (e fakeEditor) wantRuns(t *testing.T, contents ...string) []editorRun {
	t.Helper()
	var runs []editorRun
	for n := 1; fileExists(runFile(e.dir, n)); n++ {
		data, err := os.ReadFile(runFile(e.dir, n))
		if err != nil {
			t.Fatal(err)
		}
		var r editorRun
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	var got []string
	for i, r := range runs {
		got = append(got, r.Content)
		file := r.Args[len(r.Args)-1]
		if ok, _ := filepath.Match("tent-edit-*.yaml", filepath.Base(file)); !ok || filepath.Dir(file) != e.tmp {
			t.Errorf("run %d edited %s, want a file tent-edit-*.yaml in %s", i+1, file, e.tmp)
		}
	}
	if diff := cmp.Diff(contents, got); diff != "" {
		t.Errorf("what the editor was given (-want +got):\n%s", diff)
	}
	return runs
}

// files returns the files that tent edit left in its temporary directory.
func (e fakeEditor) files(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(e.tmp, "tent-edit-*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// wantNoFile fails the test unless tent edit removed its file.
func (e fakeEditor) wantNoFile(t *testing.T) {
	t.Helper()
	if files := e.files(t); len(files) > 0 {
		t.Errorf("tent edit left %q", files)
	}
}

// runEdit executes tent with args. Its stdin is a file that holds stdin, as a terminal is a file: the editor
// inherits it and leaves the answer to tent.
func runEdit(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return waitBounded(t, startEdit(t.Context(), t, stdinFile(t, stdin), args...))
}

// waitBounded waits for tent to exit and returns what it did. It fails the test after a minute.
func waitBounded(t *testing.T, r *started) result {
	t.Helper()
	select {
	case code := <-r.code:
		return result{code, r.out.String(), r.errOut.String()}
	case <-time.After(time.Minute):
		t.Fatalf("tent did not exit in a minute; stdout %q, stderr %q", r.out.String(), r.errOut.String())
		return result{}
	}
}

// stdinFile returns a file open for reading that holds content.
func stdinFile(t *testing.T, content string) *os.File {
	t.Helper()
	in, err := os.Open(writeFile(t, "stdin", content))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })
	return in
}

// startEdit starts tent with args, ctx and stdin in another goroutine.
func startEdit(ctx context.Context, t *testing.T, stdin *os.File, args ...string) *started {
	t.Helper()
	r := &started{code: make(chan int, 1)}
	go func() { r.code <- executeTest(ctx, t, args, Streams{In: stdin, Out: &r.out, Err: &r.errOut}) }()
	return r
}

// waitUntil waits until done reports true, and fails the test after a minute.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !done(); {
		if time.Now().After(deadline) {
			t.Fatalf("waited a minute for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// biggerWorkersDiff is the diff of the workers group of the test cluster to biggerWorkers.
const biggerWorkersDiff = `--- stored
+++ edited
@@ -6,4 +6,4 @@
 spec:
   role: client
   machineType: vc2-2c-4gb
-  size: 3
+  size: 5
`

func TestEditNodeGroup(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n", "Save? [y/N] "+openAPIWarning)
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
	runs := e.wantRuns(t, workersYAML)
	if runtime.GOOS != "windows" && runs[0].Mode != 0o600 {
		t.Errorf("the file has mode %v, want only its owner to read and write it", runs[0].Mode)
	}
	e.wantNoFile(t)
}

func TestEditClusterWithYes(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "region: ams", New: "region: fra"})
	wantDone(t, runEdit(t, "", "edit", "cluster", "prod", "--yes", "--state", s.url), `--- stored
+++ edited
@@ -5,5 +5,5 @@
 spec:
   cloud:
     provider: vultr
-    region: ams
+    region: fra
     vultr: {}
cluster prod replaced
`)
	s.want(t, map[string]string{clusterPath: replaced(t, clusterYAML, "ams", "fra"), serversPath: serversYAML,
		workersPath: workersYAML})
	e.wantRuns(t, clusterYAML)
	e.wantNoFile(t)
}

func TestEditAnswers(t *testing.T) {
	for _, tc := range []struct {
		answer string
		saved  bool
	}{
		{"y\n", true},
		{" YES \n", true},
		{"yes", true},
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"yep\n", false},
	} {
		t.Run(fmt.Sprintf("%q", tc.answer), func(t *testing.T) {
			s := withCluster(t)
			e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
			got := runEdit(t, tc.answer, "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
			if tc.saved {
				wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n",
					"Save? [y/N] "+openAPIWarning)
				s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML,
					workersPath: biggerWorkers(t)})
			} else {
				wantResult(t, got, 0, biggerWorkersDiff+"not saved\n", "Save? [y/N] ")
				s.want(t, prodObjects)
			}
			e.wantNoFile(t)
		})
	}
}

func TestEditCancelled(t *testing.T) {
	for _, tc := range []struct {
		name string
		step editStep
	}{
		{"unchanged", editStep{}},
		{"empty", editStep{Content: new("")}},
		{"blank", editStep{Content: new(" \n\n")}},
		{"only comments", editStep{Content: new("# nothing\n---\n")}},
		{"a comment added", editStep{Old: "spec:", New: "# bigger soon\nspec:"}},
		{"keys in another order", editStep{Old: "  role: client\n  machineType: vc2-2c-4gb\n",
			New: "  machineType: vc2-2c-4gb\n  role: client\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withCluster(t)
			e := newFakeEditor(t, "", tc.step)
			wantOK(t, runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url),
				"edit cancelled; nothing changed\n")
			s.want(t, prodObjects)
			e.wantRuns(t, workersYAML)
			e.wantNoFile(t)
		})
	}
}

// fixErrorsHeader is the first line of the comments that show the errors of an edit.
const fixErrorsHeader = "# Please fix the errors below and save, or save it unchanged to cancel; your edit is kept.\n"

func TestEditReopensOnADecodeError(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: [3"}, editStep{Old: "size: [3", New: "size: 5"})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n", "Save? [y/N] "+openAPIWarning)
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
	// The line is that of the file with the header: the spec alone has the error on line 8.
	e.wantRuns(t, workersYAML, fixErrorsHeader+
		"# document 1: yaml: line 10: did not find expected ',' or ']'\n"+
		replaced(t, workersYAML, "size: 3", "size: [3"))
	e.wantNoFile(t)
}

func TestEditReopensOnAnInvalidSpec(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 2"}, editStep{Old: "size: 2", New: "size: 5"})
	got := runEdit(t, "y\n", "edit", "nodegroup", "servers", "--name", "prod", "--state", s.url)
	wantResult(t, got, 0, `--- stored
+++ edited
@@ -6,4 +6,4 @@
 spec:
   role: server
   machineType: vc2-2c-4gb
-  size: 3
+  size: 5
node group servers replaced
`, "Save? [y/N] "+openAPIWarning)
	biggerServers := replaced(t, serversYAML, "size: 3", "size: 5")
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: biggerServers, workersPath: workersYAML})
	e.wantRuns(t, serversYAML, fixErrorsHeader+
		"# NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n"+
		replaced(t, serversYAML, "size: 3", "size: 2"))
	e.wantNoFile(t)
}

// sizeeHeader is the header that shows the error of sizeeWorkers.
const sizeeHeader = fixErrorsHeader + "# document 1 (NodeGroup): line 11: unknown field \"spec.sizee\"\n"

// sizeeWorkers is the workers group of the test cluster with a field misspelt.
func sizeeWorkers(t *testing.T) string { return replaced(t, workersYAML, "size: 3", "sizee: 3") }

// TestEditCancelledWithErrors keeps the operator's text, which was never saved.
func TestEditCancelledWithErrors(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, editStep{})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, sizeeHeader+sizeeWorkers(t))
	wantOK(t, got, "edit cancelled; nothing saved; your edit is in "+kept+"\n")
	s.want(t, prodObjects)
	e.wantRuns(t, workersYAML, sizeeHeader+sizeeWorkers(t))
}

// TestEditRevertedAfterErrors removes the file, which holds the stored spec again.
func TestEditRevertedAfterErrors(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, editStep{Content: new(workersYAML)})
	wantOK(t, runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url),
		"edit cancelled; nothing changed\n")
	s.want(t, prodObjects)
	e.wantNoFile(t)
}

// wantKept fails the test unless tent edit kept its file with content, and returns the file's path.
func (e fakeEditor) wantKept(t *testing.T, content string) string {
	t.Helper()
	files := e.files(t)
	if len(files) != 1 {
		t.Fatalf("tent edit left %q, want one file", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(content, string(data)); diff != "" {
		t.Errorf("the file kept (-want +got):\n%s", diff)
	}
	return files[0]
}

func TestEditRename(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "name: workers", New: "name: web"})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, replaced(t, workersYAML, "name: workers", "name: web"))
	wantError(t, got, "Error: edit cannot rename; the spec must still be node group workers of cluster prod\n"+
		"  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

func TestEditChangedMeanwhile(t *testing.T) {
	s := withCluster(t)
	meanwhile := replaced(t, workersYAML, "size: 3", "size: 4")
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5",
		Put: &storePut{URL: s.url, Path: workersPath, Data: meanwhile}})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, biggerWorkers(t))
	wantError(t, got, "Error: node group workers of cluster prod changed while you edited it; run edit again\n"+
		"  your edit is in "+kept+"\n")
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: meanwhile})
}

func TestEditLocked(t *testing.T) {
	s := withCluster(t)
	held := holdLock(t, s)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	got := runEdit(t, "", "edit", "nodegroup", "workers", "--yes", "--lock-timeout", "0s", "--name", "prod",
		"--state", s.url)
	kept := e.wantKept(t, biggerWorkers(t))
	locked := (&statestore.LockedError{Cluster: "prod", Holder: held.Lease()}).Error()
	wantResult(t, got, 1, biggerWorkersDiff, "Error: "+locked+"\n  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

func TestEditorFails(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Exit: 1})
	wantError(t, runEdit(t, "y\n", "edit", "cluster", "prod", "--state", s.url),
		"Error: the editor failed: exit status 1\n")
	s.want(t, prodObjects)
	e.wantRuns(t, clusterYAML)
	e.wantNoFile(t)
}

func TestEditorFailsAfterErrors(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, editStep{Exit: 1})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, fixErrorsHeader+"# document 1 (NodeGroup): line 11: unknown field \"spec.sizee\"\n"+
		replaced(t, workersYAML, "size: 3", "sizee: 3"))
	wantError(t, got, "Error: the editor failed: exit status 1\n  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

func TestEditWarnsWhenItCannotRemoveTheFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory keeps its files only on Unix, and not from root")
	}
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Lock: true})
	t.Cleanup(func() { _ = os.Chmod(e.tmp, 0o700) }) // before the directory is removed
	got := runEdit(t, "", "edit", "nodegroup", "workers", "--yes", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, biggerWorkers(t))
	wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n",
		openAPIWarning+"WARNING: could not remove the edited file: remove "+kept+": permission denied\n")
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
}

func TestEditorArguments(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, ` --wait "a b"`, editStep{Print: "the editor is running"})
	// The editor writes on the streams of tent, as a terminal editor needs.
	wantResult(t, runEdit(t, "", "edit", "cluster", "prod", "--state", s.url), 0,
		"the editor is running\nedit cancelled; nothing changed\n", "the editor is running\n")
	runs := e.wantRuns(t, clusterYAML)
	if diff := cmp.Diff([]string{"--wait", "a b"}, runs[0].Args[:len(runs[0].Args)-1]); diff != "" {
		t.Errorf("the editor's arguments before the file (-want +got):\n%s", diff)
	}
}

func TestEditNodeGroupOfTENTCLUSTER(t *testing.T) {
	s := withCluster(t)
	t.Setenv(envCluster, "prod")
	newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	wantDone(t, runEdit(t, "", "edit", "nodegroup", "workers", "--yes", "--state", s.url),
		biggerWorkersDiff+"node group workers replaced\n")
}

func TestEditAllowSingleServer(t *testing.T) {
	s := withCluster(t)
	newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 1"})
	got := runEdit(t, "", "edit", "nodegroup", "servers", "--yes", "--allow-single-server", "--name", "prod",
		"--state", s.url)
	if got.code != 0 || !strings.HasSuffix(got.out, "+  size: 1\nnode group servers replaced\n") {
		t.Errorf("edit returned %d, stdout %q, stderr %q; want the servers saved", got.code, got.out, got.errOut)
	}
}

func TestEditRefusesBeforeTheEditor(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"edit", "nodegroup", "web", "--name", "prod"},
			"Error: node group web of cluster prod not found in " + s.url + "\n"},
		{[]string{"edit", "cluster", "dev"}, "Error: cluster dev not found in " + s.url + "\n"},
		{[]string{"edit", "nodegroup", "--name", "prod"}, "Error: accepts 1 arg(s), received 0\n"},
		{[]string{"edit", "cluster", "prod", "dev"}, "Error: accepts at most 1 arg(s), received 2\n"},
	} {
		wantError(t, runEdit(t, "", append(tc.args, "--state", s.url)...), tc.want)
	}
	e.wantRuns(t)
	e.wantNoFile(t)
}

func TestEditRefusesMachineOutput(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "")
	commands := [][]string{{"edit", "cluster", "prod"}, {"edit", "nodegroup", "workers", "--name", "prod"}}
	for _, format := range []string{"json", "yaml"} {
		for _, args := range commands {
			wantError(t, runEdit(t, "y\n", append(args, "-o", format, "--state", s.url)...),
				"Error: edit is interactive and prints text; -o json and -o yaml are not supported\n")
		}
	}
	s.want(t, prodObjects)
	e.wantRuns(t)
	e.wantNoFile(t)
}

func TestEditWithANarrowAPIDoesNotWarn(t *testing.T) {
	s := withCluster(t)
	narrow := "    vultr: {}\n  access:\n    api:\n      - 10.0.0.0/8\n"
	e := newFakeEditor(t, "", editStep{Old: "    vultr: {}\n", New: narrow})
	wantOK(t, runEdit(t, "", "edit", "cluster", "prod", "--yes", "--state", s.url), `--- stored
+++ edited
@@ -7,3 +7,6 @@
     provider: vultr
     region: ams
     vultr: {}
+  access:
+    api:
+      - 10.0.0.0/8
cluster prod replaced
`)
	e.wantNoFile(t)
}

func TestEditNames(t *testing.T) {
	s := withCluster(t)
	path := configFile(t)
	e := newFakeEditor(t, "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"edit", "clustr", "prod"}, `unknown command "clustr" for "tent edit"`},
		{[]string{"edit", "cluster"}, `no cluster name: give NAME or set --name, TENT_CLUSTER or "cluster" in ` + path},
		{[]string{"edit", "nodegroup", "workers"}, `no cluster name: set --name, TENT_CLUSTER or "cluster" in ` + path},
		{[]string{"edit", "cluster", "prod", "--name", "dev"}, "NAME prod and --name dev differ; give one"},
		{[]string{"edit", "cluster", "PROD"}, `invalid cluster name "PROD": must be 2 to 20 lowercase letters, ` +
			"digits or dashes, starting with a letter and ending with a letter or digit"},
	} {
		wantError(t, runEdit(t, "", append(tc.args, "--state", s.url)...), "Error: "+tc.want+"\n")
	}
	s.want(t, prodObjects)
	e.wantRuns(t)
	e.wantNoFile(t)
}

// marks are the ways an editor may save a file other than tent wrote it: the line ends and a byte order mark.
var marks = []struct {
	name string
	step editStep
	file func(string) string // the file that the step leaves
}{
	{"CRLF", editStep{CRLF: true}, func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
	{"BOM", editStep{BOM: true}, func(s string) string { return "\ufeff" + s }},
}

func TestEditIgnoresLineEndsAndByteOrderMarks(t *testing.T) {
	for _, m := range marks {
		t.Run(m.name+" unchanged", func(t *testing.T) {
			s := withCluster(t)
			e := newFakeEditor(t, "", m.step)
			wantOK(t, runEdit(t, "", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url),
				"edit cancelled; nothing changed\n")
			e.wantNoFile(t)
		})
		t.Run(m.name+" cancelled after errors", func(t *testing.T) {
			s := withCluster(t)
			e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, m.step)
			got := runEdit(t, "", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
			kept := e.wantKept(t, m.file(sizeeHeader+sizeeWorkers(t)))
			wantOK(t, got, "edit cancelled; nothing saved; your edit is in "+kept+"\n")
			s.want(t, prodObjects)
		})
		t.Run(m.name+" fixed after errors", func(t *testing.T) {
			s := withCluster(t)
			fix := m.step
			fix.Old, fix.New = "sizee: 3", "size: 5"
			e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, fix)
			got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
			wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n", "Save? [y/N] "+openAPIWarning)
			s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML,
				workersPath: biggerWorkers(t)})
			e.wantNoFile(t)
		})
	}
}

// TestEditIgnoresTheOutputOfTheConfigFile, a default for the commands that print data.
func TestEditIgnoresTheOutputOfTheConfigFile(t *testing.T) {
	writeConfig(t, "output: json\n")
	s := withCluster(t)
	newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"}, editStep{})
	wantDone(t, runEdit(t, "", "edit", "nodegroup", "workers", "--yes", "--name", "prod", "--state", s.url),
		biggerWorkersDiff+"node group workers replaced\n")
	wantOK(t, runEdit(t, "", "edit", "nodegroup", "workers", "-o", "table", "--name", "prod", "--state", s.url),
		"edit cancelled; nothing changed\n")
}

func TestEditInterruptedAtTheQuestion(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	in, noAnswer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = noAnswer.Close(), in.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := startEdit(ctx, t, in, "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	waitUntil(t, "the question", func() bool { return r.errOut.String() == "Save? [y/N] " })
	cancel()
	got := waitBounded(t, r)
	kept := e.wantKept(t, biggerWorkers(t))
	wantResult(t, got, 1, biggerWorkersDiff,
		"Save? [y/N] \nError: interrupted; nothing saved\n  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

// TestEditInterruptedWhileTheEditorRuns waits for the editor, which it does not kill, and keeps what it wrote.
func TestEditInterruptedWhileTheEditorRuns(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Wait: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := startEdit(ctx, t, stdinFile(t, "y\n"), "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	waitUntil(t, "the editor", func() bool { return fileExists(filepath.Join(e.dir, "waiting")) })
	cancel()
	if err := os.WriteFile(filepath.Join(e.dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := waitBounded(t, r)
	kept := e.wantKept(t, biggerWorkers(t))
	wantError(t, got, "Error: interrupted; nothing saved\n  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

func TestEditorFailsAfterAChange(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Exit: 1})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, biggerWorkers(t))
	wantError(t, got, "Error: the editor failed: exit status 1\n  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

func TestEditSeveralObjects(t *testing.T) {
	s := withCluster(t)
	two := "size: 5\n---\n" + serversYAML
	e := newFakeEditor(t, "", editStep{Old: "size: 3\n", New: two})
	got := runEdit(t, "y\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	kept := e.wantKept(t, replaced(t, workersYAML, "size: 3\n", two))
	wantError(t, got, "Error: edit saves one Cluster or one NodeGroup; the spec holds 2 objects\n"+
		"  your edit is in "+kept+"\n")
	s.want(t, prodObjects)
}

// losingPutStore removes the lock's lease after it writes one path, as state unlock --force by someone else would.
type losingPutStore struct {
	statestore.Store
	after string
}

func (s losingPutStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	v, err := s.Store.Put(ctx, p, data, opts)
	if err == nil && p == s.after {
		err = s.Delete(ctx, "prod/lock")
	}
	return v, err
}

// TestEditSavedButTheLockLost prints what it saved, and fails.
func TestEditSavedButTheLockLost(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	losing := func(st statestore.Store) statestore.Store { return losingPutStore{Store: st, after: workersPath} }
	got := runWithStore(t, losing, "edit", "nodegroup", "workers", "--yes", "--name", "prod", "--state", s.url)
	wantResult(t, got, 1, biggerWorkersDiff+"node group workers replaced\n", openAPIWarning+
		"Error: the change is saved, but the lock of cluster prod was lost before tent released it\n")
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
	e.wantNoFile(t)
}

func TestEditHelp(t *testing.T) {
	const waits = "The editor must not return until the file is closed, as with code --wait."
	for _, tc := range []struct{ command, want string }{
		{"cluster", waits},
		{"nodegroup", waits},
		{"nodegroup", "Edit the node group NAME of the cluster named by --name, TENT_CLUSTER or the config file"},
	} {
		got := runIn(t, "", "edit", tc.command, "--help")
		if got.code != 0 || !strings.Contains(got.out, tc.want) {
			t.Errorf("tent edit %s --help: exit code %d, stdout\n%s\nwant 0 and it to hold %q", tc.command, got.code,
				got.out, tc.want)
		}
	}
}

func TestEditEmptiedAfterErrors(t *testing.T) {
	for _, empty := range []string{"", " \n\n"} {
		s := withCluster(t)
		e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "sizee: 3"}, editStep{Content: new(empty)})
		wantOK(t, runEdit(t, "", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url),
			"edit cancelled; nothing changed\n")
		e.wantNoFile(t)
	}
}

// TestEditorReadsStdin, which tent reads the answer from after the editor exits.
func TestEditorReadsStdin(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", ReadLine: true})
	got := runEdit(t, "for the editor\ny\n", "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	wantResult(t, got, 0, biggerWorkersDiff+"node group workers replaced\n", "Save? [y/N] "+openAPIWarning)
	if runs := e.wantRuns(t, workersYAML); runs[0].Stdin != "for the editor" {
		t.Errorf("the editor read %q from stdin, want the line for it", runs[0].Stdin)
	}
}

// TestEditInterruptedWhileTheEditorFails says that it was interrupted: the editor fails for that.
func TestEditInterruptedWhileTheEditorFails(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Wait: true, Exit: 1})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := startEdit(ctx, t, stdinFile(t, "y\n"), "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	waitUntil(t, "the editor", func() bool { return fileExists(filepath.Join(e.dir, "waiting")) })
	cancel()
	if err := os.WriteFile(filepath.Join(e.dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := waitBounded(t, r)
	kept := e.wantKept(t, biggerWorkers(t))
	wantError(t, got, "Error: interrupted; nothing saved\n  your edit is in "+kept+"\n")
}
