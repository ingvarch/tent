package rollout_test

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

const (
	oldHash = "old"
	newHash = "new"
)

var epoch = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func ip(i int) netip.Addr { return netip.AddrFrom4([4]byte{10, 64, 0, byte(i)}) }

// baseState is a cluster with one healthy server, the client group workers of size 2 (surge 1, unavailable 0) and
// no workers yet; addWorker adds them.
func baseState() rollout.State {
	server := rollout.Machine{
		ID: "m-0", Name: "prod-servers-0", Group: "servers", Role: v1alpha1.RoleServer, Zone: "ams",
		SpecHash: newHash, PrivateIP: ip(2), Ready: true, Joined: true, Created: epoch.Add(-24 * time.Hour),
	}
	return rollout.State{
		Cluster: "prod",
		Groups: []rollout.Group{{
			Name: "workers", Role: v1alpha1.RoleClient, Size: 2, Zones: []string{"ams", "fra"}, SpecHash: newHash,
			MaxSurge: 1, MaxUnavailable: 0, DrainTimeout: time.Hour,
		}},
		Machines: []rollout.Machine{server},
		Nomad: rollout.Nomad{
			Healthy: true,
			Servers: []rollout.Server{{
				ID: "r-0", Name: "prod-servers-0.global", Address: netip.AddrPortFrom(ip(2), 4647), Voter: true,
				Leader: true, Healthy: true, StableSince: epoch.Add(-time.Hour), Version: "2.0.7",
			}},
		},
		Version: "2.0.7",
		Refresh: time.Minute,
		Now:     epoch,
	}
}

// workerName is the name of worker i.
func workerName(i int) string { return rollout.NodeName("prod", "workers", i) }

// workerMachine is worker i as the cloud lists it when it is up: machine m-<i+1>, joined, in ams, created i hours
// after worker 0.
func workerMachine(i int, hash string) rollout.Machine {
	return rollout.Machine{
		ID: fmt.Sprintf("m-%d", i+1), Name: workerName(i), Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
		SpecHash: hash, PrivateIP: ip(3 + i), Ready: true, Joined: true,
		Created: epoch.Add(time.Duration(i-10) * time.Hour),
	}
}

// nodeOf is the node that Nomad lists for a machine that runs: ready and eligible, with ID n-<machine ID>.
func nodeOf(m rollout.Machine) rollout.Node {
	return rollout.Node{
		ID: "n-" + m.ID, Name: m.Name, Address: m.PrivateIP, Status: "ready", Eligible: true, Version: "2.0.7",
	}
}

// addWorker adds worker i with its node, up and joined.
func addWorker(s *rollout.State, i int, hash string) {
	m := workerMachine(i, hash)
	s.Machines = append(s.Machines, m)
	s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(m))
}

// addOrphan adds a node that no machine has, at address 10.64.0.<index>.
func addOrphan(s *rollout.State, name, status string, index int) {
	s.Nomad.Nodes = append(s.Nomad.Nodes, rollout.Node{
		ID: "n-gone-" + name, Name: name, Address: ip(index), Status: status, Eligible: true, Version: "2.0.7",
	})
}

// machineOf returns the listed machine of that name for the test to change.
func machineOf(t *testing.T, s *rollout.State, name string) *rollout.Machine {
	t.Helper()
	for i := range s.Machines {
		if s.Machines[i].Name == name {
			return &s.Machines[i]
		}
	}
	t.Fatalf("no machine %s", name)
	return nil
}

// nodeNamed returns the listed node of that name for the test to change.
func nodeNamed(t *testing.T, s *rollout.State, name string) *rollout.Node {
	t.Helper()
	for i := range s.Nomad.Nodes {
		if s.Nomad.Nodes[i].Name == name {
			return &s.Nomad.Nodes[i]
		}
	}
	t.Fatalf("no node %s", name)
	return nil
}

// dropNode removes the nodes of that name from the state.
func dropNode(s *rollout.State, name string) {
	var kept []rollout.Node
	for _, n := range s.Nomad.Nodes {
		if n.Name != name {
			kept = append(kept, n)
		}
	}
	s.Nomad.Nodes = kept
}

// outcome is what a test checks of a step: what it does and to what.
type outcome struct {
	Action    rollout.Action
	Group     string
	Machine   string // the machine's name
	MachineID string
	Node      string // the node's ID
	Zone      string // of the machine
	Deadline  time.Duration
}

func outcomeOf(s rollout.Step) outcome {
	return outcome{s.Action, s.Group, s.Machine.Name, s.Machine.ID, s.Node.ID, s.Machine.Zone, s.Deadline}
}

// nextIn returns the next step of a run in the mode, failing the test on an error.
func nextIn(t *testing.T, mode rollout.Mode, s rollout.State) rollout.Step {
	t.Helper()
	step, err := rollout.Next(s, mode)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return step
}

// nextRoll returns the next step of a roll, failing the test on an error.
func nextRoll(t *testing.T, s rollout.State) rollout.Step {
	t.Helper()
	return nextIn(t, rollout.Roll, s)
}

func checkOutcome(t *testing.T, got rollout.Step, want outcome) {
	t.Helper()
	if diff := cmp.Diff(want, outcomeOf(got)); diff != "" {
		t.Errorf("step %q mismatch (-want +got):\n%s", got, diff)
	}
}

// checkRefused fails unless Next refuses the state in a roll with exactly this text.
func checkRefused(t *testing.T, s rollout.State, want string) {
	t.Helper()
	checkRefusedIn(t, rollout.Roll, s, want)
}

// checkRefusedIn fails unless Next refuses the state in the mode with exactly this text.
func checkRefusedIn(t *testing.T, mode rollout.Mode, s rollout.State, want string) {
	t.Helper()
	step, err := rollout.Next(s, mode)
	if err == nil {
		t.Fatalf("Next = %q, want the refusal %q", step, want)
	}
	if !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("error %q does not match ErrRefused", err)
	}
	if err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}
}
