// Package nomadfake is an in-memory Nomad cluster for tests. Its clients implement nomadops.API with the types and
// error classes of nomadops, so code that drives Nomad runs on it as on the real client. A test sets the cluster's
// leader, nodes, peers, members and health, and makes chosen calls fail or lose their answer after the fake carried
// them out.
package nomadfake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/uuid"
)

// Fake is an in-memory Nomad cluster. It is safe for concurrent use.
//
// A call first checks its argument as the client does, and fails before any request for a bad one. Then a call whose
// context has ended fails with an error that matches the context's error. Every other call reaches the cluster:
// Calls logs it, and it is carried out unless a fault applies. While the cluster has no leader, every call fails with
// an error that matches nomadops.ErrNotReady, as Nomad's servers answer "No cluster leader". Errors that the fake makes
// read "nomadfake: <method>: <why>" and match the class that the client's error would match; a call with an error
// returns no value.
//
// The fake is simpler than Nomad in these ways:
//   - It checks no ACL token: each call succeeds whatever token its client holds. Tokens tells which tokens the
//     clients got. Only before the bootstrap does a call fail: Nodes, Health, Peers, KeyringReady, IntroToken,
//     CreateToken, MarkIneligible, Drain, Purge, TransferLeadership, RemovePeer, Members, ForceLeave, SaveSnapshot
//     and RestoreSnapshot fail for good, as Nomad's 403, until a Bootstrap succeeds; Leader and Bootstrap work. Peers
//     fails so, since Nomad answers the Raft configuration to a management token alone.
//   - MarkIneligible, Drain and Purge change a node as their docs say, and nothing else happens to a node over time:
//     it goes down, registers again or comes back after a purge only when a test says so with Register.
//   - TransferLeadership changes the peers, the leader's address and the Health, and RemovePeer the peers alone, as
//     their docs say, and nothing else happens to a server over time: the cluster re-adds a peer, promotes a server
//     or removes a dead one only when a test says so with SetPeers, SetHealth and SetLeader, which stay independent:
//     a test keeps them in step.
//   - Members lists what SetMembers set, and ForceLeave drops a failed or left member at once and turns an alive one to
//     leaving, which the next read shows and the one after it no longer lists, as their docs say. No member changes
//     over time otherwise: one fails, joins or leaves only when a test says so with SetMembers. Nomad's servers
//     answer these two calls from their own gossip pool even while the cluster has no leader; the fake fails them
//     then, like every call. It has one pool, so every client sees the same members.
//   - A snapshot is the bytes "nomadfake snapshot <n>", and holds nothing of the cluster: RestoreSnapshot records the
//     snapshot for Restored and changes no node, peer, member, health or leader, though Nomad's restore changes the
//     keyring, the ACL tokens, the jobs and the nodes that registered after the snapshot.
//   - Its keyring has an active key as soon as the ACL system is bootstrapped, unless SetKeyringDelay holds it back;
//     meanwhile KeyringReady is false and IntroToken fails as Nomad's 500 does.
//
// Faults change the outcome of the next call of a nomadops.API method, named as in the interface, such as Bootstrap:
// see Fail and LoseResponse. A call takes the first fault set for its method, and each fault applies to one call.
type Fake struct {
	mu           sync.Mutex
	leader       string
	bootstrapped secret.Secret // the secret of the management token, once the ACL system is bootstrapped
	nodes        []fakeNode
	drainReads   int // how many reads of the nodes a new drain stays under way for
	health       nomadops.Health
	peers        []nomadops.Peer
	members      []fakeMember
	keyringReads int             // how many reads of the keyring still find no active key
	tokens       []secret.Secret // the tokens of the clients, in the order Client made them
	issued       []IssuedToken   // the management tokens that CreateToken made, in order
	saves        int             // how many snapshots the cluster saved
	restored     []secret.Secret // the snapshots that the cluster restored, in order
	faults       []fault         // in the order they were set
	calls        []Call
}

// New returns a cluster without a leader, nodes or peers, with the zero Health, and with an ACL system that nobody
// has bootstrapped.
func New() *Fake { return &Fake{} }

// Format prints the fake as nomadfake.Fake, whatever the verb: fmt would print the secrets that its fields hold.
func (f *Fake) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "nomadfake.Fake") }

