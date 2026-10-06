package app

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
)

// wantSortedFailures fails the test unless got holds exactly the failures of want, in the order of a failure list.
func wantSortedFailures(t *testing.T, got []Failure, want ...Failure) {
	t.Helper()
	sortFailures(got)
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("failures (-want +got):\n%s", diff)
	}
}

// serversOf returns the three server machines of fullCluster.
func serversOf() []cloud.Instance { return fullCluster()[:3] }

// workersOf returns the two client machines of fullCluster.
func workersOf() []cloud.Instance { return fullCluster()[3:] }

// peerOf returns the Raft peer of the server machine in, with a vote.
func peerOf(in cloud.Instance) nomadops.Peer {
	return nomadops.Peer{Name: in.Name + ".global", Address: netip.AddrPortFrom(in.PrivateIP, 4647), Voter: true}
}

// peersOf returns the Raft peers of the server machines.
func peersOf(servers []cloud.Instance) []nomadops.Peer {
	var peers []nomadops.Peer
	for _, in := range servers {
		peers = append(peers, peerOf(in))
	}
	return peers
}

// reportOf returns autopilot's entry for the server machine in: alive and healthy, at version v.
func reportOf(in cloud.Instance, v string) nomadops.ServerHealth {
	return nomadops.ServerHealth{
		Name: in.Name + ".global", Address: netip.AddrPortFrom(in.PrivateIP, 4647), Serf: "alive", Healthy: true,
		Voter: true, Version: v,
	}
}

// healthOf returns autopilot's report of healthy servers at version v, in the reverse of the machines' order, which is
// not the order that tent lists them in.
func healthOf(servers []cloud.Instance, v string) nomadops.Health {
	h := nomadops.Health{Healthy: true, Voters: len(servers)}
	for i := len(servers) - 1; i >= 0; i-- {
		h.Servers = append(h.Servers, reportOf(servers[i], v))
	}
	return h
}

// changeReport returns h with the entry of the server machine in changed by change.
func changeReport(h nomadops.Health, in cloud.Instance, change func(*nomadops.ServerHealth)) nomadops.Health {
	servers := slices.Clone(h.Servers)
	for i := range servers {
		if servers[i].Address.Addr() == in.PrivateIP {
			change(&servers[i])
		}
	}
	h.Servers = servers
	return h
}

// dropReport returns h without the entry of the server machine in.
func dropReport(h nomadops.Health, in cloud.Instance) nomadops.Health {
	h.Servers = slices.DeleteFunc(slices.Clone(h.Servers), func(sv nomadops.ServerHealth) bool {
		return sv.Address.Addr() == in.PrivateIP
	})
	return h
}

