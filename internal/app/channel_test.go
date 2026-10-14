package app_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
)

// stableChannel returns a lookup of the release channels in which stable allows Nomad from minimum, recommends
// recommended and tests tested, and pins the CNI plugins of the embedded stable channel. Other names are unknown, as
// for the embedded channels.
func stableChannel(minimum, recommended string, tested ...string) func(string) (*channels.Channel, error) {
	return func(name string) (*channels.Channel, error) {
		embedded, err := channels.Load(name)
		if err != nil || name != "stable" {
			return embedded, err
		}
		return &channels.Channel{
			Name: name, Nomad: channels.Nomad{Minimum: minimum, Recommended: recommended, Tested: tested},
			CNI: embedded.CNI,
		}, nil
	}
}

// withVersion returns the test cluster's Cluster with its Nomad version set to v, quoted, since YAML reads 2.0 as a
// number.
func withVersion(t *testing.T, doc, v string) string {
	t.Helper()
	return edit(t, doc, "region: ams", "region: ams\n  nomad:\n    version: \""+v+"\"")
}

// problem is the problem of the test cluster's Cluster at path.
func problem(path, detail string) v1alpha1.FieldError {
	return v1alpha1.FieldError{Object: "Cluster prod", Path: path, Detail: detail}
}

// specCheck is a use case that checks a Cluster's channel and Nomad version before it changes anything.
type specCheck struct {
	name string
	// prepare fills the store for the use case, which runs with the Cluster doc.
	prepare func(t *testing.T, svc *app.Service, doc string)
	run     func(t *testing.T, svc *app.Service, doc string, apply bool) error
}

// emptyStore leaves the store empty.
func emptyStore(*testing.T, *app.Service, string) {}

// createTestCluster stores the test cluster.
func createTestCluster(t *testing.T, svc *app.Service, _ string) {
	t.Helper()
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
}

var specChecks = []specCheck{
	{"create", emptyStore, func(t *testing.T, svc *app.Service, doc string, apply bool) error {
		_, err := svc.Create(t.Context(), decode(t, doc, serversYAML, workersYAML), apply)
		return err
	}},
	{"replace", createTestCluster, func(t *testing.T, svc *app.Service, doc string, apply bool) error {
		_, err := svc.Replace(t.Context(), decode(t, doc), apply)
		return err
	}},
	{"edit", createTestCluster, func(t *testing.T, svc *app.Service, doc string, apply bool) error {
		_, err := svc.Save(t.Context(), load(t, svc, v1alpha1.KindCluster, ""), decode(t, doc), apply)
		return err
	}},
	{"update", func(t *testing.T, svc *app.Service, doc string) {
		createTestCluster(t, svc, doc)
		put(t, svc.Store, clusterPath, encode(t, doc))
	}, func(t *testing.T, svc *app.Service, _ string, apply bool) error {
		_, err := svc.Update(t.Context(), "prod", apply)
		return err
	}},
}

// TestSpecChecksTheChannel refuses a Cluster whose channel tent does not know or whose Nomad version the channel does
// not allow, with a field error, before it writes anything or reaches the cloud.
func TestSpecChecksTheChannel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want v1alpha1.FieldError
		// loaded is what update, which loads the stored Cluster, adds to the want's detail.
		loaded string
	}{
		{"unknown channel", edit(t, clusterYAML, "region: ams", "region: ams\n  channel: x"),
			problem("spec.channel", `unknown channel "x"; known: stable`), ""},
		{"version that is not X.Y.Z", withVersion(t, clusterYAML, "2.0"),
			problem("spec.nomad.version", `"2.0" is not a Nomad version such as 2.0.7`), ""},
		{"version older than the channel's minimum", withVersion(t, clusterYAML, "1.11.0"),
			problem("spec.nomad.version", "1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows"),
			"; run tent upgrade cluster"},
		{"version of the next major", withVersion(t, clusterYAML, "3.0.0"),
			problem("spec.nomad.version", "3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0"), ""},
	} {
		for _, uc := range specChecks {
			t.Run(tc.name+"/"+uc.name, func(t *testing.T) {
				svc := newService(t)
				uc.prepare(t, svc, tc.doc)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
					t.Error("the use case looked up a provider")
					return nil, errors.New("no provider")
				}
				stored := snapshot(t, svc.Store)
				for _, apply := range []bool{false, true} {
					want := tc.want
					if uc.name == "update" {
						want.Detail += tc.loaded
					}
					wantFieldErrors(t, uc.run(t, svc, tc.doc, apply), want)
				}
				wantSnapshot(t, svc.Store, stored)
			})
		}
	}
}