// Client returns a new client of the cluster, where nomadops.New returns one of a real cluster. It records a copy of
// cfg's token for Tokens, and the address as the Server of the client's calls. It ignores the rest of cfg.
func (f *Fake) Client(cfg nomadops.Config) nomadops.API {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, slices.Clone(cfg.Token))
	return client{f: f, server: cfg.Address}
}

// SetLeader makes addr the RPC address of the cluster's leader, such as 10.0.0.5:4647. An empty addr leaves the
// cluster without a leader.
func (f *Fake) SetLeader(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leader = addr
}

// NewCluster makes the cluster a new one, as New returns it: without a leader, nodes, peers or bootstrap, and with the
// zero Health, a keyring that is ready at once and drains that complete at the first read. The log of calls, the
// clients' tokens, the issued tokens, the count of saved snapshots, the restored snapshots and the faults stay.
func (f *Fake) NewCluster() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leader, f.bootstrapped, f.nodes, f.peers, f.members, f.health = "", nil, nil, nil, nil, nomadops.Health{}
	f.keyringReads, f.drainReads = 0, 0
}

// SetBootstrapped makes the cluster one whose ACL system was bootstrapped with a copy of bootstrapSecret, without a
// call and whether or not the cluster has a leader.
func (f *Fake) SetBootstrapped(bootstrapSecret secret.Secret) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootstrapped = slices.Clone(bootstrapSecret)
}

// SetKeyringDelay makes the next reads calls of KeyringReady after the bootstrap find no active key, and makes
// IntroToken fail as Nomad does until those calls were made. A call before the bootstrap fails and is not counted.
// With 0, the default, the keyring is ready as soon as the ACL system is bootstrapped.
func (f *Fake) SetKeyringDelay(reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyringReads = reads
}

// Register lists a copy of the node in the place of the node with the same ID when n.ID is set, or, when n.ID is
// empty, of the node of the same name and address; when there is no such node, after the others. So one name can be
// listed at two addresses, and with an ID at one address twice, as Nomad lists a node that went down beside its
// replacement. A test that registers a node again sets every field, the drain state too, and the drain that the fake
// ran for the replaced node ends.
func (f *Fake) Register(n nomadops.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n = cloneNode(n)
	i := slices.IndexFunc(f.nodes, func(o fakeNode) bool {
		if n.ID != "" {
			return o.ID == n.ID
		}
		return o.Name == n.Name && o.Address == n.Address
	})
	if i >= 0 {
		f.nodes[i] = fakeNode{Node: n}
		return
	}
	f.nodes = append(f.nodes, fakeNode{Node: n})
}

// SetDrainReads makes each drain that starts after the call stay under way for n reads of the nodes: the node shows as
// draining at those reads and as complete at the next one. A drain also completes at the first read at or after its
// deadline, by time.Now. With 0, the default, a drain is complete at the first read.
func (f *Fake) SetDrainReads(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainReads = n
}

// SetHealth makes a copy of h autopilot's view of the servers.
func (f *Fake) SetHealth(h nomadops.Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h.Servers = slices.Clone(h.Servers)
	f.health = h
}

// SetPeers makes a copy of peers the servers of the Raft configuration. It does not change the Health or the leader's
// address. The Raft IDs and the leader flags are the test's: a call that names a server by its Raft ID finds it here.
func (f *Fake) SetPeers(peers []nomadops.Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = slices.Clone(peers)
}

// SetMembers makes a copy of members the servers of the gossip pool, in that order. Every member stays as it is until
// a ForceLeave changes it.
func (f *Fake) SetMembers(members []nomadops.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = make([]fakeMember, len(members))
	for i, m := range members {
		f.members[i] = fakeMember{Member: m}
	}
}

// Tokens returns copies of the tokens that Client got, in order.
func (f *Fake) Tokens() []secret.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]secret.Secret, len(f.tokens))
	for i, t := range f.tokens {
		out[i] = slices.Clone(t)
	}
	return out
}

// IssuedToken is a management token that CreateToken made. It holds no secret.
type IssuedToken struct {
	Name     string
	TTL      time.Duration
	Accessor string
}

// Issued returns the management tokens that CreateToken made, in order, also those whose answer was lost.
func (f *Fake) Issued() []IssuedToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.issued)
}

