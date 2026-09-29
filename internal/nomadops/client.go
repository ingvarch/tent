// Package nomadops calls the HTTP API of a Nomad cluster's servers over mutual TLS: the leader, the ACL bootstrap,
// client introduction tokens, the client nodes and the autopilot health.
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
	// Nodes returns the client nodes that registered with the cluster.
	Nodes(ctx context.Context) ([]Node, error)
	// Health returns autopilot's view of the servers.
	Health(ctx context.Context) (Health, error)
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
	switch {
	case err != nil:
		return nil, err
	case cfg.Region == "":
		return nil, errors.New("nomad: no region")
	case !x509.NewCertPool().AppendCertsFromPEM(cfg.CA):
		return nil, errors.New("nomad: the CA bundle holds no certificate")
	}
	if _, err := tls.X509KeyPair(cfg.Cert.Cert, cfg.Cert.Key); err != nil {
		return nil, fmt.Errorf("nomad: the operator certificate: %w", err)
	}
	switch {
	case len(cfg.Token) == 0:
		return nil, errors.New("nomad: no ACL token")
	case strings.ContainsFunc(string(cfg.Token), unicode.IsControl):
		return nil, errors.New("nomad: the ACL token has a control character")
	}
	hc := newHTTPClient()
	if err := api.ConfigureTLS(hc, &api.TLSConfig{
		CACertPEM:     cfg.CA,
		ClientCertPEM: cfg.Cert.Cert,
		ClientKeyPEM:  cfg.Cert.Key,
		TLSServerName: "server." + cfg.Region + ".nomad",
	}); err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	// Built by hand, never from api.DefaultConfig, which takes the NOMAD_* variables; NewClient reads them too but
	// uses only the address, which is always set here.
	c, err := api.NewClient(&api.Config{Address: u, Region: cfg.Region, SecretID: string(cfg.Token), HttpClient: hc})
	if err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	return &Client{api: c, url: u, timeout: callTimeout}, nil
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
	host, port, err := net.SplitHostPort(addr)
	n, portErr := strconv.Atoi(port)
	u, urlErr := url.Parse("https://" + addr)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || urlErr != nil || u.Host != addr {
		return "", fmt.Errorf("nomad: address %q is not host:port", addr)
	}
	return u.String(), nil
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
