package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cli"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// The checks of M0's exit criteria (docs/roadmap.md, issue #36). The first two run tent in this process through
// cli.Execute and as the built binary; the third needs separate processes, so it runs the binary only.

// vultrSpec is the Vultr example of docs/architecture.md §3.1, as of 2026-09-27.
const vultrSpec = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod                     # [a-z][a-z0-9-]{0,18}[a-z0-9]; prefix of every resource name
spec:
  channel: stable                # recommended versions and images (see 13.5)
  cloud:
    provider: vultr
    region: ams                  # Vultr: region | Hetzner: network zone | AWS: region
    zones: [ams]                 # Vultr has no zones: left out, it defaults to [region], the only valid value
    vultr: {}                    # provider-specific block, optional; no fields yet
  networking:
    cidr: 10.64.0.0/16           # VPC CIDR, a private IPv4 range; the default
  access:
    ssh: [203.0.113.7/32]        # empty = SSH closed on the cloud firewall
    api: [0.0.0.0/0]             # the default; :4646 is protected by mTLS + ACL, and tent warns while it is open
  sshKeys:
    - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example
  nomad:
    version: 2.0.7
    region: global
    tls: {verifyHTTPSClient: true}
    clientIntroduction: strict   # strict | warn | none
    extraConfig:                 # escape hatch, rendered into 99-user.hcl
      server: ""
      client: ""
---
apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server                   # server | client | combined (ADR-0019)
  machineType: vc2-2c-4gb        # provider-native plan id
  image: ubuntu-24.04            # resolved by the provider (Vultr: os_id 2284)
  size: 3
---
apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 3
  nomad:
    nodePool: default
    nodeClass: general
    drivers: [docker, exec]
    meta: {team: platform}
`

// hetznerSpec is vultrSpec on Hetzner, as §3.1 describes it: only the cloud block and the machine types differ.
func hetznerSpec(t *testing.T) string {
	return edited(t, vultrSpec,
		"provider: vultr", "provider: hetzner",
		"region: ams ", "region: eu-central ",
		"zones: [ams]", "zones: [fsn1, nbg1, hel1]",
		"vultr: {}", "hetzner: {}",
		"machineType: vc2-2c-4gb ", "machineType: cx23 ", // the servers
		"machineType: vc2-2c-4gb\n", "machineType: cx33\n", // the workers
	)
}

// Parts of vultrSpec that the checks change.
const (
	serversType = "machineType: vc2-2c-4gb "
	serversSize = "  size: 3\n---"
	workersSize = "  size: 3\n  nomad:"
	nomadRegion = "region: global"
)

// edited returns s with each old string of pairs, which must occur once, replaced by the new one after it.
func edited(t *testing.T, s string, pairs ...string) string {
	t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		if n := strings.Count(s, pairs[i]); n != 1 {
			t.Fatalf("%q occurs %d times, want once", pairs[i], n)
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

// What tent prints for cluster prod of the spec files.
const (
	createdProd    = "cluster prod created\nnode group servers created\nnode group workers created\n"
	openAPIWarning = "WARNING: spec.access.api lets the whole internet reach the Nomad API (port 4646); mTLS and " +
		"ACLs protect it; narrow it with --api-access or spec.access.api\n"
)

// TestSpecSurvivesRoundTrip checks the exit criterion "a spec survives create → get -o yaml → replace unchanged":
// get prints the spec that create stored, and replace with that output rewrites nothing.
func TestSpecSurvivesRoundTrip(t *testing.T) {
	for _, file := range []struct{ name, spec string }{
		{"vultr", vultrSpec},
		{"hetzner", hetznerSpec(t)},
	} {
		t.Run(file.name, func(t *testing.T) {
			forEachWay(t, func(t *testing.T, c tentCLI) {
				s := newStore(t)
				wantResult(t, c.run(t, "", "create", "-f", writeFile(t, file.spec), "--state", s.url),
					0, createdProd, openAPIWarning)
				before := s.objects(t)

				printed, got := c.getReplace(t, s.url)
				wantResult(t, got, 0,
					"cluster prod unchanged\nnode group servers unchanged\nnode group workers unchanged\n",
					openAPIWarning)
				s.wantUntouched(t, before)
				if diff := cmp.Diff(decode(t, file.spec), decode(t, printed)); diff != "" {
					t.Errorf("get -o yaml printed another spec (-file +printed):\n%s", diff)
				}
				wantResult(t, c.run(t, "", "get", "prod", "-o", "yaml", "--state", s.url), 0, printed, "")
			})
		})
	}
}

// Documents of invalid spec files. Together they have several problems at once: a public networking CIDR, an empty
// access.api, a server group of 2, a node group with a bad name, and a node group of another cluster.
const (
	publicCluster = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: vultr
    region: ams
    vultr: {}
  networking:
    cidr: 203.0.113.0/24
  access:
    api: []
`
	twoServers = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  size: 2
