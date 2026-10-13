package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

const (
	// firstSnapshot is the first snapshot that the Nomad fake saves, 20 bytes.
	firstSnapshot = "nomadfake snapshot 1"
	// olderSnapshot is a snapshot that the Nomad fake restores, of a backup that an earlier run stored.
	olderSnapshot = "nomadfake snapshot 7"
	// backupTime is how the plan of a restore prints a time.
	backupTime = "2006-01-02 15:04:05 MST"
	// restoreNotice is what a restore that ended tells on stderr.
	restoreNotice = "Nomad now holds the jobs, nodes, ACL tokens, variables and keyring of the backup: run tent " +
		"export nomad again.\n" +
		"Nomad lists the backup's nodes as ready for about 5 minutes whether they answer or not, and it refuses a " +
		"client\nthat registered with these servers before the restore. After 5 minutes run tent validate cluster, " +
		"and replace the\nnodes that it reports with tent rolling-update cluster --yes --force --nodegroups GROUPS.\n"
	restoreHint = "run with --yes to restore it\n"
)

func prodLayout(t *testing.T) statestore.Layout {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// putMark writes the bootstrap mark of cluster prod, as update writes it, and returns its time.
func putMark(t *testing.T, s state, mark time.Time) time.Time {
	t.Helper()
	s.put(t, prodLayout(t).NomadBootstrapped(), mark.UTC().Format(time.RFC3339))
	return mark
}

// markOf returns the time of the bootstrap mark that update wrote.
func markOf(t *testing.T, s state) time.Time {
	t.Helper()
	mark, err := time.Parse(time.RFC3339, strings.TrimSpace(s.objects(t)[prodLayout(t).NomadBootstrapped()]))
	if err != nil {
		t.Fatal(err)
	}
	return mark
}

// putBackup stores a backup of the servers bootstrapped at bootstrapped, taken at taken, that holds snap.
func putBackup(t *testing.T, s state, taken, bootstrapped time.Time, snap string) statestore.Backup {
	t.Helper()
	b := prodLayout(t).Backup(taken, bootstrapped)
	s.put(t, b.Path, snap)
	return b
}

// earlierBackup stores a backup of servers that were bootstrapped long before the mark.
func earlierBackup(t *testing.T, s state, mark time.Time) statestore.Backup {
	t.Helper()
	return putBackup(t, s, mark.Add(-24*time.Hour), mark.Add(-48*time.Hour), olderSnapshot)
}

// backupRun is one run of a backup command on the test cluster that update built.
type backupRun struct {
	result
	nomad *nomadfake.Fake
}

// runBackup executes tent with args against the Vultr fake f and a new Nomad world.
func runBackup(t *testing.T, f *vultrfake.Fake, args ...string) backupRun {
	t.Helper()
	nomad, factory := nomadWorld("servers", "workers", 6)
	return backupRun{runWithNomad(t, onVultr(f), factory, args...), nomad}
}

func backupArgs(s state, sub string, more ...string) []string {
	return append([]string{"backup", sub, "prod", "--state", s.url}, more...)
}

// storedBackups returns the objects under the cluster's backups, by path.
func storedBackups(t *testing.T, s state) map[string]string {
	t.Helper()
	got := map[string]string{}
	for p, data := range s.objects(t) {
		if strings.HasPrefix(p, prodLayout(t).Backups()) {
			got[p] = data
		}
	}
	return got
}

// onlyBackup returns the one backup that the store holds, which has the snapshot snap.
func onlyBackup(t *testing.T, s state, snap string) statestore.Backup {
	t.Helper()
	stored := storedBackups(t, s)
	if len(stored) != 1 {
		t.Fatalf("the store holds the backups %v, want one", stored)
	}
	for p, data := range stored {
		b, ok := prodLayout(t).BackupAt(p)
		if !ok || data != snap {
			t.Fatalf("backup %s holds %q, want %q", p, data, snap)
		}
		return b
	}
	return statestore.Backup{}
}

// restoreText is the plan of a restore of b into the servers bootstrapped at mark, which list nodes nodes and run no
// job.
func restoreText(b statestore.Backup, mark time.Time, nodes int) string {
	return fmt.Sprintf("Restore backup %s (taken %s) into cluster prod.\n"+
		"The servers that run now were bootstrapped on %s; the backup is of earlier servers.\n"+
		"They run no job and list %d nodes.\n"+
		"Nomad's jobs, nodes, ACL tokens, variables and keyring become those of the backup.\n",
		b.Name, b.Time.UTC().Format(backupTime), mark.UTC().Format(backupTime), nodes)
}

// builtBackups builds the test cluster in the bubble and returns its store, its Vultr fake and the time of its mark.
func builtBackups(t *testing.T) (state, *vultrfake.Fake, time.Time) {
	t.Helper()
	s, f := builtCluster(t)
	return s, f, markOf(t, s)
}

// TestBackupCreateStoresTheSnapshot prints the line of the stored backup and keeps Nomad's bytes under the mark.
func TestBackupCreateStoresTheSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)

		got := runBackup(t, f, backupArgs(s, "create")...)

		b := onlyBackup(t, s, firstSnapshot)
		if b.Bootstrap != statestore.BackupName(mark) {
			t.Errorf("the backup is of the servers %s, want the mark's %s", b.Bootstrap, statestore.BackupName(mark))
		}
		wantOK(t, got.result, "stored backup "+b.Name+" of cluster prod (20 bytes)\n")
	})
}

