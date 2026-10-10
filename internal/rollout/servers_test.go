package rollout_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// The states of these tests have the server group servers of the given size, as the cloud and Nomad report it.

func serverName(i int) string { return rollout.NodeName("prod", "servers", i) }

// serversState is a cluster of size up to date servers, the first leading: running, joined, voting, healthy and
// stable for an hour. The failure tolerance is 1.
func serversState(size int) rollout.State {
	s := rollout.State{
		Cluster: "prod",
		Groups:  []rollout.Group{serversGroup(size)},
		Nomad:   rollout.Nomad{Healthy: true, FailureTolerance: 1},
		Version: "2.0.7",
		Refresh: time.Minute,
		Now:     epoch,
	}
	for i := range size {
		addServerNode(&s, i, newHash)
	}
	return s
}

// machineIDOf is the ID of server machine i.
func machineIDOf(i int) string { return fmt.Sprintf("m-%d", i+1) }

// addServerNode adds server i: machine m-<i+1>, its Raft server r-<i+1> and its alive member.
func addServerNode(s *rollout.State, i int, hash string) {
	zones := []string{"ams", "fra"}
	m := rollout.Machine{
		ID: machineIDOf(i), Name: serverName(i), Group: "servers", Role: v1alpha1.RoleServer,
		Zone: zones[i%2], SpecHash: hash, PrivateIP: ip(10 + i), Ready: true, Joined: true,
		Created: epoch.Add(time.Duration(i-10) * time.Hour),
	}
	s.Machines = append(s.Machines, m)
	s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{
		ID: "r-" + string(rune('1'+i)), Name: m.Name + ".global", Address: netip.AddrPortFrom(m.PrivateIP, 4647),
		Voter: true, Leader: i == 0, Healthy: true, StableSince: epoch.Add(-time.Hour), Version: "2.0.7",
	})
	s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: m.Name + ".global", Address: m.PrivateIP,
		Status: "alive"})
}

// midRoll is a group of size 3 that rolls: servers 0 to 2 are outdated, 0 leads, and server 3 is up to date and
// joined. Every server is running, voting and healthy.
func midRoll() rollout.State {
	s := serversState(3)
	for i := range 3 {
		s.Machines[i].SpecHash = oldHash
	}
	addServerNode(&s, 3, newHash)
	return s
}

func serverNode(t *testing.T, s *rollout.State, i int) *rollout.Server {
	t.Helper()
	return &s.Nomad.Servers[serverIndex(t, s, i)]
}

func serverIndex(t *testing.T, s *rollout.State, i int) int {
	t.Helper()
	for j := range s.Nomad.Servers {
		if s.Nomad.Servers[j].Name == serverName(i)+".global" {
			return j
		}
	}
	t.Fatalf("no server %d", i)
	return -1
}

func serverMember(t *testing.T, s *rollout.State, i int) *rollout.Member {
	t.Helper()
	for j := range s.Nomad.Members {
		if s.Nomad.Members[j].Name == serverName(i)+".global" {
			return &s.Nomad.Members[j]
		}
	}
	t.Fatalf("no member %d", i)
	return nil
}

// dropServerPeer removes server i from the Raft configuration.
func dropServerPeer(t *testing.T, s *rollout.State, i int) {
	t.Helper()
	j := serverIndex(t, s, i)
	s.Nomad.Servers = append(s.Nomad.Servers[:j], s.Nomad.Servers[j+1:]...)
}

// dropMember removes the member of server i from the gossip pool.
func dropMember(t *testing.T, s *rollout.State, i int) {
	t.Helper()
	m := serverMember(t, s, i)
	for j := range s.Nomad.Members {
		if &s.Nomad.Members[j] == m {
			s.Nomad.Members = append(s.Nomad.Members[:j], s.Nomad.Members[j+1:]...)
			return
		}
	}
}

// serverOutcome is what a test checks of a step on servers.
type serverOutcome struct {
	Action   rollout.Action
	Machine  string // the machine's name
	Server   string // the server's Raft ID
	Member   string // the member's name
	Node     string // the client node's ID
	Voters   int
	Deadline time.Duration
	Until    time.Time
}

func serverOutcomeOf(s rollout.Step) serverOutcome {
	return serverOutcome{
		s.Action, s.Machine.Name, s.Server.ID, s.Member.Name, s.Node.ID, s.Voters, s.Deadline, s.Until,
	}
}

func checkServerStep(t *testing.T, got rollout.Step, want serverOutcome) {
	t.Helper()
	if got.Group != "servers" && got.Group != "zeta" {
		t.Errorf("step %q is of group %q, want the server group", got, got.Group)
	}
	if diff := cmp.Diff(want, serverOutcomeOf(got)); diff != "" {
		t.Errorf("step %q mismatch (-want +got):\n%s", got, diff)
	}
}

type serverCase struct {
	name  string
	build func(t *testing.T, s *rollout.State)
	want  serverOutcome
}

func runServerCases(t *testing.T, base func() rollout.State, tests []serverCase) {
	t.Helper()
	runServerCasesIn(t, rollout.Roll, base, tests)
}

func runServerCasesIn(t *testing.T, mode rollout.Mode, base func() rollout.State, tests []serverCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.build(t, &s)
			checkServerStep(t, nextIn(t, mode, s), tt.want)
		})
	}
}

type refusalCase struct {
	name  string
	build func(t *testing.T, s *rollout.State)
	want  string
}

func runRefusalCases(t *testing.T, base func() rollout.State, tests []refusalCase) {
	t.Helper()
	runRefusalCasesIn(t, rollout.Roll, base, tests)
}

