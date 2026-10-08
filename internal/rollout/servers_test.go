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
	restAdvice      = "; run tent update cluster or tent validate cluster first"
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
	t.Run("a name that is free below the others is used", func(t *testing.T) {
		s := roll()
		machineOf(t, &s, serverName(2)).Name = serverName(3)
		checkOutcome(t, nextRoll(t, s), outcome{Action: rollout.Create, Group: "servers", Machine: serverName(2),
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
		{"a group of one server", func(_ *testing.T, s *rollout.State) {
			*s = serversState(1)
			s.Machines[0].SpecHash = oldHash
			s.Nomad.FailureTolerance = 0
		}, "node group servers: a group of one server cannot roll: its failure tolerance is 0"},
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
		}, serverOutcome{Action: rollout.WaitStable, Until: epoch.Add(time.Second)}},
		{"exactly the window", func(t *testing.T, s *rollout.State) { since(t, s, 3, window) }, stop},
		{"the latest voter decides", func(t *testing.T, s *rollout.State) {
			since(t, s, 0, 30*time.Second)
			since(t, s, 2, 50*time.Second)
			since(t, s, 3, 20*time.Second)
		}, serverOutcome{Action: rollout.WaitStable, Until: epoch.Add(window - 20*time.Second)}},
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
		}, serverOutcome{Action: rollout.WaitStable, Until: epoch.Add(time.Minute)}},
		{"a leader victim waits too, before its transfer", func(t *testing.T, s *rollout.State) {
			s.Machines[1].SpecHash, s.Machines[2].SpecHash = newHash, newHash
			since(t, s, 3, 0)
		}, serverOutcome{Action: rollout.WaitStable, Until: epoch.Add(window)}},
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

func TestServerLineFRefusesTwoVotersToOne(t *testing.T) {
	// A group of one server with a second one: both vote, and the second is outdated.
	two := func() rollout.State {
		s := serversState(1)
		addServerNode(&s, 1, oldHash)
		return s
	}
	runRefusalCases(t, two, []refusalCase{
		{"the victim runs and votes", func(*testing.T, *rollout.State) {},
			"node group servers: removing prod-servers-1 would leave one voter of two: tent does not take a group " +
				"from two voters to one yet"},
	})
	runServerCases(t, two, []serverCase{
		{"the victim leads: the transfer comes first", func(t *testing.T, s *rollout.State) {
			machineOf(t, s, serverName(0)).SpecHash = oldHash
			machineOf(t, s, serverName(1)).SpecHash = newHash
		}, serverOutcome{Action: rollout.TransferLeadership, Machine: serverName(0), Server: "r-2"}},
	})
	t.Run("a victim that does not vote is stopped", func(t *testing.T) {
		s := serversState(2)
		addServerNode(&s, 2, oldHash)
		serverNode(t, &s, 2).Voter = false
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
		}, serverOutcome{Action: rollout.WaitHealthy, Voters: 3}},
		{"k: the member is gone and autopilot is unhealthy", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 1)
			s.Nomad.Healthy = false
		}, serverOutcome{Action: rollout.WaitHealthy, Voters: 3}},
		{"k: autopilot is healthy and two servers vote", func(t *testing.T, s *rollout.State) {
			dropPeerAndMember(t, s, 1)
			serverNode(t, s, 2).Voter = false
		}, serverOutcome{Action: rollout.WaitHealthy, Voters: 3}},
		{"k: autopilot is healthy and four servers vote", func(t *testing.T, s *rollout.State) {
			dropServerPeer(t, s, 1)
			serverMember(t, s, 1).Status = "left"
			s.Nomad.Servers = append(s.Nomad.Servers, rollout.Server{ID: "r-9", Name: "x.global", Voter: true,
				Healthy: true, Version: "2.0.7"})
		}, serverOutcome{Action: rollout.WaitHealthy, Voters: 3}},
		{"k: two servers are gone, and one server of the others does not vote", func(t *testing.T, s *rollout.State) {
			stopServer(t, s, 2)
			dropPeerAndMember(t, s, 1)
			dropPeerAndMember(t, s, 2)
			serverNode(t, s, 3).Voter = false
		}, serverOutcome{Action: rollout.WaitHealthy, Voters: 2}},
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

func TestServerLineFIsNotForAStoppedServer(t *testing.T) {
	two := func() rollout.State {
		s := serversState(1)
		addServerNode(&s, 1, oldHash)
		stopServer(t, &s, 1)
		return s
	}
	runServerCases(t, two, []serverCase{
		{"autopilot counts it healthy", func(*testing.T, *rollout.State) {},
			serverOutcome{Action: rollout.WaitServerDown, Machine: serverName(1)}},
		{"autopilot counts it unhealthy", func(t *testing.T, s *rollout.State) {
			serverNode(t, s, 1).Healthy = false
		}, serverOutcome{Action: rollout.RemovePeer, Machine: serverName(1), Server: "r-2"}},
	})
}