// TestBackupCreatePrintsTheBackupAsData with -o json and -o yaml.
func TestBackupCreatePrintsTheBackupAsData(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, _ := builtBackups(t)

				got := runBackup(t, f, backupArgs(s, "create", "-o", format)...)

				b := onlyBackup(t, s, firstSnapshot)
				want := map[string]any{
					"cluster": "prod", "name": b.Name, "time": b.Time.UTC().Format(time.RFC3339), "path": b.Path,
					"size": float64(20),
				}
				var data map[string]any
				if err := yaml.Unmarshal([]byte(got.out), &data); err != nil || got.code != 0 || got.errOut != "" {
					t.Fatalf("exit code %d, stderr %q, stdout\n%s\nerr %v", got.code, got.errOut, got.out, err)
				}
				if diff := cmp.Diff(want, data); diff != "" {
					t.Errorf("the backup (-want +got):\n%s", diff)
				}
				if format == "json" && !strings.HasPrefix(got.out, "{\n  \"cluster\": \"prod\",") {
					t.Errorf("stdout\n%s\nwant indented JSON", got.out)
				}
			})
		})
	}
}

// TestBackupCreateWarnsOfTheWait tells on stderr, once, that Nomad cannot take a snapshot yet, and then stores it.
func TestBackupCreateWarnsOfTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, _ := builtBackups(t)
		nomad, factory := nomadWorld("servers", "workers", 6)
		refused := fmt.Errorf("500: cannot take snapshot now: %w", nomadops.ErrNotReady)
		for range 3 { // every server, once
			nomad.Fail(t, "SaveSnapshot", refused)
		}

		got := runWithNomad(t, onVultr(f), factory, backupArgs(s, "create")...)

		b := onlyBackup(t, s, firstSnapshot)
		if got.code != 0 || got.out != "stored backup "+b.Name+" of cluster prod (20 bytes)\n" {
			t.Errorf("exit code %d, stdout %q", got.code, got.out)
		}
		if !strings.HasPrefix(got.errOut, "WARNING: Nomad cannot take a snapshot yet: ") ||
			!strings.HasSuffix(got.errOut, "; trying again for up to 5m0s\n") ||
			strings.Count(got.errOut, "WARNING:") != 1 {
			t.Errorf("stderr = %q, want the one warning of the wait", got.errOut)
		}
	})
}

// TestBackupCommandsNeedTheCloud fail with the provider's error before they store anything.
func TestBackupCommandsNeedTheCloud(t *testing.T) {
	for _, args := range [][]string{{"create"}, {"restore", "--yes"}} {
		t.Run(args[0], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _, mark := builtBackups(t)
				earlierBackup(t, s, mark)
				before := s.objects(t)
				noKey := func(v1alpha1.Provider, *slog.Logger) (cloud.Provider, error) {
					return nil, errors.New("VULTR_API_KEY is not set")
				}

				wantError(t, runProviders(t, noKey, backupArgs(s, args[0], args[1:]...)...),
					"Error: VULTR_API_KEY is not set\n")

				s.want(t, before)
			})
		})
	}
}