func runRefusalCasesIn(t *testing.T, mode rollout.Mode, base func() rollout.State, tests []refusalCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.build(t, &s)
			checkRefusedIn(t, mode, s, tt.want)
		})
	}
}

const (
	restAdvice      = "; run tent validate cluster to see what is wrong"
	unhealthyAdvice = "; tent replaces a server only while every server is healthy"
)

// stopServer is the state of a victim after its machine was stopped: the machine is not running.
func stopServer(t *testing.T, s *rollout.State, i int) {
	t.Helper()
	machineOf(t, s, serverName(i)).Ready = false
}

func TestServerRuleS1WaitsForAMachineThatHasNotJoined(t *testing.T) {
	runServerCases(t, midRoll, []serverCase{
		{"a new machine that is not running", func(t *testing.T, s *rollout.State) {
			m := machineOf(t, s, serverName(3))
			m.Ready, m.Joined = false, false
			dropPeerAndMember(t, s, 3)
		}, serverOutcome{Action: rollout.WaitJoined, Machine: serverName(3)}},
		{"a machine that runs but has no label yet; it wins over the victim", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(3)).Joined = false
			serverNode(t, s, 3).Voter = false
		}, serverOutcome{Action: rollout.WaitJoined, Machine: serverName(3)}},
		{"the first machine by name goes first", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(2)).Joined = false
			machineOf(t, s, serverName(3)).Joined = false
		}, serverOutcome{Action: rollout.WaitJoined, Machine: serverName(2)}},
	})
}

// dropPeerAndMember removes server i from the Raft configuration and the gossip pool.
func dropPeerAndMember(t *testing.T, s *rollout.State, i int) {
	t.Helper()
	dropServerPeer(t, s, i)
	dropMember(t, s, i)
}

func TestServerRuleS4RefusesAGroupShortOfItsSize(t *testing.T) {
	runRefusalCases(t, func() rollout.State { return serversState(3) }, []refusalCase{
		{"two of three", func(_ *testing.T, s *rollout.State) {
			s.Machines = s.Machines[:2]
			s.Nomad.Servers = s.Nomad.Servers[:2]
		}, "node group servers: it has 2 of its 3 nodes; run tent update cluster first"},
		{"it wins over an outdated machine", func(_ *testing.T, s *rollout.State) {
			s.Machines = s.Machines[:1]
			s.Machines[0].SpecHash = oldHash
		}, "node group servers: it has 1 of its 3 nodes; run tent update cluster first"},
		{"nothing is outdated", func(_ *testing.T, s *rollout.State) {
			s.Machines = s.Machines[:2]
		}, "node group servers: it has 2 of its 3 nodes; run tent update cluster first"},
	})
}

func TestServerRuleS5CreatesAServerWhenAnOutdatedOneIsLeft(t *testing.T) {
	roll := func() rollout.State {
		s := serversState(3)
		machineOf(t, &s, serverName(0)).SpecHash = oldHash
		return s
	}
	t.Run("the name is the lowest free one and the zone the one with the fewest up to date servers",
		func(t *testing.T) {
			s := roll()
			// Servers 1 (fra) and 2 (ams) are up to date: a tie, so the first zone of the group.
			checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers",
				Machine: serverName(3), Zone: "ams"})
			machineOf(t, &s, serverName(2)).Zone = "fra"
			checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers",
				Machine: serverName(3), Zone: "ams"})
			machineOf(t, &s, serverName(1)).Zone = "ams"
			machineOf(t, &s, serverName(2)).Zone = "ams"
			checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers",
				Machine: serverName(3), Zone: "fra"})
		})
	t.Run("a name below the highest is not used again", func(t *testing.T) {
		s := roll()
		machineOf(t, &s, serverName(2)).Name = serverName(3)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers", Machine: serverName(4),
			Zone: "ams"})
	})
	t.Run("a new Nomad version is no reason to wait for the clients", func(t *testing.T) {
		s := roll()
		s.Version = "2.0.8"
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers", Machine: serverName(3),
			Zone: "ams"})
	})
	t.Run("a forced machine counts as outdated", func(t *testing.T) {
		s := serversState(3)
		s.Forced = map[string]bool{"m-2": true}
		// Servers 0 and 2 (ams) are up to date, 1 (fra) is forced: the new server goes to fra.
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers", Machine: serverName(3),
			Zone: "fra"})
	})
}

// renameNode gives the machine, the server, the member and the client node of node i the name of node j.
func renameNode(s *rollout.State, i, j int) {
	from, to := serverName(i), serverName(j)
	for k := range s.Machines {
		if s.Machines[k].Name == from {
			s.Machines[k].Name = to
		}
	}
	for k := range s.Nomad.Servers {
		if s.Nomad.Servers[k].Name == from+".global" {
			s.Nomad.Servers[k].Name = to + ".global"
		}
	}
	for k := range s.Nomad.Members {
		if s.Nomad.Members[k].Name == from+".global" {
			s.Nomad.Members[k].Name = to + ".global"
		}
	}
	for k := range s.Nomad.Nodes {
		if s.Nomad.Nodes[k].Name == from {
			s.Nomad.Nodes[k].Name = to
		}
	}
}

