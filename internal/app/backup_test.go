package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// backupPrefix is where the test cluster's backups are stored.
const backupPrefix = "prod/backups/"

// firstSnapshot is what the Nomad fake returns for its first snapshot.
const firstSnapshot = "nomadfake snapshot 1"

// errRefused is the error of a Nomad that cannot take a snapshot yet.
var errRefused = fmt.Errorf("500: cannot take snapshot now: %w", nomadops.ErrNotReady)

// markOf returns the time that the test cluster's bootstrap mark holds.
func markOf(t *testing.T, s statestore.Store) time.Time {
	t.Helper()
	mark, err := time.Parse(time.RFC3339, strings.TrimSpace(string(get(t, s, markPath))))
	if err != nil {
		t.Fatalf("the mark holds no time: %v", err)
	}
	return mark
}

// backupOf returns the backup that the test cluster stores at taken, of the servers whose mark is bootstrapped.
func backupOf(t *testing.T, taken, bootstrapped time.Time) statestore.Backup {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	return l.Backup(taken, bootstrapped)
}

// mustBackup makes a backup of the test cluster and stops the test on an error.
func mustBackup(t *testing.T, svc *app.Service) app.StoredBackup {
	t.Helper()
	b, err := svc.BackupCreate(t.Context(), "prod")
	if err != nil {
		t.Fatalf("BackupCreate: %v", err)
	}
	return b
}

// fixClock makes the service's clock stand still at the bubble's time, and returns that time.
func fixClock(svc *app.Service) time.Time {
	now := time.Now()
	svc.Now = func() time.Time { return now }
	return now
}

// wantNoBackup fails the test when the store holds an object under the backups' prefix.
func wantNoBackup(t *testing.T, s statestore.Store) {
	t.Helper()
	if paths := list(t, s, backupPrefix); len(paths) != 0 {
		t.Errorf("the store holds backups %v, want none", paths)
	}
}

// failSnapshots makes every SaveSnapshot of the Nomad world fail with err, and returns the count of the calls.
func failSnapshots(w *nomadWorld, err error) *int {
	calls := new(int)
	w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		if c.Name != "SaveSnapshot" {
			return next(ctx)
		}
		*calls++
		return err
	})
	return calls
}

// nonLockWrites returns the paths of the writes that are not the lock's.
func nonLockWrites(w *writeLog) []string {
	return slices.DeleteFunc(slices.Clone(w.writes), func(p string) bool { return p == lockPath })
}

// TestStoredBackupText writes the line of a stored backup and its JSON.
func TestStoredBackupText(t *testing.T) {
	t.Parallel()
	taken, bootstrapped := time.Date(2026, 10, 9, 23, 2, 7, 0, time.UTC), time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	b := app.StoredBackup{Cluster: "prod", Backup: backupOf(t, taken, bootstrapped), Size: 16855}

	var text bytes.Buffer
	if err := b.WriteText(&text); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	const wantText = "stored backup 20261009T230207Z of cluster prod (16855 bytes)\n"
	if text.String() != wantText {
		t.Errorf("WriteText = %q, want %q", text.String(), wantText)
	}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const wantJSON = `{"cluster":"prod","name":"20261009T230207Z","time":"2026-10-09T23:02:07Z",` +
		`"path":"prod/backups/20261001T100000Z/20261009T230207Z.snap","size":16855}`
	if string(got) != wantJSON {
		t.Errorf("JSON = %s, want %s", got, wantJSON)
	}
}

