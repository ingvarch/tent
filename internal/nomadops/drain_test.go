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

// nodeID is the ID of a node in the tests, a UUID as Nomad makes them.
const nodeID = "3c2d1e4b-0a5f-4d6e-8b7c-9a0b1c2d3e4f"

// drainReq asks for a drain of one hour with the meta of a machine.
var drainReq = nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": "m-1"}}

// nodeCall is one of the writes on a node, with the request that it makes.
type nodeCall struct {
	name   string // the method
	suffix string // what follows /v1/node/<id>/ in the path
	body   string // the JSON body of the request, "" for none
	do     func(context.Context, nomadops.API, string) error
	// goneIsDone is set when a node that is gone counts as success.
	goneIsDone bool
}

// nodeCalls are MarkIneligible, Drain and Purge, called with drainReq.
var nodeCalls = []nodeCall{
	{name: "MarkIneligible", suffix: "eligibility", body: `{"NodeID":"` + nodeID + `","Eligibility":"ineligible"}` + "\n",
		do: func(ctx context.Context, a nomadops.API, id string) error { return a.MarkIneligible(ctx, id) }},
	{name: "Drain", suffix: "drain",
		body: `{"NodeID":"` + nodeID + `","DrainSpec":{"Deadline":3600000000000,"IgnoreSystemJobs":false},` +
			`"MarkEligible":false,"Meta":{"tent_machine":"m-1"}}` + "\n",
		do: func(ctx context.Context, a nomadops.API, id string) error { return a.Drain(ctx, id, drainReq) }},
	{name: "Purge", suffix: "purge", goneIsDone: true,
		do: func(ctx context.Context, a nomadops.API, id string) error { return a.Purge(ctx, id) }},
}

// request is the request of the call for the node.
func (c nodeCall) request(id string) gotRequest {
	return gotRequest{Method: http.MethodPut, Path: "/v1/node/" + id + "/" + c.suffix, Query: "region=" + region,
		Token: clientToken, Peer: "cli." + region + ".nomad", Body: c.body}
}

// TestNodeWritesOverMTLS checks the request of each write: method, path, query, body, token and client certificate.
func TestNodeWritesOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range nodeCalls {
		t.Run(c.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK,
				`{"NodeModifyIndex":34,"EvalIDs":null,"EvalCreateIndex":0,"Index":34}`))

			err := c.do(t.Context(), newClient(t, srv, p, token), nodeID)

			if err != nil {
				t.Fatalf("%s: %s", c.name, show(t, err, clientTokens(token)))
			}
			checkRequests(t, srv, clientTokens(token), c.request(nodeID))
		})
	}
}

// TestDrainSendsNoMetaWhenThereIsNone checks the body of a drain without meta.
func TestDrainSendsNoMetaWhenThereIsNone(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))

	err := newClient(t, srv, p, token).Drain(t.Context(), nodeID, nomadops.DrainRequest{Deadline: 90 * time.Second})

	if err != nil {
		t.Fatalf("Drain: %s", show(t, err, clientTokens(token)))
	}
	want := nodeCalls[1].request(nodeID)
	want.Body = `{"NodeID":"` + nodeID + `","DrainSpec":{"Deadline":90000000000,"IgnoreSystemJobs":false},` +
		`"MarkEligible":false,"Meta":null}` + "\n"
	checkRequests(t, srv, clientTokens(token), want)
}

