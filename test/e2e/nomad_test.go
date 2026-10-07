package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// maxErrorBody is how much of an error answer an error text holds.
const maxErrorBody = 512

// nomadAPI calls Nomad's HTTP API over mutual TLS with an ACL token.
type nomadAPI struct {
	base   string
	token  string
	client *http.Client
}

// newNomad reads the files that x names and returns a client that trusts the CA alone, presents the client
// certificate, sends the token (without the whitespace around it) in X-Nomad-Token and follows no redirect.
func newNomad(x exportInfo) (*nomadAPI, error) {
	ca, err := os.ReadFile(x.CACert)
	if err != nil {
		return nil, fmt.Errorf("read the CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("the CA file %s holds no certificate", x.CACert)
	}
	pair, err := tls.LoadX509KeyPair(x.ClientCert, x.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load the client key pair: %w", err)
	}
	raw, err := os.ReadFile(x.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read the token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("the token file %s is empty", x.TokenFile)
	}
	return &nomadAPI{
		base:  x.Address,
		token: token,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect would carry the token to another address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{pair},
				ServerName:   x.TLSServerName,
				MinVersion:   tls.VersionTLS12,
			}},
		},
	}, nil
}

// get reads path (with its query) and decodes the JSON answer into out.
func (n *nomadAPI) get(ctx context.Context, path string, out any) error {
	return n.do(ctx, http.MethodGet, path, nil, out)
}

// post sends in as JSON and decodes the JSON answer into out, unless out is nil.
func (n *nomadAPI) post(ctx context.Context, path string, in, out any) error {
	return n.do(ctx, http.MethodPost, path, in, out)
}

// del sends a DELETE and ignores the answer.
func (n *nomadAPI) del(ctx context.Context, path string) error {
	return n.do(ctx, http.MethodDelete, path, nil, nil)
}

// do sends one request and decodes the JSON answer into out, unless out is nil. Errors name the method and the
// path, never the token.
func (n *nomadAPI) do(ctx context.Context, method, path string, in, out any) error {
	resp, label, err := n.send(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode: %w", label, err)
	}
	return nil
}

// send sends one request, with in as its JSON body unless in is nil, and returns an answer with a 2xx status and
// the label that errors about it start with. The caller closes the body.
func (n *nomadAPI) send(ctx context.Context, method, path string, in any) (*http.Response, string, error) {
	label := fmt.Sprintf("nomad: %s %s", method, path)
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, label, fmt.Errorf("%s: encode: %w", label, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.base+path, body)
	if err != nil {
		return nil, label, fmt.Errorf("%s: %w", label, err)
	}
	req.Header.Set("X-Nomad-Token", n.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, label, fmt.Errorf("%s: %w", label, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		text, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, label, fmt.Errorf("%s: %d: %s", label, resp.StatusCode, strings.TrimSpace(string(text)))
	}
	return resp, label, nil
}
