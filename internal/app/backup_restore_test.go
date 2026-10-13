package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// rebuildWay is the way out that the refusals of a restore name.
const rebuildWay = "delete the server machines in the cloud and run tent update cluster --yes"

// lostHint is what the refusal of a restore with no earlier backup adds.
const lostHint = "; after a loss, tent update cluster builds new servers only when no server machine is left, so " +
	"delete the remaining ones in the cloud first"

// rebuilt is a test cluster whose servers were built again after a backup: the backup is of the earlier servers.
type rebuilt struct {
	svc     *app.Service
	f       *vultrfake.Fake
	w       *nomadWorld
	backup  app.StoredBackup // of the earlier servers; it holds firstSnapshot
	earlier time.Time        // when the earlier servers were bootstrapped
	mark    time.Time        // when the servers that run now were bootstrapped
	// accessor is the accessor of the bootstrap token of the servers that run now, before any restore.
	accessor string
}

// bootstrapSecretPath is where the state store keeps the ACL bootstrap secret of the test cluster.
const bootstrapSecretPath = "prod/secrets/acl-bootstrap-token"

// accessorOf returns the accessor that the stored bootstrap secret has on the servers of the world now.
func accessorOf(t *testing.T, svc *app.Service, w *nomadWorld) string {
	t.Helper()
	token := secret.Secret(get(t, svc.Store, bootstrapSecretPath))
	got, err := w.Client(nomadops.Config{Token: token}).TokenAccessor(t.Context())
	if err != nil {
		t.Fatalf("TokenAccessor: %v", err)
	}
	return got
}

// rebuiltWorld builds the test cluster, stores a backup, deletes its servers and runs update, which bootstraps new
// ones. It stops the test when update does not write a newer mark.
func rebuiltWorld(t *testing.T) rebuilt {
	t.Helper()
	svc, f, w, _ := builtWorld(t)
	earlier := markOf(t, svc.Store)
	backup := mustBackup(t, svc)
	deleteServers(t, f)
	time.Sleep(time.Hour)
	mustUpdate(t, svc)
	mark := markOf(t, svc.Store)
	if !mark.After(earlier) {
		t.Fatalf("update after the loss of the servers left the mark at %v, want a time after %v: "+
			"a restore needs the new servers' mark", mark, earlier)
	}
	return rebuilt{svc: svc, f: f, w: w, backup: backup, earlier: earlier, mark: mark,
		accessor: accessorOf(t, svc, w)}
}

// utc writes a time as the texts of a restore do.
func utc(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 MST") }

// planText is the text of a plan for the backup of r that sends it for the first time.
func (r rebuilt) planText(nodes string) string {
	return fmt.Sprintf("Restore backup %s (taken %s) into cluster prod.\n"+
		"The backup is of servers bootstrapped on %s; the servers that run now were bootstrapped on %s.\n"+
		"They run no job and list %s.\n"+
		"Nomad's jobs, nodes, ACL tokens, variables and keyring become those of the backup.\n",
		r.backup.Name, utc(r.backup.Time), utc(r.earlier), utc(r.mark), nodes)
}

// record returns the record of a restore of the backup named backup into the servers bootstrapped at mark, as tent
// writes it: with the accessor and the time restored when they are not empty.
func record(backup, accessor string, mark time.Time, restored string) string {
	rec := fmt.Sprintf(`{"backup":%q,"bootstrap":%q`, backup, statestore.BackupName(mark))
	if accessor != "" {
		rec += fmt.Sprintf(`,"accessor":%q`, accessor)
	}
	if restored != "" {
		rec += fmt.Sprintf(`,"restored":%q`, restored)
	}
	return rec + "}\n"
}

// restore runs a restore of the backup that from names, with apply, and returns the plan and the error.
func (r rebuilt) restore(from string) (app.RestorePlan, error) {
	return r.svc.BackupRestore(context.Background(), "prod", from, true)
}

// mustRestore restores the backup that from names and stops the test on an error.
func (r rebuilt) mustRestore(t *testing.T, from string) app.RestorePlan {
	t.Helper()
	plan, err := r.restore(from)
	if err != nil {
		t.Fatalf("BackupRestore: %v", err)
	}
	return plan
}

// restoredCopies returns the snapshots that Nomad restored, as strings.
func (r rebuilt) restoredCopies() []string {
	var got []string
	for _, snap := range r.w.Restored() {
		got = append(got, string(snap))
	}
	return got
}

// wantNoSnapshotTaken fails the test when Nomad restored a snapshot.
func (r rebuilt) wantNoSnapshotTaken(t *testing.T) {
	t.Helper()
	if got := r.restoredCopies(); len(got) != 0 {
		t.Errorf("Nomad restored %d snapshots, want none", len(got))
	}
}

// wantNotRestored fails the test unless Nomad restored nothing and the store holds no record.
func (r rebuilt) wantNotRestored(t *testing.T) {
	t.Helper()
	r.wantNoSnapshotTaken(t)
	r.wantNotRestoredRecord(t)
}

// timeline records, in order, the calls to Nomad and the writes to the store, but not the lock's.
type timeline struct {
	mu     sync.Mutex
	events []string
}

func (tl *timeline) add(event string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, event)
}

// loggedStore adds each write that is not the lock's to a timeline, and tells the record that has a time from the one
// that has none.
type loggedStore struct {
	statestore.Store
	tl *timeline
}

func (s loggedStore) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	switch {
	case p == lockPath:
	case p == restoredPath && bytes.Contains(data, []byte(`"restored"`)):
		s.tl.add("write " + p + " with the time")
	case p == restoredPath:
		s.tl.add("write " + p + " without a time")
	default:
		s.tl.add("write " + p)
	}
	return s.Store.Put(ctx, p, data, opts)
}

// logTo makes the store and the Nomad world of r add their events to a new timeline, and returns it. It calls onCall,
// when that is set, before each call to Nomad.
func (r rebuilt) logTo(onCall func(c nomadfake.Call)) *timeline {
	tl := &timeline{}
	r.svc.Store = loggedStore{Store: unwrapped{r.svc.Store}, tl: tl}
	r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		tl.add("nomad " + c.Name)
		if onCall != nil {
			onCall(c)
		}
		return next(ctx)
	})
	return tl
}

