package nomadops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
)

const (
	bootstrapPath = "/v1/acl/bootstrap"
	selfPath      = "/v1/acl/token/self"
)

// Bootstrap bootstraps the cluster's ACL system with bootstrapSecret as the secret of the management token. It is
// safe to repeat: when the ACL system was bootstrapped already, Bootstrap asks the servers whose token the secret is,
// and succeeds when it is a management token. Otherwise it fails with ErrBootstrapMismatch. It sends only a secret
// that tent makes, a lower-case UUID of version 4: never an empty one, for which Nomad would make a secret that nobody
// knows. Its errors never show the secret.
func (c *Client) Bootstrap(ctx context.Context, bootstrapSecret secret.Secret) error {
	if err := pki.CheckBootstrapSecret(bootstrapSecret); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	err := c.call(ctx, http.MethodPut, bootstrapPath, func(ctx context.Context) error {
		_, _, err := c.api.ACLTokens().BootstrapOpts(string(bootstrapSecret), write(ctx))
		return err
	})
	if !alreadyDone(err) {
		return err
	}
	var self *api.ACLToken
	err = c.call(ctx, http.MethodGet, selfPath, func(ctx context.Context) error {
		q := query(ctx)
		q.AuthToken = string(bootstrapSecret)
		var err error
		self, _, err = c.api.ACLTokens().Self(q)
		return err
	})
	var e *callError
	if err == nil && self.Type != "management" || errors.As(err, &e) && e.status == http.StatusForbidden {
		return &callError{method: http.MethodPut, path: bootstrapPath, cause: ErrBootstrapMismatch}
	}
	return err
}

// MaxIntroTTL is the longest TTL of an introduction token: the default max_identity_ttl of Nomad servers, which cut a
// longer TTL without a word.
const MaxIntroTTL = 30 * time.Minute

// IntroLeeway is how long after its expiry a server still accepts an introduction token: the default leeway of the
// library that checks its claims is one minute.
const IntroLeeway = time.Minute

// IntroRequest asks for a client introduction token.
type IntroRequest struct {
	NodeName string        // the name of the node that the token introduces
	NodePool string        // the node pool of the node
	TTL      time.Duration // how long the token is valid: above zero and at most MaxIntroTTL
}

// Check checks that the request names a node and a pool, and that its TTL is above zero and at most MaxIntroTTL.
func (r IntroRequest) Check() error {
	switch {
	case r.NodeName == "":
		return errors.New("intro token: no node name")
	case r.NodePool == "":
		return errors.New("intro token: no node pool")
	case r.TTL <= 0 || r.TTL > MaxIntroTTL:
		return fmt.Errorf("intro token: TTL %s is not above zero and at most %s", r.TTL, MaxIntroTTL)
	}
	return nil
}

const introPath = "/v1/acl/identity/client-introduction-token"

// errNoJWT is why IntroToken fails after an answer without a token.
var errNoJWT = errors.New("the answer holds no JWT")

// IntroToken returns a new client introduction token, a JWT, bound to the node name and pool of req. It checks req
// before it sends a request.
func (c *Client) IntroToken(ctx context.Context, req IntroRequest) (secret.Secret, error) {
	if err := req.Check(); err != nil {
		return nil, fmt.Errorf("nomad: %w", err)
	}
	var resp *api.ACLIdentityClientIntroductionTokenResponse
	err := c.call(ctx, http.MethodPut, introPath, func(ctx context.Context) error {
		var err error
		resp, _, err = c.api.ACLIdentity().CreateClientIntroductionToken(&api.ACLIdentityClientIntroductionTokenRequest{
			NodeName: req.NodeName, NodePool: req.NodePool, TTL: req.TTL,
		}, write(ctx))
		return err
	})
	switch {
	case err != nil:
		return nil, err
	case resp.JWT == "":
		return nil, &callError{method: http.MethodPut, path: introPath, cause: errNoJWT}
	}
	return secret.Secret(resp.JWT), nil
}

// alreadyDone reports whether err is Nomad's answer to a bootstrap after an earlier one.
func alreadyDone(err error) bool {
	var e *callError
	return errors.As(err, &e) && e.status == http.StatusBadRequest &&
		strings.HasPrefix(e.message, "ACL bootstrap already done")
}