// TestChannelProblemsWithOthers reports the channel's problems first, with the other problems of the spec, and a
// field that has a problem already only once.
func TestChannelProblemsWithOthers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		docs []string
		want []v1alpha1.FieldError
	}{
		{"a channel that is not a name", []string{
			edit(t, clusterYAML, "region: ams", "region: ams\n  channel: Beta"), serversYAML,
		}, []v1alpha1.FieldError{problem("spec.channel", "must match ^[a-z][a-z0-9-]*$")}},
		// Without its channel, the version has nothing to be checked against.
		{"an unknown channel and a version", []string{
			withVersion(t, edit(t, clusterYAML, "region: ams", "region: ams\n  channel: x"), "3.0.0"), serversYAML,
		}, []v1alpha1.FieldError{problem("spec.channel", `unknown channel "x"; known: stable`)}},
		{"a version and a node group", []string{
			withVersion(t, clusterYAML, "3.0.0"), edit(t, serversYAML, "size: 3", "size: 2"),
		}, []v1alpha1.FieldError{
			problem("spec.nomad.version", "3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0"),
			{Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, root := newFileService(t)
			svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
			_, err := svc.Create(t.Context(), decode(t, tc.docs...), true)
			wantFieldErrors(t, err, tc.want...)
			wantNothingWritten(t, root)
		})
	}
}

// untested is the warning about an untested Nomad version v in the test channel, which tests tested.
func untested(v, tested string) string {
	return "Nomad " + v + " is not tested by this tent; channel stable tests " + tested
}

// narrowClusterYAML is the test cluster with a Nomad API that the whole internet cannot reach, so that a change of it
// warns of nothing else.
const narrowClusterYAML = clusterYAML + "  access:\n    api: [203.0.113.0/24]\n"

// TestWarnsOfAnUntestedNomad tells OnWarning when a change leaves a cluster whose spec sets a Nomad version that the
// channel allows but has not tested.
func TestWarnsOfAnUntestedNomad(t *testing.T) {
	t.Parallel()
	svc := newService(t)
	svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.6", "2.0.7")
	var warnings []string
	svc.OnWarning = func(w string) { warnings = append(warnings, w) }
	wantWarnings := func(step string, want ...string) {
		t.Helper()
		if diff := cmp.Diff(want, warnings); diff != "" {
			t.Errorf("after %s, the warnings (-want +got):\n%s", step, diff)
		}
		warnings = nil
	}

	mustCreate(t, svc, withVersion(t, narrowClusterYAML, "2.0.8"), serversYAML, workersYAML)
	wantWarnings("create", untested("2.0.8", "2.0.6 and 2.0.7"))
	mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 4")) // the stored Cluster sets 2.0.8
	wantWarnings("replace of a node group", untested("2.0.8", "2.0.6 and 2.0.7"))
	if _, err := svc.Replace(t.Context(), decode(t, withVersion(t, narrowClusterYAML, "2.1.0")), false); err != nil {
		t.Fatalf("Replace without apply: %v", err)
	}
	wantWarnings("a check")
	mustReplace(t, svc, withVersion(t, narrowClusterYAML, "2.0.6"))
	wantWarnings("replace with a tested version")
	mustReplace(t, svc, narrowClusterYAML)
	wantWarnings("replace without a version")
	ref := load(t, svc, v1alpha1.KindCluster, "")
	if _, err := svc.Save(t.Context(), ref, decode(t, withVersion(t, narrowClusterYAML, "2.1.0")), true); err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantWarnings("edit", untested("2.1.0", "2.0.6 and 2.0.7"))
}