// TestBackupCreateStoresTheSnapshot makes a backup of a built cluster: one object with Nomad's bytes, at the path of
// the time and the mark, readable by its owner alone. Nomad gets a read of its nodes and one snapshot, from the first
// server; the cloud gets one list of the machines and no write; the store gets the backup and the lock's own writes.
func TestBackupCreateStoresTheSnapshot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, root := builtWorld(t)
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec
		now := fixClock(svc)
		cloudCalls, nomadBefore, listsBefore := len(f.Calls()), len(w.Log()), countCalls(f, "ListInstances")

		got := mustBackup(t, svc)

		want := backupOf(t, now, markOf(t, svc.Store))
		file := filepath.Join(root, filepath.FromSlash(want.Path))
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("the backup is not stored at %s: %v", want.Path, err)
		}
		if string(data) != firstSnapshot {
			t.Errorf("the backup holds %q, want Nomad's bytes %q", data, firstSnapshot)
		}
		if info, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("mode of the backup = %v, %v; want 0600", info, err)
		}
		if diff := cmp.Diff(app.StoredBackup{Cluster: "prod", Backup: want, Size: len(data)}, got); diff != "" {
			t.Errorf("result (-want +got):\n%s", diff)
		}
		calls := w.Log()[nomadBefore:]
		if names := nomadCallNames(w, nomadBefore); !slices.Equal(names, []string{"Nodes", "SaveSnapshot"}) {
			t.Fatalf("calls of Nomad = %v, want Nodes and one SaveSnapshot", names)
		}
		if server := w.nodeName(calls[1].Server); server != "prod-servers-0" {
			t.Errorf("SaveSnapshot went to %s, want the first server prod-servers-0", server)
		}
		if n := countCalls(f, "ListInstances") - listsBefore; n != 1 {
			t.Errorf("%d lists of the machines, want one", n)
		}
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if diff := cmp.Diff([]string{want.Path}, nonLockWrites(rec)); diff != "" {
			t.Errorf("writes to the store besides the lock's (-want +got):\n%s", diff)
		}
		wantLockFree(t, svc.Store)
	})
}

// TestBackupCreateRaisesTheTentVersion records the running version before it stores the backup, and only when it is
// newer than the stored one.
func TestBackupCreateRaisesTheTentVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		rec := &writeLog{Store: svc.Store}
		svc.Store, svc.Version = rec, "v0.9.0"

		first := mustBackup(t, svc)

		if diff := cmp.Diff([]string{versionPath, first.Path}, nonLockWrites(rec)); diff != "" {
			t.Errorf("writes of the first backup (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, versionPath, []byte("v0.9.0\n"))
		rec.writes = nil
		time.Sleep(time.Second)

		second := mustBackup(t, svc)

		if diff := cmp.Diff([]string{second.Path}, nonLockWrites(rec)); diff != "" {
			t.Errorf("writes of the second backup (-want +got):\n%s", diff)
		}
	})
}

// TestBackupCreateHoldsTheLockAtEveryStep reads the lease at the call of Nomad that takes the snapshot and at every
// write: it names the operation. The lock is free afterwards.
func TestBackupCreateHoldsTheLockAtEveryStep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		rec := &recorder{Store: unwrapped{svc.Store}}
		svc.Store = rec
		var leased []string
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "SaveSnapshot" {
				if h := holder(t, rec); h != nil {
					leased = append(leased, h.Operation)
				}
			}
			return next(ctx)
		})

		got := mustBackup(t, svc)

		if diff := cmp.Diff([]string{"backup create"}, leased); diff != "" {
			t.Errorf("leases at the snapshot (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"backup create"}, rec.ops); diff != "" {
			t.Errorf("leases at the writes of %s (-want +got):\n%s", got.Path, diff)
		}
		wantLockFree(t, rec)
	})
}

// TestBackupCreateWaitsForTheLock waits for another holder and takes the snapshot when the holder is done.
func TestBackupCreateWaitsForTheLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		held := holdLock(t, svc.Store)
		svc.LockTimeout = time.Minute
		nomadBefore := len(w.Log())
		done := make(chan error, 1)
		go func() {
			_, err := svc.BackupCreate(t.Context(), "prod")
			done <- err
		}()
		synctest.Wait()

		if n := len(w.Log()) - nomadBefore; n != 0 {
			t.Errorf("%d calls of Nomad while the lock is held, want none", n)
		}
		release(t, held)
		if err := <-done; err != nil {
			t.Fatalf("BackupCreate: %v", err)
		}
		if paths := list(t, svc.Store, backupPrefix); len(paths) != 1 {
			t.Errorf("backups = %v, want one", paths)
		}
	})
}