// TestBackupCommandsTakeNameOrFlag like the other commands that name a cluster.
func TestBackupCommandsTakeNameOrFlag(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		earlier := earlierBackup(t, s, mark)

		create := runBackup(t, f, "backup", "create", "--name", "prod", "--state", s.url)
		list := runBackup(t, f, "backup", "list", "--name", "prod", "--state", s.url)
		restore := runBackup(t, f, "backup", "restore", "--name", "prod", "--state", s.url)

		if create.code != 0 || !strings.HasPrefix(create.out, "stored backup ") {
			t.Errorf("create: exit code %d, stdout %q, stderr %q", create.code, create.out, create.errOut)
		}
		if list.code != 0 || !strings.Contains(list.out, earlier.Name) {
			t.Errorf("list: exit code %d, stdout %q, stderr %q", list.code, list.out, list.errOut)
		}
		wantResult(t, restore.result, 0, restoreText(earlier, mark, 3), restoreHint)
	})
}

// TestBackupListPrintsTheTable of an earlier and a current backup, oldest first, reading the state store alone: no
// cloud and no Nomad.
func TestBackupListPrintsTheTable(t *testing.T) {
	s := withCluster(t)
	mark := putMark(t, s, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC))
	putBackup(t, s, time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC), mark, "current")
	putBackup(t, s, time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		"earlier")

	wantOK(t, runIn(t, "", backupArgs(s, "list")...),
		"NAME               TAKEN                     SERVERS\n"+
			"20261008T030000Z   2026-10-08 03:00:00 UTC   earlier\n"+
			"20261010T090000Z   2026-10-10 09:00:00 UTC   current\n")
}

// TestBackupListPrintsTheBackupsAsData with -o json and -o yaml.
func TestBackupListPrintsTheBackupsAsData(t *testing.T) {
	s := withCluster(t)
	mark := putMark(t, s, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC))
	current := putBackup(t, s, time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC), mark, "current")
	earlier := putBackup(t, s, time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), "earlier")
	want := []map[string]string{
		{"name": earlier.Name, "time": "2026-10-08T03:00:00Z", "path": earlier.Path, "servers": "earlier"},
		{"name": current.Name, "time": "2026-10-10T09:00:00Z", "path": current.Path, "servers": "current"},
	}
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			got := runIn(t, "", backupArgs(s, "list", "-o", format)...)

			var data []map[string]string
			if err := yaml.Unmarshal([]byte(got.out), &data); err != nil || got.code != 0 || got.errOut != "" {
				t.Fatalf("exit code %d, stderr %q, stdout\n%s\nerr %v", got.code, got.errOut, got.out, err)
			}
			if diff := cmp.Diff(want, data); diff != "" {
				t.Errorf("the list (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBackupListWithoutBackups prints the header alone on stdout, as get clusters does, and says on stderr that the
// cluster has none; the data formats print an empty list.
func TestBackupListWithoutBackups(t *testing.T) {
	s := withCluster(t)
	const none = "cluster prod has no backups\n"
	for _, tc := range []struct{ format, out string }{
		{"table", "NAME   TAKEN   SERVERS\n"}, {"json", "[]\n"}, {"yaml", "[]\n"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			wantResult(t, runIn(t, "", backupArgs(s, "list", "-o", tc.format)...), 0, tc.out, none)
		})
	}
}

// TestBackupListFailsForAMissingCluster like the other commands that read a cluster.
func TestBackupListFailsForAMissingCluster(t *testing.T) {
	got := runIn(t, "", "backup", "list", "prod", "--state", newState(t).url)
	if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, "Error: ") {
		t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and an error", got.code, got.out, got.errOut)
	}
}

// TestBackupRestorePrintsThePlan without --yes: the plan with the node count and the hint, and no change.
func TestBackupRestorePrintsThePlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		earlier := earlierBackup(t, s, mark)
		before := s.objects(t)

		got := runBackup(t, f, backupArgs(s, "restore")...)

		wantResult(t, got.result, 0, restoreText(earlier, mark, 3), restoreHint)
		s.want(t, before)
		if restored := got.nomad.Restored(); len(restored) != 0 {
			t.Errorf("Nomad restored %d snapshots, want none", len(restored))
		}
	})
}

