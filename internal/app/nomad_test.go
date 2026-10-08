package app_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secret"
)

// withAssets gives svc the release files that nodes download, from assetstest, and the development build's tent-node.
// It returns the sites, which record the URLs asked for.
func withAssets(svc *app.Service) *assetstest.Sites {
	sites := assetstest.New()
	svc.Assets = assets.Options{
		Client:    &http.Client{Transport: sites},
		DevURL:    assetstest.DevURL,
		DevSHA256: assetstest.DevSHA256,
		Now:       assetstest.Now,
	}
	return sites
}

// nomadHook wraps every call that a Nomad client of the world makes, as vultrfake.Hook wraps the calls of Vultr. It
// gets the context, the call as the Nomad fake would log it and next, which carries the call out as the world does
// without a hook.
type nomadHook func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error

// nomadCall is a call that reached the Nomad fake, with how many calls had reached the Vultr fake before it.
type nomadCall struct {
	nomadfake.Call
	Cloud int
}

// nomadWorld is the Nomad cluster of one cluster of the test service, which follows a Vultr fake: before each call it
// sets the fake's leader, health, Raft peers and nodes from the cluster's instances there. The cluster has a leader
// once as many servers are ready as its specs give, and its servers are healthy when every listed one is ready. The
// ready servers are the voting peers, named <hostname>.global at <private address>:4647, so the peers, the servers of
// the autopilot report and Health.Voters stay in step; each runs the world's Nomad version, as each node does. Once the
// cluster has a leader, each ready client or combined instance registers at its private address (the first VPC of the
// instance on the Vultr fake), unless it is withheld, with the ID n-<instance id>, and only once in a cluster: a mark,
// a drain or a purge that a test or a run makes stays. The node of a machine that is gone or not ready for downAfter
// reads down from then on, with its drain complete, while the world's last view of the fake still lists it; the view is
// what the last read of the nodes returned, plus the nodes the world registered since, minus those that a client of
// the world purged; any other write since that read is not in it.
// The peers and the autopilot report of the ready servers carry the Raft ID r-<instance id>, the first one leads, and
// the gossip members are the same servers, alive. A cluster whose servers are all gone is a new, unbootstrapped Nomad
// when new ones come. The fake's own methods, such as Fail, LoseResponse and SetDrainReads, are the world's. A test
// changes what the world answers with ChangeServer, DropServer, ChangePeers, ChangeNode and DropNode.
type nomadWorld struct {
	*nomadfake.Fake
	cloud *vultrfake.Fake
	svc   *app.Service
	name  string // the cluster

	mu       sync.Mutex
	hook     nomadHook
	log      []nomadCall
	configs  []nomadops.Config
	withheld map[string]bool // by node name
	// withheldIDs keeps single machines from registering, by instance ID.
	withheldIDs map[string]bool
	noLeader    bool
	// seen holds the ids of the server machines at the last call.
	seen []string
	// unhealthy keeps the servers from being healthy, whatever the instances show.
	unhealthy bool
	// version is the Nomad version that every server and node reports; empty for the one of the stable channel.
	version string
	// What a test changed in the answers, by the name of the machine's node.
	serverEdits  map[string]func(*nomadops.ServerHealth)
	droppedNodes map[string]bool
	nodeEdits    map[string]func(*nomadops.Node)
	peerEdit     func([]nomadops.Peer) []nomadops.Peer
	// droppedServers are the servers that the autopilot report leaves out.
	droppedServers map[string]bool
	// registered holds the instance IDs of the clients that registered in this cluster.
	registered map[string]bool
	// listed is the nodes that the fake lists as the world last saw them, by node ID.
	listed map[string]nomadops.Node
	// unreadySince is when the world first saw a registered client's machine gone or not ready, by instance ID.
	unreadySince map[string]time.Time
	// stableSince is when the world first saw a server machine ready, by instance ID.
	stableSince map[string]time.Time
	downAfter   time.Duration
}

