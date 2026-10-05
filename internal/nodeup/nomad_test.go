package nodeup_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// nomadBinary is the content of the tests' Nomad binary.
var nomadBinary = []byte("#!/bin/sh\n# the nomad binary of the tests\n")

// nomadZip returns a zip of nomadBinary as HashiCorp releases Nomad, with LICENSE.txt before the binary.
func nomadZip(t *testing.T) []byte {
	t.Helper()
	return nodeuptest.NomadZip(t, nomadBinary)
}

// nomadAssetPath is where the tests serve the Nomad zip.
const nomadAssetPath = "/nomad_2.0.7_linux_amd64.zip"

// restartFile marks a change that Nomad has not read yet.
const restartFile = "/var/lib/tent/nomad-restart"

// nomadUnits are the units of a machine that runs the nomad phase.
var nomadUnits = []string{"tent-node.service", "tent-node-join.service", "tent-node-join.timer", "nomad.service"}

// nomadMachine returns a fake Ubuntu machine that knows nomad.service, with the instance that preflight would read.
func nomadMachine(t *testing.T) (*nodeup.Host, *nodeuptest.FS, *nodeuptest.Runner) {
	t.Helper()
	h, fsys, r := machine(t)
	nodeuptest.Ubuntu(t, fsys, r, nomadUnits...)
	h.Instance = instance
	return h, fsys, r
}

// withNomad serves the zip at url, gives h a client that trusts the server, and points nc's nomad asset at the
// server.
func withNomad(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig, zip []byte) *nodeuptest.Server {
	t.Helper()
	srv := serve(t, h, map[string]http.HandlerFunc{nomadAssetPath: file(zip)})
	i := slices.IndexFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.NomadAsset })
	if i < 0 {
		t.Fatal("the config has no nomad asset")
	}
	nc.Assets[i].URLs, nc.Assets[i].SHA256 = []string{srv.URL + nomadAssetPath}, sum(zip)
	rehash(t, nc)
	return srv
}

// ranNomad returns the commands of r that touch nomad.service.
func ranNomad(r *nodeuptest.Runner) []string {
	var got []string
	for _, c := range r.Commands() {
		if strings.Contains(c, "nomad.service") || c == "systemctl daemon-reload" {
			got = append(got, c)
		}
	}
	return got
}

func TestNomadInstalls(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	srv := withNomad(t, h, nc, nomadZip(t))

	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("nomad = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	bin, ok := fsys.Entry(nodeconfig.NomadBinary)
	if !ok || !bytes.Equal(bin.Data, nomadBinary) || bin.Mode != 0o755 || bin.Owner != nodeconfig.Owner {
		t.Errorf("the nomad binary: found %v, %d bytes, mode %#o, owner %s", ok, len(bin.Data), bin.Mode, bin.Owner)
	}
	for path, mode := range map[string]fs.FileMode{"/var/lib/nomad": 0o755, "/var/lib/nomad/client": 0o700} {
		e, ok := fsys.Entry(path)
		if !ok || !e.Dir || e.Mode != mode {
			t.Errorf("%s: found %v, dir %v, mode %#o; want a directory with %#o", path, ok, e.Dir, e.Mode, mode)
		}
	}
	for _, path := range []string{"/etc/nomad.d/00-tent.hcl", "/etc/nomad.d/01-gossip.hcl", nodeconfig.KeyFile,
		nodeconfig.NomadServiceFile} {
		if _, ok := fsys.Entry(path); !ok {
			t.Errorf("%s was not written", path)
		}
	}
	e, ok := fsys.Entry("/etc/nomad.d/11-instance.hcl")
	if !ok || !strings.Contains(string(e.Data), instance.ID) {
		t.Errorf("11-instance.hcl does not carry the instance id: %v", e.Data)
	}
	// The zip's licence is not written; the restart marker goes once Nomad has started.
	for _, p := range fsys.Paths() {
		if strings.Contains(p, "LICENSE") || p == restartFile {
			t.Errorf("%s is on the machine", p)
		}
	}
	// systemd reads a unit it has not read before when first asked, so the first run needs no reload.
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive, nomadStart}, ranNomad(r)); diff != "" {
		t.Errorf("the commands for nomad.service (-want +got):\n%s", diff)
	}
	if got := srv.Requests(); !cmp.Equal(got, []string{"/nomad_2.0.7_linux_amd64.zip"}) {
		t.Errorf("downloads %q, want the zip once", got)
	}
}