// TestBackupCreateStopsWaitingForTheLock fails with the holder's name at the lock timeout, and takes no snapshot.
func TestBackupCreateStopsWaitingForTheLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		held := holdLock(t, svc.Store)
		svc.LockTimeout = 9 * time.Second
		nomadBefore := len(w.Log())

		_, err := svc.BackupCreate(t.Context(), "prod")

		wantError(t, err, lockedBy(held.Lease())+"; gave up after 9s")
		if !errors.Is(err, statestore.ErrLocked) {
			t.Errorf("error %v does not match ErrLocked", err)
		}
		if n := len(w.Log()) - nomadBefore; n != 0 {
			t.Errorf("%d calls of Nomad, want none", n)
		}
		wantNoBackup(t, svc.Store)
		release(t, held)
	})
}

// TestBackupCreateRefusesAnUnbuiltClusterWithoutTheLock fails for a cluster without its mark, without a secret and
// with a mark that holds no time: no lock, no call of the cloud or Nomad, no write.
func TestBackupCreateRefusesAnUnbuiltClusterWithoutTheLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, svc *app.Service)
		want   string
	}{
		{"no mark", func(t *testing.T, svc *app.Service) { deleteObject(t, svc, markPath) },
			"cluster prod: Nomad is not bootstrapped yet (prod/nomad/bootstrapped is missing); " +
				"run tent update cluster --yes first"},
		{"no secret", func(t *testing.T, svc *app.Service) { deleteObject(t, svc, aclPath) },
			"cluster prod: the state store lacks secrets/acl-bootstrap-token; run tent update cluster --yes first"},
		{"no time in the mark", func(t *testing.T, svc *app.Service) { put(t, svc.Store, markPath, []byte("soon\n")) },
			markPath + " does not hold a time: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				tc.damage(t, svc)
				rec := &writeLog{Store: svc.Store}
				svc.Store = rec
				cloudCalls, nomadBefore := len(f.Calls()), len(w.Log())

				_, err := svc.BackupCreate(t.Context(), "prod")

				if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
					t.Fatalf("error = %v, want it to start with %q", err, tc.want)
				}
				if tc.name == "no time in the mark" {
					if _, ok := errors.AsType[*time.ParseError](err); !ok {
						t.Errorf("error %v does not wrap the parse error", err)
					}
				}
				if len(f.Calls()) != cloudCalls || len(w.Log()) != nomadBefore || len(rec.writes) != 0 {
					t.Errorf("%d calls of the cloud, %d of Nomad and writes %v, want none",
						len(f.Calls())-cloudCalls, len(w.Log())-nomadBefore, rec.writes)
				}
			})
		})
	}
}

// deleteServers deletes the machines of the test cluster's three servers.
func deleteServers(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	for _, id := range serverIDs {
		if err := f.DeleteInstance(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBackupCreateRefusesUnreachableServersUnderTheLock fails for a cluster without a server machine, with servers
// that have no public address, and without a Nomad client: after the lock is taken, before any call of Nomad. The
// lock is free afterwards.
func TestBackupCreateRefusesUnreachableServersUnderTheLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, svc *app.Service, f *vultrfake.Fake)
		want    string
	}{
		{"no machine", func(t *testing.T, _ *app.Service, f *vultrfake.Fake) { deleteServers(t, f) },
			"cluster prod has no server machine; run tent update cluster --yes first"},
		{"no public address", func(t *testing.T, _ *app.Service, f *vultrfake.Fake) {
			deleteServers(t, f)
			seedServer(t, f, "prod-servers-0", "")
		}, "cluster prod: no server has a public address"},
		{"no Nomad", func(_ *testing.T, svc *app.Service, _ *vultrfake.Fake) { svc.Nomad = nil },
			"no Nomad client is set up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				tc.prepare(t, svc, f)
				rec := &writeLog{Store: svc.Store}
				svc.Store = rec
				nomadBefore := len(w.Log())

				_, err := svc.BackupCreate(t.Context(), "prod")

				wantError(t, err, tc.want)
				if n := len(w.Log()) - nomadBefore; n != 0 {
					t.Errorf("%d calls of Nomad, want none", n)
				}
				if !slices.Contains(rec.writes, lockPath) {
					t.Errorf("writes %v: the refusal came before the lock", rec.writes)
				}
				if writes := nonLockWrites(rec); len(writes) != 0 {
					t.Errorf("writes besides the lock's: %v", writes)
				}
				wantLockFree(t, rec)
			})
		})
	}
}

