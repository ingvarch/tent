package rollout_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

func TestNextRejectsModesItCannotRun(t *testing.T) {
	tests := []struct {
		name string
		mode rollout.Mode
		want string
	}{
		{"no mode", 0, "rollout: unknown mode 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			addWorker(&s, 0, newHash)
			_, err := rollout.Next(s, tt.mode)
			if err == nil || err.Error() != tt.want {
				t.Errorf("Next error = %v, want %q", err, tt.want)
			}
			if errors.Is(err, rollout.ErrRefused) {
				t.Errorf("error %q matches ErrRefused, want a plain error", err)
			}
		})
	}
}

func TestNextRejectsARoleItDoesNotKnow(t *testing.T) {
	s := baseState()
	s.Groups = append(s.Groups, rollout.Group{Name: "control", Role: "bogus", Size: 3})
	_, err := rollout.Next(s, rollout.Roll)
	if err == nil || err.Error() != `rollout: unknown role "bogus"` {
		t.Errorf("Next error = %v, want the error for an unknown role", err)
	}
	if errors.Is(err, rollout.ErrRefused) {
		t.Errorf("error %q matches ErrRefused, want a plain error", err)
	}
}

func TestNextHandlesGroupsByNameAndEndsWhenAllAreDone(t *testing.T) {
	s := baseState()
	s.Groups = []rollout.Group{
		s.Groups[0],
		{Name: "web", Role: v1alpha1.RoleClient, Size: 1, Zones: []string{"ams"}, SpecHash: newHash, MaxSurge: 1,
			DrainTimeout: time.Hour},
	}
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	web := rollout.Machine{ID: "m-20", Name: "prod-web-0", Group: "web", Role: v1alpha1.RoleClient, Zone: "ams",
		SpecHash: oldHash, PrivateIP: ip(30), Ready: true, Joined: true, Created: epoch.Add(-time.Hour)}
	s.Machines = append(s.Machines, web)
	s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(web))

	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "web", Machine: "prod-web-1", Zone: "ams"})

	// With web up to date its machine is replaced and workers come next.
	s.Machines[len(s.Machines)-1].SpecHash = newHash
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "workers", Machine: "prod-workers-2",
		Zone: "ams"})

	machineOf(t, &s, workerName(0)).SpecHash = newHash
	machineOf(t, &s, workerName(1)).SpecHash = newHash
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestNextDoesNotChangeItsState(t *testing.T) {
	s := baseState()
	s.Groups = append(s.Groups, rollout.Group{Name: "aaa", Role: v1alpha1.RoleClient, Size: 1, SpecHash: newHash})
	addWorker(&s, 1, oldHash)
	addWorker(&s, 0, oldHash)
	want := baseState()
	want.Groups = append(want.Groups, rollout.Group{Name: "aaa", Role: v1alpha1.RoleClient, Size: 1, SpecHash: newHash})
	addWorker(&want, 1, oldHash)
	addWorker(&want, 0, oldHash)
	nextRoll(t, s)
	if diff := cmp.Diff(want, s, cmpopts.EquateComparable(netip.Addr{}, netip.AddrPort{})); diff != "" {
		t.Errorf("Next changed its state (-before +after):\n%s", diff)
	}
}

func TestNextRefusesVersions(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
		want  string
	}{
		{"the new version is not a version", func(_ *testing.T, s *rollout.State) { s.Version = "latest" },
			`the Nomad version of a new node is "latest", which is not a version number`},
		{"no new version", func(_ *testing.T, s *rollout.State) { s.Version = "" },
			`the Nomad version of a new node is "", which is not a version number`},
		{"a server's version is not a version", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Servers[0].Version = "two"
		}, `the Nomad version of server prod-servers-0.global is "two", which is not a version number`},
		{"a node's version is not a version", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(0)).Version = "x.y"
		}, `the Nomad version of node prod-workers-0 is "x.y", which is not a version number`},
		{"a server is newer than the new version", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Servers[0].Version = "2.1.0"
		}, "tent never moves a node to an older Nomad: the cluster is pinned to 2.0.7, and server " +
			"prod-servers-0.global runs 2.1.0"},
		{"a node is newer than the new version", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(1)).Version = "2.0.8"
		}, "tent never moves a node to an older Nomad: the cluster is pinned to 2.0.7, and node prod-workers-1 runs " +
			"2.0.8"},
		{"a version with a build suffix compares by its number", func(t *testing.T, s *rollout.State) {
			nodeNamed(t, s, workerName(1)).Version = "2.0.8+ent"
		}, "tent never moves a node to an older Nomad: the cluster is pinned to 2.0.7, and node prod-workers-1 runs " +
			"2.0.8+ent"},
		{"it comes before every other check", func(t *testing.T, s *rollout.State) {
			s.Nomad.Servers[0].Version = "2.1.0"
			machineOf(t, s, workerName(1)).Name = workerName(0)
			addOrphan(s, workerName(7), "down", 50)
		}, "tent never moves a node to an older Nomad: the cluster is pinned to 2.0.7, and server " +
			"prod-servers-0.global runs 2.1.0"},
		{"it comes before the decisions of a server group", func(_ *testing.T, s *rollout.State) {
			s.Groups = append(s.Groups, rollout.Group{Name: "control", Role: v1alpha1.RoleServer, Size: 3})
			s.Nomad.Servers[0].Version = "2.1.0"
		}, "tent never moves a node to an older Nomad: the cluster is pinned to 2.0.7, and server " +
			"prod-servers-0.global runs 2.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			addWorker(&s, 0, oldHash)
			addWorker(&s, 1, oldHash)
			tt.build(t, &s)
			checkRefused(t, s, tt.want)
		})
	}
}

