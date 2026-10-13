package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/secret"
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
	// Accessor is the accessor of the servers' bootstrap token as tent read it before it sent the backup. After Nomad
	// took the backup the token has the accessor that it had in the cluster of the backup.
	Accessor string `json:"accessor,omitempty"`
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
	count, were := quantity(len(jobs), "job"), "was"
	if len(jobs) != 1 {
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

// tookBackup reports whether Nomad took the backup after tent wrote rec: the bootstrap token has the accessor found
// now, which is not the one that rec holds. A record without an accessor cannot tell, so it says no.
func tookBackup(rec restoreRecord, found string) bool {
	return rec.Accessor != "" && found != rec.Accessor
}

// lockedOut is the error of a restore of the object at backupPath after which the ACL bootstrap secret of the state
// store is the secret of no token of the servers.
func lockedOut(cluster, backupPath string) error {
	return fmt.Errorf("the restore replaced the ACL tokens of %s, and the ACL bootstrap secret of the state store is "+
		"none of them: tent can no longer call the cluster. The object %s is no backup that tent took of this "+
		"cluster. Delete the server machines in the cloud, run tent update cluster --yes, and restore another backup "+
		"with tent backup restore --from NAME, since latest takes this one again", clusterLabel(cluster), backupPath)
}

// RestorePlan is what a restore of a backup does.
type RestorePlan struct {
	Cluster      string
	Backup       statestore.Backup
	Bootstrapped time.Time // when the servers that run now were bootstrapped, in UTC
	// Nodes is how many nodes Nomad lists now. A node that the backup does not know must register again after the
	// restore.
	Nodes int
	// Resumes reports that a restore of this backup was started and has not ended: tent sends the backup again.
	Resumes bool
	// Taken reports, for a plan with Resumes, that Nomad took the backup in the run that was cut: tent sends it again
	// without a look at the jobs, which may be the backup's.
	Taken bool
	// Restored reports that these servers were restored from this backup already, so nothing is left to do.
	Restored bool
	// Applied reports that BackupRestore restored the backup.
	Applied bool
}

// WriteText writes the plan for people: what the restore does and how many nodes Nomad lists. A plan with Restored is
// the line "cluster prod is already restored from backup 20261008T030000Z".
func (p RestorePlan) WriteText(w io.Writer) error {
	if p.Restored {
		return writeText(w, "the plan", fmt.Sprintf("%s is already restored from backup %s\n",
			clusterLabel(p.Cluster), p.Backup.Name))
	}
	nodes := "list " + quantity(p.Nodes, "node")
	state := "They run no job and " + nodes + "."
	switch {
	case p.Resumes && p.Taken:
		state = "A restore of this backup was started and Nomad took the backup, but it has not ended: tent sends " +
			"the backup again. The servers " + nodes + "."
	case p.Resumes:
		state = "A restore of this backup was started and Nomad did not take the backup: tent sends it again. " +
			"They run no job and " + nodes + "."
	}
	return writeText(w, "the plan", fmt.Sprintf("Restore backup %s (taken %s) into %s.\n"+
		"The servers that run now were bootstrapped on %s; the backup is of earlier servers.\n%s\n"+
		"Nomad's jobs, nodes, ACL tokens, variables and keyring become those of the backup.\n",
		p.Backup.Name, p.Backup.Time.UTC().Format(restoreTimeLayout), clusterLabel(p.Cluster),
		p.Bootstrapped.UTC().Format(restoreTimeLayout), state))
}

// WriteApplied writes the line "Restored: backup 20261008T030000Z into cluster prod.".
func (p RestorePlan) WriteApplied(w io.Writer) error {
	line := fmt.Sprintf("Restored: backup %s into %s.\n", p.Backup.Name, clusterLabel(p.Cluster))
	return writeText(w, "the result", line)
}

// MarshalJSON encodes the plan as {"cluster": "prod", "backup": {"name": "...", "time": "...", "path": "..."},
// "bootstrapped": "2026-10-10T08:00:00Z", "nodes": 2, "resumes": true, "taken": true, "restored": true, "applied":
// true}; resumes, taken, restored and applied are left out when false, and nodes is left out of a plan with restored,
// which asks no Nomad.
func (p RestorePlan) MarshalJSON() ([]byte, error) {
	type backup struct {
		Name string `json:"name"`
		Time string `json:"time"`
		Path string `json:"path"`
	}
	out := struct {
		Cluster      string `json:"cluster"`
		Backup       backup `json:"backup"`
		Bootstrapped string `json:"bootstrapped"`
		Nodes        *int   `json:"nodes,omitempty"`
		Resumes      bool   `json:"resumes,omitempty"`
		Taken        bool   `json:"taken,omitempty"`
		Restored     bool   `json:"restored,omitempty"`
		Applied      bool   `json:"applied,omitempty"`
	}{
		Cluster:      p.Cluster,
		Backup:       backup{p.Backup.Name, p.Backup.Time.UTC().Format(time.RFC3339), p.Backup.Path},
		Bootstrapped: p.Bootstrapped.UTC().Format(time.RFC3339),
		Resumes:      p.Resumes, Taken: p.Taken, Restored: p.Restored, Applied: p.Applied,
	}
	if !p.Restored {
		out.Nodes = &p.Nodes
	}
	return marshalJSON("the plan", out)
}

// restoreRun is what the plan of a restore found, and the API that it asks Nomad through.
type restoreRun struct {
	plan  RestorePlan
	built builtCluster
	state restoreState
	api   *nomadops.Servers
	// accessor is the accessor that the record of a resumed restore holds.
	accessor string
}

// BackupRestore plans the restore of the backup that from names, or of the newest backup of earlier servers when from
// is Latest, into the cluster's Nomad and, with apply, carries it out under the cluster's lock. It restores only into
// servers that tent built again after the backup, only while Nomad lists no job, and only one backup into the servers
// of one bootstrap. A restore that was cut is finished by running the command again: it is sent without a look at the
// jobs when the accessor of the bootstrap token shows that Nomad took the backup, and with the rule for jobs otherwise.
// The plan is made once without the lock and again under it. A backup is a secret: it holds the ACL tokens and the
// keyring of Nomad.
func (s *Service) BackupRestore(ctx context.Context, cluster, from string, apply bool) (_ RestorePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return RestorePlan{}, err
	}
	run, err := s.planRestore(ctx, l, from)
	if err != nil || !apply || run.plan.Restored {
		return run.plan, err
	}
	err = s.locked(ctx, l, "backup restore", func(ctx context.Context) error {
		var err error
		if run, err = s.planRestore(ctx, l, from); err != nil || run.plan.Restored {
			return err
		}
		run.plan, err = s.restore(ctx, l, run)
		return err
	})
	return run.plan, err
}

