package app_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// unjoinedWorker builds the test cluster in which prod-workers-1 never registers: the build stops at its registration
// with its machine ready and without the joined label. The specs then ask for one worker, so that machine is the
// surplus node.
func unjoinedWorker(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld) {
	t.Helper()
	svc, f, w := newRelease(t)
	w.Withhold("prod-workers-1")
	if _, err := svc.Update(t.Context(), "prod", true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the build: %v, want a deadline", err)
	}
	wantJoined(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0")
	mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 1"))
	return svc, f, w
}

// nomadNames returns the names of the calls that the Nomad fake took after the first n.
func nomadNames(w *nomadWorld, n int) []string {
	var names []string
	for _, c := range w.Log()[n:] {
		names = append(names, c.Name)
	}
	return names
}

// twinOf seeds a machine that carries the labels of the instance id of f, except the joined label, and the user data
// of the create request of id, and attaches it to the same VPC: a twin of one name that a second create made. It
// returns the twin's ID.
func twinOf(t *testing.T, f *vultrfake.Fake, id string) string {
	t.Helper()
	req, ok := f.CreateRequest(id)
	if !ok {
		t.Fatalf("the fake has no create request for %s", id)
	}
	in := f.Instances()[0]
	for _, c := range f.Instances() {
		if c.ID == id {
			in = c
		}
	}
	in.ID, in.MainIP = "", ""
	in.Tags = withoutJoined(in.Tags)
	twin := f.AddInstance(t, in, f.InstanceVPCs(id)[0].ID).ID
	f.SetInstanceUserData(t, twin, req.UserData)
	return twin
}

// tagsOf returns the tags of the instance id of f, and whether f lists it.
func tagsOf(f *vultrfake.Fake, id string) ([]string, bool) {
	for _, in := range f.Instances() {
		if in.ID == id {
			return in.Tags, true
		}
	}
	return nil, false
}

// wantLines fails the test unless the progress lines got are want.
func wantLines(t *testing.T, got, want []string) {
	t.Helper()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the progress (-want +got):\n%s", diff)
	}
}

// withoutJoined returns tags without the joined label.
func withoutJoined(tags []string) []string {
	var out []string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, cloud.LabelJoined+"=") {
			out = append(out, tag)
		}
	}
	return out
}

// privateOf returns the private address of the instance id of f.
func privateOf(t *testing.T, f *vultrfake.Fake, id string) netip.Addr {
	t.Helper()
	vpcs := f.InstanceVPCs(id)
	if len(vpcs) == 0 {
		t.Fatalf("instance %s has no VPC", id)
	}
	return netip.MustParseAddr(vpcs[0].IPAddress)
}

// TestUpdateDeletesASurplusNodeThatNeverJoined deletes a machine that no node of Nomad stands for, after it asked
// Nomad for the clients and for the Raft configuration.
func TestUpdateDeletesASurplusNodeThatNeverJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w := unjoinedWorker(t)
		before := len(w.Log())

		plan := mustUpdate(t, svc)

		wantNodeChanges(t, plan, deleteOf("prod-workers-1", "instance-5", "surplus"))
		if diff := cmp.Diff([]string{"Nodes", "Peers"}, nomadNames(w, before)); diff != "" {
			t.Errorf("the Nomad calls (-want +got):\n%s", diff)
		}
		wantNodes(t, f, server(0), server(1), server(2), worker(0))
		wantJoined(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0")
		wantConverged(t, svc)
	})
}

// TestUpdateKeepsADuplicateThatRegistered finds a twin that registered without the label: it is not deleted, it gets
// the label and the stub, and from then on the plan refuses to delete it.
func TestUpdateKeepsADuplicateThatRegistered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		twin := twinOf(t, f, "instance-4") // a second prod-workers-0
		lines := recordProgress(svc)

		_, err := svc.Update(t.Context(), "prod", true)

		const wantErr = "delete node prod-workers-0 (instance-6): the node has joined Nomad (a registered client at "
		addr := privateOf(t, f, twin)
		wantError(t, err, wantErr+addr.String()+"); update deletes only nodes that never joined")
		wantLines(t, *lines, []string{
			"node started scrub prod-workers-0",
			"node done scrub prod-workers-0",
			"node started delete prod-workers-0",
			"node failed delete prod-workers-0: " + err.Error(),
		})
		wantJoined(t, f, append(allNames, "prod-workers-0")...)
		if unscrubbed := unscrubbedJoined(f); len(unscrubbed) != 0 {
			t.Errorf("the joined machines %v hold user data other than the stub", unscrubbed)
		}
		wantRefused(t, svc, f, duplicateRefusal("prod-workers-0", "instance-6", "instance-4"))
	})
}

