package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	// webJob and webService are the job and the service that testdata/web.nomad.hcl defines.
	webJob     = "e2e-web"
	webService = "e2e-web"
)

// jobAllocation is an allocation of a job, as /v1/job/<id>/allocations lists it.
type jobAllocation struct {
	ID           string
	ClientStatus string
}

// allocationCheck is a check of an allocation, as /v1/allocation/<id>/checks answers.
type allocationCheck struct {
	Service string
	Status  string
}

// serviceRegistration is a registration of a Nomad service, as /v1/service/<name> lists it.
type serviceRegistration struct {
	AllocID string
}

// serviceAnswers is what the three calls on a job's service answered.
type serviceAnswers struct {
	Allocations []jobAllocation
	// Checks holds the checks of the running allocations by allocation ID, then by check ID.
	Checks        map[string]map[string]allocationCheck
	Registrations []serviceRegistration
}

// missing returns what keeps service from being ready, or nil when it is ready: one running allocation that has a
// successful check of the service and a registration of it. Of several running allocations it names the gaps of the
// first one with the fewest.
func (a serviceAnswers) missing(service string) []string {
	var best []string
	found := false
	for _, alloc := range a.Allocations {
		if alloc.ClientStatus != "running" {
			continue
		}
		var gaps []string
		if !a.hasSuccessfulCheck(alloc.ID, service) {
			gaps = append(gaps, "a successful check of "+service)
		}
		if !a.hasRegistration(alloc.ID) {
			gaps = append(gaps, "a registration of "+service)
		}
		if len(gaps) == 0 {
			return nil
		}
		if !found || len(gaps) < len(best) {
			best, found = gaps, true
		}
	}
	if !found {
		return []string{"a running allocation"}
	}
	return best
}

func (a serviceAnswers) hasSuccessfulCheck(allocID, service string) bool {
	for _, c := range a.Checks[allocID] {
		if c.Service == service && c.Status == "success" {
			return true
		}
	}
	return false
}

func (a serviceAnswers) hasRegistration(allocID string) bool {
	return slices.ContainsFunc(a.Registrations, func(r serviceRegistration) bool { return r.AllocID == allocID })
}

// String quotes the answers in short: allocation IDs have 8 characters, checks follow their IDs' order.
func (a serviceAnswers) String() string {
	var allocs, checks, regs []string
	for _, alloc := range a.Allocations {
		allocs = append(allocs, shortID(alloc.ID)+" "+alloc.ClientStatus)
		ids := make([]string, 0, len(a.Checks[alloc.ID]))
		for id := range a.Checks[alloc.ID] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			c := a.Checks[alloc.ID][id]
			checks = append(checks, shortID(alloc.ID)+" "+c.Service+" "+c.Status)
		}
	}
	for _, r := range a.Registrations {
		regs = append(regs, shortID(r.AllocID))
	}
	return fmt.Sprintf("allocations: %s; checks: %s; registrations: %s", listOrNone(allocs), listOrNone(checks),
		listOrNone(regs))
}

func shortID(id string) string {
	return id[:min(len(id), 8)]
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// readServiceAnswers asks Nomad for the allocations of job, the checks of each running one and the registrations of
// service. On an error it returns what it had read until then.
func readServiceAnswers(ctx context.Context, n *nomadAPI, job, service string) (serviceAnswers, error) {
	var a serviceAnswers
	if err := n.get(ctx, "/v1/job/"+url.PathEscape(job)+"/allocations", &a.Allocations); err != nil {
		return a, err
	}
	a.Checks = map[string]map[string]allocationCheck{}
	for _, alloc := range a.Allocations {
		if alloc.ClientStatus != "running" {
			continue
		}
		var checks map[string]allocationCheck
		if err := n.get(ctx, "/v1/allocation/"+url.PathEscape(alloc.ID)+"/checks", &checks); err != nil {
			return a, err
		}
		a.Checks[alloc.ID] = checks
	}
	err := n.get(ctx, "/v1/service/"+url.PathEscape(service), &a.Registrations)
	return a, err
}

// waitService reads the answers about service every `every` until it is ready or timeout has passed.
func waitService(ctx context.Context, n *nomadAPI, job, service string, every, timeout time.Duration) error {
	return pollUntil(ctx, every, timeout, func(ctx context.Context) string {
		return serviceProblem(ctx, n, job, service)
	})
}

// serviceProblem returns "" when service is ready; otherwise what is missing, or the error of a call, with the
// answers read so far in short.
func serviceProblem(ctx context.Context, n *nomadAPI, job, service string) string {
	a, err := readServiceAnswers(ctx, n, job, service)
	if err != nil {
		return fmt.Sprintf("%v; last answers: %s", err, a)
	}
	gaps := a.missing(service)
	if len(gaps) == 0 {
		return ""
	}
	return fmt.Sprintf("missing %s; last answers: %s", strings.Join(gaps, " and "), a)
}

// submitJob has Nomad parse the HCL text of a job and then registers the parsed job.
func submitJob(ctx context.Context, n *nomadAPI, text string) error {
	var parsed json.RawMessage
	in := map[string]any{"JobHCL": text, "Canonicalize": true}
	if err := n.post(ctx, "/v1/jobs/parse", in, &parsed); err != nil {
		return fmt.Errorf("parse the job: %w", err)
	}
	if err := n.post(ctx, "/v1/jobs", map[string]any{"Job": parsed}, nil); err != nil {
		return fmt.Errorf("submit the job: %w", err)
	}
	return nil
}

// purgeJob stops a job and removes it from Nomad's state.
func purgeJob(ctx context.Context, n *nomadAPI, job string) error {
	return n.del(ctx, "/v1/job/"+url.PathEscape(job)+"?purge=true")
}
