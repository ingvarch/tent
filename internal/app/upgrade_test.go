package app_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/assets/assetstest"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

const (
	nomad207Sums  = "https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_SHA256SUMS"
	prodCompleted = "prod/cluster.completed.yaml"
)

// upgradeService returns a service over a store that holds the cluster docs, made by tent v0.4.0 and now run by tent
// v0.5.0. Its channel stable allows Nomad from 2.0.0, recommends 2.0.7 and tests it. It serves the release files of
// assetstest, whose requests the second result records, and fails the test when it reaches a cloud or a Nomad.
func upgradeService(t *testing.T, docs ...string) (*app.Service, *assetstest.Sites) {
	t.Helper()
	svc, _ := newService(t)
	svc.Version = "v0.4.0"
	mustCreate(t, svc, docs...)
	svc.Version = "v0.5.0"
	svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
	sites := withAssets(svc)
	svc.Providers = func(v1alpha1.Provider) (cloud.Provider, error) {
		t.Error("the upgrade looked up a provider")
		return nil, errors.New("no provider")
	}
	svc.Nomad = func(nomadops.Config) (nomadops.API, error) {
		t.Error("the upgrade made a Nomad client")
		return nil, errors.New("no Nomad")
	}
	return svc, sites
}

// pinTo stores a completed spec of the test cluster that pins Nomad version v.
func pinTo(t *testing.T, svc *app.Service, v string) {
	t.Helper()
	objs, err := svc.Get(t.Context(), "prod", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	objs.Cluster.Spec.Nomad.Version = v
	data, err := spec.Encode(objs)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	put(t, svc.Store, completedPath, data)
}

// upgradeCluster returns the keyed test cluster's Cluster with the Nomad version v, or without one when v is empty.
func upgradeCluster(t *testing.T, v string) string {
	t.Helper()
	if v == "" {
		return keyedClusterYAML
	}
	return withVersion(t, keyedClusterYAML, v)
}

// upgradeAt returns a service over the test cluster whose spec sets the Nomad version specVersion and whose completed
// spec pins pin; either may be empty.
func upgradeAt(t *testing.T, specVersion, pin string) (*app.Service, *assetstest.Sites) {
	t.Helper()
	svc, sites := upgradeService(t, upgradeCluster(t, specVersion), serversYAML, workersYAML)
	if pin != "" {
		pinTo(t, svc, pin)
	}
	return svc, sites
}

// withoutClusterOrVersion returns the objects of a snapshot except the Cluster and the tent version.
func withoutClusterOrVersion(objs map[string]string) map[string]string {
	rest := maps.Clone(objs)
	delete(rest, clusterPath)
	delete(rest, versionPath)
	return rest
}

// nomad207Files are the two files that the release check of Nomad 2.0.7 requests.
var nomad207Files = []string{nomad207Sums, nomad207Sums + ".sig"}

func TestUpgradeWritesTheRecommendedVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, specVersion, pin string }{
		{"the version is only pinned", "", "2.0.6"},
		{"the spec sets the version and the pin agrees", "2.0.6", "2.0.6"},
		{"the spec sets a version that nothing has pinned", "2.0.6", ""},
		{"the spec took the version back", "2.0.6", "2.0.7"},
	} {
		want := app.UpgradePlan{
			Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: tc.specVersion, Pinned: tc.pin,
			Current: "2.0.6", Target: "2.0.7",
		}
		t.Run(tc.name+"/plan", func(t *testing.T) {
			t.Parallel()
			svc, sites := upgradeAt(t, tc.specVersion, tc.pin)
			stored := snapshot(t, svc.Store)
			plan, err := svc.UpgradeCluster(t.Context(), "prod", false)
			if err != nil {
				t.Fatalf("UpgradeCluster: %v", err)
			}
			if diff := cmp.Diff(want, plan); diff != "" {
				t.Errorf("plan (-want +got):\n%s", diff)
			}
			wantSnapshot(t, svc.Store, stored)
			if diff := cmp.Diff(nomad207Files, sites.URLs()); diff != "" {
				t.Errorf("release files asked for (-want +got):\n%s", diff)
			}
		})
		t.Run(tc.name+"/apply", func(t *testing.T) {
			t.Parallel()
			svc, sites := upgradeAt(t, tc.specVersion, tc.pin)
			stored := snapshot(t, svc.Store)
			plan, err := svc.UpgradeCluster(t.Context(), "prod", true)
			if err != nil {
				t.Fatalf("UpgradeCluster: %v", err)
			}
			applied := want
			applied.Applied = true
			if diff := cmp.Diff(applied, plan); diff != "" {
				t.Errorf("plan (-want +got):\n%s", diff)
			}
			wantStored(t, svc.Store, clusterPath, encode(t, upgradeCluster(t, "2.0.7")))
			wantStored(t, svc.Store, versionPath, []byte("v0.5.0\n"))
			before, after := withoutClusterOrVersion(stored), withoutClusterOrVersion(snapshot(t, svc.Store))
			if diff := cmp.Diff(before, after); diff != "" {
				t.Errorf("the other objects changed (-before +after):\n%s", diff)
			}
			// The two plans of the run share one check of the release.
			if diff := cmp.Diff(nomad207Files, sites.URLs()); diff != "" {
				t.Errorf("release files asked for (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpgradeHasNothingToWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, specVersion, pin, current string }{
		{"the version is the recommended one", "", "2.0.7", "2.0.7"},
		{"the version is newer than the recommended one", "2.0.8", "2.0.7", "2.0.8"},
		{"the cluster has no version yet", "", "", ""},
		{"the spec sets the recommended version and the pin agrees", "2.0.7", "2.0.7", "2.0.7"},
		{"the spec sets the recommended version and the pin is behind", "2.0.7", "2.0.6", "2.0.7"},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(tc.name+"/apply "+strconv.FormatBool(apply), func(t *testing.T) {
				t.Parallel()
				svc, sites := upgradeAt(t, tc.specVersion, tc.pin)
				stored := snapshot(t, svc.Store)
				held := holdLock(t, svc.Store) // a command with nothing to write takes no lock
				plan, err := svc.UpgradeCluster(t.Context(), "prod", apply)
				if err != nil {
					t.Fatalf("UpgradeCluster: %v", err)
				}
				want := app.UpgradePlan{
					Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: tc.specVersion, Pinned: tc.pin,
					Current: tc.current, Applied: apply,
				}
				if diff := cmp.Diff(want, plan); diff != "" {
					t.Errorf("plan (-want +got):\n%s", diff)
				}
				if plan.Due() {
					t.Error("the plan has a version to write")
				}
				if urls := sites.URLs(); len(urls) != 0 {
					t.Errorf("the release files were asked for: %v", urls)
				}
				wantSnapshot(t, svc.Store, stored)
				release(t, held)
			})
		}
	}
}

// TestUpgradeMovesAVersionThatTheChannelNoLongerAllows upgrades a cluster whose Nomad version, pinned or set, is older
// than the oldest that the channel allows now, where update and replace refuse it.
func TestUpgradeMovesAVersionThatTheChannelNoLongerAllows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, specVersion, pin string
		refused                func(t *testing.T, svc *app.Service)
	}{
		{"pinned", "", "2.0.6", func(t *testing.T, svc *app.Service) {
			_, err := svc.Update(t.Context(), "prod", false)
			wantError(t, err, "cluster prod is pinned to Nomad 2.0.6 ("+prodCompleted+"), which is older than 2.0.7, "+
				"the oldest Nomad that channel stable allows; run tent upgrade cluster, or set spec.nomad.version to a "+
				"version that the channel allows")
		}},
		{"set in the spec", "2.0.6", "", func(t *testing.T, svc *app.Service) {
			_, err := svc.Replace(t.Context(), decode(t, upgradeCluster(t, "2.0.6")), false)
			wantFieldErrors(t, err, problem("spec.nomad.version",
				"2.0.6 is older than 2.0.7, the oldest Nomad that channel stable allows"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := upgradeAt(t, tc.specVersion, tc.pin)
			svc.Channels = stableChannel("2.0.7", "2.0.7", "2.0.7")
			tc.refused(t, svc)
			plan, err := svc.UpgradeCluster(t.Context(), "prod", true)
			if err != nil {
				t.Fatalf("UpgradeCluster: %v", err)
			}
			if plan.Current != "2.0.6" || plan.Target != "2.0.7" || !plan.Applied {
				t.Errorf("plan = %+v, want an applied upgrade from 2.0.6 to 2.0.7", plan)
			}
			wantStored(t, svc.Store, clusterPath, encode(t, upgradeCluster(t, "2.0.7")))
		})
	}
}

// flippingSums serves the release files of assetstest, with one byte of every checksum file changed.
type flippingSums struct{ next http.RoundTripper }

func (f flippingSums) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := f.next.RoundTrip(r)
	if err != nil || !strings.HasSuffix(r.URL.Path, "_SHA256SUMS") {
		return resp, err
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	data := body.Bytes()
	data[0] ^= 1
	resp.Body = readCloser{bytes.NewReader(data)}
	return resp, nil
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

// olderSums serves the signed checksums of the older Nomad release in place of those of the newer one.
type olderSums struct{ next http.RoundTripper }

func (o olderSums) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Path = strings.ReplaceAll(r.URL.Path, assetstest.NomadVersion, assetstest.OlderNomadVersion)
	return o.next.RoundTrip(r)
}

func TestUpgradeRefuses(t *testing.T) {
	t.Parallel()
	const (
		editAdvice = "set spec.nomad.version to a 2.1 release with tent edit cluster, run tent update cluster and " +
			"tent rolling-update cluster, and upgrade again"
		moved = "cluster prod is pinned to Nomad 2.0.7, and channel stable recommends 2.2.3: 2.2.3 is more than one " +
			"minor version above 2.0.7; "
		notVerified = "tent could not verify the release of Nomad "
	)
	missing := func(v string) string {
		return ": get https://releases.hashicorp.com/nomad/" + v + "/nomad_" + v +
			"_SHA256SUMS: 404 Not Found"
	}
	// updateSays is the error of update for the same store.
	updateSays := func(t *testing.T, svc *app.Service) string {
		t.Helper()
		_, err := svc.Update(t.Context(), "prod", false)
		if err == nil {
			t.Fatal("Update succeeded, want the error that the upgrade must give too")
		}
		return err.Error()
	}
	for _, tc := range []struct {
		name        string
		specVersion string
		pin         string
		doc         string // the Cluster, in place of the keyed one with specVersion
		channel     func(string) (*channels.Channel, error)
		prepare     func(t *testing.T, svc *app.Service)
		want        func(t *testing.T, svc *app.Service) string // the error's text
		startsWith  bool                                        // the error starts with want and has a cause
		fields      []v1alpha1.FieldError                       // the error, in place of want
		asked       bool                                        // the release files are asked for
	}{
		{name: "a release more than one minor version above the pin, with a 2.1 release allowed", pin: "2.0.7",
			channel: stableChannel("2.0.0", "2.2.3", "2.2.3"), want: text(moved + editAdvice)},
		{name: "a release more than one minor version above the pin, with 2.1.0 the oldest allowed", pin: "2.0.7",
			channel: stableChannel("2.1.0", "2.2.3", "2.2.3"), want: text(moved + editAdvice)},
		{name: "a release more than one minor version above the pin, with no 2.1 release allowed", pin: "2.0.7",
			channel: stableChannel("2.2.0", "2.2.3", "2.2.3"), want: text(moved +
				"channel stable of this tent allows no 2.1 release: move the cluster to 2.1 with an older tent first")},
		{name: "a release of another major version than the pin", pin: "2.0.7",
			channel: stableChannel("2.0.0", "3.0.0", "3.0.0"), want: text("cluster prod is pinned to Nomad 2.0.7, " +
				"and channel stable recommends 3.0.0: 3.0.0 is of another major version than 2.0.7")},
		{name: "a release that does not exist", pin: "2.0.7", channel: stableChannel("2.0.0", "2.0.99", "2.0.99"),
			want: text(notVerified + "2.0.99, which channel stable recommends" + missing("2.0.99")), asked: true},
		{name: "a release with no pin to move from", specVersion: "2.0.6",
			channel: stableChannel("2.0.0", "2.2.0", "2.2.0"),
			want:    text(notVerified + "2.2.0, which channel stable recommends" + missing("2.2.0")), asked: true},
		{name: "checksums that do not verify", pin: "2.0.6", prepare: func(_ *testing.T, svc *app.Service) {
			svc.Assets.Client = &http.Client{Transport: flippingSums{svc.Assets.Client.Transport}}
		}, want: text(notVerified + "2.0.7, which channel stable recommends: "), startsWith: true, asked: true},
		{name: "the signed checksums of another release", pin: "2.0.6", prepare: func(_ *testing.T, svc *app.Service) {
			svc.Assets.Client = &http.Client{Transport: olderSums{svc.Assets.Client.Transport}}
		}, want: text(notVerified + "2.0.7, which channel stable recommends: "), startsWith: true, asked: true},
		{name: "a stored version more than one minor version above the pin", specVersion: "2.2.0", pin: "2.0.7",
			want: text("cluster prod: spec.nomad.version: 2.2.0 is more than one minor version above 2.0.7, the " +
				"Nomad version that the cluster is pinned to (" + prodCompleted + "); move to 2.1 first")},
		{name: "an unknown channel", doc: edit(t, keyedClusterYAML, "region: ams", "region: ams\n  channel: x"),
			fields: []v1alpha1.FieldError{problem("spec.channel", `unknown channel "x"; known: stable`)}},
		{name: "a version that the channel refuses for another reason", specVersion: "3.0.0",
			fields: []v1alpha1.FieldError{problem("spec.nomad.version",
				"3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0")}},
		{name: "a completed spec that does not decode", prepare: func(t *testing.T, svc *app.Service) {
			put(t, svc.Store, completedPath, []byte("kind: Cluster\n"))
		}, want: updateSays},
		{name: "a pin that the channel refuses and an upgrade does not mend", pin: "3.0.0", want: updateSays},
		{name: "a store that needs a newer tent", prepare: func(t *testing.T, svc *app.Service) {
			put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
		}, want: text("cluster prod needs tent v0.9.0 or newer; this is v0.5.0")},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(tc.name+"/apply "+strconv.FormatBool(apply), func(t *testing.T) {
				t.Parallel()
				doc := tc.doc
				if doc == "" {
					doc = upgradeCluster(t, tc.specVersion)
				}
				svc, sites := upgradeService(t, keyedClusterYAML, serversYAML, workersYAML)
				put(t, svc.Store, clusterPath, encode(t, doc)) // past the checks of the writers
				if tc.pin != "" {
					pinTo(t, svc, tc.pin)
				}
				if tc.channel != nil {
					svc.Channels = tc.channel
				}
				if tc.prepare != nil {
					tc.prepare(t, svc)
				}
				stored := snapshot(t, svc.Store)
				held := holdLock(t, svc.Store) // a command that fails in its plan takes no lock
				plan, err := svc.UpgradeCluster(t.Context(), "prod", apply)
				switch {
				case tc.fields != nil:
					wantFieldErrors(t, err, tc.fields...)
				case tc.startsWith:
					if want := tc.want(t, svc); err == nil || !strings.HasPrefix(err.Error(), want) ||
						err.Error() == want {
						t.Errorf("error = %v, want %q and a cause", err, want)
					}
				default:
					wantError(t, err, tc.want(t, svc))
				}
				if plan != (app.UpgradePlan{}) {
					t.Errorf("the failed command returned the plan %+v", plan)
				}
				wantSnapshot(t, svc.Store, stored)
				if asked := len(sites.URLs()) > 0; asked != tc.asked {
					t.Errorf("the release files were asked for: %t, want %t (%v)", asked, tc.asked, sites.URLs())
				}
				release(t, held)
			})
		}
	}
}

// text returns a function that gives s, for a table whose other rows compute the error.
func text(s string) func(*testing.T, *app.Service) string {
	return func(*testing.T, *app.Service) string { return s }
}

// TestUpgradeRefusesAMissingClusterAndABadName fails for a cluster that the store lacks and for a name outside the
// name rule.
func TestUpgradeRefusesAMissingClusterAndABadName(t *testing.T) {
	t.Parallel()
	svc, _ := upgradeService(t, clusterYAML, serversYAML, workersYAML)
	for _, apply := range []bool{false, true} {
		_, err := svc.UpgradeCluster(t.Context(), "other", apply)
		wantError(t, err, notFound(svc, "cluster other"))
		_, err = svc.UpgradeCluster(t.Context(), "PROD", apply)
		wantError(t, err, v1alpha1.ValidateName(v1alpha1.KindCluster, "PROD").Error())
	}
}

// TestUpgradeIgnoresACompletedSpecThatDoesNotDecodeWhenTheSpecSetsTheVersion plans from the spec's version when the
// pin cannot be read.
func TestUpgradeIgnoresACompletedSpecThatDoesNotDecodeWhenTheSpecSetsTheVersion(t *testing.T) {
	t.Parallel()
	svc, _ := upgradeAt(t, "2.0.6", "")
	put(t, svc.Store, completedPath, []byte("kind: Cluster\n"))
	plan, err := svc.UpgradeCluster(t.Context(), "prod", false)
	if err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	want := app.UpgradePlan{
		Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.6", Current: "2.0.6", Target: "2.0.7",
	}
	if diff := cmp.Diff(want, plan); diff != "" {
		t.Errorf("plan (-want +got):\n%s", diff)
	}
}

// TestUpgradeNeedsTheFlagForAServerGroupOfOne validates the cluster that results as a replace does.
func TestUpgradeNeedsTheFlagForAServerGroupOfOne(t *testing.T) {
	t.Parallel()
	one := edit(t, serversYAML, "size: 3", "size: 1")
	svc, _ := newService(t)
	svc.Validate.AllowSingleServer = true
	svc.Version = "v0.4.0"
	mustCreate(t, svc, upgradeCluster(t, "2.0.6"), one, workersYAML)
	svc.Version = "v0.5.0"
	svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
	withAssets(svc)

	svc.Validate.AllowSingleServer = false
	_, err := svc.UpgradeCluster(t.Context(), "prod", true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "size 1 needs --allow-single-server",
	})
	wantStored(t, svc.Store, clusterPath, encode(t, upgradeCluster(t, "2.0.6")))

	svc.Validate.AllowSingleServer = true
	if _, err := svc.UpgradeCluster(t.Context(), "prod", true); err != nil {
		t.Fatalf("UpgradeCluster with the flag: %v", err)
	}
	wantStored(t, svc.Store, clusterPath, encode(t, upgradeCluster(t, "2.0.7")))

	// With nothing to upgrade the specs are checked all the same.
	svc.Validate.AllowSingleServer = false
	_, err = svc.UpgradeCluster(t.Context(), "prod", true)
	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "size 1 needs --allow-single-server",
	})
}

