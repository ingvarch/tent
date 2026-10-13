package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// Latest is the from of a restore that takes the newest backup of earlier servers.
const Latest = "latest"

// restoreTimeLayout writes a time in the texts of a restore.
const restoreTimeLayout = "2006-01-02 15:04:05 MST"

// rebuildWay is how to get new servers: the way out when the servers were restored already or run jobs.
const rebuildWay = "delete the server machines in the cloud and run tent update cluster --yes"

// pickBackup returns the backup that a restore of from takes among backups, which are sorted oldest first, for a
// cluster whose bootstrap mark holds the time mark. A backup is of earlier servers when its bootstrap is not the
// mark's.
func pickBackup(cluster string, backups []statestore.Backup, from string, mark time.Time) (statestore.Backup, error) {
	if len(backups) == 0 {
		return statestore.Backup{}, fmt.Errorf("cluster %s has no backups", cluster)
	}
	current := statestore.BackupName(mark)
	const rebuilt = "which tent bootstrapped on %s; tent restores a backup only into servers that tent update " +
		"cluster built again after it"
	if from == Latest {
		for _, b := range slices.Backward(backups) {
			if b.Bootstrap != current {
				return b, nil
			}
		}
		return statestore.Backup{}, fmt.Errorf("every backup of cluster %s is of the servers that it runs now, "+rebuilt,
			cluster, mark.UTC().Format(restoreTimeLayout))
	}
	l, err := statestore.NewLayout(cluster)
	if err != nil {
		return statestore.Backup{}, err
	}
	if _, ok := l.BackupAt(l.Backups() + current + "/" + from + ".snap"); !ok {
		return statestore.Backup{}, fmt.Errorf(
			"invalid backup name %q: want latest or a name that tent backup list shows, such as 20261009T230207Z", from)
	}
	var named []statestore.Backup
	for _, b := range backups {
		if b.Name == from {
			named = append(named, b)
		}
	}
	switch len(named) {
	case 0:
		return statestore.Backup{}, fmt.Errorf(
			"cluster %s has no backup %s; tent backup list shows its backups", cluster, from)
	case 1:
	default:
		paths := make([]string, 0, len(named))
		for _, b := range named {
			paths = append(paths, b.Path)
		}
		return statestore.Backup{}, fmt.Errorf("cluster %s has %d backups named %s: %s; remove all but one",
			cluster, len(named), from, english.And(paths))
	}
	if named[0].Bootstrap == current {
		return statestore.Backup{}, fmt.Errorf("backup %s is of the servers that cluster %s runs now, "+rebuilt,
			from, cluster, mark.UTC().Format(restoreTimeLayout))
	}
	return named[0], nil
}

// restoreRecord says which backup the servers of one bootstrap are being restored from, or were.
type restoreRecord struct {
	Backup    string `json:"backup"`    // the backup's name
	Bootstrap string `json:"bootstrap"` // the servers: the time of their bootstrap mark, as a backup's path writes it
	// Restored is when the restore ended. It is nil while the restore has not ended: tent writes the record before it
	// sends the backup.
	Restored *time.Time `json:"restored,omitempty"`
}

// encode returns the record as one line of JSON, with the time in UTC.
func (r restoreRecord) encode() ([]byte, error) {
	if r.Restored != nil {
		utc := r.Restored.UTC()
		r.Restored = &utc
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode the record of a restore: %w", err)
	}
	return append(data, '\n'), nil
}

// decodeRestoreRecord reads the record that the object at path holds.
func decodeRestoreRecord(path string, data []byte) (restoreRecord, error) {
	var r restoreRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return restoreRecord{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// restoreState is what the record says about a restore of one backup into the servers of one bootstrap.
type restoreState int

const (
	restoreNew  restoreState = iota // no record of this bootstrap, or a record that lets another backup take its place
	restoreCut                      // a restore of this backup was started and has not ended
	restoreDone                     // a restore of this backup ended
)

// recorded returns what the record says about a restore of the backup b into the servers bootstrapped at mark. It
// fails when the record of this bootstrap names another backup whose restore ended. A missing record, a record of
// another bootstrap, and a record of another backup without a time are restoreNew.
func recorded(cluster string, rec *restoreRecord, mark time.Time, b statestore.Backup) (restoreState, error) {
	if rec == nil || rec.Bootstrap != statestore.BackupName(mark) {
		return restoreNew, nil
	}
	switch {
	case rec.Backup == b.Name && rec.Restored != nil:
		return restoreDone, nil
	case rec.Backup == b.Name:
		return restoreCut, nil
	case rec.Restored == nil:
		return restoreNew, nil
	}
	return restoreNew, fmt.Errorf("cluster %s was restored from backup %s on %s; tent restores one backup into servers "+
		"that it built again: to restore another one, %s first",
		cluster, rec.Backup, rec.Restored.UTC().Format(restoreTimeLayout), rebuildWay)
}

// refuseJobs fails when jobs holds any job: the servers bootstrapped at mark are in use. started is the backup of the
// record when it names another backup whose restore has not ended, and "" otherwise; the text then names it.
func refuseJobs(cluster string, jobs []nomadops.Job, mark time.Time, started string) error {
	if len(jobs) == 0 {
		return nil
	}
	count := fmt.Sprintf("%d job", len(jobs))
	were := "was"
	if len(jobs) != 1 {
		count += "s"
		were = "were"
	}
	if started != "" {
		return fmt.Errorf("a restore of backup %s into cluster %s was started and has not ended, and the cluster runs %s. "+
			"Run tent backup restore --from %s --yes to end it; to restore another backup, purge the jobs first, or %s",
			started, cluster, count, started, rebuildWay)
	}
	return fmt.Errorf("cluster %s runs %s that %s registered after its servers were built again on %s; "+
		"a restore replaces them with the backup's. tent restores a backup only into servers that run no job: "+
		"purge the jobs first, or %s", cluster, count, were, mark.UTC().Format(restoreTimeLayout), rebuildWay)
}