// defaultDownAfter is how long the node of a machine that is gone reads ready: Nomad marks a node down 14 to 19 s
// after its last heartbeat.
const defaultDownAfter = 20 * time.Second

// withNomad gives svc the Nomad of the test cluster prod, which follows f, and returns it.
func withNomad(svc *app.Service, f *vultrfake.Fake) *nomadWorld { return withNomadOf(svc, f, "prod") }

// withNomadOf gives svc the Nomad of the cluster called name, which follows f, and returns it.
func withNomadOf(svc *app.Service, f *vultrfake.Fake, name string) *nomadWorld {
	w := &nomadWorld{
		Fake: nomadfake.New(), cloud: f, svc: svc, name: name, withheld: map[string]bool{}, withheldIDs: map[string]bool{},
		serverEdits:  map[string]func(*nomadops.ServerHealth){},
		droppedNodes: map[string]bool{}, nodeEdits: map[string]func(*nomadops.Node){}, droppedServers: map[string]bool{},
		downAfter: defaultDownAfter,
	}
	w.forgetNodes()
	svc.Nomad = func(cfg nomadops.Config) (nomadops.API, error) {
		w.mu.Lock()
		w.configs = append(w.configs, cfg)
		w.mu.Unlock()
		return &worldClient{w: w, inner: w.Client(cfg), server: cfg.Address}, nil
	}
	return w
}

// pinned returns the Nomad version that every server and node reports: the one that SetVersion made, or else the one
// that the embedded stable channel recommends, which a cluster is pinned to. The caller holds w.mu.
func (w *nomadWorld) pinned() string {
	if w.version != "" {
		return w.version
	}
	ch, err := channels.Load("stable")
	if err != nil {
		return ""
	}
	return ch.Nomad.Recommended
}

// forgetNodes clears the clients that registered, the world's view of the nodes, and the times it noted for machines.
func (w *nomadWorld) forgetNodes() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.registered = map[string]bool{}
	w.listed = map[string]nomadops.Node{}
	w.unreadySince = map[string]time.Time{}
	w.stableSince = map[string]time.Time{}
}

// NewCluster makes the Nomad a new cluster, as the fake does, and has every ready client register again.
func (w *nomadWorld) NewCluster() {
	w.Fake.NewCluster()
	w.forgetNodes()
}

// SetDownAfter sets how long the node of a machine that is gone or not ready reads ready; the default is 20 s.
func (w *nomadWorld) SetDownAfter(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.downAfter = d
}

// SetVersion makes v the Nomad version that every server and node reports from now on.
func (w *nomadWorld) SetVersion(v string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.version = v
}

// ChangeServer changes the entry of the autopilot report for the server machine called name by change, in every
// answer from now on.
func (w *nomadWorld) ChangeServer(name string, change func(*nomadops.ServerHealth)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.serverEdits[name] = change
}

// DropServer leaves the server machine called name out of the autopilot report from now on.
func (w *nomadWorld) DropServer(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.droppedServers[name] = true
}

// ChangePeers makes change the edit of the Raft configuration in every answer from now on.
func (w *nomadWorld) ChangePeers(change func([]nomadops.Peer) []nomadops.Peer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peerEdit = change
}

// ChangeNode changes the node of the machine called name by change, in every answer from now on.
func (w *nomadWorld) ChangeNode(name string, change func(*nomadops.Node)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nodeEdits[name] = change
}

// DropNode leaves the node of the machine called name out of the list of nodes from now on, as if it never registered.
func (w *nomadWorld) DropNode(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.droppedNodes[name] = true
}

// SetHook makes every later call go through hook; nil removes it.
func (w *nomadWorld) SetHook(hook nomadHook) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hook = hook
}

// Withhold keeps the nodes called names from registering.
func (w *nomadWorld) Withhold(names ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		w.withheld[n] = true
	}
}

