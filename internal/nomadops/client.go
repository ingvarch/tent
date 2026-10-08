// Package nomadops calls the HTTP API of a Nomad cluster's servers over mutual TLS: the leader, the ACL bootstrap,
// client introduction tokens, management tokens that expire, the client nodes with their eligibility, drains and
// purges, the autopilot health, the Raft peers with leadership transfers and removals, and the keyring.
// It also makes the reverse proxy that serves a cluster's API on a local port, with the mutual TLS and the token added.
package nomadops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/nomad/api"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
)

// callTimeout bounds each call, so that a server that stops answering cannot hold the caller.
const callTimeout = 30 * time.Second

// Config is how a client reaches one Nomad server and who it is there.
type Config struct {
	// Address is the host and port of the server's HTTP API, such as 203.0.113.5:4646. The client always uses https.
	Address string
	// Region is the Nomad region. The server must show a certificate for server.<region>.nomad.
	Region string
	// CA is the cluster's CA bundle, PEM, which the server's certificate must chain to.
	CA []byte
	// Cert is the operator certificate, for cli.<region>.nomad, and its key. The client shows it to the server.
	Cert pki.Certificate
	// Token is the secret of the ACL token that the calls send.
	Token secret.Secret
}

// API is the part of the Nomad API that tent uses, on one server. A call that fails with an error that matches
// ErrNotReady may succeed later, on this server or on another one.
type API interface {
	// Leader returns the RPC address of the cluster's leader, such as 10.0.0.5:4647.
	Leader(ctx context.Context) (string, error)
	// Bootstrap bootstraps the ACL system with bootstrapSecret as the secret of the management token. It is safe to
	// repeat, and fails with ErrBootstrapMismatch when the ACL system was bootstrapped with another secret.
	Bootstrap(ctx context.Context, bootstrapSecret secret.Secret) error
	// IntroToken returns a new client introduction token for the node and pool of req.
	IntroToken(ctx context.Context, req IntroRequest) (secret.Secret, error)
	// CreateToken makes a management token that expires after req.TTL.
	CreateToken(ctx context.Context, req TokenRequest) (Token, error)
	// Nodes returns the client nodes that registered with the cluster.
	Nodes(ctx context.Context) ([]Node, error)
	// Health returns autopilot's view of the servers.
	Health(ctx context.Context) (Health, error)
	// Peers returns the servers of the Raft configuration.
	Peers(ctx context.Context) ([]Peer, error)
	// KeyringReady reports whether the keyring has an active key, which Nomad signs intro tokens with.
	KeyringReady(ctx context.Context) (bool, error)
	// MarkIneligible marks the client node ineligible for new work. A node that is ineligible stays so.
	MarkIneligible(ctx context.Context, nodeID string) error
	// Drain drains the client node: Nomad marks it ineligible, moves its allocations, those of system jobs too, and
	// stops the ones that remain at the deadline.
	Drain(ctx context.Context, nodeID string, req DrainRequest) error
	// Purge removes the client node from Nomad. A node that is not there counts as purged.
	Purge(ctx context.Context, nodeID string) error
	// TransferLeadership asks the leader to hand the leadership to the server with the Raft ID.
	TransferLeadership(ctx context.Context, raftID string) error
	// RemovePeer removes the server with the Raft ID from the Raft configuration. A peer that is not there counts as
	// removed.
	RemovePeer(ctx context.Context, raftID string) error
}

// Client is the API over the HTTP API of one Nomad server. Each call has at most 30 seconds, and none is retried.
// Make one Client per server and reuse it: its idle connections stay open for 90 seconds, and a Nomad server takes
// at most 100 HTTP connections from one address.
type Client struct {
	api     *api.Client
	url     string // the URL of the server
	timeout time.Duration
}

// New returns a client of the server that cfg names. The NOMAD_* variables of the Nomad CLI have no effect on it. Like
// tent's other HTTPS clients, it goes through the proxy that HTTPS_PROXY and NO_PROXY name; TLS stays end to end. It
// never follows a redirect, so that the token goes to the server alone. It fails when the address is not host:port,
// the region is empty, the CA bundle holds no certificate, the certificate and its key do not make a pair, or the
// token is empty or has a control character. Its errors never show the key or the token.
func New(cfg Config) (*Client, error) {
	u, err := serverURL(cfg.Address)
	if err != nil {
		return nil, err
	}
	if err := checkCluster(cfg.Region, cfg.CA, cfg.Cert, cfg.Token); err != nil {
		return nil, err
	}
	hc, err := clusterHTTPClient(cfg.Region, cfg.CA, cfg.Cert)
	if err != nil {
		return nil, err
	}
	// Built by hand, never from api.DefaultConfig, which takes the NOMAD_* variables; NewClient reads them too but
	// uses only the address, which is always set here.
	c, err := api.NewClient(&api.Config{Address: u, Region: cfg.Region, SecretID: string(cfg.Token), HttpClient: hc})
	if err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	return &Client{api: c, url: u, timeout: callTimeout}, nil
}

