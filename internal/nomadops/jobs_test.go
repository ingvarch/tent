package nomadops_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// jobsRequest is the request of Jobs: the jobs of every namespace.
var jobsRequest = gotRequest{Method: http.MethodGet, Path: "/v1/jobs", Query: "namespace=%2A&region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// jobsJSON is how Nomad lists a job of the default namespace and a stopped job of a second one.
const jobsJSON = `[
{"ID":"web","ParentID":"","Name":"web","Namespace":"default","Type":"service","Status":"running","Stop":false,
 "JobModifyIndex":12},
{"ID":"batch","ParentID":"","Name":"batch","Namespace":"dev","Type":"batch","Status":"dead","Stop":true,
 "JobModifyIndex":15}
]`

func TestJobs(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, jobsJSON))
	got, err := newClient(t, srv, p, token).Jobs(t.Context())
	if err != nil {
		t.Fatalf("Jobs: %s", show(t, err, clientTokens(token)))
	}
	want := []nomadops.Job{{Namespace: "default", ID: "web"}, {Namespace: "dev", ID: "batch"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Jobs() (-want +got):\n%s", diff)
	}
	checkRequests(t, srv, clientTokens(token), jobsRequest)
}

// TestJobsSkipsNullAndListsNone checks that an empty list gives no jobs and that a null in the list is left out.
func TestJobsSkipsNullAndListsNone(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, tc := range []struct {
		name, body string
		want       []nomadops.Job
	}{
		{"an empty list", `[]`, nil},
		{"null alone", `[null]`, nil},
		{"null around jobs", `[null,{"ID":"a","Namespace":"default"},null,{"ID":"b","Namespace":"dev"}]`,
			[]nomadops.Job{{Namespace: "default", ID: "a"}, {Namespace: "dev", ID: "b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, tc.body))
			got, err := newClient(t, srv, p, token).Jobs(t.Context())
			if err != nil {
				t.Fatalf("Jobs: %s", show(t, err, clientTokens(token)))
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Jobs() (-want +got):\n%s", diff)
			}
			if got == nil {
				t.Error("Jobs() = nil, want a list that is empty")
			}
		})
	}
}

// TestJobsErrors checks the class of each answer that a read of the jobs can get.
func TestJobsErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		want     string
		notReady bool
	}{
		{"no leader", answer(http.StatusInternalServerError, "No cluster leader"),
			"nomad: GET /v1/jobs: 500: No cluster leader", true},
		{"rate limited", answer(http.StatusTooManyRequests, "slow down"), "nomad: GET /v1/jobs: 429: slow down", true},
		{"no permission", answer(http.StatusForbidden, "Permission denied"),
			"nomad: GET /v1/jobs: 403: Permission denied", false},
		{"an answer cut short", cutAnswer(t), "nomad: GET /v1/jobs: unexpected EOF", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), tc.handler)
			got, err := newClient(t, srv, p, token).Jobs(t.Context())
			checkCallErr(t, err, tc.want, tc.notReady, clientTokens(token))
			if got != nil {
				t.Errorf("Jobs() = %v with the error, want nil", got)
			}
			checkRequests(t, srv, clientTokens(token), jobsRequest)
		})
	}
}

// TestJobsOfASlowServer checks that a server that does not answer ends the call at the client's time limit, as an
// error that may succeed later.
func TestJobsOfASlowServer(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
	cl := newClient(t, srv, p, token)
	cl.SetTimeout(50 * time.Millisecond)

	_, err := cl.Jobs(t.Context())

	checkCallErr(t, err, "nomad: GET /v1/jobs: no answer within 50ms", true, clientTokens(token))
}

// TestJobsStopsWhenTheContextEnds checks that the end of the caller's context ends the call.
func TestJobsStopsWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	got := make(chan struct{})
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-got
		cancel()
	}()

	_, err := newClient(t, srv, p, token).Jobs(ctx)

	checkCallErr(t, err, "nomad: GET /v1/jobs: context canceled", false, clientTokens(token))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one that matches context.Canceled", err)
	}
}
