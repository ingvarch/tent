package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// cutPut ends the run's context just before or just after the first Put of a backup, as Ctrl-C would.
type cutPut struct {
	statestore.Store
	cancel context.CancelFunc
	after  bool
	cut    bool
}

func (c *cutPut) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if !strings.HasPrefix(p, backupPrefix) || c.cut {
		return c.Store.Put(ctx, p, data, opts)
	}
	c.cut = true
	if !c.after {
		c.cancel()
		return "", fmt.Errorf("put %q: %w", p, context.Canceled)
	}
	v, err := c.Store.Put(ctx, p, data, opts)
	c.cancel()
	return v, err
}

// TestBackupCreateIsCut ends the run's context before and after the read of the nodes, the snapshot and the write
// of the backup. The store then holds no backup, or one whole backup when the cut came after the write; the lock is
// free, and the next run stores a backup.
func TestBackupCreateIsCut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		at, name string
		after    bool
		stored   int
	}{
		{"Nodes", "before the nodes", false, 0},
		{"Nodes", "after the nodes", true, 0},
		{"SaveSnapshot", "before the snapshot", false, 0},
		{"SaveSnapshot", "after the snapshot", true, 0},
		{"Put", "before the write", false, 0},
		{"Put", "after the write", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w, _ := builtWorld(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.at == "Put" {
					svc.Store = &cutPut{Store: svc.Store, cancel: cancel, after: tc.after}
				} else {
					w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
						if c.Name != tc.at {
							return next(ctx)
						}
						if !tc.after {
							cancel()
							return ctx.Err()
						}
						err := next(ctx)
						cancel()
						return err
					})
				}

				_, err := svc.BackupCreate(ctx, "prod")

				if tc.stored == 0 && (err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled)) {
					t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
				}
				if tc.stored == 1 && err != nil {
					t.Errorf("error = %v, want none: the backup was stored", err)
				}
				paths := list(t, svc.Store, backupPrefix)
				if len(paths) != tc.stored {
					t.Fatalf("backups = %v, want %d", paths, tc.stored)
				}
				for _, p := range paths {
					if got := string(get(t, svc.Store, p)); got != firstSnapshot {
						t.Errorf("%s holds %q, want one whole snapshot %q", p, got, firstSnapshot)
					}
				}
				wantLockFree(t, svc.Store)

				w.SetHook(nil)
				time.Sleep(time.Second)
				next := mustBackup(t, svc)
				if len(list(t, svc.Store, backupPrefix)) != tc.stored+1 {
					t.Errorf("the next run stored %s, and the store holds %v", next.Path, list(t, svc.Store, backupPrefix))
				}
			})
		})
	}
}

// TestBackupCreateIsCutWhileItWaitsForASnapshot ends the run's context while it waits to ask Nomad again: the run ends
// at once, stores nothing and frees the lock.
func TestBackupCreateIsCutWhileItWaitsForASnapshot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc.OnWarning = func(string) { cancel() } // as Ctrl-C would, while it waits
		calls := failSnapshots(w, errRefused)
		start := time.Now()

		_, err := svc.BackupCreate(ctx, "prod")

		if err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
		}
		if *calls != 3 || time.Since(start) != 0 {
			t.Errorf("%d snapshot calls after %v, want one on each server and no wait", *calls, time.Since(start))
		}
		wantNoBackup(t, svc.Store)
		wantLockFree(t, svc.Store)
	})
}

// cutRecord ends the run's context just before or just after the nth Put of the record of a restore, as Ctrl-C would.
type cutRecord struct {
	statestore.Store
	cancel context.CancelFunc
	nth    int
	after  bool
	puts   int
}

func (c *cutRecord) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if p != restoredPath {
		return c.Store.Put(ctx, p, data, opts)
	}
	c.puts++
	if c.puts != c.nth {
		return c.Store.Put(ctx, p, data, opts)
	}
	if !c.after {
		c.cancel()
		return "", fmt.Errorf("put %q: %w", p, context.Canceled)
	}
	v, err := c.Store.Put(ctx, p, data, opts)
	c.cancel()
	return v, err
}

