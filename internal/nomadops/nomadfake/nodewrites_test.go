package nomadfake_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// workers lists two nodes, n-1 and n-2, that are ready and eligible.
func workers(f *nomadfake.Fake) {
	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready", Eligible: true})
	f.Register(nomadops.Node{ID: "n-2", Name: "prod-workers-2", Status: "ready", Eligible: true})
}

// listed returns the nodes that the cluster lists, and fails the test when it cannot read them.
func listed(t *testing.T, a nomadops.API) []nomadops.Node {
	t.Helper()
	nodes, err := a.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	return nodes
}

// checkGone fails t unless err is the error of a call on a node that is not in the cluster: its text is want, and it
// matches ErrGone and not ErrNotReady.
func checkGone(t *testing.T, err error, want string) {
	t.Helper()
	checkErr(t, err, want, false)
	if !errors.Is(err, nomadops.ErrGone) {
		t.Errorf("errors.Is(%v, ErrGone) = false, want true", err)
	}
}

func TestMarkIneligible(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	workers(f)

	if err := a.MarkIneligible(t.Context(), "n-1"); err != nil {
		t.Fatalf("MarkIneligible: %v", err)
	}
	if err := a.MarkIneligible(t.Context(), "n-1"); err != nil {
		t.Errorf("MarkIneligible again: %v, want success: an ineligible node stays so", err)
	}

	want := []nomadops.Node{
		{ID: "n-1", Name: "prod-workers-1", Status: "ready"},
		{ID: "n-2", Name: "prod-workers-2", Status: "ready", Eligible: true},
	}
	if diff := cmp.Diff(want, listed(t, a), equateAddrs); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
	call := nomadfake.Call{Name: "MarkIneligible", Arg: "n-1"}
	wantCalls(t, f, bootstrapCall, call, call, nomadfake.Call{Name: "Nodes"})
}

func TestMarkIneligibleOfAnUnknownNode(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	workers(f)

	checkGone(t, a.MarkIneligible(t.Context(), "n-9"), "nomadfake: MarkIneligible: node not found")

	if got := listed(t, a); !got[0].Eligible || !got[1].Eligible {
		t.Errorf("Nodes() = %+v, want both nodes eligible still", got)
	}
}

func TestNodeWritesAreRefusedForABadArgumentBeforeAnyCall(t *testing.T) {
	f, a := drainFixture(t)
	for _, id := range []string{"", "n/1", "n 1"} {
		if err := a.MarkIneligible(t.Context(), id); err == nil || errors.Is(err, nomadops.ErrGone) {
			t.Errorf("MarkIneligible(%q) = %v, want a permanent error", id, err)
		}
		if err := a.Purge(t.Context(), id); err == nil || errors.Is(err, nomadops.ErrGone) {
			t.Errorf("Purge(%q) = %v, want a permanent error", id, err)
		}
		if err := a.Drain(t.Context(), id, tableDrain); err == nil || errors.Is(err, nomadops.ErrGone) {
			t.Errorf("Drain(%q) = %v, want a permanent error", id, err)
		}
	}
	checkErr(t, a.MarkIneligible(t.Context(), ""), "nomadfake: MarkIneligible: no node ID", false)
	checkErr(t, a.Purge(t.Context(), "n/1"),
		`nomadfake: Purge: node ID "n/1" has a character other than an ASCII letter, a digit or "-"`, false)
	for _, deadline := range []time.Duration{0, -time.Second} {
		err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: deadline})
		checkErr(t, err, "nomadfake: Drain: drain: deadline "+deadline.String()+" is not above zero", false)
	}
	wantCalls(t, f, bootstrapCall)
	if got := listed(t, a); !got[0].Eligible || got[0].Draining {
		t.Errorf("Nodes() = %+v, want n-1 untouched", got)
	}
}

// drainFixture is a cluster with the workers, and a client of it.
func drainFixture(t *testing.T) (*nomadfake.Fake, nomadops.API) {
	t.Helper()
	f, a := newBootstrappedAPI(t)
	workers(f)
	return f, a
}

