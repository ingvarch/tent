package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/statestore"
)

const (
	// snapshotWait is how long a backup goes on asking Nomad for a snapshot that it cannot take yet: after its
	// servers change, Nomad takes none until its next write, which came after 59 s in a test.
	snapshotWait = 5 * time.Minute
	// snapshotPoll is how long a backup waits between two tries.
	snapshotPoll = 2 * time.Second
)

// StoredBackup is a backup that tent has just stored.
type StoredBackup struct {
	Cluster string
	statestore.Backup
	Size int // of the snapshot, in bytes
}

// WriteText writes the line "stored backup 20261009T230207Z of cluster prod (16855 bytes)".
func (b StoredBackup) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "stored backup %s of cluster %s (%d bytes)\n", b.Name, b.Cluster, b.Size)
	return err
}

// MarshalJSON encodes it as {"cluster": "prod", "name": "20261009T230207Z", "time": "2026-10-09T23:02:07Z",
// "path": "prod/backups/20261001T100000Z/20261009T230207Z.snap", "size": 16855}.
func (b StoredBackup) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Cluster string `json:"cluster"`
		Name    string `json:"name"`
		Time    string `json:"time"`
		Path    string `json:"path"`
		Size    int    `json:"size"`
	}{b.Cluster, b.Name, b.Time.UTC().Format(time.RFC3339), b.Path, b.Size})
}

// ListedBackup is a backup in the list of a cluster's backups.
type ListedBackup struct {
	statestore.Backup
	// Earlier reports that the backup is of servers that tent has replaced since, or that the cluster has no
	// bootstrap mark: a restore takes only such a backup.
	Earlier bool
}

// MarshalJSON encodes it as {"name": "20261009T230207Z", "time": "2026-10-09T23:02:07Z", "path": "...", "servers":
// "earlier"}; servers is "current" for a backup of the servers that run now.
func (b ListedBackup) MarshalJSON() ([]byte, error) {
	servers := "current"
	if b.Earlier {
		servers = "earlier"
	}
	return marshalJSON("the backup", struct {
		Name    string `json:"name"`
		Time    string `json:"time"`
		Path    string `json:"path"`
		Servers string `json:"servers"`
	}{b.Name, b.Time.UTC().Format(time.RFC3339), b.Path, servers})
}

// Backups returns the backups of the cluster in the state store, oldest first; a cluster without any gets an empty
// list. It reads only the state store, so it works while the cluster's servers are gone.
func (s *Service) Backups(ctx context.Context, cluster string) (_ []ListedBackup, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if _, err := s.get(ctx, l, false); err != nil {
		return nil, err
	}
	var current string
	mark, marked, err := s.bootstrapMark(ctx, l)
	if err != nil {
		return nil, err
	}
	if marked {
		t, err := markTime(l, mark)
		if err != nil {
			return nil, err
		}
		current = statestore.BackupName(t)
	}
	backups, err := statestore.Backups(ctx, s.Store, l)
	if err != nil {
		return nil, err
	}
	listed := make([]ListedBackup, 0, len(backups))
	for _, b := range backups {
		listed = append(listed, ListedBackup{Backup: b, Earlier: b.Bootstrap != current})
	}
	return listed, nil
}

// BackupCreate takes a snapshot of the cluster's Nomad state and stores it in the state store, under the cluster's
// lock. The cluster must be built: its store holds its secrets and the mark of the Nomad bootstrap. A snapshot that
// Nomad cannot take yet is asked for again for up to 5 minutes, with a warning. tent never replaces a backup: one of
// the same second is an error, on a store that can refuse an overwrite. A backup is a secret: it holds the ACL tokens
// and the keyring of Nomad.
func (s *Service) BackupCreate(ctx context.Context, cluster string) (_ StoredBackup, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return StoredBackup{}, err
	}
	built, err := s.loadBuilt(ctx, l)
	if err != nil {
		return StoredBackup{}, err
	}
	if _, err := markTime(l, built.mark); err != nil {
		return StoredBackup{}, err
	}
	var stored StoredBackup
	err = s.locked(ctx, l, "backup create", func(ctx context.Context) error {
		var err error
		stored, err = s.storeBackup(ctx, l)
		return err
	})
	return stored, err
}