// TestCheckNodeID checks the IDs that CheckNodeID accepts.
func TestCheckNodeID(t *testing.T) {
	for _, id := range []string{
		nodeID, "N-1", "abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789", "-", "0",
	} {
		if err := nomadops.CheckNodeID(id); err != nil {
			t.Errorf("CheckNodeID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"`", "{", "@", "[", "/", ":", "_", "."} { // the neighbours of the ranges, and "_" and "."
		if err := nomadops.CheckNodeID("a" + id); err == nil {
			t.Errorf("CheckNodeID(%q) = nil, want an error", "a"+id)
		}
	}
}

// TestNodeWritesRefuseABadIDBeforeAnyRequest checks that an empty ID and one that holds more than ASCII letters,
// digits and "-" fail without a request, since the path would take them as they are.
func TestNodeWritesRefuseABadIDBeforeAnyRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	c := newClient(t, srv, p, token)
	for _, tc := range []struct{ name, id, want string }{
		{"empty", "", "nomad: no node ID"},
		{"a slash", "a/b", `nomad: node ID "a/b" has a character other than an ASCII letter, a digit or "-"`},
		{"a dot segment", "..", `nomad: node ID ".." has a character other than an ASCII letter, a digit or "-"`},
		{"a question mark", "a?b", `nomad: node ID "a?b" has a character other than an ASCII letter, a digit or "-"`},
		{"a space", "a b", `nomad: node ID "a b" has a character other than an ASCII letter, a digit or "-"`},
		{"a letter that is not ASCII", "é",
			`nomad: node ID "é" has a character other than an ASCII letter, a digit or "-"`},
		{"a line end", nodeID + "\n", `nomad: node ID "` + nodeID + `\n" has a character other than an ASCII letter, ` +
			`a digit or "-"`},
	} {
		for _, call := range nodeCalls {
			t.Run(call.name+" "+tc.name, func(t *testing.T) {
				checkCallErr(t, call.do(t.Context(), c, tc.id), tc.want, false, clientTokens(token))
			})
		}
	}
	checkRequests(t, srv, clientTokens(token))
}

// TestDrainRefusesABadDeadlineBeforeAnyRequest checks that a deadline of 0, which Nomad reads as no deadline, and a
// negative one, which stops everything at once, fail without a request.
func TestDrainRefusesABadDeadlineBeforeAnyRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, "{}"))
	c := newClient(t, srv, p, token)
	for deadline, want := range map[time.Duration]string{
		0:            "nomad: drain: deadline 0s is not above zero",
		-time.Second: "nomad: drain: deadline -1s is not above zero",
	} {
		err := c.Drain(t.Context(), nodeID, nomadops.DrainRequest{Deadline: deadline})
		checkCallErr(t, err, want, false, clientTokens(token))
	}
	checkRequests(t, srv, clientTokens(token))
}

// TestDrainRequestCheck checks that Check accepts a deadline above zero and refuses 0 and a negative one.
func TestDrainRequestCheck(t *testing.T) {
	for _, tc := range []struct {
		deadline time.Duration
		want     string
	}{
		{time.Hour, ""},
		{time.Nanosecond, ""},
		{0, "drain: deadline 0s is not above zero"},
		{-time.Hour, "drain: deadline -1h0m0s is not above zero"},
	} {
		err := nomadops.DrainRequest{Deadline: tc.deadline}.Check()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("Check() with %s = %v, want nil", tc.deadline, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("Check() with %s = %v, want %q", tc.deadline, err, tc.want)
		}
	}
}

// TestNodeWritesOfAGoneNode checks the class of each answer that Nomad gives for a node: gone is ErrGone, and success
// for a purge; the answers that only look like it keep their class.
func TestNodeWritesOfAGoneNode(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	const (
		gone      = "gone"
		notReady  = "not ready"
		permanent = "permanent"
		notFound  = "node not found"
	)
	cases := []struct {
		name   string
		status int
		body   string
		class  string
	}{
		{"the leader's answer", http.StatusInternalServerError, notFound, gone},
		{"a follower's answer", http.StatusInternalServerError, "rpc error: " + notFound, gone},
		{"an answer forwarded twice", http.StatusInternalServerError, "rpc error: rpc error: " + notFound, gone},
		{"an answer with a line end", http.StatusInternalServerError, notFound + "\n", gone},
		{"more words after it", http.StatusInternalServerError, notFound + " yet", notReady},
		{"more words before it", http.StatusInternalServerError, "no rpc error: " + notFound, notReady},
		{"the text with another status", http.StatusBadRequest, notFound, permanent},
		{"the text with a 404", http.StatusNotFound, notFound, permanent},
		{"no leader", http.StatusInternalServerError, "No cluster leader", notReady},
		{"the server is not ready", http.StatusServiceUnavailable, notFound, notReady},
		{"too many requests", http.StatusTooManyRequests, notFound, notReady},
		{"no permission", http.StatusForbidden, "Permission denied", permanent},
	}
	for _, c := range nodeCalls {
		for _, tc := range cases {
			t.Run(c.name+" "+tc.name, func(t *testing.T) {
				srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))

				err := c.do(t.Context(), newClient(t, srv, p, token), nodeID)

				text := show(t, err, clientTokens(token))
				if tc.class == gone && c.goneIsDone {
					if err != nil {
						t.Errorf("%s: %s, want success for a node that is gone", c.name, text)
					}
					return
				}
				if err == nil {
					t.Fatalf("%s: no error, want a %s one", c.name, tc.class)
				}
				wantText := "nomad: PUT /v1/node/" + nodeID + "/" + c.suffix + ": " + strconv.Itoa(tc.status) + ": " +
					strings.Join(strings.Fields(tc.body), " ")
				if text != wantText {
					t.Errorf("error = %s, want %q", text, wantText)
				}
				if got := errors.Is(err, nomadops.ErrGone); got != (tc.class == gone) {
					t.Errorf("errors.Is(%s, ErrGone) = %v, want %v", text, got, !got)
				}
				if got := errors.Is(err, nomadops.ErrNotReady); got != (tc.class == notReady) {
					t.Errorf("errors.Is(%s, ErrNotReady) = %v, want %v", text, got, !got)
				}
				if n := len(srv.requests()); n != 1 {
					t.Errorf("the server got %d requests, want 1", n)
				}
			})
		}
	}
}

// TestNodeWritesStopWhenTheContextEnds checks that the end of the caller's context ends each call and wins over any
// class.
func TestNodeWritesStopWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range nodeCalls {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan struct{})
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-got
				cancel()
			}()

			err := c.do(ctx, newClient(t, srv, p, token), nodeID)

			checkCallErr(t, err, "nomad: PUT /v1/node/"+nodeID+"/"+c.suffix+": context canceled", false, clientTokens(token))
			if !errors.Is(err, context.Canceled) || errors.Is(err, nomadops.ErrGone) {
				t.Errorf("error = %v, want one that matches context.Canceled and not ErrGone", err)
			}
		})
	}
}

// TestNodeWritesOfASlowServer checks that a server that does not answer ends each call at the client's time limit, as
// an error that may succeed later.
func TestNodeWritesOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range nodeCalls {
		t.Run(c.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
			cl := newClient(t, srv, p, token)
			cl.SetTimeout(50 * time.Millisecond)

			err := c.do(t.Context(), cl, nodeID)

			checkCallErr(t, err, "nomad: PUT /v1/node/"+nodeID+"/"+c.suffix+": no answer within 50ms", true,
				clientTokens(token))
		})
	}
}
