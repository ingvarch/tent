package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/statestore"
)

// holdLock takes the test cluster's lock as another tent would, until the test releases it or ends.
func holdLock(t *testing.T, s statestore.Store) *statestore.Lock {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	lk, _, err := statestore.NewLocker(t.Context(), s, l, nil)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := statestore.Acquire(t.Context(), lk, "prod", statestore.AcquireOptions{Operation: "update"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release(context.Background()) })
	return lock
}

func release(t *testing.T, lock *statestore.Lock) {
	t.Helper()
	if err := lock.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// changes are the mutating use cases, each changing the test cluster made of the cluster, servers and workers.
var changes = []struct {
	name string
	op   string // the operation its lease names
	run  func(t *testing.T, svc *app.Service) error
}{
	{"create", "create", func(t *testing.T, svc *app.Service) error {
		_, err := svc.Create(t.Context(), decode(t, edit(t, workersYAML, "name: workers", "name: web")), true)
		return err
	}},
	{"replace", "replace", func(t *testing.T, svc *app.Service) error {
		_, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		return err
	}},
	{"save", "edit", func(t *testing.T, svc *app.Service) error {
		_, ref, err := svc.Load(t.Context(), "prod", v1alpha1.KindNodeGroup, "workers")
		if err == nil {
			_, err = svc.Save(t.Context(), ref, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		}
		return err
	}},
	{"delete", "delete", func(t *testing.T, svc *app.Service) error {
		_, err := svc.DeleteState(t.Context(), "prod", true, false)
		return err
	}},
}

// recorder records the operation of the lock's lease at every write to the store other than the lease's own.
type recorder struct {
	statestore.Store
	mu  sync.Mutex
	ops []string
}

func (r *recorder) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	r.record(ctx, p)
	return r.Store.Put(ctx, p, data, opts)
}

func (r *recorder) Delete(ctx context.Context, p string) error {
	r.record(ctx, p)
	return r.Store.Delete(ctx, p)
}

func (r *recorder) record(ctx context.Context, p string) {
	if p == lockPath {
		return
	}
	var l statestore.Lease
	data, _, err := r.Get(ctx, lockPath)
	if err == nil {
		err = json.Unmarshal(data, &l)
	}
	op := l.Operation
	if err != nil {
		op = "no lease: " + err.Error()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

// TestChangesHoldTheLock checks that every write of a change happens under the cluster's lock, whose lease names the
// operation, and that the lease is gone afterwards.
func TestChangesHoldTheLock(t *testing.T) {
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			rec := &recorder{Store: unwrapped{svc.Store}}
			svc.Store = rec
			if err := tc.run(t, svc); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(rec.ops) == 0 {
				t.Fatal("no writes")
			}
			for _, op := range rec.ops {
				if op != tc.op {
					t.Errorf("a write under the lease of %q, want %q", op, tc.op)
				}
			}
			if paths := list(t, rec, lockPath); len(paths) != 0 {
				t.Errorf("the lease is left: %v", paths)
			}
		})
	}
}

func TestChangesWaitForTheLock(t *testing.T) {
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _ := newService(t)
				mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
				stored := snapshot(t, svc.Store)
				held := holdLock(t, svc.Store)
				waits := make(chan error, 10)
				svc.LockTimeout = time.Minute
				svc.OnWait = func(holder error) { waits <- holder }
				done := make(chan error, 1)
				go func() { done <- tc.run(t, svc) }()

				time.Sleep(10 * time.Second) // five tries
				synctest.Wait()
				if len(waits) != 1 {
					t.Fatalf("OnWait called %d times, want once", len(waits))
				}
				wantError(t, <-waits, lockedBy(held.Lease()))
				wantSnapshot(t, svc.Store, stored)

				release(t, held)
				if err := <-done; err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				if len(waits) != 0 {
					t.Errorf("OnWait called again")
				}
				if diff := cmp.Diff(stored, snapshot(t, svc.Store)); diff == "" {
					t.Errorf("%s changed nothing", tc.name)
				}
			})
		})
	}
}