func TestNomadSecondRun(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	srv := withNomad(t, h, nc, nomadZip(t))
	if _, err := runPhase(t, "nomad", h, nc, nil); err != nil {
		t.Fatal(err)
	}
	firstRun := len(ranNomad(r))
	changes, reads := len(fsys.Changes()), len(fsys.Reads())

	// The binary is compared, not written again: a write of it would fail.
	fsys.Fail(nodeconfig.NomadBinary, errors.New("the binary was written again"))
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Unchanged {
		t.Errorf("the second nomad = %s %q, %v; want unchanged", res.Status, res.Reason, err)
	}
	// The cached zip is checked and read as a stream: no whole file goes into memory.
	if got := fsys.Reads()[reads:]; slices.Contains(got, "/var/lib/tent/assets/nomad") {
		t.Errorf("the second run read %q whole, want no cached zip among them", got)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second run changed %q, want nothing", got)
	}
	// No start, no restart, no reload: Nomad is not interrupted for nothing.
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive}, ranNomad(r)[firstRun:]); diff != "" {
		t.Errorf("the second run's commands for nomad.service (-want +got):\n%s", diff)
	}
	if got := srv.Requests(); len(got) != 1 {
		t.Errorf("the second run downloaded again: %q", got)
	}
}

// nomadInstalled returns a machine on which the nomad phase has run once for the combined config, which it returns
// too.
func nomadInstalled(t *testing.T) (*nodeup.Host, *nodeuptest.FS, *nodeuptest.Runner, *nodeconfig.NodeConfig) {
	t.Helper()
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	withNomad(t, h, nc, nomadZip(t))
	if _, err := runPhase(t, "nomad", h, nc, nil); err != nil {
		t.Fatal(err)
	}
	return h, fsys, r, nc
}

// edit appends text to the content of nc's file at path, and gives nc its new hash.
func edit(t *testing.T, nc *nodeconfig.NodeConfig, path, text string) {
	t.Helper()
	i := slices.IndexFunc(nc.Files, func(f nodeconfig.File) bool { return f.Path == path })
	if i < 0 {
		t.Fatalf("the config has no file %s", path)
	}
	nc.Files[i].Content = append(slices.Clone(nc.Files[i].Content), text...)
	rehash(t, nc)
}

// marked reports whether the restart marker is on fsys, and fails t unless it is a file that only root reads.
func marked(t *testing.T, fsys *nodeuptest.FS) bool {
	t.Helper()
	e, ok := fsys.Entry(restartFile)
	if ok && (e.Dir || e.Mode != 0o600 || e.Owner != nodeconfig.Owner) {
		t.Errorf("the restart marker is %+v, want a file with mode 0600 owned by root:root", e)
	}
	return ok
}

// Commands of the nomad phase.
const (
	needDaemonReload = "systemctl show -p NeedDaemonReload --value nomad.service"
	nomadActive      = "systemctl is-active nomad.service"
	nomadStart       = "systemctl start nomad.service"
	nomadRestart     = "systemctl restart nomad.service"
	daemonReload     = "systemctl daemon-reload"
)

func TestNomadRestartsOnAChange(t *testing.T) {
	h, fsys, r, nc := nomadInstalled(t)
	firstRun := len(ranNomad(r))

	// A changed agent file restarts Nomad once. The unit file did not change, so systemd needs no reload.
	edit(t, nc, tentPath, "# a new tent version\n")
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("nomad after a change = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive, nomadRestart}, ranNomad(r)[firstRun:]); diff != "" {
		t.Errorf("the commands after a change (-want +got):\n%s", diff)
	}
	if marked(t, fsys) {
		t.Error("the restart marker is left after the restart")
	}
}

