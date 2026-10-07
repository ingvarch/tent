package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetTextReturnsThePlainBodyAndSendsTheToken(t *testing.T) {
	var token, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, path = r.Header.Get("X-Nomad-Token"), r.URL.RequestURI()
		_, _ = w.Write([]byte("wget: download timed out\nsecond line"))
	}))
	t.Cleanup(srv.Close)
	n := &nomadAPI{base: srv.URL, token: testToken, client: srv.Client()}
	got, err := n.getText(t.Context(), "/v1/x?plain=true")
	if err != nil {
		t.Fatalf("getText: %v", err)
	}
	if got != "wget: download timed out\nsecond line" {
		t.Errorf("getText = %q, want the body as it is", got)
	}
	if token != testToken || path != "/v1/x?plain=true" {
		t.Errorf("request = token %q, path %q, want the token and the path with its query", token, path)
	}
}

func TestGetTextErrorNamesTheCallAndTheStatusButNotTheToken(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){"GET /v1/x": fixed(500, " boom ")})
	n.token = testToken
	_, err := n.getText(t.Context(), "/v1/x")
	if err == nil || err.Error() != "nomad: GET /v1/x: 500: boom" {
		t.Errorf("getText error = %v, want %q", err, "nomad: GET /v1/x: 500: boom")
	}
	if err != nil && strings.Contains(err.Error(), testToken) {
		t.Errorf("getText error = %v, holds the token", err)
	}
}

func TestGetTextReadsNoMoreThanTheLimit(t *testing.T) {
	_, n := newFakeNomad(t, map[string]func() (int, string){
		"GET /v1/x": fixed(200, strings.Repeat("a", maxTextBody+100)),
	})
	got, err := n.getText(t.Context(), "/v1/x")
	if err != nil {
		t.Fatalf("getText: %v", err)
	}
	if len(got) != maxTextBody {
		t.Errorf("getText read %d bytes, want %d", len(got), maxTextBody)
	}
}