// TestBackupRestoreRestores with --yes: the summary and the notice, the backup in Nomad, and the record in the store.
func TestBackupRestoreRestores(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		earlier := earlierBackup(t, s, mark)

		got := runBackup(t, f, backupArgs(s, "restore", "--yes")...)

		wantResult(t, got.result, 0, "Restored: backup "+earlier.Name+" into cluster prod.\n", restoreNotice)
		if restored := got.nomad.Restored(); len(restored) != 1 || string(restored[0]) != olderSnapshot {
			t.Errorf("Nomad restored %q, want the backup's snapshot", restored)
		}
		record := s.objects(t)[prodLayout(t).NomadRestored()]
		if !strings.Contains(record, `"backup":"`+earlier.Name+`"`) || !strings.Contains(record, `"restored":`) {
			t.Errorf("the record of the restore is %q, want the backup and the time", record)
		}
	})
}

// TestBackupRestoreTakesTheBackupOfFrom restores the older of two backups when --from names it, and the newer one by
// default.
func TestBackupRestoreTakesTheBackupOfFrom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		older := putBackup(t, s, mark.Add(-30*time.Hour), mark.Add(-48*time.Hour), olderSnapshot)
		newer := earlierBackup(t, s, mark)

		named := runBackup(t, f, backupArgs(s, "restore", "--from", older.Name)...)
		latest := runBackup(t, f, backupArgs(s, "restore", "--from", "latest")...)
		byDefault := runBackup(t, f, backupArgs(s, "restore")...)

		wantResult(t, named.result, 0, restoreText(older, mark, 3), restoreHint)
		wantResult(t, latest.result, 0, restoreText(newer, mark, 3), restoreHint)
		wantResult(t, byDefault.result, 0, restoreText(newer, mark, 3), restoreHint)
	})
}

// TestBackupRestoreRefusesABadFromBeforeAnyCall reaches neither the cloud nor Nomad for a name that is none.
func TestBackupRestoreRefusesABadFromBeforeAnyCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		earlierBackup(t, s, mark)
		before, cloudCalls := s.objects(t), len(f.Calls())

		got := runBackup(t, f, backupArgs(s, "restore", "--from", "x", "--yes")...)

		wantError(t, got.result, "Error: invalid backup name \"x\": want latest or a name that tent backup list "+
			"shows, such as 20261009T230207Z\n")
		if calls := got.nomad.Calls(); len(calls) != 0 || len(f.Calls()) != cloudCalls {
			t.Errorf("Nomad got %v and the cloud %d calls, want none", calls, len(f.Calls())-cloudCalls)
		}
		s.want(t, before)
	})
}

// TestBackupRestoreRefuses exits with 1 and an error, and changes nothing, when a rule does not let the restore go
// ahead: only current backups, a job that runs, no backup at all.
func TestBackupRestoreRefuses(t *testing.T) {
	jobs := []nomadops.Job{{Namespace: "default", ID: "web"}}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s state, mark time.Time)
		jobs  []nomadops.Job
		want  string
	}{
		{"every backup is current", func(t *testing.T, s state, mark time.Time) {
			putBackup(t, s, mark.Add(time.Hour), mark, firstSnapshot)
		}, nil, "Error: every backup of cluster prod is of the servers that it runs now, "},
		{"a job runs", func(t *testing.T, s state, mark time.Time) { earlierBackup(t, s, mark) }, jobs,
			"Error: cluster prod runs 1 job that was registered after its servers were built again on "},
		{"no backups", func(*testing.T, state, time.Time) {}, nil, "Error: cluster prod has no backups\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, mark := builtBackups(t)
				tc.setup(t, s, mark)
				before := s.objects(t)
				nomad, factory := nomadWorld("servers", "workers", 6)
				nomad.SetJobs(tc.jobs)

				got := runWithNomad(t, onVultr(f), factory, backupArgs(s, "restore", "--yes")...)

				if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, tc.want) {
					t.Errorf("exit code %d, stdout %q, stderr %q, want 1, nothing and %q", got.code, got.out,
						got.errOut, tc.want)
				}
				s.want(t, before)
				if restored := nomad.Restored(); len(restored) != 0 {
					t.Errorf("Nomad restored %d snapshots, want none", len(restored))
				}
			})
		})
	}
}

