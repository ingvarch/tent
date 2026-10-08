package app

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// TestDrainMetaKey pins the key of the drain meta, which Nomad keeps with a node: a new key would hide every drain
// that an earlier run made.
func TestDrainMetaKey(t *testing.T) {
	t.Parallel()
	if drainMeta != "tent_machine" {
		t.Errorf("drainMeta = %q, want tent_machine", drainMeta)
	}
}

// TestNomadReadingStateServers builds one server per peer, with autopilot's entry of the same Raft ID: an entry that
// the report lacks gives false, the zero time and ""; an entry that is no peer is left out; two peers of one name stay
// apart by their IDs.
func TestNomadReadingStateServers(t *testing.T) {
	t.Parallel()
	stable := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	addr := func(host string) netip.AddrPort { return netip.MustParseAddrPort(host + ":4647") }
	reading := nomadReading{
		peers: []nomadops.Peer{
			{ID: "r-1", Name: "prod-servers-0.global", Address: addr("10.64.0.2"), Voter: true, Leader: true},
			{ID: "r-2", Name: "prod-servers-1.global", Address: addr("10.64.0.3")},
			{ID: "r-3", Name: "twin.global", Address: addr("10.64.0.4"), Voter: true},
			{ID: "r-4", Name: "twin.global", Address: addr("10.64.0.5"), Voter: true},
		},
		health: nomadops.Health{Healthy: true, FailureTolerance: 1, Servers: []nomadops.ServerHealth{
			{ID: "r-4", Healthy: false, Version: "2.0.6", StableSince: stable.Add(time.Minute)},
			{ID: "r-1", Name: "other-name.global", Healthy: true, Version: "2.0.7", StableSince: stable},
			{ID: "r-3", Healthy: true, Version: "2.0.5", StableSince: stable.Add(2 * time.Minute)},
			{ID: "r-9", Name: "prod-servers-1.global", Healthy: true, Version: "2.0.7", StableSince: stable},
		}},
	}
	got := reading.state()
	want := []rollout.Server{
		{ID: "r-1", Name: "prod-servers-0.global", Address: addr("10.64.0.2"), Voter: true, Leader: true,
			Healthy: true, StableSince: stable, Version: "2.0.7"},
		{ID: "r-2", Name: "prod-servers-1.global", Address: addr("10.64.0.3")},
		{ID: "r-3", Name: "twin.global", Address: addr("10.64.0.4"), Voter: true, Healthy: true,
			StableSince: stable.Add(2 * time.Minute), Version: "2.0.5"},
		{ID: "r-4", Name: "twin.global", Address: addr("10.64.0.5"), Voter: true, Healthy: false,
			StableSince: stable.Add(time.Minute), Version: "2.0.6"},
	}
	if diff := cmp.Diff(want, got.Servers, equateNetip); diff != "" {
		t.Errorf("Servers mismatch (-want +got):\n%s", diff)
	}
	if !got.Healthy || got.FailureTolerance != 1 {
		t.Errorf("Healthy, FailureTolerance = %v, %d, want true, 1", got.Healthy, got.FailureTolerance)
	}
}

// TestNomadReadingStateHealthFields takes Healthy and FailureTolerance from autopilot's report as they are, also when
// they are false and zero.
func TestNomadReadingStateHealthFields(t *testing.T) {
	t.Parallel()
	got := nomadReading{health: nomadops.Health{Healthy: false, FailureTolerance: 0}}.state()
	if got.Healthy || got.FailureTolerance != 0 {
		t.Errorf("Healthy, FailureTolerance = %v, %d, want false, 0", got.Healthy, got.FailureTolerance)
	}
	got = nomadReading{health: nomadops.Health{Healthy: true, FailureTolerance: 2}}.state()
	if !got.Healthy || got.FailureTolerance != 2 {
		t.Errorf("Healthy, FailureTolerance = %v, %d, want true, 2", got.Healthy, got.FailureTolerance)
	}
}