// TestBackupRestorePlansWithoutApply plans the restore of the earlier backup: the plan holds the backup, the time of
// the bootstrap and the nodes, in text and JSON. Nomad gets one read of the nodes and one of the jobs, the store gets
// no write, not even the lock's, and nothing reaches Nomad that writes.
func TestBackupRestorePlansWithoutApply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		rec := &writeLog{Store: r.svc.Store}
		r.svc.Store = rec
		before := len(r.w.Log())

		plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)

		if err != nil {
			t.Fatalf("BackupRestore: %v", err)
		}
		want := app.RestorePlan{Cluster: "prod", Backup: r.backup.Backup, Bootstrapped: r.mark.UTC(), Nodes: 2}
		if diff := cmp.Diff(want, plan); diff != "" {
			t.Errorf("plan (-want +got):\n%s", diff)
		}
		var text bytes.Buffer
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		if text.String() != r.planText("2 nodes") {
			t.Errorf("WriteText =\n%s\nwant\n%s", text.String(), r.planText("2 nodes"))
		}
		got, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		wantJSON := fmt.Sprintf(`{"cluster":"prod","backup":{"name":%q,"time":%q,"path":%q},`+
			`"bootstrapped":%q,"nodes":2}`, r.backup.Name, r.backup.Time.UTC().Format(time.RFC3339), r.backup.Path,
			r.mark.UTC().Format(time.RFC3339))
		if string(got) != wantJSON {
			t.Errorf("JSON = %s\nwant   %s", got, wantJSON)
		}
		if names := nomadCallNames(r.w, before); !slices.Equal(names, []string{"Nodes", "Jobs"}) {
			t.Errorf("calls of Nomad = %v, want one Nodes and one Jobs", names)
		}
		if len(rec.writes) != 0 {
			t.Errorf("writes %v, want none", rec.writes)
		}
		r.wantNotRestored(t)
	})
}

// TestBackupRestorePlanCountsOneNode writes "1 node" for a Nomad that lists one node.
func TestBackupRestorePlanCountsOneNode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.w.DropNode("prod-workers-1")

		plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)

		if err != nil {
			t.Fatalf("BackupRestore: %v", err)
		}
		var text bytes.Buffer
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		if plan.Nodes != 1 || text.String() != r.planText("1 node") {
			t.Errorf("nodes %d, text\n%s\nwant 1 node and\n%s", plan.Nodes, text.String(), r.planText("1 node"))
		}
	})
}

// TestBackupRestoreApplies restores the earlier backup: Nomad gets the reads of the plan twice, the snapshot from the
// store, once, and the check of the secret; the store gets the version, the record without a time before the snapshot
// and the record with the time after the check. The lease names the operation at the snapshot, and the lock is free
// at the end.
func TestBackupRestoreApplies(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.svc.Version = "v0.9.0"
		now := fixClock(r.svc)
		var leased []string
		tl := r.logTo(func(c nomadfake.Call) {
			if c.Name == "RestoreSnapshot" {
				if h := holder(t, r.svc.Store); h != nil {
					leased = append(leased, h.Operation)
				}
			}
		})
		before := len(r.w.Log())

		plan := r.mustRestore(t, app.Latest)

		want := app.RestorePlan{
			Cluster: "prod", Backup: r.backup.Backup, Bootstrapped: r.mark.UTC(), Nodes: 2, Applied: true,
		}
		if diff := cmp.Diff(want, plan); diff != "" {
			t.Errorf("plan (-want +got):\n%s", diff)
		}
		wantEvents := []string{
			"nomad Nodes", "nomad Jobs", "nomad Nodes", "nomad Jobs", "write " + versionPath,
			"nomad TokenAccessor", "write " + restoredPath + " without a time",
			"nomad RestoreSnapshot", "nomad Bootstrap", "write " + restoredPath + " with the time",
		}
		if diff := cmp.Diff(wantEvents, tl.events); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
		for _, c := range r.w.Log()[before:] {
			if c.Name == "RestoreSnapshot" && c.Arg != secret.Secret(firstSnapshot).String() {
				t.Errorf("RestoreSnapshot got %s, want %s", c.Arg, secret.Secret(firstSnapshot))
			}
		}
		if diff := cmp.Diff([]string{firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"backup restore"}, leased); diff != "" {
			t.Errorf("leases at the snapshot (-want +got):\n%s", diff)
		}
		wantStored(t, r.svc.Store, restoredPath,
			[]byte(record(r.backup.Name, r.accessor, r.mark, now.UTC().Format(time.RFC3339Nano))))
		wantLockFree(t, r.svc.Store)
		var applied bytes.Buffer
		if err := plan.WriteApplied(&applied); err != nil {
			t.Fatalf("WriteApplied: %v", err)
		}
		if wantLine := "Restored: backup " + r.backup.Name + " into cluster prod.\n"; applied.String() != wantLine {
			t.Errorf("WriteApplied = %q, want %q", applied.String(), wantLine)
		}
		got, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		wantJSON := fmt.Sprintf(`{"cluster":"prod","backup":{"name":%q,"time":%q,"path":%q},`+
			`"bootstrapped":%q,"nodes":2,"applied":true}`, r.backup.Name, r.backup.Time.UTC().Format(time.RFC3339),
			r.backup.Path, r.mark.UTC().Format(time.RFC3339))
		if string(got) != wantJSON {
			t.Errorf("JSON = %s\nwant   %s", got, wantJSON)
		}
	})
}