// TestBackupRestoreRefusesAnotherBackupAfterARestore names the backup that the servers were restored from, and the
// way out.
func TestBackupRestoreRefusesAnotherBackupAfterARestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		first := earlierBackup(t, s, mark)
		other := putBackup(t, s, mark.Add(-30*time.Hour), mark.Add(-48*time.Hour), olderSnapshot)
		if got := runBackup(t, f, backupArgs(s, "restore", "--from", first.Name, "--yes")...); got.code != 0 {
			t.Fatalf("the first restore: exit code %d\n%s", got.code, got.errOut)
		}

		got := runBackup(t, f, backupArgs(s, "restore", "--from", other.Name, "--yes")...)

		if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut,
			"Error: cluster prod was restored from backup "+first.Name+" on ") {
			t.Errorf("exit code %d, stdout %q, stderr %q, want the refusal of another restore", got.code, got.out,
				got.errOut)
		}
	})
}

// TestBackupRestoreIsDoneOnce prints only the line of a plan that is restored already, with no hint and no notice,
// exits with 0 and asks Nomad nothing, with and without --yes.
func TestBackupRestoreIsDoneOnce(t *testing.T) {
	for _, yes := range []string{"--yes=false", "--yes"} {
		t.Run(yes, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, mark := builtBackups(t)
				earlier := earlierBackup(t, s, mark)
				if got := runBackup(t, f, backupArgs(s, "restore", "--yes")...); got.code != 0 {
					t.Fatalf("the restore: exit code %d\n%s", got.code, got.errOut)
				}

				got := runBackup(t, f, backupArgs(s, "restore", yes)...)

				wantOK(t, got.result, "cluster prod is already restored from backup "+earlier.Name+"\n")
				if calls := got.nomad.Calls(); len(calls) != 0 {
					t.Errorf("Nomad got %v, want no call", calls)
				}
			})
		})
	}
}

// TestBackupRestoreResumes sends the backup again when a record without a time shows that an earlier run was cut: the
// plan says so, the hint stays and --yes ends it with the summary and the notice.
func TestBackupRestoreResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, mark := builtBackups(t)
		earlier := earlierBackup(t, s, mark)
		s.put(t, prodLayout(t).NomadRestored(),
			fmt.Sprintf(`{"backup":%q,"bootstrap":%q}`, earlier.Name, statestore.BackupName(mark)))

		plan := runBackup(t, f, backupArgs(s, "restore")...)
		done := runBackup(t, f, backupArgs(s, "restore", "--yes")...)

		resumed := strings.Replace(restoreText(earlier, mark, 3), "They run no job and list 3 nodes.\n",
			"A restore of this backup was started and has not ended: tent sends the backup again. The servers list "+
				"3 nodes.\n", 1)
		wantResult(t, plan.result, 0, resumed, restoreHint)
		wantResult(t, done.result, 0, "Restored: backup "+earlier.Name+" into cluster prod.\n", restoreNotice)
	})
}

// TestBackupRestorePrintsThePlanAsData with -o json and -o yaml, with "applied": true after --yes and the notice on
// stderr; the plan before --yes carries the hint.
func TestBackupRestorePrintsThePlanAsData(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		for _, yes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s yes=%t", format, yes), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s, f, mark := builtBackups(t)
					earlier := earlierBackup(t, s, mark)
					args := backupArgs(s, "restore", "-o", format)
					wantErr := restoreHint
					if yes {
						args, wantErr = append(args, "--yes"), restoreNotice
					}

					got := runBackup(t, f, args...)

					want := map[string]any{
						"cluster": "prod", "bootstrapped": mark.UTC().Format(time.RFC3339), "nodes": float64(3),
						"backup": map[string]any{
							"name": earlier.Name, "time": earlier.Time.UTC().Format(time.RFC3339), "path": earlier.Path,
						},
					}
					if yes {
						want["applied"] = true
					}
					var data map[string]any
					if err := yaml.Unmarshal([]byte(got.out), &data); err != nil || got.code != 0 || got.errOut != wantErr {
						t.Fatalf("exit code %d, stderr %q, stdout\n%s\nerr %v", got.code, got.errOut, got.out, err)
					}
					if diff := cmp.Diff(want, data); diff != "" {
						t.Errorf("the plan (-want +got):\n%s", diff)
					}
					if format == "json" && !json.Valid([]byte(got.out)) {
						t.Errorf("stdout is no JSON:\n%s", got.out)
					}
				})
			})
		}
	}
}

