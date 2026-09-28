package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// waitNotice is what a change prints once when it finds the lock of cluster prod held.
func waitNotice(holder statestore.Lease, timeout string) string {
	return "cluster prod is locked by " + holder.String() + "; waiting up to " + timeout + " (--lock-timeout)\n"
}

func TestReplaceStopsWaitingForTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		held := holdLock(t, s)
		got := runIn(t, biggerWorkers(t), "replace", "-f", "-", "--lock-timeout", "200ms", "--state", s.url)
		locked := (&statestore.LockedError{Cluster: "prod", Holder: held.Lease()}).Error()
		wantResult(t, got, 1, "",
			waitNotice(held.Lease(), "200ms")+"Error: "+locked+"; gave up after 200ms\n")
		s.want(t, prodObjects)
	})
}

func TestReplaceWaitsForTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := withCluster(t)
		held := holdLock(t, s)
		r := startIn(t, biggerWorkers(t), "replace", "-f", "-", "--lock-timeout", "1m", "--state", s.url)

		time.Sleep(10 * time.Second) // five tries
		synctest.Wait()
		notice := waitNotice(held.Lease(), "1m0s")
		if got := r.errOut.String(); got != notice {
			t.Errorf("stderr while waiting = %q, want %q", got, notice)
		}
		s.want(t, prodObjects)

		if err := held.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		wantResult(t, r.wait(), 0, "node group workers replaced\n", notice+openAPIWarning)
		s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
	})
}

func TestWeakLockWarning(t *testing.T) {
	opts, _, err := resolve(t, "--state", newState(t).url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetErr(&errOut)
	svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	svc.OnWeakLock()
	const want = "WARNING: this state store does not support conditional writes, so its cluster lock is best " +
		"effort: two tents may change the cluster at once\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr = %q\nwant     %q", got, want)
	}
}

// TestReplaceTakesOverALockLeftBehind warns when it takes the lock from a tent that died holding it.
func TestReplaceTakesOverALockLeftBehind(t *testing.T) {
	s := withCluster(t)
	leaveLease(t, s)
	got := runIn(t, biggerWorkers(t), "replace", "-f", "-", "--state", s.url)
	wantResult(t, got, 0, "node group workers replaced\n",
		"WARNING: took over the cluster lock from ops@ci (pid 42) for update since 2026-09-26 12:00:01 UTC, whose "+
			"lease had expired or whose tent had ended\n"+openAPIWarning)
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
}

// errSaved is the error of a change that was saved although its lock was lost.
var errSaved = errors.New("the change is saved, but the lock of cluster prod was lost before tent released it")

func TestReport(t *testing.T) {
	failed := errors.New("failed")
	for _, tc := range []struct {
		name    string
		did     bool
		err     error
		printed bool
	}{
		{"done", true, nil, true},
		{"nothing to show", false, nil, true},
		{"done with an error", true, errSaved, true},
		{"failed", false, failed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			printed := false
			err := report(tc.did, tc.err, func() error {
				printed = true
				return nil
			})
			if printed != tc.printed || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Errorf("report printed = %t, returned %v; want %t and %v", printed, err, tc.printed, tc.err)
			}
		})
	}
	printErr := errors.New("stdout is closed")
	err := report(true, errSaved, func() error { return printErr })
	if !errors.Is(err, errSaved) || !errors.Is(err, printErr) {
		t.Errorf("report returned %v, want both errors", err)
	}
}

func TestWriteObjectsPrintsAChangeSavedWithoutItsLock(t *testing.T) {
	opts, _, err := resolve(t, "--state", newState(t).url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	write := func(*app.Service, context.Context, spec.Objects, bool) ([]app.Change, error) {
		return []app.Change{{Kind: v1alpha1.KindNodeGroup, Name: "workers", Action: app.Replaced}}, errSaved
	}
	if err := writeObjects(cmd, opts, spec.Objects{}, false, write, false); !errors.Is(err, errSaved) {
		t.Errorf("writeObjects returned %v, want %v", err, errSaved)
	}
	if got, want := out.String(), "node group workers replaced\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestWaitForAHolderWithoutALease names the holder in the same words while it waits and when it gives up.
func TestWaitForAHolderWithoutALease(t *testing.T) {
	s := withCluster(t)
	holdLock(t, s)
	if err := os.Remove(filepath.Join(s.root, ".tent-locks", "prod.lease")); err != nil {
		t.Fatal(err)
	}
	const unnamed = "cluster prod is locked, but its holder has written no lease"
	// One try finds the lock held; the next would come after 2s, past the timeout.
	got := runIn(t, biggerWorkers(t), "replace", "-f", "-", "--lock-timeout", "200ms", "--state", s.url)
	wantResult(t, got, 1, "",
		unnamed+"; waiting up to 200ms (--lock-timeout)\nError: "+unnamed+"; gave up after 200ms\n")
	s.want(t, prodObjects)
}