// TestNomadReadingStateMembersAndNodes maps every field of the members and of the nodes.
func TestNomadReadingStateMembersAndNodes(t *testing.T) {
	t.Parallel()
	reading := nomadReading{
		members: []nomadops.Member{
			{Name: "prod-servers-0.global", Address: netip.MustParseAddr("10.64.0.2"), Status: "alive"},
			{Name: "prod-servers-1.global", Address: netip.MustParseAddr("10.64.0.3"), Status: "failed"},
		},
		nodes: []nomadops.Node{
			{ID: "n-1", Name: "prod-workers-0", Status: "ready", Eligible: true, Version: "2.0.7",
				Address: netip.MustParseAddr("10.64.0.6")},
			{ID: "n-2", Name: "prod-workers-0", Status: "down", Draining: true, Version: "2.0.6",
				Address: netip.MustParseAddr("10.64.0.6")},
		},
	}
	got := reading.state()
	wantMembers := []rollout.Member{
		{Name: "prod-servers-0.global", Address: netip.MustParseAddr("10.64.0.2"), Status: "alive"},
		{Name: "prod-servers-1.global", Address: netip.MustParseAddr("10.64.0.3"), Status: "failed"},
	}
	if diff := cmp.Diff(wantMembers, got.Members, equateNetip); diff != "" {
		t.Errorf("Members mismatch (-want +got):\n%s", diff)
	}
	wantNodes := []rollout.Node{
		{ID: "n-1", Name: "prod-workers-0", Address: netip.MustParseAddr("10.64.0.6"), Status: "ready", Eligible: true,
			Version: "2.0.7"},
		{ID: "n-2", Name: "prod-workers-0", Address: netip.MustParseAddr("10.64.0.6"), Status: "down", Draining: true,
			Version: "2.0.6"},
	}
	if diff := cmp.Diff(wantNodes, got.Nodes, equateNetip); diff != "" {
		t.Errorf("Nodes mismatch (-want +got):\n%s", diff)
	}
}

// TestNomadReadingStateDrainedFor names the machine of a drain only when the drain completed and carries the key.
func TestNomadReadingStateDrainedFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drain nomadops.LastDrain
		want  string
	}{
		{"a complete drain with the key", nomadops.LastDrain{Status: "complete",
			Meta: map[string]string{drainMeta: "instance-7", "other": "x"}}, "instance-7"},
		{"a drain under way", nomadops.LastDrain{Status: "draining",
			Meta: map[string]string{drainMeta: "instance-7"}}, ""},
		{"a canceled drain", nomadops.LastDrain{Status: "canceled",
			Meta: map[string]string{drainMeta: "instance-7"}}, ""},
		{"a complete drain without the key", nomadops.LastDrain{Status: "complete",
			Meta: map[string]string{"other": "instance-7"}}, ""},
		{"a complete drain without meta", nomadops.LastDrain{Status: "complete"}, ""},
		{"a node that never drained", nomadops.LastDrain{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nomadReading{nodes: []nomadops.Node{{ID: "n-1", LastDrain: tc.drain}}}.state()
			if len(got.Nodes) != 1 || got.Nodes[0].DrainedFor != tc.want {
				t.Errorf("DrainedFor of %v = %+v, want %q", tc.drain, got.Nodes, tc.want)
			}
		})
	}
}

// TestRolloutMachines maps every field of every listed machine, in the order of the list.
func TestRolloutMachines(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	listed := []cloud.Instance{
		{ID: "instance-7", Name: "prod-workers-0", Cluster: "prod", Group: "workers", Role: v1alpha1.RoleClient,
			Zone: "ams", SpecHash: "abc", Op: "op-1", PrivateIP: netip.MustParseAddr("10.64.0.6"),
			PublicIP: netip.MustParseAddr("192.0.2.6"), Ready: true, Joined: true, Created: created},
		{ID: "instance-8", Name: "prod-servers-0", Group: "servers", Role: v1alpha1.RoleServer, Ready: true},
	}
	want := []rollout.Machine{
		{ID: "instance-7", Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
			SpecHash: "abc", PrivateIP: netip.MustParseAddr("10.64.0.6"), Ready: true, Joined: true, Created: created},
		{ID: "instance-8", Name: "prod-servers-0", Group: "servers", Role: v1alpha1.RoleServer, Ready: true},
	}
	if diff := cmp.Diff(want, rolloutMachines(listed), equateNetip); diff != "" {
		t.Errorf("machines mismatch (-want +got):\n%s", diff)
	}
}

// rollTestModel is a model with a server, a combined and two client groups.
func rollTestModel() *model.Cluster {
	return &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
		{Name: "servers", Role: v1alpha1.RoleServer, Size: 3, Zones: []string{"ams"}},
		{Name: "all", Role: v1alpha1.RoleCombined, Size: 1, Zones: []string{"ams"}},
		{Name: "workers", Role: v1alpha1.RoleClient, Size: 2, Zones: []string{"ams", "fra"}},
		{Name: "batch", Role: v1alpha1.RoleClient, Size: 4, Zones: []string{"fra"}},
	}}
}

func rollTestSpec(update v1alpha1.RollingUpdate) *v1alpha1.NodeGroup {
	return &v1alpha1.NodeGroup{Spec: v1alpha1.NodeGroupSpec{RollingUpdate: update}}
}

