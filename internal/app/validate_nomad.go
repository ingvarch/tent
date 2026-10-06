package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/statestore"
)

// certificateWarnDays is how many days before its end a certificate is a warning.
const certificateWarnDays = 30

// noPrivateAddress says why the Nomad checks of a machine cannot be made.
const noPrivateAddress = "the cloud reports no private address for it"

// nomadFindings are the failures and the warnings of the checks of Nomad and of the certificates.
type nomadFindings struct {
	failures []Failure
	warnings []string
}

// checkNomad makes the checks of Nomad and of the certificates for the cluster c, whose machines are set, at now. The
// store must hold the CA, the gossip key, the bootstrap secret and the mark of the bootstrap, and a server that stays
// must have a public address: otherwise the one failure nomad-not-set-up says what is missing, and Nomad is not asked.
// The certificates of the machines that stay, and the CA, are checked when the store holds all four secrets.
func (s *Service) checkNomad(ctx context.Context, l statestore.Layout, c loadedCluster, set machineSet, now time.Time,
) (nomadFindings, error) {
	secrets, err := s.storedSecrets(ctx, l)
	if missing, ok := errors.AsType[*missingSecretsError](err); ok {
		return nomadFindings{failures: []Failure{notSetUp(missing.Error())}}, nil
	}
	if err != nil {
		return nomadFindings{}, err
	}
	var found nomadFindings
	found.failures, found.warnings = certificateFindings(secrets.ca.Certificate().NotAfter, set.stays, now)
	asked, err := s.askNomad(ctx, l, c, set, secrets)
	found.failures = append(found.failures, asked...)
	return found, err
}

// notSetUp returns the failure of a cluster whose Nomad tent cannot ask, for the reason detail.
func notSetUp(detail string) Failure { return Failure{Check: checkNomadNotSetUp, Detail: detail} }

// askNomad asks the servers that stay and have a public address one round of questions, and returns the failures of
// the answers. A Nomad that does not answer, and a cluster that tent cannot ask, are failures. The errors are a store
// that cannot be read, a Nomad client that cannot be made, and the end of ctx.
func (s *Service) askNomad(ctx context.Context, l statestore.Layout, c loadedCluster, set machineSet,
	secrets clusterSecrets,
) ([]Failure, error) {
	marked, err := s.bootstrapped(ctx, l)
	if err != nil {
		return nil, err
	}
	if !marked {
		return []Failure{notSetUp("Nomad is not bootstrapped yet: tent update cluster --yes bootstraps it")}, nil
	}
	servers, clients := set.servers(c.m), set.clients(c.m)
	reachable := slices.DeleteFunc(slices.Clone(servers), func(in cloud.Instance) bool { return !in.PublicIP.IsValid() })
	if len(reachable) == 0 {
		return []Failure{notSetUp("no server has a public address")}, nil
	}
	api, err := s.nomadOver(reachable, nomadAccess{
		cluster: c.m.Name, region: c.objs.Cluster.Spec.Nomad.Region, secrets: secrets,
	})
	if err != nil {
		return nil, err
	}
	view, unreachable, err := askRound(ctx, api)
	if err != nil || unreachable != nil {
		return orNone(unreachable), err
	}
	pinned := c.objs.Cluster.Spec.Nomad.Version
	fs := serverFailures(servers, view.peers, view.health)
	fs = append(fs, clientFailures(clients, view.nodes)...)
	return append(fs, versionFailures(servers, view.health, clients, view.nodes, pinned)...), nil
}

// orNone returns the failure f as a list, or nil for no failure.
func orNone(f *Failure) []Failure {
	if f == nil {
		return nil
	}
	return []Failure{*f}
}

// nomadView is what one round of questions got from Nomad.
type nomadView struct {
	peers  []nomadops.Peer
	health nomadops.Health
	nodes  []nomadops.Node
}

// askRound asks Nomad for the leader, the Raft configuration, autopilot's health and the nodes, once each, in this
// order. The first call that fails ends the round with the failure nomad-no-leader, which names the call. An error is
// returned only when ctx has ended.
func askRound(ctx context.Context, api nomadops.API) (view nomadView, unreachable *Failure, err error) {
	for _, step := range []struct {
		what string
		call func() error
	}{
		{"", func() error { _, err := api.Leader(ctx); return err }},
		{"read the Raft configuration", func() (err error) { view.peers, err = api.Peers(ctx); return }},
		{"read autopilot's health", func() (err error) { view.health, err = api.Health(ctx); return }},
		{"list the nodes", func() (err error) { view.nodes, err = api.Nodes(ctx); return }},
	} {
		if err := step.call(); err != nil {
			if ctx.Err() != nil {
				return nomadView{}, nil, err
			}
			cause := err.Error()
			if step.what != "" {
				cause = step.what + ": " + cause
			}
			return nomadView{}, &Failure{
				Check: checkNoLeader,
				Detail: fmt.Sprintf("Nomad has no leader, or tent cannot reach it: %s; tent reaches the servers on port %d: "+
					"check spec.access.api", cause, model.APIPort),
			}, nil
		}
	}
	return view, nil, nil
}