// TestUpgradeWarnsAfterAWrite tells OnWarning the warnings about the cluster that results once, after a write and not
// for a plan or a run with nothing to write.
func TestUpgradeWarnsAfterAWrite(t *testing.T) {
	t.Parallel()
	// The keyed cluster leaves access.api open.
	svc, _ := upgradeAt(t, "2.0.6", "")
	var warnings []string
	svc.OnWarning = func(w string) { warnings = append(warnings, w) }
	if _, err := svc.UpgradeCluster(t.Context(), "prod", false); err != nil {
		t.Fatalf("UpgradeCluster without apply: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a plan told %q", warnings)
	}
	if _, err := svc.UpgradeCluster(t.Context(), "prod", true); err != nil {
		t.Fatalf("UpgradeCluster: %v", err)
	}
	if diff := cmp.Diff([]string{openAPIWarning}, warnings); diff != "" {
		t.Errorf("warnings after the write (-want +got):\n%s", diff)
	}
	warnings = nil
	if _, err := svc.UpgradeCluster(t.Context(), "prod", true); err != nil {
		t.Fatalf("UpgradeCluster with nothing to upgrade: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a run with nothing to write told %q", warnings)
	}
}

// readCounter counts the Get and List calls of a store by path.
type readCounter struct {
	statestore.Store
	mu    sync.Mutex
	calls map[string]int
}