// draining is n-1 as the cluster lists it while it drains: ineligible, with a last drain that is under way.
func draining(meta map[string]string) nomadops.Node {
	return nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready", Draining: true,
		LastDrain: nomadops.LastDrain{Status: "draining", Meta: meta}}
}

// completed is n-1 after its drain completed: ineligible, with the meta kept.
func completed(meta map[string]string) nomadops.Node {
	return nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready",
		LastDrain: nomadops.LastDrain{Status: "complete", Meta: meta}}
}

// checkNode fails t unless the cluster lists n-1 as want.
func checkNode(t *testing.T, a nomadops.API, want nomadops.Node) {
	t.Helper()
	got := listed(t, a)
	if diff := cmp.Diff(want, got[0], equateAddrs); diff != "" {
		t.Errorf("node n-1 (-want +got):\n%s", diff)
	}
}

// TestDrainCompletesAtTheFirstReadByDefault checks that, with no SetDrainReads, a drain that reaches the cluster is
// complete when the nodes are read, and that the node stays ineligible and keeps the meta.
func TestDrainCompletesAtTheFirstReadByDefault(t *testing.T) {
	f, a := drainFixture(t)
	meta := map[string]string{"tent_machine": "m-1"}

	if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: meta}); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	checkNode(t, a, completed(meta))
	checkNode(t, a, completed(meta)) // and stays so
	if got := listed(t, a)[1]; !got.Eligible || got.Draining {
		t.Errorf("node n-2 = %+v, want it untouched", got)
	}
	wantCalls(t, f, bootstrapCall, nomadfake.Call{Name: "Drain", Arg: "n-1 1h0m0s tent_machine=m-1"},
		nomadfake.Call{Name: "Nodes"}, nomadfake.Call{Name: "Nodes"}, nomadfake.Call{Name: "Nodes"})
}

// TestSetDrainReads checks that a drain shows as draining for as many reads of the nodes as SetDrainReads set, and is
// complete at the next one.
func TestSetDrainReads(t *testing.T) {
	f, a := drainFixture(t)
	meta := map[string]string{"tent_machine": "m-1"}
	f.SetDrainReads(2)
	if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: meta}); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	checkNode(t, a, draining(meta))
	checkNode(t, a, draining(meta))
	checkNode(t, a, completed(meta))
	checkNode(t, a, completed(meta))
}

// TestSetDrainReadsAppliesToLaterDrains checks that SetDrainReads sets the count of the drains that start after it, and
// that NewCluster sets it back to none.
func TestSetDrainReadsAppliesToLaterDrains(t *testing.T) {
	f, a := drainFixture(t)
	drain := func(id string) {
		t.Helper()
		if err := a.Drain(t.Context(), id, nomadops.DrainRequest{Deadline: time.Hour}); err != nil {
			t.Fatalf("Drain %s: %v", id, err)
		}
	}
	drain("n-1") // before: no reads to wait for
	f.SetDrainReads(1)
	drain("n-2")

	got := listed(t, a)
	if got[0].Draining || !got[1].Draining {
		t.Errorf("Nodes() = %+v, want n-1 complete and n-2 draining", got)
	}

	f.NewCluster()
	f.SetLeader(leader)
	f.SetBootstrapped(bootstrapSecret)
	workers(f)
	drain("n-1")
	if got := listed(t, a); got[0].Draining {
		t.Errorf("Nodes() after NewCluster = %+v, want n-1 complete at once", got)
	}
}

// TestDrainDeadline checks that a drain that has not completed by its reads is complete at its deadline: at the
// first read at or after the start plus the deadline, by the clock, and not before.
func TestDrainDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := drainFixture(t)
		f.SetDrainReads(1000)
		meta := map[string]string{"tent_machine": "m-1"}
		if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: meta}); err != nil {
			t.Fatalf("Drain: %v", err)
		}

		time.Sleep(time.Hour - time.Nanosecond)
		checkNode(t, a, draining(meta))
		time.Sleep(time.Nanosecond)
		checkNode(t, a, completed(meta))
	})
}