// Call is a call that reached the cluster.
type Call struct {
	Name string // the nomadops.API method, such as Bootstrap
	// Server is the Address of the Config of the client that made the call.
	Server string
	// Arg is the call's argument without a secret: the size of the secret for Bootstrap, such as [secret, 36 bytes];
	// the node's name, the pool and the TTL for IntroToken, such as "prod-workers-1 default 30m0s"; the name and the TTL
	// for CreateToken, such as "tent export nomad ana@laptop 24h0m0s"; the node ID for MarkIneligible and Purge; DrainArg
	// for Drain; the Raft ID for TransferLeadership and RemovePeer; the member's name for ForceLeave; the size of the
	// snapshot for RestoreSnapshot, such as [secret, 20 bytes]; empty for the others.
	Arg string
}

// Calls returns the calls that reached the cluster, in order, whatever their outcome. Calls with a bad argument or an
// ended context are not among them, since the client sends no request for them.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Why calls fail.
var (
	errNoLeader = errors.New("no leader")
	errLost     = errors.New("the answer was lost")
	// errKeyring is the 500 of Nomad for an intro token that is asked for before the keyring has a key.
	errKeyring = notReadyError("500: failed to sign node introduction identity claims: keyring has not been " +
		"initialized yet")
	// errDenied is the 403 of Nomad for a call that needs an ACL token before any exists.
	errDenied = errors.New("permission denied")
)

// notReadyError is a cause that matches nomadops.ErrNotReady, as the 5xx answers of Nomad do.
type notReadyError string

func (e notReadyError) Error() string { return string(e) }

// Is makes errors.Is hold for nomadops.ErrNotReady.
func (notReadyError) Is(target error) bool { return target == nomadops.ErrNotReady }

// callError is the error of a call that the fake fails.
type callError struct {
	name     string // the API method
	cause    error
	notReady bool // the error matches nomadops.ErrNotReady
}

// Error prints "nomadfake: <method>: <cause>".
func (e *callError) Error() string { return "nomadfake: " + e.name + ": " + e.cause.Error() }

// Is makes errors.Is hold for nomadops.ErrNotReady when the error is of that class.
func (e *callError) Is(target error) bool { return e.notReady && target == nomadops.ErrNotReady }

// Unwrap returns the cause, such as nomadops.ErrBootstrapMismatch or the error of the caller's context.
func (e *callError) Unwrap() error { return e.cause }

// call carries out the call of the API method name, which Calls logs with server and arg, as the client would send
// it. When ctx has ended it does nothing. Otherwise it logs the call and takes the first fault for name. A failure
// fault returns its error without doing anything. Without a leader the call fails with ErrNotReady; with one, do runs
// under the lock and its error becomes the call's. A lost answer replaces the outcome with ErrNotReady.
func (f *Fake) call(ctx context.Context, server, name, arg string, do func() error) error {
	if err := ctx.Err(); err != nil {
		return &callError{name: name, cause: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Name: name, Server: server, Arg: arg})
	ft, faulted := f.takeFault(name)
	if faulted && ft.err != nil {
		return ft.err
	}
	var err error
	if f.leader == "" {
		err = &callError{name: name, cause: errNoLeader, notReady: true}
	} else if cause := do(); cause != nil {
		err = &callError{name: name, cause: cause}
	}
	if faulted {
		return &callError{name: name, cause: errLost, notReady: true}
	}
	return err
}

// client is the nomadops.API of a client of the cluster.
type client struct {
	f      *Fake
	server string // the Address of the Config that made the client
}

// Format prints the client as nomadfake.Client, whatever the verb: fmt would print the secrets of the fake for %s and
// %q.
func (c client) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "nomadfake.Client") }

func (c client) Leader(ctx context.Context) (string, error) {
	var leader string
	err := c.f.call(ctx, c.server, "Leader", "", func() error {
		leader = c.f.leader
		return nil
	})
	return result(leader, err)
}

