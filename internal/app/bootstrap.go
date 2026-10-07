package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/statestore"
)

// nomadTimeout is how long each wait of the Nomad step may take: for a leader, for healthy servers, for the keyring and
// for a node to register.
const nomadTimeout = 10 * time.Minute

// operatorCertTTL is how long the operator certificate that tent calls Nomad with is valid: longer than any run. It is
// made for one run and never stored.
const operatorCertTTL = 24 * time.Hour

// errNoNomad is why an update or a validation fails when the service has no way to reach Nomad.
var errNoNomad = errors.New("no Nomad client is set up")

// scrubTimeout is how long the scrub of one node may take.
const scrubTimeout = 5 * time.Minute

// bootsMachine reports whether the change makes or finds a machine with user data: a create, or a wait that repeats a
// create with its operation id. Other waits call no cloud and need no user data.
func bootsMachine(c NodeChange) bool {
	return c.Action == NodeCreate || c.Action == NodeWait && c.Op != ""
}

// changesNodes reports whether the node changes create a node or wait for one that the cloud has not made ready.
func changesNodes(changes []NodeChange) bool { return slices.ContainsFunc(changes, bootsMachine) }

// clusterServers returns how many machines the server and combined groups of the cluster have.
func clusterServers(m *model.Cluster) int {
	servers := 0
	for _, g := range m.Groups {
		if g.Role.RunsServer() {
			servers += max(g.Size, 0)
		}
	}
	return servers
}

// planNomad returns what an update does with Nomad. It bootstraps the ACL system and waits for healthy servers while
// the store holds no mark of the bootstrap, and also when no server or combined machine stays: the servers that come
// are a new Nomad whatever the mark says. With the mark and servers that stay it only waits for healthy servers, when
// the changes create or wait for a server or combined node. It is nil when Nomad needs nothing.
func planNomad(m *model.Cluster, changes []NodeChange, staying []cloud.Instance, marked bool) *NomadStep {
	servers := clusterServers(m)
	switch {
	case !marked || len(staying) == 0:
		return &NomadStep{Bootstrap: true, Servers: servers}
	case slices.ContainsFunc(changes, func(c NodeChange) bool { return c.Action != NodeDelete && isServerChange(c) }):
		return &NomadStep{Servers: servers}
	}
	return nil
}

// needsNomad reports whether applying the plan calls Nomad: for the Nomad step, for a client's intro token or its
// registration wait, or to ask whether a node that the plan deletes has joined.
func (u updateRun) needsNomad() bool {
	return u.plan.Nomad != nil ||
		slices.ContainsFunc(u.plan.Nodes, func(c NodeChange) bool { return c.Action == NodeDelete || !isServerChange(c) })
}

// prepareNodes gives each create and each wait with an operation id of the plan the spec hash of its group, and builds
// the user data of each as the apply will, with the longest seed and, for a client, an intro token of the size of a
// large real one, so that a node whose user data does not fit fails the plan. The certificate that it issues for the
// check is not kept.
func (u *updateRun) prepareNodes(m *model.Cluster, now time.Time) error {
	seed := lastAddresses(m.CIDR, u.builder.servers)
	for i, c := range u.plan.Nodes {
		if !bootsMachine(c) {
			continue
		}
		u.plan.Nodes[i].SpecHash = u.builder.specHash(c.Group)
		var intro pki.Secret
		if u.builder.role(c.Group) == v1alpha1.RoleClient {
			intro = standInIntroToken()
		}
		if _, err := u.userData(c, now, seed, intro); err != nil {
			return err
		}
	}
	return nil
}

// userData returns the user data of the node that the change c creates or waits for: the NodeConfig of its group with
// the node's name, a certificate issued at now, the seed and the intro token.
func (u updateRun) userData(c NodeChange, now time.Time, seed []netip.Addr, intro pki.Secret) (cloud.UserData, error) {
	cert, err := u.secrets.ca.IssueNode(u.builder.role(c.Group), u.region, now)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", c.Name, err)
	}
	nc, err := u.builder.node(c.Group, c.Name, c.Zone, cert, seed, intro)
	if err != nil {
		return nil, err
	}
	data, err := nodeconfig.UserData(nc)
	if err != nil {
		return nil, err
	}
	return cloud.UserData(data), nil
}