// checkCluster fails unless the region, the CA bundle, the operator certificate and the token are fit for a client of
// the cluster's servers. Its errors never show the key or the token.
func checkCluster(region string, ca []byte, cert pki.Certificate, token secret.Secret) error {
	switch {
	case region == "":
		return errors.New("nomad: no region")
	case !x509.NewCertPool().AppendCertsFromPEM(ca):
		return errors.New("nomad: the CA bundle holds no certificate")
	}
	if _, err := tls.X509KeyPair(cert.Cert, cert.Key); err != nil {
		return fmt.Errorf("nomad: the operator certificate: %w", err)
	}
	switch {
	case len(token) == 0:
		return errors.New("nomad: no ACL token")
	case strings.ContainsFunc(string(token), unicode.IsControl):
		return errors.New("nomad: the ACL token has a control character")
	}
	return nil
}

// clusterHTTPClient returns the HTTP client that speaks to the cluster's servers: the cluster's CA is the only root,
// the operator certificate is shown to the server, and the server must show a certificate for server.<region>.nomad.
func clusterHTTPClient(region string, ca []byte, cert pki.Certificate) (*http.Client, error) {
	hc := newHTTPClient()
	if err := api.ConfigureTLS(hc, &api.TLSConfig{
		CACertPEM:     ca,
		ClientCertPEM: cert.Cert,
		ClientKeyPEM:  cert.Key,
		TLSServerName: "server." + region + ".nomad",
	}); err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	return hc, nil
}

// newHTTPClient returns the HTTP client that the Nomad API module would make for itself, but one that never follows
// a redirect.
func newHTTPClient() *http.Client {
	tr := cleanhttp.DefaultPooledTransport()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	tr.ForceAttemptHTTP2 = false
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Format prints the client as the URL of its server, such as nomadops.Client(https://203.0.113.5:4646), whatever the
// verb: fmt would print the token that its fields hold for %s and %q.
func (c Client) Format(f fmt.State, _ rune) { _, _ = fmt.Fprintf(f, "nomadops.Client(%s)", c.url) }

// serverURL returns the https URL of a server's address, which must be host:port.
func serverURL(addr string) (string, error) {
	u, ok := hostPortURL(addr)
	if !ok {
		return "", fmt.Errorf("nomad: address %q is not host:port", addr)
	}
	return u.String(), nil
}

// hostPortURL returns the https URL of addr, which must be host:port with a host and a port from 1 to 65535.
func hostPortURL(addr string) (*url.URL, bool) {
	host, port, err := net.SplitHostPort(addr)
	n, portErr := strconv.Atoi(port)
	u, urlErr := url.Parse("https://" + addr)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || urlErr != nil || u.Host != addr {
		return nil, false
	}
	return u, true
}

// call runs do with a context that ends after the client's timeout, and turns its failure into a *callError.
func (c *Client) call(ctx context.Context, method, path string, do func(context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err := do(callCtx)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return &callError{method: method, path: path, cause: ctx.Err()}
	case callCtx.Err() != nil:
		return &callError{method: method, path: path, cause: fmt.Errorf("no answer within %s", c.timeout),
			notReady: true}
	}
	return newCallError(method, path, err)
}

// query returns the options of a read in the context.
func query(ctx context.Context) *api.QueryOptions { return (&api.QueryOptions{}).WithContext(ctx) }

// write returns the options of a write in the context.
func write(ctx context.Context) *api.WriteOptions { return (&api.WriteOptions{}).WithContext(ctx) }

const leaderPath = "/v1/status/leader"

// Leader returns the RPC address of the cluster's leader, such as 10.0.0.5:4647. A server that knows no leader fails
// with ErrNotReady.
func (c *Client) Leader(ctx context.Context) (string, error) {
	var leader string
	err := c.call(ctx, http.MethodGet, leaderPath, func(ctx context.Context) error {
		// Not Status().Leader, which takes no QueryOptions and so no context.
		_, err := c.api.Raw().Query(leaderPath, &leader, query(ctx))
		return err
	})
	switch {
	case err != nil:
		return "", err
	case leader == "":
		return "", &callError{method: http.MethodGet, path: leaderPath, cause: errNoLeader, notReady: true}
	}
	return leader, nil
}