// TestBackupRestoreIsDoneOnce runs the restore a second time, with and without apply: the plan says that the servers
// were restored from this backup, in text and JSON, and the run makes no call to the cloud or Nomad, takes no lock and
// writes nothing.
func TestBackupRestoreIsDoneOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.mustRestore(t, app.Latest)
		rec := withoutOutsideCalls(t, r.svc)

		for _, apply := range []bool{false, true} {
			plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, apply)

			if err != nil {
				t.Fatalf("BackupRestore with apply %t: %v", apply, err)
			}
			want := app.RestorePlan{
				Cluster: "prod", Backup: r.backup.Backup, Bootstrapped: r.mark.UTC(), Restored: true,
			}
			if diff := cmp.Diff(want, plan); diff != "" {
				t.Errorf("plan with apply %t (-want +got):\n%s", apply, diff)
			}
			var text bytes.Buffer
			if err := plan.WriteText(&text); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
			if wantText := "cluster prod is already restored from backup " + r.backup.Name + "\n"; text.String() != wantText {
				t.Errorf("WriteText = %q, want %q", text.String(), wantText)
			}
			got, err := json.Marshal(plan)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			wantJSON := fmt.Sprintf(`{"cluster":"prod","backup":{"name":%q,"time":%q,"path":%q},`+
				`"bootstrapped":%q,"restored":true}`, r.backup.Name, r.backup.Time.UTC().Format(time.RFC3339),
				r.backup.Path, r.mark.UTC().Format(time.RFC3339))
			if string(got) != wantJSON {
				t.Errorf("JSON = %s\nwant   %s", got, wantJSON)
			}
		}
		if len(rec.writes) != 0 {
			t.Errorf("writes %v, want none", rec.writes)
		}
		if got := r.restoredCopies(); len(got) != 1 {
			t.Errorf("Nomad restored %d snapshots, want one", len(got))
		}
	})
}

// jobsWere is the text of the refusal of a restore into servers that run jobs.
func (r rebuilt) jobsWere(count string, were string) string {
	return fmt.Sprintf("cluster prod runs %s that %s registered after its servers were built again on %s; "+
		"a restore replaces them with the backup's. tent restores a backup only into servers that run no job: "+
		"purge the jobs first, or %s", count, were, utc(r.mark), rebuildWay)
}

// TestBackupRestoreRefusesWhileNomadRunsJobs refuses the restore, with and without apply, for one job and for twelve:
// the text, no record and no snapshot sent.
func TestBackupRestoreRefusesWhileNomadRunsJobs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		jobs        int
		count, were string
	}{
		{1, "1 job", "was"},
		{12, "12 jobs", "were"},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s apply %t", tc.count, apply), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					r := rebuiltWorld(t)
					var jobs []nomadops.Job
					for i := range tc.jobs {
						jobs = append(jobs, nomadops.Job{Namespace: "default", ID: fmt.Sprintf("job-%d", i)})
					}
					r.w.SetJobs(jobs)
					rec := &writeLog{Store: r.svc.Store}
					r.svc.Store = rec

					_, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, apply)

					wantError(t, err, r.jobsWere(tc.count, tc.were))
					r.wantNotRestored(t)
					if len(rec.writes) != 0 {
						t.Errorf("writes %v, want none", rec.writes)
					}
				})
			})
		}
	}
}

// TestBackupRestoreStopsWhenTheJobsCannotBeRead ends the run with the error of the read, before the lock.
func TestBackupRestoreStopsWhenTheJobsCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.w.Fail(t, "Jobs", errPermanent)
		rec := &writeLog{Store: r.svc.Store}
		r.svc.Store = rec

		_, err := r.restore(app.Latest)

		if err == nil || !strings.HasPrefix(err.Error(), "list the jobs: ") || !errors.Is(err, errPermanent) {
			t.Errorf("error = %v, want %q and the cause", err, "list the jobs: ")
		}
		r.wantNotRestored(t)
		if len(rec.writes) != 0 {
			t.Errorf("writes %v, want none", rec.writes)
		}
	})
}

// tookAccessor is the accessor that a record names when Nomad took the backup after the record was written: the
// servers' bootstrap token has another one now.
const tookAccessor = "00000000-0000-4000-8000-0000000000aa"

// resumeText is the third line of the plan of a resumed restore, with the lines around it.
func (r rebuilt) resumeText(state string) string {
	return fmt.Sprintf("Restore backup %s (taken %s) into cluster prod.\n"+
		"The backup is of servers bootstrapped on %s; the servers that run now were bootstrapped on %s.\n%s\n"+
		"Nomad's jobs, nodes, ACL tokens, variables and keyring become those of the backup.\n",
		r.backup.Name, utc(r.backup.Time), utc(r.earlier), utc(r.mark), state)
}

// TestBackupRestoreResumesARestoreThatNomadTook finds a record of this backup without a time whose accessor is not the
// bootstrap token's any more, so Nomad took the backup, while Nomad lists jobs: the plan says that, in text and JSON,
// and asks for no jobs; the run sends the backup, checks the secret and writes the time, and the record keeps its
// accessor.
func TestBackupRestoreResumesARestoreThatNomadTook(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, tookAccessor, r.mark, "")))
		r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "from-the-backup"}})
		now := fixClock(r.svc)
		before := len(r.w.Log())

		plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)

		if err != nil {
			t.Fatalf("BackupRestore: %v", err)
		}
		want := app.RestorePlan{
			Cluster: "prod", Backup: r.backup.Backup, Bootstrapped: r.mark.UTC(), Nodes: 2, Resumes: true, Taken: true,
		}
		if diff := cmp.Diff(want, plan); diff != "" {
			t.Errorf("plan (-want +got):\n%s", diff)
		}
		var text bytes.Buffer
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		wantText := r.resumeText("A restore of this backup was started and Nomad took the backup, but it has not ended: " +
			"tent sends the backup again. The servers list 2 nodes.")
		if text.String() != wantText {
			t.Errorf("WriteText =\n%s\nwant\n%s", text.String(), wantText)
		}
		got, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.HasSuffix(string(got), `"nodes":2,"resumes":true,"taken":true}`) {
			t.Errorf("JSON = %s, want it to end with nodes, resumes and taken", got)
		}
		if names := nomadCallNames(r.w, before); !slices.Equal(names, []string{"Nodes", "TokenAccessor"}) {
			t.Errorf("calls of Nomad = %v, want Nodes and TokenAccessor and no Jobs", names)
		}

		tl := r.logTo(nil)

		plan = r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes || !plan.Taken {
			t.Errorf("plan = %+v, want Applied, Resumes and Taken", plan)
		}
		wantEvents := []string{
			"nomad Nodes", "nomad TokenAccessor", "nomad Nodes", "nomad TokenAccessor", "nomad RestoreSnapshot",
			"nomad Bootstrap", "write " + restoredPath + " with the time",
		}
		if diff := cmp.Diff(wantEvents, tl.events); diff != "" {
			t.Errorf("events: no Jobs and no new record without a time (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
		wantStored(t, r.svc.Store, restoredPath,
			[]byte(record(r.backup.Name, tookAccessor, r.mark, now.UTC().Format(time.RFC3339Nano))))
	})
}