// A new server or combined node takes the index above the highest that the group's name pattern has in any listed
// machine, server or member, so it never takes a name at or below one of theirs.
func TestNewServerNameIsAboveEveryIndexTheGroupHas(t *testing.T) {
	bases := []struct {
		name string
		base func() rollout.State
	}{
		{"server group", func() rollout.State { return serversState(3) }},
		{"combined group", func() rollout.State { return combinedState(3) }},
	}
	// Each build leaves server 0 outdated, so the next step creates a server; want is the index of its name.
	tests := []struct {
		name  string
		build func(s *rollout.State)
		want  int
	}{
		{"machines 0 to 2", func(*rollout.State) {}, 3},
		{"machines 1 to 3", func(s *rollout.State) { renameNode(s, 0, 3) }, 4},
		{"machines 0, 1 and 3", func(s *rollout.State) { renameNode(s, 2, 3) }, 4},
		{"a member above every machine", func(s *rollout.State) {
			s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: serverName(7) + ".global",
				Address: ip(40), Status: "failed"})
		}, 8},
		{"a member without a region suffix", func(s *rollout.State) {
			s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: serverName(5), Address: ip(40),
				Status: "failed"})
		}, 6},
		{"a server of the Raft configuration above every machine", func(s *rollout.State) {
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: serverName(6) + ".global",
				Address: netip.AddrPortFrom(ip(41), 4647), Healthy: true, Version: "2.0.7"})
		}, 7},
		{"a machine above every server and member", func(s *rollout.State) {
			m := &s.Machines[2]
			for k := range s.Nomad.Nodes {
				if s.Nomad.Nodes[k].Name == m.Name {
					s.Nomad.Nodes[k].Name = serverName(9)
				}
			}
			m.Name = serverName(9)
		}, 10},
		{"names that are not of the group's pattern", func(s *rollout.State) {
			for _, name := range []string{
				"prod-servers-x", "prod-servers-", "prod-servers--9", "prod-servers-+9", "prod-servers-9-b",
				"prod-servers-4294967296",
				"prod-servers-99999999999999999999", "other-servers-9", "prod-workers-9", "servers-9", "prod-servers9",
			} {
				s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: name + ".global", Address: ip(40),
					Status: "failed"})
				s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-" + name, Name: name + ".global",
					Address: netip.AddrPortFrom(ip(41), 4647), Healthy: true, Version: "2.0.7"})
			}
		}, 3},
	}
	for _, b := range bases {
		for _, tt := range tests {
			t.Run(b.name+": "+tt.name, func(t *testing.T) {
				s := b.base()
				s.Machines[0].SpecHash = oldHash
				tt.build(&s)
				checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers",
					Machine: serverName(tt.want), Zone: "ams"})
			})
		}
	}
}

// A new server or combined node takes the index that tent remembers when it is above every name that something lists,
// and the index above the highest listed name when that is higher.
func TestNewServerNameTakesTheRememberedIndex(t *testing.T) {
	bases := []struct {
		name string
		base func() rollout.State
	}{
		{"server group", func() rollout.State { return serversState(3) }},
		{"combined group", func() rollout.State { return combinedState(3) }},
	}
	tests := []struct {
		name      string
		nextIndex int
		build     func(s *rollout.State)
		want      int
	}{
		{"nothing remembered", 0, func(*rollout.State) {}, 3},
		{"the remembered index is above every name", 7, func(*rollout.State) {}, 7},
		{"the remembered index is the one above the highest name", 3, func(*rollout.State) {}, 3},
		{"the remembered index is below the highest name", 2, func(*rollout.State) {}, 3},
		{"a member above the remembered index", 7, func(s *rollout.State) {
			s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: serverName(9) + ".global",
				Address: ip(40), Status: "failed"})
		}, 10},
		{"a server of the Raft configuration above the remembered index", 7, func(s *rollout.State) {
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: serverName(8) + ".global",
				Address: netip.AddrPortFrom(ip(41), 4647), Healthy: true, Version: "2.0.7"})
		}, 9},
	}
	for _, b := range bases {
		for _, tt := range tests {
			t.Run(b.name+": "+tt.name, func(t *testing.T) {
				s := b.base()
				s.Machines[0].SpecHash = oldHash
				s.Groups[0].NextIndex = tt.nextIndex
				tt.build(&s)
				checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers",
					Machine: serverName(tt.want), Zone: "ams"})
			})
		}
	}
}

// A client group takes the lowest free name whatever index is remembered for it.
func TestClientGroupIgnoresTheRememberedIndex(t *testing.T) {
	s := baseState()
	addWorker(&s, 0, oldHash)
	addWorker(&s, 1, newHash)
	s.Groups[0].NextIndex = 7
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "workers", Machine: workerName(2), Zone: "fra"})
}

// With no machine, server or member of the group listed, the next index is the remembered one.
func TestNextIndexIsTheRememberedOneWhenNothingOfTheGroupIsListed(t *testing.T) {
	s := rollout.State{Cluster: "prod", Machines: []rollout.Machine{{Name: workerName(9)}}}
	for _, remembered := range []int{0, 4} {
		if got := rollout.NextIndex(s, rollout.Group{Name: "servers", NextIndex: remembered}); got != remembered {
			t.Errorf("NextIndex with %d remembered = %d, want %d", remembered, got, remembered)
		}
	}
}