// lockedBy is the error of a change that gave up waiting for holder.
func lockedBy(holder statestore.Lease) string {
	return (&statestore.LockedError{Cluster: "prod", Holder: holder}).Error()
}

func TestChangesDoNotWaitWithoutTimeout(t *testing.T) {
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			stored := snapshot(t, svc.Store)
			held := holdLock(t, svc.Store)
			svc.OnWait = func(error) { t.Error("OnWait called") }
			err := tc.run(t, svc)
			wantError(t, err, lockedBy(held.Lease()))
			if !errors.Is(err, statestore.ErrLocked) {
				t.Errorf("errors.Is(%v, ErrLocked) = false", err)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

func TestChangesStopWaitingAfterTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		held := holdLock(t, svc.Store)
		svc.LockTimeout = 9 * time.Second // between two tries, which come every 2 seconds
		start := time.Now()
		_, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		wantError(t, err, lockedBy(held.Lease())+"; gave up after 9s")
		if !errors.Is(err, statestore.ErrLocked) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not match ErrLocked and context.DeadlineExceeded", err)
		}
		if waited := time.Since(start); waited != svc.LockTimeout {
			t.Errorf("waited %v, want %v", waited, svc.LockTimeout)
		}
		wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
		release(t, held)
	})
}

func TestChangesCheckTheTentVersion(t *testing.T) {
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
			stored := snapshot(t, svc.Store)
			svc.Version = "v0.4.0"
			err := tc.run(t, svc)
			wantError(t, err, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
			if !errors.Is(err, statestore.ErrTentTooOld) {
				t.Errorf("errors.Is(%v, ErrTentTooOld) = false", err)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

func TestChangesRaiseTheTentVersion(t *testing.T) {
	svc, _ := newService(t)
	svc.Version = "v0.4.0"
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	wantStored(t, svc.Store, versionPath, []byte("v0.4.0\n"))

	svc.Version = "v0.5.0-3-gabc1234"
	bigger := edit(t, workersYAML, "size: 2", "size: 4")
	if _, err := svc.Replace(t.Context(), decode(t, bigger), true); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	wantStored(t, svc.Store, versionPath, []byte("v0.5.0\n"))

	// A change that changes nothing takes no lock and records no version.
	svc.Version = "v0.6.0"
	if _, err := svc.Replace(t.Context(), decode(t, bigger), true); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	wantStored(t, svc.Store, versionPath, []byte("v0.5.0\n"))
}

// blocking holds the Put of the test cluster's spec until its context ends. Before it blocks it removes the lock's
// lease, as tent state unlock --force by someone else would.
type blocking struct{ statestore.Store }

func (b blocking) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if p != clusterPath {
		return b.Store.Put(ctx, p, data, opts)
	}
	if err := b.Delete(ctx, lockPath); err != nil {
		return "", err
	}
	<-ctx.Done()
	return "", fmt.Errorf("put %q: %w", p, ctx.Err())
}

func TestChangesStopWhenTheLockIsLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		svc.Store = blocking{unwrapped{svc.Store}}
		_, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML), true)
		wantError(t, err, `lost the lock of cluster prod; stopped: put "prod/cluster.yaml": context canceled`)
		if !errors.Is(err, statestore.ErrLockLost) {
			t.Errorf("errors.Is(%v, ErrLockLost) = false", err)
		}
		wantPaths(t, svc.Store, serversPath)
	})
}

func TestWeakLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		svc.Store = noConditions{svc.Store}
		warned := 0
		svc.OnWeakLock = func() { warned++ }
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		if warned != 1 {
			t.Errorf("OnWeakLock called %d times, want once", warned)
		}
		wantPaths(t, svc.Store, clusterPath, serversPath, workersPath)
	})
	svc, _ := newService(t)
	svc.OnWeakLock = func() { t.Error("OnWeakLock called for a file store") }
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
}

