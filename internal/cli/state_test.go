package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/statestore"
)

func TestStateUnlockNotLocked(t *testing.T) {
	s := withCluster(t)
	for _, args := range [][]string{{"prod"}, {"--name", "prod"}, {"prod", "--force"}} {
		wantOK(t, runIn(t, "", append([]string{"state", "unlock", "--state", s.url}, args...)...),
			"cluster prod is not locked\n")
	}
}

// TestStateUnlockMissingCluster says that the cluster is missing, rather than that it is not locked.
func TestStateUnlockMissingCluster(t *testing.T) {
	for _, s := range []state{newState(t), withCluster(t)} {
		for _, args := range [][]string{{"dev"}, {"dev", "--force"}} {
			wantError(t, runIn(t, "", append([]string{"state", "unlock", "--state", s.url}, args...)...),
				"Error: cluster dev not found in "+s.url+"\n")
		}
	}
}

// TestStateUnlockRunningHolder is refused even with --force: the OS holds a file store's lock for its holder.
func TestStateUnlockRunningHolder(t *testing.T) {
	s := withCluster(t)
	held := holdLock(t, s)
	want := "Error: " + (&statestore.LockedError{Cluster: "prod", Holder: held.Lease()}).Error() +
		"; that tent is still running and holds a file lock that only it can release: stop that process first\n"
	wantError(t, runIn(t, "", "state", "unlock", "prod", "--state", s.url), want)
	wantError(t, runIn(t, "", "state", "unlock", "prod", "--force", "--state", s.url), want)
	if err := held.Release(t.Context()); err != nil {
		t.Errorf("the lock was taken from its holder: %v", err)
	}
}

// deadHolder is the lease of a tent that died holding the lock.
var deadHolder = statestore.Lease{
	ID: "other", Owner: "ops", Host: "ci", PID: 42, Operation: "update",
	AcquiredAt: time.Date(2026, 9, 26, 12, 0, 1, 0, time.UTC),
	ExpiresAt:  time.Date(2026, 9, 26, 12, 2, 1, 0, time.UTC),
}

// leaveLease writes the lease that a tent leaves in a file store when it dies holding the lock: the OS released the
// file lock.
func leaveLease(t *testing.T, s state) string {
	t.Helper()
	dir := filepath.Join(s.root, ".tent-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(deadHolder)
	if err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(dir, "prod.lease")
	for path, content := range map[string][]byte{filepath.Join(dir, "prod.lock"): nil, lease: data} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}

func TestStateUnlockDeadHolder(t *testing.T) {
	s := withCluster(t)
	lease := leaveLease(t, s)
	wantOK(t, runIn(t, "", "state", "unlock", "prod", "--state", s.url),
		"removed the lock of cluster prod held by ops@ci (pid 42) for update since 2026-09-26 12:00:01 UTC\n")
	if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lease is left: %v", err)
	}
	wantOK(t, runIn(t, "", "state", "unlock", "prod", "--state", s.url), "cluster prod is not locked\n")
}

func TestStateUnlockJSON(t *testing.T) {
	s := withCluster(t)
	leaveLease(t, s)
	wantOK(t, runIn(t, "", "state", "unlock", "prod", "-o", "json", "--state", s.url), `{
  "cluster": "prod",
  "removed": {
    "id": "other",
    "owner": "ops",
    "host": "ci",
    "pid": 42,
    "operation": "update",
    "acquiredAt": "2026-09-26T12:00:01Z",
    "expiresAt": "2026-09-26T12:02:01Z"
  }
}
`)
	wantOK(t, runIn(t, "", "state", "unlock", "prod", "-o", "json", "--state", s.url), `{
  "cluster": "prod",
  "removed": null
}
`)
}

func TestStateUnlockForce(t *testing.T) {
	s := withCluster(t)
	now := time.Now().UTC()
	live := statestore.Lease{
		ID: "other", Owner: "ops", Host: "ci", PID: 42, Operation: "update",
		AcquiredAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	data, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	s.put(t, "prod/lock", string(data))
	asLease := func(st statestore.Store) statestore.Store { return leaseStore{st} }

	wantError(t, runWithStore(t, asLease, "state", "unlock", "prod", "--state", s.url),
		"Error: "+(&statestore.LockedError{Cluster: "prod", Holder: live}).Error()+
			"; run it again with --force if that tent is gone\n")
	wantOK(t, runWithStore(t, asLease, "state", "unlock", "prod", "--force", "--state", s.url),
		"removed the lock of cluster prod held by "+live.String()+"\n")
	s.want(t, prodObjects)
}
