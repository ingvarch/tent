package nomadops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/ingvarch/tent/internal/secret"
)

// TokenRequest asks for a management token that expires.
type TokenRequest struct {
	Name string        // the name of the token, which Nomad lists beside it
	TTL  time.Duration // how long the token is valid, above zero; Nomad refuses less than a minute and more than a day
}

// Check checks that the request has a name and a TTL above zero. Nomad's own limits on the TTL are left to Nomad.
func (r TokenRequest) Check() error {
	switch {
	case r.Name == "":
		return errors.New("ACL token: no name")
	case r.TTL <= 0:
		return fmt.Errorf("ACL token: TTL %s is not above zero", r.TTL)
	}
	return nil
}

// Token is a management token that Nomad made. Printing it never shows its secret.
type Token struct {
	Accessor string        // the token's public id, by which an operator deletes it
	Secret   secret.Secret // what the calls send as the token
	Expires  time.Time     // when Nomad stops accepting the token
}

const tokenPath = "/v1/acl/token"

// Why CreateToken fails after an answer that Nomad should not give.
var (
	errNoTokenSecret = errors.New("the answer holds no secret")
	errNoTokenEnd    = errors.New("the answer holds no end")
)

// CreateToken makes a management token that Nomad stops accepting after req.TTL, and returns it. It checks req before
// it sends a request. Nomad refuses a TTL of less than a minute or of more than a day with a 400 that says so. The
// client's own token must be a management token. Its errors never show a secret.
func (c *Client) CreateToken(ctx context.Context, req TokenRequest) (Token, error) {
	if err := req.Check(); err != nil {
		return Token{}, fmt.Errorf("nomad: %w", err)
	}
	var resp *api.ACLToken
	err := c.call(ctx, http.MethodPut, tokenPath, func(ctx context.Context) error {
		var err error
		resp, _, err = c.api.ACLTokens().Create(&api.ACLToken{
			Name: req.Name, Type: "management", ExpirationTTL: req.TTL,
		}, write(ctx))
		return err
	})
	switch {
	case err != nil:
		return Token{}, err
	case resp.SecretID == "":
		return Token{}, &callError{method: http.MethodPut, path: tokenPath, cause: errNoTokenSecret}
	case resp.ExpirationTime == nil:
		return Token{}, &callError{method: http.MethodPut, path: tokenPath, cause: errNoTokenEnd}
	}
	return Token{Accessor: resp.AccessorID, Secret: secret.Secret(resp.SecretID), Expires: *resp.ExpirationTime}, nil
}