// WithholdInstance keeps the machines with the instance IDs ids from registering, whatever their names: a machine
// that replaces one of them registers.
func (w *nomadWorld) WithholdInstance(ids ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range ids {
		w.withheldIDs[id] = true
	}
}

// Release ends the Withhold of the nodes called names: they register at the next call.
func (w *nomadWorld) Release(names ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		delete(w.withheld, n)
	}
}

// NoLeader keeps the cluster from electing a leader.
func (w *nomadWorld) NoLeader() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.noLeader = true
}

// Unhealthy keeps the servers of the cluster from being healthy.
func (w *nomadWorld) Unhealthy() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.unhealthy = true
}

// Configs returns the configurations that the service made Nomad clients with, in order.
func (w *nomadWorld) Configs() []nomadops.Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.configs)
}

// Log returns the calls that reached the Nomad fake, in order, with the count of Vultr calls before each.
func (w *nomadWorld) Log() []nomadCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.log)
}

// line returns the call as "nomad <method> <argument> (<the hostname of the server it went to>)", without the
// argument's space when it has none.
func (w *nomadWorld) line(c nomadfake.Call) string {
	return strings.TrimSpace("nomad "+c.Name+" "+c.Arg) + " (" + w.nodeName(c.Server) + ")"
}

// nodeName returns the hostname of the instance whose public address is the host of addr, or addr when none is.
func (w *nomadWorld) nodeName(addr string) string {
	host, _, _ := strings.Cut(addr, ":")
	for _, m := range w.machines() {
		if m.address == host {
			return m.name
		}
	}
	return addr
}

// machine is an instance of the Vultr fake as the world sees it.
type machine struct {
	id, name, address, role string
	private                 netip.Addr // its address in the cluster's VPC; invalid while it has none
	ready                   bool       // Vultr shows it active, running and ok
}

// machines returns the instances of the world's cluster, in creation order.
func (w *nomadWorld) machines() []machine {
	var out []machine
	for _, in := range w.cloud.Instances() {
		if tagOf(in.Tags, cloud.LabelCluster) != w.name {
			continue
		}
		var private netip.Addr
		if vpcs := w.cloud.InstanceVPCs(in.ID); len(vpcs) > 0 {
			private, _ = netip.ParseAddr(vpcs[0].IPAddress)
		}
		out = append(out, machine{
			id: in.ID, name: in.Hostname, address: in.MainIP, role: tagOf(in.Tags, cloud.LabelRole), private: private,
			ready: in.Status == "active" && in.PowerStatus == "running" && in.ServerStatus == "ok",
		})
	}
	return out
}

// serverCount returns how many servers the specs of the cluster give.
func (w *nomadWorld) serverCount() int {
	objs, err := w.svc.Get(context.Background(), w.name, true)
	if err != nil {
		return 0
	}
	n := 0
	for _, g := range objs.NodeGroups {
		if g.Spec.Role.RunsServer() {
			n += g.Spec.Size
		}
	}
	return n
}

// follow sets the fake to what the instances of the Vultr fake show now.
func (w *nomadWorld) follow() {
	var servers, readyServers, clients []machine
	for _, m := range w.machines() {
		switch m.role {
		case "server":
			servers = append(servers, m)
		case "combined":
			servers = append(servers, m)
			clients = append(clients, m)
		case "client":
			clients = append(clients, m)
		}
	}
	for _, m := range servers {
		if m.ready {
			readyServers = append(readyServers, m)
		}
	}
	if !w.keepsAServer(servers) {
		w.NewCluster() // a cluster whose servers are all gone is lost with them
	}
	w.mu.Lock()
	noLeader, unhealthy := w.noLeader, w.unhealthy
	w.mu.Unlock()
	leader := ""
	if n := w.serverCount(); !noLeader && n > 0 && len(readyServers) >= n {
		leader = readyServers[0].address + ":4647"
	}
	w.SetLeader(leader)
	w.SetHealth(nomadops.Health{
		Healthy: !unhealthy && len(servers) > 0 && len(readyServers) == len(servers), Voters: len(readyServers),
		Servers: w.report(readyServers),
	})
	w.SetPeers(raftPeers(readyServers))
	w.SetMembers(membersOf(readyServers))
	if leader == "" {
		return
	}
	w.registerClients(clients)
}