// TestUpdateWarnsOfAnUntestedNomad tells OnWarning once, before the first change, when an update applies changes to a
// cluster whose Nomad version, set in its spec or pinned, the channel allows but has not tested.
func TestUpdateWarnsOfAnUntestedNomad(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc := newService(t)
		mustCreate(t, svc, narrowClusterYAML, serversYAML, workersYAML)
		withCloud(svc)
		svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
		warning := untested("2.0.7", "2.0.8")
		var events []string
		svc.OnWarning = func(w string) { events = append(events, w) }
		svc.OnProgress = func(p app.Progress) { events = append(events, progressLine(p)) }
		wantWarned := func(step string, want bool) {
			t.Helper()
			n := 0
			for _, e := range events {
				if !strings.HasPrefix(e, "node ") && !strings.HasPrefix(e, "infra ") && !strings.HasPrefix(e, "nomad ") {
					n++
				}
			}
			if want && (n != 1 || events[0] != warning) || !want && n != 0 {
				t.Errorf("after %s, the events are %q; want the warning %t, once, before the first change", step,
					events, want)
			}
			events = nil
		}

		mustUpdate(t, svc) // pins 2.0.7, which the channel tests
		wantWarned("the first update", false)

		svc.Channels = stableChannel("2.0.0", "2.0.8", "2.0.8")
		if _, err := svc.Update(t.Context(), "prod", false); err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		wantWarned("a plan", false)
		mustUpdate(t, svc)
		wantWarned("an update without changes", false)
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))
		mustUpdate(t, svc)
		wantWarned("an update that applies changes", true)
	})
}

// wantPinned fails the test unless the test cluster's stored completed spec holds the Nomad version v.
func wantPinned(t *testing.T, svc *app.Service, v string) {
	t.Helper()
	if got := decode(t, string(get(t, svc.Store, completedPath))).Cluster.Spec.Nomad.Version; got != v {
		t.Errorf("the completed spec holds Nomad %q, want %q", got, v)
	}
}

// completedOnly is the plan of an update that writes the completed spec alone.
const completedOnly = "State: cluster.completed.yaml will be written.\n"

// TestUpdatePinsTheNomadVersion records the channel's recommended Nomad version in the completed spec on the first
// update of a cluster whose spec leaves it out, and keeps it when the channel recommends another one later.
func TestUpdatePinsTheNomadVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
		mustUpdate(t, svc)
		wantPinned(t, svc, "2.0.7")
		wantStored(t, svc.Store, clusterPath, encode(t, keyedClusterYAML))

		svc.Channels = stableChannel("2.0.0", "2.0.8", "2.0.7", "2.0.8")
		wantConverged(t, svc)
		mustUpdate(t, svc)
		wantPinned(t, svc, "2.0.7")
	})
}

// TestUpdateTakesTheUsersVersion records the Nomad version that the spec sets, and keeps it once the spec leaves it out
// again.
func TestUpdateTakesTheUsersVersion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		withNomad(svc, f).NodeVersions()
		svc.Channels = stableChannel("2.0.0", "2.0.6", "2.0.6")
		mustUpdate(t, svc)

		mustReplace(t, svc, withVersion(t, keyedClusterYAML, "2.0.7"))
		if got, want := planText(t, mustUpdate(t, svc)), completedOnly+allOutdatedLine; got != want {
			t.Errorf("the plan is\n%s\nwant the completed spec and the outdated nodes:\n%s", got, want)
		}
		wantPinned(t, svc, "2.0.7")

		svc.Channels = stableChannel("2.0.0", "2.0.9", "2.0.9")
		mustReplace(t, svc, keyedClusterYAML)
		wantPinned(t, svc, "2.0.7")
		wantConverged(t, svc)
		wantPinned(t, svc, "2.0.7")
	})
}