// TestBackupCreateEndsAtOnceWhenNomadIsNotReachable fails the read of the nodes on every server: the run ends with
// the reach hint, takes no snapshot and stores nothing.
func TestBackupCreateEndsAtOnceWhenNomadIsNotReachable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		for range 3 {
			w.Fail(t, "Nodes", fmt.Errorf("no answer: %w", nomadops.ErrNotReady))
		}
		nomadBefore, start := len(w.Log()), time.Now()

		_, err := svc.BackupCreate(t.Context(), "prod")

		if err == nil || !strings.HasPrefix(err.Error(), "reach Nomad: ") || !strings.HasSuffix(err.Error(), hintOfAccess) ||
			!errors.Is(err, nomadops.ErrNotReady) {
			t.Errorf("error = %v, want the prefix %q, the reach hint and ErrNotReady", err, "reach Nomad: ")
		}
		if slices.Contains(nomadCallNames(w, nomadBefore), "SaveSnapshot") || time.Since(start) != 0 {
			t.Errorf("calls %v after %v, want no snapshot and no wait", nomadCallNames(w, nomadBefore), time.Since(start))
		}
		wantNoBackup(t, svc.Store)
		wantLockFree(t, svc.Store)
	})
}

// TestBackupCreateWaitsForASnapshot meets Nomad's refusal on every server in two rounds, then takes the snapshot: one
// warning with the limit, one object, and 4 s of waiting.
func TestBackupCreateWaitsForASnapshot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		var warnings []string
		svc.OnWarning = func(s string) { warnings = append(warnings, s) }
		refused := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "SaveSnapshot" && refused < 6 {
				refused++
				return errRefused
			}
			return next(ctx)
		})
		start := time.Now()

		got := mustBackup(t, svc)

		if waited := time.Since(start); waited != 4*time.Second {
			t.Errorf("waited %v, want 4s: two polls of 2s", waited)
		}
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "Nomad cannot take a snapshot yet: ") ||
			!strings.Contains(warnings[0], errRefused.Error()) ||
			!strings.HasSuffix(warnings[0], "; trying again for up to 5m0s") {
			t.Errorf("warnings = %q, want one: the refusal with the limit", warnings)
		}
		wantStored(t, svc.Store, got.Path, []byte(firstSnapshot))
		if paths := list(t, svc.Store, backupPrefix); len(paths) != 1 {
			t.Errorf("backups = %v, want one", paths)
		}
	})
}

// TestBackupCreateGivesUpOnASnapshot is refused for the whole limit: the run ends at 5 minutes with Nomad's last
// error, and stores nothing.
func TestBackupCreateGivesUpOnASnapshot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		failSnapshots(w, errRefused)
		start := time.Now()

		_, err := svc.BackupCreate(t.Context(), "prod")

		if err == nil || !strings.HasPrefix(err.Error(), "no snapshot from Nomad within 5m0s: ") ||
			!strings.Contains(err.Error(), errRefused.Error()) || !strings.HasSuffix(err.Error(),
			"; after its servers change Nomad takes none until its next write: run the command again") ||
			!errors.Is(err, nomadops.ErrNotReady) {
			t.Errorf("error = %v, want the limit, Nomad's refusal and the advice", err)
		}
		if waited := time.Since(start); waited != 5*time.Minute {
			t.Errorf("waited %v, want 5m0s", waited)
		}
		wantNoBackup(t, svc.Store)
		wantLockFree(t, svc.Store)
	})
}

