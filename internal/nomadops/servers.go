package nomadops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/ingvarch/tent/internal/secret"
)

// Server is one Nomad server: the address of its HTTP API, which errors name it by, and the API of that server.
type Server struct {
	Address string
	API     API
}

// Servers is the API over the servers of a cluster. A call goes to the server that answered last, the first one at the
// start. When that server fails with an error that matches ErrNotReady, the call goes to the next server, until one
// answers or every server was tried once. Any other error is returned as it is and no other server is tried: it is
// permanent, or the caller's context ended.
//
// Writes move on too, since each is safe to repeat after an answer that was lost. Bootstrap is safe to repeat, and an
// IntroToken or CreateToken whose answer was lost leaves a token that nothing uses. A node that MarkIneligible made
// ineligible stays so. A repeated Drain moves the deadline of the drain that runs to the time of the repeat plus the
// deadline. A node that the first server purged is gone on the next one, which counts as purged. ErrGone ends a call
// at once, like any error that is not ErrNotReady.
//
// When no server answers, the error matches ErrNotReady and names each server with its cause, so the waits go on
// polling over Servers and show the last causes when they end. Servers is safe for concurrent use.
type Servers struct {
	servers []Server
	last    *atomic.Int64 // the index of the server that answered last
}

// NewServers returns the API over the servers. It fails when there is no server or a server has no API.
func NewServers(servers ...Server) (*Servers, error) {
	if len(servers) == 0 {
		return nil, errors.New("nomad: no servers")
	}
	for _, s := range servers {
		if s.API == nil {
			return nil, fmt.Errorf("nomad: server %s has no API", s.Address)
		}
	}
	return &Servers{servers: append([]Server(nil), servers...), last: new(atomic.Int64)}, nil
}

// Format prints the servers as their addresses, such as nomadops.Servers(203.0.113.5:4646, 203.0.113.6:4646), whatever
// the verb: fmt would print the tokens that the servers' APIs may hold.
func (s Servers) Format(f fmt.State, _ rune) {
	addrs := make([]string, len(s.servers))
	for i, srv := range s.servers {
		addrs[i] = srv.Address
	}
	_, _ = fmt.Fprintf(f, "nomadops.Servers(%s)", strings.Join(addrs, ", "))
}

// noServerReady is the error of a call that no server answered.
type noServerReady struct{ causes []error } // each cause names its server

func (e *noServerReady) Error() string {
	texts := make([]string, len(e.causes))
	for i, c := range e.causes {
		texts[i] = c.Error()
	}
	return "nomad: no server is ready: " + strings.Join(texts, "; ")
}

// Unwrap returns the causes, which all match ErrNotReady.
func (e *noServerReady) Unwrap() []error { return e.causes }

// try runs call on the servers, from the one that answered last, as the doc of Servers says.
func try[T any](ctx context.Context, s *Servers, call func(API) (T, error)) (T, error) {
	var zero T
	n := len(s.servers)
	start := int(s.last.Load())
	var causes []error
	for i := range n {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		idx := (start + i) % n
		v, err := call(s.servers[idx].API)
		switch {
		case err == nil:
			s.last.Store(int64(idx))
			return v, nil
		case !errors.Is(err, ErrNotReady):
			return zero, err
		}
		causes = append(causes, fmt.Errorf("%s: %w", s.servers[idx].Address, err))
	}
	return zero, &noServerReady{causes: causes}
}

// Last returns the address of the server that answered the last call that returned no error: the server that the next
// call asks first. Before any such call it is the first server's address.
func (s *Servers) Last() string { return s.servers[s.last.Load()].Address }

// Leader returns the RPC address of the cluster's leader, such as 10.0.0.5:4647.
func (s *Servers) Leader(ctx context.Context) (string, error) {
	return try(ctx, s, func(a API) (string, error) { return a.Leader(ctx) })
}

// Bootstrap bootstraps the ACL system with bootstrapSecret as the secret of the management token.
func (s *Servers) Bootstrap(ctx context.Context, bootstrapSecret secret.Secret) error {
	_, err := try(ctx, s, func(a API) (struct{}, error) { return struct{}{}, a.Bootstrap(ctx, bootstrapSecret) })
	return err
}

// IntroToken returns a new client introduction token for the node and pool of req.
func (s *Servers) IntroToken(ctx context.Context, req IntroRequest) (secret.Secret, error) {
	return try(ctx, s, func(a API) (secret.Secret, error) { return a.IntroToken(ctx, req) })
}

// CreateToken makes a management token that expires after req.TTL. After an answer that was lost, the next server
// makes another token; the lost one stays in Nomad until it expires, and nobody holds its secret.
func (s *Servers) CreateToken(ctx context.Context, req TokenRequest) (Token, error) {
	return try(ctx, s, func(a API) (Token, error) { return a.CreateToken(ctx, req) })
}

// Nodes returns the client nodes that registered with the cluster.
func (s *Servers) Nodes(ctx context.Context) ([]Node, error) {
	return try(ctx, s, func(a API) ([]Node, error) { return a.Nodes(ctx) })
}

// Health returns autopilot's view of the servers.
func (s *Servers) Health(ctx context.Context) (Health, error) {
	return try(ctx, s, func(a API) (Health, error) { return a.Health(ctx) })
}

// Peers returns the servers of the Raft configuration.
func (s *Servers) Peers(ctx context.Context) ([]Peer, error) {
	return try(ctx, s, func(a API) ([]Peer, error) { return a.Peers(ctx) })
}

// KeyringReady reports whether the keyring has an active key.
func (s *Servers) KeyringReady(ctx context.Context) (bool, error) {
	return try(ctx, s, func(a API) (bool, error) { return a.KeyringReady(ctx) })
}

// MarkIneligible marks the client node ineligible for new work.
func (s *Servers) MarkIneligible(ctx context.Context, nodeID string) error {
	_, err := try(ctx, s, func(a API) (struct{}, error) { return struct{}{}, a.MarkIneligible(ctx, nodeID) })
	return err
}

// Drain drains the client node.
func (s *Servers) Drain(ctx context.Context, nodeID string, req DrainRequest) error {
	_, err := try(ctx, s, func(a API) (struct{}, error) { return struct{}{}, a.Drain(ctx, nodeID, req) })
	return err
}

// Purge removes the client node from Nomad.
func (s *Servers) Purge(ctx context.Context, nodeID string) error {
	_, err := try(ctx, s, func(a API) (struct{}, error) { return struct{}{}, a.Purge(ctx, nodeID) })
	return err
}