// TestNomadUnitChange checks that a changed unit is read by systemd again, then started with.
func TestNomadUnitChange(t *testing.T) {
	h, _, r, nc := nomadInstalled(t)
	firstRun := len(ranNomad(r))

	edit(t, nc, nodeconfig.NomadServiceFile, "RestartSec=3\n")
	if _, err := runPhase(t, "nomad", h, nc, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{needDaemonReload, daemonReload, nomadActive, nomadRestart}
	if diff := cmp.Diff(want, ranNomad(r)[firstRun:]); diff != "" {
		t.Errorf("the commands after a unit change (-want +got):\n%s", diff)
	}
}

// TestNomadRestartsAfterAFailure checks that a change that a run wrote before it failed restarts Nomad on the next
// run, once: the next run changes nothing and would not know of it otherwise.
func TestNomadRestartsAfterAFailure(t *testing.T) {
	gossipFails := func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
		t.Helper()
		fsys.Fail(gossipPath, errors.New("read-only file system"))
	}
	cases := []struct {
		name   string
		change func(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig)
		fail   func(t *testing.T, fsys *nodeuptest.FS, r *nodeuptest.Runner)
		want   string   // the error of the failed run
		next   []string // the next run's commands
	}{
		// An unchanged file is not written, so the file that fails changes too.
		{"a later file", func(t *testing.T, _ *nodeup.Host, nc *nodeconfig.NodeConfig) {
			edit(t, nc, tentPath, "# a new tent version\n")
			edit(t, nc, gossipPath, "# a new gossip key\n")
		}, gossipFails, "configure Nomad: write " + gossipPath + ": read-only file system",
			[]string{needDaemonReload, nomadActive, nomadRestart}},
		{"a new binary, then a file", func(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig) {
			withNomad(t, h, nc, nodeuptest.NomadZip(t, []byte("#!/bin/sh\n# a newer nomad\n")))
			edit(t, nc, gossipPath, "# a new gossip key\n")
		}, gossipFails, "configure Nomad: write " + gossipPath + ": read-only file system",
			[]string{needDaemonReload, nomadActive, nomadRestart}},
		// The marker comes before the binary's write, which may replace the file and then fail.
		{"the new binary's write", func(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig) {
			withNomad(t, h, nc, nodeuptest.NomadZip(t, []byte("#!/bin/sh\n# a newer nomad\n")))
		}, func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			t.Helper()
			fsys.Fail(nodeconfig.NomadBinary, errors.New("no space left on device"))
		}, "install Nomad: write " + nodeconfig.NomadBinary + ": no space left on device",
			[]string{needDaemonReload, nomadActive, nomadRestart}},
		{"daemon-reload", func(t *testing.T, _ *nodeup.Host, nc *nodeconfig.NodeConfig) {
			edit(t, nc, nodeconfig.NomadServiceFile, "RestartSec=3\n")
		}, func(_ *testing.T, _ *nodeuptest.FS, r *nodeuptest.Runner) {
			r.Next(daemonReload, nodeuptest.Exit(1, "Failed to reload daemon: Connection timed out"))
		}, "reload systemd: systemctl daemon-reload: exit status 1: Failed to reload daemon: Connection timed out",
			[]string{needDaemonReload, daemonReload, nomadActive, nomadRestart}},
		{"restart", func(t *testing.T, _ *nodeup.Host, nc *nodeconfig.NodeConfig) {
			edit(t, nc, tentPath, "# a new tent version\n")
		}, func(_ *testing.T, _ *nodeuptest.FS, r *nodeuptest.Runner) {
			r.Next(nomadRestart, nodeuptest.Exit(1, "Job for nomad.service failed because the control process "+
				"exited with error code."))
		}, "restart Nomad: systemctl restart nomad.service: exit status 1: Job for nomad.service failed because the " +
			"control process exited with error code.", []string{needDaemonReload, nomadActive, nomadRestart}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, nc := nomadInstalled(t)
			c.change(t, h, nc)
			c.fail(t, fsys, r)
			if _, err := runPhase(t, "nomad", h, nc, nil); errText(err) != c.want {
				t.Fatalf("nomad = %q, want %q", errText(err), c.want)
			}
			if !marked(t, fsys) {
				t.Error("the failed run left no restart marker")
			}

			fsys.Fail(gossipPath, nil)
			fsys.Fail(nodeconfig.NomadBinary, nil)
			commands := len(ranNomad(r))
			res, err := runPhase(t, "nomad", h, nc, nil)
			if err != nil || res.Status != nodeup.Done {
				t.Fatalf("the next nomad = %s %q, %v; want done", res.Status, res.Reason, err)
			}
			if diff := cmp.Diff(c.next, ranNomad(r)[commands:]); diff != "" {
				t.Errorf("the next run's commands (-want +got):\n%s", diff)
			}
			if marked(t, fsys) {
				t.Error("the restart marker is left after the restart")
			}

			// Then nothing is left to do.
			commands, changes := len(ranNomad(r)), len(fsys.Changes())
			res, err = runPhase(t, "nomad", h, nc, nil)
			if err != nil || res.Status != nodeup.Unchanged {
				t.Errorf("the run after = %s %q, %v; want unchanged", res.Status, res.Reason, err)
			}
			if diff := cmp.Diff([]string{needDaemonReload, nomadActive}, ranNomad(r)[commands:]); diff != "" {
				t.Errorf("the commands of the run after (-want +got):\n%s", diff)
			}
			if got := fsys.Changes()[changes:]; len(got) != 0 {
				t.Errorf("the run after changed %q, want nothing", got)
			}
		})
	}
}