// Bootstrap stores a copy of bootstrapSecret as the secret of the management token on the first call. A later call
// succeeds with the same secret and fails with nomadops.ErrBootstrapMismatch with another one. It refuses a secret that
// is not a lower-case UUID of version 4 before any call, as the client does.
func (c client) Bootstrap(ctx context.Context, bootstrapSecret secret.Secret) error {
	if err := pki.CheckBootstrapSecret(bootstrapSecret); err != nil {
		return &callError{name: "Bootstrap", cause: err}
	}
	return c.f.call(ctx, c.server, "Bootstrap", bootstrapSecret.String(), func() error {
		switch {
		case c.f.bootstrapped == nil:
			c.f.bootstrapped = slices.Clone(bootstrapSecret)
		case !bytes.Equal(c.f.bootstrapped, bootstrapSecret):
			return nomadops.ErrBootstrapMismatch
		}
		return nil
	})
}

// IntroToken returns a JWT without a signature whose claims are Nomad's {"nomad_node_name": <node>,
// "nomad_node_pool": <pool>}, so the same node and pool always get the same token. While SetKeyringDelay holds back the
// keyring, it fails with Nomad's 500 and a cause that matches nomadops.ErrNotReady.
func (c client) IntroToken(ctx context.Context, req nomadops.IntroRequest) (secret.Secret, error) {
	if err := req.Check(); err != nil {
		return nil, &callError{name: "IntroToken", cause: err}
	}
	var jwt secret.Secret
	err := c.f.call(ctx, c.server, "IntroToken", req.NodeName+" "+req.NodePool+" "+req.TTL.String(), func() error {
		switch {
		case c.f.bootstrapped == nil:
			return errDenied
		case c.f.keyringReads > 0:
			return errKeyring
		}
		claims, err := json.Marshal(map[string]string{"nomad_node_name": req.NodeName, "nomad_node_pool": req.NodePool})
		if err != nil {
			return err
		}
		enc := base64.RawURLEncoding
		jwt = secret.Secret(enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
			enc.EncodeToString(claims) + ".")
		return nil
	})
	return result(jwt, err)
}

// Nomad's limits for the TTL of a token, its defaults.
const (
	minTokenTTL = time.Minute
	maxTokenTTL = 24 * time.Hour
)

// tokenInvalid starts Nomad's 400 for a token that it refuses, on one line as the client prints it.
const tokenInvalid = "token 0 invalid: 1 error occurred: * "

// CreateToken makes a token with a new accessor and secret, which ends at time.Now plus req.TTL, and lists it for
// Issued without its secret. It refuses a TTL below a minute or above a day with Nomad's text, before it makes
// anything.
func (c client) CreateToken(ctx context.Context, req nomadops.TokenRequest) (nomadops.Token, error) {
	if err := req.Check(); err != nil {
		return nomadops.Token{}, &callError{name: "CreateToken", cause: err}
	}
	var tok nomadops.Token
	err := c.f.call(ctx, c.server, "CreateToken", req.Name+" "+req.TTL.String(), func() error {
		switch {
		case c.f.bootstrapped == nil:
			return errDenied
		case req.TTL < minTokenTTL:
			return fmt.Errorf(tokenInvalid+"expiration time cannot be less than %s in the future (was %s)",
				minTokenTTL, req.TTL)
		case req.TTL > maxTokenTTL:
			return fmt.Errorf(tokenInvalid+"expiration time cannot be more than %s in the future (was %s)",
				maxTokenTTL, req.TTL)
		}
		tok = nomadops.Token{Accessor: uuid.New(), Secret: secret.Secret(uuid.New()), Expires: time.Now().Add(req.TTL)}
		c.f.issued = append(c.f.issued, IssuedToken{Name: req.Name, TTL: req.TTL, Accessor: tok.Accessor})
		return nil
	})
	return result(tok, err)
}

func (c client) Nodes(ctx context.Context) ([]nomadops.Node, error) {
	var nodes []nomadops.Node
	err := c.f.call(ctx, c.server, "Nodes", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		now := time.Now()
		nodes = make([]nomadops.Node, len(c.f.nodes))
		for i := range c.f.nodes {
			c.f.nodes[i].advanceDrain(now)
			nodes[i] = cloneNode(c.f.nodes[i].Node)
		}
		return nil
	})
	return result(nodes, err)
}

func (c client) Health(ctx context.Context) (nomadops.Health, error) {
	var h nomadops.Health
	err := c.f.call(ctx, c.server, "Health", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		h = c.f.health
		h.Servers = slices.Clone(h.Servers)
		return nil
	})
	return result(h, err)
}

