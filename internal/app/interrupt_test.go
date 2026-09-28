package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/secrettest"
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
// stopped with an error that matches context.Canceled, and changed nothing in the store s but for writing secrets that
// s lacked, as an update does before the infrastructure. It returns the secrets that the run wrote.
func runCut(t *testing.T, f *vultrfake.Fake, s statestore.Store, c cutCase, run func(context.Context) error) (
	wrote map[string][]byte,
) {
	t.Helper()
	stored, before := snapshot(t, s), secretsOf(t, s)
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
	wrote = map[string][]byte{}
	for p, data := range secretsOf(t, s) {
		if _, ok := before[p]; !ok {
			wrote[p], stored[p] = data, hidden(string(data))
		}
	}
	wantSnapshot(t, s, stored)
	wantLockFree(t, s)
	return wrote
}

// wantSecretsKept fails the test unless the store s holds the test cluster's four secrets, with a CA bundle of one
// certificate that matches the CA key, and holds the secrets of kept byte for byte. Its messages show the paths, never
// the secrets.
func wantSecretsKept(t *testing.T, s statestore.Store, kept map[string][]byte) {
	t.Helper()
	got := secretsOf(t, s)
	if len(got) != len(secretPaths) {
		t.Fatalf("the store holds %d secrets, want %d", len(got), len(secretPaths))
	}
	storedCA(t, s)
	for p, data := range kept {
		if !bytes.Equal(got[p], data) {
			t.Errorf("%s is not the one that the cut run wrote", p)
		}
	}
}

// TestUpdateCutAtEveryCall builds the example cluster with a run cut at each of the calls of an uninterrupted build,
// just before it and just after it. The next run, with a fresh context, leaves the cloud as the uninterrupted build
// does, without a second copy of any object, keeps the secrets that the cut run wrote, and a run after it has nothing
// to change.
func TestUpdateCutAtEveryCall(t *testing.T) {
	_, calls, want := buildExample(t)
	_, template := exampleStore(t)
	eachCut(t, calls, func(t *testing.T, c cutCase) {
		svc, f := newExampleFrom(t, template)
		wrote := runCut(t, f, svc.Store, c, func(ctx context.Context) error {
			_, err := svc.Update(ctx, "prod", true)
			return err
		})
		mustUpdate(t, svc)
		wantView(t, f, want)
		wantSecretsKept(t, svc.Store, wrote)
		wantConverged(t, svc)
		wantLockFree(t, svc.Store)
	})
}

// errCut is why a put or a delete that a test cut failed.
var errCut = errors.New("the test cut the call")

// cutStore fails the first put or delete of path: just before the call reaches the store, or just after the store
// carried it out, as a call whose answer is lost. It records the secrets it receives.
type cutStore struct {
	statestore.Store
	sent
	path  string
	after bool
	cut   atomic.Bool
}

func (s *cutStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	s.record(p, data)
	var v statestore.Version
	err := s.call("put", p, func() (err error) {
		v, err = s.Store.Put(ctx, p, data, opts)
		return err
	})
	if err != nil {
		return "", err
	}
	return v, nil
}

func (s *cutStore) Delete(ctx context.Context, p string) error {
	return s.call("delete", p, func() error { return s.Store.Delete(ctx, p) })
}