// TestBackupCommandsWaitForTheLock prints the wait line of the lock and fails at the limit for the commands that
// write; with --lock-timeout 0 they fail at once.
func TestBackupCommandsWaitForTheLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"create", []string{"create"}},
		{"restore", []string{"restore", "--yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, mark := builtBackups(t)
				earlierBackup(t, s, mark)
				held := holdLock(t, s)
				before := s.objects(t)
				locked := (&statestore.LockedError{Cluster: "prod", Holder: held.Lease()}).Error()

				run := func(timeout string) backupRun {
					more := slices.Concat(tc.args[1:], []string{"--lock-timeout", timeout})
					return runBackup(t, f, backupArgs(s, tc.args[0], more...)...)
				}
				waited, atOnce := run("200ms"), run("0")

				wantResult(t, waited.result, 1, "",
					waitNotice(held.Lease(), "200ms")+"Error: "+locked+"; gave up after 200ms\n")
				if atOnce.code != 1 || atOnce.out != "" || !strings.Contains(atOnce.errOut, "Error: "+locked) ||
					strings.Contains(atOnce.errOut, "waiting up to") {
					t.Errorf("--lock-timeout 0: exit code %d, stdout %q, stderr %q, want a refusal at once",
						atOnce.code, atOnce.out, atOnce.errOut)
				}
				s.want(t, before)
			})
		})
	}
}

// TestBackupListTakesNoLock reads while another tent holds the lock.
func TestBackupListTakesNoLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		holdLock(t, s)

		wantResult(t, runIn(t, "", backupArgs(s, "list", "--lock-timeout", "0")...), 0,
			"NAME   TAKEN   SERVERS\n", "cluster prod has no backups\n")
	})
}

// TestBackupCommandsShowNoSnapshot checks stdout, stderr and the debug log of every command, in each format, against
// the bytes of the snapshots that the run handled.
func TestBackupCommandsShowNoSnapshot(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, mark := builtBackups(t)
				earlierBackup(t, s, mark)
				secrets := map[string][]byte{"saved": []byte(firstSnapshot), "restored": []byte(olderSnapshot)}
				for _, run := range []struct {
					args []string
					code int
				}{
					{backupArgs(s, "create", "-o", format, "-vv"), 0},
					{backupArgs(s, "list", "-o", format, "-vv"), 0},
					{backupArgs(s, "restore", "-o", format, "-vv"), 0},
					{backupArgs(s, "restore", "-o", format, "-vv", "--yes"), 0},
					{backupArgs(s, "restore", "-o", format, "-vv", "--from", "x"), 1},
				} {
					args := run.args
					got := runBackup(t, f, args...)

					if got.code != run.code {
						t.Errorf("%s: exit code %d, want %d\n%s", strings.Join(args, " "), got.code, run.code, got.errOut)
					}
					if !strings.Contains(got.errOut, "opened the state store") {
						t.Errorf("%s: stderr holds no debug log:\n%s", strings.Join(args, " "), got.errOut)
					}
					secrettest.CheckHidden(t, map[string]string{strings.Join(args, " "): got.out + got.errOut},
						secrets, "")
				}
			})
		})
	}
}