// nodeIDOf returns the ID of the node that the machine with the instance ID id registers.
func nodeIDOf(id string) string { return "n-" + id }

// raftID returns the Raft ID of the server machine m.
func raftID(m machine) string { return "r-" + m.id }

// registerClients registers each ready client that is not withheld and did not register in this cluster, and turns the
// node of each registered client whose machine has been gone or not ready for downAfter to down. A node that the
// world's view no longer lists, or that is down already, stays as it is.
func (w *nomadWorld) registerClients(clients []machine) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	ready := map[string]bool{}
	for _, m := range clients {
		if !m.ready {
			continue
		}
		ready[m.id] = true
		if w.registered[m.id] || w.withheld[m.name] || w.withheldIDs[m.id] {
			continue
		}
		n := nomadops.Node{
			ID: nodeIDOf(m.id), Name: m.name, Status: "ready", Eligible: true, Address: m.private, Version: w.pinned(),
		}
		w.Register(n)
		w.registered[m.id], w.listed[n.ID] = true, n
	}
	for id := range w.registered {
		if ready[id] {
			continue
		}
		since, seen := w.unreadySince[id]
		if !seen {
			w.unreadySince[id] = now
			since = now
		}
		n, listed := w.listed[nodeIDOf(id)]
		if !listed || n.Status == "down" || now.Sub(since) < w.downAfter {
			continue
		}
		n.Status, n.Draining = "down", false
		if n.LastDrain.Status == "draining" {
			n.LastDrain.Status = "complete"
		}
		w.Register(n)
		w.listed[n.ID] = n
	}
}

// report returns autopilot's entries for the ready servers m: alive, healthy and voting, the first one leading. The
// entries come in the reverse order of the machines, as Nomad does not keep one.
func (w *nomadWorld) report(m []machine) []nomadops.ServerHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []nomadops.ServerHealth
	for i := len(m) - 1; i >= 0; i-- {
		out = append(out, nomadops.ServerHealth{
			ID: raftID(m[i]), Name: m[i].name + ".global", Address: netip.AddrPortFrom(m[i].private, 4647), Serf: "alive",
			Healthy: true, Voter: true, Leader: i == 0, Version: w.pinned(), StableSince: w.stableOf(m[i]),
		})
	}
	return out
}

// stableOf returns when the world first saw the server machine m ready, and notes now when it did not. The caller holds
// w.mu.
func (w *nomadWorld) stableOf(m machine) time.Time {
	if _, ok := w.stableSince[m.id]; !ok {
		w.stableSince[m.id] = time.Now()
	}
	return w.stableSince[m.id]
}

// raftPeers returns the servers m as the voting peers of the Raft configuration, at their private addresses, the first
// one leading, as the autopilot report has it.
func raftPeers(m []machine) []nomadops.Peer {
	peers := make([]nomadops.Peer, 0, len(m))
	for i, s := range m {
		peers = append(peers, nomadops.Peer{
			ID: raftID(s), Name: s.name + ".global", Address: netip.AddrPortFrom(s.private, 4647), Voter: true, Leader: i == 0,
		})
	}
	return peers
}

// membersOf returns the servers m as alive members of the gossip pool, at their private addresses.
func membersOf(m []machine) []nomadops.Member {
	members := make([]nomadops.Member, 0, len(m))
	for _, s := range m {
		members = append(members, nomadops.Member{Name: s.name + ".global", Address: s.private, Status: "alive"})
	}
	return members
}

