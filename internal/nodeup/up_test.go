package nodeup_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
	"github.com/ingvarch/tent/internal/secrettest"
)

// equateAddrs compares addresses, whose fields cmp cannot see.
var equateAddrs = cmpopts.EquateComparable(netip.Addr{})

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// checkGolden compares got with the file testdata/name. With -update it rewrites the file first.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	file := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("output differs from %s (-file +got):\n%s", file, diff)
	}
}

// Stand-ins for the secrets of a node. They are not real keys, but the tests treat them as secrets.
var (
	gossipKey = []byte(base64.StdEncoding.EncodeToString([]byte("secret-gossip-key-for-nodeup-tests")))
	nodeKey   = []byte("node-key-stand-in-7e2a4c9d1b8f3065\n")
)

// Paths of the sample's files.
const (
	keyPath    = "/etc/nomad.d/tls/agent-key.pem"
	tentPath   = "/etc/nomad.d/00-tent.hcl"
	gossipPath = "/etc/nomad.d/01-gossip.hcl"
)

// sample returns a valid NodeConfig of a combined node with two secret files, the node's key and the gossip key.
func sample(t *testing.T) *nodeconfig.NodeConfig {
	t.Helper()
	nc := &nodeconfig.NodeConfig{
		APIVersion: v1alpha1.APIVersion,
		Kind:       nodeconfig.Kind,
		Cluster:    "prod",
		Provider:   v1alpha1.ProviderVultr,
		NodeGroup:  "core",
		Name:       "prod-core-0",
		Role:       v1alpha1.RoleCombined,
		Region:     "global",
		Files: []nodeconfig.File{
			{Path: keyPath, Mode: 0o600, Owner: nodeconfig.Owner, Content: nodeKey, PerNode: true, Secret: true},
			{Path: tentPath, Mode: 0o644, Owner: nodeconfig.Owner, Content: []byte("region = \"global\"\n")},
			{
				Path: gossipPath, Mode: 0o600, Owner: nodeconfig.Owner, Secret: true,
				Content: []byte("server {\n  encrypt = \"" + string(gossipKey) + "\"\n}\n"),
			},
		},
		Join: nodeconfig.Join{
			Strategy: nodeconfig.JoinSeedAndRefresh, Servers: []netip.Addr{netip.MustParseAddr("10.64.0.5")},
			RefreshInterval: time.Minute,
		},
		Firewall: nodeconfig.HostFirewall{BlockMetadata: netip.MustParseAddr("169.254.169.254")},
	}
	nc.SpecHash = nodeconfig.SpecHash(nc)
	if err := nc.Validate(); err != nil {
		t.Fatal(err)
	}
	return nc
}

// machine returns a fake machine named prod-core-0 with the directories of the sample's files and /var/lib, and its
// filesystem and runner.
func machine(t *testing.T) (*nodeup.Host, *nodeuptest.FS, *nodeuptest.Runner) {
	t.Helper()
	fsys, r := nodeuptest.NewFS(), &nodeuptest.Runner{}
	fsys.AddDir(t, "/etc/nomad.d/tls", 0o755, nodeconfig.Owner)
	fsys.AddDir(t, "/etc/systemd/system", 0o755, nodeconfig.Owner)
	fsys.AddDir(t, "/var/lib", 0o755, nodeconfig.Owner)
	return nodeuptest.NewHost("prod-core-0", fsys, r), fsys, r
}

// phase returns a phase that records its name in ran and returns res and err.
func phase(name string, ran *[]string, res nodeup.Result, err error) nodeup.Phase {
	return nodeup.Phase{Name: name, Run: func(context.Context, *nodeup.Host, *nodeconfig.NodeConfig) (nodeup.Result, error) {
		*ran = append(*ran, name)
		return res, err
	}}
}

// writeFiles is a phase that writes the files of the NodeConfig, as tent-node's phases do: it reports Done when it
// changed one and Unchanged when all were as they should be.
var writeFiles = nodeup.Phase{Name: "files", Run: func(_ context.Context, h *nodeup.Host, nc *nodeconfig.NodeConfig) (
	nodeup.Result, error,
) {
	res := nodeup.Result{Status: nodeup.Unchanged}
	for _, f := range nc.Files {
		changed, err := h.FS.WriteFile(f.Path, f.Content, fs.FileMode(f.Mode), f.Owner)
		if err != nil {
			return nodeup.Result{}, fmt.Errorf("write %v: %w", f, err)
		}
		if changed {
			res.Status = nodeup.Done
		}
	}
	return res, nil
}}

