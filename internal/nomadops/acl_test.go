package nomadops_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

const (
	bootstrapPath = "/v1/acl/bootstrap"
	selfPath      = "/v1/acl/token/self"
	// alreadyDone is Nomad's answer to a second bootstrap.
	alreadyDone = "ACL bootstrap already done (reset index: 7)"
	// bootstrapSecret names the bootstrap secret in the requests that tests expect.
	bootstrapSecret = "the bootstrap secret"
	// mismatch is the error of a bootstrap with another secret than the cluster's.
	mismatch = "nomad: PUT /v1/acl/bootstrap: " +
		"the ACL system was bootstrapped with a secret other than the one in secrets/acl-bootstrap-token"
)

// The requests of Bootstrap: the bootstrap, and the check of the secret after an earlier bootstrap.
var (
	bootstrapRequest = gotRequest{Method: http.MethodPut, Path: bootstrapPath, Query: "region=" + region,
		Token: clientToken, Peer: "cli." + region + ".nomad", Body: `{"BootstrapSecret":"<` + bootstrapSecret + `>"}` + "\n"}
	selfRequest = gotRequest{Method: http.MethodGet, Path: selfPath, Query: "region=" + region,
		Token: bootstrapSecret, Peer: "cli." + region + ".nomad"}
)

// bootstrapTokens names the client's token and the bootstrap secret for checkRequests.
func bootstrapTokens(token, s secret.Secret) map[string]secret.Secret {
	return map[string]secret.Secret{clientToken: token, bootstrapSecret: s}
}

// managementToken is the JSON of a management token with the secret, as Nomad answers a bootstrap or token/self.
func managementToken(s secret.Secret) string {
	return `{"AccessorID":"8f3a1c2e-7b4d-4e6f-9a0b-1c2d3e4f5a6b","SecretID":"` + string(s) +
		`","Name":"Bootstrap Token","Type":"management","Global":true}`
}

// routes returns a handler that answers each "METHOD path" with its handler, and anything else with 404.
func routes(handlers map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func TestBootstrap(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token, s := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, managementToken(s)))
	if err := newClient(t, srv, p, token).Bootstrap(t.Context(), s); err != nil {
		t.Fatalf("Bootstrap: %s", show(t, err, bootstrapTokens(token, s)))
	}
	checkRequests(t, srv, bootstrapTokens(token, s), bootstrapRequest)
}

// TestBootstrapAlreadyDone checks that a bootstrap after an earlier one asks the servers whose token the secret is.
func TestBootstrapAlreadyDone(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token, s := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		self     http.HandlerFunc // the answer to token/self with the secret
		want     string           // the error; empty for none
		notReady bool
	}{
		{"by the secret", answer(http.StatusOK, managementToken(s)), "", false},
		{"by another secret", answer(http.StatusForbidden, "Permission denied"), mismatch, false},
		{"a client token of the secret", answer(http.StatusOK, `{"SecretID":"`+string(s)+`","Type":"client"}`),
			mismatch, false},
		{"no leader", answer(http.StatusInternalServerError, "No cluster leader"),
			"nomad: GET /v1/acl/token/self: 500: No cluster leader", true},
		{"no token/self", answer(http.StatusNotFound, "Not found"), "nomad: GET /v1/acl/token/self: 404: Not found",
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), routes(map[string]http.HandlerFunc{
				"PUT " + bootstrapPath: answer(http.StatusBadRequest, alreadyDone),
				"GET " + selfPath:      tc.self,
			}))
			err := newClient(t, srv, p, token).Bootstrap(t.Context(), s)
			if tc.want == "" {
				if err != nil {
					t.Errorf("Bootstrap: %s", show(t, err, bootstrapTokens(token, s)))
				}
			} else {
				checkCallErr(t, err, tc.want, tc.notReady, bootstrapTokens(token, s))
			}
			if got, want := errors.Is(err, nomadops.ErrBootstrapMismatch), tc.want == mismatch; got != want {
				t.Errorf("errors.Is(%s, ErrBootstrapMismatch) = %v, want %v", show(t, err, bootstrapTokens(token, s)),
					got, want)
			}
			checkRequests(t, srv, bootstrapTokens(token, s), bootstrapRequest, selfRequest)
		})
	}
}

