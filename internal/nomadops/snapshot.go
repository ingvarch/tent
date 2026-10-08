package nomadops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ingvarch/tent/internal/secret"
)

const snapshotPath = "/v1/operator/snapshot"

// CheckSnapshot checks that snap holds bytes: Nomad answers 500 to an empty restore, which a caller would take for a
// snapshot that fails now but may restore later.
func CheckSnapshot(snap secret.Secret) error {
	if len(snap) == 0 {
		return errors.New("no snapshot")
	}
	return nil
}

// SaveSnapshot returns a snapshot of the cluster's state, as Nomad sends it: a gzip file that holds the keyring and
// the ACL tokens, which is why it comes as a secret. It reads the whole snapshot into memory within the call's limit
// of 5 minutes, and fails when the Digest header is missing, is of a kind that the client does not know, or does not
// match the bytes, and when the answer holds no bytes. A server whose configuration entry is not applied yet answers
// 500, which matches ErrNotReady.
func (c *Client) SaveSnapshot(ctx context.Context) (secret.Secret, error) {
	var snap []byte
	err := c.callWithin(ctx, c.snapshotTimeout, http.MethodGet, snapshotPath, func(ctx context.Context) error {
		body, err := c.api.Operator().Snapshot(query(ctx))
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }() // nothing to flush after a read
		// Read here: the context of the call ends when call returns, and with it the body.
		snap, err = io.ReadAll(body)
		if err == nil && len(snap) == 0 {
			return errors.New("no snapshot in the answer")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// RestoreSnapshot replaces the cluster's state with the snapshot, which SaveSnapshot returned, within the call's limit
// of 5 minutes. A snapshot that Nomad refuses is a 500, which matches ErrNotReady, like every 500. It checks that the
// snapshot holds bytes before it sends a request.
func (c *Client) RestoreSnapshot(ctx context.Context, snap secret.Secret) error {
	if err := CheckSnapshot(snap); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	return c.callWithin(ctx, c.snapshotTimeout, http.MethodPut, snapshotPath, func(ctx context.Context) error {
		_, err := c.api.Operator().SnapshotRestore(bytes.NewReader(snap), write(ctx))
		return err
	})
}
