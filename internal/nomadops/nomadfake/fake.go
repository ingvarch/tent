// Package nomadfake is an in-memory Nomad cluster for tests. Its clients implement nomadops.API with the types and
// error classes of nomadops, so code that drives Nomad runs on it as on the real client. A test sets the cluster's
// leader, nodes, peers and health, and makes chosen calls fail or lose their answer after the fake carried them out.
package nomadfake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
//     clients got. Only before the bootstrap does a call fail: Nodes, Health, Peers, KeyringReady, IntroToken and
//     CreateToken fail for good, as Nomad's 403, until a Bootstrap succeeds; Leader and Bootstrap work. Peers fails so,
//     since Nomad answers the Raft configuration to a management token alone.
//   - Its keyring has an active key as soon as the ACL system is bootstrapped, unless SetKeyringDelay holds it back;
//     meanwhile KeyringReady is false and IntroToken fails as Nomad's 500 does.
//
// Faults change the outcome of the next call of a nomadops.API method, named as in the interface, such as Bootstrap:
// see Fail and LoseResponse. A call takes the first fault set for its method, and each fault applies to one call.
type Fake struct {
	mu           sync.Mutex
	leader       string
	bootstrapped secret.Secret // the secret of the management token, once the ACL system is bootstrapped
	nodes        []nomadops.Node
	health       nomadops.Health
	peers        []nomadops.Peer
	keyringReads int             // how many reads of the keyring still find no active key
	tokens       []secret.Secret // the tokens of the clients, in the order Client made them
	issued       []IssuedToken   // the management tokens that CreateToken made, in order
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
// zero Health and a keyring that is ready at once. The log of calls, the clients' tokens, the issued tokens and the
// faults stay.
func (f *Fake) NewCluster() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leader, f.bootstrapped, f.nodes, f.peers, f.health = "", nil, nil, nil, nomadops.Health{}
	f.keyringReads = 0
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

// Register lists the node, in the place of the node of the same name and address when there is one, and after the
// others when there is none. So one name can be listed at two addresses, as Nomad lists a node that went down beside
// its replacement.
func (f *Fake) Register(n nomadops.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.nodes, func(o nomadops.Node) bool {
		return o.Name == n.Name && o.Address == n.Address
	})
	if i >= 0 {
		f.nodes[i] = n
		return
	}
	f.nodes = append(f.nodes, n)
}

// SetHealth makes a copy of h autopilot's view of the servers.
func (f *Fake) SetHealth(h nomadops.Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h.Servers = slices.Clone(h.Servers)
	f.health = h
}

// SetPeers makes a copy of peers the servers of the Raft configuration. It does not change the Health.
func (f *Fake) SetPeers(peers []nomadops.Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = slices.Clone(peers)
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
	// for CreateToken, such as "tent export nomad ana@laptop 24h0m0s"; empty for the others.
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
		nodes = append(make([]nomadops.Node, 0, len(c.f.nodes)), c.f.nodes...)
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

// result returns v, or the zero value and err when err is not nil.
func result[T any](v T, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}