// keepsAServer records the ids of servers and reports whether the machines of the last call that were servers are not
// all gone: it is true when there were none.
func (w *nomadWorld) keepsAServer(servers []machine) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.seen
	w.seen = nil
	for _, m := range servers {
		w.seen = append(w.seen, m.id)
	}
	return len(prev) == 0 || slices.ContainsFunc(w.seen, func(id string) bool { return slices.Contains(prev, id) })
}

// worldClient is the nomadops.API of one client of the world.
type worldClient struct {
	w      *nomadWorld
	inner  nomadops.API
	server string // the address of the server it calls
}

// do carries out the call through the world's hook, and logs what reached the Nomad fake.
func (c *worldClient) do(ctx context.Context, call nomadfake.Call, run func(context.Context) error) error {
	call.Server = c.server
	c.w.follow()
	c.w.mu.Lock()
	hook := c.w.hook
	c.w.mu.Unlock()
	next := func(ctx context.Context) error {
		before := len(c.w.Calls())
		cloudCalls := len(c.w.cloud.Calls())
		err := run(ctx)
		c.w.mu.Lock()
		defer c.w.mu.Unlock()
		for _, call := range c.w.Calls()[before:] {
			c.w.log = append(c.w.log, nomadCall{Call: call, Cloud: cloudCalls})
		}
		return err
	}
	if hook == nil {
		return next(ctx)
	}
	return hook(ctx, call, next)
}

func (c *worldClient) Leader(ctx context.Context) (v string, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Leader"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Leader(ctx)
		return
	})
	if err != nil {
		return "", err
	}
	return v, nil
}

func (c *worldClient) Bootstrap(ctx context.Context, s secret.Secret) error {
	return c.do(ctx, nomadfake.Call{Name: "Bootstrap", Arg: s.String()}, func(ctx context.Context) error {
		return c.inner.Bootstrap(ctx, s)
	})
}

func (c *worldClient) IntroToken(ctx context.Context, req nomadops.IntroRequest) (v secret.Secret, err error) {
	arg := req.NodeName + " " + req.NodePool + " " + req.TTL.String()
	err = c.do(ctx, nomadfake.Call{Name: "IntroToken", Arg: arg}, func(ctx context.Context) (err error) {
		v, err = c.inner.IntroToken(ctx, req)
		return
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (c *worldClient) CreateToken(ctx context.Context, req nomadops.TokenRequest) (v nomadops.Token, err error) {
	arg := req.Name + " " + req.TTL.String()
	err = c.do(ctx, nomadfake.Call{Name: "CreateToken", Arg: arg}, func(ctx context.Context) (err error) {
		v, err = c.inner.CreateToken(ctx, req)
		return
	})
	if err != nil {
		return nomadops.Token{}, err
	}
	return v, nil
}

func (c *worldClient) Nodes(ctx context.Context) (v []nomadops.Node, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Nodes"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Nodes(ctx)
		return
	})
	if err != nil {
		return nil, err
	}
	c.w.view(v)
	return c.w.shapeNodes(v), nil
}

func (c *worldClient) Peers(ctx context.Context) (v []nomadops.Peer, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Peers"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Peers(ctx)
		return
	})
	if err != nil {
		return nil, err
	}
	return c.w.shapePeers(v), nil
}

func (c *worldClient) Health(ctx context.Context) (v nomadops.Health, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Health"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Health(ctx)
		return
	})
	if err != nil {
		return nomadops.Health{}, err
	}
	return c.w.shapeHealth(v), nil
}

func (c *worldClient) KeyringReady(ctx context.Context) (v bool, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "KeyringReady"}, func(ctx context.Context) (err error) {
		v, err = c.inner.KeyringReady(ctx)
		return
	})
	if err != nil {
		return false, err
	}
	return v, nil
}

func (c *worldClient) MarkIneligible(ctx context.Context, nodeID string) error {
	return c.do(ctx, nomadfake.Call{Name: "MarkIneligible", Arg: nodeID}, func(ctx context.Context) error {
		return c.inner.MarkIneligible(ctx, nodeID)
	})
}