// A server group of three with five machines, three of them outdated, loses two outdated servers before it creates
// another, which takes a name above every name that the group had.
func TestAServerGroupAboveItsSizeByTwoRemovesBeforeItCreates(t *testing.T) {
	s := serversState(3)
	for i := range 3 {
		s.Machines[i].SpecHash = oldHash
	}
	addServerNode(&s, 3, newHash)
	addServerNode(&s, 4, newHash)
	// Zone ams holds three machines and fra two, so the outdated server in ams that does not lead goes first.
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(2)})
	// Server 2 is gone: four machines, two outdated, two in each zone.
	s.Machines = append(s.Machines[:2], s.Machines[3:]...)
	dropPeerAndMember(t, &s, 2)
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
	// Server 1 is gone too: three machines, one outdated, and the next create takes the next name.
	s.Machines = append(s.Machines[:1], s.Machines[2:]...)
	dropPeerAndMember(t, &s, 1)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers", Machine: serverName(5), Zone: "ams"})
}

func TestServerRuleS6IsDoneWhenNothingIsOutdated(t *testing.T) {
	t.Run("at the size", func(t *testing.T) {
		checkOutcome(t, nextRoll(t, serversState(3)), outcome{Action: rollout.Done})
	})
	t.Run("beyond the size, which update deals with", func(t *testing.T) {
		s := serversState(3)
		addServerNode(&s, 3, newHash)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
	})
}

func TestServerChecksAtRest(t *testing.T) {
	// Both states have an outdated server to remove or replace; target is an up to date server that no rule picks.
	places := []struct {
		name   string
		base   func() rollout.State
		target int
	}{
		{"before a victim starts", midRoll, 3},
		{"before a server is created", func() rollout.State {
			s := serversState(3)
			s.Machines[2].SpecHash = oldHash
			return s
		}, 1},
	}
	tests := []struct {
		name  string
		build func(t *testing.T, s *rollout.State, target int)
		want  string
	}{
		{"autopilot reports a server unhealthy", func(t *testing.T, s *rollout.State, i int) {
			s.Nomad.Healthy = false
			serverNode(t, s, i).Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy (%s)" + unhealthyAdvice},
		{"autopilot reports two servers unhealthy", func(t *testing.T, s *rollout.State, i int) {
			s.Nomad.Healthy = false
			serverNode(t, s, i).Healthy = false
			serverNode(t, s, 0).Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy (prod-servers-0 and %s)" + unhealthyAdvice},
		{"autopilot is unhealthy and the report names no server", func(_ *testing.T, s *rollout.State, _ int) {
			s.Nomad.Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy" + unhealthyAdvice},
		{"a machine is not running", func(t *testing.T, s *rollout.State, i int) {
			machineOf(t, s, serverName(i)).Ready = false
		}, "node group servers: node %s is not running" + restAdvice},
		{"a machine has no server", func(t *testing.T, s *rollout.State, i int) {
			dropServerPeer(t, s, i)
		}, "node group servers: node %s is not a server in the Raft configuration" + restAdvice},
		{"a server does not vote", func(t *testing.T, s *rollout.State, i int) {
			serverNode(t, s, i).Voter = false
		}, "node group servers: node %s is not a voting server" + restAdvice},
		{"the cloud reports no private address", func(t *testing.T, s *rollout.State, i int) {
			machineOf(t, s, serverName(i)).PrivateIP = netip.Addr{}
		}, "node group servers: node %s is not a voting server: the cloud reports no private address for it" +
			restAdvice},
		{"autopilot comes first", func(t *testing.T, s *rollout.State, i int) {
			s.Nomad.Healthy = false
			serverNode(t, s, 0).Healthy = false
			machineOf(t, s, serverName(i)).Ready = false
		}, "node group servers: autopilot reports the servers unhealthy (prod-servers-0)" + unhealthyAdvice},
	}
	for _, place := range places {
		for _, tt := range tests {
			t.Run(tt.name+" "+place.name, func(t *testing.T) {
				s := place.base()
				tt.build(t, &s, place.target)
				want := tt.want
				if strings.Contains(want, "%s") {
					want = fmt.Sprintf(want, serverName(place.target))
				}
				checkRefused(t, s, want)
			})
		}
	}
	t.Run("the first machine by name that fails comes first", func(t *testing.T) {
		s := serversState(3)
		s.Machines[2].SpecHash = oldHash
		machineOf(t, &s, serverName(1)).Ready = false
		serverNode(t, &s, 0).Voter = false
		checkRefused(t, s, "node group servers: node prod-servers-0 is not a voting server"+restAdvice)
	})
}

func TestServerRuleS5NeedsAFailureToleranceOfOne(t *testing.T) {
	runRefusalCases(t, func() rollout.State {
		s := serversState(3)
		s.Machines[2].SpecHash = oldHash
		return s
	}, []refusalCase{
		{"a group of three that can lose no voter", func(_ *testing.T, s *rollout.State) {
			s.Nomad.FailureTolerance = 0
		}, "node group servers: the servers can lose no voter (failure tolerance 0); tent adds a server only to a " +
			"cluster that can lose one"},
		{"the other checks come first", func(_ *testing.T, s *rollout.State) {
			s.Nomad.FailureTolerance = 0
			s.Nomad.Healthy = false
		}, "node group servers: autopilot reports the servers unhealthy" + unhealthyAdvice},
	})
}

func TestServerVictimNeedsNoFailureTolerance(t *testing.T) {
	s := midRoll()
	s.Nomad.FailureTolerance = 0
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
}

func TestServerRuleAWaitsForTheStabilityWindow(t *testing.T) {
	window := time.Minute + 10*time.Second
	since := func(t *testing.T, s *rollout.State, i int, ago time.Duration) {
		t.Helper()
		serverNode(t, s, i).StableSince = s.Now.Add(-ago)
	}
	stop := serverOutcome{Action: rollout.Stop, Machine: serverName(1)}
	runServerCases(t, midRoll, []serverCase{
		{"one second short of the window", func(t *testing.T, s *rollout.State) {
			since(t, s, 3, window-time.Second)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(time.Second)}},
		{"exactly the window", func(t *testing.T, s *rollout.State) { since(t, s, 3, window) }, stop},
		{"the latest voter decides", func(t *testing.T, s *rollout.State) {
			since(t, s, 0, 30*time.Second)
			since(t, s, 2, 50*time.Second)
			since(t, s, 3, 20*time.Second)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(window - 20*time.Second)}},
		{"the victim's own server does not count", func(t *testing.T, s *rollout.State) {
			since(t, s, 1, 0)
		}, stop},
		{"a nonvoter does not count", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: "other.global", Healthy: true,
				StableSince: s.Now, Version: "2.0.7"})
		}, stop},
		{"the window is the refresh interval plus ten seconds", func(t *testing.T, s *rollout.State) {
			s.Refresh = 2 * time.Minute
			since(t, s, 3, window)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(time.Minute)}},
		{"a leader victim waits too, before its transfer", func(t *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash, s.Machines[2].SpecHash = newHash, newHash
			since(t, s, 3, 0)
		}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(0), Until: epoch.Add(window)}},
	})
	t.Run("the checks at rest come before the window", func(t *testing.T) {
		s := midRoll()
		since(t, &s, 3, 0)
		s.Nomad.Healthy = false
		checkRefused(t, s, "node group servers: autopilot reports the servers unhealthy"+unhealthyAdvice)
	})
	t.Run("a removal that has started does not wait", func(t *testing.T) {
		s := midRoll()
		since(t, &s, 3, 0)
		stopServer(t, &s, 1)
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)})
	})
}