// failSends makes every RestoreSnapshot of the world fail as a network error does, with the snapshot never reaching
// Nomad.
func (r rebuilt) failSends() {
	r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
		if c.Name == "RestoreSnapshot" {
			return fmt.Errorf("dial tcp: i/o timeout: %w", nomadops.ErrNotReady)
		}
		return next(ctx)
	})
}

// TestBackupRestoreResumesAFailedSend sends the backup again, after a run whose send reached no server, when Nomad
// lists no job: the accessor is the one in the record, so Nomad did not take the backup, the plan says that and counts
// no job, and the run ends with the time and the record's accessor.
func TestBackupRestoreResumesAFailedSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.failSends()
		if _, err := r.restore(app.Latest); !errors.Is(err, nomadops.ErrNotReady) {
			t.Fatalf("the first run: %v, want the network error", err)
		}
		r.w.SetHook(nil)
		now := fixClock(r.svc)
		before := len(r.w.Log())

		plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)

		if err != nil {
			t.Fatalf("BackupRestore: %v", err)
		}
		want := app.RestorePlan{
			Cluster: "prod", Backup: r.backup.Backup, Bootstrapped: r.mark.UTC(), Nodes: 2, Resumes: true,
		}
		if diff := cmp.Diff(want, plan); diff != "" {
			t.Errorf("plan (-want +got):\n%s", diff)
		}
		var text bytes.Buffer
		if err := plan.WriteText(&text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		wantText := r.resumeText("A restore of this backup was started and Nomad did not take the backup: tent sends " +
			"it again. They run no job and list 2 nodes.")
		if text.String() != wantText {
			t.Errorf("WriteText =\n%s\nwant\n%s", text.String(), wantText)
		}
		if names := nomadCallNames(r.w, before); !slices.Equal(names, []string{"Nodes", "TokenAccessor", "Jobs"}) {
			t.Errorf("calls of Nomad = %v, want Nodes, TokenAccessor and Jobs", names)
		}

		plan = r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes || plan.Taken {
			t.Errorf("plan = %+v, want Applied and Resumes, and no Taken", plan)
		}
		if diff := cmp.Diff([]string{firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
		wantStored(t, r.svc.Store, restoredPath,
			[]byte(record(r.backup.Name, r.accessor, r.mark, now.UTC().Format(time.RFC3339Nano))))
	})
}

// TestBackupRestoreRefusesJobsAfterAFailedSend refuses, with the text for jobs and with and without apply, a restore
// whose earlier run never reached Nomad when jobs were registered in the meantime, however much later: the record stays
// as it was, and once the jobs are gone the run sends the backup.
func TestBackupRestoreRefusesJobsAfterAFailedSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.failSends()
		if _, err := r.restore(app.Latest); !errors.Is(err, nomadops.ErrNotReady) {
			t.Fatalf("the first run: %v, want the network error", err)
		}
		r.w.SetHook(nil)
		var jobs []nomadops.Job
		for i := range 12 {
			jobs = append(jobs, nomadops.Job{Namespace: "default", ID: fmt.Sprintf("prod-%d", i)})
		}
		r.w.SetJobs(jobs)
		time.Sleep(30 * 24 * time.Hour)

		for _, apply := range []bool{false, true} {
			_, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, apply)

			wantError(t, err, r.jobsWere("12 jobs", "were"))
		}
		r.wantNoSnapshotTaken(t)
		wantStored(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))

		r.w.SetJobs(nil)
		plan := r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes {
			t.Errorf("plan = %+v, want Applied and Resumes once the jobs are gone", plan)
		}
	})
}

// TestBackupRestoreTreatsARecordWithoutAnAccessorAsNotTaken applies the rule for jobs to a record that holds no
// accessor, so that nothing is sent over jobs on a record that cannot tell.
func TestBackupRestoreTreatsARecordWithoutAnAccessorAsNotTaken(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, "", r.mark, "")))
		r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "new"}})

		_, err := r.restore(app.Latest)

		wantError(t, err, r.jobsWere("1 job", "was"))
		r.wantNoSnapshotTaken(t)
		r.w.SetJobs(nil)

		plan := r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes || plan.Taken {
			t.Errorf("plan = %+v, want Applied and Resumes, and no Taken", plan)
		}
	})
}

// TestBackupRestoreNamesAFailedRead ends with the cause when the accessor of tent's token cannot be read: in the plan
// of a resumed restore, before the lock, and under the lock for a first run, where nothing is written and nothing sent.
func TestBackupRestoreNamesAFailedRead(t *testing.T) {
	t.Parallel()
	for _, resume := range []bool{true, false} {
		t.Run(fmt.Sprintf("resume %t", resume), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				if resume {
					put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))
				}
				r.w.Fail(t, "TokenAccessor", errPermanent)
				rec := &writeLog{Store: r.svc.Store}
				r.svc.Store = rec

				_, err := r.restore(app.Latest)

				if err == nil || !strings.HasPrefix(err.Error(), "read the accessor of tent's token: ") ||
					!errors.Is(err, errPermanent) {
					t.Errorf("error = %v, want the read and the cause", err)
				}
				r.wantNoSnapshotTaken(t)
				if resume && len(rec.writes) != 0 {
					t.Errorf("writes %v, want none: the plan fails before the lock", rec.writes)
				}
				if !resume {
					if paths := list(t, r.svc.Store, restoredPath); len(paths) != 0 {
						t.Errorf("the store holds the record %v, want none", paths)
					}
				}
				wantLockFree(t, r.svc.Store)
			})
		})
	}
}