func (c client) Peers(ctx context.Context) ([]nomadops.Peer, error) {
	var peers []nomadops.Peer
	err := c.f.call(ctx, c.server, "Peers", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		peers = append(make([]nomadops.Peer, 0, len(c.f.peers)), c.f.peers...)
		return nil
	})
	return result(peers, err)
}

// KeyringReady is true once the reads that SetKeyringDelay asked for were made after the bootstrap. A read that finds
// no active key counts down those reads.
func (c client) KeyringReady(ctx context.Context) (bool, error) {
	var ready bool
	err := c.f.call(ctx, c.server, "KeyringReady", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		if c.f.keyringReads > 0 {
			c.f.keyringReads--
			return nil
		}
		ready = true
		return nil
	})
	return result(ready, err)
}

// fakeNode is a node that the cluster lists, with the drain that the fake runs for it.
type fakeNode struct {
	nomadops.Node
	drain *drain // nil when no drain is under way
}

// drain is a drain that the fake runs for a node.
type drain struct {
	deadline time.Time // the drain completes at the first read at or after it
	reads    int       // how many more reads show the node as draining
}

// advanceDrain is a read of the node at now: a drain that is under way stays so for one read fewer, or completes when
// its reads are used up or its deadline has come. Completion keeps the node ineligible and the meta of the drain.
func (n *fakeNode) advanceDrain(now time.Time) {
	if n.drain == nil {
		return
	}
	if n.drain.reads > 0 && now.Before(n.drain.deadline) {
		n.drain.reads--
		return
	}
	n.Draining, n.LastDrain.Status, n.drain = false, "complete", nil
}

// cloneNode returns n with a copy of the meta of its last drain.
func cloneNode(n nomadops.Node) nomadops.Node {
	n.LastDrain.Meta = maps.Clone(n.LastDrain.Meta)
	return n
}

// result returns v, or the zero value and err when err is not nil.
func result[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// goneError is a cause that matches nomadops.ErrGone, as Nomad's answer for a node or a Raft peer that is not there
// does.
type goneError string

func (e goneError) Error() string { return string(e) }

// Is makes errors.Is hold for nomadops.ErrGone.
func (goneError) Is(target error) bool { return target == nomadops.ErrGone }

// errNodeGone is Nomad's answer for a node that is not in the cluster.
const errNodeGone = goneError("node not found")

// DrainArg is the Arg that Calls logs for a Drain: the node ID, the deadline and the meta as k=v pairs sorted by key,
// such as "n-1 1h0m0s tent_machine=m-1".
func DrainArg(nodeID string, req nomadops.DrainRequest) string {
	parts := []string{nodeID, req.Deadline.String()}
	for _, k := range slices.Sorted(maps.Keys(req.Meta)) {
		parts = append(parts, k+"="+req.Meta[k])
	}
	return strings.Join(parts, " ")
}

// node returns the node with the ID, or errDenied before the bootstrap, or errNodeGone when there is none. The caller
// holds the lock.
func (f *Fake) node(id string) (*fakeNode, error) {
	if f.bootstrapped == nil {
		return nil, errDenied
	}
	i := slices.IndexFunc(f.nodes, func(n fakeNode) bool { return n.ID == id })
	if i < 0 {
		return nil, errNodeGone
	}
	return &f.nodes[i], nil
}

// MarkIneligible makes the node ineligible. It fails with an error that matches nomadops.ErrGone when no node has the
// ID, and refuses an ID that the client refuses before any call.
func (c client) MarkIneligible(ctx context.Context, nodeID string) error {
	if err := nomadops.CheckNodeID(nodeID); err != nil {
		return &callError{name: "MarkIneligible", cause: err}
	}
	return c.f.call(ctx, c.server, "MarkIneligible", nodeID, func() error {
		n, err := c.f.node(nodeID)
		if err != nil {
			return err
		}
		n.Eligible = false
		return nil
	})
}

// Drain starts the drain of the node: it turns ineligible and draining, and its last drain is a copy of req's meta.
// The drain completes as SetDrainReads says. A node that is down has a complete drain at once. A drain of a node that
// drains moves the deadline and the meta and keeps the count of reads. It fails with an error that matches
// nomadops.ErrGone when no node has the ID, and refuses an ID or a request that the client refuses before any call.
func (c client) Drain(ctx context.Context, nodeID string, req nomadops.DrainRequest) error {
	if err := nomadops.CheckNodeID(nodeID); err != nil {
		return &callError{name: "Drain", cause: err}
	}
	if err := req.Check(); err != nil {
		return &callError{name: "Drain", cause: err}
	}
	return c.f.call(ctx, c.server, "Drain", DrainArg(nodeID, req), func() error {
		n, err := c.f.node(nodeID)
		if err != nil {
			return err
		}
		n.Eligible = false
		n.LastDrain = nomadops.LastDrain{Meta: maps.Clone(req.Meta)}
		if n.Status == "down" {
			n.Draining, n.LastDrain.Status = false, "complete"
			return nil
		}
		reads := c.f.drainReads
		if n.drain != nil {
			reads = n.drain.reads
		}
		n.Draining, n.LastDrain.Status = true, "draining"
		n.drain = &drain{deadline: time.Now().Add(req.Deadline), reads: reads}
		return nil
	})
}

// Purge removes the node; a node that is not there counts as purged. The node does not come back unless Register lists
// it again. It refuses an ID that the client refuses before any call.
func (c client) Purge(ctx context.Context, nodeID string) error {
	if err := nomadops.CheckNodeID(nodeID); err != nil {
		return &callError{name: "Purge", cause: err}
	}
	return c.f.call(ctx, c.server, "Purge", nodeID, func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		c.f.nodes = slices.DeleteFunc(c.f.nodes, func(n fakeNode) bool { return n.ID == nodeID })
		return nil
	})
}

