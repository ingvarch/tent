package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// maxTextBody is how much of a plain-text answer getText reads.
const maxTextBody = 64 << 10

// getText reads path (with its query) and returns the plain body, at most maxTextBody bytes of it. Errors read as
// those of get.
func (n *nomadAPI) getText(ctx context.Context, path string) (string, error) {
	resp, label, err := n.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(io.LimitReader(resp.Body, maxTextBody))
	if err != nil {
		return "", fmt.Errorf("%s: read: %w", label, err)
	}
	return string(text), nil
}