// TestBackupRestoreIgnoresTheRecordOfOtherServers meets a record of earlier servers, which names this or another
// backup, with and without a time, and the empty record: while Nomad lists a job the refusal is the plain one for jobs,
// and without a job the run restores and the record names these servers.
func TestBackupRestoreIgnoresTheRecordOfOtherServers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		record func(r rebuilt) string
	}{
		{"another backup without a time", func(r rebuilt) string { return record("20000101T000000Z", "", r.earlier, "") }},
		{"this backup without a time", func(r rebuilt) string { return record(r.backup.Name, "", r.earlier, "") }},
		{"another backup with a time", func(r rebuilt) string {
			return record("20000101T000000Z", "", r.earlier, "2026-10-10T08:20:00Z")
		}},
		{"this backup with a time", func(r rebuilt) string {
			return record(r.backup.Name, "", r.earlier, "2026-10-10T08:20:00Z")
		}},
		{"no fields", func(rebuilt) string { return "{}\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				old := tc.record(r)
				put(t, r.svc.Store, restoredPath, []byte(old))
				r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "new"}})

				_, err := r.restore(app.Latest)

				wantError(t, err, r.jobsWere("1 job", "was"))
				wantStored(t, r.svc.Store, restoredPath, []byte(old))
				r.w.SetJobs(nil)
				now := fixClock(r.svc)

				plan := r.mustRestore(t, app.Latest)

				if !plan.Applied || plan.Resumes {
					t.Errorf("plan = %+v, want Applied and no Resumes", plan)
				}
				wantStored(t, r.svc.Store, restoredPath,
					[]byte(record(r.backup.Name, r.accessor, r.mark, now.UTC().Format(time.RFC3339Nano))))
			})
		})
	}
}

// failingGet fails the Get of one path.
type failingGet struct {
	statestore.Store
	path string
}

func (f failingGet) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	if p == f.path {
		return nil, "", fmt.Errorf("get %q: interrupted", p)
	}
	return f.Store.Get(ctx, p)
}

// TestBackupRestoreNamesABackupThatCannotBeRead ends with the backup's name and the cause before the record and before
// anything is sent.
func TestBackupRestoreNamesABackupThatCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.svc.Store = failingGet{Store: r.svc.Store, path: r.backup.Path}

		_, err := r.restore(app.Latest)

		wantError(t, err, fmt.Sprintf("read backup %s: get %q: interrupted", r.backup.Name, r.backup.Path))
		r.wantNotRestored(t)
		wantLockFree(t, r.svc.Store)
	})
}

// otherSnapshot is the bytes of the second backup that anotherEarlier stores.
const otherSnapshot = "nomadfake snapshot 99"

// anotherEarlier stores a second backup of the earlier servers, taken a minute after the first, and returns it.
func (r rebuilt) anotherEarlier(t *testing.T) statestore.Backup {
	t.Helper()
	b := backupOf(t, r.backup.Time.Add(time.Minute), r.earlier)
	put(t, r.svc.Store, b.Path, []byte(otherSnapshot))
	return b
}

// TestBackupRestoreAnotherBackup asks for a backup other than the one that the record names, in the three states that
// the record can have.
func TestBackupRestoreAnotherBackup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		other := r.anotherEarlier(t)
		done := record(r.backup.Name, "", r.mark, "2026-10-10T08:20:00Z")
		put(t, r.svc.Store, restoredPath, []byte(done))
		before := len(r.w.Log())

		_, err := r.restore(other.Name)

		wantError(t, err, fmt.Sprintf("cluster prod was restored from backup %s on 2026-10-10 08:20:00 UTC; "+
			"tent restores one backup into servers that it built again: to restore another one, %s first",
			r.backup.Name, rebuildWay))
		if names := nomadCallNames(r.w, before); len(names) != 0 {
			t.Errorf("calls of Nomad = %v, want none", names)
		}
		wantStored(t, r.svc.Store, restoredPath, []byte(done))
		if got := r.restoredCopies(); len(got) != 0 {
			t.Errorf("Nomad restored %d snapshots, want none", len(got))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		other := r.anotherEarlier(t)
		started := record(r.backup.Name, "", r.mark, "")
		put(t, r.svc.Store, restoredPath, []byte(started))
		r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "a"}, {Namespace: "other", ID: "b"}})

		_, err := r.restore(other.Name)

		wantError(t, err, fmt.Sprintf("a restore of backup %s into cluster prod was started and has not ended, and the "+
			"cluster runs 2 jobs. Run tent backup restore --from %s --yes to end it; to restore another backup, purge "+
			"the jobs first, or %s", r.backup.Name, r.backup.Name, rebuildWay))
		wantStored(t, r.svc.Store, restoredPath, []byte(started))
		if got := r.restoredCopies(); len(got) != 0 {
			t.Errorf("Nomad restored %d snapshots, want none", len(got))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		other := r.anotherEarlier(t)
		put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, "", r.mark, "")))
		now := fixClock(r.svc)

		plan := r.mustRestore(t, other.Name)

		if !plan.Applied || plan.Resumes || plan.Backup != other {
			t.Errorf("plan = %+v, want Applied, no Resumes and the other backup %+v", plan, other)
		}
		if diff := cmp.Diff([]string{otherSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
		wantStored(t, r.svc.Store, restoredPath,
			[]byte(record(other.Name, r.accessor, r.mark, now.UTC().Format(time.RFC3339Nano))))
	})
}

// TestBackupRestoreRefusesWhatTheStoreDoesNotAllow runs the rules that read the state store alone: they refuse before
// any call to Nomad and before the lock.
func TestBackupRestoreRefusesWhatTheStoreDoesNotAllow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, svc *app.Service, first app.StoredBackup)
		from    string
		want    func(first app.StoredBackup, mark time.Time) string
	}{
		{"current backup by name", nil, "", func(b app.StoredBackup, mark time.Time) string {
			return fmt.Sprintf("backup %s is of the servers that cluster prod runs now, which tent bootstrapped on %s; "+
				"tent restores a backup only into servers that tent update cluster built again after it", b.Name, utc(mark))
		}},
		{"only current backups", nil, app.Latest, func(_ app.StoredBackup, mark time.Time) string {
			return fmt.Sprintf("every backup of cluster prod is of the servers that it runs now, which tent bootstrapped on "+
				"%s; tent restores a backup only into servers that tent update cluster built again after it"+lostHint, utc(mark))
		}},
		{"no backup", func(t *testing.T, svc *app.Service, b app.StoredBackup) { deleteObject(t, svc, b.Path) }, app.Latest,
			func(app.StoredBackup, time.Time) string { return "cluster prod has no backups" }},
		{"unknown name", nil, "20000101T000000Z", func(app.StoredBackup, time.Time) string {
			return "cluster prod has no backup 20000101T000000Z; tent backup list shows its backups"
		}},
		{"invalid name", nil, "yesterday", func(app.StoredBackup, time.Time) string {
			return `invalid backup name "yesterday": want latest or a name that tent backup list shows, ` +
				`such as 20261009T230207Z`
		}},
		{"no mark", func(t *testing.T, svc *app.Service, _ app.StoredBackup) { deleteObject(t, svc, markPath) }, app.Latest,
			func(app.StoredBackup, time.Time) string {
				return "cluster prod: Nomad is not bootstrapped yet (prod/nomad/bootstrapped is missing); " +
					"run tent update cluster --yes first"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w, _ := builtWorld(t)
				mark := markOf(t, svc.Store)
				first := mustBackup(t, svc)
				from := tc.from
				if from == "" {
					from = first.Name
				}
				if tc.prepare != nil {
					tc.prepare(t, svc, first)
				}
				rec := &writeLog{Store: svc.Store}
				svc.Store = rec
				before := len(w.Log())

				_, err := svc.BackupRestore(t.Context(), "prod", from, true)

				wantError(t, err, tc.want(first, mark))
				if n := len(w.Log()) - before; n != 0 || len(rec.writes) != 0 {
					t.Errorf("%d calls of Nomad and writes %v, want none", n, rec.writes)
				}
			})
		})
	}
}