// TestDrainAgainMovesTheDeadlineAndTheMetaAndKeepsTheReads checks a drain that is asked again while the node drains:
// the deadline is the time of the new request plus its deadline, the meta is the new one, and the reads that were
// counted stay counted.
func TestDrainAgainMovesTheDeadlineAndTheMetaAndKeepsTheReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, a := drainFixture(t)
		f.SetDrainReads(1000)
		first := map[string]string{"tent_machine": "m-1"}
		second := map[string]string{"tent_machine": "m-2"}
		if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: first}); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		time.Sleep(30 * time.Minute)
		if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: second}); err != nil {
			t.Fatalf("Drain again: %v", err)
		}

		time.Sleep(31 * time.Minute) // after the first deadline, before the new one
		checkNode(t, a, draining(second))
		time.Sleep(29*time.Minute - time.Nanosecond)
		checkNode(t, a, draining(second))
		time.Sleep(time.Nanosecond)
		checkNode(t, a, completed(second))
	})

	f, a := drainFixture(t)
	f.SetDrainReads(2)
	req := nomadops.DrainRequest{Deadline: time.Hour}
	if err := a.Drain(t.Context(), "n-1", req); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	checkNode(t, a, draining(nil)) // one of two reads
	if err := a.Drain(t.Context(), "n-1", req); err != nil {
		t.Fatalf("Drain again: %v", err)
	}
	checkNode(t, a, draining(nil)) // the second
	checkNode(t, a, completed(nil))
}

// TestDrainOfADownNode checks that a node that is down has its drain complete at once, with the meta, and is
// ineligible.
func TestDrainOfADownNode(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	f.SetDrainReads(5)
	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "down", Eligible: true})
	meta := map[string]string{"tent_machine": "m-1"}

	if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: meta}); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	want := completed(meta)
	want.Status = "down"
	checkNode(t, a, want)
}

func TestDrainOfAnUnknownNode(t *testing.T) {
	_, a := drainFixture(t)

	checkGone(t, a.Drain(t.Context(), "n-9", tableDrain), "nomadfake: Drain: node not found")

	for _, n := range listed(t, a) {
		if n.Draining || !n.Eligible || n.LastDrain.Status != "" {
			t.Errorf("node %s = %+v, want it untouched", n.ID, n)
		}
	}
}

// TestDrainCopiesTheMeta checks that the fake keeps its own copy of the meta of a request, and gives a copy back.
func TestDrainCopiesTheMeta(t *testing.T) {
	_, a := drainFixture(t)
	meta := map[string]string{"tent_machine": "m-1"}
	if err := a.Drain(t.Context(), "n-1", nomadops.DrainRequest{Deadline: time.Hour, Meta: meta}); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	meta["tent_machine"] = "changed"

	got := listed(t, a)
	if m := got[0].LastDrain.Meta["tent_machine"]; m != "m-1" {
		t.Errorf("changing the request's map changed the fake: meta = %q, want m-1", m)
	}
	got[0].LastDrain.Meta["tent_machine"] = "changed again"
	if m := listed(t, a)[0].LastDrain.Meta["tent_machine"]; m != "m-1" {
		t.Errorf("changing a returned map changed the fake: meta = %q, want m-1", m)
	}
}

// TestRegisterAgainEndsADrain checks that a node that a test registers again has the state that the test gave it, not
// the drain that the fake was running for the node before: the drain would complete at the first read.
func TestRegisterAgainEndsADrain(t *testing.T) {
	f, a := drainFixture(t)
	if err := a.Drain(t.Context(), "n-1", tableDrain); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready", Eligible: true})

	checkNode(t, a, nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready", Eligible: true})
}

// TestRegisterWithoutAnIDReplacesTheNodeAndItsDrain checks the same for a node that replaces the drained one by its
// name and address.
func TestRegisterWithoutAnIDReplacesTheNodeAndItsDrain(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "ready", Eligible: true})
	if err := a.Drain(t.Context(), "n-1", tableDrain); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	f.Register(nomadops.Node{Name: "prod-workers-1", Status: "initializing", Eligible: true})

	got := listed(t, a)
	want := []nomadops.Node{{Name: "prod-workers-1", Status: "initializing", Eligible: true}}
	if diff := cmp.Diff(want, got, equateAddrs); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
}