func TestServerVictimOrder(t *testing.T) {
	victim := func(name string) serverOutcome { return serverOutcome{Action: rollout.Stop, Machine: name} }
	// The cluster has servers 0 (leader, ams), 1 (fra), 2 (ams), 3 (fra), all outdated but 3; by age 0 is the
	// oldest and 3 the youngest.
	runServerCases(t, midRoll, []serverCase{
		{"the oldest outdated server that does not lead", func(*testing.T, *rollout.State) {},
			victim(serverName(1))},
		{"an outdated server wins over an older one that is up to date", func(_ *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash = newHash
			s.Machines[3].Created = epoch.Add(-100 * time.Hour)
		}, victim(serverName(2))},
		{"the leader goes last", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(1)).Created = epoch
			machineOf(t, s, serverName(2)).Created = epoch
		}, victim(serverName(1))},
		{"the zone with the most servers goes first", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(1)).Created = epoch.Add(-50 * time.Hour)
			machineOf(t, s, serverName(2)).Created = epoch.Add(-20 * time.Hour)
			machineOf(t, s, serverName(3)).Zone = "ams"
		}, victim(serverName(2))},
		{"a server that autopilot does not count healthy goes before an older one", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 2).Healthy = false
		}, victim(serverName(2))},
		{"a server without a creation time counts as the youngest", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(1)).Created = time.Time{}
		}, victim(serverName(2))},
		{"the lowest ID goes first when the ages are equal", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(1)).Created = epoch
			machineOf(t, s, serverName(2)).Created = epoch
			machineOf(t, s, serverName(1)).ID, machineOf(t, s, serverName(2)).ID = "m-8", "m-7"
		}, victim(serverName(2))},
		{"a server whose removal has started goes before all others", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 2)
		}, serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(2)}},
	})
}

func TestServerStartedMachinesCountOnlyBeyondTheSize(t *testing.T) {
	s := serversState(3)
	machineOf(t, &s, serverName(2)).SpecHash = oldHash
	stopServer(t, &s, 2)
	checkRefused(t, s, "node group servers: node prod-servers-2 is not running"+restAdvice)
}

func TestServerRolledOnlyWhenOutdated(t *testing.T) {
	s := serversState(3)
	addServerNode(&s, 3, newHash)
	stopServer(t, &s, 3)
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Done})
}

func TestServerLineETransfersTheLeadership(t *testing.T) {
	// Server 0 leads and is the only outdated server; 1, 2 and 3 are up to date.
	leader := func() rollout.State {
		s := midRoll()
		s.Machines[1].SpecHash, s.Machines[2].SpecHash = newHash, newHash
		return s
	}
	transfer := func(to string) serverOutcome {
		return serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: to}
	}
	runServerCases(t, leader, []serverCase{
		{"to the first server by name", func(*testing.T, *rollout.State) {}, transfer("r-2")},
		{"not to a server that autopilot does not count healthy", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Healthy = false
		}, transfer("r-3")},
		{"by the name of the server, not by its position", func(t *testing.T, s *rollout.State) {
			s.Nomad.Servers[serverIndex(t, s, 1)].Name = "prod-servers-9.global"
		}, transfer("r-3")},
		{"not to a server that no machine of the group has", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: "a.global", Voter: true,
				Healthy: true, Version: "2.0.7"})
		}, transfer("r-2")},
	})
	t.Run("a refusal when no server can take it", func(t *testing.T) {
		s := leader()
		for i := 1; i <= 3; i++ {
			serverNode(t, &s, i).Healthy = false
		}
		checkRefused(t, s, "node group servers: prod-servers-0 leads, and no healthy voter of the group that is up to date "+
			"can take the leadership")
	})
}