func TestNextAcceptsVersionsThatAreNotInTheWay(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
	}{
		{"the same version everywhere", func(_ *testing.T, _ *rollout.State) {}},
		{"an older server and node", func(t *testing.T, s *rollout.State) {
			s.Version = "2.0.8"
			s.Nomad.Servers[0].Version = "2.0.8"
			nodeNamed(t, s, workerName(0)).Version = "2.0.5"
		}},
		{"a down node of a newer version", func(t *testing.T, s *rollout.State) {
			n := nodeNamed(t, s, workerName(1))
			n.Version = "3.0.0"
			n.Status = "down"
		}},
		{"a down node without a version", func(t *testing.T, s *rollout.State) {
			n := nodeNamed(t, s, workerName(1))
			n.Version = ""
			n.Status = "down"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			addWorker(&s, 0, oldHash)
			addWorker(&s, 1, oldHash)
			tt.build(t, &s)
			if _, err := rollout.Next(s, rollout.Roll); err != nil {
				t.Errorf("Next: %v", err)
			}
		})
	}
}

func TestNextRefusesDuplicateNames(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State)
		want  string
	}{
		{"two machines of one name", func(_ *testing.T, s *rollout.State) {
			twin := workerMachine(1, oldHash)
			twin.ID = "m-9"
			s.Machines = append(s.Machines, twin)
		}, "node group workers: machines m-2 and m-9 share the name prod-workers-1; run tent update cluster first"},
		{"three machines of one name", func(_ *testing.T, s *rollout.State) {
			for _, id := range []string{"m-9", "m-8"} {
				twin := workerMachine(1, oldHash)
				twin.ID = id
				s.Machines = append(s.Machines, twin)
			}
		}, "node group workers: machines m-2, m-8 and m-9 share the name prod-workers-1; run tent update cluster first"},
		{"the first name that repeats", func(_ *testing.T, s *rollout.State) {
			for _, i := range []int{1, 0} {
				twin := workerMachine(i, oldHash)
				twin.ID = "m-1" + string(rune('0'+i))
				s.Machines = append(s.Machines, twin)
			}
		}, "node group workers: machines m-1 and m-10 share the name prod-workers-0; run tent update cluster first"},
		{"a role label that differs does not hide it", func(_ *testing.T, s *rollout.State) {
			twin := workerMachine(1, oldHash)
			twin.ID = "m-9"
			twin.Role = v1alpha1.RoleServer
			s.Machines = append(s.Machines, twin)
		}, "node group workers: machines m-2 and m-9 share the name prod-workers-1; run tent update cluster first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseState()
			addWorker(&s, 0, oldHash)
			addWorker(&s, 1, oldHash)
			tt.build(t, &s)
			checkRefused(t, s, tt.want)
		})
	}
}

func TestNextIgnoresDuplicatesOutsideTheGroupsOfTheRun(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, newHash)
	addWorker(&s, 1, newHash)
	twin := s.Machines[0]
	twin.ID = "m-99"
	s.Machines = append(s.Machines, twin)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestNextRefusesDuplicatesInAnyGroupBeforeActing(t *testing.T) {
	s := baseState()
	s.Groups = append(s.Groups, rollout.Group{Name: "zeta", Role: v1alpha1.RoleClient, Size: 1, Zones: []string{"ams"},
		SpecHash: newHash, MaxSurge: 1})
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, oldHash)
	for _, id := range []string{"m-50", "m-51"} {
		s.Machines = append(s.Machines, rollout.Machine{ID: id, Name: "prod-zeta-0", Group: "zeta",
			Role: v1alpha1.RoleClient})
	}
	checkRefused(t, s, "node group zeta: machines m-50 and m-51 share the name prod-zeta-0; run tent update cluster "+
		"first")
}

func TestNextAcceptsAServerThatReportsNoVersion(t *testing.T) {
	// A server is in the Raft configuration a moment before autopilot reports it, with no version yet.
	s := midRoll()
	s.Nomad.Servers[0].Version = ""
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
}