func TestServerFailures(t *testing.T) {
	servers := serversOf()
	noVote := func(p []nomadops.Peer, i int) []nomadops.Peer {
		p[i].Voter = false
		return p
	}
	unhealthy := func(h nomadops.Health) nomadops.Health {
		h.Healthy = false
		return h
	}
	noAddress := serversOf()
	noAddress[0].PrivateIP = netip.Addr{}
	for _, tc := range []struct {
		name    string
		servers []cloud.Instance
		peers   []nomadops.Peer
		health  nomadops.Health
		want    []Failure
	}{
		{"healthy servers", servers, peersOf(servers), healthOf(servers, "2.0.7"), nil},
		{
			"no server votes at the address", servers, peersOf(servers[:1]), healthOf(servers, "2.0.7"),
			[]Failure{
				{
					Check: "server-no-vote", Node: "prod-servers-1", ID: "instance-2",
					Detail: "no server votes at its address 10.64.0.4",
				},
				{
					Check: "server-no-vote", Node: "prod-servers-2", ID: "instance-3",
					Detail: "no server votes at its address 10.64.0.5",
				},
			},
		},
		{
			"a server with no vote yet", servers, noVote(peersOf(servers), 1), healthOf(servers, "2.0.7"),
			[]Failure{{
				Check: "server-no-vote", Node: "prod-servers-1", ID: "instance-2",
				Detail: "its server at 10.64.0.4:4647 has no vote",
			}},
		},
		{
			"a peer at another port still counts", servers,
			[]nomadops.Peer{
				{Name: "a.global", Address: netip.MustParseAddrPort("10.64.0.3:4700"), Voter: true},
				peerOf(servers[1]), peerOf(servers[2]),
			},
			healthOf(servers, "2.0.7"), nil,
		},
		{
			"a peer that is no machine", servers,
			append(peersOf(servers), nomadops.Peer{
				Name: "prod-servers-9.global", Address: netip.MustParseAddrPort("10.64.0.9:4647"), Voter: true,
			}),
			healthOf(servers, "2.0.7"),
			[]Failure{{
				Check: "server-unknown",
				Detail: "the Raft configuration lists a server at 10.64.0.9:4647 (prod-servers-9.global) that is no " +
					"server machine of the cluster",
			}},
		},
		{
			"a peer that has no address", servers,
			append(peersOf(servers), nomadops.Peer{Name: "(unknown)"}), healthOf(servers, "2.0.7"),
			[]Failure{{
				Check: "server-unknown",
				Detail: "the Raft configuration lists a server named (unknown) with no usable address, which is no server " +
					"machine of the cluster",
			}},
		},
		{
			"a failed server", servers, peersOf(servers),
			changeReport(healthOf(servers, "2.0.7"), servers[1], func(sv *nomadops.ServerHealth) {
				sv.Serf, sv.Healthy = "failed", false
			}),
			[]Failure{
				{
					Check: "server-not-alive", Node: "prod-servers-1", ID: "instance-2",
					Detail: "Serf reports its server as failed",
				},
				{
					Check: "server-unhealthy", Node: "prod-servers-1", ID: "instance-2",
					Detail: "autopilot reports its server unhealthy",
				},
			},
		},
		{
			"a server that left", servers, peersOf(servers),
			changeReport(healthOf(servers, "2.0.7"), servers[0], func(sv *nomadops.ServerHealth) { sv.Serf = "left" }),
			[]Failure{{
				Check: "server-not-alive", Node: "prod-servers-0", ID: "instance-1",
				Detail: "Serf reports its server as left",
			}},
		},
		{
			"a server that autopilot does not list", servers, peersOf(servers),
			dropReport(healthOf(servers, "2.0.7"), servers[2]),
			[]Failure{{
				Check: "server-not-alive", Node: "prod-servers-2", ID: "instance-3",
				Detail: "autopilot does not list its server",
			}},
		},
		{
			"an unhealthy server that is alive", servers, peersOf(servers),
			changeReport(healthOf(servers, "2.0.7"), servers[0], func(sv *nomadops.ServerHealth) { sv.Healthy = false }),
			[]Failure{{
				Check: "server-unhealthy", Node: "prod-servers-0", ID: "instance-1",
				Detail: "autopilot reports its server unhealthy",
			}},
		},
		{
			"an unhealthy report", servers, peersOf(servers), unhealthy(healthOf(servers, "2.0.7")),
			[]Failure{{Check: "autopilot-unhealthy", Detail: "autopilot reports the servers unhealthy"}},
		},
		{
			"a machine without a private address", noAddress, peersOf(servers), healthOf(servers, "2.0.7"),
			[]Failure{
				{
					Check: "server-no-vote", Node: "prod-servers-0", ID: "instance-1",
					Detail: "the cloud reports no private address for it",
				},
				{
					Check: "server-unknown",
					Detail: "the Raft configuration lists a server at 10.64.0.3:4647 (prod-servers-0.global) that is no " +
						"server machine of the cluster",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantSortedFailures(t, serverFailures(tc.servers, tc.peers, tc.health), tc.want...)
		})
	}
}

func TestClientFailures(t *testing.T) {
	clients := workersOf()
	node := func(in cloud.Instance, status string, eligible bool) nomadops.Node {
		return nomadops.Node{Name: in.Name, Status: status, Eligible: eligible, Address: in.PrivateIP, Version: "2.0.7"}
	}
	ready := []nomadops.Node{node(clients[0], "ready", true), node(clients[1], "ready", true)}
	elsewhere := node(clients[1], "ready", true)
	elsewhere.Address = netip.MustParseAddr("10.64.0.99")
	noAddress := clients[1]
	noAddress.PrivateIP = netip.Addr{}
	for _, tc := range []struct {
		name    string
		clients []cloud.Instance
		nodes   []nomadops.Node
		want    []Failure
	}{
		{"ready clients", clients, ready, nil},
		{
			"a client that is not listed", clients, ready[:1],
			[]Failure{{
				Check: "client-not-registered", Node: "prod-workers-1", ID: "instance-5",
				Detail: "Nomad lists no client of its name at 10.64.0.7",
			}},
		},
		{
			"a client listed at another address", clients, []nomadops.Node{ready[0], elsewhere},
			[]Failure{{
				Check: "client-not-registered", Node: "prod-workers-1", ID: "instance-5",
				Detail: "Nomad lists no client of its name at 10.64.0.7",
			}},
		},
		{
			"a client that is down", clients, []nomadops.Node{ready[0], node(clients[1], "down", true)},
			[]Failure{{
				Check: "client-not-ready", Node: "prod-workers-1", ID: "instance-5",
				Detail: "its Nomad client is down",
			}},
		},
		{
			"a client that is initializing", clients, []nomadops.Node{node(clients[0], "initializing", false), ready[1]},
			[]Failure{{
				Check: "client-not-ready", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its Nomad client is initializing",
			}},
		},
		{
			"a client that is ready but not eligible", clients, []nomadops.Node{ready[0], node(clients[1], "ready", false)},
			[]Failure{{
				Check: "client-not-ready", Node: "prod-workers-1", ID: "instance-5",
				Detail: "its Nomad client is ready but not eligible",
			}},
		},
		{
			"a client that is disconnected", clients, []nomadops.Node{ready[0], node(clients[1], "disconnected", true)},
			[]Failure{{
				Check: "client-not-ready", Node: "prod-workers-1", ID: "instance-5",
				Detail: "its Nomad client is disconnected",
			}},
		},
		{
			"a down node beside a ready one", clients,
			[]nomadops.Node{node(clients[0], "down", true), ready[0], ready[1]}, nil,
		},
		{
			"a down node beside an initializing one", clients,
			[]nomadops.Node{node(clients[0], "down", true), node(clients[0], "initializing", false), ready[1]},
			[]Failure{{
				Check: "client-not-ready", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its Nomad client is initializing",
			}},
		},
		{
			"a machine without a private address", []cloud.Instance{clients[0], noAddress}, ready,
			[]Failure{{
				Check: "client-not-registered", Node: "prod-workers-1", ID: "instance-5",
				Detail: "the cloud reports no private address for it",
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantSortedFailures(t, clientFailures(tc.clients, tc.nodes), tc.want...)
		})
	}
}

func TestVersionFailures(t *testing.T) {
	servers, clients := serversOf(), workersOf()
	node := func(in cloud.Instance, status, v string) nomadops.Node {
		return nomadops.Node{Name: in.Name, Status: status, Eligible: true, Address: in.PrivateIP, Version: v}
	}
	nodes := []nomadops.Node{node(clients[0], "ready", "2.0.7"), node(clients[1], "ready", "2.0.7")}
	for _, tc := range []struct {
		name   string
		health nomadops.Health
		nodes  []nomadops.Node
		want   []Failure
	}{
		{"every version is the pinned one", healthOf(servers, "2.0.7"), nodes, nil},
		{
			"a server of another version",
			changeReport(healthOf(servers, "2.0.7"), servers[1], func(sv *nomadops.ServerHealth) { sv.Version = "2.0.6" }),
			nodes,
			[]Failure{{
				Check: "nomad-version", Node: "prod-servers-1", ID: "instance-2",
				Detail: "its server runs Nomad 2.0.6; the cluster is pinned to 2.0.7",
			}},
		},
		{
			"a client of another version", healthOf(servers, "2.0.7"),
			[]nomadops.Node{nodes[0], node(clients[1], "ready", "2.0.6")},
			[]Failure{{
				Check: "nomad-version", Node: "prod-workers-1", ID: "instance-5",
				Detail: "its client runs Nomad 2.0.6; the cluster is pinned to 2.0.7",
			}},
		},
		{
			"a server that autopilot does not list is not compared",
			dropReport(healthOf(servers, "2.0.6"), servers[1]), nodes,
			[]Failure{
				{
					Check: "nomad-version", Node: "prod-servers-0", ID: "instance-1",
					Detail: "its server runs Nomad 2.0.6; the cluster is pinned to 2.0.7",
				},
				{
					Check: "nomad-version", Node: "prod-servers-2", ID: "instance-3",
					Detail: "its server runs Nomad 2.0.6; the cluster is pinned to 2.0.7",
				},
			},
		},
		{"a client that is not listed is not compared", healthOf(servers, "2.0.7"), nodes[:1], nil},
		{
			"a down client is not compared", healthOf(servers, "2.0.7"),
			[]nomadops.Node{nodes[0], node(clients[1], "down", "2.0.1")}, nil,
		},
		{
			"the node that stands for the machine is the ready one", healthOf(servers, "2.0.7"),
			[]nomadops.Node{node(clients[0], "down", "2.0.1"), nodes[0], nodes[1]}, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantSortedFailures(t, versionFailures(servers, tc.health, clients, tc.nodes, "2.0.7"), tc.want...)
		})
	}
}

func TestCertificateFindings(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	// created returns the creation time of a machine whose node certificate ends d after now.
	created := func(d time.Duration) time.Time { return now.Add(d).AddDate(-1, 0, 0) }
	workers := workersOf()
	workers[0].Created, workers[1].Created = created(200*day), created(200*day)
	farCA := now.AddDate(9, 0, 0)
	for _, tc := range []struct {
		name         string
		caEnd        time.Time
		created0     time.Time // the creation time of prod-workers-0; of prod-workers-1 it is 200 days
		wantFailures []Failure
		wantWarnings []string
	}{
		{name: "far from the end", caEnd: farCA, created0: created(200 * day)},
		{name: "31 days from the end", caEnd: farCA, created0: created(31 * day)},
		{
			name: "30 days from the end", caEnd: farCA, created0: created(30 * day),
			wantWarnings: []string{"the certificate of node prod-workers-0 ends about 2026-11-05, in 30 days; node " +
				"certificates last one year, and a node gets a new one when it is replaced"},
		},
		{
			name: "29 days from the end", caEnd: farCA, created0: created(29*day + time.Hour),
			wantWarnings: []string{"the certificate of node prod-workers-0 ends about 2026-11-04, in 29 days; node " +
				"certificates last one year, and a node gets a new one when it is replaced"},
		},
		{
			name: "one day from the end", caEnd: farCA, created0: created(day + time.Hour),
			wantWarnings: []string{"the certificate of node prod-workers-0 ends about 2026-10-07, in 1 day; node " +
				"certificates last one year, and a node gets a new one when it is replaced"},
		},
		{
			name: "hours from the end", caEnd: farCA, created0: created(5 * time.Hour),
			wantWarnings: []string{"the certificate of node prod-workers-0 ends about 2026-10-06, in less than a day; " +
				"node certificates last one year, and a node gets a new one when it is replaced"},
		},
		{
			name: "a day past the end", caEnd: farCA, created0: created(-day),
			wantFailures: []Failure{{
				Check: "certificate-expired", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its node certificate ended about 2026-10-05",
			}},
		},
		{
			name: "at the end", caEnd: farCA, created0: created(0),
			wantFailures: []Failure{{
				Check: "certificate-expired", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its node certificate ended about 2026-10-06",
			}},
		},
		{name: "no creation time", caEnd: farCA},
		{
			name: "the CA 31 days from its end", caEnd: now.Add(31 * day), created0: created(200 * day),
		},
		{
			name: "the CA 29 days from its end", caEnd: now.Add(29 * day), created0: created(200 * day),
			wantWarnings: []string{"the cluster CA ends on 2026-11-04, in 29 days; tent cannot renew a CA yet"},
		},
		{
			name: "the CA past its end", caEnd: now.Add(-day), created0: created(200 * day),
			wantFailures: []Failure{{
				Check: "certificate-expired", Detail: "the cluster CA ended on 2026-10-05",
			}},
		},
		{
			name: "a node and the CA", caEnd: now.Add(10 * day), created0: created(-2 * day),
			wantFailures: []Failure{{
				Check: "certificate-expired", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its node certificate ended about 2026-10-04",
			}},
			wantWarnings: []string{"the cluster CA ends on 2026-10-16, in 10 days; tent cannot renew a CA yet"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machines := []cloud.Instance{workers[1], workers[0]}
			machines[1].Created = tc.created0
			failures, warnings := certificateFindings(tc.caEnd, machines, now)

			wantSortedFailures(t, failures, tc.wantFailures...)
			if diff := cmp.Diff(tc.wantWarnings, warnings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCertificateFindingsOrderTheWarningsByNode tells the warnings of the nodes by name, then the CA's.
func TestCertificateFindingsOrderTheWarningsByNode(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	soon := now.Add(48*time.Hour).AddDate(-1, 0, 0)
	machines := fullCluster()
	for i := range machines {
		machines[i].Created = now
	}
	machines[4].Created, machines[0].Created = soon, soon

	_, warnings := certificateFindings(now.Add(48*time.Hour), machines, now)

	for i, prefix := range []string{
		"the certificate of node prod-servers-0 ends", "the certificate of node prod-workers-1 ends", "the cluster CA ends",
	} {
		if len(warnings) != 3 || !strings.HasPrefix(warnings[i], prefix) {
			t.Fatalf("warnings = %q, want those of prod-servers-0, prod-workers-1 and the CA, in this order", warnings)
		}
	}
}

// TestFindingsAreOfTheRoles checks that a machine of a client group is no expected server.
func TestFindingsAreOfTheRoles(t *testing.T) {
	m := validateModel()
	m.Groups = append(m.Groups,
		model.NodeGroup{Name: "mixed", Role: v1alpha1.RoleCombined, Zones: []string{"ams"}, Size: 1})
	set := planMachines(m, append(fullCluster(), machine(6, "prod-mixed-0", "mixed", v1alpha1.RoleCombined)))

	servers, clients := names(set.servers(m)), names(set.clients(m))

	wantServers := []string{"prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-mixed-0"}
	if diff := cmp.Diff(wantServers, servers); diff != "" {
		t.Errorf("servers (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"prod-workers-0", "prod-workers-1", "prod-mixed-0"}, clients); diff != "" {
		t.Errorf("clients (-want +got):\n%s", diff)
	}
}

// names returns the names of the machines.
func names(machines []cloud.Instance) []string {
	var out []string
	for _, in := range machines {
		out = append(out, in.Name)
	}
	return out
}
