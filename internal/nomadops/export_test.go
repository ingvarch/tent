package nomadops

import (
	"net/http"
	"time"
)

// SetTimeout sets how long each call of the client may take, the snapshot calls too, for tests.
func (c *Client) SetTimeout(d time.Duration) { c.timeout, c.snapshotTimeout = d, d }

// SetSnapshotTimeout sets how long each snapshot call of the client may take, for tests.
func (c *Client) SetSnapshotTimeout(d time.Duration) { c.snapshotTimeout = d }

// TimeLimits returns how long a call and a snapshot call of the client may take, for tests.
func (c *Client) TimeLimits() (call, snapshot time.Duration) { return c.timeout, c.snapshotTimeout }

// NewCallError is the error of a call that failed without an answer's status, for tests.
var NewCallError = newCallError

// Refusal returns why the proxy listening on listen refuses a request with the Host and headers, or "" when it lets
// the request through, for tests.
func Refusal(listen, host string, h http.Header) (string, error) {
	g, err := newHostGuard(listen)
	if err != nil {
		return "", err
	}
	return g.refusal(host, h), nil
}
