package nomadops_test

import (
	"net/http"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/pki"
)

// keyringRequest is the request of KeyringReady.
var keyringRequest = gotRequest{Method: http.MethodGet, Path: "/v1/operator/keyring/keys", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// A key of the keyring as Nomad lists it, in the state that the argument gives.
func keyJSON(id, state string) string {
	return `{"KeyID":"` + id + `","Algorithm":"aes256-gcm","CreateTime":1790000000000000000,"CreateIndex":9,` +
		`"ModifyIndex":9,"State":"` + state + `","PublishTime":0}`
}

// TestKeyringReady checks that KeyringReady is true exactly when the keyring lists a key in the state active.
func TestKeyringReady(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"an active key", "[" + keyJSON("a1", "active") + "]", true},
		{"an active key among others", "[" + keyJSON("a1", "deprecated") + "," + keyJSON("a2", "active") + "]", true},
		{"only a deprecated key", "[" + keyJSON("a1", "deprecated") + "]", false},
		{"only a key that is not published", "[" + keyJSON("a1", "prepublished") + "]", false},
		{"no key", "[]", false},
		{"a list that is null", "null", false},
		{"a null entry", "[null]", false},
		{"a null entry before an active key", "[null," + keyJSON("a1", "active") + "]", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, tc.body))
			got, err := newClient(t, srv, p, token).KeyringReady(t.Context())
			if err != nil || got != tc.want {
				t.Errorf("KeyringReady() = %v, %v; want %v, nil", got, err, tc.want)
			}
			checkRequests(t, srv, clientTokens(token), keyringRequest)
		})
	}
}

func TestKeyringReadyErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	const path = "nomad: GET /v1/operator/keyring/keys: "
	cases := []struct {
		name     string
		status   int
		body     string
		want     string
		notReady bool
	}{
		{"no leader", http.StatusInternalServerError, "No cluster leader", path + "500: No cluster leader", true},
		{"no permission", http.StatusForbidden, "Permission denied", path + "403: Permission denied", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))
			got, err := newClient(t, srv, p, token).KeyringReady(t.Context())
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if got {
				t.Error("KeyringReady() = true with the error, want false")
			}
			checkRequests(t, srv, clientTokens(token), keyringRequest)
		})
	}
}