// reloadUnit is a phase that writes a unit and reloads systemd only when the unit changed.
var reloadUnit = nodeup.Phase{Name: "unit", Run: func(ctx context.Context, h *nodeup.Host, _ *nodeconfig.NodeConfig) (
	nodeup.Result, error,
) {
	changed, err := h.FS.WriteFile("/etc/systemd/system/demo.service", []byte("[Unit]\n"), 0o644, nodeconfig.Owner)
	if err != nil || !changed {
		return nodeup.Result{Status: nodeup.Unchanged}, err
	}
	if err := (nodeup.Systemd{Runner: h.Runner}).DaemonReload(ctx); err != nil {
		return nodeup.Result{}, err
	}
	return nodeup.Result{Status: nodeup.Done}, nil
}}

// readStatus returns the report in the status file, and fails t unless the file and its directory can only be read
// by root.
func readStatus(t *testing.T, fsys *nodeuptest.FS) nodeup.Report {
	t.Helper()
	dir, ok := fsys.Entry(path.Dir(nodeup.StatusFile))
	if want := (nodeuptest.Entry{Dir: true, Mode: 0o700, Owner: "root:root"}); !ok || !cmp.Equal(dir, want) {
		t.Errorf("the status directory is %+v, want %+v", dir, want)
	}
	file, ok := fsys.Entry(nodeup.StatusFile)
	if !ok || file.Dir || file.Mode != 0o600 || file.Owner != "root:root" {
		t.Fatalf("the status file is %+v, want a file with mode 0600 owned by root:root", file)
	}
	var r nodeup.Report
	if err := json.Unmarshal(file.Data, &r); err != nil {
		t.Fatalf("the status file is not JSON: %v", err)
	}
	return r
}