func (r *readCounter) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	r.count("get " + p)
	return r.Store.Get(ctx, p)
}

func (r *readCounter) List(ctx context.Context, prefix string) ([]string, error) {
	r.count("list " + prefix)
	return r.Store.List(ctx, prefix)
}

func (r *readCounter) count(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[call]++
}

// TestUpgradeReadsEachObjectOncePerPlan reads the entries of the cluster and its completed spec once for each plan: one
// plan without apply, two with it.
func TestUpgradeReadsEachObjectOncePerPlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		apply bool
		plans int
	}{{false, 1}, {true, 2}} {
		t.Run("apply "+strconv.FormatBool(tc.apply), func(t *testing.T) {
			t.Parallel()
			svc, _ := upgradeAt(t, "", "2.0.6")
			r := &readCounter{Store: svc.Store, calls: map[string]int{}}
			svc.Store = r
			if _, err := svc.UpgradeCluster(t.Context(), "prod", tc.apply); err != nil {
				t.Fatalf("UpgradeCluster: %v", err)
			}
			want := map[string]int{}
			for _, call := range []string{
				"list prod/nodegroups/", "get " + clusterPath, "get " + serversPath, "get " + workersPath,
				"get " + completedPath,
			} {
				want[call] = tc.plans
			}
			got := map[string]int{}
			for call := range want {
				got[call] = r.calls[call]
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("reads (-want +got):\n%s", diff)
			}
		})
	}
}
