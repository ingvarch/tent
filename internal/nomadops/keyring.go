package nomadops

import (
	"context"
	"net/http"
	"slices"

	"github.com/hashicorp/nomad/api"
)

const keyringPath = "/v1/operator/keyring/keys"

// KeyringReady reports whether the keyring has a key in the state active. Nomad makes its first key after a server
// becomes the leader, and signs client introduction tokens with it.
func (c *Client) KeyringReady(ctx context.Context) (bool, error) {
	var keys []*api.RootKeyMeta
	err := c.call(ctx, http.MethodGet, keyringPath, func(ctx context.Context) error {
		var err error
		keys, _, err = c.api.Keyring().List(query(ctx))
		return err
	})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(keys, func(k *api.RootKeyMeta) bool {
		return k != nil && k.State == api.RootKeyStateActive
	}), nil
}
