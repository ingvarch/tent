package nomadops_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// raftID is the Raft ID of a server in the tests, a UUID as Nomad makes them.
const raftID = "6d1f1e2a-9c1b-4d53-8a0e-1b2c3d4e5f60"

// otherRaftID is the Raft ID of another server.
const otherRaftID = "7e2a2f3b-0d2c-4e64-9b1f-2c3d4e5f6071"

// raftCall is one of the writes on a Raft server, with the request that it makes.
type raftCall struct {
	name   string // the method
	method string // the HTTP method
	path   string
	do     func(context.Context, nomadops.API, string) error
	// goneStatus is the status that Nomad answers for a server that is not in the Raft configuration.
	goneStatus int
	// otherStatus is a status that is not goneStatus.
	otherStatus int
	// goneIsDone is set when a server that is gone counts as success.
	goneIsDone bool
}

// raftCalls are TransferLeadership and RemovePeer.
var raftCalls = []raftCall{
	{name: "TransferLeadership", method: http.MethodPut, path: "/v1/operator/raft/transfer-leadership",
		goneStatus: http.StatusBadRequest, otherStatus: http.StatusInternalServerError,
		do: func(ctx context.Context, a nomadops.API, id string) error { return a.TransferLeadership(ctx, id) }},
	{name: "RemovePeer", method: http.MethodDelete, path: "/v1/operator/raft/peer",
		goneStatus: http.StatusInternalServerError, otherStatus: http.StatusBadRequest, goneIsDone: true,
		do: func(ctx context.Context, a nomadops.API, id string) error { return a.RemovePeer(ctx, id) }},
}

// request is the request of the call for the server with the Raft ID.
func (c raftCall) request(id string) gotRequest {
	return gotRequest{Method: c.method, Path: c.path, Query: "id=" + id + "&region=" + region,
		Token: clientToken, Peer: "cli." + region + ".nomad"}
}

// notInRaft is what Nomad says about a Raft ID that is not in the configuration.
func notInRaft(id string) string { return `id "` + id + `" was not found in the Raft configuration` }

// TestRaftWritesOverMTLS checks the request of each write: method, path, query without a body, token and client
// certificate. An answer with a body, as Nomad gives to a transfer, and one without both succeed.
func TestRaftWritesOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range raftCalls {
		for name, body := range map[string]string{
			"with a body": `{"From":{"ID":"a"},"To":{"ID":"` + raftID + `"},"Noop":false,"Err":null}`,
			"the leader":  `{"Noop":true}`,
			"no body":     "",
		} {
			t.Run(c.name+" "+name, func(t *testing.T) {
				srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, body))

				err := c.do(t.Context(), newClient(t, srv, p, token), raftID)

				if err != nil {
					t.Fatalf("%s: %s", c.name, show(t, err, clientTokens(token)))
				}
				checkRequests(t, srv, clientTokens(token), c.request(raftID))
			})
		}
	}
}

// TestRaftWritesRefuseABadIDBeforeAnyRequest checks that an empty Raft ID and one that holds more than ASCII letters,
// digits and "-" fail without a request.
func TestRaftWritesRefuseABadIDBeforeAnyRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	c := newClient(t, srv, p, token)
	const rule = ` has a character other than an ASCII letter, a digit or "-"`
	for _, tc := range []struct{ name, id, want string }{
		{"empty", "", "nomad: no Raft ID"},
		{"a question mark", "a?b", `nomad: Raft ID "a?b"` + rule},
		{"an ampersand", raftID + "&id=x", `nomad: Raft ID "` + raftID + `&id=x"` + rule},
		{"a slash", "a/b", `nomad: Raft ID "a/b"` + rule},
		{"a space", "a b", `nomad: Raft ID "a b"` + rule},
	} {
		for _, call := range raftCalls {
			t.Run(call.name+" "+tc.name, func(t *testing.T) {
				checkCallErr(t, call.do(t.Context(), c, tc.id), tc.want, false, clientTokens(token))
			})
		}
	}
	checkRequests(t, srv, clientTokens(token))
}

