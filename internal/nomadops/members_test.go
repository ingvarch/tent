package nomadops_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// membersRequest is the request of Members.
var membersRequest = gotRequest{Method: http.MethodGet, Path: "/v1/agent/members", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// forceLeaveRequest is the request of ForceLeave for the member, with the query that Nomad's API module sorts.
func forceLeaveRequest(query string) gotRequest {
	return gotRequest{Method: http.MethodPut, Path: "/v1/agent/force-leave", Query: query + "&region=" + region,
		Token: clientToken, Peer: "cli." + region + ".nomad"}
}

// membersJSON is how Nomad shows the gossip pool of three servers: one alive, one failed and one that left.
const membersJSON = `{"ServerName":"s1","ServerRegion":"eu","ServerDC":"dc1","Members":[
{"Name":"s1.eu","Addr":"10.64.0.3","Port":4648,"Tags":{"id":"6d1f1e2a","role":"nomad","region":"eu"},
 "Status":"alive","ProtocolMin":1,"ProtocolMax":5,"ProtocolCur":2},
{"Name":"s2.eu","Addr":"10.64.0.4","Port":4648,"Tags":{"id":"7e2a2f3b"},"Status":"failed"},
{"Name":"s3.eu","Addr":"fd00::5","Port":4648,"Tags":null,"Status":"left"}]}`

func TestMembers(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name string
		body string
		want []nomadops.Member
	}{
		{"three members", membersJSON, []nomadops.Member{
			{Name: "s1.eu", Address: netip.MustParseAddr("10.64.0.3"), Status: "alive"},
			{Name: "s2.eu", Address: netip.MustParseAddr("10.64.0.4"), Status: "failed"},
			{Name: "s3.eu", Address: netip.MustParseAddr("fd00::5"), Status: "left"},
		}},
		{"an address that does not parse, and a null element",
			`{"Members":[{"Name":"s1.eu","Addr":"nonsense","Status":"alive"},null,{"Name":"s2.eu","Status":"leaving"}]}`,
			[]nomadops.Member{{Name: "s1.eu", Status: "alive"}, {Name: "s2.eu", Status: "leaving"}}},
		{"no members", `{"Members":[]}`, []nomadops.Member{}},
		{"a null answer", `null`, []nomadops.Member{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, tc.body))
			got, err := newClient(t, srv, p, token).Members(t.Context())
			if err != nil {
				t.Fatalf("Members: %s", show(t, err, clientTokens(token)))
			}
			if diff := cmp.Diff(tc.want, got, equateAddrs); diff != "" {
				t.Errorf("Members() (-want +got):\n%s", diff)
			}
			checkRequests(t, srv, clientTokens(token), membersRequest)
		})
	}
}

// gossipCall is Members or ForceLeave, with the request that it makes.
type gossipCall struct {
	name, method, path string
	do                 func(context.Context, nomadops.API) error
}

// gossipCalls are Members and ForceLeave of "s1.eu".
var gossipCalls = []gossipCall{
	{"Members", http.MethodGet, "/v1/agent/members", func(ctx context.Context, a nomadops.API) error {
		got, err := a.Members(ctx)
		if err != nil && got != nil {
			return fmt.Errorf("Members() = %v with the error %w, want nil", got, err)
		}
		return err
	}},
	{"ForceLeave", http.MethodPut, "/v1/agent/force-leave", func(ctx context.Context, a nomadops.API) error {
		return a.ForceLeave(ctx, "s1.eu")
	}},
}

// TestMembersAndForceLeaveErrors checks the class of each answer that a call of the gossip pool can get.
func TestMembersAndForceLeaveErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		status   int
		body     string
		notReady bool
	}{
		{"no leader", http.StatusInternalServerError, "No cluster leader", true},
		{"rate limited", http.StatusTooManyRequests, "slow down", true},
		{"no permission", http.StatusForbidden, "Permission denied", false},
		{"a bad request", http.StatusBadRequest, "bad", false},
		{"not found", http.StatusNotFound, "node not found", false},
	}
	for _, c := range gossipCalls {
		for _, tc := range cases {
			t.Run(c.name+" "+tc.name, func(t *testing.T) {
				srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))

				err := c.do(t.Context(), newClient(t, srv, p, token))

				want := "nomad: " + c.method + " " + c.path + ": " + strconv.Itoa(tc.status) + ": " + tc.body
				checkCallErr(t, err, want, tc.notReady, clientTokens(token))
				if errors.Is(err, nomadops.ErrGone) {
					t.Errorf("error = %v matches ErrGone, want no gone class for the gossip pool", err)
				}
				if n := len(srv.requests()); n != 1 {
					t.Errorf("the server got %d requests, want 1", n)
				}
			})
		}
	}
}

func TestForceLeaveOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name, member, query string
	}{
		{"a member", "prod-servers-1.eu", "node=prod-servers-1.eu&prune=1"},
		{"a name with characters of a query", "a b&c=d.eu", "node=a+b%26c%3Dd.eu&prune=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))

			err := newClient(t, srv, p, token).ForceLeave(t.Context(), tc.member)

			if err != nil {
				t.Fatalf("ForceLeave: %s", show(t, err, clientTokens(token)))
			}
			checkRequests(t, srv, clientTokens(token), forceLeaveRequest(tc.query))
		})
	}
}

// TestForceLeaveRefusesABadNameBeforeAnyRequest checks that an empty name and one without a "." fail without a
// request: Nomad answers 200 to a name without its region and does nothing.
func TestForceLeaveRefusesABadNameBeforeAnyRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))
	c := newClient(t, srv, p, token)
	for _, tc := range []struct{ name, member, want string }{
		{"empty", "", "nomad: no member name"},
		{"no region", "prod-servers-1", `nomad: member name "prod-servers-1" has no "."`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkCallErr(t, c.ForceLeave(t.Context(), tc.member), tc.want, false, clientTokens(token))
		})
	}
	checkRequests(t, srv, clientTokens(token))
}

// TestMembersAndForceLeaveStopWhenTheContextEnds checks that the end of the caller's context ends each call.
func TestMembersAndForceLeaveStopWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range gossipCalls {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan struct{})
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-got
				cancel()
			}()

			err := c.do(ctx, newClient(t, srv, p, token))

			checkCallErr(t, err, "nomad: "+c.method+" "+c.path+": context canceled", false, clientTokens(token))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want one that matches context.Canceled", err)
			}
		})
	}
}

// TestMembersAndForceLeaveOfASlowServer checks that a server that does not answer ends each call at the client's time
// limit, as an error that may succeed later.
func TestMembersAndForceLeaveOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range gossipCalls {
		t.Run(c.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
			cl := newClient(t, srv, p, token)
			cl.SetTimeout(50 * time.Millisecond)

			err := c.do(t.Context(), cl)

			checkCallErr(t, err, "nomad: "+c.method+" "+c.path+": no answer within 50ms", true, clientTokens(token))
		})
	}
}