// call carries out the call op of p, or cuts it when it is the first call of the store's path.
func (s *cutStore) call(op, p string, carry func() error) error {
	if p != s.path || s.cut.Swap(true) {
		return carry()
	}
	if s.after {
		if err := carry(); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s %q: %w", op, p, errCut)
}

// TestUpdateCutAtEverySecretWrite builds the example cluster with a run cut at each write of a secret, just before
// the store writes it and just after, when the answer is lost. The cut run stops before the infrastructure, with an
// error that shows none of the secrets it made. The next run leaves the cloud as an uninterrupted build does, with one
// CA, one gossip key and one ACL bootstrap secret, and keeps each secret that the cut run wrote byte for byte.
func TestUpdateCutAtEverySecretWrite(t *testing.T) {
	_, _, want := buildExample(t)
	_, template := exampleStore(t)
	for _, after := range []bool{false, true} {
		when := "before"
		if after {
			when = "after"
		}
		for _, p := range secretPaths {
			t.Run(when+" "+path.Base(p), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					svc, f := newExampleFrom(t, template)
					store := svc.Store
					cut := &cutStore{Store: store, path: p, after: after}
					svc.Store = cut

					_, err := svc.Update(t.Context(), "prod", true)
					if err == nil {
						t.Fatal("the cut run succeeded")
					}
					shown := map[string]string{"the error": err.Error()}
					if secrettest.CheckHidden(t, shown, cut.received(t, p), ""); t.Failed() {
						t.FailNow() // the error would show a secret
					}
					if !errors.Is(err, errCut) {
						t.Fatalf("the cut run returned %v, want an error that matches %v", err, errCut)
					}
					wantOnlyReads(t, f)
					wantLockFree(t, cut)
					wrote := secretsOf(t, store)
					if _, ok := wrote[p]; ok != after {
						t.Errorf("the cut run wrote %s: %t, want %t", p, ok, after)
					}

					svc.Store = store
					mustUpdate(t, svc)
					wantView(t, f, want)
					wantSecretsKept(t, store, wrote)
					wantConverged(t, svc)
				})
			})
		}
	}
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

// TestDeleteCutAtEveryStateDelete deletes the built test cluster with a run cut at each delete of its state, just
// before the store deletes the object and just after, when the answer is lost. The cut run has deleted the objects
// before the cut, in the order of deletion, and never leaves a CA bundle without its key. When it leaves the CA key
// without its bundle, an update, once the node groups are back, signs a new bundle for the stored key and keeps the
// key. The next delete deletes the rest.
func TestDeleteCutAtEveryStateDelete(t *testing.T) {
	for _, after := range []bool{false, true} {
		when := "before"
		if after {
			when = "after"
		}
		for i, p := range builtState {
			t.Run(when+" "+path.Base(p), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					svc, f := newBuilt(t)
					store := svc.Store
					cut := &cutStore{Store: store, path: p, after: after}
					svc.Store = cut

					if _, err := svc.DeleteCluster(t.Context(), "prod", true, false); !errors.Is(err, errCut) {
						t.Fatalf("the cut run returned %v, want an error that matches %v", err, errCut)
					}
					wantLockFree(t, cut)
					left := builtState[i:]
					if after {
						left = builtState[i+1:]
					}
					wantPaths(t, store, slices.Sorted(slices.Values(left))...)
					stored := list(t, store, "")
					hasKey, hasBundle := slices.Contains(stored, caKeyPath), slices.Contains(stored, caBundlePath)
					if hasBundle && !hasKey {
						t.Error("the cut run left the CA bundle without its key")
					}

					svc.Store = store
					if hasKey && !hasBundle {
						key := get(t, store, caKeyPath)
						mustCreate(t, svc, serversYAML, workersYAML)
						plan := mustUpdate(t, svc)
						if diff := cmp.Diff(secretNames[1:], plan.Secrets); diff != "" {
							t.Errorf("the update's secrets (-want +got):\n%s", diff)
						}
						if !bytes.Equal(get(t, store, caKeyPath), key) {
							t.Error("the update replaced the CA key")
						}
						storedCA(t, store)
					}
					if len(stored) == 0 {
						_, err := svc.DeleteCluster(t.Context(), "prod", true, false)
						wantError(t, err, notFound(svc, "cluster prod"))
						return
					}
					mustDelete(t, svc)
					wantPaths(t, store)
					if objs := owned(f, "prod"); len(objs) != 0 {
						t.Errorf("the cloud still holds objects of cluster prod: %v", objs)
					}
				})
			})
		}
	}
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
