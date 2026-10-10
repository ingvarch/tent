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

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// cloudView returns what f holds, one line per object, sorted, without ids and with the operation ids masked: each
// SSH key, VPC and firewall group by its marker, a group with its rules, and each instance by its hostname, with its
// place, plan, image, state and tags and the names of its firewall group, VPCs and SSH keys, and whether its user data
// is the stub. Two copies of an object are two lines.
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
		view = append(view, fmt.Sprintf(
			"instance %s %s %s os %d %s/%s/%s tags %s firewall %s vpcs %s ssh keys %s user data %s",
			in.Hostname, in.Region, in.Plan, in.OsID, in.Status, in.PowerStatus, in.ServerStatus,
			strings.Join(in.Tags, ","), nm[in.FirewallGroupID], named(req.AttachVPC), named(req.SSHKeys),
			userDataView(f, in.ID)))
	}
	for i, v := range view {
		view[i] = specHashPattern.ReplaceAllString(callKey(v), cloud.LabelSpecHash+"=<hash>")
	}
	slices.Sort(view)
	return view
}

// userDataView is "scrubbed" when the instance id in f holds the stub as its user data, and "kept" otherwise.
func userDataView(f *vultrfake.Fake, id string) string {
	if isScrubbed(f, id) {
		return "scrubbed"
	}
	return "kept"
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

// atNomadCall returns a hook that hands the n-th call whose line, as the world's line gives it, is key to act, and
// carries every other call out.
func atNomadCall(w *nomadWorld, key string, n int, act nomadHook) nomadHook {
	var mu sync.Mutex
	seen := map[string]int{}
	return func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		k := w.line(c)
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

// cutCase cuts a run at one of the calls that an uninterrupted run makes, to Vultr or to Nomad.
type cutCase struct {
	index int    // the call's place among the calls of the uninterrupted run, from 1: its line in the golden file
	key   string // the call, as callKey gives it; a call to Nomad starts with "nomad "
	n     int    // the call is the n-th with that key
	after bool   // the run's context ends just after the fake carried the call out; otherwise just before the call
}

// eachCut runs test in a parallel subtest of its own, in a synctest bubble, for each case that cuts a run at one of
// calls, the calls of an uninterrupted run as flowCalls gives them: first just before each call, then just after
// each. A subtest's name gives the call's place and method, such as "after 017 CreateFirewallRule".
func eachCut(t *testing.T, calls []string, test func(t *testing.T, c cutCase)) {
	eachCutAt(t, calls, func(int) bool { return true }, test)
}

// eachCutAt is eachCut for the calls that keep says to cut, by their place in calls from 0. A call that keep leaves
// out still counts in the n of the cases of the calls with its key.
func eachCutAt(t *testing.T, calls []string, keep func(index int) bool, test func(t *testing.T, c cutCase)) {
	for _, after := range []bool{false, true} {
		when := "before"
		if after {
			when = "after"
		}
		seen := map[string]int{}
		for i, call := range calls {
			key := callKey(call)
			seen[key]++
			if !keep(i) {
				continue
			}
			c := cutCase{index: i + 1, key: key, n: seen[key], after: after}
			method, _, _ := strings.Cut(strings.TrimPrefix(key, "nomad "), " ")
			t.Run(fmt.Sprintf("%s %03d %s", when, c.index, method), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) { test(t, c) })
			})
		}
	}
}