// storeBackup reads the cluster again, which another change may have rebuilt while the run waited for the lock, then
// takes a snapshot and stores it. The caller holds the lock.
func (s *Service) storeBackup(ctx context.Context, l statestore.Layout) (StoredBackup, error) {
	built, err := s.loadBuilt(ctx, l)
	if err != nil {
		return StoredBackup{}, err
	}
	bootstrapped, err := markTime(l, built.mark)
	if err != nil {
		return StoredBackup{}, err
	}
	servers, _, err := s.reach(ctx, built)
	if err != nil {
		return StoredBackup{}, err
	}
	if _, err := checkReach(ctx, servers); err != nil {
		return StoredBackup{}, err
	}
	snap, err := s.takeSnapshot(ctx, servers)
	if err != nil {
		return StoredBackup{}, err
	}
	if err := statestore.RaiseVersion(ctx, s.Store, l, s.Version); err != nil {
		return StoredBackup{}, err
	}
	backup := l.Backup(s.now(), bootstrapped)
	caps, err := s.Store.Capabilities(ctx)
	if err != nil {
		return StoredBackup{}, err
	}
	_, err = s.Store.Put(ctx, backup.Path, snap.Bytes(), statestore.PutOptions{IfNoneMatch: caps.ConditionalPut})
	switch {
	case errors.Is(err, statestore.ErrPreconditionFailed):
		return StoredBackup{}, fmt.Errorf("backup %s of cluster %s exists already; run the command again",
			backup.Name, l.Cluster())
	case err != nil:
		return StoredBackup{}, fmt.Errorf("store the backup: %w", err)
	}
	return StoredBackup{Cluster: l.Cluster(), Backup: backup, Size: len(snap)}, nil
}

// takeSnapshot asks the servers for a snapshot of Nomad's state. It asks again every snapshotPoll for up to
// snapshotWait while Nomad answers that it is not ready, and warns once before the first wait. Any other error ends
// it at once.
func (s *Service) takeSnapshot(ctx context.Context, api nomadops.API) (secret.Secret, error) {
	start := time.Now()
	warned := false
	for {
		snap, err := api.SaveSnapshot(ctx)
		switch {
		case err == nil:
			return snap, nil
		case !errors.Is(err, nomadops.ErrNotReady):
			return nil, fmt.Errorf("take a snapshot of Nomad: %w", err)
		case time.Since(start) >= snapshotWait:
			return nil, fmt.Errorf("no snapshot from Nomad within %s: %w; after its servers change Nomad takes none "+
				"until its next write: run the command again", snapshotWait, err)
		}
		if !warned {
			warned = true
			s.warn(fmt.Sprintf("Nomad cannot take a snapshot yet: %v; trying again for up to %s", err, snapshotWait))
		}
		select {
		case <-time.After(snapshotPoll):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// markTime returns the time that the bootstrap mark holds.
func markTime(l statestore.Layout, mark []byte) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, string(bytes.TrimSpace(mark)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s does not hold a time: %w", l.NomadBootstrapped(), err)
	}
	return t, nil
}

// checkReach returns the nodes that the servers list, a read that needs a leader and a token that Nomad accepts. A
// run that cannot read them ends before it changes anything.
func checkReach(ctx context.Context, api nomadops.API) ([]nomadops.Node, error) {
	nodes, err := api.Nodes(ctx)
	if err != nil {
		err = fmt.Errorf("reach Nomad: %w", err)
		if errors.Is(err, nomadops.ErrNotReady) {
			err = withReachHint(err)
		}
		return nil, err
	}
	return nodes, nil
}