// TestBackupRestoreLatestSkipsTheServersThatRun restores the earlier backup although a backup of the servers that run
// now is newer.
func TestBackupRestoreLatestSkipsTheServersThatRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		time.Sleep(time.Second)
		current := mustBackup(t, r.svc)
		if !current.Time.After(r.backup.Time) {
			t.Fatalf("the backup of the new servers is not newer: %v and %v", current.Time, r.backup.Time)
		}

		plan := r.mustRestore(t, app.Latest)

		if plan.Backup != r.backup.Backup {
			t.Errorf("restored %+v, want the earlier backup %+v", plan.Backup, r.backup.Backup)
		}
		if diff := cmp.Diff([]string{firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
	})
}

// deleteAllServers deletes every server machine that the fake lists now, whatever its ID.
func deleteAllServers(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	for _, in := range f.Instances() {
		if strings.Contains(in.Label, "-servers-") {
			if err := f.DeleteInstance(t.Context(), in.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestBackupRestoreLatestRefusesBackupsOfTwoRebuilds loses the servers twice: a backup of the empty servers of the
// first rebuild sits between the backup of the workload and the second rebuild. latest does not take the newer of the
// two earlier backups: it refuses and names both, the oldest bootstrap first, and sends nothing to Nomad. --from with
// the backup of the workload restores it.
func TestBackupRestoreLatestRefusesBackupsOfTwoRebuilds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		time.Sleep(time.Minute)
		empty := mustBackup(t, r.svc)
		deleteAllServers(t, r.f)
		time.Sleep(time.Hour)
		mustUpdate(t, r.svc)
		third := markOf(t, r.svc.Store)
		before := len(r.w.Log())

		_, err := r.restore(app.Latest)

		wantError(t, err, fmt.Sprintf("cluster prod has earlier backups of 2 sets of servers: %s of the servers "+
			"bootstrapped on %s and %s of the servers bootstrapped on %s; name one with --from",
			r.backup.Name, utc(r.earlier), empty.Name, utc(r.mark)))
		if n := len(r.w.Log()) - before; n != 0 {
			t.Errorf("%d calls of Nomad, want none", n)
		}

		plan := r.mustRestore(t, r.backup.Name)

		if plan.Backup != r.backup.Backup || !plan.Applied || !plan.Bootstrapped.Equal(third) {
			t.Errorf("plan = %+v, want the backup of the workload %+v applied into the servers bootstrapped at %v",
				plan, r.backup.Backup, third)
		}
		if diff := cmp.Diff([]string{firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
	})
}

// lockedOut is the text of the lock-out after a restore: tent's secret is no token of the servers.
func (r rebuilt) lockedOut() string {
	return fmt.Sprintf("the restore replaced the ACL tokens of cluster prod, and the ACL bootstrap secret of the state "+
		"store is none of them: tent can no longer call the cluster. The object %s is no backup that tent took of "+
		"this cluster. Delete the server machines in the cloud, run tent update cluster --yes, and restore another "+
		"backup with tent backup restore --from NAME, since latest takes this one again", r.backup.Path)
}

// TestBackupRestoreLockOut ends with the lock-out text when the stored secret no longer works after the restore,
// leaves the record without a time and does not name the backup's bytes. The next run, with and without apply, ends
// with the same text and sends nothing.
func TestBackupRestoreLockOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			err := next(ctx)
			if c.Name == "RestoreSnapshot" {
				r.w.SetBootstrapped(secret.Secret("11111111-2222-4333-8444-555555555555"))
			}
			return err
		})

		plan, err := r.restore(app.Latest)

		wantError(t, err, r.lockedOut())
		if plan.Applied {
			t.Errorf("plan = %+v, want it not applied", plan)
		}
		wantStored(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))
		wantLockFree(t, r.svc.Store)

		for _, apply := range []bool{false, true} {
			_, err = r.svc.BackupRestore(t.Context(), "prod", app.Latest, apply)

			wantError(t, err, r.lockedOut())
		}
		if got := r.restoredCopies(); len(got) != 1 {
			t.Errorf("Nomad took %d snapshots, want the one of the first run", len(got))
		}
		wantStored(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))
		wantLockFree(t, r.svc.Store)
	})
}

// TestBackupRestoreLockOutAtTheReach gives the lock-out text, and not the bare 403, when a resumed restore is refused
// at the first read of Nomad, as a real Nomad answers a secret that is no token. A restore that is no resume keeps the
// plain error: nothing was restored that could have replaced the tokens.
func TestBackupRestoreLockOutAtTheReach(t *testing.T) {
	t.Parallel()
	denied := fmt.Errorf("nomad: GET /v1/nodes: 403: Permission denied: %w", nomadops.ErrDenied)
	for _, tc := range []struct {
		name   string
		resume bool // a record of the backup without a time is stored
		want   func(r rebuilt) string
	}{
		{"a resume", true, func(r rebuilt) string { return r.lockedOut() }},
		{"a first run", false, func(rebuilt) string { return "reach Nomad: " + denied.Error() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				if tc.resume {
					put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))
				}
				r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
					if c.Name == "Nodes" {
						return denied
					}
					return next(ctx)
				})

				_, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)

				wantError(t, err, tc.want(r))
				if !tc.resume && !errors.Is(err, nomadops.ErrDenied) {
					t.Errorf("error = %v, want it to keep matching ErrDenied", err)
				}
			})
		})
	}
}