func TestUpRunsThePhasesInOrder(t *testing.T) {
	h, fsys, _ := machine(t)
	nc := sample(t)
	var ran []string
	report, err := nodeup.Up(t.Context(), h, nc, []nodeup.Phase{
		phase("preflight", &ran, nodeup.Result{Status: nodeup.Done}, nil),
		phase("system", &ran, nodeup.Result{Status: nodeup.Unchanged}, nil),
		phase("cni", &ran, nodeup.Result{Status: nodeup.Skipped, Reason: "servers run no workloads"}, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"preflight", "system", "cni"}, ran); diff != "" {
		t.Errorf("phases run (-want +got):\n%s", diff)
	}
	want := nodeup.Report{
		Version: nodeuptest.Version, SpecHash: nc.SpecHash, Started: h.Now(), Finished: h.Now(),
		Phases: []nodeup.PhaseResult{
			{Name: "preflight", Result: nodeup.Result{Status: nodeup.Done}},
			{Name: "system", Result: nodeup.Result{Status: nodeup.Unchanged}},
			{Name: "cni", Result: nodeup.Result{Status: nodeup.Skipped, Reason: "servers run no workloads"}},
		},
	}
	if diff := cmp.Diff(want, report, equateAddrs); diff != "" {
		t.Errorf("report (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(report, readStatus(t, fsys), equateAddrs); diff != "" {
		t.Errorf("status file (-report +file):\n%s", diff)
	}
}

func TestUpStopsAtTheFirstFailure(t *testing.T) {
	h, fsys, r := machine(t)
	nc := sample(t)
	r.On("modprobe br_netfilter", nodeuptest.Exit(1, "modprobe: FATAL: Module br_netfilter not found."))
	var ran []string
	phases := []nodeup.Phase{
		{Name: "preflight", Run: func(context.Context, *nodeup.Host, *nodeconfig.NodeConfig) (nodeup.Result, error) {
			h.Instance = nodeup.Instance{
				ID: "9d1c6f3e-2b7a-4e58-8c0d-5a4f1e7b3c92", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.7"),
			}
			return nodeup.Result{Status: nodeup.Done}, nil
		}},
		{Name: "system", Run: func(ctx context.Context, h *nodeup.Host, _ *nodeconfig.NodeConfig) (nodeup.Result, error) {
			if _, err := h.Runner.Run(ctx, "modprobe", "br_netfilter"); err != nil {
				return nodeup.Result{}, fmt.Errorf("load kernel modules: %w", err)
			}
			return nodeup.Result{Status: nodeup.Done}, nil
		}},
		phase("hostfirewall", &ran, nodeup.Result{Status: nodeup.Done}, nil),
		phase("verify", &ran, nodeup.Result{Status: nodeup.Done}, nil),
	}
	report, err := nodeup.Up(t.Context(), h, nc, phases)
	want := "phase system: load kernel modules: modprobe br_netfilter: exit status 1: modprobe: FATAL: Module " +
		"br_netfilter not found."
	if _, ok := errors.AsType[*nodeup.ExitError](err); !ok || errText(err) != want {
		t.Errorf("Up: %q, want %q, which matches the ExitError", errText(err), want)
	}
	if len(ran) != 0 {
		t.Errorf("the phases %q ran after system failed", ran)
	}
	if diff := cmp.Diff(report, readStatus(t, fsys), equateAddrs); diff != "" {
		t.Errorf("status file (-report +file):\n%s", diff)
	}
	status, _ := fsys.Entry(nodeup.StatusFile)
	checkGolden(t, "status-failed.json.golden", status.Data)
}

func TestUpStopsBetweenPhasesWhenTheContextEnds(t *testing.T) {
	h, fsys, _ := machine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var ran []string
	stop := nodeup.Phase{Name: "preflight", Run: func(context.Context, *nodeup.Host, *nodeconfig.NodeConfig) (
		nodeup.Result, error,
	) {
		cancel() // as SIGTERM does while the phase runs
		return nodeup.Result{Status: nodeup.Done}, nil
	}}
	report, err := nodeup.Up(ctx, h, sample(t), []nodeup.Phase{
		stop,
		phase("system", &ran, nodeup.Result{Status: nodeup.Done}, nil),
		phase("verify", &ran, nodeup.Result{Status: nodeup.Done}, nil),
	})
	if want := "phase system: not started: context canceled"; !errors.Is(err, context.Canceled) || errText(err) != want {
		t.Errorf("Up: %v, want %q, which matches context.Canceled", err, want)
	}
	if len(ran) != 0 {
		t.Errorf("the phases %q ran after the context ended", ran)
	}
	want := []nodeup.PhaseResult{
		{Name: "preflight", Result: nodeup.Result{Status: nodeup.Done}},
		{Name: "system", Result: nodeup.Result{Status: nodeup.Failed, Reason: "not started: context canceled"}},
		{Name: "verify", Result: nodeup.Result{Status: nodeup.Skipped, Reason: "not run: system failed"}},
	}
	if diff := cmp.Diff(want, report.Phases); diff != "" {
		t.Errorf("phases (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(report, readStatus(t, fsys), equateAddrs); diff != "" {
		t.Errorf("status file (-report +file):\n%s", diff)
	}
}

func TestUpStopsACommandWhenTheContextEnds(t *testing.T) {
	h, fsys, r := machine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.On("systemctl start nomad.service", func(ctx context.Context) ([]byte, error) {
		cancel() // as SIGTERM does while the command runs
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := nodeup.Phase{Name: "nomad", Run: func(ctx context.Context, h *nodeup.Host, _ *nodeconfig.NodeConfig) (
		nodeup.Result, error,
	) {
		if err := (nodeup.Systemd{Runner: h.Runner}).Start(ctx, "nomad.service"); err != nil {
			return nodeup.Result{}, err
		}
		return nodeup.Result{Status: nodeup.Done}, nil
	}}
	var ran []string
	report, err := nodeup.Up(ctx, h, sample(t), []nodeup.Phase{start, phase("verify", &ran, nodeup.Result{}, nil)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Up: %v, want an error that matches context.Canceled", err)
	}
	if len(ran) != 0 {
		t.Errorf("the phases %q ran after the context ended", ran)
	}
	want := []nodeup.PhaseResult{
		{Name: "nomad", Result: nodeup.Result{
			Status: nodeup.Failed, Reason: "systemctl start nomad.service: context canceled",
		}},
		{Name: "verify", Result: nodeup.Result{Status: nodeup.Skipped, Reason: "not run: nomad failed"}},
	}
	if diff := cmp.Diff(want, report.Phases); diff != "" {
		t.Errorf("phases (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(report, readStatus(t, fsys), equateAddrs); diff != "" {
		t.Errorf("status file (-report +file):\n%s", diff)
	}
}

func TestUpSecondRunChangesNothing(t *testing.T) {
	h, fsys, r := machine(t)
	// A clock that moves, as a real one does, so that only the status changes on the second run.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	nc, phases := sample(t), []nodeup.Phase{writeFiles, reloadUnit}
	first, err := nodeup.Up(t.Context(), h, nc, phases)
	if err != nil {
		t.Fatal(err)
	}
	changes, commands := len(fsys.Changes()), len(r.Commands())
	second, err := nodeup.Up(t.Context(), h, nc, phases)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range second.Phases {
		if p.Status != nodeup.Unchanged || first.Phases[i].Status != nodeup.Done {
			t.Errorf("phase %s: %s on the first run and %s on the second, want done and unchanged", p.Name,
				first.Phases[i].Status, p.Status)
		}
	}
	if diff := cmp.Diff([]string{nodeup.StatusFile}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("the second run changed files (-want +got):\n%s", diff)
	}
	if got := r.Commands()[commands:]; len(got) != 0 {
		t.Errorf("the second run ran %q, want no commands", got)
	}
}

func TestUpShowsNoSecrets(t *testing.T) {
	h, fsys, _ := machine(t)
	var logs bytes.Buffer
	h.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// The node's key is written, and the gossip key fails.
	fsys.Fail(gossipPath, errors.New("read-only file system"))
	nc := sample(t)
	report, err := nodeup.Up(t.Context(), h, nc, []nodeup.Phase{writeFiles})
	want := "phase files: write " + nc.Files[2].String() + ": write " + gossipPath + ": read-only file system"
	if errText(err) != want {
		t.Errorf("Up: %q, want %q", errText(err), want)
	}
	if key, _ := fsys.Entry(keyPath); !bytes.Equal(key.Data, nodeKey) {
		t.Error("the node's key was not written")
	}
	status, _ := fsys.Entry(nodeup.StatusFile)
	outputs := secrettest.Printed(t, report)
	outputs["the logs"] = logs.String()
	outputs["the error"] = errText(err)
	outputs["the status file"] = string(status.Data)
	secrettest.CheckHidden(t, outputs, map[string][]byte{"the gossip key": gossipKey, "the node's key": nodeKey}, "")
	if logs.Len() == 0 {
		t.Error("Up logged nothing")
	}
}

func TestUpReportsAStatusItCannotWrite(t *testing.T) {
	boom := errors.New("no space left on device")
	cases := []struct {
		name   string
		phases []nodeup.Phase
		want   string
	}{
		{"after success", []nodeup.Phase{writeFiles}, "write the status: write " + nodeup.StatusFile + ": " +
			"no space left on device"},
		{"after a failure", []nodeup.Phase{{Name: "preflight", Run: func(context.Context, *nodeup.Host,
			*nodeconfig.NodeConfig,
		) (nodeup.Result, error) {
			return nodeup.Result{}, errors.New("the host name is prod-core-9, not prod-core-0")
		}}}, "phase preflight: the host name is prod-core-9, not prod-core-0\nwrite the status: write " +
			nodeup.StatusFile + ": no space left on device"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, _ := machine(t)
			fsys.Fail(nodeup.StatusFile, boom)
			_, err := nodeup.Up(t.Context(), h, sample(t), c.phases)
			if !errors.Is(err, boom) || errText(err) != c.want {
				t.Errorf("Up: %q, want %q", errText(err), c.want)
			}
		})
	}
}

func TestUpRefusesAResultWithoutAStatus(t *testing.T) {
	for _, status := range []nodeup.Status{"", nodeup.Failed, "partly"} {
		h, _, _ := machine(t)
		var ran []string
		report, err := nodeup.Up(t.Context(), h, sample(t), []nodeup.Phase{
			phase("system", &ran, nodeup.Result{Status: status}, nil),
			phase("verify", &ran, nodeup.Result{Status: nodeup.Done}, nil),
		})
		want := fmt.Sprintf("phase system: the phase returned the status %q without an error", status)
		if errText(err) != want || report.Phases[0].Status != nodeup.Failed || len(ran) != 1 {
			t.Errorf("a result with the status %q: err %q, phases %+v; want %q and system failed", status,
				errText(err), report.Phases, want)
		}
	}
}

func TestLocal(t *testing.T) {
	h, err := nodeup.Local(nil)
	if err != nil {
		t.Fatal(err)
	}
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if h.HostName != name || h.EUID != os.Geteuid() || h.OS != runtime.GOOS || h.Arch != runtime.GOARCH ||
		h.Version != buildinfo.Get().Version {
		t.Errorf("Local() = %+v, want the facts of this process", h)
	}
	if h.FS != (nodeup.OSFS{}) || h.Runner != (nodeup.ExecRunner{}) {
		t.Errorf("Local() uses %T and %T, want the machine's own filesystem and programs", h.FS, h.Runner)
	}
	if d := time.Since(h.Now()); d < 0 || d > time.Minute {
		t.Errorf("Local().Now() is %s away from the time", d)
	}
}