// TestRaftWritesOfAServerThatIsGone checks the class of each answer that Nomad gives for a Raft ID: gone is ErrGone,
// and success for a removal; the answers that only look like it keep their class.
func TestRaftWritesOfAServerThatIsGone(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name   string
		status int // 0 is the call's goneStatus, -1 its otherStatus
		body   string
		gone   bool
	}{
		{"the leader's answer", 0, notInRaft(raftID), true},
		{"a follower's answer", 0, "rpc error: " + notInRaft(raftID), true},
		{"an answer forwarded twice", 0, "rpc error: rpc error: " + notInRaft(raftID), true},
		{"an answer with a line end", 0, notInRaft(raftID) + "\n", true},
		{"another ID", 0, notInRaft(otherRaftID), false},
		{"a part of the ID", 0, notInRaft(raftID[:8]), false},
		{"more words after it", 0, notInRaft(raftID) + " yet", false},
		{"more words before it", 0, "no rpc error: " + notInRaft(raftID), false},
		{"the text with another status", -1, notInRaft(raftID), false},
		{"the text with a 404", http.StatusNotFound, notInRaft(raftID), false},
		{"the text with a 503", http.StatusServiceUnavailable, notInRaft(raftID), false},
		{"the text with a 429", http.StatusTooManyRequests, notInRaft(raftID), false},
		{"no ID", 0, "must specify id or address", false},
		{"no leader", http.StatusInternalServerError, "No cluster leader", false},
		{"no permission", http.StatusForbidden, "Permission denied", false},
	}
	for _, c := range raftCalls {
		for _, tc := range cases {
			t.Run(c.name+" "+tc.name, func(t *testing.T) {
				status := tc.status
				switch status {
				case 0:
					status = c.goneStatus
				case -1:
					status = c.otherStatus
				}
				srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(status, tc.body))

				err := c.do(t.Context(), newClient(t, srv, p, token), raftID)

				text := show(t, err, clientTokens(token))
				if tc.gone && c.goneIsDone {
					if err != nil {
						t.Errorf("%s: %s, want success for a server that is gone", c.name, text)
					}
					return
				}
				if err == nil {
					t.Fatalf("%s: no error", c.name)
				}
				wantText := "nomad: " + c.method + " " + c.path + ": " + strconv.Itoa(status) + ": " +
					strings.Join(strings.Fields(tc.body), " ")
				if text != wantText {
					t.Errorf("error = %s, want %q", text, wantText)
				}
				wantNotReady := !tc.gone &&
					(status >= http.StatusInternalServerError || status == http.StatusTooManyRequests)
				if got := errors.Is(err, nomadops.ErrGone); got != tc.gone {
					t.Errorf("errors.Is(%s, ErrGone) = %v, want %v", text, got, tc.gone)
				}
				if got := errors.Is(err, nomadops.ErrNotReady); got != wantNotReady {
					t.Errorf("errors.Is(%s, ErrNotReady) = %v, want %v", text, got, wantNotReady)
				}
				if n := len(srv.requests()); n != 1 {
					t.Errorf("the server got %d requests, want 1", n)
				}
			})
		}
	}
}

// TestRaftWritesStopWhenTheContextEnds checks that the end of the caller's context ends each call and wins over any
// class.
func TestRaftWritesStopWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range raftCalls {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan struct{})
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-got
				cancel()
			}()

			err := c.do(ctx, newClient(t, srv, p, token), raftID)

			checkCallErr(t, err, "nomad: "+c.method+" "+c.path+": context canceled", false, clientTokens(token))
			if !errors.Is(err, context.Canceled) || errors.Is(err, nomadops.ErrGone) {
				t.Errorf("error = %v, want one that matches context.Canceled and not ErrGone", err)
			}
		})
	}
}

// TestRaftWritesOfASlowServer checks that a server that does not answer ends each call at the client's time limit, as
// an error that may succeed later.
func TestRaftWritesOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range raftCalls {
		t.Run(c.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
			cl := newClient(t, srv, p, token)
			cl.SetTimeout(50 * time.Millisecond)

			err := c.do(t.Context(), cl, raftID)

			checkCallErr(t, err, "nomad: "+c.method+" "+c.path+": no answer within 50ms", true, clientTokens(token))
		})
	}
}