// TestUpdatePinsAnOlderCompletedSpec updates a cluster whose completed spec a tent without channels wrote, without a
// Nomad version: the plan writes the completed spec alone, with the channel's recommended version.
func TestUpdatePinsAnOlderCompletedSpec(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		withNomad(svc, f).NodeVersions()
		svc.Channels = stableChannel("2.0.0", "2.0.6", "2.0.6")
		mustUpdate(t, svc)
		put(t, svc.Store, completedPath, completedSpec(t, svc, ""))
		svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")

		plan, err := svc.Update(t.Context(), "prod", false)
		if err != nil {
			t.Fatalf("Update without apply: %v", err)
		}
		if got, want := planText(t, plan), completedOnly+allOutdatedLine; got != want {
			t.Errorf("the plan is\n%s\nwant the completed spec and the outdated nodes:\n%s", got, want)
		}
		mustUpdate(t, svc)
		wantPinned(t, svc, "2.0.7")
		wantConverged(t, svc)
	})
}

// TestUpdateRefusesAPinOutsideTheChannel fails the plan of a cluster pinned to a Nomad version that its channel no
// longer allows, before it writes anything or reaches the cloud.
func TestUpdateRefusesAPinOutsideTheChannel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		withNomad(svc, f).NodeVersions()
		svc.Channels = stableChannel("2.0.0", "2.0.6", "2.0.6")
		mustUpdate(t, svc)
		svc.Channels = stableChannel("2.0.7", "2.0.7", "2.0.7")
		stored, calls := snapshot(t, svc.Store), len(f.Calls())

		for _, apply := range []bool{false, true} {
			_, err := svc.Update(t.Context(), "prod", apply)
			wantError(t, err, "cluster prod is pinned to Nomad 2.0.6 ("+completedPath+"), which is older than "+
				"2.0.7, the oldest Nomad that channel stable allows; run tent upgrade cluster, or set "+
				"spec.nomad.version to a version that the channel allows")
		}
		wantSnapshot(t, svc.Store, stored)
		if n := len(f.Calls()); n != calls {
			t.Errorf("the update called the cloud: %v", f.Calls()[calls:])
		}

		// A version in the spec wins over the pin.
		mustReplace(t, svc, withVersion(t, keyedClusterYAML, "2.0.7"))
		mustUpdate(t, svc)
		wantPinned(t, svc, "2.0.7")
	})
}

// TestUpdateRefusesToPinAVersionWhoseReleaseFilesCannotBeRead fails the plan of a cluster whose spec sets a Nomad
// version other than the pin when the release files of that version cannot be read, newer or older, with and without
// apply, before it writes to the store or the cloud.
func TestUpdateRefusesToPinAVersionWhoseReleaseFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.0.99", "2.0.5"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f := newUpdate(t)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				mustUpdate(t, svc)
				mustReplace(t, svc, withVersion(t, keyedClusterYAML, version))
				stored, calls := snapshot(t, svc.Store), len(f.Calls())

				for _, apply := range []bool{false, true} {
					plan, err := svc.Update(t.Context(), "prod", apply)
					wantError(t, err, "cluster prod: tent pins Nomad "+version+" only after it has read the release "+
						"files that the nodes download: find Nomad "+version+": get https://releases.hashicorp.com/nomad/"+
						version+"/nomad_"+version+"_SHA256SUMS: 404 Not Found")
					if plan.Applied {
						t.Errorf("Update(apply %t) applied a plan", apply)
					}
				}
				wantSnapshot(t, svc.Store, stored)
				wantNoWrites(t, f.Calls()[calls:])
				wantPinned(t, svc, "2.0.7")
			})
		})
	}
}