// nodeFailure returns the failure of the check for the node of the machine in, with the detail.
func nodeFailure(check string, in cloud.Instance, detail string) Failure {
	return Failure{Check: check, Node: in.Name, ID: in.ID, Detail: detail}
}

// serverFailures returns the failures of the servers, which are the machines of the server and combined groups that
// stay, against the Raft configuration peers and autopilot's report health, in no particular order. A server is told
// by its private address: the vote comes from the Raft configuration, and whether it is alive and healthy from the
// report, in which Nomad changes the order of the servers between calls.
func serverFailures(servers []cloud.Instance, peers []nomadops.Peer, health nomadops.Health) []Failure {
	var fs []Failure
	if !health.Healthy {
		fs = append(fs, Failure{Check: checkAutopilotUnhealthy, Detail: "autopilot reports the servers unhealthy"})
	}
	for _, in := range servers {
		if !in.PrivateIP.IsValid() {
			fs = append(fs, nodeFailure(checkServerNoVote, in, noPrivateAddress))
			continue
		}
		fs = append(fs, voteFailures(in, peers)...)
		fs = append(fs, reportFailures(in, health.Servers)...)
	}
	for _, p := range peers {
		if !isMachine(servers, p) {
			fs = append(fs, unknownPeerFailure(p))
		}
	}
	return fs
}

// voteFailures returns the failure of the server machine in when the Raft configuration gives it no vote.
func voteFailures(in cloud.Instance, peers []nomadops.Peer) []Failure {
	p, ok := nomadops.FindPeer(peers, in.PrivateIP)
	switch {
	case !ok:
		return []Failure{nodeFailure(checkServerNoVote, in, "no server votes at its address "+in.PrivateIP.String())}
	case !p.Voter:
		return []Failure{nodeFailure(checkServerNoVote, in, "its server at "+p.Address.String()+" has no vote")}
	}
	return nil
}

// serverReport returns autopilot's entry for the server machine in, which it finds by the machine's private address.
func serverReport(report []nomadops.ServerHealth, in cloud.Instance) (nomadops.ServerHealth, bool) {
	i := slices.IndexFunc(report, func(sv nomadops.ServerHealth) bool {
		return in.PrivateIP.IsValid() && sv.Address.Addr() == in.PrivateIP
	})
	if i < 0 {
		return nomadops.ServerHealth{}, false
	}
	return report[i], true
}

// reportFailures returns the failures of the server machine in that autopilot's report shows: its server is not listed,
// is not alive in Serf, or is not healthy.
func reportFailures(in cloud.Instance, report []nomadops.ServerHealth) []Failure {
	sv, ok := serverReport(report, in)
	var fs []Failure
	switch {
	case !ok:
		return []Failure{nodeFailure(checkServerNotAlive, in, "autopilot does not list its server")}
	case sv.Serf != "alive":
		fs = append(fs, nodeFailure(checkServerNotAlive, in, "Serf reports its server as "+sv.Serf))
	}
	if !sv.Healthy {
		fs = append(fs, nodeFailure(checkServerUnhealthy, in, "autopilot reports its server unhealthy"))
	}
	return fs
}

// unknownPeerFailure returns the failure of the peer p, which is no server machine of the cluster.
func unknownPeerFailure(p nomadops.Peer) Failure {
	listed := fmt.Sprintf("a server at %s (%s) that is", p.Address, p.Name)
	if !p.Address.IsValid() {
		listed = fmt.Sprintf("a server named %s with no usable address, which is", p.Name)
	}
	return Failure{
		Check: checkServerUnknown, Detail: "the Raft configuration lists " + listed + " no server machine of the cluster",
	}
}

// isMachine reports whether the peer p is at the private address of one of the machines.
func isMachine(machines []cloud.Instance, p nomadops.Peer) bool {
	return slices.ContainsFunc(machines, func(in cloud.Instance) bool {
		return in.PrivateIP.IsValid() && in.PrivateIP == p.Address.Addr()
	})
}

// clientNode returns the node that stands for the client machine in among nodes: one that Nomad lists at the machine's
// name and private address, a ready one first, then one that is not down, then a down one. It reports false when
// Nomad lists none.
func clientNode(nodes []nomadops.Node, in cloud.Instance) (nomadops.Node, bool) {
	listed := slices.DeleteFunc(slices.Clone(nodes), func(n nomadops.Node) bool { return !n.Is(in.Name, in.PrivateIP) })
	if len(listed) == 0 {
		return nomadops.Node{}, false
	}
	if i := slices.IndexFunc(listed, nomadops.Node.Ready); i >= 0 {
		return listed[i], true
	}
	if registered(listed, in) {
		listed = slices.DeleteFunc(listed, func(n nomadops.Node) bool { return n.Status == "down" })
	}
	return listed[0], true
}