// TestNomadMarksBeforeAFile checks that a file that differs is written only after the restart mark: a mark that fails
// leaves the file as it was, and the next run writes it and restarts Nomad.
func TestNomadMarksBeforeAFile(t *testing.T) {
	h, fsys, r, nc := nomadInstalled(t)
	old, _ := fsys.Entry(tentPath)
	edit(t, nc, tentPath, "# a new tent version\n")
	fsys.Fail(restartFile, errors.New("no space left on device"))
	want := "configure Nomad: write " + restartFile + ": no space left on device"
	if _, err := runPhase(t, "nomad", h, nc, nil); errText(err) != want {
		t.Fatalf("nomad = %q, want %q", errText(err), want)
	}
	if e, _ := fsys.Entry(tentPath); !bytes.Equal(e.Data, old.Data) {
		t.Errorf("00-tent.hcl was written without a restart mark:\n%s", e.Data)
	}

	fsys.Fail(restartFile, nil)
	commands := len(ranNomad(r))
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("the next nomad = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if e, _ := fsys.Entry(tentPath); !strings.HasSuffix(string(e.Data), "# a new tent version\n") {
		t.Errorf("the next run did not write 00-tent.hcl:\n%s", e.Data)
	}
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive, nomadRestart}, ranNomad(r)[commands:]); diff != "" {
		t.Errorf("the next run's commands (-want +got):\n%s", diff)
	}
	if marked(t, fsys) {
		t.Error("the restart marker is left after the restart")
	}
}

// TestNomadStartsAfterAFailedStart checks that a failed start leaves the marker, and the next run starts Nomad and
// removes it.
func TestNomadStartsAfterAFailedStart(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	withNomad(t, h, nc, nomadZip(t))
	r.Next(nomadStart, nodeuptest.Exit(1, "Job for nomad.service failed because the control process exited with "+
		"error code."))
	want := "start Nomad: systemctl start nomad.service: exit status 1: Job for nomad.service failed because the " +
		"control process exited with error code."
	if _, err := runPhase(t, "nomad", h, nc, nil); errText(err) != want {
		t.Fatalf("nomad = %q, want %q", errText(err), want)
	}
	if !marked(t, fsys) {
		t.Error("the failed start left no restart marker")
	}
	commands := len(ranNomad(r))
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("the next nomad = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive, nomadStart}, ranNomad(r)[commands:]); diff != "" {
		t.Errorf("the next run's commands (-want +got):\n%s", diff)
	}
	if marked(t, fsys) {
		t.Error("the restart marker is left after the start")
	}
}