// TestUpdateRefusesAFirstPinWhoseReleaseFilesCannotBeRead fails the plan of a built cluster whose store holds no pin,
// a completed spec without a Nomad version or one that does not decode, when the release files of the version that
// the plan would pin cannot be read, with and without apply, before it writes to the store or the cloud.
func TestUpdateRefusesAFirstPinWhoseReleaseFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, svc *app.Service)
	}{
		{"a completed spec without a version", func(t *testing.T, svc *app.Service) {
			put(t, svc.Store, completedPath, completedSpec(t, svc, ""))
			svc.Channels = stableChannel("2.0.0", "2.0.99", "2.0.99")
		}},
		{"a completed spec that does not decode", func(t *testing.T, svc *app.Service) {
			put(t, svc.Store, completedPath, []byte("kind: Cluster\n"))
			mustReplace(t, svc, withVersion(t, keyedClusterYAML, "2.0.99"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				svc, f := newUpdate(t)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				mustUpdate(t, svc)
				tc.prepare(t, svc)
				stored, calls := snapshot(t, svc.Store), len(f.Calls())

				for _, apply := range []bool{false, true} {
					_, err := svc.Update(t.Context(), "prod", apply)
					wantError(t, err, "cluster prod: tent pins Nomad 2.0.99 only after it has read the release files "+
						"that the nodes download: find Nomad 2.0.99: get "+
						"https://releases.hashicorp.com/nomad/2.0.99/nomad_2.0.99_SHA256SUMS: 404 Not Found")
				}
				wantSnapshot(t, svc.Store, stored)
				wantNoWrites(t, f.Calls()[calls:])
			})
		})
	}
}

// TestUpdateNeedsACompletedSpecThatDecodes fails the plan when the pinned Nomad version is needed and the stored
// completed spec, which holds it, does not decode or holds no Cluster, and says how to go on. Nothing is written.
func TestUpdateNeedsACompletedSpecThatDecodes(t *testing.T) {
	t.Parallel()
	const hint = "; set spec.nomad.version to the Nomad version the cluster was built with, or fix " + completedPath +
		" in the state store by hand"
	for _, tc := range []struct {
		name, data, want string
	}{
		{"broken", "kind: Cluster\n", completedPath + ": document 1 (Cluster): apiVersion is required" + hint},
		{"node groups only", string(encode(t, serversYAML)), completedPath + ": holds no Cluster" + hint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, f := newUpdate(t)
			put(t, svc.Store, completedPath, []byte(tc.data))
			stored := snapshot(t, svc.Store)
			for _, apply := range []bool{false, true} {
				_, err := svc.Update(t.Context(), "prod", apply)
				wantError(t, err, tc.want)
			}
			wantSnapshot(t, svc.Store, stored)
			wantOnlyReads(t, f)
		})
	}
}

// TestUpdateRewritesABrokenCompletedSpec updates a built cluster whose stored completed spec does not decode or holds
// no Cluster, when the spec sets the Nomad version: the pin is not needed, and the plan writes the completed spec
// alone.
func TestUpdateRewritesABrokenCompletedSpec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data string }{
		{"broken", "kind: Cluster\n"},
		{"node groups only", string(encode(t, serversYAML))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _ := newUpdate(t)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				mustUpdate(t, svc)
				put(t, svc.Store, completedPath, []byte(tc.data))
				mustReplace(t, svc, withVersion(t, keyedClusterYAML, "2.0.7"))

				if got := planText(t, mustUpdate(t, svc)); got != completedOnly {
					t.Errorf("the plan is\n%s\nwant the completed spec alone", got)
				}
				wantPinned(t, svc, "2.0.7")
				wantConverged(t, svc)
			})
		})
	}
}

// moveDetail is the problem of a Nomad version to that is more than one minor version above the pin from, as the spec
// writers report it.
func moveDetail(to, from, advice string) string {
	return to + " is more than one minor version above " + from + ", the Nomad version that the cluster is pinned to" +
		advice
}

// moveChecks are the use cases that write a Cluster over a stored one: replace and edit.
var moveChecks = specChecks[1:3]

// pinnedService returns a service whose store holds the test cluster and a completed spec that pins Nomad 2.0.7, in
// a channel that allows 2.x from 2.0.0.
func pinnedService(t *testing.T) *app.Service {
	t.Helper()
	svc := newService(t)
	svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	put(t, svc.Store, completedPath, completedSpec(t, svc, "2.0.7"))
	return svc
}