// lastAddresses returns n addresses of the IPv4 prefix p, from its last one down: the longest texts that a seed can
// hold. For another prefix it returns n times the prefix's address.
func lastAddresses(p netip.Prefix, n int) []netip.Addr {
	addrs := make([]netip.Addr, 0, max(n, 0))
	a := p.Addr()
	if p.Addr().Is4() {
		a = netip.AddrFrom4(lastOf(p))
	}
	for range max(n, 0) {
		addrs = append(addrs, a)
		if prev := a.Prev(); prev.IsValid() && p.Contains(prev) {
			a = prev
		}
	}
	return addrs
}

// lastOf returns the last address of the IPv4 prefix p.
func lastOf(p netip.Prefix) [4]byte {
	a := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	for i := range a {
		bits := min(max(host-8*(3-i), 0), 8)
		a[i] |= byte(uint(1)<<bits - 1)
	}
	return a
}

// applier carries out the node changes and the Nomad step of an update.
type applier struct {
	s *Service
	u updateRun
	// known are the servers that exist, by name: those that stay and those that this run made or waited for.
	known []cloud.Instance
	api   nomadops.API // over the known servers; nil until a step needs it
}

// run carries out the node changes in the order of the plan: the creates and waits of the server and combined nodes,
// the Nomad step, then the other changes, which are the clients' waits and creates and the deletes.
func (a *applier) run(ctx context.Context) error {
	var servers, rest []NodeChange
	for _, c := range a.u.plan.Nodes {
		if c.Action != NodeDelete && isServerChange(c) {
			servers = append(servers, c)
		} else {
			rest = append(rest, c)
		}
	}
	for _, c := range servers {
		if err := a.applyServer(ctx, c); err != nil {
			return err
		}
	}
	if a.u.plan.Nomad != nil {
		if err := a.nomadStep(ctx, servers); err != nil {
			return err
		}
	}
	for _, c := range rest {
		apply := a.applyClient
		if c.Action == NodeDelete {
			apply = a.applyDelete
		}
		if err := apply(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// applyServer creates the server or combined node of c, or waits for it, and adds it to the known servers. The node
// boots with the private addresses of the other known servers as its seed. A wait without an operation id calls no
// cloud and makes no user data: the machine already exists, and the Nomad step scrubs it once its node has joined.
func (a *applier) applyServer(ctx context.Context, c NodeChange) error {
	if !bootsMachine(c) {
		return nil
	}
	in, err := a.s.applyNodeWith(ctx, a.u.nodes, a.u.cluster, c, func(context.Context) (cloud.UserData, error) {
		seed, err := a.seed(c.Name, false)
		if err != nil {
			return nil, err
		}
		return a.u.userData(c, a.s.now(), seed, nil)
	})
	if err != nil {
		return err
	}
	a.know(in)
	return nil
}

// applyClient creates the client node of c, or waits for it with its operation id, with an intro token; then waits
// until it registers, and scrubs it. A wait without an operation id asks for no token and calls no cloud before the
// scrub: the machine already exists.
func (a *applier) applyClient(ctx context.Context, c NodeChange) error {
	in := a.machineOf(c)
	if bootsMachine(c) {
		var err error
		if in, err = a.bootClient(ctx, c); err != nil {
			return err
		}
	}
	if err := a.register(ctx, in); err != nil {
		return err
	}
	return a.markJoined(ctx, in)
}

// bootClient creates the client node of c, or repeats its create, with an intro token, and returns its machine.
func (a *applier) bootClient(ctx context.Context, c NodeChange) (cloud.Instance, error) {
	return a.s.applyNodeWith(ctx, a.u.nodes, a.u.cluster, c, func(ctx context.Context) (cloud.UserData, error) {
		api, err := a.nomadAPI()
		if err != nil {
			return nil, err
		}
		seed, err := a.seed(c.Name, true)
		if err != nil {
			return nil, err
		}
		intro, err := api.IntroToken(ctx, nomadops.IntroRequest{
			NodeName: c.Name, NodePool: a.u.builder.nodePool(c.Group), TTL: nomadops.MaxIntroTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("intro token for node %s: %w", c.Name, err)
		}
		return a.u.userData(c, a.s.now(), seed, intro)
	})
}

// seed returns the private addresses of the known servers other than the node called name, in the order of their
// names. It fails when servers are known and none has an address yet, as a server's join would be in vain; and when
// none is known, if the node is not a server.
func (a *applier) seed(name string, client bool) ([]netip.Addr, error) {
	var seed []netip.Addr
	var missing []string
	for _, in := range a.known {
		switch {
		case in.Name == name:
		case in.PrivateIP.IsValid():
			seed = append(seed, in.PrivateIP)
		default:
			missing = append(missing, in.Name)
		}
	}
	if len(seed) > 0 || len(missing) == 0 && !client {
		return seed, nil
	}
	detail := ""
	if len(missing) > 0 {
		detail = " (" + strings.Join(missing, ", ") + ")"
	}
	return nil, fmt.Errorf("node %s: no server of %s has a private address yet%s; run the command again", name,
		clusterLabel(a.u.cluster), detail)
}

// know adds the machine of a server to the known servers, in the place of the one of its name.
func (a *applier) know(in cloud.Instance) {
	i := slices.IndexFunc(a.known, func(k cloud.Instance) bool { return k.Name == in.Name })
	if i < 0 {
		a.known = append(a.known, in)
	} else {
		a.known[i] = in
	}
	slices.SortFunc(a.known, compareName)
}

// nomadAPI returns the API over the known servers that have a public address, which it makes on its first call.
func (a *applier) nomadAPI() (nomadops.API, error) {
	if a.api != nil {
		return a.api, nil
	}
	api, err := a.s.nomadOver(a.known, a.u.nomad())
	if err != nil {
		return nil, err
	}
	a.api = api
	return api, nil
}

// nomadAccess is what tent needs to call the servers of a cluster.
type nomadAccess struct {
	cluster string
	region  string // the Nomad region
	secrets clusterSecrets
}

// nomad returns what the run needs to call the cluster's servers.
func (u updateRun) nomad() nomadAccess {
	return nomadAccess{cluster: u.cluster, region: u.region, secrets: u.secrets}
}

// apiAddress returns the address of the Nomad API of the machine in, as host:port, at its public address.
func apiAddress(in cloud.Instance) string {
	return net.JoinHostPort(in.PublicIP.String(), strconv.Itoa(model.APIPort))
}

// reachHint says where to look when no server answered: tent reaches the servers through spec.access.api.
func reachHint() string {
	return fmt.Sprintf("; tent reaches the servers on port %d: check spec.access.api", model.APIPort)
}

// withReachHint adds reachHint to err, which says that no server answered.
func withReachHint(err error) error { return fmt.Errorf("%w%s", err, reachHint()) }

// nomadOver returns the API over those of the machines in servers that have a public address, which also tells which
// server answered last: one client for each, with an operator certificate that it makes now and the ACL bootstrap
// secret as the token.
func (s *Service) nomadOver(servers []cloud.Instance, access nomadAccess) (*nomadops.Servers, error) {
	if s.Nomad == nil {
		return nil, errNoNomad
	}
	cert, err := access.secrets.ca.IssueOperator(access.region, operatorCertTTL, s.now())
	if err != nil {
		return nil, err
	}
	var over []nomadops.Server
	for _, in := range servers {
		if !in.PublicIP.IsValid() {
			continue
		}
		addr := apiAddress(in)
		api, err := s.Nomad(nomadops.Config{
			Address: addr, Region: access.region, CA: access.secrets.ca.Bundle(), Cert: cert, Token: access.secrets.bootstrap,
		})
		if err != nil {
			return nil, fmt.Errorf("reach server %s: %w", in.Name, err)
		}
		over = append(over, nomadops.Server{Address: addr, API: api})
	}
	if len(over) == 0 {
		return nil, fmt.Errorf("%s: no server has a public address", clusterLabel(access.cluster))
	}
	return nomadops.NewServers(over...)
}

// nomadStep waits for a leader, bootstraps the ACL system when the plan says so, and waits for healthy servers that
// all vote and for an active key in Nomad's keyring, which Nomad makes after it elects a leader and which it needs to
// sign the intro tokens of clients. Then, when the plan has a server or combined change, it reads the Raft
// configuration once and, for each such change in order, waits for a combined node to register, checks that the
// machine's server votes at its address, and scrubs the machine. Last it stores the mark of the bootstrap when it
// bootstrapped.
func (a *applier) nomadStep(ctx context.Context, servers []NodeChange) error {
	api, err := a.nomadAPI()
	if err != nil {
		return err
	}
	if err := a.step(NomadEvent{Action: NomadLeader}, func() (NomadEvent, error) {
		waitCtx, cancel := context.WithTimeout(ctx, nomadTimeout)
		defer cancel()
		leader, err := nomadops.WaitLeader(waitCtx, api)
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = withReachHint(err)
		}
		return NomadEvent{Action: NomadLeader, Leader: leader}, err
	}); err != nil {
		return err
	}
	if a.u.plan.Nomad.Bootstrap {
		if err := a.step(NomadEvent{Action: NomadBootstrap}, func() (NomadEvent, error) {
			return NomadEvent{Action: NomadBootstrap}, api.Bootstrap(ctx, a.u.secrets.bootstrap)
		}); err != nil {
			return fmt.Errorf("bootstrap the ACL system: %w", err)
		}
	}
	want, voters := a.u.plan.Nomad.Servers, 0
	if err := a.step(NomadEvent{Action: NomadHealthy, Voters: want}, func() (NomadEvent, error) {
		waitCtx, cancel := context.WithTimeout(ctx, nomadTimeout)
		defer cancel()
		h, err := nomadops.WaitHealthy(waitCtx, api, want)
		voters = h.Voters
		return NomadEvent{Action: NomadHealthy, Voters: h.Voters}, err
	}); err != nil {
		return err
	}
	if err := a.step(NomadEvent{Action: NomadKeyring}, func() (NomadEvent, error) {
		waitCtx, cancel := context.WithTimeout(ctx, nomadTimeout)
		defer cancel()
		return NomadEvent{Action: NomadKeyring}, nomadops.WaitKeyring(waitCtx, api)
	}); err != nil {
		return err
	}
	if err := a.scrubServers(ctx, api, servers, voters); err != nil {
		return err
	}
	if a.u.plan.Nomad.Bootstrap {
		return a.markBootstrapped(ctx)
	}
	return nil
}

// scrubServers scrubs the machine of each server and combined change in servers, in order, once its server votes at its
// private address in the Raft configuration, which it reads once. A combined node must have registered first. voters
// is how many servers the health wait found voting.
func (a *applier) scrubServers(ctx context.Context, api nomadops.API, servers []NodeChange, voters int) error {
	if len(servers) == 0 {
		return nil
	}
	peers, err := api.Peers(ctx)
	if err != nil {
		return fmt.Errorf("read the Raft configuration: %w", err)
	}
	for _, c := range servers {
		in := a.machineOf(c)
		if c.Role == v1alpha1.RoleCombined {
			if err := a.register(ctx, in); err != nil {
				return err
			}
		}
		if err := checkVote(peers, in, voters); err != nil {
			return err
		}
		if err := a.markJoined(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// checkVote fails unless the Raft configuration lists a voter at the private address of the machine in. voters is how
// many servers the cluster has that vote.
func checkVote(peers []nomadops.Peer, in cloud.Instance, voters int) error {
	if err := privateAddress(in); err != nil {
		return err
	}
	if p, ok := nomadops.FindPeer(peers, in.PrivateIP); !ok || !p.Voter {
		return fmt.Errorf("node %s: the servers are healthy with %d %s, but none votes at its address %s", in.Name,
			voters, voterNoun(voters), in.PrivateIP)
	}
	return nil
}

// voterNoun returns "voter" for one and "voters" for any other count.
func voterNoun(n int) string {
	if n == 1 {
		return "voter"
	}
	return "voters"
}

// markJoined replaces the user data of the machine in with the stub and labels the machine as joined, and reports the
// scrub as a step. A failure stops the run with the provider's error; the next run plans the wait again.
func (a *applier) markJoined(ctx context.Context, in cloud.Instance) error {
	step := NodeChange{Action: NodeScrub, Name: in.Name, ID: in.ID}
	a.s.progress(Progress{Node: step, Step: NodeStarted})
	ctx, cancel := context.WithTimeout(ctx, scrubTimeout)
	defer cancel()
	if err := a.u.nodes.MarkJoined(ctx, in); err != nil {
		a.s.progress(Progress{Node: step, Step: NodeFailed, Err: err})
		return err
	}
	a.s.progress(Progress{Node: step, Step: NodeDone})
	return nil
}

// markBootstrapped stores the mark that the ACL system was bootstrapped.
func (a *applier) markBootstrapped(ctx context.Context) error {
	path := a.u.layout.NomadBootstrapped()
	mark := []byte(a.s.now().UTC().Format(time.RFC3339) + "\n")
	if _, err := a.s.Store.Put(ctx, path, mark, statestore.PutOptions{}); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// machineOf returns the machine of the node change c: the known server of its name, which this run may have made, or
// else the machine that the cloud listed with its ID, as a wait without an operation id names it. A machine that is
// neither is returned with its name only.
func (a *applier) machineOf(c NodeChange) cloud.Instance {
	if i := slices.IndexFunc(a.known, func(k cloud.Instance) bool { return k.Name == c.Name }); i >= 0 {
		return a.known[i]
	}
	return a.listedMachine(c)
}

// listedMachine returns the machine that the cloud listed with the ID of the node change c, or a machine with its name
// only when it listed none.
func (a *applier) listedMachine(c NodeChange) cloud.Instance {
	if in, ok := instanceByID(a.u.listed, c.ID); ok {
		return in
	}
	return cloud.Instance{Name: c.Name}
}

// privateAddress fails when the cloud reports no private address for the machine in.
func privateAddress(in cloud.Instance) error {
	if !in.PrivateIP.IsValid() {
		return fmt.Errorf("node %s: the cloud reports no private address for it yet; run the command again", in.Name)
	}
	return nil
}

// register waits until the node of the machine has registered with the servers, under the machine's name and at its
// private address.
func (a *applier) register(ctx context.Context, in cloud.Instance) error {
	if err := privateAddress(in); err != nil {
		return err
	}
	api, err := a.nomadAPI()
	if err != nil {
		return err
	}
	return a.step(NomadEvent{Action: NomadRegister, Node: in.Name}, func() (NomadEvent, error) {
		waitCtx, cancel := context.WithTimeout(ctx, nomadTimeout)
		defer cancel()
		_, err := nomadops.WaitNode(waitCtx, api, in.Name, in.PrivateIP)
		return NomadEvent{Action: NomadRegister, Node: in.Name}, err
	})
}

// step reports the Nomad step that started describes as started, runs do, and reports it as done with the event that
// do returns, or as failed with its error, which step returns.
func (a *applier) step(started NomadEvent, do func() (NomadEvent, error)) error {
	a.s.progress(Progress{Step: NodeStarted, Nomad: &started})
	done, err := do()
	if err != nil {
		a.s.progress(Progress{Step: NodeFailed, Err: err, Nomad: &started})
		return err
	}
	a.s.progress(Progress{Step: NodeDone, Nomad: &done})
	return nil
}