// TestChangesCheckTheTentVersionAfterWaiting has a newer tent raise the tent version while it holds the lock that a
// change waits for.
func TestChangesCheckTheTentVersionAfterWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		held := holdLock(t, svc.Store)
		svc.Version, svc.LockTimeout = "v0.4.0", time.Minute
		bigger := decode(t, edit(t, workersYAML, "size: 2", "size: 4"))
		done := make(chan error, 1)
		go func() {
			_, err := svc.Replace(t.Context(), bigger, true)
			done <- err
		}()
		synctest.Wait()
		put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
		release(t, held)
		wantError(t, <-done, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
		wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
	})
}

// puts records the path and options of every Put other than the lock's.
type puts struct {
	statestore.Store
	mu   sync.Mutex
	puts []putCall
}

type putCall struct {
	Path string
	Opts statestore.PutOptions
}

func (p *puts) Put(
	ctx context.Context, path string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if path != lockPath {
		p.mu.Lock()
		p.puts = append(p.puts, putCall{path, opts})
		p.mu.Unlock()
	}
	return p.Store.Put(ctx, path, data, opts)
}

// TestWrites checks the order of the writes and their conditions: create-only for new objects, and replace-only for
// the version read under the lock. The tent version comes first, and the Cluster last, so an interrupted create
// leaves no cluster.yaml.
func TestWrites(t *testing.T) {
	svc, _ := newService(t)
	rec := &puts{Store: svc.Store}
	svc.Store, svc.Version = rec, "v0.4.0"
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	_, old, err := svc.Store.Get(t.Context(), workersPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	create := statestore.PutOptions{IfNoneMatch: true}
	want := []putCall{
		{versionPath, statestore.PutOptions{}}, {serversPath, create}, {workersPath, create}, {clusterPath, create},
		{workersPath, statestore.PutOptions{IfMatch: old}},
	}
	if diff := cmp.Diff(want, rec.puts); diff != "" {
		t.Errorf("puts (-want +got):\n%s", diff)
	}
}

// failing fails the first Put of one path, as an interruption would.
type failing struct {
	statestore.Store
	path   string
	failed bool
}

func (f *failing) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if p == f.path && !f.failed {
		f.failed = true
		return "", fmt.Errorf("put %q: interrupted", p)
	}
	return f.Store.Put(ctx, p, data, opts)
}

// TestCreateAgainAfterTheVersionFailed runs a create again whose write of the tent version failed: the version is
// recorded in the end, although the specs are unchanged.
func TestCreateAgainAfterTheVersionFailed(t *testing.T) {
	svc, _ := newService(t)
	svc.Store, svc.Version = &failing{Store: svc.Store, path: versionPath}, "v0.4.0"
	objs := decode(t, clusterYAML, serversYAML, workersYAML)
	if _, err := svc.Create(t.Context(), objs, true); err == nil {
		t.Fatal("Create succeeded, want the interruption")
	}
	if _, err := svc.Create(t.Context(), objs, true); err != nil {
		t.Fatalf("Create again: %v", err)
	}
	wantStored(t, svc.Store, versionPath, []byte("v0.4.0\n"))
	wantPaths(t, svc.Store, clusterPath, serversPath, workersPath, versionPath)
}

// replaceWorkers is a change of the test cluster.
func replaceWorkers(t *testing.T, svc *app.Service) error {
	t.Helper()
	_, err := svc.Replace(t.Context(), decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
	return err
}

func TestOnTakeover(t *testing.T) {
	t.Run("dead flock holder", func(t *testing.T) {
		svc, root := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		dead := leaveFlockLease(t, root)
		var taken []statestore.Lease
		svc.OnTakeover = func(previous statestore.Lease) { taken = append(taken, previous) }
		if err := replaceWorkers(t, svc); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		if diff := cmp.Diff([]statestore.Lease{dead}, taken); diff != "" {
			t.Errorf("OnTakeover (-want +got):\n%s", diff)
		}
	})
	t.Run("expired lease", func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		svc.Store = unwrapped{svc.Store}
		expired := otherLease(time.Now().Add(-time.Minute).UTC())
		put(t, svc.Store, lockPath, leaseJSON(t, expired))
		var taken []statestore.Lease
		svc.OnTakeover = func(previous statestore.Lease) { taken = append(taken, previous) }
		if err := replaceWorkers(t, svc); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		// A free lock is no takeover.
		web := decode(t, edit(t, workersYAML, "name: workers", "name: web"))
		if _, err := svc.Create(t.Context(), web, true); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if diff := cmp.Diff([]statestore.Lease{expired}, taken); diff != "" {
			t.Errorf("OnTakeover (-want +got):\n%s", diff)
		}
	})
}