// clientFailures returns the failures of the clients, which are the machines of the client and combined groups that
// stay, against the nodes that Nomad lists, in no particular order. A client is told by its name and private address.
func clientFailures(clients []cloud.Instance, nodes []nomadops.Node) []Failure {
	var fs []Failure
	for _, in := range clients {
		if !in.PrivateIP.IsValid() {
			fs = append(fs, nodeFailure(checkClientNotRegistered, in, noPrivateAddress))
			continue
		}
		n, ok := clientNode(nodes, in)
		switch {
		case !ok:
			fs = append(fs, nodeFailure(checkClientNotRegistered, in,
				"Nomad lists no client of its name at "+in.PrivateIP.String()))
		case !n.Ready():
			state := n.Status
			if n.Status == "ready" {
				state = "ready but not eligible"
			}
			fs = append(fs, nodeFailure(checkClientNotReady, in, "its Nomad client is "+state))
		}
	}
	return fs
}

// versionFailures returns a failure for each server in autopilot's report and each client among nodes that runs a
// Nomad version other than pinned, in no particular order. A server that the report lacks, and a client that Nomad does
// not list or lists as down, are not compared.
func versionFailures(servers []cloud.Instance, health nomadops.Health, clients []cloud.Instance,
	nodes []nomadops.Node, pinned string,
) []Failure {
	var fs []Failure
	differs := func(in cloud.Instance, role, version string) {
		fs = append(fs, nodeFailure(checkNomadVersion, in,
			fmt.Sprintf("its %s runs Nomad %s; the cluster is pinned to %s", role, version, pinned)))
	}
	for _, in := range servers {
		if sv, ok := serverReport(health.Servers, in); ok && sv.Version != pinned {
			differs(in, "server", sv.Version)
		}
	}
	for _, in := range clients {
		if n, ok := clientNode(nodes, in); ok && n.Status != "down" && n.Version != pinned {
			differs(in, "client", n.Version)
		}
	}
	return fs
}

// certificateFindings checks the end of the CA's certificate, caEnd, and of the node certificate of each machine, which
// ends a year after the machine's creation, against now. A certificate that has ended is a failure; one that ends
// within certificateWarnDays days is a warning. The warnings of the nodes come by name, then the CA's. A machine
// without a creation time is not checked.
func certificateFindings(caEnd time.Time, machines []cloud.Instance, now time.Time) ([]Failure, []string) {
	var fs []Failure
	var ws []string
	if !caEnd.After(now) {
		fs = append(fs, Failure{Check: checkCertificateExpired, Detail: "the cluster CA ended on " + dateOf(caEnd)})
	}
	byName := slices.Clone(machines)
	slices.SortFunc(byName, compareName)
	for _, in := range byName {
		if in.Created.IsZero() {
			continue
		}
		end := pki.NodeCertificateEnd(in.Created)
		switch {
		case !end.After(now):
			fs = append(fs, nodeFailure(checkCertificateExpired, in, "its node certificate ended about "+dateOf(end)))
		case endsSoon(end, now):
			ws = append(ws, fmt.Sprintf("the certificate of node %s ends about %s, %s; node certificates last one year, and "+
				"a node gets a new one when it is replaced", in.Name, dateOf(end), inDays(end.Sub(now))))
		}
	}
	if caEnd.After(now) && endsSoon(caEnd, now) {
		ws = append(ws, fmt.Sprintf("the cluster CA ends on %s, %s; tent cannot renew a CA yet", dateOf(caEnd),
			inDays(caEnd.Sub(now))))
	}
	return fs, ws
}

// endsSoon reports whether a certificate that ends at end is within certificateWarnDays days of now.
func endsSoon(end, now time.Time) bool { return end.Sub(now) <= certificateWarnDays*24*time.Hour }

// dateOf returns the UTC date of t, such as 2026-11-04.
func dateOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// inDays says how far ahead d is, in whole days: "in 29 days", "in 1 day" or "in less than a day".
func inDays(d time.Duration) string {
	switch days := int(d / (24 * time.Hour)); days {
	case 0:
		return "in less than a day"
	case 1:
		return "in 1 day"
	default:
		return "in " + strconv.Itoa(days) + " days"
	}
}

// in returns the machines that stay, oldest first, of the node groups of m whose role runs what runs says.
func (set machineSet) in(m *model.Cluster, runs func(v1alpha1.Role) bool) []cloud.Instance {
	var out []cloud.Instance
	for _, in := range set.stays {
		if g, ok := findGroup(m, in.Group); ok && runs(g.Role) {
			out = append(out, in)
		}
	}
	return out
}

// servers returns the machines that stay in the server and combined groups of m, oldest first.
func (set machineSet) servers(m *model.Cluster) []cloud.Instance {
	return set.in(m, v1alpha1.Role.RunsServer)
}

// clients returns the machines that stay in the client and combined groups of m, oldest first.
func (set machineSet) clients(m *model.Cluster) []cloud.Instance {
	return set.in(m, v1alpha1.Role.RunsClient)
}