func rollTestSpecs() map[string]*v1alpha1.NodeGroup {
	return map[string]*v1alpha1.NodeGroup{
		"servers": rollTestSpec(v1alpha1.RollingUpdate{}),
		"all":     rollTestSpec(v1alpha1.RollingUpdate{DrainTimeout: "30m"}),
		"workers": rollTestSpec(v1alpha1.RollingUpdate{MaxSurge: new(2), MaxUnavailable: new(1), DrainTimeout: "1h"}),
		"batch":   rollTestSpec(v1alpha1.RollingUpdate{DrainTimeout: "90s"}),
	}
}

func rollTestBuilder() *nodeBuilder {
	tmpl := func(hash string) nodeconfig.NodeConfig { return nodeconfig.NodeConfig{SpecHash: hash} }
	return &nodeBuilder{templates: map[string]nodeconfig.NodeConfig{
		"servers": tmpl("hash-servers"), "all": tmpl("hash-all"), "workers": tmpl("hash-workers"),
		"batch": tmpl("hash-batch"),
	}}
}

// TestRolloutGroups takes size, role and zones from the model, the hash from the builder and the settings from the
// specs: a client group's two limits and drain timeout, a combined group's drain timeout alone, a server group's
// nothing. A limit that a spec leaves out is 0, and the groups come in the order of the names.
func TestRolloutGroups(t *testing.T) {
	t.Parallel()
	names := []string{"workers", "servers", "all", "batch"}
	got, err := rolloutGroups(rollTestModel(), rollTestSpecs(), rollTestBuilder(), names)
	if err != nil {
		t.Fatal(err)
	}
	want := []rollout.Group{
		{Name: "workers", Role: v1alpha1.RoleClient, Size: 2, Zones: []string{"ams", "fra"},
			SpecHash: "hash-workers", MaxSurge: 2, MaxUnavailable: 1, DrainTimeout: time.Hour},
		{Name: "servers", Role: v1alpha1.RoleServer, Size: 3, Zones: []string{"ams"}, SpecHash: "hash-servers"},
		{Name: "all", Role: v1alpha1.RoleCombined, Size: 1, Zones: []string{"ams"}, SpecHash: "hash-all",
			DrainTimeout: 30 * time.Minute},
		{Name: "batch", Role: v1alpha1.RoleClient, Size: 4, Zones: []string{"fra"}, SpecHash: "hash-batch",
			DrainTimeout: 90 * time.Second},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("groups mismatch (-want +got):\n%s", diff)
	}
}