// TestChangesPlanAgainUnderTheLock changes the store while a change waits for the lock: the change decides on what
// it finds once it holds the lock.
func TestChangesPlanAgainUnderTheLock(t *testing.T) {
	bigger := edit(t, workersYAML, "size: 2", "size: 4")
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, svc *app.Service, ref app.Ref) error
		want string // the error, or "" when the change is stored
	}{
		{"create", func(t *testing.T, svc *app.Service, _ app.Ref) error {
			_, err := svc.Create(t.Context(), decode(t, edit(t, workersYAML, "name: workers", "name: web")), true)
			return err
		}, "node group web of cluster prod already exists; change it with edit or replace -f"},
		{"replace", func(t *testing.T, svc *app.Service, _ app.Ref) error {
			_, err := svc.Replace(t.Context(), decode(t, bigger), true)
			return err
		}, ""},
		{"save", func(t *testing.T, svc *app.Service, ref app.Ref) error {
			_, err := svc.Save(t.Context(), ref, decode(t, bigger), true)
			return err
		}, "node group workers of cluster prod changed while you edited it; run edit again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _ := newService(t)
				mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
				ref := load(t, svc, v1alpha1.KindNodeGroup, "workers")
				held := holdLock(t, svc.Store)
				svc.LockTimeout = time.Minute
				done := make(chan error, 1)
				go func() { done <- tc.run(t, svc, ref) }()
				synctest.Wait()

				// What another tent changes while it holds the lock.
				put(t, svc.Store, workersPath, encode(t, edit(t, workersYAML, "size: 2", "size: 3")))
				put(t, svc.Store, "prod/nodegroups/web.yaml",
					encode(t, edit(t, edit(t, workersYAML, "name: workers", "name: web"), "size: 2", "size: 5")))
				release(t, held)
				err := <-done
				if tc.want == "" {
					if err != nil {
						t.Fatalf("%s: %v", tc.name, err)
					}
					wantStored(t, svc.Store, workersPath, encode(t, bigger))
					return
				}
				wantError(t, err, tc.want)
			})
		})
	}
}

func TestChangesStopWaitingWhenInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		held := holdLock(t, svc.Store)
		ctx, interrupt := context.WithCancel(t.Context())
		svc.LockTimeout = time.Minute
		svc.OnWait = func(error) { interrupt() } // as Ctrl-C would, while it waits
		_, err := svc.Replace(ctx, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		wantError(t, err, lockedBy(held.Lease())+"; interrupted")
		if !errors.Is(err, statestore.ErrLocked) || !errors.Is(err, context.Canceled) {
			t.Errorf("error %v does not match ErrLocked and context.Canceled", err)
		}
		wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
		release(t, held)
	})
}