// runCut runs a use case on f and w with a context that ends at the call of c: just before the call reaches the fake,
// or just after the fake carried it out, when the call fails as the client fails for an ended context and loses its
// answer, as a request in flight does when tent is interrupted. w may be nil for a use case that calls no Nomad. It
// fails the test unless the run made the call and stopped with an error that matches context.Canceled, and changed
// nothing in the store s but for writing the secrets that s lacked, the completed spec, the highest index of the
// names of the server and combined machines and the mark of the Nomad bootstrap, and deleting a stale mark, as an
// update does. It returns the secrets that the run wrote.
func runCut(t *testing.T, f *vultrfake.Fake, w *nomadWorld, s statestore.Store, c cutCase,
	run func(context.Context) error,
) (wrote map[string][]byte) {
	t.Helper()
	stored, before := snapshot(t, s), secretsOf(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cut atomic.Bool
	act := func(ctx context.Context, next func(context.Context) error) error {
		cut.Store(true)
		if c.after {
			_ = next(ctx) // carried out; its answer is lost
		}
		cancel()
		return next(ctx)
	}
	f.SetHook(atCall(f, c.key, c.n, func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
		return act(ctx, next)
	}))
	if w != nil {
		w.SetHook(atNomadCall(w, c.key, c.n,
			func(ctx context.Context, _ nomadfake.Call, next func(context.Context) error) error {
				return act(ctx, next)
			}))
	}
	err := run(ctx)
	f.SetHook(nil)
	if w != nil {
		w.SetHook(nil)
	}
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
	paths := list(t, s, "")
	if _, ok := stored[completedPath]; !ok && slices.Contains(paths, completedPath) {
		stored[completedPath] = string(get(t, s, completedPath))
	}
	allowGrownNames(t, s, stored)
	if slices.Contains(paths, markPath) {
		stored[markPath] = string(get(t, s, markPath))
	} else {
		delete(stored, markPath) // an update deletes a mark that no server stands behind
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

// wantBuilt fails the test unless the build of the example cluster in the store s, on f and w, is whole after the runs
// that a test cut: the cloud is as an uninterrupted build leaves it, with one instance per node, the mark of the
// bootstrap is stored, Nomad's ACL system was bootstrapped by at most two calls, the secrets of kept are as they were
// written, and the next plan has no changes.
func wantBuilt(t *testing.T, svc *app.Service, f *vultrfake.Fake, w *nomadWorld, want []string,
	kept map[string][]byte,
) {
	t.Helper()
	wantView(t, f, want)
	if !slices.Contains(list(t, svc.Store, ""), markPath) {
		t.Errorf("the store holds no %s", markPath)
	}
	if n := countNomad(w, "Bootstrap"); n < 1 || n > 2 {
		t.Errorf("%d Bootstrap calls reached Nomad, want 1 or 2", n)
	}
	wantSecretsKept(t, svc.Store, kept)
	wantConverged(t, svc)
	wantLockFree(t, svc.Store)
}

// countNomad returns how many calls of the nomadops.API method name reached the Nomad fake of w.
func countNomad(w *nomadWorld, name string) int {
	n := 0
	for _, c := range w.Log() {
		if c.Name == name {
			n++
		}
	}
	return n
}

// TestUpdateCutAtEveryCall builds each example cluster, the one of servers and workers and the combined one, with a run
// cut at each of the calls of an uninterrupted build to Vultr and to Nomad, just before it and just after it. The next
// run, with a fresh context, leaves the cloud as the uninterrupted build does, without a second copy of any object,
// keeps the secrets that the cut run wrote, and a run after it has nothing to change.
func TestUpdateCutAtEveryCall(t *testing.T) {
	for _, ex := range exampleClusters {
		t.Run(ex.name, func(t *testing.T) {
			_, calls, want := buildExample(t, ex.docs...)
			_, template := exampleStore(t, ex.docs...)
			eachCut(t, calls, func(t *testing.T, c cutCase) {
				svc, f, w := newExampleFrom(t, template)
				wrote := runCut(t, f, w, svc.Store, c, func(ctx context.Context) error {
					_, err := svc.Update(ctx, "prod", true)
					return err
				})
				mustUpdate(t, svc)
				wantBuilt(t, svc, f, w, want, wrote)
			})
		})
	}
}

// builtTemplate builds the cluster of the specs docs in a new store, on a fake of its own, and returns the store's
// root.
func builtTemplate(t *testing.T, docs ...string) string {
	t.Helper()
	svc, root := exampleStore(t, docs...)
	synctest.Test(t, func(t *testing.T) {
		f, _ := withCloud(svc)
		withNomad(svc, f)
		mustUpdate(t, svc)
	})
	return root
}

// TestUpdateCutAtEveryCallOfARebuild rebuilds each example cluster on an empty cloud and a new Nomad, from a store
// that holds the built cluster's secrets and its mark of the bootstrap, with a run cut at each of the calls of an
// uninterrupted rebuild, just before it and just after it. After the next run the cloud is as the uninterrupted
// rebuild leaves it, the new Nomad is bootstrapped and the mark is stored again.
func TestUpdateCutAtEveryCallOfARebuild(t *testing.T) {
	for _, ex := range exampleClusters {
		t.Run(ex.name, func(t *testing.T) {
			template := builtTemplate(t, ex.docs...)
			var calls, want []string
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := newExampleFrom(t, template)
				_, calls = updateFlow(t, svc, f, w)
				want = cloudView(f)
			})
			eachCut(t, calls, func(t *testing.T, c cutCase) {
				svc, f, w := newExampleFrom(t, template)
				wrote := runCut(t, f, w, svc.Store, c, func(ctx context.Context) error {
					_, err := svc.Update(ctx, "prod", true)
					return err
				})
				mustUpdate(t, svc)
				wantBuilt(t, svc, f, w, want, wrote)
			})
		})
	}
}