func TestServerGroupOfOneNeedsNoFailureToleranceToCreate(t *testing.T) {
	one := func() rollout.State {
		s := serversState(1)
		s.Machines[0].SpecHash = oldHash
		s.Nomad.FailureTolerance = 0
		return s
	}
	t.Run("the new server is created", func(t *testing.T) {
		step := nextRoll(t, one())
		if step.Action != rollout.Create || step.Machine.Name != serverName(1) {
			t.Errorf("step = %q, want the creation of %s", step, serverName(1))
		}
	})
	runRefusalCases(t, one, []refusalCase{
		{"the checks at rest stay", func(_ *testing.T, s *rollout.State) { s.Nomad.Healthy = false },
			"node group servers: autopilot reports the servers unhealthy" + unhealthyAdvice},
		{"the server must vote", func(t *testing.T, s *rollout.State) { serverNode(t, s, 0).Voter = false },
			"node group servers: node prod-servers-0 is not a voting server" + restAdvice},
	})
}

// twoVoters is a group of one server with a second one: both vote, the first leads and is up to date, and the second
// is outdated and the victim. Both have been stable for an hour.
func twoVoters() rollout.State {
	s := serversState(1)
	addServerNode(&s, 1, oldHash)
	return s
}

func TestServerLineE1WaitsForTheWindowWhateverTheNumberOfVoters(t *testing.T) {
	window := 70 * time.Second
	fresh := func(t *testing.T, s *rollout.State) { serverNode(t, s, 0).StableSince = s.Now }
	t.Run("two voters", func(t *testing.T) {
		runServerCases(t, twoVoters, []serverCase{
			{"the window is not over", fresh,
				serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(window)}},
			{"the window is over", func(t *testing.T, s *rollout.State) {
				serverNode(t, s, 0).StableSince = s.Now.Add(-window)
			}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
		})
	})
	t.Run("a drained combined victim", func(t *testing.T) {
		combinedTwo := func() rollout.State {
			s := combinedState(1)
			addCombinedNode(&s, 1, oldHash)
			serverDrained(t, &s, 1)
			return s
		}
		runServerCases(t, combinedTwo, []serverCase{
			{"beside one other voter, the window is not over", fresh,
				serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(window)}},
			{"beside one other voter, the window is over", func(t *testing.T, s *rollout.State) {
				serverNode(t, s, 0).StableSince = s.Now.Add(-window)
			}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
		})
		runServerCases(t, combinedMidRoll, []serverCase{
			{"beside two or more other voters, the window is not over", func(t *testing.T, s *rollout.State) {
				serverDrained(t, s, 1)
				serverNode(t, s, 3).StableSince = s.Now
			}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(window)}},
			{"beside two or more other voters, the window is over", func(t *testing.T, s *rollout.State) {
				serverDrained(t, s, 1)
			}, serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
		})
	})
	t.Run("the window counts the other voters only", func(t *testing.T) {
		s := twoVoters()
		s.Groups[0].Role = v1alpha1.RoleCombined
		s.Groups[0].DrainTimeout = time.Hour
		makeCombined(&s, 0)
		makeCombined(&s, 1)
		serverDrained(t, &s, 1)
		serverNode(t, &s, 1).StableSince = s.Now
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1),
			Server: "r-2"})
	})
	t.Run("checkServing comes before the window", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 1)
		serverNode(t, &s, 3).StableSince = s.Now
		s.Nomad.Healthy = false
		serverNode(t, &s, 2).Healthy = false
		checkRefused(t, s, "node group servers: autopilot reports the servers unhealthy (prod-servers-2)"+unhealthyAdvice)
	})
	t.Run("a nonvoter is not waited for", func(t *testing.T) {
		s := twoVoters()
		serverNode(t, &s, 0).StableSince = s.Now
		serverNode(t, &s, 1).Voter = false
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1),
			Server: "r-2"})
	})
	t.Run("a drained nonvoter beside two or more other voters is stopped within the window", func(t *testing.T) {
		s := combinedMidRoll()
		serverDrained(t, &s, 1)
		serverNode(t, &s, 1).Voter = false
		serverNode(t, &s, 3).StableSince = s.Now
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})
	})
	t.Run("a drained combined victim of a shrink waits too", func(t *testing.T) {
		s := combinedState(3)
		s.Groups[0].Size = 2
		serverDrained(t, &s, 2)
		serverNode(t, &s, 0).StableSince = s.Now
		checkServerStep(t, nextIn(t, rollout.Shrink, s), serverOutcome{Action: rollout.WaitStable,
			Machine: serverName(2), Until: epoch.Add(window)})
	})
}

func TestServerLineFRemovesThePeerOfARunningServerBesideFewerThanTwoOtherVoters(t *testing.T) {
	remove := serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}
	stop := serverOutcome{Action: rollout.Stop, Machine: serverName(1)}
	runServerCases(t, twoVoters, []serverCase{
		{"the victim votes", func(*testing.T, *rollout.State) {}, remove},
		{"the victim leads: the transfer comes first", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(0)).SpecHash = oldHash
			machineOf(t, s, serverName(1)).SpecHash = newHash
		}, serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: "r-2"}},
		{"the leader added it again as a nonvoter", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Voter = false
		}, remove},
		{"it has no server", func(t *testing.T, s *rollout.State) { dropServerPeer(t, s, 1) }, stop},
	})
	t.Run("a nonvoter beside two voters is stopped", func(t *testing.T) {
		s := serversState(2)
		addServerNode(&s, 2, oldHash)
		serverNode(t, &s, 2).Voter = false
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(2)})
	})
	t.Run("a voter beside two other voters is stopped", func(t *testing.T) {
		s := serversState(2)
		addServerNode(&s, 2, oldHash)
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(2)})
	})
}

