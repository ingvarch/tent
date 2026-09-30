// Package vultr reads Vultr's metadata service, which describes the machine that tent-node runs on.
package vultr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/retry"
)

const (
	documentURL = "http://169.254.169.254/v1.json" // the metadata document, which describes the machine in JSON

	tryTimeout = 5 * time.Second // the longest a try takes, the document's body included
	retryWait  = time.Second     // the wait after a failed try
)

// Environment is Vultr's metadata service.
type Environment struct {
	client *http.Client
	try    time.Duration             // the longest a try takes
	wait   func(n int) time.Duration // the wait after failed try n, counting from 1
}

var _ env.Environment = (*Environment)(nil)

// New returns the metadata service of the Vultr instance that tent-node runs on. On Linux it marks each socket with
// env.MetadataMark, which needs CAP_NET_ADMIN or CAP_NET_RAW; a failed mark fails the try.
func New() *Environment {
	d := net.Dialer{Control: markSocket}
	return newEnvironment(d.DialContext, tryTimeout, func(int) time.Duration { return retryWait })
}

// newEnvironment returns an Environment that connects through dial, gives each try at most try, and waits wait(n)
// after failed try n.
func newEnvironment(dial func(ctx context.Context, network, addr string) (net.Conn, error), try time.Duration,
	wait func(n int) time.Duration,
) *Environment {
	return &Environment{
		client: &http.Client{
			Transport: &http.Transport{
				// No proxy, whatever the environment says: through a proxy, 169.254.169.254 would be the proxy's own
				// metadata service.
				Proxy:             nil,
				DialContext:       dial,
				DisableKeepAlives: true, // a run reads the document once
			},
			// The document comes from the metadata service itself or not at all.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		try:  try,
		wait: wait,
	}
}

// Read gets the metadata document, http://169.254.169.254/v1.json, and returns the machine it describes: its
// instance-v2-id, its region code in lower case as its zone, and the IPv4 address of its private interface. After a
// failed connection, a try that does not get the whole answer in 5 seconds, a 429 or a 5xx other than 501, it waits a
// second and tries again, until ctx ends. Any other answer than 200 fails the read at once, as does a document that
// does not parse or lacks the id, the region or the private IPv4 address. No error holds the document, which carries
// the user data.
func (e *Environment) Read(ctx context.Context) (env.Instance, error) {
	var last error // the failure of the last try that ctx did not cut short
	for tries := 0; ; tries++ {
		if tries > 0 {
			retry.Sleep(ctx, e.wait(tries))
		}
		if cause := context.Cause(ctx); cause != nil {
			return env.Instance{}, stopped(cause, last)
		}
		body, again, err := e.fetch(ctx)
		switch {
		case err == nil:
			return parse(body)
		case !again:
			return env.Instance{}, fmt.Errorf("vultr metadata: %w", err)
		case ctx.Err() == nil:
			last = err
		}
	}
}

// fetch gets the document once, its body included, in at most e.try. again reports whether a failure is worth another
// try: a failed connection, no whole answer in time, or an answer that retry.Status takes.
func (e *Environment) fetch(ctx context.Context) (body []byte, again bool, err error) {
	tryCtx, cancel := context.WithTimeout(ctx, e.try)
	defer cancel()
	req, err := http.NewRequestWithContext(tryCtx, http.MethodGet, documentURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("GET %s: %w", documentURL, err)
	}
	resp, err := e.client.Do(req) // its errors name the method and the URL
	if err == nil {
		defer func() { _ = resp.Body.Close() }() // the connection is never reused, so an unread body costs nothing
		if resp.StatusCode != http.StatusOK {
			// The body stays unread: it may be the document.
			return nil, retry.Status(resp.StatusCode), fmt.Errorf("GET %s answered %s", documentURL, resp.Status)
		}
		if body, err = io.ReadAll(resp.Body); err == nil {
			return body, false, nil
		}
		err = fmt.Errorf("GET %s: read the answer: %w", documentURL, err)
	}
	if tryCtx.Err() != nil && ctx.Err() == nil {
		return nil, true, fmt.Errorf("GET %s: no answer within %v", documentURL, e.try)
	}
	return nil, true, err
}

// stopped returns the error of a read that ctx ended with cause, after a try that failed with last, if any.
func stopped(cause, last error) error {
	if last == nil {
		return fmt.Errorf("vultr metadata: %w", cause)
	}
	return fmt.Errorf("vultr metadata: %w; the last try: %w", cause, last)
}

// document is what Read takes from the metadata document. The document holds more, the user data among it, which
// Read leaves undecoded.
type document struct {
	InstanceID string             `json:"instance-v2-id"`
	Region     region             `json:"region"`
	Interfaces []networkInterface `json:"interfaces"`
}

// region is where the machine runs.
type region struct {
	Code string `json:"regioncode"` // in upper case, such as AMS
}

// networkInterface is one network interface of the machine.
type networkInterface struct {
	NetworkType string `json:"network-type"` // public or private
	IPv4        ipv4   `json:"ipv4"`
}

// ipv4 is the IPv4 settings of a network interface.
type ipv4 struct {
	Address string `json:"address"`
}

// parse returns the machine that the document data describes. An error names what is missing or wrong, and holds no
// more of the document than the value it names.
func parse(data []byte) (env.Instance, error) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return env.Instance{}, fmt.Errorf("vultr metadata: the document does not parse: %w", err)
	}
	switch {
	case doc.InstanceID == "":
		return env.Instance{}, errors.New("vultr metadata: no instance-v2-id")
	case doc.Region.Code == "":
		return env.Instance{}, errors.New("vultr metadata: no region.regioncode")
	}
	ip, err := doc.privateIPv4()
	if err != nil {
		return env.Instance{}, fmt.Errorf("vultr metadata: %w", err)
	}
	return env.Instance{ID: doc.InstanceID, Zone: strings.ToLower(doc.Region.Code), PrivateIP: ip}, nil
}

// privateIPv4 returns the IPv4 address of the one interface whose network-type is private.
func (doc *document) privateIPv4() (netip.Addr, error) {
	var addrs []string
	for _, i := range doc.Interfaces {
		if i.NetworkType == "private" {
			addrs = append(addrs, i.IPv4.Address)
		}
	}
	switch {
	case len(addrs) == 0:
		return netip.Addr{}, errors.New("no interface with network-type private")
	case len(addrs) > 1:
		return netip.Addr{}, fmt.Errorf("%d interfaces with network-type private, want one", len(addrs))
	case addrs[0] == "":
		return netip.Addr{}, errors.New("the private interface has no IPv4 address")
	}
	ip, err := netip.ParseAddr(addrs[0])
	switch {
	case err != nil || !ip.Is4():
		return netip.Addr{}, fmt.Errorf("the private interface's IPv4 address %q is not an IPv4 address", addrs[0])
	case ip.IsUnspecified():
		return netip.Addr{}, errors.New("the private interface has no IPv4 address: it reports 0.0.0.0")
	}
	return ip, nil
}
