package app

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// restoreMark is the bootstrap mark of the servers that the rule tests run.
var restoreMark = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// testBackup returns the backup taken at taken from the servers bootstrapped at bootstrapped.
func testBackup(t *testing.T, taken, bootstrapped time.Time) statestore.Backup {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	return l.Backup(taken, bootstrapped)
}

// earlierMark is the bootstrap mark of servers that were built before restoreMark's.
var earlierMark = restoreMark.Add(-72 * time.Hour)

func TestPickBackup(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 9, d, 3, 0, 0, 0, time.UTC) }
	old1 := testBackup(t, day(20), earlierMark)
	old2 := testBackup(t, day(25), earlierMark)
	current := testBackup(t, restoreMark.Add(time.Hour), restoreMark)
	twin := testBackup(t, day(25), earlierMark.Add(-time.Hour))
	const nowText = "which tent bootstrapped on 2026-10-01 10:00:00 UTC; tent restores a backup only into servers " +
		"that tent update cluster built again after it"
	tests := []struct {
		name     string
		cluster  string
		backups  []statestore.Backup
		from     string
		want     statestore.Backup
		wantText string
	}{
		{name: "latest takes the newest earlier backup", backups: []statestore.Backup{old1, old2}, from: Latest, want: old2},
		{
			name: "latest passes over a newer current backup", backups: []statestore.Backup{old1, old2, current},
			from: Latest, want: old2,
		},
		{
			name: "latest passes over a current backup between earlier ones",
			backups: []statestore.Backup{
				old1, testBackup(t, day(22), restoreMark), old2,
			},
			from: Latest, want: old2,
		},
		{name: "a name takes that backup", backups: []statestore.Backup{old1, old2, current}, from: old1.Name, want: old1},
		{
			name: "no backup at all", backups: nil, from: Latest,
			wantText: "cluster prod has no backups",
		},
		{
			name: "no backup at all, by name", backups: nil, from: old1.Name,
			wantText: "cluster prod has no backups",
		},
		{
			name: "latest with current backups only", backups: []statestore.Backup{current}, from: Latest,
			wantText: "every backup of cluster prod is of the servers that it runs now, " + nowText,
		},
		{
			name: "a name of a current backup", backups: []statestore.Backup{old1, current}, from: current.Name,
			wantText: "backup " + current.Name + " is of the servers that cluster prod runs now, " + nowText,
		},
		{
			name: "a name that is no time", backups: []statestore.Backup{old1}, from: "yesterday",
			wantText: `invalid backup name "yesterday": want latest or a name that tent backup list shows, ` +
				"such as 20261009T230207Z",
		},
		{
			name: "a time that BackupName does not write", backups: []statestore.Backup{old1}, from: "20260920T030000.5Z",
			wantText: `invalid backup name "20260920T030000.5Z": want latest or a name that tent backup list ` +
				"shows, such as 20261009T230207Z",
		},
		{
			name: "a name that no backup has", backups: []statestore.Backup{old1}, from: "20261009T230207Z",
			wantText: "cluster prod has no backup 20261009T230207Z; tent backup list shows its backups",
		},
		{
			name: "two backups of one name", backups: []statestore.Backup{twin, old2}, from: old2.Name,
			wantText: "cluster prod has 2 backups named " + old2.Name + ": " + twin.Path + " and " + old2.Path +
				"; remove all but one",
		},
		{
			name: "an invalid cluster name", cluster: "a/b", backups: []statestore.Backup{old1}, from: old1.Name,
			wantText: "cluster name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := tc.cluster
			if cluster == "" {
				cluster = "prod"
			}
			got, err := pickBackup(cluster, tc.backups, tc.from, restoreMark)
			if tc.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantText) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantText)
				}
				if tc.cluster == "" && err.Error() != tc.wantText {
					t.Fatalf("error = %q, want %q", err, tc.wantText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("backup mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRecorded(t *testing.T) {
	t.Parallel()
	b := testBackup(t, time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC), earlierMark)
	other := testBackup(t, time.Date(2026, 9, 25, 3, 0, 0, 0, time.UTC), earlierMark)
	restored := time.Date(2026, 10, 10, 8, 20, 0, 0, time.UTC)
	here := statestore.BackupName(restoreMark)
	elsewhere := statestore.BackupName(earlierMark)
	const refusal = "cluster prod was restored from backup 20260925T030000Z on 2026-10-10 08:20:00 UTC; " +
		"tent restores one backup into servers that it built again: to restore another one, delete the server " +
		"machines in the cloud and run tent update cluster --yes first"
	tests := []struct {
		name     string
		rec      *restoreRecord
		want     restoreState
		wantText string
	}{
		{name: "no record", rec: nil, want: restoreNew},
		{
			name: "this backup, another bootstrap, with a time",
			rec:  &restoreRecord{Backup: b.Name, Bootstrap: elsewhere, Restored: &restored}, want: restoreNew,
		},
		{
			name: "another backup, another bootstrap, with a time",
			rec:  &restoreRecord{Backup: other.Name, Bootstrap: elsewhere, Restored: &restored}, want: restoreNew,
		},
		{
			name: "this backup, another bootstrap, no time",
			rec:  &restoreRecord{Backup: b.Name, Bootstrap: elsewhere}, want: restoreNew,
		},
		{
			name: "another backup, another bootstrap, no time",
			rec:  &restoreRecord{Backup: other.Name, Bootstrap: elsewhere}, want: restoreNew,
		},
		{
			name: "this bootstrap, this backup, with a time",
			rec:  &restoreRecord{Backup: b.Name, Bootstrap: here, Restored: &restored}, want: restoreDone,
		},
		{
			name: "this bootstrap, this backup, no time",
			rec:  &restoreRecord{Backup: b.Name, Bootstrap: here}, want: restoreCut,
		},
		{
			name:     "this bootstrap, another backup, with a time",
			rec:      &restoreRecord{Backup: other.Name, Bootstrap: here, Restored: &restored},
			wantText: refusal,
		},
		{
			name: "this bootstrap, another backup, no time",
			rec:  &restoreRecord{Backup: other.Name, Bootstrap: here}, want: restoreNew,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := recorded("prod", tc.rec, restoreMark, b)
			if tc.wantText != "" {
				if err == nil || err.Error() != tc.wantText {
					t.Fatalf("error = %v, want %q", err, tc.wantText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordedWritesTheRestoreTimeInUTC(t *testing.T) {
	t.Parallel()
	b := testBackup(t, time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC), earlierMark)
	zone := time.FixedZone("CEST", 2*3600)
	restored := time.Date(2026, 10, 10, 10, 20, 0, 0, zone)
	rec := &restoreRecord{Backup: "20260925T030000Z", Bootstrap: statestore.BackupName(restoreMark), Restored: &restored}
	_, err := recorded("prod", rec, restoreMark, b)
	if err == nil || !strings.Contains(err.Error(), "on 2026-10-10 08:20:00 UTC;") {
		t.Fatalf("error = %v, want the time in UTC", err)
	}
}

func TestRefuseJobs(t *testing.T) {
	t.Parallel()
	jobs := func(n int) []nomadops.Job {
		out := make([]nomadops.Job, 0, n)
		for i := range n {
			ns := "default"
			if i%2 == 1 {
				ns = "other"
			}
			out = append(out, nomadops.Job{Namespace: ns, ID: "job" + string(rune('a'+i))})
		}
		return out
	}
	const tail = "a restore replaces them with the backup's. tent restores a backup only into servers that run " +
		"no job: purge the jobs first, or delete the server machines in the cloud and run tent update cluster --yes"
	const startedTail = " was started and has not ended, and the cluster runs "
	const startedEnd = ". Run tent backup restore --from 20261008T030000Z --yes to end it; to restore another " +
		"backup, purge the jobs first, or delete the server machines in the cloud and run tent update cluster --yes"
	tests := []struct {
		name     string
		jobs     []nomadops.Job
		started  string
		wantText string
	}{
		{name: "no job", jobs: nil},
		{name: "an empty list", jobs: []nomadops.Job{}},
		{
			name: "one job", jobs: jobs(1),
			wantText: "cluster prod runs 1 job that was registered after its servers were built again on " +
				"2026-10-01 10:00:00 UTC; " + tail,
		},
		{
			name: "twelve jobs of two namespaces", jobs: jobs(12),
			wantText: "cluster prod runs 12 jobs that were registered after its servers were built again on " +
				"2026-10-01 10:00:00 UTC; " + tail,
		},
		{
			name: "a started restore and jobs", jobs: jobs(12), started: "20261008T030000Z",
			wantText: "a restore of backup 20261008T030000Z into cluster prod" + startedTail + "12 jobs" + startedEnd,
		},
		{
			name: "a started restore and one job", jobs: jobs(1), started: "20261008T030000Z",
			wantText: "a restore of backup 20261008T030000Z into cluster prod" + startedTail + "1 job" + startedEnd,
		},
		{name: "a started restore and no job", jobs: nil, started: "20261008T030000Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := refuseJobs("prod", tc.jobs, restoreMark, tc.started)
			if tc.wantText == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantText {
				t.Fatalf("error = %v, want %q", err, tc.wantText)
			}
		})
	}
}

func TestRefuseJobsWritesTheMarkInUTC(t *testing.T) {
	t.Parallel()
	mark := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	err := refuseJobs("prod", []nomadops.Job{{Namespace: "default", ID: "a"}}, mark, "")
	if err == nil || !strings.Contains(err.Error(), "again on 2026-10-01 10:00:00 UTC;") {
		t.Fatalf("error = %v, want the mark in UTC", err)
	}
}

func TestRestoreRecordRoundTrip(t *testing.T) {
	t.Parallel()
	restored := time.Date(2026, 10, 10, 8, 20, 0, 0, time.UTC)
	tests := []struct {
		name string
		rec  restoreRecord
		want string
	}{
		{
			name: "no time",
			rec:  restoreRecord{Backup: "20261008T030000Z", Bootstrap: "20261010T080000Z"},
			want: `{"backup":"20261008T030000Z","bootstrap":"20261010T080000Z"}` + "\n",
		},
		{
			name: "with an accessor",
			rec:  restoreRecord{Backup: "20261008T030000Z", Bootstrap: "20261010T080000Z", Accessor: "8f3a1c2e"},
			want: `{"backup":"20261008T030000Z","bootstrap":"20261010T080000Z","accessor":"8f3a1c2e"}` + "\n",
		},
		{
			name: "with a time",
			rec:  restoreRecord{Backup: "20261008T030000Z", Bootstrap: "20261010T080000Z", Restored: &restored},
			want: `{"backup":"20261008T030000Z","bootstrap":"20261010T080000Z","restored":"2026-10-10T08:20:00Z"}` + "\n",
		},
		{
			name: "with an accessor and a time",
			rec: restoreRecord{
				Backup: "20261008T030000Z", Bootstrap: "20261010T080000Z", Accessor: "8f3a1c2e", Restored: &restored,
			},
			want: `{"backup":"20261008T030000Z","bootstrap":"20261010T080000Z","accessor":"8f3a1c2e",` +
				`"restored":"2026-10-10T08:20:00Z"}` + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := tc.rec.encode()
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("encoded = %q, want %q", data, tc.want)
			}
			got, err := decodeRestoreRecord("prod/nomad/restored", data)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.rec, got); diff != "" {
				t.Fatalf("record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRestoreRecordEncodesTheTimeInUTC(t *testing.T) {
	t.Parallel()
	restored := time.Date(2026, 10, 10, 10, 20, 0, 0, time.FixedZone("CEST", 2*3600))
	data, err := (restoreRecord{Backup: "a", Bootstrap: "b", Restored: &restored}).encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"restored":"2026-10-10T08:20:00Z"`) {
		t.Fatalf("encoded = %q, want the time in UTC", data)
	}
}

func TestRestoreRecordEncodeFailsForATimeJSONCannotWrite(t *testing.T) {
	t.Parallel()
	restored := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := (restoreRecord{Backup: "a", Bootstrap: "b", Restored: &restored}).encode(); err == nil {
		t.Fatal("encode succeeded for the year 10000")
	}
}

func TestDecodeRestoreRecordFails(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"", "not json\n", `{"backup":`, `["a"]`, `{"backup":"a","bootstrap":"b"} x`} {
		_, err := decodeRestoreRecord("prod/nomad/restored", []byte(data))
		if err == nil || !strings.HasPrefix(err.Error(), "prod/nomad/restored: ") {
			t.Fatalf("decode(%q) error = %v, want it to start with the path", data, err)
		}
	}
}

func TestPickBackupWritesTheMarkInUTC(t *testing.T) {
	t.Parallel()
	mark := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	current := testBackup(t, mark.Add(time.Hour), mark)
	for _, from := range []string{Latest, current.Name} {
		_, err := pickBackup("prod", []statestore.Backup{current}, from, mark)
		if err == nil || !strings.Contains(err.Error(), "bootstrapped on 2026-10-01 10:00:00 UTC;") {
			t.Fatalf("from %q: error = %v, want the mark in UTC", from, err)
		}
	}
}

func TestTookBackup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		recorded, found string
		want            bool
	}{
		{"the same accessor: Nomad took nothing", "8f3a1c2e", "8f3a1c2e", false},
		{"another accessor: Nomad took the backup", "8f3a1c2e", "0b1c2d3e", true},
		{"a record without an accessor cannot tell", "", "0b1c2d3e", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := restoreRecord{Backup: "20261008T030000Z", Bootstrap: "20261010T080000Z", Accessor: tc.recorded}
			if got := tookBackup(rec, tc.found); got != tc.want {
				t.Errorf("tookBackup(%+v, %q) = %v, want %v", rec, tc.found, got, tc.want)
			}
		})
	}
}