// TestBackupRestoreNamesAFailedCheck ends with the cause, and with the advice to run the command again, when the
// check of the secret fails for another reason; the record stays without a time.
func TestBackupRestoreNamesAFailedCheck(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		r.w.Fail(t, "Bootstrap", errPermanent)

		_, err := r.restore(app.Latest)

		if err == nil || !strings.HasPrefix(err.Error(), "check tent's access after the restore: ") ||
			!strings.HasSuffix(err.Error(), "; run the command again") || !errors.Is(err, errPermanent) {
			t.Errorf("error = %v, want the check, the cause and the advice", err)
		}
		wantStored(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))

		plan := r.mustRestore(t, app.Latest)

		if !plan.Applied || !plan.Resumes {
			t.Errorf("plan = %+v, want the next run to resume and apply", plan)
		}
	})
}

// TestBackupRestoreNamesAFailedRecord ends with the path and, for the second write, the advice; the first write fails
// before anything reaches Nomad.
func TestBackupRestoreNamesAFailedRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		nth    int
		want   string
		sent   int
		stored func(r rebuilt) string
	}{
		{"the first write", 1, "write " + restoredPath + ": put \"" + restoredPath + "\": interrupted", 0,
			func(rebuilt) string { return "" }},
		{"the second write", 2, "write " + restoredPath + ": put \"" + restoredPath + "\": interrupted; " +
			"run the command again", 1, func(r rebuilt) string { return record(r.backup.Name, r.accessor, r.mark, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				r.svc.Store = &failingNth{Store: r.svc.Store, path: restoredPath, nth: tc.nth}

				_, err := r.restore(app.Latest)

				wantError(t, err, tc.want)
				if got := r.restoredCopies(); len(got) != tc.sent {
					t.Errorf("Nomad restored %d snapshots, want %d", len(got), tc.sent)
				}
				if want := tc.stored(r); want == "" {
					r.wantNotRestoredRecord(t)
				} else {
					wantStored(t, r.svc.Store, restoredPath, []byte(want))
				}
				wantLockFree(t, r.svc.Store)
			})
		})
	}
}

// storedRecord returns the record of a restore that the store holds, or "" when it holds none.
func (r rebuilt) storedRecord(t *testing.T) string {
	t.Helper()
	if len(list(t, r.svc.Store, restoredPath)) == 0 {
		return ""
	}
	return string(get(t, r.svc.Store, restoredPath))
}

// wantNotRestoredRecord fails the test when the store holds the record of a restore.
func (r rebuilt) wantNotRestoredRecord(t *testing.T) {
	t.Helper()
	if paths := list(t, r.svc.Store, restoredPath); len(paths) != 0 {
		t.Errorf("the store holds the record %v, want none", paths)
	}
}

// failingNth fails the nth Put of one path, as an interruption would.
type failingNth struct {
	statestore.Store
	path string
	nth  int
	puts int
}

func (f *failingNth) Put(
	ctx context.Context, p string, data []byte, opts statestore.PutOptions,
) (statestore.Version, error) {
	if p == f.path {
		f.puts++
		if f.puts == f.nth {
			return "", fmt.Errorf("put %q: interrupted", p)
		}
	}
	return f.Store.Put(ctx, p, data, opts)
}

// TestBackupRestoreNamesWhatNomadRefuses puts bytes that are no snapshot at the backup's path: the error names the call
// and each server, shows no byte, leaves the record without a time, and the same backup fails again.
func TestBackupRestoreNamesWhatNomadRefuses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		bad := []byte("not a snapshot, tent-secret-bytes")
		put(t, r.svc.Store, r.backup.Path, bad)
		before := len(r.w.Log())

		_, err := r.restore(app.Latest)

		if err == nil || !strings.HasPrefix(err.Error(), "restore backup "+r.backup.Name+": ") ||
			!strings.HasSuffix(err.Error(), "; run the command again") || !errors.Is(err, nomadops.ErrNotReady) {
			t.Fatalf("error = %v, want the call, the causes and the advice", err)
		}
		if n := countNomadCalls(r.w, before, "RestoreSnapshot"); n != 3 {
			t.Errorf("%d RestoreSnapshot calls, want one on each of three servers", n)
		}
		for _, c := range r.w.Log()[before:] {
			if c.Name == "RestoreSnapshot" && !strings.Contains(err.Error(), c.Server) {
				t.Errorf("error %v does not name the server %s", err, c.Server)
			}
		}
		secrettest.CheckHidden(t, map[string]string{"the error": err.Error()},
			map[string][]byte{"the object": bad}, "")
		wantStored(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, r.accessor, r.mark, "")))
		wantLockFree(t, r.svc.Store)
		if _, err := r.restore(app.Latest); err == nil {
			t.Error("the second run succeeded, want the same refusal")
		}
	})
}

// countNomadCalls returns how many calls called name reached the Nomad fake after the first n.
func countNomadCalls(w *nomadWorld, n int, name string) int {
	count := 0
	for _, got := range nomadCallNames(w, n) {
		if got == name {
			count++
		}
	}
	return count
}

// TestBackupRestoreRefusesAnEmptyBackup ends with the path of the object, sends nothing and writes no record.
func TestBackupRestoreRefusesAnEmptyBackup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		put(t, r.svc.Store, r.backup.Path, nil)
		before := len(r.w.Log())

		_, err := r.restore(app.Latest)

		wantError(t, err, fmt.Sprintf("backup %s of cluster prod holds no snapshot", r.backup.Name))
		if n := countNomadCalls(r.w, before, "RestoreSnapshot"); n != 0 {
			t.Errorf("%d RestoreSnapshot calls, want none", n)
		}
		r.wantNotRestored(t)
		wantLockFree(t, r.svc.Store)
	})
}