// planRestore reads the cluster and decides which backup the restore takes, and whether the servers may take it. A
// plan with Restored asks no cloud and no Nomad.
func (s *Service) planRestore(ctx context.Context, l statestore.Layout, from string) (restoreRun, error) {
	built, err := s.loadBuilt(ctx, l)
	if err != nil {
		return restoreRun{}, err
	}
	mark, err := markTime(l, built.mark)
	if err != nil {
		return restoreRun{}, err
	}
	backups, err := statestore.Backups(ctx, s.Store, l)
	if err != nil {
		return restoreRun{}, err
	}
	backup, err := pickBackup(l.Cluster(), backups, from, mark)
	if err != nil {
		return restoreRun{}, err
	}
	rec, err := s.storedRestore(ctx, l)
	if err != nil {
		return restoreRun{}, err
	}
	state, err := recorded(l.Cluster(), rec, mark, backup)
	run := restoreRun{
		plan:  RestorePlan{Cluster: l.Cluster(), Backup: backup, Bootstrapped: mark.UTC(), Restored: state == restoreDone},
		built: built, state: state,
	}
	if err != nil || run.plan.Restored {
		return run, err
	}
	servers, _, err := s.reach(ctx, built)
	if err != nil {
		return run, err
	}
	nodes, err := checkReach(ctx, servers)
	if err != nil {
		if state == restoreCut && errors.Is(err, nomadops.ErrDenied) {
			err = lockedOut(l.Cluster(), backup.Path)
		}
		return run, err
	}
	run.api, run.plan.Nodes = servers, len(nodes)
	started := ""
	if state == restoreCut {
		run.plan.Resumes, run.accessor = true, rec.Accessor
		found, err := servers.TokenAccessor(ctx)
		switch {
		case errors.Is(err, nomadops.ErrDenied):
			return run, lockedOut(l.Cluster(), backup.Path)
		case err != nil:
			return run, fmt.Errorf("read the accessor of tent's token: %w", err)
		}
		if run.plan.Taken = tookBackup(*rec, found); run.plan.Taken {
			return run, nil
		}
	} else {
		started = startedBackup(rec, mark)
	}
	jobs, err := servers.Jobs(ctx)
	if err != nil {
		return run, fmt.Errorf("list the jobs: %w", err)
	}
	return run, refuseJobs(l.Cluster(), jobs, mark, started)
}