// TestUpdateFailedScrubStillKeepsTheJoinedNode fails the scrub of a twin that registered: the twin is not deleted
// and stays without the label, and the next update, which can scrub it, labels it and refuses its delete.
func TestUpdateFailedScrubStillKeepsTheJoinedNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		twin := twinOf(t, f, "instance-4") // a second prod-workers-0
		boom := errors.New("boom")
		f.Fail(t, "UpdateInstance", boom, 1)
		calls := len(f.Calls())

		_, err := svc.Update(t.Context(), "prod", true)

		const prefix = "delete node prod-workers-0 (instance-6): the node has joined Nomad (a registered client at "
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, boom) {
			t.Errorf("Update = %v, want an error that starts with %q and wraps boom", err, prefix)
		}
		for _, c := range f.Calls()[calls:] {
			if c.Name == "DeleteInstance" {
				t.Errorf("the update called %s", c.Name)
			}
		}
		if tags, ok := tagsOf(f, twin); !ok {
			t.Errorf("the twin %s is gone", twin)
		} else if isJoined(tags) {
			t.Errorf("the twin carries the joined label: %v", tags)
		}
		wantLockFree(t, svc.Store)

		_, err = svc.Update(t.Context(), "prod", true)

		wantError(t, err, "delete node prod-workers-0 (instance-6): the node has joined Nomad (a registered client at "+
			privateOf(t, f, twin).String()+"); update deletes only nodes that never joined")
		wantJoined(t, f, append(allNames, "prod-workers-0")...)
		wantRefused(t, svc, f, duplicateRefusal("prod-workers-0", "instance-6", "instance-4"))
	})
}

// TestUpdateKeepsAServerOfTheRaftConfiguration finds that the Raft configuration lists a twin server that has no
// label: it is not deleted, it gets the label and the stub, and the plan refuses to delete it from then on.
func TestUpdateKeepsAServerOfTheRaftConfiguration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		twin := twinOf(t, f, "instance-3") // a second prod-servers-2

		_, err := svc.Update(t.Context(), "prod", true)

		addr := privateOf(t, f, twin)
		wantError(t, err, "delete node prod-servers-2 (instance-6): the node has joined Nomad (a server at "+
			netip.AddrPortFrom(addr, 4647).String()+"); update deletes only nodes that never joined")
		wantJoined(t, f, append(allNames, "prod-servers-2")...)
		if unscrubbed := unscrubbedJoined(f); len(unscrubbed) != 0 {
			t.Errorf("the joined machines %v hold user data other than the stub", unscrubbed)
		}
		wantRefused(t, svc, f, duplicateRefusal("prod-servers-2", "instance-6", "instance-3"))
	})
}

// TestUpdateDeletesNothingWhenNomadCannotBeAsked stops the delete of a machine that never joined when either call
// that tells whether it joined fails, and shows the delete as started and failed. The next update deletes it.
func TestUpdateDeletesNothingWhenNomadCannotBeAsked(t *testing.T) {
	boom := errors.New("boom")
	for _, call := range []string{"Nodes", "Peers"} {
		t.Run(call, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w := unjoinedWorker(t)
				w.Fail(t, call, boom)
				view := cloudView(f)
				lines := recordProgress(svc)

				_, err := svc.Update(t.Context(), "prod", true)

				const prefix = "delete node prod-workers-1 (instance-5): ask Nomad whether the node joined: "
				if err == nil || !strings.HasPrefix(err.Error(), prefix) || !errors.Is(err, boom) {
					t.Fatalf("Update = %v, want an error that starts with %q and wraps boom", err, prefix)
				}
				wantLines(t, *lines, []string{
					"node started delete prod-workers-1",
					"node failed delete prod-workers-1: " + err.Error(),
				})
				if diff := cmp.Diff(view, cloudView(f)); diff != "" {
					t.Errorf("the cloud changed (-before +after):\n%s", diff)
				}
				wantLockFree(t, svc.Store)

				mustUpdate(t, svc)

				wantNodes(t, f, server(0), server(1), server(2), worker(0))
				wantConverged(t, svc)
			})
		})
	}
}

// TestUpdateDeleteNeedsNomad fails an update whose only change is a delete before it calls the cloud when the
// service has no way to reach Nomad.
func TestUpdateDeleteNeedsNomad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := unjoinedWorker(t)
		svc.Nomad = nil
		view := cloudView(f)

		_, err := svc.Update(t.Context(), "prod", true)

		wantError(t, err, "no Nomad client is set up")
		if diff := cmp.Diff(view, cloudView(f)); diff != "" {
			t.Errorf("the cloud changed (-before +after):\n%s", diff)
		}
	})
}

// TestUpdateAfterACutRollNamesRollingUpdate cuts a rolling update just before the first delete of an outdated node,
// when the new node has joined and the group is one machine above its size, and updates: the plan refuses to delete
// the new node as surplus, with and without apply, and its advice names tent rolling-update cluster.
func TestUpdateAfterACutRollNamesRollingUpdate(t *testing.T) {
	calls := uninterruptedRoll(t)
	del := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(callKey(call), "DeleteInstance ") })
	if del < 0 {
		t.Fatal("the roll made no DeleteInstance call")
	}
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _, _ := cutWorld(t)
		cut := cutCase{index: del + 1, key: callKey(calls[del]), n: 1}
		runCut(t, f, w, svc.Store, cut, func(ctx context.Context) error {
			_, err := svc.RollingUpdate(ctx, "prod", app.RollOptions{Apply: true})
			return err
		})

		wantRefused(t, svc, f, joinedRefusal(true, "prod-workers-2 (ID instance-6, surplus)"))
	})
}
