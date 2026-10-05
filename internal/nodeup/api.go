package nodeup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// apiTimeout bounds one call to a Nomad agent.
const apiTimeout = 5 * time.Second

// apiPort is the Nomad HTTP API's port.
const apiPort = 4646

// localAgent is the node's own Nomad agent, which answers on 127.0.0.1:4646 on every role; the node's certificate
// covers 127.0.0.1 and localhost.
var localAgent = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// apiBodyLimit is the longest answer that the client takes: the status calls answer in bytes.
const apiBodyLimit = 1 << 20

// apiClient is an mTLS client of a Nomad agent's HTTP API: TLS 1.2 or newer, only the cluster's CA, the node's own
// certificate and the TLS name of the call, no redirects and no proxy.
type apiClient struct {
	client *http.Client
}

// newAPIClient reads the CA bundle, the node's certificate and its key from the host and builds the client for the
// TLS name serverName, which the caller picks: verify names localhost; join and refresh-join name
// server.<region>.nomad, the own agent of a server included, as a server's certificate carries that name.
func newAPIClient(h *Host, serverName string) (*apiClient, error) {
	ca, err := h.FS.ReadFile(nodeconfig.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read the CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the CA bundle has no certificates")
	}
	cert, err := h.FS.ReadFile(nodeconfig.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read the node's certificate: %w", err)
	}
	key, err := h.FS.ReadFile(nodeconfig.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the node's key: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("the node's certificate and key: %w", err)
	}
	return &apiClient{client: &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      pool,
				Certificates: []tls.Certificate{pair},
				ServerName:   serverName,
			},
			DialContext: h.DialContext,
		},
		// The status calls answer no redirect worth following.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// close closes the connections that the client keeps open between calls.
func (c *apiClient) close() { c.client.CloseIdleConnections() }

// get calls GET url, within apiTimeout, and returns the status and the body. An answer over apiBodyLimit fails.
func (c *apiClient) get(ctx context.Context, url string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, apiBodyLimit+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > apiBodyLimit {
		return 0, nil, fmt.Errorf("the answer is over %d bytes", apiBodyLimit)
	}
	return res.StatusCode, body, nil
}