// TestNomadDirectoryChange checks that a directory's mode is set again without a restart: Nomad reads none of it.
func TestNomadDirectoryChange(t *testing.T) {
	h, fsys, r, nc := nomadInstalled(t)
	fsys.AddDir(t, "/etc/nomad.d", 0o700, nodeconfig.Owner)
	commands, changes := len(ranNomad(r)), len(fsys.Changes())
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("nomad = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if diff := cmp.Diff([]string{"/etc/nomad.d"}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{needDaemonReload, nomadActive}, ranNomad(r)[commands:]); diff != "" {
		t.Errorf("the commands (-want +got):\n%s", diff)
	}
}

// TestNomadReloadAlone checks that a daemon-reload is a change of its own, which restarts nothing.
func TestNomadReloadAlone(t *testing.T) {
	h, _, r, nc := nomadInstalled(t)
	r.Next(needDaemonReload, nodeuptest.Output("yes\n"))
	commands := len(ranNomad(r))
	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("nomad = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if diff := cmp.Diff([]string{needDaemonReload, daemonReload, nomadActive}, ranNomad(r)[commands:]); diff != "" {
		t.Errorf("the commands (-want +got):\n%s", diff)
	}
}

// TestNomadIntroToken checks that the intro token goes into the client's state directory, which only root reads and
// which is made before the token is written.
func TestNomadIntroToken(t *testing.T) {
	h, fsys, _ := nomadMachine(t)
	nc := combined(t)
	nc.Files = append(nc.Files, nodeconfig.File{
		Path: nodeconfig.IntroTokenFile, Mode: 0o600, Owner: nodeconfig.Owner, Content: []byte("intro-token\n"),
		PerNode: true, Secret: true,
	})
	withNomad(t, h, nc, nomadZip(t))
	if _, err := runPhase(t, "nomad", h, nc, nil); err != nil {
		t.Fatal(err)
	}
	dir := path.Dir(nodeconfig.IntroTokenFile)
	if e, ok := fsys.Entry(dir); !ok || !e.Dir || e.Mode != 0o700 || e.Owner != nodeconfig.Owner {
		t.Errorf("%s is %+v, want a directory with mode 0700 owned by root:root", dir, e)
	}
	if e, ok := fsys.Entry(nodeconfig.IntroTokenFile); !ok || e.Mode != 0o600 || e.Owner != nodeconfig.Owner {
		t.Errorf("the intro token is %+v, want a file with mode 0600 owned by root:root", e)
	}
	changes := fsys.Changes()
	if d, f := slices.Index(changes, dir), slices.Index(changes, nodeconfig.IntroTokenFile); d < 0 || f < d {
		t.Errorf("changes %q, want %s before the intro token", changes, dir)
	}
}

func TestNomadServer(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := server(t)
	withNomad(t, h, nc, nomadZip(t))

	res, err := runPhase(t, "nomad", h, nc, nil)
	if err != nil || res.Status != nodeup.Done {
		t.Fatalf("nomad on a server = %s %q, %v; want done", res.Status, res.Reason, err)
	}
	if _, ok := fsys.Entry("/var/lib/nomad/client"); ok {
		t.Error("a server has /var/lib/nomad/client")
	}
	if _, ok := fsys.Entry("/etc/nomad.d/11-instance.hcl"); ok {
		t.Error("a server has 11-instance.hcl")
	}
	if _, ok := fsys.Entry(nodeconfig.NomadBinary); !ok {
		t.Error("a server has no nomad binary")
	}
	if !slices.Contains(ranNomad(r), nomadStart) {
		t.Errorf("nomad.service was not started: %q", r.Commands())
	}
}

