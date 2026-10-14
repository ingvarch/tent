package app_test

import (
	"testing"

	"github.com/ingvarch/tent/internal/statestore/memstore"
)

// TestSnapshotLeavesOutTheLock checks that the lock's lease is not in a snapshot, as a file store's flock never
// showed in its list: the lease is renewed while a lock is held, so a snapshot that held it would change with time.
func TestSnapshotLeavesOutTheLock(t *testing.T) {
	t.Parallel()
	s := memstore.New()
	put(t, s, lockPath, []byte("lease"))
	put(t, s, clusterPath, []byte("cluster"))
	wantSnapshot(t, s, map[string]string{clusterPath: "cluster"})
}