// peerIndex returns the index of the peer with the Raft ID, or -1. The caller holds the lock.
func (f *Fake) peerIndex(raftID string) int {
	return slices.IndexFunc(f.peers, func(p nomadops.Peer) bool { return p.ID == raftID })
}

// notInRaft is Nomad's answer for a Raft ID that is not in the Raft configuration.
func notInRaft(raftID string) goneError {
	return goneError(fmt.Sprintf("id %q was not found in the Raft configuration", raftID))
}

// moveLeadership makes the peer at index i lead, as a transfer does: the leader flags of the peers and of the report,
// the leader's address, and the stable time of every server in the report, which a new leader resets. It does nothing
// when the peer leads already. A peer that does not vote cannot lead: the first voter other than the leader takes the
// leadership instead, and when there is none the leader stays. The caller holds the lock.
func (f *Fake) moveLeadership(i int) {
	if f.peers[i].Leader {
		return
	}
	if !f.peers[i].Voter {
		i = slices.IndexFunc(f.peers, func(p nomadops.Peer) bool { return p.Voter && !p.Leader })
		if i < 0 {
			return
		}
	}
	for k := range f.peers {
		f.peers[k].Leader = k == i
	}
	f.leader = f.peers[i].Address.String()
	now := time.Now()
	for k := range f.health.Servers {
		f.health.Servers[k].Leader = f.health.Servers[k].ID == f.peers[i].ID
		f.health.Servers[k].StableSince = now
	}
}

// TransferLeadership hands the leadership to the voter with the Raft ID, or to the first other voter when the server
// does not vote, as moveLeadership says; to the leader it changes nothing. It fails with an error that matches
// nomadops.ErrGone when no peer has the ID, and refuses an ID that the client refuses before any call.
func (c client) TransferLeadership(ctx context.Context, raftID string) error {
	if err := nomadops.CheckRaftID(raftID); err != nil {
		return &callError{name: "TransferLeadership", cause: err}
	}
	return c.f.call(ctx, c.server, "TransferLeadership", raftID, func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		i := c.f.peerIndex(raftID)
		if i < 0 {
			return notInRaft(raftID)
		}
		c.f.moveLeadership(i)
		return nil
	})
}

// RemovePeer removes the peer from the Raft configuration and leaves the Health as it is; a peer that is not there
// counts as removed. It fails with a permanent error for the leader's own peer, which tent never asks and Nomad's
// answer to which is not known, and refuses an ID that the client refuses before any call.
func (c client) RemovePeer(ctx context.Context, raftID string) error {
	if err := nomadops.CheckRaftID(raftID); err != nil {
		return &callError{name: "RemovePeer", cause: err}
	}
	return c.f.call(ctx, c.server, "RemovePeer", raftID, func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		i := c.f.peerIndex(raftID)
		switch {
		case i < 0:
			return nil
		case c.f.peers[i].Leader:
			return fmt.Errorf("the peer %s leads the cluster", raftID)
		}
		c.f.peers = slices.Delete(c.f.peers, i, i+1)
		return nil
	})
}