func TestPurge(t *testing.T) {
	f, a := drainFixture(t)

	if err := a.Purge(t.Context(), "n-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if err := a.Purge(t.Context(), "n-1"); err != nil {
		t.Errorf("Purge again: %v, want success: a node that is gone counts as purged", err)
	}
	if err := a.Purge(t.Context(), "n-9"); err != nil {
		t.Errorf("Purge of an unknown node: %v, want success", err)
	}

	want := []nomadops.Node{{ID: "n-2", Name: "prod-workers-2", Status: "ready", Eligible: true}}
	if diff := cmp.Diff(want, listed(t, a), equateAddrs); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
	checkGone(t, a.MarkIneligible(t.Context(), "n-1"), "nomadfake: MarkIneligible: node not found")
	checkGone(t, a.Drain(t.Context(), "n-1", tableDrain), "nomadfake: Drain: node not found")
	wantCalls(t, f, bootstrapCall, nomadfake.Call{Name: "Purge", Arg: "n-1"}, nomadfake.Call{Name: "Purge", Arg: "n-1"},
		nomadfake.Call{Name: "Purge", Arg: "n-9"}, nomadfake.Call{Name: "Nodes"},
		nomadfake.Call{Name: "MarkIneligible", Arg: "n-1"}, nomadfake.Call{Name: "Drain", Arg: "n-1 1h0m0s tent_machine=m-1"})
}

// TestPurgedNodeStaysOutUntilRegistered checks that a purged node does not come back, and that registering it again
// lists it with the state that was given.
func TestPurgedNodeStaysOutUntilRegistered(t *testing.T) {
	f, a := drainFixture(t)
	f.SetDrainReads(5)
	if err := a.Drain(t.Context(), "n-1", tableDrain); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := a.Purge(t.Context(), "n-1"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got := listed(t, a); len(got) != 1 {
		t.Fatalf("Nodes() = %+v, want only n-2", got)
	}

	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-1", Status: "initializing", Eligible: true})

	got := listed(t, a)
	want := []nomadops.Node{
		{ID: "n-2", Name: "prod-workers-2", Status: "ready", Eligible: true},
		{ID: "n-1", Name: "prod-workers-1", Status: "initializing", Eligible: true},
	}
	if diff := cmp.Diff(want, got, equateAddrs); diff != "" {
		t.Errorf("Nodes() (-want +got):\n%s", diff)
	}
}

func TestDrainArg(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  nomadops.DrainRequest
		want string
	}{
		{"no meta", nomadops.DrainRequest{Deadline: 90 * time.Second}, "n-1 1m30s"},
		{"one pair", nomadops.DrainRequest{Deadline: time.Hour, Meta: map[string]string{"tent_machine": "m-1"}},
			"n-1 1h0m0s tent_machine=m-1"},
		{"pairs sorted by key", nomadops.DrainRequest{Deadline: time.Hour,
			Meta: map[string]string{"b": "2", "c": "3", "a": "1"}}, "n-1 1h0m0s a=1 b=2 c=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 20 { // a map's order changes from run to run: one call could pass by chance
				if got := nomadfake.DrainArg("n-1", tc.req); got != tc.want {
					t.Fatalf("DrainArg() = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestPurgeBeforeTheBootstrapKeepsTheNode checks that a purge that the fake refuses as Nomad's 403 removes nothing.
func TestPurgeBeforeTheBootstrapKeepsTheNode(t *testing.T) {
	f, a := newAPI()
	f.Register(nomadops.Node{ID: "n-1", Name: "prod-workers-0", Status: "down"})

	checkErr(t, a.Purge(t.Context(), "n-1"), "nomadfake: Purge: permission denied", false)

	if err := a.Bootstrap(t.Context(), bootstrapSecret); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if nodes, err := a.Nodes(t.Context()); err != nil || len(nodes) != 1 {
		t.Errorf("Nodes() after a refused purge = %+v, %v; want the node", nodes, err)
	}
}
