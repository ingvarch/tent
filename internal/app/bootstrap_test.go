package app_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
)

// The specs of test clusters that run combined nodes.
const (
	combinedYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: all
  cluster: prod
spec:
  role: combined
  machineType: vc2-2c-4gb
  size: 3
`
)

// newRelease returns a service of tent v0.5.0 that allows a single server, whose store holds the test cluster, or the
// specs docs, and whose providers reach the Vultr fake and the Nomad that follows it. A test that applies an update
// calls it in a synctest bubble.
func newRelease(t *testing.T, docs ...string) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	if len(docs) == 0 {
		docs = []string{keyedClusterYAML, serversYAML, workersYAML}
	}
	svc, _ := newService(t)
	svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
	mustCreate(t, svc, docs...)
	svc.Version = "v0.5.0"
	f, _ := withCloud(svc)
	return svc, f, withNomad(svc, f)
}

// configOf returns the NodeConfig in the user data of the instance called name.
func configOf(t *testing.T, f *vultrfake.Fake, name string) *nodeconfig.NodeConfig {
	t.Helper()
	for _, in := range f.Instances() {
		if in.Hostname != name {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(f.UserData(in.ID))
		if err != nil {
			t.Fatalf("the user data of %s: %v", name, err)
		}
		nc, err := nodeconfig.Decode(app.PayloadOf(t, data))
		if err != nil {
			t.Fatalf("the NodeConfig of %s: %v", name, err)
		}
		return nc
	}
	t.Fatalf("the fake has no instance %s", name)
	return nil
}

// fileOf returns the file of nc at path, or nil.
func fileOf(nc *nodeconfig.NodeConfig, path string) *nodeconfig.File {
	for i := range nc.Files {
		if nc.Files[i].Path == path {
			return &nc.Files[i]
		}
	}
	return nil
}

// seedOf returns the servers that nc joins first, as text.
func seedOf(nc *nodeconfig.NodeConfig) []string {
	var seed []string
	for _, a := range nc.Join.Servers {
		seed = append(seed, a.String())
	}
	return seed
}

// wantCertificate fails the test unless the certificate in nc chains to ca and names names.
func wantCertificate(t *testing.T, nc *nodeconfig.NodeConfig, ca *pki.CA, names ...string) {
	t.Helper()
	file := fileOf(nc, nodeconfig.CertFile)
	if file == nil {
		t.Fatalf("%s has no certificate", nc.Name)
	}
	block, _ := pem.Decode(file.Content)
	if block == nil {
		t.Fatalf("the certificate of %s is not PEM", nc.Name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("the certificate of %s: %v", nc.Name, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: time.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		t.Errorf("the certificate of %s does not chain to the cluster's CA: %v", nc.Name, err)
	}
	for _, n := range names {
		if !slices.Contains(cert.DNSNames, n) {
			t.Errorf("the certificate of %s names %v, want %s too", nc.Name, cert.DNSNames, n)
		}
	}
}

// introClaims returns the claims of the intro token in nc, a JWT, or nil when nc has none.
func introClaims(t *testing.T, nc *nodeconfig.NodeConfig) map[string]string {
	t.Helper()
	file := fileOf(nc, nodeconfig.IntroTokenFile)
	if file == nil {
		return nil
	}
	parts := strings.Split(string(file.Content), ".")
	if len(parts) != 3 {
		t.Fatalf("the intro token of %s has %d parts, want 3", nc.Name, len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("the claims of the intro token of %s: %v", nc.Name, err)
	}
	var claims map[string]string
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("the claims of the intro token of %s: %v", nc.Name, err)
	}
	return claims
}

// nomadLines returns the calls that reached the Nomad fake of w, as its line gives each. The line of a
// Bootstrap call shows the secret's size and never the secret.
func nomadLines(w *nomadWorld) []string {
	var lines []string
	for _, c := range w.Log() {
		lines = append(lines, w.line(c.Call))
	}
	return lines
}

// bootstrapLine is the line of the Bootstrap call with the stored bootstrap secret.
func bootstrapLine(t *testing.T, svc *app.Service, server string) string {
	t.Helper()
	return "nomad Bootstrap " + secret.Secret(get(t, svc.Store, aclPath)).String() + " (" + server + ")"
}

// introLine is the line of the IntroToken call for the node of the test cluster's workers.
func introLine(node, server string) string {
	return "nomad IntroToken " + node + " default " + nomadops.MaxIntroTTL.String() + " (" + server + ")"
}

// onlyNomadAndNodes keeps the progress lines of node steps and of the Nomad step.
func onlyNomadAndNodes(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return !strings.HasPrefix(l, "node ") && !strings.HasPrefix(l, "nomad ")
	})
}

// TestUpdateBuildsANomadCluster builds the test cluster, three servers and two clients, on an empty cloud: every node
// boots with the NodeConfig of its group, the servers with the seed of the servers that exist, the clients with an
// intro token; Nomad is bootstrapped with the stored secret once the servers run, and the clients are made after.
func TestUpdateBuildsANomadCluster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		now := time.Now()
		svc.Now = func() time.Time { return now }
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec
		progress := recordProgress(svc)
		var events []app.Progress
		record := svc.OnProgress
		svc.OnProgress = func(p app.Progress) {
			record(p)
			events = append(events, p)
		}
		var firstCreate sync.Once
		stored := false
		f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
			if c.Name == "CreateInstance" {
				firstCreate.Do(func() {
					_, _, err := rec.Get(ctx, completedPath)
					stored = err == nil
				})
			}
			return next(ctx)
		})

		plan := mustUpdate(t, svc)

		f.SetHook(nil)
		if want := (&app.NomadStep{Bootstrap: true, Servers: 3}); !cmp.Equal(plan.Nomad, want) {
			t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
		}
		if !plan.Applied {
			t.Error("the plan does not say it was applied")
		}
		wantNodes(t, f, allNodes...)
		if !stored {
			t.Error("the completed spec was not stored when the first node was created")
		}
		// The lock is written when it is taken, renewed and released; the other writes come in this order.
		wantWrites := []string{versionPath, caKeyPath, caBundlePath, gossipPath, aclPath, completedPath, markPath}
		writes := slices.DeleteFunc(slices.Clone(rec.writes), func(p string) bool { return p == lockPath })
		if diff := cmp.Diff(wantWrites, writes); diff != "" {
			t.Errorf("writes to the store (-want +got):\n%s", diff)
		}

		wantStored(t, svc.Store, markPath, []byte(now.UTC().Format(time.RFC3339)+"\n"))
		ca := storedCA(t, svc.Store)
		gossip := get(t, svc.Store, gossipPath)
		wantSeeds := map[string][]string{
			"prod-servers-0": nil,
			"prod-servers-1": {"10.64.0.3"},
			"prod-servers-2": {"10.64.0.3", "10.64.0.4"},
			"prod-workers-0": {"10.64.0.3", "10.64.0.4", "10.64.0.5"},
			"prod-workers-1": {"10.64.0.3", "10.64.0.4", "10.64.0.5"},
		}
		hashes := map[string]string{}
		for _, in := range f.Instances() {
			nc := configOf(t, f, in.Hostname)
			if diff := cmp.Diff(wantSeeds[in.Hostname], seedOf(nc)); diff != "" {
				t.Errorf("the seed of %s (-want +got):\n%s", in.Hostname, diff)
			}
			if tag := tagOf(in.Tags, "tent/spec-hash"); tag != nc.SpecHash {
				t.Errorf("%s carries the spec hash %q, its NodeConfig has %q", in.Hostname, tag, nc.SpecHash)
			}
			hashes[nc.NodeGroup] = nc.SpecHash
			isServer := nc.Role == v1alpha1.RoleServer
			if got := fileOf(nc, "/etc/nomad.d/01-gossip.hcl"); (got != nil) != isServer {
				t.Errorf("%s has a gossip file: %t, want %t", in.Hostname, got != nil, isServer)
			} else if isServer && !bytes.Contains(got.Content, gossip) {
				t.Errorf("the gossip file of %s does not hold the stored gossip key", in.Hostname)
			}
			if got := fileOf(nc, "/etc/nomad.d/10-node.hcl"); isServer &&
				!strings.Contains(string(got.Content), "bootstrap_expect = 3") {
				t.Errorf("%s does not bootstrap with 3 servers", in.Hostname)
			}
			claims := introClaims(t, nc)
			if isServer {
				wantCertificate(t, nc, ca, "server.global.nomad")
				if claims != nil {
					t.Errorf("%s has an intro token", in.Hostname)
				}
				continue
			}
			wantCertificate(t, nc, ca, "client.global.nomad")
			want := map[string]string{"nomad_node_name": in.Hostname, "nomad_node_pool": "default"}
			if diff := cmp.Diff(want, claims); diff != "" {
				t.Errorf("the intro token of %s (-want +got):\n%s", in.Hostname, diff)
			}
		}
		if hashes["servers"] == hashes["workers"] {
			t.Error("the servers and the clients have the same spec hash")
		}

		tokens := w.Tokens()
		if len(tokens) != 3 {
			t.Errorf("%d Nomad clients were made, want one for each of the 3 servers", len(tokens))
		}
		for i, tok := range tokens {
			if !bytes.Equal(tok, get(t, svc.Store, aclPath)) {
				t.Errorf("the token of Nomad client %d is not the stored bootstrap secret", i)
			}
		}
		wantCalls := []string{
			"nomad Leader (prod-servers-0)",
			bootstrapLine(t, svc, "prod-servers-0"),
			"nomad Health (prod-servers-0)",
			introLine("prod-workers-0", "prod-servers-0"), "nomad Nodes (prod-servers-0)",
			introLine("prod-workers-1", "prod-servers-0"), "nomad Nodes (prod-servers-0)",
		}
		if diff := cmp.Diff(wantCalls, nomadLines(w)); diff != "" {
			t.Errorf("the Nomad calls (-want +got):\n%s", diff)
		}
		if first := w.Log()[0]; countOf(f.Calls()[:first.Cloud], "CreateInstance") != 3 {
			t.Errorf("Nomad was called after %d creates, want the 3 servers' first", countOf(f.Calls()[:first.Cloud],
				"CreateInstance"))
		}

		wantNomadConfigs(t, svc, f, w, now)
		wantNomadEvents(t, events, f)

		wantProgress := slices.Concat(
			nodeSteps("create", "prod-servers-0"), nodeSteps("create", "prod-servers-1"),
			nodeSteps("create", "prod-servers-2"),
			[]string{
				"nomad started leader", "nomad done leader", "nomad started bootstrap", "nomad done bootstrap",
				"nomad started healthy", "nomad done healthy",
			},
			nodeSteps("create", "prod-workers-0"),
			[]string{"nomad started register prod-workers-0", "nomad done register prod-workers-0"},
			nodeSteps("create", "prod-workers-1"),
			[]string{"nomad started register prod-workers-1", "nomad done register prod-workers-1"},
		)
		if diff := cmp.Diff(wantProgress, onlyNomadAndNodes(*progress)); diff != "" {
			t.Errorf("the progress (-want +got):\n%s", diff)
		}

		// A second update changes nothing, calls Nomad not at all and only reads the cloud.
		cloudCalls, nomadCalls := len(f.Calls()), len(w.Log())
		plan = mustUpdate(t, svc)
		if plan.HasChanges() || !plan.Applied {
			t.Errorf("the second update has changes or was not applied (%t):\n%s", plan.Applied, planText(t, plan))
		}
		if n := len(w.Log()); n != nomadCalls {
			t.Errorf("the second update made %d Nomad calls, want none", n-nomadCalls)
		}
		for _, c := range f.Calls()[cloudCalls:] {
			if !strings.HasPrefix(c.Name, "List") && c.Name != "AvailablePlans" {
				t.Errorf("the second update called %s %s", c.Name, c.Arg)
			}
		}
	})
}

// wantNomadConfigs fails the test unless the service made one Nomad client for each server, in the order of their
// names, at its public address on the API port, with the region, the cluster's CA and a certificate of an operator, for
// client authentication, made at now that lasts 24 hours.
func wantNomadConfigs(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld, now time.Time) {
	t.Helper()
	var want []string
	for _, in := range f.Instances() {
		if in.Hostname == "prod-servers-0" || in.Hostname == "prod-servers-1" || in.Hostname == "prod-servers-2" {
			want = append(want, in.MainIP+":4646")
		}
	}
	var got []string
	roots := x509.NewCertPool()
	roots.AddCert(storedCA(t, svc.Store).Certificate())
	for _, cfg := range w.Configs() {
		got = append(got, cfg.Address)
		if cfg.Region != "global" {
			t.Errorf("the Nomad client of %s has the region %q, want global", cfg.Address, cfg.Region)
		}
		if !bytes.Equal(cfg.CA, get(t, svc.Store, caBundlePath)) {
			t.Errorf("the Nomad client of %s has a CA that is not the stored bundle", cfg.Address)
		}
		if len(cfg.Cert.Key) == 0 {
			t.Errorf("the Nomad client of %s has no key", cfg.Address)
		}
		block, _ := pem.Decode(cfg.Cert.Cert)
		if block == nil {
			t.Fatalf("the certificate of the Nomad client of %s is not PEM", cfg.Address)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the certificate of the Nomad client of %s: %v", cfg.Address, err)
		}
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Errorf("the operator certificate does not chain to the cluster's CA for client authentication: %v", err)
		}
		if diff := cmp.Diff([]string{"cli.global.nomad"}, cert.DNSNames); diff != "" {
			t.Errorf("the names of the operator certificate (-want +got):\n%s", diff)
		}
		if !cert.NotAfter.Equal(now.Add(24 * time.Hour)) {
			t.Errorf("the operator certificate ends at %s, want 24 hours after %s", cert.NotAfter, now)
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the addresses of the Nomad clients (-want +got):\n%s", diff)
	}
}

// wantNomadEvents fails the test unless the done events of the Nomad step show the leader that the fake gives, the
// address of the first server on the RPC port, and the three voters that it waited for.
func wantNomadEvents(t *testing.T, events []app.Progress, f *vultrfake.Fake) {
	t.Helper()
	leader := f.Instances()[0].MainIP + ":4647"
	seen := map[app.NomadAction]bool{}
	for _, p := range events {
		if p.Nomad == nil || p.Step != app.NodeDone {
			continue
		}
		seen[p.Nomad.Action] = true
		switch p.Nomad.Action {
		case app.NomadLeader:
			if p.Nomad.Leader != leader {
				t.Errorf("the leader wait is done with the leader %q, want %q", p.Nomad.Leader, leader)
			}
		case app.NomadHealthy:
			if p.Nomad.Voters != 3 {
				t.Errorf("the health wait is done with %d voters, want 3", p.Nomad.Voters)
			}
		}
	}
	if !seen[app.NomadLeader] || !seen[app.NomadHealthy] {
		t.Errorf("the events hold a done leader wait: %t, a done health wait: %t, want both", seen[app.NomadLeader],
			seen[app.NomadHealthy])
	}
}

// countOf returns how many of the calls are of the method name.
func countOf(calls []vultrfake.Call, name string) int {
	n := 0
	for _, c := range calls {
		if c.Name == name {
			n++
		}
	}
	return n
}

// TestUpdateBuildsACombinedCluster builds a cluster of three combined nodes: they join like servers, take no intro
// token, and each registers after the servers are healthy. A cluster of one combined node bootstraps with one server.
func TestUpdateBuildsACombinedCluster(t *testing.T) {
	t.Run("three nodes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t, keyedClusterYAML, combinedYAML)

			plan := mustUpdate(t, svc)

			if want := (&app.NomadStep{Bootstrap: true, Servers: 3}); !cmp.Equal(plan.Nomad, want) {
				t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
			}
			wantSeeds := [][]string{nil, {"10.64.0.3"}, {"10.64.0.3", "10.64.0.4"}}
			for i, name := range []string{"prod-all-0", "prod-all-1", "prod-all-2"} {
				nc := configOf(t, f, name)
				if diff := cmp.Diff(wantSeeds[i], seedOf(nc)); diff != "" {
					t.Errorf("the seed of %s (-want +got):\n%s", name, diff)
				}
				if claims := introClaims(t, nc); claims != nil {
					t.Errorf("%s has an intro token", name)
				}
				wantCertificate(t, nc, storedCA(t, svc.Store), "server.global.nomad", "client.global.nomad")
			}
			wantCalls := []string{
				"nomad Leader (prod-all-0)", bootstrapLine(t, svc, "prod-all-0"), "nomad Health (prod-all-0)",
				"nomad Nodes (prod-all-0)", "nomad Nodes (prod-all-0)", "nomad Nodes (prod-all-0)",
			}
			if diff := cmp.Diff(wantCalls, nomadLines(w)); diff != "" {
				t.Errorf("the Nomad calls (-want +got):\n%s", diff)
			}
			wantConverged(t, svc)
		})
	})
	t.Run("one node", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, _ := newRelease(t, keyedClusterYAML, edit(t, combinedYAML, "size: 3", "size: 1"))

			plan := mustUpdate(t, svc)

			if want := (&app.NomadStep{Bootstrap: true, Servers: 1}); !cmp.Equal(plan.Nomad, want) {
				t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
			}
			nc := configOf(t, f, "prod-all-0")
			if got := fileOf(nc, "/etc/nomad.d/10-node.hcl"); !strings.Contains(string(got.Content), "bootstrap_expect = 1") {
				t.Errorf("the node does not bootstrap with 1 server:\n%s", got.Content)
			}
			if len(nc.Join.Servers) != 0 {
				t.Errorf("the only node has the seed %v, want none", seedOf(nc))
			}
			wantConverged(t, svc)
		})
	})
}

// errBoom is an error of a Nomad call that no server or time changes.
var errBoom = errors.New("boom")

// errNotReady is an error of a Nomad call that another server may answer.
var errNotReady = fmt.Errorf("test: %w", nomadops.ErrNotReady)

// TestUpdateMovesToTheNextServer fails the first call to the first server with a not-ready error: the call goes to
// the second server, and the later calls start there.
func TestUpdateMovesToTheNextServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w := newRelease(t)
		w.Fail(t, "Leader", errNotReady)

		mustUpdate(t, svc)

		wantCalls := []string{
			"nomad Leader (prod-servers-0)", "nomad Leader (prod-servers-1)",
			bootstrapLine(t, svc, "prod-servers-1"), "nomad Health (prod-servers-1)",
			introLine("prod-workers-0", "prod-servers-1"), "nomad Nodes (prod-servers-1)",
			introLine("prod-workers-1", "prod-servers-1"), "nomad Nodes (prod-servers-1)",
		}
		if diff := cmp.Diff(wantCalls, nomadLines(w)); diff != "" {
			t.Errorf("the Nomad calls (-want +got):\n%s", diff)
		}
	})
}

// TestUpdateResumesAfter cuts the build of the example cluster at a point of its flow. The next run plans what is left
// and finishes: one instance per node, the mark stored, a plan without changes.
func TestUpdateResumesAfter(t *testing.T) {
	_, _, want := buildExample(t)
	_, template := exampleStore(t)
	const leader = "nomad Leader (prod-servers-0)"
	for _, tc := range []struct {
		name  string
		cut   string // the call at which the run is cut
		after bool   // the call is carried out and its answer is lost; otherwise the run is cut before it
		plan  []string
		nomad *app.NomadStep
		// bootstraps is how many Bootstrap calls the cut run and the next one make together, and nodes how many Nodes
		// calls reach Nomad.
		bootstraps, nodes int
		// progress, unless nil, is the node and Nomad progress of the next run.
		progress []string
	}{
		{name: "the first server", cut: "CreateInstance prod-servers-1", plan: []string{
			"create prod-servers-1", "create prod-servers-2", "create prod-workers-0", "create prod-workers-1",
		}, nomad: &app.NomadStep{Bootstrap: true, Servers: 3}, bootstraps: 1, nodes: 2},
		{name: "every server, before the leader", cut: leader, plan: []string{
			"create prod-workers-0", "create prod-workers-1",
		}, nomad: &app.NomadStep{Bootstrap: true, Servers: 3}, bootstraps: 1, nodes: 2},
		{name: "the health wait, before the mark", cut: "nomad Health (prod-servers-0)", plan: []string{
			"create prod-workers-0", "create prod-workers-1",
		}, nomad: &app.NomadStep{Bootstrap: true, Servers: 3}, bootstraps: 2, nodes: 2},
		{name: "the mark, before the clients", cut: "nomad IntroToken prod-workers-0 default 30m0s (prod-servers-0)",
			plan: []string{"create prod-workers-0", "create prod-workers-1"}, bootstraps: 1, nodes: 2},
		{name: "a client's create, before its registration", cut: "nomad Nodes (prod-servers-0)",
			plan: []string{"create prod-workers-1"}, bootstraps: 1, nodes: 1},
		{name: "a client's create that lost its answer", cut: "CreateInstance prod-workers-0", after: true,
			plan: []string{"wait prod-workers-0", "create prod-workers-1"}, bootstraps: 1, nodes: 2,
			progress: slices.Concat(
				nodeSteps("wait", "prod-workers-0"),
				[]string{"nomad started register prod-workers-0", "nomad done register prod-workers-0"},
				nodeSteps("create", "prod-workers-1"),
				[]string{"nomad started register prod-workers-1", "nomad done register prod-workers-1"},
			)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := newExampleFrom(t, template)
				wrote := runCut(t, f, w, svc.Store, cutCase{key: tc.cut, n: 1, after: tc.after}, func(ctx context.Context) error {
					_, err := svc.Update(ctx, "prod", true)
					return err
				})

				plan, err := svc.Update(t.Context(), "prod", false)
				if err != nil {
					t.Fatalf("Update without apply: %v", err)
				}
				var got []string
				for _, c := range plan.Nodes {
					got = append(got, c.Action.String()+" "+c.Name)
				}
				if diff := cmp.Diff(tc.plan, got); diff != "" {
					t.Errorf("the node changes of the next run (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(tc.nomad, plan.Nomad); diff != "" {
					t.Errorf("the Nomad step of the next run (-want +got):\n%s", diff)
				}

				progress := recordProgress(svc)
				mustUpdate(t, svc)
				wantBuilt(t, svc, f, w, want, wrote)
				if n := countNomad(w, "Bootstrap"); n != tc.bootstraps {
					t.Errorf("%d Bootstrap calls reached Nomad, want %d", n, tc.bootstraps)
				}
				if n := countNomad(w, "Nodes"); n != tc.nodes {
					t.Errorf("%d Nodes calls reached Nomad, want %d", n, tc.nodes)
				}
				if tc.progress != nil {
					if diff := cmp.Diff(tc.progress, onlyNomadAndNodes(*progress)); diff != "" {
						t.Errorf("the progress of the next run (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}

// TestUpdateStoresNoMarkBeforeTheServersAreHealthy fails the health wait of the build. The run creates no client and
// stores no mark, so the next run repeats the leader wait, the bootstrap and the health wait before the clients.
func TestUpdateStoresNoMarkBeforeTheServersAreHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		w.Fail(t, "Health", errBoom)

		_, err := svc.Update(t.Context(), "prod", true)

		if !errors.Is(err, errBoom) {
			t.Fatalf("Update = %v, want an error that matches %v", err, errBoom)
		}
		if slices.Contains(list(t, svc.Store, ""), markPath) {
			t.Errorf("the failed run stored %s", markPath)
		}
		wantNodes(t, f, server(0), server(1), server(2))
		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if want := (&app.NomadStep{Bootstrap: true, Servers: 3}); !cmp.Equal(plan.Nomad, want) {
			t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
		}
		before := len(w.Log())

		mustUpdate(t, svc)

		want := []string{
			"nomad Leader (prod-servers-0)", bootstrapLine(t, svc, "prod-servers-0"), "nomad Health (prod-servers-0)",
			introLine("prod-workers-0", "prod-servers-0"), "nomad Nodes (prod-servers-0)",
			introLine("prod-workers-1", "prod-servers-0"), "nomad Nodes (prod-servers-0)",
		}
		if diff := cmp.Diff(want, nomadLines(w)[before:]); diff != "" {
			t.Errorf("the Nomad calls of the next run (-want +got):\n%s", diff)
		}
		if !slices.Contains(list(t, svc.Store, ""), markPath) {
			t.Errorf("the store holds no %s", markPath)
		}
		wantNodes(t, f, allNodes...)
		wantConverged(t, svc)
	})
}

// TestUpdateLosesTheBootstrapAnswer loses the answer of the Bootstrap call. With three servers the next server finds
// the bootstrap done. With one server the run fails, no mark is stored, and the next run finishes.
func TestUpdateLosesTheBootstrapAnswer(t *testing.T) {
	t.Run("three servers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t)
			now := time.Now()
			svc.Now = func() time.Time { return now }
			w.LoseResponse(t, "Bootstrap")

			mustUpdate(t, svc)

			if n := countNomad(w, "Bootstrap"); n != 2 {
				t.Errorf("%d Bootstrap calls, want one that lost its answer and one more", n)
			}
			wantStored(t, svc.Store, markPath, []byte(now.UTC().Format(time.RFC3339)+"\n"))
			wantConverged(t, svc)
		})
	})
	t.Run("one server", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t, keyedClusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
			w.LoseResponse(t, "Bootstrap")

			_, err := svc.Update(t.Context(), "prod", true)

			if !errors.Is(err, nomadops.ErrNotReady) {
				t.Errorf("Update = %v, want an error that matches nomadops.ErrNotReady", err)
			}
			if slices.Contains(list(t, svc.Store, ""), markPath) {
				t.Errorf("the failed run stored %s", markPath)
			}
			wantLockFree(t, svc.Store)

			mustUpdate(t, svc)

			if !slices.Contains(list(t, svc.Store, ""), markPath) {
				t.Errorf("the store holds no %s", markPath)
			}
			if n := countNomad(w, "Bootstrap"); n != 2 {
				t.Errorf("%d Bootstrap calls, want 2", n)
			}
			wantConverged(t, svc)
		})
	})
}

// TestUpdateLosesAnIntroTokenAnswer loses the answer of an IntroToken call: the call goes to the next server, and the
// node boots with a token for its own name.
func TestUpdateLosesAnIntroTokenAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := newRelease(t)
		w.LoseResponse(t, "IntroToken")

		mustUpdate(t, svc)

		if n := countNomad(w, "IntroToken"); n != 3 {
			t.Errorf("%d IntroToken calls, want one for each of the 2 clients and one more", n)
		}
		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			want := map[string]string{"nomad_node_name": name, "nomad_node_pool": "default"}
			if diff := cmp.Diff(want, introClaims(t, configOf(t, f, name))); diff != "" {
				t.Errorf("the intro token of %s (-want +got):\n%s", name, diff)
			}
		}
		wantConverged(t, svc)
	})
}

// TestUpdateWaitsTenMinutes ends the waits of the Nomad step at 10 minutes.
func TestUpdateWaitsTenMinutes(t *testing.T) {
	t.Run("no leader", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			w.NoLeader()
			var started time.Time
			svc.OnProgress = func(p app.Progress) {
				if p.Nomad != nil && p.Nomad.Action == app.NomadLeader && p.Step == app.NodeStarted {
					started = time.Now()
				}
			}

			_, err := svc.Update(t.Context(), "prod", true)

			const prefix = "nomad: wait for a leader: context deadline exceeded; last: "
			const hint = "; tent reaches the servers on port 4646: check spec.access.api"
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), hint) ||
				!errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Update = %v, want an error that starts with %q and ends with %q", err, prefix, hint)
			}
			if d := time.Since(started); d < 10*time.Minute || d >= 10*time.Minute+time.Second {
				t.Errorf("the wait took %v, want 10m0s", d)
			}
			wantNodes(t, f, server(0), server(1), server(2))
			wantLockFree(t, svc.Store)
		})
	})
	t.Run("servers that stay unhealthy", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			w.Unhealthy()
			var started time.Time
			svc.OnProgress = func(p app.Progress) {
				if p.Nomad != nil && p.Nomad.Action == app.NomadHealthy && p.Step == app.NodeStarted {
					started = time.Now()
				}
			}

			_, err := svc.Update(t.Context(), "prod", true)

			const prefix = "nomad: wait for healthy servers with at least 3 voters: context deadline exceeded; last: "
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Update = %v, want an error that starts with %q and matches context.DeadlineExceeded", err,
					prefix)
			}
			if d := time.Since(started); d < 10*time.Minute || d >= 10*time.Minute+time.Second {
				t.Errorf("the wait took %v, want 10m0s", d)
			}
			wantNodes(t, f, server(0), server(1), server(2))
			wantLockFree(t, svc.Store)
		})
	})
	t.Run("a client that never registers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			w.Withhold("prod-workers-1")
			var started time.Time
			var progress []string
			svc.OnProgress = func(p app.Progress) {
				progress = append(progress, progressLine(p))
				if p.Nomad != nil && p.Nomad.Action == app.NomadRegister && p.Step == app.NodeStarted {
					started = time.Now()
				}
			}

			_, err := svc.Update(t.Context(), "prod", true)

			const prefix = "nomad: wait for node prod-workers-1: context deadline exceeded; last: "
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Update = %v, want an error that starts with %q and matches context.DeadlineExceeded", err,
					prefix)
			}
			if d := time.Since(started); d < 10*time.Minute || d >= 10*time.Minute+time.Second {
				t.Errorf("the wait took %v, want 10m0s", d)
			}
			last := progress[len(progress)-1]
			if !strings.HasPrefix(last, "nomad failed register prod-workers-1: ") {
				t.Errorf("the last progress line is %q, want the failed registration of prod-workers-1", last)
			}
			wantNodes(t, f, allNodes...)
			wantLockFree(t, svc.Store)

			// The machine is ready, so the next run does not look for it again.
			wantConverged(t, svc)
		})
	})
}

// TestUpdateNeedsANomadClient fails an update that needs Nomad when the service has none, before it changes anything:
// the first build, and a built cluster that gets only a client, which needs the intro token.
func TestUpdateNeedsANomadClient(t *testing.T) {
	t.Run("a first build", func(t *testing.T) {
		svc, f, _ := newRelease(t)
		svc.Nomad = nil
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "no Nomad client is set up")
		if diff := cmp.Diff([]string{lockPath, lockPath}, rec.writes); diff != "" {
			t.Errorf("writes to the store (-want +got):\n%s", diff)
		}
		wantOnlyReads(t, f)
		wantNodes(t, f)
	})
	t.Run("a client alone", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, _ := newRelease(t)
			mustUpdate(t, svc)
			mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))
			svc.Nomad = nil
			rec := &writeLog{Store: svc.Store}
			svc.Store = rec

			_, err := svc.Update(t.Context(), "prod", true)

			wantError(t, err, "no Nomad client is set up")
			if diff := cmp.Diff([]string{lockPath, lockPath}, rec.writes); diff != "" {
				t.Errorf("writes to the store (-want +got):\n%s", diff)
			}
			if n := countOf(f.Calls(), "CreateInstance"); n != 5 {
				t.Errorf("%d instances were created, want the 5 of the first build", n)
			}
		})
	})
}

// TestUpdateGivesTheAssetsItsClock verifies the signature of Nomad's release files at the service's time when the
// assets have no clock of their own.
func TestUpdateGivesTheAssetsItsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := newRelease(t)
		svc.Assets.Now = nil
		svc.Now = assetstest.Now

		mustUpdate(t, svc)

		wantNodes(t, f, allNodes...)
	})
}

// TestUpdateReadsReleaseFilesOnlyForNodes reads Nomad's release files once for an update that creates nodes, and not at
// all for one that creates none.
func TestUpdateReadsReleaseFilesOnlyForNodes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := newRelease(t)
		sites := withAssets(svc)

		mustUpdate(t, svc)

		sums := 0
		for _, u := range sites.URLs() {
			if strings.Contains(u, "SHA256SUMS") && !strings.HasSuffix(u, ".sig") {
				sums++
			}
		}
		if sums != 1 {
			t.Errorf("Nomad's SHA256SUMS was read %d times by an update that made 5 nodes, want once", sums)
		}
		read := len(sites.URLs())
		wantConverged(t, svc)
		mustUpdate(t, svc)
		if n := len(sites.URLs()); n != read {
			t.Errorf("%d release files were read for a cluster that needs no node", n-read)
		}
		wantNodes(t, f, allNodes...)
	})
}

// TestUpdatePlansTheNomadStep plans the Nomad step of a built cluster in the cases that give it one.
func TestUpdatePlansTheNomadStep(t *testing.T) {
	t.Run("ready servers and no mark", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t)
			mustUpdate(t, svc)
			if err := svc.Store.Delete(t.Context(), markPath); err != nil {
				t.Fatal(err)
			}
			before := len(w.Log())

			plan, err := svc.Update(t.Context(), "prod", false)
			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}

			const want = "Nomad: bootstrap the ACL system and wait for 3 healthy servers.\n" +
				"State: nomad/bootstrapped will be written.\n"
			if got := planText(t, plan); got != want {
				t.Errorf("the plan is\n%s\nwant\n%s", got, want)
			}
			if !plan.HasChanges() {
				t.Error("the plan has no changes")
			}
			mustUpdate(t, svc)
			if got := nomadLines(w)[before:]; len(got) != 3 || !strings.HasPrefix(got[1], "nomad Bootstrap") {
				t.Errorf("the update made the Nomad calls %v, want Leader, Bootstrap and Health", got)
			}
			wantConverged(t, svc)
		})
	})
	t.Run("a server that was deleted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			mustUpdate(t, svc)
			if err := f.DeleteInstance(t.Context(), "instance-3"); err != nil {
				t.Fatal(err)
			}
			before := len(w.Log())

			plan, err := svc.Update(t.Context(), "prod", false)
			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}

			wantNodeChanges(t, plan, createOf("servers", v1alpha1.RoleServer, 2))
			if want := (&app.NomadStep{Servers: 3}); !cmp.Equal(plan.Nomad, want) {
				t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
			}
			mustUpdate(t, svc)
			nc := configOf(t, f, "prod-servers-2")
			if diff := cmp.Diff([]string{"10.64.0.3", "10.64.0.4"}, seedOf(nc)); diff != "" {
				t.Errorf("the seed of the new server (-want +got):\n%s", diff)
			}
			for _, line := range nomadLines(w)[before:] {
				if strings.HasPrefix(line, "nomad Bootstrap") {
					t.Errorf("the update bootstrapped again: %s", line)
				}
			}
			wantConverged(t, svc)
		})
	})
	t.Run("every machine gone while the mark stays", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			mustUpdate(t, svc)
			for _, in := range f.Instances() {
				if err := f.DeleteInstance(t.Context(), in.ID); err != nil {
					t.Fatal(err)
				}
			}

			plan, err := svc.Update(t.Context(), "prod", false)
			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}

			if want := (&app.NomadStep{Bootstrap: true, Servers: 3}); !cmp.Equal(plan.Nomad, want) {
				t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
			}
			rec := &writeLog{Store: svc.Store}
			svc.Store = rec
			var firstCreate sync.Once
			markedAtCreate := false
			f.SetHook(func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
				if c.Name == "CreateInstance" {
					firstCreate.Do(func() { markedAtCreate = slices.Contains(list(t, svc.Store, ""), markPath) })
				}
				return next(ctx)
			})

			mustUpdate(t, svc)

			f.SetHook(nil)
			if markedAtCreate {
				t.Error("the stale mark was still stored when the first server was created")
			}
			if n := slices.Index(rec.writes, markPath); n < 0 || slices.Index(rec.writes[n+1:], markPath) < 0 {
				t.Errorf("writes to the store %v, want the delete of the stale mark and then the write of a new one",
					rec.writes)
			}
			wantNodes(t, f, allNodes...)
			if n := countNomad(w, "Bootstrap"); n != 2 {
				t.Errorf("%d Bootstrap calls reached Nomad, want one for each cluster", n)
			}
			wantConverged(t, svc)
		})
	})
	t.Run("the stale mark cannot be deleted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, _ := newRelease(t)
			mustUpdate(t, svc)
			for _, in := range f.Instances() {
				if err := f.DeleteInstance(t.Context(), in.ID); err != nil {
					t.Fatal(err)
				}
			}
			store := svc.Store
			svc.Store = &cutStore{Store: store, path: markPath}

			_, err := svc.Update(t.Context(), "prod", true)

			wantError(t, err, "delete "+markPath+": "+cutError("delete", markPath).Error())
			wantNodes(t, f)
			svc.Store = store
			mustUpdate(t, svc)
			wantNodes(t, f, allNodes...)
			wantConverged(t, svc)
		})
	})
	t.Run("a server that a cut run created and a new run waits for", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t)
			mustUpdate(t, svc)
			if err := f.DeleteInstance(t.Context(), "instance-3"); err != nil {
				t.Fatal(err)
			}
			cut := cutCase{key: "CreateInstance prod-servers-2", n: 1, after: true}
			runCut(t, f, w, svc.Store, cut, func(ctx context.Context) error {
				_, err := svc.Update(ctx, "prod", true)
				return err
			})
			before := len(w.Log())

			plan, err := svc.Update(t.Context(), "prod", false)
			if err != nil {
				t.Fatalf("Update without apply: %v", err)
			}

			if got := plan.Nodes; len(got) != 1 || got[0].Action != app.NodeWait || got[0].Name != "prod-servers-2" {
				t.Errorf("the node changes are %+v, want the wait for prod-servers-2", got)
			}
			if want := (&app.NomadStep{Servers: 3}); !cmp.Equal(plan.Nomad, want) {
				t.Errorf("the plan's Nomad step is %+v, want %+v", plan.Nomad, want)
			}
			mustUpdate(t, svc)
			want := []string{"nomad Leader (prod-servers-0)", "nomad Health (prod-servers-0)"}
			if diff := cmp.Diff(want, nomadLines(w)[before:]); diff != "" {
				t.Errorf("the Nomad calls of the update (-want +got):\n%s", diff)
			}
			wantConverged(t, svc)
		})
	})
}

// withExtraConfig returns the cluster spec with n bytes of text in the extraConfig of role, server or client, that gzip
// does not shrink. The seed makes it differ from other text that comes from the zero seed, such as the stand-in intro
// token: gzip would fold a copy of that.
func withExtraConfig(t *testing.T, role string, n int, seed byte) string {
	t.Helper()
	raw := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(raw)
	noise := base64.RawURLEncoding.EncodeToString(raw)[:n]
	extra := "nomad:\n    extraConfig:\n      " + role + ": \"# " + noise + "\\n\"\n  sshKeys:"
	return edit(t, keyedClusterYAML, "sshKeys:", extra)
}

// TestUpdatePlanFailsForTooMuchUserData fails the plan of an update whose nodes would boot with more user data than a
// provider takes, before it writes anything, with or without apply.
func TestUpdatePlanFailsForTooMuchUserData(t *testing.T) {
	svc, f, _ := newRelease(t, withExtraConfig(t, "server", 30<<10, 0), serversYAML, workersYAML)
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec

	for _, apply := range []bool{false, true} {
		_, err := svc.Update(t.Context(), "prod", apply)

		const prefix = "user data: node group servers needs "
		const suffix = " bytes, more than the 24576 that fit"
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), suffix) {
			t.Errorf("Update(apply: %t) = %v, want an error that starts with %q and ends with %q", apply, err, prefix,
				suffix)
		}
	}
	if len(rec.writes) != 0 {
		t.Errorf("writes to the store: %v", rec.writes)
	}
	wantOnlyReads(t, f)
}

// TestUpdatePlanCountsTheIntroToken fails the plan for a client whose user data fits until the intro token is added:
// the client config is about 1 KiB under the limit without the token, and the token adds about 2 KiB.
func TestUpdatePlanCountsTheIntroToken(t *testing.T) {
	svc, f, _ := newRelease(t, withExtraConfig(t, "client", 18000, 1), serversYAML, workersYAML)
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec

	_, err := svc.Update(t.Context(), "prod", false)

	const prefix = "user data: node group workers needs "
	const suffix = " bytes, more than the 24576 that fit"
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), suffix) {
		t.Errorf("Update = %v, want an error that starts with %q and ends with %q", err, prefix, suffix)
	}
	if len(rec.writes) != 0 {
		t.Errorf("writes to the store: %v", rec.writes)
	}
	wantOnlyReads(t, f)
}

// TestUpdateFindsTentNodeOnlyForNodes fails a plan that creates nodes with a development build that has no tent-node,
// and passes a plan that creates none.
func TestUpdateFindsTentNodeOnlyForNodes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
		f, _ := withCloud(svc)
		withNomad(svc, f)
		dev := svc.Assets
		svc.Assets.DevURL, svc.Assets.DevSHA256 = "", ""

		_, err := svc.Update(t.Context(), "prod", false)

		wantError(t, err, "find tent-node: tent dev is a development build, so no release holds its tent-node: set "+
			"TENT_NODE_URL and TENT_NODE_SHA256 to a tent-node built from the same commit")

		svc.Assets = dev
		mustUpdate(t, svc)
		svc.Assets = assets.Options{Now: dev.Now, Client: dev.Client}
		wantConverged(t, svc)
	})
}

// TestUpdateWarnsOfDevelopmentVariables warns once, before the first change, that a release build ignores TENT_NODE_URL
// and TENT_NODE_SHA256.
func TestUpdateWarnsOfDevelopmentVariables(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := newRelease(t)
		var events []string
		svc.OnWarning = func(w string) { events = append(events, "warning: "+w) }
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }

		mustUpdate(t, svc)

		const want = "warning: TENT_NODE_URL and TENT_NODE_SHA256 are set, but tent v0.5.0 is a release build and " +
			"ignores them: its nodes download the tent-node of release v0.5.0"
		isWarning := func(e string) bool { return strings.HasPrefix(e, "warning: ") }
		first := slices.IndexFunc(events, func(e string) bool { return !isWarning(e) })
		if first < 0 || slices.ContainsFunc(events[first:], isWarning) {
			t.Fatalf("the warnings do not all come before the first change:\n%s", strings.Join(events, "\n"))
		}
		var told []string
		for _, e := range events[:first] {
			if e == want {
				told = append(told, e)
			}
		}
		if len(told) != 1 {
			t.Errorf("the warnings before the first change are %q, want %q once", events[:first], want)
		}
	})
}