// TestBackupCreateStopsAtAPermanentSnapshotError ends the run at once, without a warning and without a wait.
func TestBackupCreateStopsAtAPermanentSnapshotError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		warned := false
		svc.OnWarning = func(string) { warned = true }
		denied := errors.New("403 Permission denied")
		calls := failSnapshots(w, denied)
		start := time.Now()

		_, err := svc.BackupCreate(t.Context(), "prod")

		wantError(t, err, "take a snapshot of Nomad: "+denied.Error())
		if !errors.Is(err, denied) || warned || *calls != 1 || time.Since(start) != 0 {
			t.Errorf("matches the cause %t, warned %t, %d snapshot calls after %v; want true, false, 1 and 0",
				errors.Is(err, denied), warned, *calls, time.Since(start))
		}
		wantNoBackup(t, svc.Store)
	})
}

// TestBackupCreateNeverReplacesABackup meets a backup of its own name: the second backup of a second fails and the
// first stays as it was. A store that cannot refuse an overwrite takes a plain write.
func TestBackupCreateNeverReplacesABackup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		fixClock(svc)
		first := mustBackup(t, svc)

		_, err := svc.BackupCreate(t.Context(), "prod")

		wantError(t, err, "backup "+first.Name+" of cluster prod exists already; run the command again")
		wantStored(t, svc.Store, first.Path, []byte(firstSnapshot))
		wantLockFree(t, svc.Store)

		svc.Store = noConditions{svc.Store}
		if _, err := svc.BackupCreate(t.Context(), "prod"); err != nil {
			t.Fatalf("BackupCreate on a store without conditional puts: %v", err)
		}
		wantStored(t, svc.Store, first.Path, []byte("nomadfake snapshot 3"))
	})
}

// TestBackupCreateNamesAFailedStore stores nothing when the write of the backup fails, names its path, and has
// recorded the version before.
func TestBackupCreateNamesAFailedStore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		now := fixClock(svc)
		want := backupOf(t, now, markOf(t, svc.Store))
		svc.Store, svc.Version = &failing{Store: svc.Store, path: want.Path}, "v0.9.0"

		_, err := svc.BackupCreate(t.Context(), "prod")

		wantError(t, err, "store the backup: put "+fmt.Sprintf("%q", want.Path)+": interrupted")
		wantNoBackup(t, svc.Store)
		wantStored(t, svc.Store, versionPath, []byte("v0.9.0\n"))
		wantLockFree(t, svc.Store)
		if got := mustBackup(t, svc); got.Path != want.Path {
			t.Errorf("the next backup is at %s, want %s", got.Path, want.Path)
		}
	})
}

// TestBackupCreateLosesTheLockAfterTheWrite returns the backup with an error that says it was saved.
func TestBackupCreateLosesTheLockAfterTheWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		now := fixClock(svc)
		want := backupOf(t, now, markOf(t, svc.Store))
		svc.Store = forceUnlocked{unwrapped{svc.Store}, want.Path}

		got, err := svc.BackupCreate(t.Context(), "prod")

		wantError(t, err, "the change is saved, but the lock of cluster prod was lost before tent released it")
		if !errors.Is(err, statestore.ErrLockLost) || !app.Saved(err) {
			t.Errorf("error %v: matches ErrLockLost %t, Saved %t; want both", err, errors.Is(err, statestore.ErrLockLost),
				app.Saved(err))
		}
		if diff := cmp.Diff(app.StoredBackup{Cluster: "prod", Backup: want, Size: len(firstSnapshot)}, got); diff != "" {
			t.Errorf("result (-want +got):\n%s", diff)
		}
		wantStored(t, svc.Store, want.Path, []byte(firstSnapshot))
	})
}