// TestSpecWritersHoldTheVersionToThePin refuses a Cluster whose Nomad version is more than one minor version above
// the pin of the stored completed spec, with or without apply and without a write, and accepts an older version, the
// pin and a version at most one minor version above it.
func TestSpecWritersHoldTheVersionToThePin(t *testing.T) {
	t.Parallel()
	for _, uc := range moveChecks {
		t.Run(uc.name, func(t *testing.T) {
			t.Parallel()
			svc := pinnedService(t)
			stored := snapshot(t, svc.Store)
			for _, apply := range []bool{false, true} {
				wantFieldErrors(t, uc.run(t, svc, withVersion(t, clusterYAML, "2.2.0"), apply),
					problem("spec.nomad.version", moveDetail("2.2.0", "2.0.7", "; move to 2.1 first")))
			}
			wantSnapshot(t, svc.Store, stored)
			for _, v := range []string{"2.0.6", "2.0.7", "2.0.9", "2.1.5"} {
				if err := uc.run(t, svc, withVersion(t, clusterYAML, v), true); err != nil {
					t.Errorf("version %s over the pin 2.0.7: %v", v, err)
				}
			}
		})
	}
}

// TestSpecWritersAdviseOnlyTheSameMajor names no version to move to first when the version is of another major
// version than the pin, which only a pin that the channel no longer allows shows.
func TestSpecWritersAdviseOnlyTheSameMajor(t *testing.T) {
	t.Parallel()
	svc := pinnedService(t)
	put(t, svc.Store, completedPath, completedSpec(t, svc, "1.11.0"))
	_, err := svc.Replace(t.Context(), decode(t, withVersion(t, clusterYAML, "2.0.7")), true)
	wantFieldErrors(t, err, problem("spec.nomad.version",
		"2.0.7 is of another major version than 1.11.0, the Nomad version that the cluster is pinned to"))
}

// TestSpecWritersReportAVersionThatTheChannelRefusesOnce gives the channel's problem alone for a version that the
// channel refuses, though it is also more than one minor version above the pin.
func TestSpecWritersReportAVersionThatTheChannelRefusesOnce(t *testing.T) {
	t.Parallel()
	svc := pinnedService(t)
	_, err := svc.Replace(t.Context(), decode(t, withVersion(t, clusterYAML, "3.0.0")), true)
	wantFieldErrors(t, err, problem("spec.nomad.version",
		"3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0"))
}

// TestSpecWritersCheckNoMoveWithoutAUsablePin accepts a version far above a pin that is not there: the store has no
// completed spec, or the stored one does not decode or holds no Cluster.
func TestSpecWritersCheckNoMoveWithoutAUsablePin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte // the completed spec; nil for none
	}{
		{"no completed spec", nil},
		{"broken", []byte("kind: Cluster\n")},
		{"node groups only", encode(t, serversYAML)},
	} {
		for _, uc := range moveChecks {
			t.Run(tc.name+"/"+uc.name, func(t *testing.T) {
				t.Parallel()
				svc := pinnedService(t)
				if tc.data == nil {
					if err := svc.Store.Delete(t.Context(), completedPath); err != nil {
						t.Fatalf("delete the completed spec: %v", err)
					}
				} else {
					put(t, svc.Store, completedPath, tc.data)
				}
				if err := uc.run(t, svc, withVersion(t, clusterYAML, "2.2.0"), true); err != nil {
					t.Errorf("version 2.2.0 without a pin: %v", err)
				}
			})
		}
	}
}

// TestCreateHoldsTheVersionToALeftoverPin refuses a create whose Nomad version is more than one minor version above
// the pin of a completed spec that the store still holds without its cluster.yaml.
func TestCreateHoldsTheVersionToALeftoverPin(t *testing.T) {
	t.Parallel()
	svc := pinnedService(t)
	if err := svc.Store.Delete(t.Context(), clusterPath); err != nil {
		t.Fatalf("delete cluster.yaml: %v", err)
	}
	stored := snapshot(t, svc.Store)
	for _, apply := range []bool{false, true} {
		_, err := svc.Create(t.Context(), decode(t, withVersion(t, clusterYAML, "2.2.0"), serversYAML, workersYAML), apply)
		wantFieldErrors(t, err, problem("spec.nomad.version", moveDetail("2.2.0", "2.0.7", "; move to 2.1 first")))
	}
	wantSnapshot(t, svc.Store, stored)
}