// fakeMember is a member of the gossip pool, with what the fake does to it after a ForceLeave.
type fakeMember struct {
	nomadops.Member
	forced bool // a ForceLeave turned the member to leaving
	shown  bool // a read of Members showed it so
}

// Members returns a copy of the gossip pool. A member that a ForceLeave turned to leaving shows so at the first read
// and is gone at the next one.
func (c client) Members(ctx context.Context) ([]nomadops.Member, error) {
	var members []nomadops.Member
	err := c.f.call(ctx, c.server, "Members", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		c.f.members = slices.DeleteFunc(c.f.members, func(m fakeMember) bool { return m.shown })
		members = make([]nomadops.Member, len(c.f.members))
		for i := range c.f.members {
			c.f.members[i].shown = c.f.members[i].forced
			members[i] = c.f.members[i].Member
		}
		return nil
	})
	return result(members, err)
}

// ForceLeave drops a member that failed or left at once, turns an alive member to leaving, as Members says, and leaves
// a member that is leaving as it is. A name that is not in the pool succeeds, as Nomad answers 200 to any name. It
// refuses a name that the client refuses before any call.
func (c client) ForceLeave(ctx context.Context, name string) error {
	if err := nomadops.CheckMemberName(name); err != nil {
		return &callError{name: "ForceLeave", cause: err}
	}
	return c.f.call(ctx, c.server, "ForceLeave", name, func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		i := slices.IndexFunc(c.f.members, func(m fakeMember) bool { return m.Name == name })
		if i < 0 {
			return nil
		}
		switch m := &c.f.members[i]; m.Status {
		case "failed", "left":
			c.f.members = slices.Delete(c.f.members, i, i+1)
		case "alive":
			m.Status, m.forced = "leaving", true
		}
		return nil
	})
}

// snapshotPrefix starts every snapshot that the fake saves, and the only bytes that it restores.
const snapshotPrefix = "nomadfake snapshot "

// errBadSnapshot is Nomad's 500 for bytes that are no snapshot.
const errBadSnapshot = notReadyError("500: failed to restore from snapshot: failed to decompress snapshot: " +
	"gzip: invalid header")

// SaveSnapshot returns "nomadfake snapshot <n>", where n counts the saves that the cluster carried out, from 1, whoever
// asked; a save whose answer was lost counts, and one that fails before the bootstrap does not.
func (c client) SaveSnapshot(ctx context.Context) (secret.Secret, error) {
	var snap secret.Secret
	err := c.f.call(ctx, c.server, "SaveSnapshot", "", func() error {
		if c.f.bootstrapped == nil {
			return errDenied
		}
		c.f.saves++
		snap = secret.Secret(snapshotPrefix + strconv.Itoa(c.f.saves))
		return nil
	})
	return result(snap, err)
}

// RestoreSnapshot records a copy of the snapshot for Restored and changes nothing else in the cluster. Bytes that do
// not start with "nomadfake snapshot " fail as Nomad's 500 does for a file that is no snapshot, which matches
// nomadops.ErrNotReady. It refuses an empty snapshot before any call, as the client does.
func (c client) RestoreSnapshot(ctx context.Context, snap secret.Secret) error {
	if err := nomadops.CheckSnapshot(snap); err != nil {
		return &callError{name: "RestoreSnapshot", cause: err}
	}
	return c.f.call(ctx, c.server, "RestoreSnapshot", snap.String(), func() error {
		switch {
		case c.f.bootstrapped == nil:
			return errDenied
		case !bytes.HasPrefix(snap, []byte(snapshotPrefix)):
			return errBadSnapshot
		}
		c.f.restored = append(c.f.restored, slices.Clone(snap))
		return nil
	})
}

// Restored returns copies of the snapshots that the cluster restored, in order, also those whose answer was lost.
func (f *Fake) Restored() []secret.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]secret.Secret, len(f.restored))
	for i, snap := range f.restored {
		out[i] = slices.Clone(snap)
	}
	return out
}