// TestChangesReleaseTheLockWhenInterrupted interrupts a change that holds the lock: the change releases the lock and
// says that it was interrupted.
func TestChangesReleaseTheLockWhenInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, svc *app.Service, root string) // the lease of a holder that is gone
	}{
		{"flock", func(t *testing.T, _ *app.Service, root string) { leaveFlockLease(t, root) }},
		{"lease", func(t *testing.T, svc *app.Service, _ string) {
			svc.Store = unwrapped{svc.Store}
			put(t, svc.Store, lockPath, leaseJSON(t, otherLease(time.Now().Add(-time.Minute).UTC())))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, root := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			tc.leave(t, svc, root)
			ctx, interrupt := context.WithCancel(t.Context())
			// The change takes over the lease, then Ctrl-C comes while it holds the lock.
			svc.OnTakeover = func(statestore.Lease) { interrupt() }
			_, err := svc.Replace(ctx, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
			wantError(t, err, "interrupted")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(%v, context.Canceled) = false", err)
			}
			l, err := statestore.NewLayout("prod")
			if err != nil {
				t.Fatal(err)
			}
			lk, _, err := statestore.NewLocker(t.Context(), svc.Store, l, nil)
			if err != nil {
				t.Fatal(err)
			}
			if holder, err := lk.Holder(t.Context()); holder != nil || err != nil {
				t.Errorf("Holder = %v, %v; want nil: the lock is released", holder, err)
			}
			wantStored(t, svc.Store, workersPath, encode(t, workersYAML))
		})
	}
}

// TestUseCasesSayInterrupted runs every use case with a context that has ended, as after Ctrl-C.
func TestUseCasesSayInterrupted(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	stored := snapshot(t, svc.Store)
	ref := load(t, svc, v1alpha1.KindNodeGroup, "workers")
	bigger := decode(t, edit(t, workersYAML, "size: 2", "size: 4"))
	ctx, interrupt := context.WithCancel(t.Context())
	interrupt()
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"Get", func() error { _, err := svc.Get(ctx, "prod", false); return err }},
		{"Clusters", func() error { _, err := svc.Clusters(ctx, false); return err }},
		{"NodeGroups", func() error { _, err := svc.NodeGroups(ctx, "prod", nil, false); return err }},
		{"Load", func() error { _, _, err := svc.Load(ctx, "prod", v1alpha1.KindCluster, ""); return err }},
		{"Save", func() error { _, err := svc.Save(ctx, ref, bigger, true); return err }},
		{"Create", func() error { _, err := svc.Create(ctx, decode(t, clusterYAML), true); return err }},
		{"Replace", func() error { _, err := svc.Replace(ctx, bigger, true); return err }},
		{"DeleteState", func() error { _, err := svc.DeleteState(ctx, "prod", true, false); return err }},
		{"Unlock", func() error { _, err := svc.Unlock(ctx, "prod", false); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			wantError(t, err, "interrupted")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(%v, context.Canceled) = false", err)
			}
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

// TestChangesThatNeedNoLock runs changes that fail or change nothing while another tent holds the lock: they
// neither wait nor fail for the lock.
func TestChangesThatNeedNoLock(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	held := holdLock(t, svc.Store)

	changes, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML, workersYAML), true)
	if err != nil {
		t.Fatalf("Create with nothing to change: %v", err)
	}
	wantChanges(t, changes, cluster(app.Unchanged), group("servers", app.Unchanged), group("workers", app.Unchanged))

	_, err = svc.Create(t.Context(), decode(t, edit(t, serversYAML, "name: servers", "name: more")), true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "Cluster prod", Path: "nodeGroups",
		Detail: "need exactly one server or combined group, found 2: more, servers",
	})

	put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
	svc.Version = "v0.4.0"
	_, err = svc.Create(t.Context(), decode(t, edit(t, workersYAML, "name: workers", "name: web")), true)
	wantError(t, err, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
	release(t, held)
}