// TestSpecWritersReadTheCompletedSpecOnlyForACluster reads the completed spec when the objects hold a Cluster, and
// not when they hold node groups alone.
func TestSpecWritersReadTheCompletedSpecOnlyForACluster(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		docs []string
		want bool
	}{
		{"node groups", []string{edit(t, workersYAML, "size: 2", "size: 4")}, false},
		{"a Cluster", []string{clusterYAML}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := pinnedService(t)
			log := &reads{Store: svc.Store}
			svc.Store = log
			if _, err := svc.Replace(t.Context(), decode(t, tc.docs...), false); err != nil {
				t.Fatalf("Replace: %v", err)
			}
			if got := slices.Contains(log.paths, completedPath); got != tc.want {
				t.Errorf("the completed spec was read: %t, want %t; reads: %v", got, tc.want, log.paths)
			}
		})
	}
}

// TestCommandsThatLoadTheSpecsRefuseAVersionTooFarAboveThePin fails update, rolling-update and validate with the
// load's text when the stored Cluster, put past the writers, sets a version more than one minor version above the
// pin, before they reach a provider or write anything.
func TestCommandsThatLoadTheSpecsRefuseAVersionTooFarAboveThePin(t *testing.T) {
	t.Parallel()
	const want = "cluster prod: spec.nomad.version: 2.2.0 is more than one minor version above 2.0.7, the Nomad " +
		"version that the cluster is pinned to (" + completedPath + "); move to 2.1 first"
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, svc *app.Service) error
	}{
		{"update", func(t *testing.T, svc *app.Service) error {
			_, err := svc.Update(t.Context(), "prod", true)
			return err
		}},
		{"rolling-update", func(t *testing.T, svc *app.Service) error {
			_, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true})
			return err
		}},
		{"validate", func(t *testing.T, svc *app.Service) error {
			_, err := svc.ValidateCluster(t.Context(), "prod")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := pinnedService(t)
			svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
				t.Error("the command looked up a provider")
				return nil, errors.New("no provider")
			}
			put(t, svc.Store, clusterPath, encode(t, withVersion(t, clusterYAML, "2.2.0")))
			stored := snapshot(t, svc.Store)
			wantError(t, tc.run(t, svc), want)
			wantSnapshot(t, svc.Store, stored)
		})
	}
}

// TestCommandsThatLoadTheSpecsLoadAVersionOlderThanThePin loads the specs for update, validate and rolling-update when
// the spec sets an older Nomad version than the pin: a version in the spec wins over the pin. The plan of update then
// holds it against the 2.0.7 that the servers report.
func TestCommandsThatLoadTheSpecsLoadAVersionOlderThanThePin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.6", "2.0.7")
		mustUpdate(t, svc)
		mustReplace(t, svc, withVersion(t, keyedClusterYAML, "2.0.6"))
		_, err := svc.Update(t.Context(), "prod", false)
		wantError(t, err, lowering("server prod-servers-0.global"))
		if _, err := svc.ValidateCluster(t.Context(), "prod"); err != nil {
			t.Errorf("ValidateCluster over a pin of 2.0.7 with 2.0.6 in the spec: %v", err)
		}
		_, err = svc.RollingUpdate(t.Context(), "prod", app.RollOptions{})
		wantError(t, err, "the specs of cluster prod changed since the last tent update cluster; run it first")
	})
}