// TestBootstrapAfterALostAnswer checks that a bootstrap whose answer was lost fails with ErrNotReady, and that the
// next one finds the bootstrap done with the secret. The bootstrap goes on a connection that a call before it
// opened, where an HTTP client may send a request again after a lost answer.
func TestBootstrapAfterALostAnswer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token, s := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	var mu sync.Mutex
	var bootstrapped string // the secret of the management token, once there is one
	var clients []string    // the client addresses of the leader call and the first bootstrap
	srv := newNomadServer(t, p.server, p.ca.Bundle(), routes(map[string]http.HandlerFunc{
		"GET " + leaderRequest.Path: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			clients = append(clients, r.RemoteAddr)
			mu.Unlock()
			answer(http.StatusOK, `"10.0.0.5:4647"`)(w, r)
		},
		"PUT " + bootstrapPath: func(w http.ResponseWriter, r *http.Request) {
			var req struct{ BootstrapSecret string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode the bootstrap request: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if bootstrapped != "" {
				answer(http.StatusBadRequest, alreadyDone)(w, r)
				return
			}
			bootstrapped = req.BootstrapSecret
			clients = append(clients, r.RemoteAddr)
			dropConnection(t, w)
		},
		"GET " + selfPath: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Header.Get("X-Nomad-Token") != bootstrapped {
				answer(http.StatusForbidden, "Permission denied")(w, r)
				return
			}
			answer(http.StatusOK, managementToken(secret.Secret(bootstrapped)))(w, r)
		},
	}))
	c := newClient(t, srv, p, token)
	secrets := bootstrapTokens(token, s)

	if _, err := c.Leader(t.Context()); err != nil {
		t.Fatalf("Leader: %s", show(t, err, secrets))
	}
	err := c.Bootstrap(t.Context(), s)
	if !errors.Is(err, nomadops.ErrNotReady) {
		t.Errorf("Bootstrap() error = %s, want one that matches ErrNotReady", show(t, err, secrets))
	}
	if err := c.Bootstrap(t.Context(), s); err != nil {
		t.Errorf("Bootstrap again: %s", show(t, err, secrets))
	}
	checkRequests(t, srv, secrets, leaderRequest, bootstrapRequest, bootstrapRequest, selfRequest)
	mu.Lock()
	defer mu.Unlock()
	if len(clients) != 2 || clients[0] != clients[1] {
		t.Errorf("the leader call and the first bootstrap came from %v, want one connection", clients)
	}
}

// dropConnection closes the connection of a request without an answer, as a server does that fails after it carried
// the request out.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack the connection: %v", err)
		return
	}
	_ = conn.Close()
}

// TestBootstrapErrors checks the answers to the bootstrap itself that end Bootstrap.
func TestBootstrapErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token, s := pki.NewBootstrapSecret(), pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no leader", http.StatusInternalServerError, "No cluster leader",
			"nomad: PUT /v1/acl/bootstrap: 500: No cluster leader", true},
		{"a secret that Nomad refuses", http.StatusBadRequest, "invalid acl token",
			"nomad: PUT /v1/acl/bootstrap: 400: invalid acl token", false},
		{"ACLs off", http.StatusBadRequest, "ACL support disabled",
			"nomad: PUT /v1/acl/bootstrap: 400: ACL support disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			err := newClient(t, srv, p, token).Bootstrap(t.Context(), s)
			checkCallErr(t, err, tc.want, tc.notReady, bootstrapTokens(token, s))
			if errors.Is(err, nomadops.ErrBootstrapMismatch) {
				t.Error("the error matches ErrBootstrapMismatch")
			}
			checkRequests(t, srv, bootstrapTokens(token, s), bootstrapRequest)
		})
	}
}

// TestBootstrapRefusesABadSecret checks that Bootstrap sends only a secret as tent makes one: never none, which would
// let Nomad pick one that tent does not know, and never one that Nomad refuses only after an earlier bootstrap.
func TestBootstrapRefusesABadSecret(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, managementToken(token)))
	c := newClient(t, srv, p, token)
	for name, s := range map[string]secret.Secret{
		"none":       nil,
		"not a UUID": secret.Secret("root-token"),
		"upper case": secret.Secret(strings.ToUpper(string(pki.NewBootstrapSecret()))),
	} {
		t.Run(name, func(t *testing.T) {
			err := c.Bootstrap(t.Context(), s)
			checkCallErr(t, err, "nomad: ACL bootstrap secret: not a lower-case UUID of version 4", false,
				bootstrapTokens(token, s))
		})
	}
	checkRequests(t, srv, clientTokens(token))
}