func (c *worldClient) Drain(ctx context.Context, nodeID string, req nomadops.DrainRequest) error {
	call := nomadfake.Call{Name: "Drain", Arg: nomadfake.DrainArg(nodeID, req)}
	return c.do(ctx, call, func(ctx context.Context) error { return c.inner.Drain(ctx, nodeID, req) })
}

func (c *worldClient) Purge(ctx context.Context, nodeID string) error {
	err := c.do(ctx, nomadfake.Call{Name: "Purge", Arg: nodeID}, func(ctx context.Context) error {
		return c.inner.Purge(ctx, nodeID)
	})
	if err == nil {
		c.w.mu.Lock()
		defer c.w.mu.Unlock()
		delete(c.w.listed, nodeID)
	}
	return err
}

func (c *worldClient) TransferLeadership(ctx context.Context, raftID string) error {
	call := nomadfake.Call{Name: "TransferLeadership", Arg: raftID}
	return c.do(ctx, call, func(ctx context.Context) error { return c.inner.TransferLeadership(ctx, raftID) })
}

func (c *worldClient) RemovePeer(ctx context.Context, raftID string) error {
	return c.do(ctx, nomadfake.Call{Name: "RemovePeer", Arg: raftID}, func(ctx context.Context) error {
		return c.inner.RemovePeer(ctx, raftID)
	})
}

