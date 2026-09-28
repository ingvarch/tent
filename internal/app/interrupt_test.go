package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// cloudView returns what f holds, one line per object, sorted, without ids and with the operation ids masked: each
// SSH key, VPC and firewall group by its marker, a group with its rules, and each instance by its hostname, with its
// place, plan, image, state and tags and the names of its firewall group, VPCs and SSH keys. Two copies of an object
// are two lines.
func cloudView(f *vultrfake.Fake) []string {
	nm := names(f)
	named := func(ids []string) string {
		var s []string
		for _, id := range ids {
			s = append(s, nm[id])
		}
		slices.Sort(s)
		return strings.Join(s, ",")
	}
	var view []string
	for _, k := range f.SSHKeys() {
		view = append(view, "ssh key "+k.Name+" "+k.SSHKey)
	}
	for _, v := range f.VPCs() {
		view = append(view, fmt.Sprintf("vpc %s %s %s/%d", v.Description, v.Region, v.V4Subnet, v.V4SubnetMask))
	}
	for _, g := range f.FirewallGroups() {
		var rules []string
		for _, r := range f.FirewallRules(g.ID) {
			rules = append(rules, fmt.Sprintf("%s %s %s/%d %s", r.IPType, r.Protocol, r.Subnet, r.SubnetSize, r.Port))
		}
		slices.Sort(rules)
		view = append(view, "firewall group "+g.Description+": "+strings.Join(rules, "; "))
	}
	for _, in := range f.Instances() {
		req, _ := f.CreateRequest(in.ID)
		view = append(view, fmt.Sprintf("instance %s %s %s os %d %s/%s/%s tags %s firewall %s vpcs %s ssh keys %s",
			in.Hostname, in.Region, in.Plan, in.OsID, in.Status, in.PowerStatus, in.ServerStatus,
			strings.Join(in.Tags, ","), nm[in.FirewallGroupID], named(req.AttachVPC), named(req.SSHKeys)))
	}
	for i, v := range view {
		view[i] = callKey(v)
	}
	slices.Sort(view)
	return view
}

// wantView fails the test unless the cloud of f is want, as cloudView gives it.
func wantView(t *testing.T, f *vultrfake.Fake, want []string) {
	t.Helper()
	if diff := cmp.Diff(want, cloudView(f)); diff != "" {
		t.Errorf("the cloud (-want +got):\n%s", diff)
	}
}

// atCall returns a hook that hands the n-th call whose line, as callKey gives it, is key to act, and carries every
// other call out.
func atCall(f *vultrfake.Fake, key string, n int, act vultrfake.Hook) vultrfake.Hook {
	var mu sync.Mutex
	seen := map[string]int{}
	return func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
		k := callKey(line(c, names(f)))
		mu.Lock()
		seen[k]++
		hit := k == key && seen[k] == n
		mu.Unlock()
		if !hit {
			return next(ctx)
		}
		return act(ctx, c, next)
	}
}

// cutCase cuts a run at one of the calls that an uninterrupted run makes.
type cutCase struct {
	index int    // the call's place among the calls of the uninterrupted run, from 1: its line in the golden file
	key   string // the call, as callKey gives it
	n     int    // the call is the n-th with that key
	after bool   // the run's context ends just after the fake carried the call out; otherwise just before the call
}

// eachCut runs test in a parallel subtest of its own, in a synctest bubble, for each case that cuts a run at one of
// calls, the calls of an uninterrupted run as flowCalls gives them: first just before each call, then just after
// each. A subtest's name gives the call's place and method, such as "after 017 CreateFirewallRule".
func eachCut(t *testing.T, calls []string, test func(t *testing.T, c cutCase)) {
	for _, after := range []bool{false, true} {
		when := "before"
		if after {
			when = "after"
		}
		seen := map[string]int{}
		for i, call := range calls {
			key := callKey(call)
			seen[key]++
			c := cutCase{index: i + 1, key: key, n: seen[key], after: after}
			method, _, _ := strings.Cut(key, " ")
			t.Run(fmt.Sprintf("%s %03d %s", when, c.index, method), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) { test(t, c) })
			})
		}
	}
}

