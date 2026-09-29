package nodeuptest_test

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// The fake is a nodeup.FS.
var _ nodeup.FS = (*nodeuptest.FS)(nil)

func TestFSStartsWithTheRootAlone(t *testing.T) {
	f := nodeuptest.NewFS()
	root, ok := f.Entry("/")
	if want := (nodeuptest.Entry{Dir: true, Mode: 0o755, Owner: "root:root"}); !ok || !cmp.Equal(root, want) {
		t.Errorf("Entry(/) = %+v, %v; want %+v", root, ok, want)
	}
	if got := f.Paths(); !cmp.Equal(got, []string{"/"}) {
		t.Errorf("Paths() = %q, want only /", got)
	}
}

func TestFSAddMakesParentsWithoutChanges(t *testing.T) {
	f := nodeuptest.NewFS()
	f.AddFile(t, "/etc/systemd/system/a.service", []byte("[Unit]\n"), 0o644, "root:root")
	f.AddDir(t, "/var/lib/nomad/client", 0o700, "root:root")
	want := map[string]nodeuptest.Entry{
		"/":                             {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/etc":                          {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/etc/systemd":                  {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/etc/systemd/system":           {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/etc/systemd/system/a.service": {Data: []byte("[Unit]\n"), Mode: 0o644, Owner: "root:root"},
		"/var":                          {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/var/lib":                      {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/var/lib/nomad":                {Dir: true, Mode: 0o755, Owner: "root:root"},
		"/var/lib/nomad/client":         {Dir: true, Mode: 0o700, Owner: "root:root"},
	}
	for _, p := range f.Paths() {
		got, _ := f.Entry(p)
		if diff := cmp.Diff(want[p], got); diff != "" {
			t.Errorf("Entry(%s) (-want +got):\n%s", p, diff)
		}
		delete(want, p)
	}
	if len(want) > 0 {
		t.Errorf("missing entries: %v", want)
	}
	if got := f.Changes(); len(got) != 0 {
		t.Errorf("Changes() = %q after AddFile and AddDir, want none", got)
	}
}

func TestFSRecordsChanges(t *testing.T) {
	f := nodeuptest.NewFS()
	f.AddFile(t, "/etc/old.conf", []byte("old\n"), 0o644, "root:root")
	steps := []func() (bool, error){
		func() (bool, error) { return f.EnsureDir("/etc/tent", 0o700, "root:root") },
		func() (bool, error) { return f.EnsureDir("/etc/tent", 0o700, "root:root") },
		func() (bool, error) { return f.WriteFile("/etc/tent/a", []byte("a"), 0o600, "root:root") },
		func() (bool, error) { return f.WriteFile("/etc/tent/a", []byte("a"), 0o600, "root:root") },
		func() (bool, error) { return f.WriteFile("/etc/tent/a", []byte("a"), 0o600, "nomad:nomad") },
		func() (bool, error) { return f.Remove("/etc/old.conf") },
		func() (bool, error) { return f.Remove("/etc/old.conf") },
	}
	for i, step := range steps {
		if _, err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	want := []string{"/etc/tent", "/etc/tent/a", "/etc/tent/a", "/etc/old.conf"}
	if diff := cmp.Diff(want, f.Changes()); diff != "" {
		t.Errorf("Changes() (-want +got):\n%s", diff)
	}
	if a, _ := f.Entry("/etc/tent/a"); a.Owner != "nomad:nomad" {
		t.Errorf("the owner of /etc/tent/a is %s, want nomad:nomad", a.Owner)
	}
}

func TestFSKeepsCopies(t *testing.T) {
	f := nodeuptest.NewFS()
	data := []byte("one")
	if _, err := f.WriteFile("/a", data, 0o644, "root:root"); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	got, err := f.ReadFile("/a")
	if err != nil {
		t.Fatal(err)
	}
	got[1] = 'X'
	entry, _ := f.Entry("/a")
	entry.Data[2] = 'X'
	if again, _ := f.ReadFile("/a"); string(again) != "one" {
		t.Errorf("the file holds %q after its caller changed the slices, want %q", again, "one")
	}
}

func TestFSFail(t *testing.T) {
	f := nodeuptest.NewFS()
	f.AddFile(t, "/etc/a", []byte("a"), 0o644, "root:root")
	boom := errors.New("disk full")
	for _, path := range []string{"/etc/a", "/etc/d"} {
		f.Fail(path, boom)
	}
	calls := map[string]func() (bool, error){
		"WriteFile": func() (bool, error) { return f.WriteFile("/etc/a", []byte("b"), 0o644, "root:root") },
		"EnsureDir": func() (bool, error) { return f.EnsureDir("/etc/d", 0o755, "root:root") },
		"Remove":    func() (bool, error) { return f.Remove("/etc/a") },
	}
	for name, call := range calls {
		if changed, err := call(); !errors.Is(err, boom) || changed {
			t.Errorf("%s of a failing path: changed %v, err %v; want an error that matches the fault", name, changed,
				err)
		}
	}
	if got, _ := f.ReadFile("/etc/a"); string(got) != "a" {
		t.Errorf("/etc/a holds %q after the failed calls, want %q", got, "a")
	}
	if len(f.Changes()) != 0 {
		t.Errorf("Changes() = %q after failed calls, want none", f.Changes())
	}
	f.Fail("/etc/a", nil)
	if changed, err := f.WriteFile("/etc/a", []byte("b"), 0o644, "root:root"); err != nil || !changed {
		t.Errorf("WriteFile after the fault was cleared: changed %v, err %v", changed, err)
	}
}

func TestFSStat(t *testing.T) {
	f := nodeuptest.NewFS()
	f.AddFile(t, "/etc/a.conf", []byte("abc"), 0o640, "root:root")
	fi, err := f.Stat("/etc/a.conf")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != "a.conf" || fi.Size() != 3 || fi.Mode() != 0o640 || fi.IsDir() || !fi.ModTime().IsZero() {
		t.Errorf("Stat = %s %d %v %v %v, want a.conf 3 -rw-r----- a file", fi.Name(), fi.Size(), fi.Mode(), fi.IsDir(),
			fi.ModTime())
	}
	dir, err := f.Stat("/etc")
	if err != nil {
		t.Fatal(err)
	}
	if dir.Name() != "etc" || !dir.IsDir() || dir.Mode() != fs.ModeDir|0o755 {
		t.Errorf("Stat(/etc) = %s %v %v, want etc, a directory, drwxr-xr-x", dir.Name(), dir.IsDir(), dir.Mode())
	}
}

// fatalRecorder records the message of Fatalf instead of stopping the test.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.msg = fmt.Sprintf(format, args...) }

func TestFSAddRefusesTheOtherKind(t *testing.T) {
	f := nodeuptest.NewFS()
	f.AddFile(t, "/etc/a", nil, 0o644, "root:root")
	f.AddDir(t, "/etc/d", 0o755, "root:root")
	cases := []struct {
		name string
		add  func(testing.TB)
		want string
	}{
		{"a directory over a file", func(tb testing.TB) { f.AddDir(tb, "/etc/a", 0o755, "root:root") },
			"nodeuptest: add /etc/a: a file is there"},
		{"a file over a directory", func(tb testing.TB) { f.AddFile(tb, "/etc/d", nil, 0o644, "root:root") },
			"nodeuptest: add /etc/d: a directory is there"},
		{"a file in a file", func(tb testing.TB) { f.AddFile(tb, "/etc/a/b", nil, 0o644, "root:root") },
			"nodeuptest: add /etc/a/b: /etc/a is a file"},
	}
	for _, c := range cases {
		r := &fatalRecorder{TB: t}
		c.add(r)
		if r.msg != c.want {
			t.Errorf("%s: stopped with %q, want %q", c.name, r.msg, c.want)
		}
	}
	for path, dir := range map[string]bool{"/etc/a": false, "/etc/d": true} {
		if e, _ := f.Entry(path); e.Dir != dir {
			t.Errorf("%s is a directory %v after the refusals, want %v", path, e.Dir, dir)
		}
	}
	if _, ok := f.Entry("/etc/a/b"); ok {
		t.Error("/etc/a/b exists after the refusal")
	}
}
