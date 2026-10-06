package nomadops_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

const (
	tokenPath = "/v1/acl/token"

	// tokenAccessor and tokenSecret are the accessor and the secret in an answer of Nomad.
	tokenAccessor = "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70"
	tokenSecret   = "0b7e51a4-3f2d-4a96-8c1e-7d5a9b3f6e28"
)

// tokenRequest asks for a token named "tent export nomad ana@laptop" for 24 hours.
var tokenRequest = nomadops.TokenRequest{Name: "tent export nomad ana@laptop", TTL: 24 * time.Hour}

// tokenAnswer is the body of an answer of Nomad that holds a token, with the fields that the client reads.
func tokenAnswer(t *testing.T, secretID, expires string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"AccessorID": tokenAccessor, "SecretID": secretID,
		"ExpirationTime": expires})
	if err != nil {
		t.Fatalf("the answer: %v", err)
	}
	return string(b)
}

// sentToken is what the client of CreateToken puts in the body of its request: every field of the API's token.
type sentToken struct {
	AccessorID, SecretID, Name, Type string
	Policies, Roles                  []string
	Global                           bool
	CreateTime                       time.Time
	ExpirationTime                   *time.Time
	ExpirationTTL                    string // such as 24h0m0s
	CreateIndex, ModifyIndex         uint64
}

// onlyRequest returns the single request that the server got, with its body decoded into a sentToken and left out of
// the request. A field in the body that sentToken lacks fails the test.
func onlyRequest(t *testing.T, s *nomadServer) (gotRequest, sentToken) {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("the server got %d requests, want 1", len(reqs))
	}
	var sent sentToken
	dec := json.NewDecoder(strings.NewReader(reqs[0].Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sent); err != nil {
		t.Fatalf("the request body: %v", err)
	}
	reqs[0].Body = ""
	return reqs[0], sent
}

func TestCreateToken(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK,
		tokenAnswer(t, tokenSecret, "2026-10-07T10:00:00.5Z")))

	got, err := newClient(t, srv, p, token).CreateToken(t.Context(), tokenRequest)

	if err != nil {
		t.Fatalf("CreateToken: %s", show(t, err, map[string]secret.Secret{clientToken: token,
			"the new token": []byte(tokenSecret)}))
	}
	want := nomadops.Token{Accessor: tokenAccessor, Secret: secret.Secret(tokenSecret),
		Expires: time.Date(2026, 10, 7, 10, 0, 0, 500_000_000, time.UTC)}
	if diff := cmp.Diff(want, got, cmp.Comparer(time.Time.Equal)); diff != "" {
		t.Errorf("CreateToken() (-want +got):\n%s", diff)
	}
	req, sent := onlyRequest(t, srv)
	wantReq := gotRequest{Method: http.MethodPut, Path: tokenPath, Query: "region=" + region, Token: clientToken,
		Peer: "cli." + region + ".nomad"}
	if req.Token != string(token) {
		t.Error("the request carried another token than the client's")
	}
	req.Token = clientToken
	if req != wantReq {
		t.Errorf("request = %+v, want %+v", req, wantReq)
	}
	wantSent := sentToken{Name: tokenRequest.Name, Type: "management", ExpirationTTL: "24h0m0s"}
	if diff := cmp.Diff(wantSent, sent); diff != "" {
		t.Errorf("request body (-want +got):\n%s", diff)
	}
}

// A token prints without its secret, whatever the verb or the encoder.
func TestTokenHidesItsSecret(t *testing.T) {
	tok := nomadops.Token{Accessor: tokenAccessor, Secret: secret.Secret(tokenSecret), Expires: time.Now()}

	secrettest.CheckHidden(t, secrettest.Printed(t, tok), map[string][]byte{"the new token": []byte(tokenSecret)}, "")
}

func TestCreateTokenRefusesABadRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK,
		tokenAnswer(t, tokenSecret, "2026-10-07T10:00:00Z")))
	c := newClient(t, srv, p, token)
	cases := []struct {
		name   string
		change func(*nomadops.TokenRequest)
		want   string
	}{
		{"no name", func(r *nomadops.TokenRequest) { r.Name = "" }, "nomad: ACL token: no name"},
		{"no TTL", func(r *nomadops.TokenRequest) { r.TTL = 0 }, "nomad: ACL token: TTL 0s is not above zero"},
		{"a TTL below zero", func(r *nomadops.TokenRequest) { r.TTL = -time.Minute },
			"nomad: ACL token: TTL -1m0s is not above zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tokenRequest
			tc.change(&req)
			got, err := c.CreateToken(t.Context(), req)
			checkCallErr(t, err, tc.want, false, clientTokens(token))
			if got.Secret != nil || got.Accessor != "" || !got.Expires.IsZero() {
				t.Errorf("CreateToken() gave a token with the error")
			}
		})
	}
	checkRequests(t, srv, clientTokens(token))
}

func TestCreateTokenErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	const (
		prefix  = "nomad: PUT /v1/acl/token: "
		invalid = "token 0 invalid: 1 error occurred:" // how Nomad starts its 400 for a token that it refuses
	)
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no permission", http.StatusForbidden, "Permission denied", prefix + "403: Permission denied", false},
		{"a TTL under a minute", http.StatusBadRequest,
			invalid + "\n\t* expiration time cannot be less than 1m0s in the future (was 30s)\n\n",
			prefix + "400: " + invalid + " * expiration time cannot be less than 1m0s in the future (was 30s)", false},
		{"a TTL over a day", http.StatusBadRequest,
			invalid + "\n\t* expiration time cannot be more than 24h0m0s in the future (was 24h1m0s)\n\n",
			prefix + "400: " + invalid + " * expiration time cannot be more than 24h0m0s in the future (was 24h1m0s)",
			false},
		{"no leader", http.StatusInternalServerError, "No cluster leader", prefix + "500: No cluster leader", true},
		{"no secret", http.StatusOK, tokenAnswer(t, "", "2026-10-07T10:00:00Z"),
			prefix + "the answer holds no secret", false},
		{"no end", http.StatusOK, `{"AccessorID":"` + tokenAccessor + `","SecretID":"` + tokenSecret + `"}`,
			prefix + "the answer holds no end", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))

			got, err := newClient(t, srv, p, token).CreateToken(t.Context(), tokenRequest)

			checkCallErr(t, err, tc.want, tc.notReady, map[string]secret.Secret{clientToken: token,
				"the new token": []byte(tokenSecret)})
			if got.Secret != nil || got.Accessor != "" || !got.Expires.IsZero() {
				t.Errorf("CreateToken() gave a token with the error")
			}
			if n := len(srv.requests()); n != 1 {
				t.Errorf("the server got %d requests, want 1", n)
			}
		})
	}
}

// The call ends with the client's time limit when the server does not answer.
func TestCreateTokenOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
	c := newClient(t, srv, p, token)
	c.SetTimeout(50 * time.Millisecond)

	_, err := c.CreateToken(t.Context(), tokenRequest)

	checkCallErr(t, err, "nomad: PUT /v1/acl/token: no answer within 50ms", true, clientTokens(token))
}