// TestCommandsThatLoadTheSpecsPointToUpgradeForAVersionBelowTheMinimum adds "run tent upgrade cluster" to the channel's
// problem with the Nomad version that the stored Cluster sets, when the version is older than the channel's minimum,
// and keeps the other problems of the specs after it. A version that the channel refuses for another reason gets no
// advice.
func TestCommandsThatLoadTheSpecsPointToUpgradeForAVersionBelowTheMinimum(t *testing.T) {
	t.Parallel()
	smallServers := edit(t, serversYAML, "size: 3", "size: 2")
	groupProblem := v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "must be 1, 3 or 5 for role=server",
	}
	for _, tc := range []struct {
		name    string
		cluster string
		servers string
		want    []v1alpha1.FieldError
	}{
		{"older than the minimum", withVersion(t, clusterYAML, "1.11.0"), serversYAML,
			[]v1alpha1.FieldError{problem("spec.nomad.version",
				"1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows; run tent upgrade cluster")}},
		{
			"older than the minimum, and other problems",
			edit(t, withVersion(t, clusterYAML, "1.11.0"), "region: ams", "region: ams\n    zones: [fra]"),
			smallServers,
			[]v1alpha1.FieldError{
				problem("spec.nomad.version",
					"1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows; run tent upgrade cluster"),
				problem("spec.cloud.zones", "must be [ams]: Vultr has no zones"),
				groupProblem,
			},
		},
		{"of the next major", withVersion(t, clusterYAML, "3.0.0"), serversYAML,
			[]v1alpha1.FieldError{problem("spec.nomad.version",
				"3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0")}},
		{"not a version", withVersion(t, clusterYAML, "2.0"), serversYAML, []v1alpha1.FieldError{problem(
			"spec.nomad.version", `"2.0" is not a Nomad version such as 2.0.7`)}},
		{"a pre-release older than the minimum", withVersion(t, clusterYAML, "1.11.0-rc.1"), serversYAML,
			[]v1alpha1.FieldError{problem("spec.nomad.version", `"1.11.0-rc.1" is not a Nomad version such as 2.0.7`)}},
	} {
		for _, uc := range []struct {
			name string
			run  func(t *testing.T, svc *app.Service) error
		}{
			{"update", func(t *testing.T, svc *app.Service) error {
				_, err := svc.Update(t.Context(), "prod", true)
				return err
			}},
			{"rolling-update", func(t *testing.T, svc *app.Service) error {
				_, err := svc.RollingUpdate(t.Context(), "prod", app.RollOptions{Apply: true})
				return err
			}},
			{"validate", func(t *testing.T, svc *app.Service) error {
				_, err := svc.ValidateCluster(t.Context(), "prod")
				return err
			}},
		} {
			t.Run(tc.name+"/"+uc.name, func(t *testing.T) {
				t.Parallel()
				svc := newService(t)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
				put(t, svc.Store, clusterPath, encode(t, tc.cluster))
				put(t, svc.Store, serversPath, encode(t, tc.servers))
				wantFieldErrors(t, uc.run(t, svc), tc.want...)
			})
		}
	}
}

// TestWritesWithoutAClusterPointToUpgradeForAStoredVersionBelowTheMinimum adds "run tent upgrade cluster" to the
// channel's problem when a replace or an edit of a node group alone fails over the stored Cluster's version, which is
// older than the channel's minimum. A write whose input holds such a Cluster gets the problem without the advice.
func TestWritesWithoutAClusterPointToUpgradeForAStoredVersionBelowTheMinimum(t *testing.T) {
	t.Parallel()
	const detail = "1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows"
	old := withVersion(t, clusterYAML, "1.11.0")
	for _, apply := range []bool{false, true} {
		for _, uc := range []struct {
			name string
			run  func(t *testing.T, svc *app.Service, apply bool) error
			want string
		}{
			{"replace", func(t *testing.T, svc *app.Service, apply bool) error {
				_, err := svc.Replace(t.Context(), decode(t, workersYAML), apply)
				return err
			}, detail + "; run tent upgrade cluster"},
			{"edit", func(t *testing.T, svc *app.Service, apply bool) error {
				ref := load(t, svc, v1alpha1.KindNodeGroup, "workers")
				_, err := svc.Save(t.Context(), ref, decode(t, workersYAML), apply)
				return err
			}, detail + "; run tent upgrade cluster"},
			{"replace with the Cluster", func(t *testing.T, svc *app.Service, apply bool) error {
				_, err := svc.Replace(t.Context(), decode(t, old), apply)
				return err
			}, detail},
		} {
			t.Run(fmt.Sprintf("%s/apply=%t", uc.name, apply), func(t *testing.T) {
				t.Parallel()
				svc := newService(t)
				svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
				mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
				put(t, svc.Store, clusterPath, encode(t, old))
				stored := snapshot(t, svc.Store)
				wantFieldErrors(t, uc.run(t, svc, apply), problem("spec.nomad.version", uc.want))
				wantSnapshot(t, svc.Store, stored)
			})
		}
	}
}