`
	badName = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: web_1
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  size: 1
`
	otherCluster = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: dev
spec:
  role: client
  machineType: vc2-2c-4gb
  size: 3
`
)

// docs joins YAML documents into one spec file.
func docs(d ...string) string { return strings.Join(d, "---\n") }

// The problems of the invalid documents, one line each, as tent prints them.
const (
	invalidCluster = "  Cluster prod: spec.networking.cidr: must be a private range inside 10.0.0.0/8, 172.16.0.0/12 " +
		"or 192.168.0.0/16\n" +
		"  Cluster prod: spec.access.api: must not be empty: tent needs the Nomad API; list the addresses that " +
		"may reach it\n"
	invalidServers = "  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n"
	invalidName    = "  NodeGroup web_1: metadata.name: must be 2 to 20 lowercase letters, digits or dashes, " +
		"starting with a letter and ending with a letter or digit\n"
	invalidWorkers = "  NodeGroup workers: metadata.cluster: must be prod, the cluster's name\n"
)

// sizeTypo misspells the servers' size in a spec file.
func sizeTypo(t *testing.T) string { return edited(t, vultrSpec, serversSize, "  sizee: 3\n---") }

// TestInvalidSpecNamesFieldPaths checks the exit criterion "invalid specs produce field-path errors": each problem
// is a line that names the object and the field, tent exits 1, and nothing is written.
func TestInvalidSpecNamesFieldPaths(t *testing.T) {
	forEachWay(t, func(t *testing.T, c tentCLI) {
		s := newStore(t)
		invalid := docs(publicCluster, twoServers, badName, otherCluster)
		wantResult(t, c.run(t, "", "create", "-f", writeFile(t, invalid), "--state", s.url), 1, "",
			"Error: invalid spec:\n"+invalidCluster+invalidServers+invalidName+invalidWorkers)
		wantResult(t, c.run(t, "", "create", "-f", writeFile(t, sizeTypo(t)), "--state", s.url), 1, "",
			"Error: document 2 (NodeGroup): line 37: unknown field \"spec.sizee\"\n")
		s.wantEmpty(t)

		wantResult(t, c.run(t, "", "create", "-f", writeFile(t, vultrSpec), "--state", s.url),
			0, createdProd, openAPIWarning)
		before := s.objects(t)
		// replace takes stored objects only, so the group with the bad name, which cannot be stored, is left out.
		existing := docs(publicCluster, twoServers, otherCluster)
		wantResult(t, c.run(t, "", "replace", "-f", writeFile(t, existing), "--state", s.url), 1, "",
			"Error: invalid spec:\n"+invalidCluster+invalidServers+invalidWorkers)
		wantResult(t, c.run(t, "", "replace", "-f", writeFile(t, sizeTypo(t)), "--state", s.url), 1, "",
			"Error: document 2 (NodeGroup): line 37: unknown field \"spec.sizee\"\n")
		s.wantUntouched(t, before)
	})
}

// TestFileBackendSerializesChanges checks the exit criterion "two concurrent mutating commands on the file backend
// are serialized". The file backend locks a cluster with a file lock that the OS holds for a process, so the check
// runs tent as separate processes.
func TestFileBackendSerializesChanges(t *testing.T) {
	bin := sharedTent(t)

	t.Run("waits for a lock held by another process", func(t *testing.T) {
		s := createdStore(t, bin)
		held := s.holdLock(t)
		before := s.objects(t)
		bigger := edited(t, vultrSpec, workersSize, "  size: 5\n  nomad:")

		p := startReplace(t, bin, s, bigger)
		p.waitForStderr(t, waitingNotice(held))
		s.wantUntouched(t, before)

		if err := held.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		wantResult(t, p.wait(t), 0,
			"cluster prod unchanged\nnode group servers unchanged\nnode group workers replaced\n",
			waitingNotice(held)+openAPIWarning)
		wantSpec(t, binary{bin}, s, bigger)
	})

	t.Run("two changes at once", func(t *testing.T) {
		s := createdStore(t, bin)
		// Both change the workers, and each changes one more object: the store ends as one file or the other only
		// if the changes ran one after the other.
		a := change{spec: edited(t, vultrSpec, serversType, "machineType: vc2-4c-8gb ", workersSize,
			"  size: 5\n  nomad:"), alone: "cluster prod unchanged\nnode group servers replaced\n"}
		b := change{spec: edited(t, vultrSpec, nomadRegion, "region: europe", workersSize, "  size: 1\n  nomad:"),
			alone: "cluster prod replaced\nnode group servers unchanged\n"}
		// The test holds the lock until both wait for it, so that they contend for it once it is free.
		held := s.holdLock(t)
		both := []*change{&a, &b}
		for _, c := range both {
			c.proc = startReplace(t, bin, s, c.spec)
		}
		for _, c := range both {
			c.proc.waitForStderr(t, waitingNotice(held))
		}
		if err := held.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		for _, c := range both {
			c.result = c.proc.wait(t)
		}

		final := decode(t, getYAML(t, binary{bin}, s.url))
		first, second := &a, &b
		switch {
		case cmp.Equal(final, decode(t, b.spec)):
		case cmp.Equal(final, decode(t, a.spec)):
			first, second = &b, &a
		default:
			t.Fatalf("the store holds neither change alone:\n%s\nA: %+v\nB: %+v", getYAML(t, binary{bin}, s.url),
				a.result, b.result)
		}
		// The first found the store as create left it, the second as the first left it.
		wantResult(t, first.result, 0, first.alone+"node group workers replaced\n",
			waitingNotice(held)+openAPIWarning)
		wantResult(t, second.result, 0,
			"cluster prod replaced\nnode group servers replaced\nnode group workers replaced\n",
			waitingNotice(held)+openAPIWarning)
	})
}

// change is one of two replaces that run at once.
type change struct {
	spec   string
	alone  string // what it prints for the Cluster and the servers when it runs first
	proc   *proc
	result result
}

// createdStore returns a store that holds cluster prod of vultrSpec, created by the tent binary bin.
func createdStore(t *testing.T, bin string) store {
	t.Helper()
	s := newStore(t)
	wantResult(t, binary{bin}.run(t, "", "create", "-f", writeFile(t, vultrSpec), "--state", s.url),
		0, createdProd, openAPIWarning)
	return s
}

// startReplace starts the tent binary bin to replace the specs of the store with those of a spec file. It waits up
// to a minute for the lock.
func startReplace(t *testing.T, bin string, s store, spec string) *proc {
	t.Helper()
	return startTent(t, bin, "replace", "-f", writeFile(t, spec), "--lock-timeout", "1m", "--state", s.url)
}

// waitingNotice is what a change with --lock-timeout 1m prints when it finds the lock that held holds.
func waitingNotice(held *statestore.Lock) string {
	return "cluster prod is locked by " + held.Lease().String() + "; waiting up to 1m0s (--lock-timeout)\n"
}

// tentCLI runs tent: in this process or as the built binary.
type tentCLI interface {
	// run runs tent with args and stdin and returns what it did.
	run(t *testing.T, stdin string, args ...string) result
	// getReplace prints cluster prod with get -o yaml and replaces the specs with that output. It returns what get
	// printed and what replace did.
	getReplace(t *testing.T, state string) (string, result)
}

// forEachWay runs check once for each way to run tent, as subtests.
func forEachWay(t *testing.T, check func(*testing.T, tentCLI)) {
	t.Helper()
	for _, way := range []struct {
		name string
		new  func(*testing.T) tentCLI
	}{
		{"in-process", newInProcess},
		{"binary", func(t *testing.T) tentCLI { return binary{sharedTent(t)} }},
	} {
		t.Run(way.name, func(t *testing.T) { check(t, way.new(t)) })
	}
}

// inProcess runs tent through cli.Execute.
type inProcess struct{}

// newInProcess keeps the user's config file and TENT_ variables away from the test.
func newInProcess(t *testing.T) tentCLI {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TENT_STATE", "")
	t.Setenv("TENT_CLUSTER", "")
	return inProcess{}
}

func (inProcess) run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Execute(t.Context(), args, cli.Streams{In: strings.NewReader(stdin), Out: &out, Err: &errOut})
	return result{code, out.String(), errOut.String()}
}

// getReplace replaces the specs from a file that holds what get printed.
func (c inProcess) getReplace(t *testing.T, state string) (string, result) {
	t.Helper()
	printed := getYAML(t, c, state)
	return printed, c.run(t, "", "replace", "-f", writeFile(t, printed), "--state", state)
}

// binary runs the tent binary at its path.
type binary struct{ path string }

func (b binary) run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	cmd := tent(t, b.path, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	return result{exitCode(t, cmd.Run()), out.String(), errOut.String()}
}

// getReplace pipes get into replace -f -, as tent get -o yaml | tent replace -f - does.
func (b binary) getReplace(t *testing.T, state string) (string, result) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	get := tent(t, b.path, "get", "prod", "-o", "yaml", "--state", state)
	var getErr bytes.Buffer
	get.Stdout, get.Stderr = w, &getErr
	err = get.Start()
	_ = w.Close() // get holds its own copy; replace sees the end of its input when get exits
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = get.Process.Kill() // fails when it has exited
		_ = get.Wait()         // fails when the test has waited already
	})
	var printed, out, errOut bytes.Buffer
	replace := tent(t, b.path, "replace", "-f", "-", "--state", state)
	replace.Stdin, replace.Stdout, replace.Stderr = io.TeeReader(r, &printed), &out, &errOut
	code := exitCode(t, replace.Run())
	if err := get.Wait(); err != nil {
		t.Fatalf("tent get: %v\n%s", err, getErr.String())
	}
	return printed.String(), result{code, out.String(), errOut.String()}
}

// exitCode returns the exit code of a tent that ended with err.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("run tent: %v", err)
	}
	return 0
}

// getYAML returns what get -o yaml prints for cluster prod of the store at state, and stops the test unless it
// succeeds.
func getYAML(t *testing.T, c tentCLI, state string) string {
	t.Helper()
	got := c.run(t, "", "get", "prod", "-o", "yaml", "--state", state)
	if got.code != 0 || got.errOut != "" {
		t.Fatalf("tent get exited with %d:\n%s", got.code, got.errOut)
	}
	return got.out
}

// wantSpec fails the test unless the store holds the objects of the spec file want.
func wantSpec(t *testing.T, c tentCLI, s store, want string) {
	t.Helper()
	if diff := cmp.Diff(decode(t, want), decode(t, getYAML(t, c, s.url))); diff != "" {
		t.Errorf("the store (-want +got):\n%s", diff)
	}
}

func decode(t *testing.T, file string) spec.Objects {
	t.Helper()
	objs, err := spec.Decode([]byte(file))
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, file)
	}
	return objs
}

// result is what one run of tent did.
type result struct {
	code        int
	out, errOut string
}

// wantResult fails the test unless tent exited with code and wrote exactly out and errOut.
func wantResult(t *testing.T, got result, code int, out, errOut string) {
	t.Helper()
	if got.code != code {
		t.Errorf("exit code = %d, want %d; stderr:\n%s", got.code, code, got.errOut)
	}
	if diff := cmp.Diff(out, got.out); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(errOut, got.errOut); diff != "" {
		t.Errorf("stderr (-want +got):\n%s", diff)
	}
}

// writeFile writes content to a new file and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// store is a file state store in a temporary directory.
type store struct {
	url  string
	root string // does not exist until something is written
}

func newStore(t *testing.T) store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	p := filepath.ToSlash(root)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path
	}
	return store{url: (&url.URL{Scheme: "file", Path: p}).String(), root: root}
}

func (s store) open(t *testing.T) statestore.Store {
	t.Helper()
	st, err := statestore.Open(t.Context(), s.url)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return st
}

// object is a stored object as its file holds it.
type object struct {
	data string
	info os.FileInfo
}

// objects returns every object in the store, read from its file.
func (s store) objects(t *testing.T) map[string]object {
	t.Helper()
	paths, err := s.open(t).List(t.Context(), "")
	if err != nil {
		t.Fatalf("list the store: %v", err)
	}
	objs := make(map[string]object, len(paths))
	for _, p := range paths {
		file := filepath.Join(s.root, filepath.FromSlash(p))
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		objs[p] = object{string(data), info}
	}
	return objs
}

// wantUntouched fails the test unless the store holds the objects of before, none of them written again.
func (s store) wantUntouched(t *testing.T, before map[string]object) {
	t.Helper()
	after := s.objects(t)
	data := func(objs map[string]object) map[string]string {
		m := make(map[string]string, len(objs))
		for p, o := range objs {
			m[p] = o.data
		}
		return m
	}
	if diff := cmp.Diff(data(before), data(after)); diff != "" {
		t.Errorf("the store changed (-before +after):\n%s", diff)
		return
	}
	for p, b := range before {
		// A write renames a new file over the old one. SameFile sees that, except on Windows, where it reads the
		// file ids only when called; the modification time shows it there.
		if a := after[p]; !os.SameFile(b.info, a.info) || !b.info.ModTime().Equal(a.info.ModTime()) {
			t.Errorf("%s was written again", p)
		}
	}
}

// wantEmpty fails the test unless nothing was ever written to the store.
func (s store) wantEmpty(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(s.root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the store was written to: stat %s: %v", s.root, err)
	}
}

// holdLock takes the lock of cluster prod as another tent would, until the test releases it or ends.
func (s store) holdLock(t *testing.T) *statestore.Lock {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	lk, mech, err := statestore.NewLocker(t.Context(), s.open(t), l, nil)
	if err != nil || mech != statestore.MechanismFlock {
		t.Fatalf("NewLocker = %s, %v; want the file lock", mech, err)
	}
	lock, err := statestore.Acquire(t.Context(), lk, "prod", statestore.AcquireOptions{Operation: "update"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release(context.Background()) })
	return lock
}

// procTimeout bounds a tent process, beyond its --lock-timeout of 1m, so a stuck one fails the test.
const procTimeout = 2 * time.Minute

// proc is a tent process that runs while the test goes on.
type proc struct {
	cmd         *exec.Cmd
	out, errOut syncBuffer
	done        chan struct{} // closed when the process has exited and its output is complete
	err         error         // of Wait
}

// startTent starts the tent binary with args. The test kills it and waits for it when it ends.
func startTent(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: tent(t, bin, args...), done: make(chan struct{})}
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.errOut
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill() // fails when it has exited
		<-p.done
	})
	return p
}

// waitForStderr waits until the process has written want to stderr. It fails the test when the process writes
// something else, exits first, or takes longer than procTimeout.
func (p *proc) waitForStderr(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(procTimeout)
	for got := p.errOut.String(); got != want; got = p.errOut.String() {
		if !strings.HasPrefix(want, got) {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
		select {
		case <-p.done:
			if got = p.errOut.String(); got != want {
				t.Fatalf("tent exited (%v) before it wrote %q\nstdout: %s\nstderr: %s", p.err, want, p.out.String(),
					got)
			}
		case <-deadline:
			t.Fatalf("tent did not write %q within %s; stderr: %q", want, procTimeout, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// wait waits for the process to exit and returns what it did. It kills a process that runs longer than procTimeout.
func (p *proc) wait(t *testing.T) result {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(procTimeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Fatalf("tent did not exit within %s\nstdout: %s\nstderr: %s", procTimeout, p.out.String(), p.errOut.String())
	}
	return result{exitCode(t, p.err), p.out.String(), p.errOut.String()}
}

// syncBuffer is a buffer that a process's output goes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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