const introPath = "/v1/acl/identity/client-introduction-token"

// jwt is an introduction token as Nomad answers one.
const jwt = "eyJhbGciOiJFUzI1NiIsImtpZCI6ImsxIn0.eyJub21hZF9ub2RlX25hbWUiOiJwcm9kLXdvcmtlcnMtMSJ9.c2lnbmF0dXJl"

// introRequest is the request of an introduction token for prod-workers-1 in the pool default, for 30 minutes.
var introRequest = nomadops.IntroRequest{NodeName: "prod-workers-1", NodePool: "default", TTL: 30 * time.Minute}

// introCall is the call of IntroToken with introRequest.
var introCall = gotRequest{Method: http.MethodPut, Path: introPath, Query: "region=" + region, Token: clientToken,
	Peer: "cli." + region + ".nomad", Body: `{"TTL":1800000000000,"NodeName":"prod-workers-1","NodePool":"default"}` + "\n"}

func TestIntroToken(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `{"JWT":"`+jwt+`"}`))
	got, err := newClient(t, srv, p, token).IntroToken(t.Context(), introRequest)
	if err != nil {
		t.Fatalf("IntroToken: %s", show(t, err, map[string]secret.Secret{clientToken: token, "the JWT": []byte(jwt)}))
	}
	if string(got) != jwt {
		t.Errorf("IntroToken() of %d bytes is not the JWT of the answer", len(got))
	}
	secrettest.CheckHidden(t, secrettest.Printed(t, got), map[string][]byte{"the JWT": []byte(jwt)},
		fmt.Sprintf("[secret, %d bytes]", len(jwt)))
	checkRequests(t, srv, clientTokens(token), introCall)
}

func TestIntroTokenRefusesABadRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, `{"JWT":"`+jwt+`"}`))
	c := newClient(t, srv, p, token)
	badTTL := func(ttl string) string {
		return "nomad: intro token: TTL " + ttl + " is not above zero and at most 30m0s"
	}
	cases := []struct {
		name   string
		change func(*nomadops.IntroRequest)
		want   string
	}{
		{"no node name", func(r *nomadops.IntroRequest) { r.NodeName = "" }, "nomad: intro token: no node name"},
		{"no node pool", func(r *nomadops.IntroRequest) { r.NodePool = "" }, "nomad: intro token: no node pool"},
		{"no TTL", func(r *nomadops.IntroRequest) { r.TTL = 0 }, badTTL("0s")},
		{"a TTL below zero", func(r *nomadops.IntroRequest) { r.TTL = -time.Minute }, badTTL("-1m0s")},
		{"a TTL above 30 minutes", func(r *nomadops.IntroRequest) { r.TTL = 30*time.Minute + time.Second },
			badTTL("30m1s")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := introRequest
			tc.change(&req)
			got, err := c.IntroToken(t.Context(), req)
			checkCallErr(t, err, tc.want, false, clientTokens(token))
			if got != nil {
				t.Errorf("IntroToken() gave a token of %d bytes with the error", len(got))
			}
		})
	}
	checkRequests(t, srv, clientTokens(token))
}

func TestIntroTokenErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no permission", http.StatusForbidden, "Permission denied",
			"nomad: PUT /v1/acl/identity/client-introduction-token: 403: Permission denied", false},
		{"no leader", http.StatusInternalServerError, "No cluster leader",
			"nomad: PUT /v1/acl/identity/client-introduction-token: 500: No cluster leader", true},
		{"no JWT", http.StatusOK, `{}`,
			"nomad: PUT /v1/acl/identity/client-introduction-token: the answer holds no JWT", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).IntroToken(t.Context(), introRequest)
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if got != nil {
				t.Errorf("IntroToken() gave a token of %d bytes with the error", len(got))
			}
			checkRequests(t, srv, clientTokens(token), introCall)
		})
	}
}
