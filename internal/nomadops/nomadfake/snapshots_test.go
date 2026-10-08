package nomadfake_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// save saves a snapshot and fails the test when the call fails.
func save(t *testing.T, a nomadops.API) secret.Secret {
	t.Helper()
	snap, err := a.SaveSnapshot(t.Context())
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	return snap
}

// TestSaveSnapshotNumbersTheSaves checks that the saves are numbered from 1, whichever client makes them.
func TestSaveSnapshotNumbersTheSaves(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	other := f.Client(nomadops.Config{Address: "198.51.100.2:4646"})

	got := []string{string(save(t, a)), string(save(t, other)), string(save(t, a))}

	want := []string{"nomadfake snapshot 1", "nomadfake snapshot 2", "nomadfake snapshot 3"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("snapshots (-want +got):\n%s", diff)
	}
	wantCalls(t, f, bootstrapCall, nomadfake.Call{Name: "SaveSnapshot"},
		nomadfake.Call{Name: "SaveSnapshot", Server: "198.51.100.2:4646"}, nomadfake.Call{Name: "SaveSnapshot"})
}

// TestSaveSnapshotCountsOnlyTheSavesThatHappened checks that a save that Nomad would refuse does not use a number,
// and that a save whose answer was lost does: the cluster took it.
func TestSaveSnapshotCountsOnlyTheSavesThatHappened(t *testing.T) {
	f, a := newAPI()
	_, err := a.SaveSnapshot(t.Context()) // before the bootstrap
	checkErr(t, err, "nomadfake: SaveSnapshot: permission denied", false)
	if err := a.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	f.LoseResponse(t, "SaveSnapshot")
	_, err = a.SaveSnapshot(t.Context())
	checkErr(t, err, "nomadfake: SaveSnapshot: the answer was lost", true)

	if got := string(save(t, a)); got != "nomadfake snapshot 2" {
		t.Errorf("SaveSnapshot() = %q, want the second save: the lost one counts, the refused one does not", got)
	}
}

// TestRestoreSnapshotBeforeTheBootstrapRestoresNothing checks that a restore that the fake refuses as Nomad's 403 is
// not recorded.
func TestRestoreSnapshotBeforeTheBootstrapRestoresNothing(t *testing.T) {
	f, a := newAPI()

	err := a.RestoreSnapshot(t.Context(), secret.Secret("nomadfake snapshot 1"))

	checkErr(t, err, "nomadfake: RestoreSnapshot: permission denied", false)
	if got := f.Restored(); len(got) != 0 {
		t.Errorf("Restored() = %v after a refused restore, want none", got)
	}
}

// TestRestoreSnapshotRecordsACopy checks that a restore of a snapshot that the fake saved is recorded as a copy, in
// order, and that neither the caller nor the reader of Restored shares memory with the record.
func TestRestoreSnapshotRecordsACopy(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	first, second := save(t, a), save(t, a)
	restore := slices.Clone(first)

	if err := a.RestoreSnapshot(t.Context(), restore); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	restore[len(restore)-1]++ // the caller reuses its slice
	if err := a.RestoreSnapshot(t.Context(), second); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	got := f.Restored()
	got[0][0]++ // the reader changes what it got

	want := []secret.Secret{first, second}
	if diff := cmp.Diff(want, f.Restored()); diff != "" {
		t.Errorf("Restored() (-want +got):\n%s", diff)
	}
}

// TestRestoredIsEmptyBeforeARestore checks that Restored lists nothing before the first restore.
func TestRestoredIsEmptyBeforeARestore(t *testing.T) {
	f, _ := newBootstrappedAPI(t)

	if got := f.Restored(); len(got) != 0 {
		t.Errorf("Restored() = %v, want none", got)
	}
}

// TestRestoreSnapshotRefusesOtherBytes checks that bytes that are not a snapshot of the fake fail as Nomad's 500 for a
// file that is no snapshot does, which may succeed later by its class, and that nothing is recorded.
func TestRestoreSnapshotRefusesOtherBytes(t *testing.T) {
	const want = "nomadfake: RestoreSnapshot: 500: failed to restore from snapshot: failed to decompress snapshot: " +
		"gzip: invalid header"
	for name, snap := range map[string]string{
		"text":                 "not a snapshot",
		"a gzip header":        "\x1f\x8b\x08\x00",
		"the prefix, no space": "nomadfake snapshot",
		"the prefix, later":    " nomadfake snapshot 1",
		"another case":         "Nomadfake snapshot 1",
	} {
		t.Run(name, func(t *testing.T) {
			f, a := newBootstrappedAPI(t)

			err := a.RestoreSnapshot(t.Context(), secret.Secret(snap))

			checkErr(t, err, want, true)
			if got := f.Restored(); len(got) != 0 {
				t.Errorf("Restored() = %v after a refused restore, want none", got)
			}
		})
	}
}