// TestBackupCreatePlansAgainUnderTheLock changes the bootstrap mark while the run waits for the lock: the backup is of
// the servers that the mark names once the run holds the lock.
func TestBackupCreatePlansAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		held := holdLock(t, svc.Store)
		svc.LockTimeout = time.Minute
		done := make(chan app.StoredBackup, 1)
		go func() {
			b, err := svc.BackupCreate(t.Context(), "prod")
			if err != nil {
				t.Errorf("BackupCreate: %v", err)
			}
			done <- b
		}()
		synctest.Wait()

		// What another tent does while it holds the lock: it builds new servers, which writes a new mark.
		later := markOf(t, svc.Store).Add(time.Hour)
		put(t, svc.Store, markPath, []byte(later.Format(time.RFC3339)+"\n"))
		release(t, held)
		got := <-done

		want := backupOf(t, got.Time, later)
		if got.Backup != want {
			t.Errorf("backup = %+v, want %+v: of the new mark", got.Backup, want)
		}
		if paths := list(t, svc.Store, backupPrefix); !slices.Equal(paths, []string{want.Path}) {
			t.Errorf("backups = %v, want [%s]", paths, want.Path)
		}
	})
}

// TestBackupCreateShowsNoSnapshotBytes checks the result in both forms, a warning and each error against the bytes of
// Nomad's snapshots.
func TestBackupCreateShowsNoSnapshotBytes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		var warnings []string
		svc.OnWarning = func(s string) { warnings = append(warnings, s) }
		fixClock(svc)
		refused := 0
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "SaveSnapshot" && refused < 3 {
				refused++
				return errRefused
			}
			return next(ctx)
		})
		got := mustBackup(t, svc)
		var text bytes.Buffer
		if err := got.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		asJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		_, exists := svc.BackupCreate(t.Context(), "prod")
		failSnapshots(w, errRefused)
		_, gaveUp := svc.BackupCreate(t.Context(), "prod")
		failSnapshots(w, errors.New("403 Permission denied"))
		_, denied := svc.BackupCreate(t.Context(), "prod")

		outputs := map[string]string{
			"the text": text.String(), "the JSON": string(asJSON), "the warnings": strings.Join(warnings, "\n"),
		}
		for name, err := range map[string]error{"exists": exists, "gave up": gaveUp, "denied": denied} {
			if err == nil {
				t.Fatalf("%s: no error", name)
			}
			outputs["the error that "+name] = err.Error()
		}
		secrettest.CheckHidden(t, outputs, map[string][]byte{
			"the first snapshot": []byte(firstSnapshot), "the second snapshot": []byte("nomadfake snapshot 2"),
		}, "")
	})
}

// putBackup puts a backup object, of the servers bootstrapped at bootstrapped and taken at taken, into the test
// cluster's store, and returns the backup.
func putBackup(t *testing.T, s statestore.Store, taken, bootstrapped time.Time) statestore.Backup {
	t.Helper()
	b := backupOf(t, taken, bootstrapped)
	put(t, s, b.Path, []byte("snapshot "+b.Name))
	return b
}

// withoutOutsideCalls makes a call of the cloud or of Nomad fail the test, and returns the log of the store's writes.
func withoutOutsideCalls(t *testing.T, svc *app.Service) *writeLog {
	t.Helper()
	svc.Providers = func(p v1alpha1.Provider) (cloud.Provider, error) {
		t.Errorf("a call for the cloud provider %s", p)
		return nil, errors.New("no cloud in this test")
	}
	svc.Nomad = func(nomadops.Config) (nomadops.API, error) {
		t.Error("a call for a Nomad client")
		return nil, errors.New("no Nomad in this test")
	}
	rec := &writeLog{Store: svc.Store}
	svc.Store = rec
	return rec
}

// TestBackupsListsOldestFirst lists three backups oldest first: a backup of the servers that run now is not earlier,
// while one of any other bootstrap is. A call of the cloud or of Nomad fails the test, and Backups writes nothing, not
// even the lock.
func TestBackupsListsOldestFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		mark := markOf(t, svc.Store)
		old := mark.Add(-48 * time.Hour)
		base := mark.Add(-24 * time.Hour)
		third := putBackup(t, svc.Store, base.Add(2*time.Hour), mark)
		first := putBackup(t, svc.Store, base, old)
		second := putBackup(t, svc.Store, base.Add(time.Hour), old)
		rec := withoutOutsideCalls(t, svc)

		got, err := svc.Backups(t.Context(), "prod")

		if err != nil {
			t.Fatalf("Backups: %v", err)
		}
		want := []app.ListedBackup{{Backup: first, Earlier: true}, {Backup: second, Earlier: true}, {Backup: third}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("backups (-want +got):\n%s", diff)
		}
		if len(rec.writes) != 0 {
			t.Errorf("writes %v, want none", rec.writes)
		}
	})
}