// startedBackup returns the backup that rec names for the servers bootstrapped at mark, or "" when rec is not of
// these servers. For a restoreNew run it is the backup of a restore that was started and has not ended.
func startedBackup(rec *restoreRecord, mark time.Time) string {
	if rec == nil || rec.Bootstrap != statestore.BackupName(mark) {
		return ""
	}
	return rec.Backup
}

// storedRestore returns the record of a restore of the cluster, or nil when the store holds none.
func (s *Service) storedRestore(ctx context.Context, l statestore.Layout) (*restoreRecord, error) {
	data, _, err := s.Store.Get(ctx, l.NomadRestored())
	switch {
	case errors.Is(err, statestore.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	rec, err := decodeRestoreRecord(l.NomadRestored(), data)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// restore sends the backup to Nomad and checks that tent's own access survived. The caller holds the lock. It writes
// the record before it sends the backup, with the accessor of the bootstrap token and without a time, so that a run
// that is cut after Nomad took the backup sends it again although the backup's jobs are there. It writes the time once
// the check has passed.
func (s *Service) restore(ctx context.Context, l statestore.Layout, run restoreRun) (RestorePlan, error) {
	plan, cluster := run.plan, l.Cluster()
	data, _, err := s.Store.Get(ctx, plan.Backup.Path)
	if err != nil {
		return plan, fmt.Errorf("read backup %s: %w", plan.Backup.Name, err)
	}
	snap := secret.Secret(data)
	if nomadops.CheckSnapshot(snap) != nil {
		return plan, fmt.Errorf("backup %s of %s holds no snapshot", plan.Backup.Name, clusterLabel(cluster))
	}
	if err := statestore.RaiseVersion(ctx, s.Store, l, s.Version); err != nil {
		return plan, err
	}
	rec := restoreRecord{
		Backup: plan.Backup.Name, Bootstrap: statestore.BackupName(plan.Bootstrapped), Accessor: run.accessor,
	}
	if run.state == restoreNew {
		var err error
		if rec.Accessor, err = run.api.TokenAccessor(ctx); err != nil {
			return plan, fmt.Errorf("read the accessor of tent's token: %w", err)
		}
		if err := s.putRecord(ctx, l, rec); err != nil {
			return plan, err
		}
	}
	if err := run.api.RestoreSnapshot(ctx, snap); err != nil {
		return plan, fmt.Errorf("restore backup %s: %w; run the command again", plan.Backup.Name, err)
	}
	switch err := run.api.Bootstrap(ctx, run.built.secrets.bootstrap); {
	case errors.Is(err, nomadops.ErrBootstrapMismatch):
		return plan, lockedOut(cluster, plan.Backup.Path)
	case err != nil:
		return plan, fmt.Errorf("check tent's access after the restore: %w; run the command again", err)
	}
	restored := s.now()
	rec.Restored = &restored
	if err := s.putRecord(ctx, l, rec); err != nil {
		return plan, fmt.Errorf("%w; run the command again", err)
	}
	plan.Applied = true
	return plan, nil
}

// putRecord writes the record of a restore, replacing the one that is there.
func (s *Service) putRecord(ctx context.Context, l statestore.Layout, rec restoreRecord) error {
	data, err := rec.encode()
	if err != nil {
		return err
	}
	if _, err := s.Store.Put(ctx, l.NomadRestored(), data, statestore.PutOptions{}); err != nil {
		return fmt.Errorf("write %s: %w", l.NomadRestored(), err)
	}
	return nil
}

// quantity returns n and the noun, which takes an s unless n is 1, such as "1 job" and "12 jobs".
func quantity(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