// TestRolloutGroupsSelectsByName returns only the named groups, and none for no name.
func TestRolloutGroupsSelectsByName(t *testing.T) {
	t.Parallel()
	got, err := rolloutGroups(rollTestModel(), rollTestSpecs(), rollTestBuilder(), []string{"batch"})
	if err != nil || len(got) != 1 || got[0].Name != "batch" {
		t.Errorf("rolloutGroups(batch) = %+v, %v, want the group batch alone", got, err)
	}
	got, err = rolloutGroups(rollTestModel(), rollTestSpecs(), rollTestBuilder(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("rolloutGroups() = %+v, %v, want no group", got, err)
	}
}

// TestRolloutGroupsFailures fails for a name that the model lacks, a group whose spec is missing or nil, and a drain
// timeout that does not parse, each with the group's name.
func TestRolloutGroupsFailures(t *testing.T) {
	t.Parallel()
	noSpec := rollTestSpecs()
	delete(noSpec, "workers")
	nilSpec := rollTestSpecs()
	nilSpec["workers"] = nil
	badDrain := rollTestSpecs()
	badDrain["workers"] = rollTestSpec(v1alpha1.RollingUpdate{DrainTimeout: "soon"})
	for _, tc := range []struct {
		name  string
		specs map[string]*v1alpha1.NodeGroup
		names []string
		want  string
	}{
		{"a name that the model lacks", rollTestSpecs(), []string{"workers", "db"}, "node group db: not in the specs"},
		{"a group without a spec", noSpec, []string{"workers"}, "node group workers: no spec"},
		{"a nil spec", nilSpec, []string{"workers"}, "node group workers: no spec"},
		{"a drain timeout that does not parse", badDrain, []string{"workers"},
			`node group workers: drain timeout: time: invalid duration "soon"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rolloutGroups(rollTestModel(), tc.specs, rollTestBuilder(), tc.names)
			if err == nil || err.Error() != tc.want {
				t.Errorf("rolloutGroups error = %v, want %q", err, tc.want)
			}
			if got != nil {
				t.Errorf("rolloutGroups groups = %+v, want none with an error", got)
			}
		})
	}
}

// TestForcedMachines holds the machines of the selected groups that carry the replace label, by ID, and with force
// every machine of the selected groups; no machine of another group or of none.
func TestForcedMachines(t *testing.T) {
	t.Parallel()
	listed := []cloud.Instance{
		{ID: "instance-1", Group: "servers", Replace: true},
		{ID: "instance-2", Group: "workers", Replace: true},
		{ID: "instance-3", Group: "workers"},
		{ID: "instance-4", Group: "batch", Replace: true},
		{ID: "instance-5", Replace: true},
	}
	groups := []rollout.Group{{Name: "workers"}, {Name: "gone"}}
	for _, tc := range []struct {
		name  string
		force bool
		want  map[string]bool
	}{
		{"labelled", false, map[string]bool{"instance-2": true}},
		{"forced", true, map[string]bool{"instance-2": true, "instance-3": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, forcedMachines(listed, groups, tc.force)); diff != "" {
				t.Errorf("forced mismatch (-want +got):\n%s", diff)
			}
		})
	}
	for _, force := range []bool{false, true} {
		if got := forcedMachines(listed, nil, force); len(got) != 0 {
			t.Errorf("forced for no group (force %v) = %v, want none", force, got)
		}
	}
}

// readStub is a Nomad API that answers the four reads of an observation, records their order and fails the one that
// failOn names.
type readStub struct {
	nomadops.API
	calls  []string
	failOn string
	err    error
}

func (s *readStub) read(name string) error {
	s.calls = append(s.calls, name)
	if s.failOn == name {
		return s.err
	}
	return nil
}

func (s *readStub) Peers(context.Context) ([]nomadops.Peer, error) {
	return []nomadops.Peer{{ID: "r-1"}}, s.read("Peers")
}

func (s *readStub) Health(context.Context) (nomadops.Health, error) {
	return nomadops.Health{FailureTolerance: 1}, s.read("Health")
}

func (s *readStub) Members(context.Context) ([]nomadops.Member, error) {
	return []nomadops.Member{{Name: "m.global"}}, s.read("Members")
}

func (s *readStub) Nodes(context.Context) ([]nomadops.Node, error) {
	return []nomadops.Node{{ID: "n-1"}}, s.read("Nodes")
}

// TestReadNomadOrder reads the Raft configuration, autopilot's report, the members and the nodes, in that order, and
// returns each answer in its own field.
func TestReadNomadOrder(t *testing.T) {
	t.Parallel()
	stub := &readStub{}
	got, err := readNomad(t.Context(), stub)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"Peers", "Health", "Members", "Nodes"}, stub.calls); diff != "" {
		t.Errorf("calls mismatch (-want +got):\n%s", diff)
	}
	want := nomadReading{
		peers:   []nomadops.Peer{{ID: "r-1"}},
		health:  nomadops.Health{FailureTolerance: 1},
		members: []nomadops.Member{{Name: "m.global"}},
		nodes:   []nomadops.Node{{ID: "n-1"}},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(nomadReading{}), equateNetip); diff != "" {
		t.Errorf("reading mismatch (-want +got):\n%s", diff)
	}
}

// TestReadNomadFailures names the read that failed, keeps its error visible to errors.Is and reads nothing after it.
func TestReadNomadFailures(t *testing.T) {
	t.Parallel()
	boom := nomadops.ErrNotReady
	for _, tc := range []struct {
		failOn    string
		wantText  string
		wantCalls []string
	}{
		{"Peers", "read the Raft configuration: " + boom.Error(), []string{"Peers"}},
		{"Health", "read autopilot's report: " + boom.Error(), []string{"Peers", "Health"}},
		{"Members", "read the gossip members: " + boom.Error(), []string{"Peers", "Health", "Members"}},
		{"Nodes", "read the client nodes: " + boom.Error(), []string{"Peers", "Health", "Members", "Nodes"}},
	} {
		t.Run(tc.failOn, func(t *testing.T) {
			stub := &readStub{failOn: tc.failOn, err: boom}
			got, err := readNomad(t.Context(), stub)
			if err == nil || err.Error() != tc.wantText || !errors.Is(err, boom) {
				t.Errorf("readNomad error = %v, want %q wrapping %v", err, tc.wantText, boom)
			}
			if diff := cmp.Diff(nomadReading{}, got, cmp.AllowUnexported(nomadReading{}), equateNetip); diff != "" {
				t.Errorf("reading on error (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantCalls, stub.calls); diff != "" {
				t.Errorf("calls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