// TestBackupsWithoutAMarkAreAllEarlier lists the backups of a cluster whose bootstrap mark is gone: each is earlier,
// also one whose path names the time that the mark held.
func TestBackupsWithoutAMarkAreAllEarlier(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		mark := markOf(t, svc.Store)
		b := putBackup(t, svc.Store, mark.Add(time.Hour), mark)
		deleteObject(t, svc, markPath)

		got, err := svc.Backups(t.Context(), "prod")

		if err != nil {
			t.Fatalf("Backups: %v", err)
		}
		if diff := cmp.Diff([]app.ListedBackup{{Backup: b, Earlier: true}}, got); diff != "" {
			t.Errorf("backups (-want +got):\n%s", diff)
		}
	})
}

// TestBackupsRefuseWhatTheStoreDoesNotHold fails for a cluster that is not in the store and for a mark that holds no
// time, which wraps the parse error and names the mark's path.
func TestBackupsRefuseWhatTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)

		_, err := svc.Backups(t.Context(), "stage")

		if err == nil || !strings.HasPrefix(err.Error(), "cluster stage not found in ") {
			t.Errorf("unknown cluster: error = %v, want it to start with %q", err, "cluster stage not found in ")
		}
		put(t, svc.Store, markPath, []byte("soon\n"))

		_, err = svc.Backups(t.Context(), "prod")

		if err == nil || !strings.HasPrefix(err.Error(), markPath+" does not hold a time: ") {
			t.Fatalf("bad mark: error = %v, want it to start with %q", err, markPath+" does not hold a time: ")
		}
		if _, ok := errors.AsType[*time.ParseError](err); !ok {
			t.Errorf("error %v does not wrap the parse error", err)
		}
	})
}

// TestListedBackupJSON encodes a backup of earlier servers and one of the servers that run now, and a list without
// backups, which is [] and not null.
func TestListedBackupJSON(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		empty, err := svc.Backups(t.Context(), "prod")
		if err != nil {
			t.Fatalf("Backups: %v", err)
		}
		if got, err := json.Marshal(empty); err != nil || string(got) != "[]" {
			t.Errorf("an empty list encodes as %s, %v; want []", got, err)
		}
		taken, bootstrapped := time.Date(2026, 10, 9, 23, 2, 7, 0, time.UTC), time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
		list := []app.ListedBackup{
			{Backup: backupOf(t, taken, bootstrapped), Earlier: true},
			{Backup: backupOf(t, taken, bootstrapped)},
		}

		got, err := json.Marshal(list)

		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		const want = `[{"name":"20261009T230207Z","time":"2026-10-09T23:02:07Z",` +
			`"path":"prod/backups/20261001T100000Z/20261009T230207Z.snap","bootstrap":"20261001T100000Z",` +
			`"servers":"earlier"},` +
			`{"name":"20261009T230207Z","time":"2026-10-09T23:02:07Z",` +
			`"path":"prod/backups/20261001T100000Z/20261009T230207Z.snap","bootstrap":"20261001T100000Z",` +
			`"servers":"current"}]`
		if string(got) != want {
			t.Errorf("JSON = %s\nwant   %s", got, want)
		}
	})
}

// TestListedBackupBootstrapped returns the time of the servers that a backup is of, in UTC.
func TestListedBackupBootstrapped(t *testing.T) {
	t.Parallel()
	taken, bootstrapped := time.Date(2026, 10, 9, 23, 2, 7, 0, time.UTC), time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	b := app.ListedBackup{Backup: backupOf(t, taken, bootstrapped)}

	if got := b.Bootstrapped(); !got.Equal(bootstrapped) || got.Location() != time.UTC {
		t.Errorf("Bootstrapped = %v, want %v in UTC", got, bootstrapped)
	}
}