func TestServerLineGStopsAServerThatRuns(t *testing.T) {
	runServerCases(t, midRoll, []serverCase{
		{"four voters", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
		{"three voters", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 3)
			s.Machines = s.Machines[:3]
			s.Groups[0].Size = 2
		}, serverOutcome{Action: rollout.Stop, Machine: serverName(2)}},
		{"the leader re-added the removed server as a nonvoter", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Voter = false
		}, serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
		{"it has no server", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
		}, serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
	})
}

func TestServerRemovalOfAStoppedServer(t *testing.T) {
	stopped := func() rollout.State {
		s := midRoll()
		stopServer(t, &s, 1)
		return s
	}
	runServerCases(t, stopped, []serverCase{
		{"h: autopilot still counts it a healthy voter", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
		{"h: autopilot turned unhealthy while it was stopped, a wait and no refusal", func(_ *testing.T, s *rollout.State) {
			s.Nomad.Healthy = false
		}, serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
		{"i: autopilot counts it unhealthy", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Healthy = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
		{"i: it is a nonvoter", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Voter = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
		{"j: autopilot removed the peer first, and the member is alive", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
		}, serverOutcome{Action: rollout.ForceLeave, Machine: serverName(1), Member: serverName(1) + ".global"}},
		{"j: the member is failed", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
			serverMember(t, s, 1).Status = "failed"
		}, serverOutcome{Action: rollout.ForceLeave, Machine: serverName(1), Member: serverName(1) + ".global"}},
		{"k: the member is leaving and autopilot is unhealthy", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
			serverMember(t, s, 1).Status = "leaving"
			s.Nomad.Healthy = false
		}, serverOutcome{Action: rollout.WaitHealthy, Machine: serverName(1), Voters: 3}},
		{"k: the member is gone and autopilot is unhealthy", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 1)
			s.Nomad.Healthy = false
		}, serverOutcome{Action: rollout.WaitHealthy, Machine: serverName(1), Voters: 3}},
		{"k: autopilot is healthy and two servers vote", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 1)
			serverNode(t, s, 2).Voter = false
		}, serverOutcome{Action: rollout.WaitHealthy, Machine: serverName(1), Voters: 3}},
		{"k: autopilot is healthy and four servers vote", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
			serverMember(t, s, 1).Status = "left"
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: "x.global", Voter: true,
				Healthy: true, Version: "2.0.7"})
		}, serverOutcome{Action: rollout.WaitHealthy, Machine: serverName(1), Voters: 3}},
		{"k: two servers are gone, and one server of the others does not vote", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 2)
			dropPeerAndMember(t, s, 1)
			dropPeerAndMember(t, s, 2)
			serverNode(t, s, 3).Voter = false
		}, serverOutcome{Action: rollout.WaitHealthy, Machine: serverName(1), Voters: 2}},
		{"l: two servers are gone and their peers with them: only the others vote", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 2)
			dropPeerAndMember(t, s, 1)
			dropPeerAndMember(t, s, 2)
		}, serverOutcome{Action: rollout.Delete, Machine: serverName(1)}},
		{"l: the peer and the member are gone, and three servers vote", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 1)
		}, serverOutcome{Action: rollout.Delete, Machine: serverName(1)}},
		{"l: the member is leaving", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
			serverMember(t, s, 1).Status = "leaving"
		}, serverOutcome{Action: rollout.Delete, Machine: serverName(1)}},
	})
}

func TestServerGroupsComeBeforeClientGroups(t *testing.T) {
	s := midRoll()
	s.Groups[0].Name = "zeta"
	for i := range s.Machines {
		s.Machines[i].Group = "zeta"
	}
	s.Groups = append(s.Groups, rollout.Group{Name: "alpha", Role: v1alpha1.RoleClient, Size: 1, Zones: []string{"ams"},
		SpecHash: newHash, MaxSurge: 1, DrainTimeout: time.Hour})
	w := workerMachine(0, oldHash)
	w.Group = "alpha"
	w.Name = "prod-alpha-0"
	s.Machines = append(s.Machines, w)
	s.Nomad.Nodes = append(s.Nomad.Nodes, nodeOf(w))
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Stop, Machine: serverName(1)})

	// With the servers done the client group follows.
	for i := range 4 {
		s.Machines[i].SpecHash = newHash
	}
	s.Machines = s.Machines[:3]
	s.Machines = append(s.Machines, w)
	s.Nomad.Servers = s.Nomad.Servers[:3]
	checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "alpha", Machine: "prod-alpha-1",
		Zone: "ams"})
}

func TestServerMachineWithoutAPrivateAddress(t *testing.T) {
	t.Run("a running victim is refused and not stopped", func(t *testing.T) {
		s := midRoll()
		machineOf(t, &s, serverName(1)).PrivateIP = netip.Addr{}
		checkRefused(t, s, "node group servers: node prod-servers-1 is not a voting server: the cloud reports no "+
			"private address for it"+restAdvice)
	})
	t.Run("it owns no server and no member of a zero address", func(t *testing.T) {
		s := midRoll()
		stopServer(t, &s, 1)
		machineOf(t, &s, serverName(1)).PrivateIP = netip.Addr{}
		dropPeerAndMember(t, &s, 1)
		s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: "x.global", Healthy: true,
			Version: "2.0.7"})
		s.Nomad.Members = append(s.Nomad.Members, rollout.Member{Name: "x.global", Status: "alive"})
		checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.Delete, Machine: serverName(1)})
	})
}