// TestWaitForUnnamedHolder waits for a flock whose holder has written no lease.
func TestWaitForUnnamedHolder(t *testing.T) {
	const unnamed = "cluster prod is locked, but its holder has written no lease"
	synctest.Test(t, func(t *testing.T) {
		svc, root := newService(t)
		mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
		held := holdLock(t, svc.Store)
		if err := os.Remove(flockLease(root)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		waits := 0
		svc.OnWait = func(holder error) {
			waits++
			wantError(t, holder, unnamed)
		}

		// Without a timeout it fails at once.
		start := time.Now()
		_, err := svc.Replace(ctx, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		wantError(t, err, unnamed)
		if !errors.Is(err, statestore.ErrLocked) || time.Since(start) != 0 || waits != 0 {
			t.Errorf("error %v matches ErrLocked: %t; waited %v; OnWait called %d times; want true, 0, 0",
				err, errors.Is(err, statestore.ErrLocked), time.Since(start), waits)
		}

		// With one it tells OnWait that it waits.
		svc.LockTimeout = 9 * time.Second
		_, err = svc.Replace(ctx, decode(t, edit(t, workersYAML, "size: 2", "size: 4")), true)
		wantError(t, err, unnamed+"; gave up after 9s")
		if waits != 1 {
			t.Errorf("OnWait called %d times, want once", waits)
		}
		release(t, held)
	})
}

// forceUnlocked removes the lock's lease after the Put or Delete of one path, as tent state unlock --force by someone
// else would.
type forceUnlocked struct {
	statestore.Store
	after string
}

func (f forceUnlocked) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	v, err := f.Store.Put(ctx, p, data, opts)
	if err == nil && p == f.after {
		err = f.Store.Delete(ctx, lockPath)
	}
	return v, err
}

func (f forceUnlocked) Delete(ctx context.Context, p string) error {
	err := f.Store.Delete(ctx, p)
	if err == nil && p == f.after {
		err = f.Store.Delete(ctx, lockPath)
	}
	return err
}

// TestLockLostAfterTheChange loses the lock after the last write: the change comes back with the error.
func TestLockLostAfterTheChange(t *testing.T) {
	const want = "the change is saved, but the lock of cluster prod was lost before tent released it"
	wantSaved := func(t *testing.T, err error) {
		t.Helper()
		wantError(t, err, want)
		if !errors.Is(err, statestore.ErrLockLost) {
			t.Errorf("errors.Is(%v, ErrLockLost) = false", err)
		}
	}
	t.Run("create", func(t *testing.T) {
		svc, _ := newService(t)
		svc.Store = forceUnlocked{unwrapped{svc.Store}, clusterPath}
		changes, err := svc.Create(t.Context(), decode(t, clusterYAML, serversYAML, workersYAML), true)
		wantSaved(t, err)
		wantChanges(t, changes, cluster(app.Created), group("servers", app.Created), group("workers", app.Created))
		wantPaths(t, svc.Store, clusterPath, serversPath, workersPath)
	})
	t.Run("delete", func(t *testing.T) {
		svc := newCluster(t)
		svc.Store = forceUnlocked{unwrapped{svc.Store}, versionPath}
		paths, err := svc.DeleteState(t.Context(), "prod", true, false)
		wantSaved(t, err)
		if diff := cmp.Diff(allState, paths); diff != "" {
			t.Errorf("DeleteState (-want +got):\n%s", diff)
		}
		wantPaths(t, svc.Store)
	})
}

// racing writes other content to a path just before a conditional Put of it, as a writer that ignores the lock
// would.
type racing struct {
	statestore.Store
	path string
}

func (r racing) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if p == r.path && (opts.IfNoneMatch || opts.IfMatch != "") {
		if _, err := r.Store.Put(ctx, p, []byte("other"), statestore.PutOptions{}); err != nil {
			return "", err
		}
	}
	return r.Store.Put(ctx, p, data, opts)
}

func TestConditionFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		run  func(t *testing.T, svc *app.Service) error
		want string
	}{
		{"create", "prod/nodegroups/web.yaml", func(t *testing.T, svc *app.Service) error {
			_, err := svc.Create(t.Context(), decode(t, edit(t, workersYAML, "name: workers", "name: web")), true)
			return err
		}, "node group web of cluster prod changed meanwhile; run the command again"},
		{"replace", workersPath, replaceWorkers,
			"node group workers of cluster prod changed meanwhile; run the command again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
			svc.Store = racing{svc.Store, tc.path}
			wantError(t, tc.run(t, svc), tc.want)
		})
	}
}