func (c *worldClient) Members(ctx context.Context) (v []nomadops.Member, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "Members"}, func(ctx context.Context) (err error) {
		v, err = c.inner.Members(ctx)
		return
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (c *worldClient) ForceLeave(ctx context.Context, name string) error {
	return c.do(ctx, nomadfake.Call{Name: "ForceLeave", Arg: name}, func(ctx context.Context) error {
		return c.inner.ForceLeave(ctx, name)
	})
}

func (c *worldClient) SaveSnapshot(ctx context.Context) (v secret.Secret, err error) {
	err = c.do(ctx, nomadfake.Call{Name: "SaveSnapshot"}, func(ctx context.Context) (err error) {
		v, err = c.inner.SaveSnapshot(ctx)
		return
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (c *worldClient) RestoreSnapshot(ctx context.Context, snap secret.Secret) error {
	call := nomadfake.Call{Name: "RestoreSnapshot", Arg: snap.String()}
	return c.do(ctx, call, func(ctx context.Context) error { return c.inner.RestoreSnapshot(ctx, snap) })
}

// view makes nodes, as the fake listed them, the world's view of what the fake lists.
func (w *nomadWorld) view(nodes []nomadops.Node) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listed = make(map[string]nomadops.Node, len(nodes))
	for _, n := range nodes {
		w.listed[n.ID] = n
	}
}

// shapeNodes returns the nodes without those that a test dropped, and with the edits of the test.
func (w *nomadWorld) shapeNodes(nodes []nomadops.Node) []nomadops.Node {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []nomadops.Node
	for _, n := range nodes {
		if w.droppedNodes[n.Name] {
			continue
		}
		if edit := w.nodeEdits[n.Name]; edit != nil {
			edit(&n)
		}
		out = append(out, n)
	}
	return out
}

// shapePeers returns the peers as the test's edit leaves them.
func (w *nomadWorld) shapePeers(peers []nomadops.Peer) []nomadops.Peer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peerEdit == nil {
		return peers
	}
	return w.peerEdit(slices.Clone(peers))
}

// shapeHealth returns the report without the servers that a test dropped, and with the edits of the test.
func (w *nomadWorld) shapeHealth(h nomadops.Health) nomadops.Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	var servers []nomadops.ServerHealth
	for _, sv := range h.Servers {
		name := strings.TrimSuffix(sv.Name, ".global")
		if w.droppedServers[name] {
			continue
		}
		if edit := w.serverEdits[name]; edit != nil {
			edit(&sv)
		}
		servers = append(servers, sv)
	}
	h.Servers = servers
	return h
}

// TestWorldClientPassesTheWritesThroughItsHook checks that MarkIneligible, Drain, Purge, TransferLeadership and
// RemovePeer of a client of the world go through the world's hook with the Call that the Nomad fake logs, reach the
// fake when the hook calls next, and return the hook's error, without reaching the fake, when it does not.
func TestWorldClientPassesTheWritesThroughItsHook(t *testing.T) {
	svc, _, w := newRelease(t)
	errStopped := errors.New("the hook stopped the call")
	var hooked []nomadfake.Call
	var stop bool
	w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		hooked = append(hooked, c)
		if stop {
			return errStopped
		}
		return next(ctx)
	})
	const server = "198.51.100.1:4646"
	api, err := svc.Nomad(nomadops.Config{Address: server})
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	req := nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": "m-1"}}
	calls := map[string]func() error{
		"MarkIneligible":     func() error { return api.MarkIneligible(t.Context(), "n-1") },
		"Drain":              func() error { return api.Drain(t.Context(), "n-1", req) },
		"Purge":              func() error { return api.Purge(t.Context(), "n-1") },
		"TransferLeadership": func() error { return api.TransferLeadership(t.Context(), "raft-1") },
		"RemovePeer":         func() error { return api.RemovePeer(t.Context(), "raft-1") },
		"Members": func() error {
			_, err := api.Members(t.Context())
			return err
		},
		"ForceLeave": func() error { return api.ForceLeave(t.Context(), "s1.global") },
		"SaveSnapshot": func() error {
			_, err := api.SaveSnapshot(t.Context())
			return err
		},
		"RestoreSnapshot": func() error { return api.RestoreSnapshot(t.Context(), secret.Secret("nomadfake snapshot 1")) },
	}
	wantCalls := []nomadfake.Call{
		{Name: "MarkIneligible", Server: server, Arg: "n-1"},
		{Name: "Drain", Server: server, Arg: "n-1 1h0m0s tent_machine=m-1"},
		{Name: "Purge", Server: server, Arg: "n-1"},
		{Name: "TransferLeadership", Server: server, Arg: "raft-1"},
		{Name: "RemovePeer", Server: server, Arg: "raft-1"},
		{Name: "Members", Server: server},
		{Name: "ForceLeave", Server: server, Arg: "s1.global"},
		{Name: "SaveSnapshot", Server: server},
		{Name: "RestoreSnapshot", Server: server, Arg: "[secret, 20 bytes]"},
	}
	names := []string{"MarkIneligible", "Drain", "Purge", "TransferLeadership", "RemovePeer", "Members", "ForceLeave",
		"SaveSnapshot", "RestoreSnapshot"}
	logged := func() []nomadfake.Call {
		var out []nomadfake.Call
		for _, c := range w.Log() {
			out = append(out, c.Call)
		}
		return out
	}

	for _, name := range names {
		// The world has no instances yet, so it has no leader and a call that reaches the fake fails as Nomad's does.
		if err := calls[name](); !errors.Is(err, nomadops.ErrNotReady) {
			t.Errorf("%s error = %v, want the error of the fake, ErrNotReady", name, err)
		}
	}
	if diff := cmp.Diff(wantCalls, hooked); diff != "" {
		t.Errorf("hooked calls (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantCalls, logged()); diff != "" {
		t.Errorf("logged calls (-want +got):\n%s", diff)
	}

	stop = true
	for _, name := range names {
		if err := calls[name](); !errors.Is(err, errStopped) {
			t.Errorf("%s error = %v, want the error of the hook", name, err)
		}
	}
	if diff := cmp.Diff(append(wantCalls, wantCalls...), hooked); diff != "" {
		t.Errorf("hooked calls after the stop (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantCalls, logged()); diff != "" {
		t.Errorf("logged calls after the stop (-want +got):\n%s", diff)
	}
}