// TestBackupRestoreIsCut ends the run's context while it plans without the lock, and before and after each write of
// the record and each call that sends or checks the backup. The run that was cut leaves the lock free and the record
// as far as it got; the next run ends with the record's time, and Nomad holds the backup's bytes, sent once or twice.
// A cut after the second write ends the run without an error, and the next run finds the restore done and sends
// nothing.
func TestBackupRestoreIsCut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		at, name    string
		nth         int // of the write of the record, for at "Put"
		after       bool
		sent, total int // snapshots that Nomad took when the run was cut, and after the next run
	}{
		{"Nodes", "before the plan", 0, false, 0, 1},
		{"Put", "before the first write", 1, false, 0, 1},
		{"Put", "after the first write", 1, true, 0, 1},
		{"RestoreSnapshot", "before the snapshot", 0, false, 0, 1},
		{"RestoreSnapshot", "after the snapshot", 0, true, 1, 2},
		{"Bootstrap", "before the check", 0, false, 1, 2},
		{"Bootstrap", "after the check", 0, true, 1, 2},
		{"Put", "before the second write", 2, false, 1, 2},
		{"Put", "after the second write", 2, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.at == "Put" {
					r.svc.Store = &cutRecord{Store: r.svc.Store, cancel: cancel, nth: tc.nth, after: tc.after}
				} else {
					r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
						if c.Name != tc.at {
							return next(ctx)
						}
						if !tc.after {
							cancel()
							return ctx.Err()
						}
						err := next(ctx)
						cancel()
						return err
					})
				}

				_, err := r.svc.BackupRestore(ctx, "prod", app.Latest, true)

				finished := tc.at == "Put" && tc.nth == 2 && tc.after
				if finished && err != nil {
					t.Errorf("error = %v, want none: the restore ended", err)
				}
				if !finished && (err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled)) {
					t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
				}
				if got := len(r.restoredCopies()); got != tc.sent {
					t.Errorf("Nomad took %d snapshots before the next run, want %d", got, tc.sent)
				}
				wantLockFree(t, r.svc.Store)

				r.w.SetHook(nil)
				plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, true)

				if err != nil {
					t.Fatalf("the next run: %v", err)
				}
				if plan.Applied != !finished || plan.Restored != finished {
					t.Errorf("plan = %+v, want Applied %t and Restored %t", plan, !finished, finished)
				}
				copies := r.restoredCopies()
				if len(copies) != tc.total {
					t.Errorf("Nomad took %d snapshots, want %d", len(copies), tc.total)
				}
				for _, c := range copies {
					if c != firstSnapshot {
						t.Errorf("Nomad took %q, want the backup's bytes %q", c, firstSnapshot)
					}
				}
				var rec struct {
					Restored *time.Time `json:"restored"`
				}
				if err := json.Unmarshal(get(t, r.svc.Store, restoredPath), &rec); err != nil || rec.Restored == nil {
					t.Errorf("the record %q has no time: %v", get(t, r.svc.Store, restoredPath), err)
				}
				wantLockFree(t, r.svc.Store)
			})
		})
	}
}

// TestBackupRestoreResumesAfterNomadTookTheBackup cuts a run right after Nomad took the backup, and registers the
// backup's jobs: the next run sees that the bootstrap token has another accessor, reads no job, sends the backup again
// and ends with the time.
func TestBackupRestoreResumesAfterNomadTookTheBackup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "RestoreSnapshot" {
				cancel()
			}
			return err
		})
		if _, err := r.svc.BackupRestore(ctx, "prod", app.Latest, true); !errors.Is(err, context.Canceled) {
			t.Fatalf("the first run: %v, want it cut", err)
		}
		r.w.SetHook(nil)
		if got := accessorOf(t, r.svc, r.w); got == r.accessor {
			t.Fatalf("the accessor is %q after the restore, want the backup's", got)
		}
		r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "from-the-backup"}})
		before := len(r.w.Log())

		plan := r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes || !plan.Taken {
			t.Errorf("plan = %+v, want Applied, Resumes and Taken", plan)
		}
		for _, name := range nomadCallNames(r.w, before) {
			if name == "Jobs" {
				t.Error("the run read the jobs, want none")
			}
		}
		if got := len(r.restoredCopies()); got != 2 {
			t.Errorf("Nomad took %d snapshots, want the cut run's and the resumed run's", got)
		}
		var rec struct {
			Accessor string     `json:"accessor"`
			Restored *time.Time `json:"restored"`
		}
		if err := json.Unmarshal(get(t, r.svc.Store, restoredPath), &rec); err != nil || rec.Restored == nil {
			t.Errorf("the record %q has no time: %v", get(t, r.svc.Store, restoredPath), err)
		}
		if rec.Accessor != r.accessor {
			t.Errorf("the record's accessor is %q, want the one before the restore, %q", rec.Accessor, r.accessor)
		}
	})
}
