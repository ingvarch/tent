package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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