// errCut is why a put or a delete that a test cut failed.
var errCut = errors.New("the test cut the call")

// cutStore fails the nth put or delete of path (the first when nth is 0): just before the call reaches the store, or
// just after the store carried it out, as a call whose answer is lost. It records the secrets it receives.
type cutStore struct {
	statestore.Store
	sent
	path  string
	nth   int
	after bool
	calls atomic.Int32
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

// call carries out the call op of p, or cuts it when it is the nth call of the store's path.
func (s *cutStore) call(op, p string, carry func() error) error {
	if p != s.path || int(s.calls.Add(1)) != max(s.nth, 1) {
		return carry()
	}
	if s.after {
		if err := carry(); err != nil {
			return err
		}
	}
	return cutError(op, p)
}

// cutError returns the error of the call op of path p that a test cut.
func cutError(op, p string) error { return fmt.Errorf("%s %q: %w", op, p, errCut) }

// TestUpdateCutAtEveryStateWrite builds each example cluster with a run cut at each write of a secret, of the completed
// spec and of the mark of the Nomad bootstrap, just before the store writes it and just after, when the answer is
// lost. The cut run stops with an error that shows none of the secrets, and that names the write when it is the one of
// the completed spec or of the mark. A cut before the mark leaves Nomad bootstrapped and the next run bootstraps
// again; a cut after it leaves the next run to go straight to the clients. The next run leaves the cloud as an
// uninterrupted build does, with one CA, one gossip key and one ACL bootstrap secret, and keeps each secret that the
// cut run wrote byte for byte.
func TestUpdateCutAtEveryStateWrite(t *testing.T) {
	for _, ex := range exampleClusters {
		_, _, want := buildExample(t, ex.docs...)
		_, template := exampleStore(t, ex.docs...)
		for _, after := range []bool{false, true} {
			when := "before"
			if after {
				when = "after"
			}
			for _, p := range append(slices.Clone(secretPaths), completedPath, markPath) {
				t.Run(ex.name+" "+when+" "+path.Base(p), func(t *testing.T) {
					t.Parallel()
					synctest.Test(t, func(t *testing.T) {
						svc, f, w := newExampleFrom(t, template)
						store := svc.Store
						cut := &cutStore{Store: store, path: p, after: after}
						svc.Store = cut

						_, err := svc.Update(t.Context(), "prod", true)
						if err == nil {
							t.Fatal("the cut run succeeded")
						}
						shown := map[string]string{"the error": err.Error()}
						// The completed spec and the mark are written after every secret.
						last := p
						if !isSecret(p) {
							last = aclPath
						}
						if secrettest.CheckHidden(t, shown, cut.received(t, last), ""); t.Failed() {
							t.FailNow() // the error would show a secret
						}
						if !errors.Is(err, errCut) {
							t.Fatalf("the cut run returned %v, want an error that matches %v", err, errCut)
						}
						switch cause := cutError("put", p).Error(); p {
						case completedPath:
							wantError(t, err, "write the completed spec: "+cause)
						case markPath:
							wantError(t, err, "write "+markPath+": "+cause)
						}
						wantLockFree(t, cut)
						if p != markPath {
							wantOnlyReads(t, f)
						}
						wrote := secretsOf(t, store)
						if _, ok := wrote[p]; isSecret(p) && ok != after {
							t.Errorf("the cut run wrote %s: %t, want %t", p, ok, after)
						}
						marked := slices.Contains(list(t, store, ""), markPath)
						if p == markPath && marked != after {
							t.Errorf("the cut run wrote the mark: %t, want %t", marked, after)
						}
						before := countNomad(w, "Bootstrap")

						svc.Store = store
						mustUpdate(t, svc)
						wantBuilt(t, svc, f, w, want, wrote)
						if p == markPath && after && countNomad(w, "Bootstrap") != before {
							t.Error("the next run bootstrapped again although the mark was stored")
						}
					})
				})
			}
		}
	}
}

// TestUpdateCutAtEveryWriteOfTheNames builds each example cluster with a run cut at each write of the highest index of
// its group's machine names, just before the store writes it and just after, when the answer is lost. The cut run sends
// no create request for the server of that write and stops with an error that names the object. The store holds the
// index of the write before it when the cut came first and the index of the write when it came after. The next run
// leaves the cloud as an uninterrupted build does.
func TestUpdateCutAtEveryWriteOfTheNames(t *testing.T) {
	groups := map[string]string{"servers and workers": "servers", "combined": "all"}
	for _, ex := range exampleClusters {
		_, _, want := buildExample(t, ex.docs...)
		_, template := exampleStore(t, ex.docs...)
		names := "prod/names/" + groups[ex.name]
		for _, after := range []bool{false, true} {
			when := "before"
			if after {
				when = "after"
			}
			for nth := 1; nth <= 3; nth++ {
				t.Run(fmt.Sprintf("%s %s write %d", ex.name, when, nth), func(t *testing.T) {
					t.Parallel()
					synctest.Test(t, func(t *testing.T) {
						svc, f, w := newExampleFrom(t, template)
						store := svc.Store
						svc.Store = &cutStore{Store: store, path: names, nth: nth, after: after}
						created := countCalls(f, "CreateInstance")

						_, err := svc.Update(t.Context(), "prod", true)

						if cause := cutError("put", names).Error(); !errors.Is(err, errCut) ||
							!strings.Contains(err.Error(), "write "+names+": "+cause) {
							t.Fatalf("the cut run returned %v, want an error that names %s and matches %v", err, names,
								errCut)
						}
						if got, want := countCalls(f, "CreateInstance")-created, nth-1; got != want {
							t.Errorf("the cut run sent %d creates, want %d", got, want)
						}
						held := nth - 2 // the index of the write before the cut one
						if after {
							held = nth - 1
						}
						if held < 0 {
							if got := list(t, store, names); len(got) != 0 {
								t.Errorf("the store holds %v, want no object of the names", got)
							}
						} else {
							wantStored(t, store, names, []byte(fmt.Sprintf("%d\n", held)))
						}
						wantLockFree(t, store)

						svc.Store = store
						mustUpdate(t, svc)
						wantBuilt(t, svc, f, w, want, nil)
					})
				})
			}
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
		svc, f, _ := newExampleFrom(t, template)
		mustUpdate(t, svc)
		runCut(t, f, nil, svc.Store, c, func(ctx context.Context) error {
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
						withNomad(svc, f) // the new bootstrap secret belongs to a Nomad that nobody has bootstrapped
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
				svc, f, _ := newExampleFrom(t, template)
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
