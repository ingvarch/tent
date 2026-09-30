// Package retry holds what tent-node's retries share: which answers are worth another try, and a wait that ends with
// its context.
package retry

import (
	"context"
	"net/http"
	"time"
)

// Status reports whether an HTTP answer with the status code is worth another try: 429, or a 5xx other than 501,
// which says the server will never serve the request.
func Status(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599 && code != http.StatusNotImplemented)
}

// Sleep waits for d and reports true, or reports false once ctx ends.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