// TestRestoreSnapshotTakesAnyBytesAfterThePrefix checks that the snapshot of the fake is the prefix and what follows
// it, so that a test can restore a snapshot that it made up.
func TestRestoreSnapshotTakesAnyBytesAfterThePrefix(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	snap := secret.Secret("nomadfake snapshot of the week before")

	if err := a.RestoreSnapshot(t.Context(), snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	if got := f.Restored(); len(got) != 1 || !bytes.Equal(got[0], snap) {
		t.Errorf("Restored() = %v, want the snapshot", got)
	}
}

// TestRestoreSnapshotRefusesAnEmptySnapshotBeforeAnyCall checks that the fake refuses what the client refuses, and
// logs no call.
func TestRestoreSnapshotRefusesAnEmptySnapshotBeforeAnyCall(t *testing.T) {
	f, a := newBootstrappedAPI(t)

	checkErr(t, a.RestoreSnapshot(t.Context(), nil), "nomadfake: RestoreSnapshot: no snapshot", false)
	checkErr(t, a.RestoreSnapshot(t.Context(), secret.Secret{}), "nomadfake: RestoreSnapshot: no snapshot",
		false)

	wantCalls(t, f, bootstrapCall)
	if got := f.Restored(); len(got) != 0 {
		t.Errorf("Restored() = %v, want none", got)
	}
}

// TestRestoreSnapshotChangesNothingElse checks that a restore leaves the nodes, peers, members, health and leader as
// they are, and that a lost answer of a restore still records it: the cluster took it.
func TestRestoreSnapshotChangesNothingElse(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	registerTableNode(f)
	setTablePeers(f)
	setTableMembers(f)
	health := nomadops.Health{Healthy: true, Voters: 3}
	f.SetHealth(health)
	snap := save(t, a)

	f.LoseResponse(t, "RestoreSnapshot")
	checkErr(t, a.RestoreSnapshot(t.Context(), snap), "nomadfake: RestoreSnapshot: the answer was lost", true)

	if got := f.Restored(); len(got) != 1 {
		t.Errorf("Restored() lists %d restores after a lost answer, want 1", len(got))
	}
	if nodes, err := a.Nodes(t.Context()); err != nil || len(nodes) != 1 || !cmp.Equal(nodes[0], tableNode, equateAddrs) {
		t.Errorf("Nodes() = %+v, %v; want the node as it was", nodes, err)
	}
	if peers, err := a.Peers(t.Context()); err != nil || !cmp.Equal(peers, tablePeers, equateAddrs) {
		t.Errorf("Peers() = %+v, %v; want the peers as they were", peers, err)
	}
	if members, err := a.Members(t.Context()); err != nil || !cmp.Equal(members, tableMembers, equateAddrs) {
		t.Errorf("Members() = %+v, %v; want the members as they were", members, err)
	}
	if h, err := a.Health(t.Context()); err != nil || !cmp.Equal(h, health, equateAddrs) {
		t.Errorf("Health() = %+v, %v; want the health as it was", h, err)
	}
	if got, err := a.Leader(t.Context()); err != nil || got != leader {
		t.Errorf("Leader() = %q, %v; want %s", got, err, leader)
	}
}

// TestNewClusterKeepsTheSnapshots checks that NewCluster keeps the numbering of the saves and the restores that were
// recorded, as it keeps the log of calls.
func TestNewClusterKeepsTheSnapshots(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	snap := save(t, a)
	if err := a.RestoreSnapshot(t.Context(), snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	f.NewCluster()
	f.SetLeader(leader)
	if err := a.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if got := string(save(t, a)); got != "nomadfake snapshot 2" {
		t.Errorf("SaveSnapshot() after NewCluster = %q, want the second save", got)
	}
	if diff := cmp.Diff([]secret.Secret{snap}, f.Restored()); diff != "" {
		t.Errorf("Restored() after NewCluster (-want +got):\n%s", diff)
	}
}

// TestSnapshotCallsHideTheSnapshot checks that no print of a snapshot, of the fake or of its log of calls shows its
// bytes, and that the error of a refused restore does not either.
func TestSnapshotCallsHideTheSnapshot(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	snap := save(t, a)
	if err := a.RestoreSnapshot(t.Context(), snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	other := secret.Secret("a secret that is no snapshot of the fake")
	err := a.RestoreSnapshot(t.Context(), other)
	if err == nil || errors.Is(err, nomadops.ErrGone) {
		t.Fatalf("RestoreSnapshot of other bytes = %v, want a refusal", err)
	}

	secrets := map[string][]byte{"the snapshot": snap, "the other bytes": other}
	secrettest.CheckHidden(t, secrettest.Printed(t, f.Calls()), secrets, "")
	secrettest.CheckHidden(t, secrettest.Printed(t, f.Restored()), secrets, "[secret, 20 bytes]")
	secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, secrets, "")
	secrettest.CheckHidden(t, secrettest.Printed(t, f), secrets, "")
}