// TestBackupRestoreEndsWhenNomadIsNotReachable ends with the reach hint when the nodes cannot be read, before the lock.
func TestBackupRestoreEndsWhenNomadIsNotReachable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		for range 3 {
			r.w.Fail(t, "Nodes", fmt.Errorf("no answer: %w", nomadops.ErrNotReady))
		}
		rec := &writeLog{Store: r.svc.Store}
		r.svc.Store = rec

		_, err := r.restore(app.Latest)

		if err == nil || !strings.HasPrefix(err.Error(), "reach Nomad: ") || !strings.HasSuffix(err.Error(), hintOfAccess) ||
			!errors.Is(err, nomadops.ErrNotReady) {
			t.Errorf("error = %v, want the prefix %q, the reach hint and ErrNotReady", err, "reach Nomad: ")
		}
		r.wantNotRestored(t)
		if len(rec.writes) != 0 {
			t.Errorf("writes %v, want none", rec.writes)
		}
	})
}

// TestBackupRestoreFollowsALostAnswer loses the answer of the snapshot on the first server: the same run sends the
// bytes to the next server and ends applied.
func TestBackupRestoreFollowsALostAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		lost := false
		r.w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name != "RestoreSnapshot" || lost {
				return next(ctx)
			}
			lost = true
			_ = next(ctx)
			return fmt.Errorf("answer lost: %w", nomadops.ErrNotReady)
		})

		plan := r.mustRestore(t, app.Latest)

		if !plan.Applied {
			t.Errorf("plan = %+v, want it applied", plan)
		}
		if diff := cmp.Diff([]string{firstSnapshot, firstSnapshot}, r.restoredCopies()); diff != "" {
			t.Errorf("restored snapshots (-want +got):\n%s", diff)
		}
	})
}

// TestBackupRestoreDecidesAgainUnderTheLock holds the lock while a restore waits, and changes one thing before it lets
// go. Each case fails when the run does not decide again once it holds the lock. After the release Nomad gets only
// the reads that the decision needs, and the record stays as the change left it.
func TestBackupRestoreDecidesAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, r rebuilt)
		want   func(r rebuilt) string // the error, or "" for a run that ends without one
		done   bool                   // the plan says the servers are restored already
		calls  []string               // of Nomad after the release
	}{
		{"a restore of another backup ended", func(t *testing.T, r rebuilt) {
			put(t, r.svc.Store, restoredPath, []byte(record("20000101T000000Z", "", r.mark, "2026-10-10T08:20:00Z")))
		}, func(rebuilt) string {
			return "cluster prod was restored from backup 20000101T000000Z on 2026-10-10 08:20:00 UTC; tent restores " +
				"one backup into servers that it built again: to restore another one, " + rebuildWay + " first"
		}, false, nil},
		{"a restore of this backup ended", func(t *testing.T, r rebuilt) {
			put(t, r.svc.Store, restoredPath, []byte(record(r.backup.Name, "", r.mark, "2026-10-10T08:20:00Z")))
		}, func(rebuilt) string { return "" }, true, nil},
		{"the backup became current", func(t *testing.T, r rebuilt) {
			put(t, r.svc.Store, markPath, []byte(r.earlier.Format(time.RFC3339)+"\n"))
		}, func(r rebuilt) string {
			return fmt.Sprintf("every backup of cluster prod is of the servers that it runs now, which tent bootstrapped "+
				"on %s; tent restores a backup only into servers that tent update cluster built again after it"+lostHint,
				utc(r.earlier))
		}, false, nil},
		{"a job was registered", func(_ *testing.T, r rebuilt) {
			r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "new"}})
		}, func(r rebuilt) string { return r.jobsWere("1 job", "was") }, false, []string{"Nodes", "Jobs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := rebuiltWorld(t)
				held := holdLock(t, r.svc.Store)
				r.svc.LockTimeout = time.Minute
				type result struct {
					plan app.RestorePlan
					err  error
				}
				done := make(chan result, 1)
				go func() {
					plan, err := r.restore(app.Latest)
					done <- result{plan, err}
				}()
				synctest.Wait()
				before := len(r.w.Log())

				tc.change(t, r)
				left := r.storedRecord(t)
				release(t, held)
				got := <-done

				if want := tc.want(r); want != "" {
					wantError(t, got.err, want)
				} else if got.err != nil {
					t.Errorf("error = %v, want none", got.err)
				}
				if got.plan.Restored != tc.done {
					t.Errorf("plan = %+v, want Restored %t", got.plan, tc.done)
				}
				if names := nomadCallNames(r.w, before); !slices.Equal(names, tc.calls) {
					t.Errorf("calls of Nomad after the release = %v, want %v", names, tc.calls)
				}
				if rec := r.storedRecord(t); rec != left {
					t.Errorf("the record is %q, want it as the change left it: %q", rec, left)
				}
				if got := r.restoredCopies(); len(got) != 0 {
					t.Errorf("Nomad restored %d snapshots, want none", len(got))
				}
			})
		})
	}
}

// TestBackupRestoreShowsNoSnapshotBytes checks the text, the JSON and the errors of a restore against the bytes of the
// snapshot that it sends.
func TestBackupRestoreShowsNoSnapshotBytes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := rebuiltWorld(t)
		var warnings []string
		r.svc.OnWarning = func(s string) { warnings = append(warnings, s) }
		outputs := map[string]string{}
		plan, err := r.svc.BackupRestore(t.Context(), "prod", app.Latest, false)
		if err != nil {
			t.Fatalf("BackupRestore: %v", err)
		}
		var text bytes.Buffer
		if err := plan.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		outputs["the plan"] = text.String()
		applied := r.mustRestore(t, app.Latest)
		text.Reset()
		if err := applied.WriteApplied(&text); err != nil {
			t.Fatal(err)
		}
		outputs["the applied line"] = text.String()
		asJSON, err := json.Marshal(applied)
		if err != nil {
			t.Fatal(err)
		}
		outputs["the JSON"] = string(asJSON)
		_, other := r.restore("yesterday")
		outputs["the error of a name"] = other.Error()
		r.w.SetJobs([]nomadops.Job{{Namespace: "default", ID: "x"}})
		put(t, r.svc.Store, restoredPath, []byte("{}"))
		_, jobs := r.restore(app.Latest)
		outputs["the error of the jobs"] = jobs.Error()
		outputs["the warnings"] = strings.Join(warnings, "\n")

		secrettest.CheckHidden(t, outputs, map[string][]byte{"the snapshot": []byte(firstSnapshot)}, "")
	})
}