// zipEntry returns an entry named name with mode and content for nodeuptest.Zip.
func zipEntry(name string, mode fs.FileMode, content string) nodeuptest.ZipFile {
	h := zip.FileHeader{Name: name}
	h.SetMode(mode)
	return nodeuptest.ZipFile{Header: h, Content: []byte(content)}
}

// wantNoInstall fails t if the binary was written or nomad.service touched.
func wantNoInstall(t *testing.T, fsys *nodeuptest.FS, r *nodeuptest.Runner) {
	t.Helper()
	if _, ok := fsys.Entry(nodeconfig.NomadBinary); ok {
		t.Error("the binary was written")
	}
	if len(ranNomad(r)) != 0 {
		t.Errorf("nomad.service was touched: %q", r.Commands())
	}
}

func TestNomadBadArchive(t *testing.T) {
	license := zipEntry("LICENSE.txt", 0o644, "Business Source License 1.1\n")
	binary := zipEntry("nomad", 0o755, string(nomadBinary))
	huge := zipEntry("nomad", 0o755, "x")
	huge.Header.UncompressedSize64 = 256<<20 + 1
	cases := []struct {
		name string
		zip  []byte
		want string
	}{
		{"no nomad", nodeuptest.Zip(t, license), "the nomad archive has no nomad"},
		{"nomad in a directory", nodeuptest.Zip(t, license, zipEntry("bin/nomad", 0o755, "x")),
			"the nomad archive has no nomad"},
		{"nomad twice", nodeuptest.Zip(t, license, binary, binary), "the nomad archive has nomad twice"},
		{"a link", nodeuptest.Zip(t, license, zipEntry("nomad", fs.ModeSymlink|0o777, "/usr/bin/true")),
			"the nomad archive's nomad is not a regular file"},
		{"a directory", nodeuptest.Zip(t, license, zipEntry("nomad", fs.ModeDir|0o755, "")),
			"the nomad archive's nomad is not a regular file"},
		{"over 256 MiB", nodeuptest.Zip(t, license, huge), "the nomad archive's nomad is over 268435456 bytes"},
		{"not a zip", []byte("not a zip"), "the nomad archive: zip: not a valid zip file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r := nomadMachine(t)
			nc := combined(t)
			withNomad(t, h, nc, c.zip)
			if _, err := runPhase(t, "nomad", h, nc, nil); errText(err) != "install Nomad: "+c.want {
				t.Errorf("nomad = %q, want %q", errText(err), "install Nomad: "+c.want)
			}
			wantNoInstall(t, fsys, r)
		})
	}
}

// TestNomadBinaryLongerThanDeclared checks that a binary longer than its zip declares fails before anything is
// written: archive/zip stops a read past the declared size.
func TestNomadBinaryLongerThanDeclared(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	lying := zipEntry("nomad", 0o755, string(nomadBinary))
	lying.Header.UncompressedSize64 = 4
	withNomad(t, h, nc, nodeuptest.Zip(t, lying))
	if _, err := runPhase(t, "nomad", h, nc, nil); !errors.Is(err, zip.ErrFormat) {
		t.Errorf("nomad = %v, want an error that matches %v", err, zip.ErrFormat)
	}
	wantNoInstall(t, fsys, r)
}

func TestNomadMissingAsset(t *testing.T) {
	h, fsys, r := nomadMachine(t)
	nc := combined(t)
	nc.Assets = slices.DeleteFunc(nc.Assets, func(a nodeconfig.Asset) bool { return a.Name == nodeconfig.NomadAsset })
	rehash(t, nc)
	if _, err := runPhase(t, "nomad", h, nc, nil); errText(err) != "install Nomad: NodeConfig has no nomad asset" {
		t.Errorf("nomad without the asset: %v", err)
	}
	if len(fsys.Changes()) != 0 || len(r.Commands()) != 0 {
		t.Errorf("changed %q and ran %q, want nothing", fsys.Changes(), r.Commands())
	}
}