// runCut runs a use case on f with a context that ends at the call of c: just before the call reaches the fake, or
// just after the fake carried it out, when the call fails as the client fails for an ended context and loses its
// answer, as a request in flight does when tent is interrupted. It fails the test unless the run made the call and
// stopped with an error that matches context.Canceled, and changed nothing in the store s.
func runCut(t *testing.T, f *vultrfake.Fake, s statestore.Store, c cutCase, run func(context.Context) error) {
	t.Helper()
	stored := snapshot(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cut atomic.Bool
	f.SetHook(atCall(f, c.key, c.n, func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
		cut.Store(true)
		if c.after {
			_ = next(ctx) // carried out; its answer is lost
		}
		cancel()
		return next(ctx)
	}))
	err := run(ctx)
	f.SetHook(nil)
	if !cut.Load() {
		t.Fatalf("the run made no call %s", c.key)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the run cut at %s returned %v, want an error that matches context.Canceled", c.key, err)
	}
	wantSnapshot(t, s, stored)
	wantLockFree(t, s)
}

// TestUpdateCutAtEveryCall builds the example cluster with a run cut at each of the calls of an uninterrupted build,
// just before it and just after it. The next run, with a fresh context, leaves the cloud as the uninterrupted build
// does, without a second copy of any object, and a run after it has nothing to change.
func TestUpdateCutAtEveryCall(t *testing.T) {
	_, calls, want := buildExample(t)
	_, template := exampleStore(t)
	eachCut(t, calls, func(t *testing.T, c cutCase) {
		svc, f := newExampleFrom(t, template)
		runCut(t, f, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.Update(ctx, "prod", true)
			return err
		})
		mustUpdate(t, svc)
		wantView(t, f, want)
		wantConverged(t, svc)
		wantLockFree(t, svc.Store)
	})
}

// TestDeleteCutAtEveryCall deletes the built example cluster with a run cut at each of the calls of an uninterrupted
// delete, just before it and just after it. The next run, with a fresh context, deletes what is left of the cluster
// and its state, and then the cluster is not found.
func TestDeleteCutAtEveryCall(t *testing.T) {
	calls := deleteExample(t)
	_, template := exampleStore(t)
	eachCut(t, calls, func(t *testing.T, c cutCase) {
		svc, f := newExampleFrom(t, template)
		mustUpdate(t, svc)
		runCut(t, f, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.DeleteCluster(ctx, "prod", true, false)
			return err
		})
		mustDelete(t, svc)
		if left := owned(f, "prod"); len(left) != 0 {
			t.Errorf("the cloud still holds objects of cluster prod: %v", left)
		}
		wantPaths(t, svc.Store)
		wantLockFree(t, svc.Store)
		_, err := svc.DeleteCluster(t.Context(), "prod", false, false)
		wantError(t, err, notFound(svc, "cluster prod"))
	})
}

// errLost is why a call whose answer a test lost got none.
var errLost = errors.New("the test lost the answer")

// TestUpdateLosesTheAnswerOfEveryCreate builds the example cluster with the answer to one of its creates lost, for
// each create of an uninterrupted build: the fake carries the call out and the call fails with no answer, as
// LoseResponse makes the next call of a method fail. The update finds what the call made by its operation id, or
// lists the rules again, and finishes in the same run with one copy of every object and one instance per node. A
// subtest's name gives the call's place in the build and its method.
func TestUpdateLosesTheAnswerOfEveryCreate(t *testing.T) {
	_, calls, want := buildExample(t)
	_, template := exampleStore(t)
	for i, call := range calls {
		method, _, _ := strings.Cut(call, " ")
		if !strings.HasPrefix(method, "Create") {
			continue
		}
		key := callKey(call)
		t.Run(fmt.Sprintf("%03d %s", i+1, method), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f := newExampleFrom(t, template)
				var lost atomic.Bool
				f.SetHook(atCall(f, key, 1,
					func(ctx context.Context, c vultrfake.Call, next func(context.Context) error) error {
						lost.Store(true)
						_ = next(ctx)
						return vultr.NewNoAnswerError(c.Name, c.Arg, errLost)
					}))
				mustUpdate(t, svc)
				f.SetHook(nil)
				if !lost.Load() {
					t.Fatalf("the update made no call %s", key)
				}
				wantView(t, f, want)
				wantConverged(t, svc)
			})
		})
	}
}