func TestServerLineEIsNotForAStoppedLeader(t *testing.T) {
	s := midRoll()
	s.Machines[1].SpecHash, s.Machines[2].SpecHash = newHash, newHash
	stopServer(t, &s, 0)
	checkServerStep(t, nextRoll(t, s), serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(0)})
}

const noQuorumAdvice = "so the servers have no quorum; start its instance again (ID m-2), which tent has not " +
	"deleted, and run the command again"

func TestServerLineH0RefusesAStoppedVoterBesideOneOtherVoter(t *testing.T) {
	refusal := "node group servers: node prod-servers-1 is stopped and its server votes beside one other voter, " +
		noQuorumAdvice
	stopped := func() rollout.State {
		s := twoVoters()
		stopServer(t, &s, 1)
		return s
	}
	runRefusalCases(t, stopped, []refusalCase{
		{"autopilot counts it healthy", func(*testing.T, *rollout.State) {}, refusal},
		{"autopilot counts it unhealthy", func(t *testing.T, s *rollout.State) { serverNode(t, s, 1).Healthy = false },
			refusal},
		{"the cluster is unhealthy", func(_ *testing.T, s *rollout.State) { s.Nomad.Healthy = false }, refusal},
	})
	t.Run("in a shrink", func(t *testing.T) {
		s := shrinkServersState(2, 1)
		stopServer(t, &s, 1)
		checkRefusedIn(t, rollout.Shrink, s, refusal)
	})
	runServerCases(t, twoVoters, []serverCase{
		{"a stopped nonvoter is removed, not refused", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 1)
			serverNode(t, s, 1).Voter = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
	})
	// A group of two servers with a third one, which is outdated and the victim: it votes beside two other voters.
	three := func() rollout.State {
		s := serversState(2)
		addServerNode(&s, 2, oldHash)
		stopServer(t, &s, 2)
		return s
	}
	runServerCases(t, three, []serverCase{
		{"beside two other voters autopilot's count is waited for", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(2)}},
		{"beside two other voters the peer of an unhealthy server is removed", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 2).Healthy = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(2), Server: "r-3"}},
	})
}

func TestServerVictimAtEveryLaterObservationOfAGroupOfOne(t *testing.T) {
	forceLeave := serverOutcome{Action: rollout.ForceLeave, Machine: serverName(1), Member: serverName(1) + ".global"}
	remove := serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}
	modes := []struct {
		name   string
		mode   rollout.Mode
		base   func() rollout.State
		advice string
	}{
		{"roll", rollout.Roll, twoVoters, unhealthyAdvice},
		{"shrink", rollout.Shrink, func() rollout.State { return shrinkServersState(2, 1) }, shrinkUnhealthyAdvice},
	}
	for _, m := range modes {
		mode, base := m.mode, m.base
		t.Run(m.name, func(t *testing.T) {
			runServerCasesIn(t, mode, base, []serverCase{
				{"it runs and has no server: stop within the window", func(t *testing.T, s *rollout.State) {
					dropServerPeer(t, s, 1)
					serverNode(t, s, 0).StableSince = s.Now
				}, serverOutcome{Action: rollout.Stop, Machine: serverName(1)}},
				{"it runs and is a nonvoter again: remove the peer within the window", func(t *testing.T,
					s *rollout.State) {
					serverNode(t, s, 1).Voter = false
					serverNode(t, s, 0).StableSince = s.Now
				}, remove},
				{"it runs and was promoted: the window comes first", func(t *testing.T, s *rollout.State) {
					serverNode(t, s, 0).StableSince = s.Now
				}, serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(70 * time.Second)}},
				{"it runs and was promoted, the window is over: remove the peer", func(*testing.T, *rollout.State) {},
					remove},
				{"it is stopped, has no server and its member is alive", func(t *testing.T, s *rollout.State) {
					stopServer(t, s, 1)
					dropServerPeer(t, s, 1)
				}, forceLeave},
				{"it is stopped, has no server and its member failed", func(t *testing.T, s *rollout.State) {
					stopServer(t, s, 1)
					dropServerPeer(t, s, 1)
					serverMember(t, s, 1).Status = "failed"
				}, forceLeave},
				{"it is stopped and a nonvoter again: remove the peer", func(t *testing.T, s *rollout.State) {
					stopServer(t, s, 1)
					serverNode(t, s, 1).Voter = false
				}, remove},
			})
			t.Run("it runs and was promoted: the checks at rest come first", func(t *testing.T) {
				s := base()
				s.Nomad.Healthy = false
				checkRefusedIn(t, mode, s, "node group servers: autopilot reports the servers unhealthy"+m.advice)
			})
		})
	}
	t.Run("a combined victim that was promoted: checkServing, the window, then the peer", func(t *testing.T) {
		combined := func() rollout.State {
			s := combinedState(1)
			addCombinedNode(&s, 1, oldHash)
			serverDrained(t, &s, 1)
			return s
		}
		runServerCases(t, combined, []serverCase{
			{"the window is not over", func(t *testing.T, s *rollout.State) { serverNode(t, s, 0).StableSince = s.Now },
				serverOutcome{Action: rollout.WaitStable, Machine: serverName(1), Until: epoch.Add(70 * time.Second)}},
			{"the window is over", func(*testing.T, *rollout.State) {}, remove},
		})
		s := combined()
		s.Nomad.Healthy = false
		checkRefused(t, s, "node group servers: autopilot reports the servers unhealthy"+unhealthyAdvice)
	})
}