// TestBackupHelp tells what each command does and which flags it takes.
func TestBackupHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"backup", "--help"}, []string{
			"Back up and restore Nomad's state", "Usage:\n  tent backup [flags]\n  tent backup [command]\n",
			"create ", "list ", "restore ",
		}},
		{[]string{"backup", "create", "--help"}, []string{
			"Usage:\n  tent backup create [NAME] [flags]\n",
			"Take a snapshot of the Nomad state of the cluster named by NAME or --name and store it in the state " +
				"store, under backups/. The snapshot holds the jobs, the nodes, the ACL tokens, the variables and " +
				"the keyring, so it is a secret, stored as the cluster's other secrets are. tent takes the " +
				"cluster's lock for the run. After the servers change, Nomad takes no snapshot until its next " +
				"write; tent then tries again for up to 5 minutes. The command needs the cloud's credentials in " +
				"the environment, VULTR_API_KEY for Vultr, and a way to port 4646 of the servers, which " +
				"spec.access.api allows. tent delete cluster deletes the backups with the cluster. Nothing else " +
				"removes a backup: schedule the command with cron or CI, and remove old backups with a lifecycle " +
				"rule of the bucket, or delete the objects.",
		}},
		{[]string{"backup", "list", "--help"}, []string{
			"Usage:\n  tent backup list [NAME] [flags]\n",
			"List the backups of the cluster named by NAME or --name, oldest first. SERVERS says whether a backup " +
				"is of the servers that run now (current) or of servers that tent has replaced since (earlier); " +
				"tent backup restore takes only an earlier one. The command reads only the state store.",
		}},
		{[]string{"backup", "restore", "--help"}, []string{
			"Usage:\n  tent backup restore [NAME] [flags]\n", "--from string", "(default \"latest\")", "--yes",
			"Put a backup back into the Nomad of the cluster named by NAME or --name. This is for a cluster that " +
				"lost its servers. When a server machine is left, delete it in the cloud first. Then run tent " +
				"update cluster --yes, which builds new servers and bootstraps them with the stored secret, and " +
				"then this command. tent restores only a backup of earlier servers, only while the new servers " +
				"run no job, and one backup into servers that it built again; --from names it as tent backup " +
				"list shows it, and latest, the default, takes the newest. Without --yes the command prints what " +
				"it would restore. With --yes it takes the cluster's lock, remembers the restore, sends the " +
				"backup to Nomad and checks that the stored ACL bootstrap secret still works; a run that stops " +
				"is finished by running the command again, and a finished one changes nothing more. Afterwards " +
				"Nomad holds the jobs, nodes, ACL tokens, variables and keyring of the backup. A client of the " +
				"backup that still runs registers again by itself when it reaches the new servers. Nomad " +
				"refuses a client that registered with the new servers before the restore, and lists the " +
				"backup's nodes as ready for about 5 minutes: then run tent validate cluster, and replace the " +
				"nodes that it reports with tent rolling-update cluster --yes --force --nodegroups GROUPS. The " +
				"command needs the cloud's credentials in the environment, VULTR_API_KEY for Vultr, and a way " +
				"to port 4646 of the servers.",
			"restore the backup; without it, print what would be restored",
		}},
	} {
		got := runIn(t, "", tc.args...)
		for _, want := range tc.want {
			if got.code != 0 || !strings.Contains(got.out, want) {
				t.Errorf("tent %s: exit code %d, stdout\n%s\nwant 0 and it to hold %q", strings.Join(tc.args, " "),
					got.code, got.out, want)
			}
		}
	}
}

// losingAfterBackup removes the lock's lease after the store writes a backup, as state unlock --force by someone else
// would.
type losingAfterBackup struct{ statestore.Store }

func (s losingAfterBackup) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	v, err := s.Store.Put(ctx, p, data, opts)
	if err == nil && strings.HasPrefix(p, "prod/backups/") {
		err = s.Delete(ctx, "prod/lock")
	}
	return v, err
}

// TestBackupCommandsPrintWhatTheyDidWhenTheLockIsLost prints the result and then the error, as the other commands that
// write do: the backup is stored and the restore is done.
func TestBackupCommandsPrintWhatTheyDidWhenTheLockIsLost(t *testing.T) {
	const lost = "Error: the change is saved, but the lock of cluster prod was lost before tent released it\n"
	for _, tc := range []struct {
		name string
		lose func(statestore.Store) statestore.Store
		args []string
		out  func(b statestore.Backup) string
		note string
	}{
		{"create", func(s statestore.Store) statestore.Store { return losingAfterBackup{s} },
			[]string{"create"}, nil, ""},
		{"restore", func(s statestore.Store) statestore.Store {
			return losingStore{Store: s, after: "prod/nomad/restored"}
		}, []string{"restore", "--yes"}, func(b statestore.Backup) string {
			return "Restored: backup " + b.Name + " into cluster prod.\n"
		}, restoreNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, mark := builtBackups(t)
				earlier := earlierBackup(t, s, mark)
				nomad, factory := nomadWorld("servers", "workers", 6)
				opts := &globalOptions{
					openStore: wrapped(tc.lose), providers: onVultr(f), assets: testAssets(), nomad: factory,
				}

				got := runWith(t, opts, backupArgs(s, tc.args[0], tc.args[1:]...)...)

				out := ""
				if tc.out != nil {
					out = tc.out(earlier)
				} else {
					for p := range storedBackups(t, s) {
						if p != earlier.Path {
							b, _ := prodLayout(t).BackupAt(p)
							out = "stored backup " + b.Name + " of cluster prod (20 bytes)\n"
						}
					}
				}
				wantResult(t, got, 1, out, tc.note+lost)
				if tc.name == "restore" && len(nomad.Restored()) != 1 {
					t.Errorf("Nomad restored %d snapshots, want one", len(nomad.Restored()))
				}
			})
		})
	}
}
