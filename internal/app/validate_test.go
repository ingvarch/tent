package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// oneZoneWarning is the warning about the test cluster, whose machines all run in one region.
const oneZoneWarning = "cluster prod runs in one failure domain, ams: an outage there takes the whole cluster down"

// builtWorld returns a service over the test cluster, built by an update, the Vultr fake and the Nomad that it ran on
// and the store's root directory.
func builtWorld(t *testing.T) (*app.Service, *vultrfake.Fake, *nomadWorld, string) {
	t.Helper()
	svc, root := newService(t)
	mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
	f := vultrfake.New()
	w := withNomad(svc, f)
	withAPI(svc, f)
	mustUpdate(t, svc)
	return svc, f, w, root
}

// builtCluster is builtWorld without the Nomad.
func builtCluster(t *testing.T) (*app.Service, *vultrfake.Fake, string) {
	t.Helper()
	svc, f, _, root := builtWorld(t)
	return svc, f, root
}

// mustValidate validates the test cluster and stops the test on an error.
func mustValidate(t *testing.T, svc *app.Service) app.Validation {
	t.Helper()
	v, err := svc.ValidateCluster(t.Context(), "prod")
	if err != nil {
		t.Fatalf("ValidateCluster: %v", err)
	}
	return v
}

// wantFailures fails the test unless the validation holds exactly these failures.
func wantFailures(t *testing.T, v app.Validation, want ...app.Failure) {
	t.Helper()
	if diff := cmp.Diff(want, v.Failures); diff != "" {
		t.Errorf("failures (-want +got):\n%s", diff)
	}
	if v.Valid() != (len(want) == 0) {
		t.Errorf("Valid() = %t with %d failures", v.Valid(), len(want))
	}
}

// TestValidateClusterPassesTheChecks validates the cluster that an update built: it is valid, and tells the counts,
// the pinned Nomad version and the warnings about the specs and the zone.
func TestValidateClusterPassesTheChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		var told []string
		svc.OnWarning = func(w string) { told = append(told, w) }
		stable, err := channels.Load("stable")
		if err != nil {
			t.Fatal(err)
		}

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Cluster != "prod" || v.Servers != 3 || v.Clients != 2 || v.NomadVersion != stable.Nomad.Recommended {
			t.Errorf("cluster %q, %d servers, %d clients, Nomad %q; want prod, 3, 2, %q", v.Cluster, v.Servers, v.Clients,
				v.NomadVersion, stable.Nomad.Recommended)
		}
		if v.Lock != nil {
			t.Errorf("Lock = %v, want nil", v.Lock)
		}
		want := []string{openAPIWarning, oneZoneWarning}
		if diff := cmp.Diff(want, v.Warnings); diff != "" {
			t.Errorf("warnings (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(want, told); diff != "" {
			t.Errorf("OnWarning was told (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterTellsThePinnedNomadVersion tells the Nomad version of the completed spec when the channel
// recommends a newer one.
func TestValidateClusterTellsThePinnedNomadVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		withNomad(svc, f).SetVersion("2.0.7")
		svc.Channels = stableChannel("2.0.0", "2.0.7", "2.0.7")
		mustUpdate(t, svc)
		svc.Channels = stableChannel("2.0.0", "2.0.8", "2.0.7", "2.0.8")

		if v := mustValidate(t, svc); v.NomadVersion != "2.0.7" {
			t.Errorf("NomadVersion = %q, want 2.0.7", v.NomadVersion)
		}
	})
}

// TestValidateClusterWarnsAboutACombinedGroup tells the warning about a combined group after the one about the
// open API, and counts a combined node as a server and as a client.
func TestValidateClusterWarnsAboutACombinedGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
		mustCreate(t, svc, clusterYAML, combinedYAML)
		withCloud(svc)
		mustUpdate(t, svc)

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Servers != 3 || v.Clients != 3 {
			t.Errorf("%d servers and %d clients, want 3 and 3", v.Servers, v.Clients)
		}
		want := []string{openAPIWarning, combinedWarning, oneZoneWarning}
		if diff := cmp.Diff(want, v.Warnings); diff != "" {
			t.Errorf("warnings (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterFindsMachinesThatChanged fails the checks of the machines that a halt and a delete leave. The
// halted machine's node still reads ready in Nomad, as it does until Nomad notices.
func TestValidateClusterFindsMachinesThatChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := builtCluster(t)

		if err := f.HaltInstance(t.Context(), "instance-4"); err != nil {
			t.Fatal(err)
		}
		wantFailures(t, mustValidate(t, svc), app.Failure{
			Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-4",
			Detail: "the cloud reports its machine (ID instance-4) as not running",
		})

		if err := f.DeleteInstance(t.Context(), "instance-5"); err != nil {
			t.Fatal(err)
		}
		wantFailures(t, mustValidate(t, svc),
			app.Failure{
				Check: "machine-missing", Node: "prod-workers-1",
				Detail: "no machine: node group workers has 1 of its 2",
			},
			app.Failure{
				Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-4",
				Detail: "the cloud reports its machine (ID instance-4) as not running",
			},
		)
	})
}

// TestValidateClusterShowsTheLock tells who holds the cluster's lock, and reports no failure for it.
func TestValidateClusterShowsTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		held := holdLock(t, svc.Store)

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Lock == nil || v.Lock.Operation != "update" || v.Lock.ID != held.Lease().ID {
			t.Errorf("Lock = %+v, want the lease of the held lock %+v", v.Lock, held.Lease())
		}
		release(t, held)
		if v := mustValidate(t, svc); v.Lock != nil {
			t.Errorf("Lock = %v after the release, want nil", v.Lock)
		}
	})
}

// TestValidateClusterNamesNoHolder shows a lock whose holder wrote no lease as one of an unknown holder.
func TestValidateClusterNamesNoHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, root := builtCluster(t)
		held := holdLock(t, svc.Store)
		if err := os.Remove(flockLease(root)); err != nil {
			t.Fatal(err)
		}

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Lock == nil || v.Lock.String() != "an unknown holder" {
			t.Errorf("Lock = %v, want an unknown holder", v.Lock)
		}
		release(t, held)
	})
}

// TestValidateClusterReportsALeaseItCannotRead warns about a lock whose lease cannot be read, and fails nothing.
func TestValidateClusterReportsALeaseItCannotRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		svc.Store = unwrapped{svc.Store}
		put(t, svc.Store, lockPath, []byte("not a lease"))
		var told []string
		svc.OnWarning = func(w string) { told = append(told, w) }

		v := mustValidate(t, svc)

		wantFailures(t, v)
		const prefix = "tent could not read the lock of cluster prod: "
		if len(v.Warnings) != 3 || !strings.HasPrefix(v.Warnings[2], prefix) {
			t.Fatalf("warnings = %q, want the two of the cluster and one that starts with %q", v.Warnings, prefix)
		}
		if diff := cmp.Diff(v.Warnings, told); diff != "" {
			t.Errorf("OnWarning was told (-warnings +told):\n%s", diff)
		}
		if v.Lock != nil {
			t.Errorf("Lock = %v, want nil", v.Lock)
		}
	})
}

// TestValidateClusterOnlyReads leaves the store as it was, takes no lock and calls the cloud only to read.
func TestValidateClusterOnlyReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := builtWorld(t)
		before, calls := snapshot(t, svc.Store), len(f.Calls())
		nomadBefore := len(w.Log())

		mustValidate(t, svc)

		wantSnapshot(t, svc.Store, before)
		wantLockFree(t, svc.Store)
		wantNoWrites(t, f.Calls()[calls:])
		var got []string
		for _, c := range f.Calls()[calls:] {
			got = append(got, c.Name)
		}
		// One list of the machines, and one look at the network of each of the five.
		want := []string{
			"ListInstances", "ListInstanceVPCs", "ListInstanceVPCs", "ListInstanceVPCs", "ListInstanceVPCs",
			"ListInstanceVPCs",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("calls of the cloud (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"Leader", "Peers", "Health", "Nodes"}, nomadCallNames(w, nomadBefore)); diff != "" {
			t.Errorf("calls of Nomad (-want +got):\n%s", diff)
		}
	})
}

// readOnlyProbe is a store of a kind that is no file store, which fails the test on a Capabilities call and on any
// write: the s3 store answers Capabilities with a probe that writes.
type readOnlyProbe struct {
	statestore.Store
	t *testing.T
}

func (s readOnlyProbe) Capabilities(context.Context) (statestore.Capabilities, error) {
	s.t.Error("validate asked the store for its Capabilities")
	return statestore.Capabilities{}, errors.New("not expected")
}

func (s readOnlyProbe) Put(
	_ context.Context, p string, _ []byte, _ statestore.PutOptions,
) (statestore.Version, error) {
	s.t.Errorf("validate put %q", p)
	return "", errors.New("not expected")
}

func (s readOnlyProbe) Delete(_ context.Context, p string) error {
	s.t.Errorf("validate deleted %q", p)
	return errors.New("not expected")
}

// TestValidateClusterReadsTheLockWithoutProbingTheStore validates over a store that is no file store without a
// Capabilities call or a write, and still shows the lease that a holder wrote.
func TestValidateClusterReadsTheLockWithoutProbingTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		inner := svc.Store
		svc.Store = readOnlyProbe{inner, t}
		lease := statestore.Lease{ID: "other", Owner: "tester", Host: "elsewhere", PID: 7, Operation: "update",
			AcquiredAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
		data, err := json.Marshal(lease)
		if err != nil {
			t.Fatal(err)
		}
		put(t, inner, lockPath, data)

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Lock == nil || v.Lock.ID != "other" {
			t.Errorf("Lock = %+v, want the lease %q", v.Lock, "other")
		}
	})
}

// nomadCallNames returns the methods of the calls that reached the Nomad fake after the first n.
func nomadCallNames(w *nomadWorld, n int) []string {
	var names []string
	for _, c := range w.Log()[n:] {
		names = append(names, c.Name)
	}
	return names
}

// TestValidateClusterWithoutTheCluster fails for a cluster that the store does not hold, and writes nothing.
func TestValidateClusterWithoutTheCluster(t *testing.T) {
	svc, root := newService(t)
	withCloud(svc)

	_, err := svc.ValidateCluster(t.Context(), "prod")

	wantError(t, err, notFound(svc, "cluster prod"))
	wantNothingWritten(t, root)
}

// TestValidateClusterChecksTheTentVersion fails for a cluster that a newer tent wrote.
func TestValidateClusterChecksTheTentVersion(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	withCloud(svc)
	put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
	svc.Version = "v0.4.0"

	_, err := svc.ValidateCluster(t.Context(), "prod")

	wantError(t, err, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
}

// TestValidateClusterChecksTheSpecs fails for a single server without AllowSingleServer, as an update does.
func TestValidateClusterChecksTheSpecs(t *testing.T) {
	svc, _ := newService(t)
	svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
	mustCreate(t, svc, clusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
	withCloud(svc)

	svc.Validate = v1alpha1.ValidateOptions{}
	_, err := svc.ValidateCluster(t.Context(), "prod")

	wantFieldErrors(t, err, v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "size 1 needs --allow-single-server",
	})

	svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
	if _, err := svc.ValidateCluster(t.Context(), "prod"); err != nil {
		t.Errorf("ValidateCluster with AllowSingleServer: %v", err)
	}
}

// TestValidateClusterNeedsProviders fails when the service has no cloud providers.
func TestValidateClusterNeedsProviders(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)

	_, err := svc.ValidateCluster(t.Context(), "prod")

	wantError(t, err, "no cloud providers are set up")
}

// exampleValidation is a result with a failure of each kind, a warning and a lock.
func exampleValidation() app.Validation {
	acquired := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	return app.Validation{
		Cluster: "prod", Servers: 3, Clients: 2, NomadVersion: "2.0.7",
		Failures: []app.Failure{
			{Check: "machine-missing", Node: "prod-workers-1", Detail: "no machine: node group workers has 1 of its 2"},
			{
				Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-4",
				Detail: "the cloud reports its machine (ID instance-4) as not running",
			},
			{
				Check: "machine-surplus", Node: "prod-workers-2", ID: "instance-6",
				Detail: "machine ID instance-6 is one more than the size of node group workers, 2",
			},
			{
				Check: "machine-duplicate", Node: "prod-workers-0", ID: "instance-9",
				Detail: "machine ID instance-9 has the name of machine ID instance-4",
			},
			{
				Check: "machine-unknown", Node: "prod-old-0", ID: "instance-7",
				Detail: "machine ID instance-7 is of node group old, which the specs do not have",
			},
			{
				Check: "not-joined", Node: "prod-servers-2", ID: "instance-3",
				Detail: "machine ID instance-3 has not joined Nomad: it carries no tent/joined label",
			},
			{Check: "nomad-not-set-up", Detail: "no server has a public address"},
			{
				Check: "nomad-no-leader",
				Detail: "Nomad has no leader, or tent cannot reach it: no route; tent reaches the servers on port " +
					"4646: check spec.access.api",
			},
			{
				Check: "server-no-vote", Node: "prod-servers-1", ID: "instance-2",
				Detail: "no server votes at its address 10.64.0.4",
			},
			{
				Check: "server-unknown",
				Detail: "the Raft configuration lists a server at 10.64.0.9:4647 (prod-servers-9.global) that is no " +
					"server machine of the cluster",
			},
			{Check: "autopilot-unhealthy", Detail: "autopilot reports the servers unhealthy"},
			{
				Check: "server-not-alive", Node: "prod-servers-2", ID: "instance-3",
				Detail: "Serf reports its server as left",
			},
			{
				Check: "server-unhealthy", Node: "prod-servers-2", ID: "instance-3",
				Detail: "autopilot reports its server unhealthy",
			},
			{
				Check: "client-not-registered", Node: "prod-workers-1", ID: "instance-5",
				Detail: "Nomad lists no client of its name at 10.64.0.7",
			},
			{
				Check: "client-not-ready", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its Nomad client is down",
			},
			{
				Check: "nomad-version", Node: "prod-workers-0", ID: "instance-4",
				Detail: "its client runs Nomad 2.0.6; the cluster is pinned to 2.0.7",
			},
			{
				Check: "certificate-expired", Node: "prod-servers-0", ID: "instance-1",
				Detail: "its node certificate ended about 2027-10-05",
			},
		},
		Warnings: []string{
			openAPIWarning, oneZoneWarning,
			"the certificate of node prod-workers-0 ends about 2026-11-01, in 26 days; node certificates last one " +
				"year, and a node gets a new one when it is replaced",
		},
		Lock: &statestore.Lease{
			ID: "other", Owner: "igor", Host: "laptop", PID: 4242, Operation: "update",
			AcquiredAt: acquired, ExpiresAt: acquired.Add(2 * time.Minute),
		},
	}
}

// validText returns what v.WriteText writes.
func validText(t *testing.T, v app.Validation) string {
	t.Helper()
	var b strings.Builder
	if err := v.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestValidationTextGolden(t *testing.T) {
	checkGolden(t, "validate.golden", validText(t, exampleValidation()))
}

func TestValidationJSONGolden(t *testing.T) {
	checkGolden(t, "validate.json.golden", encodeJSON(t, exampleValidation(), "  "))
}

func TestValidationWriteText(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    app.Validation
		want string
	}{
		{
			"valid", app.Validation{Cluster: "prod", Servers: 3, Clients: 2, NomadVersion: "2.0.7"},
			"cluster prod is valid: 3 servers and 2 clients run Nomad 2.0.7\n",
		},
		{
			"valid with one of each", app.Validation{Cluster: "dev", Servers: 1, Clients: 1, NomadVersion: "2.0.7"},
			"cluster dev is valid: 1 server and 1 client run Nomad 2.0.7\n",
		},
		{
			"one failure of the cluster",
			app.Validation{Cluster: "prod", Failures: []app.Failure{{Check: "nomad-no-leader", Detail: "no leader"}}},
			"NODE  FAILURE\n-     no leader\n\ncluster prod is not valid: 1 failure\n",
		},
		{
			"two failures of nodes, aligned",
			app.Validation{Cluster: "prod", Failures: []app.Failure{
				{Check: "not-joined", Node: "a", ID: "instance-1", Detail: "x"},
				{Check: "not-joined", Node: "prod-workers-10", ID: "instance-2", Detail: "y"},
			}},
			"NODE             FAILURE\na                x\nprod-workers-10  y\n\ncluster prod is not valid: 2 failures\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, validText(t, tc.v)); diff != "" {
				t.Errorf("WriteText (-want +got):\n%s", diff)
			}
		})
	}
}

func TestValidationJSONListsAreNeverNull(t *testing.T) {
	got := encodeJSON(t, app.Validation{Cluster: "prod", Servers: 3, Clients: 2, NomadVersion: "2.0.7"}, "")
	want := `{"cluster":"prod","valid":true,"servers":3,"clients":2,"nomadVersion":"2.0.7",` +
		`"failures":[],"warnings":[]}` + "\n"
	if got != want {
		t.Errorf("JSON of a valid result\n got %swant %s", got, want)
	}
}

func TestValidationWriteTextWriteError(t *testing.T) {
	errBroken := errors.New("broken pipe")
	for name, v := range map[string]app.Validation{
		"valid":     {Cluster: "prod", Servers: 3, Clients: 2, NomadVersion: "2.0.7"},
		"not valid": exampleValidation(),
	} {
		t.Run(name, func(t *testing.T) {
			err := v.WriteText(&failWriter{err: errBroken})

			if !errors.Is(err, errBroken) || err.Error() != "writing the result: broken pipe" {
				t.Errorf("WriteText error = %v, want %q wrapping %v", err, "writing the result: broken pipe", errBroken)
			}
		})
	}
}

// TestValidateClusterFailsWhenTheCloudDoesNotAnswer returns the error of a list that fails: a machine that cannot be
// listed is not a missing machine.
func TestValidateClusterFailsWhenTheCloudDoesNotAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := builtCluster(t)
		var told []string
		svc.OnWarning = func(w string) { told = append(told, w) }
		errDown := errors.New("the cloud is down")
		f.Fail(t, "ListInstances", errDown, 8)

		v, err := svc.ValidateCluster(t.Context(), "prod")

		if !errors.Is(err, errDown) {
			t.Errorf("error = %v, want %v", err, errDown)
		}
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
		if len(told) != 0 {
			t.Errorf("OnWarning was told %q", told)
		}
	})
}

// cancelOnGet is a store whose Get of the lock cancels the context and fails with its error. Wrapped in unwrapped,
// statestore.Holder reads the lease from it.
type cancelOnGet struct {
	statestore.Store
	cancel context.CancelFunc
	ctx    context.Context
}

func (s cancelOnGet) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	if p == lockPath {
		s.cancel()
		return nil, "", s.ctx.Err()
	}
	return s.Store.Get(ctx, p)
}

// TestValidateClusterIsInterruptedWhileReadingTheLock says "interrupted" when the context ends during the read of the
// lock, and does not turn the end into a warning.
func TestValidateClusterIsInterruptedWhileReadingTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc.Store = unwrapped{cancelOnGet{Store: svc.Store, cancel: cancel, ctx: ctx}}

		v, err := svc.ValidateCluster(ctx, "prod")

		if err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
		}
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
	})
}

// ipOf returns the private address of the machine id of the Vultr fake f.
func ipOf(t *testing.T, f *vultrfake.Fake, id string) string {
	t.Helper()
	vpcs := f.InstanceVPCs(id)
	if len(vpcs) == 0 {
		t.Fatalf("machine %s has no VPC", id)
	}
	return vpcs[0].IPAddress
}

// pinnedNomad returns the Nomad version that the embedded stable channel pins a new cluster to.
func pinnedNomad(t *testing.T) string {
	t.Helper()
	ch, err := channels.Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	return ch.Nomad.Recommended
}

// noLeaderSuffix ends the detail of a failure to reach Nomad.
const noLeaderSuffix = "; tent reaches the servers on port 4646: check spec.access.api"

// TestValidateClusterFindsWhatDiffersInNomad changes what the Nomad of the built cluster answers, one thing at a time,
// and fails the check for it.
func TestValidateClusterFindsWhatDiffersInNomad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*nomadWorld)
		want   func(ip func(id string) string, pinned string) []app.Failure
	}{
		{
			"a server that has no vote",
			func(w *nomadWorld) {
				w.ChangePeers(func(p []nomadops.Peer) []nomadops.Peer {
					return slices.DeleteFunc(p, func(x nomadops.Peer) bool { return x.Name == "prod-servers-1.global" })
				})
			},
			func(ip func(string) string, _ string) []app.Failure {
				return []app.Failure{{
					Check: "server-no-vote", Node: "prod-servers-1", ID: "instance-2",
					Detail: "no server votes at its address " + ip("instance-2"),
				}}
			},
		},
		{
			"a server that has no vote yet",
			func(w *nomadWorld) {
				w.ChangePeers(func(p []nomadops.Peer) []nomadops.Peer {
					p[1].Voter = false
					return p
				})
			},
			func(ip func(string) string, _ string) []app.Failure {
				return []app.Failure{{
					Check: "server-no-vote", Node: "prod-servers-1", ID: "instance-2",
					Detail: "its server at " + ip("instance-2") + ":4647 has no vote",
				}}
			},
		},
		{
			"a peer that is no machine",
			func(w *nomadWorld) {
				w.ChangePeers(func(p []nomadops.Peer) []nomadops.Peer {
					return append(p, nomadops.Peer{
						Name: "prod-servers-9.global", Address: netip.MustParseAddrPort("10.64.0.9:4647"), Voter: true,
					})
				})
			},
			func(func(string) string, string) []app.Failure {
				return []app.Failure{{
					Check: "server-unknown",
					Detail: "the Raft configuration lists a server at 10.64.0.9:4647 (prod-servers-9.global) that is no " +
						"server machine of the cluster",
				}}
			},
		},
		{
			"a server that left",
			func(w *nomadWorld) {
				w.ChangeServer("prod-servers-2", func(sv *nomadops.ServerHealth) { sv.Serf, sv.Healthy = "left", false })
			},
			func(func(string) string, string) []app.Failure {
				return []app.Failure{
					{
						Check: "server-not-alive", Node: "prod-servers-2", ID: "instance-3",
						Detail: "Serf reports its server as left",
					},
					{
						Check: "server-unhealthy", Node: "prod-servers-2", ID: "instance-3",
						Detail: "autopilot reports its server unhealthy",
					},
				}
			},
		},
		{
			"a server that autopilot does not list",
			func(w *nomadWorld) { w.DropServer("prod-servers-0") },
			func(func(string) string, string) []app.Failure {
				return []app.Failure{{
					Check: "server-not-alive", Node: "prod-servers-0", ID: "instance-1",
					Detail: "autopilot does not list its server",
				}}
			},
		},
		{
			"an unhealthy report",
			func(w *nomadWorld) { w.Unhealthy() },
			func(func(string) string, string) []app.Failure {
				return []app.Failure{{Check: "autopilot-unhealthy", Detail: "autopilot reports the servers unhealthy"}}
			},
		},
		{
			"a worker that Nomad does not list",
			func(w *nomadWorld) { w.DropNode("prod-workers-1") },
			func(ip func(string) string, _ string) []app.Failure {
				return []app.Failure{{
					Check: "client-not-registered", Node: "prod-workers-1", ID: "instance-5",
					Detail: "Nomad lists no client of its name at " + ip("instance-5"),
				}}
			},
		},
		{
			"a worker whose node is down while its machine carries the label",
			func(w *nomadWorld) { w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Status = "down" }) },
			func(func(string) string, string) []app.Failure {
				return []app.Failure{{
					Check: "client-not-ready", Node: "prod-workers-0", ID: "instance-4",
					Detail: "its Nomad client is down",
				}}
			},
		},
		{
			"a worker that is not eligible",
			func(w *nomadWorld) { w.ChangeNode("prod-workers-1", func(n *nomadops.Node) { n.Eligible = false }) },
			func(func(string) string, string) []app.Failure {
				return []app.Failure{{
					Check: "client-not-ready", Node: "prod-workers-1", ID: "instance-5",
					Detail: "its Nomad client is ready but not eligible",
				}}
			},
		},
		{
			"a server and a worker of another version",
			func(w *nomadWorld) {
				w.ChangeServer("prod-servers-1", func(sv *nomadops.ServerHealth) { sv.Version = "2.0.1" })
				w.ChangeNode("prod-workers-0", func(n *nomadops.Node) { n.Version = "2.0.2" })
			},
			func(_ func(string) string, pinned string) []app.Failure {
				return []app.Failure{
					{
						Check: "nomad-version", Node: "prod-servers-1", ID: "instance-2",
						Detail: "its server runs Nomad 2.0.1; the cluster is pinned to " + pinned,
					},
					{
						Check: "nomad-version", Node: "prod-workers-0", ID: "instance-4",
						Detail: "its client runs Nomad 2.0.2; the cluster is pinned to " + pinned,
					},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				tc.change(w)

				v := mustValidate(t, svc)

				wantFailures(t, v, tc.want(func(id string) string { return ipOf(t, f, id) }, pinnedNomad(t))...)
			})
		})
	}
}

// TestValidateClusterNeedsNomadThatAnswers fails with one failure of the cluster when Nomad has no leader, or fails a
// call after it had one, and asks nothing more.
func TestValidateClusterNeedsNomadThatAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		w.NoLeader()
		before := len(w.Log())

		v := mustValidate(t, svc)

		if len(v.Failures) != 1 || v.Failures[0].Check != "nomad-no-leader" || v.Failures[0].Node != "" ||
			v.Failures[0].ID != "" ||
			!strings.HasPrefix(v.Failures[0].Detail, "Nomad has no leader, or tent cannot reach it: ") ||
			!strings.HasSuffix(v.Failures[0].Detail, noLeaderSuffix) {
			t.Errorf("failures = %+v, want only nomad-no-leader, of the cluster, with the cause and the advice", v.Failures)
		}
		if got := nomadCallNames(w, before); slices.Contains(got, "Peers") || slices.Contains(got, "Health") ||
			slices.Contains(got, "Nodes") {
			t.Errorf("Nomad was asked %v after Leader failed", got)
		}
	})
}

// TestValidateClusterFailsWhenAnotherNomadCallFails fails the check of Nomad with the call's error when the leader
// answered but a later call fails, and asks no further.
func TestValidateClusterFailsWhenAnotherNomadCallFails(t *testing.T) {
	for _, tc := range []struct{ call, what string }{
		{"Peers", "read the Raft configuration"},
		{"Health", "read autopilot's health"},
		{"Nodes", "list the nodes"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w, _ := builtWorld(t)
				w.Fail(t, tc.call, errBoom)

				v := mustValidate(t, svc)

				want := "Nomad has no leader, or tent cannot reach it: " + tc.what + ": boom" + noLeaderSuffix
				wantFailures(t, v, app.Failure{Check: "nomad-no-leader", Detail: want})
			})
		})
	}
}

// TestValidateClusterNeedsTheStoreToSetUpNomad fails with one failure of the cluster, and asks Nomad nothing, when
// the store lacks a secret or the mark of the bootstrap, or no server has a public address.
func TestValidateClusterNeedsTheStoreToSetUpNomad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *app.Service)
		want   string
	}{
		{"no mark", func(t *testing.T, svc *app.Service) { deleteObject(t, svc, markPath) },
			"Nomad is not bootstrapped yet: tent update cluster --yes bootstraps it"},
		{"no gossip key", func(t *testing.T, svc *app.Service) { deleteObject(t, svc, gossipPath) },
			"the state store lacks secrets/gossip.key"},
		{"no secrets and no mark", func(t *testing.T, svc *app.Service) {
			for _, p := range []string{caKeyPath, caBundlePath, gossipPath, aclPath, markPath} {
				deleteObject(t, svc, p)
			}
		}, "the state store lacks pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key and secrets/acl-bootstrap-token"},
		{"no public address", func(_ *testing.T, svc *app.Service) {
			inner := svc.Providers
			svc.Providers = func(name v1alpha1.Provider) (cloud.Provider, error) {
				p, err := inner(name)
				return withoutPublicAddresses{p}, err
			}
		}, "no server has a public address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w, _ := builtWorld(t)
				tc.change(t, svc)
				before := len(w.Log())

				v := mustValidate(t, svc)

				wantFailures(t, v, app.Failure{Check: "nomad-not-set-up", Detail: tc.want})
				if got := nomadCallNames(w, before); len(got) != 0 {
					t.Errorf("Nomad was asked %v", got)
				}
			})
		})
	}
}

// deleteObject removes the object p from the store of svc.
func deleteObject(t *testing.T, svc *app.Service, p string) {
	t.Helper()
	if err := svc.Store.Delete(t.Context(), p); err != nil {
		t.Fatalf("delete %s: %v", p, err)
	}
}

// withoutPublicAddresses is a provider whose machines have no public address.
type withoutPublicAddresses struct{ cloud.Provider }

func (p withoutPublicAddresses) Nodes() cloud.Nodes { return noPublicNodes{p.Provider.Nodes()} }

type noPublicNodes struct{ cloud.Nodes }

func (n noPublicNodes) List(ctx context.Context, cluster string) ([]cloud.Instance, error) {
	instances, err := n.Nodes.List(ctx, cluster)
	for i := range instances {
		instances[i].PublicIP = netip.Addr{}
	}
	return instances, err
}

// TestValidateClusterFailsForASecretThatDoesNotLoad fails with an error when a stored secret is no secret of its kind:
// tent could not check, which is no failure of the cluster as a missing secret is. It asks Nomad nothing, and the
// error does not show the stored bytes.
func TestValidateClusterFailsForASecretThatDoesNotLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		stored := []byte("hunter2-is-no-gossip-key")
		put(t, svc.Store, gossipPath, stored)
		before := len(w.Log())

		v, err := svc.ValidateCluster(t.Context(), "prod")

		if err == nil {
			t.Fatalf("ValidateCluster succeeded with %+v, want an error", v.Failures)
		}
		if want := gossipPath + ": gossip key: not standard base64"; err.Error() != want {
			t.Errorf("error = %v\nwant    %s", err, want)
		}
		secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, map[string][]byte{gossipPath: stored}, "")
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
		if got := nomadCallNames(w, before); len(got) != 0 {
			t.Errorf("Nomad was asked %v", got)
		}
	})
}

// TestValidateClusterNeedsANomadClient fails with an error, not a failure, when the service has no way to reach Nomad.
func TestValidateClusterNeedsANomadClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		svc.Nomad = nil

		v, err := svc.ValidateCluster(t.Context(), "prod")

		wantError(t, err, "no Nomad client is set up")
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterNeedsANomadClientBeforeTheSecrets fails with the error even for a cluster that was never
// updated, whose store holds no secrets and no mark: it is no failure of Nomad.
func TestValidateClusterNeedsANomadClientBeforeTheSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
		withCloud(svc)
		svc.Nomad = nil

		v, err := svc.ValidateCluster(t.Context(), "prod")

		wantError(t, err, "no Nomad client is set up")
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterPassesAClusterOfOneNode counts a single combined node as a server and as a client.
func TestValidateClusterPassesAClusterOfOneNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		svc.Validate = v1alpha1.ValidateOptions{AllowSingleServer: true}
		mustCreate(t, svc, clusterYAML, edit(t, combinedYAML, "size: 3", "size: 1"))
		withCloud(svc)
		mustUpdate(t, svc)

		v := mustValidate(t, svc)

		wantFailures(t, v)
		if v.Servers != 1 || v.Clients != 1 {
			t.Errorf("%d servers and %d clients, want 1 and 1", v.Servers, v.Clients)
		}
	})
}

// certNodes are the names of the machines of the test cluster, in the order of their IDs instance-1 to instance-5.
var certNodes = []string{"prod-servers-0", "prod-servers-1", "prod-servers-2", "prod-workers-0", "prod-workers-1"}

// expiredCertificates returns the failures of the certificates of the test cluster a long time after an update made
// it: the CA's, then each node's.
func expiredCertificates() []app.Failure {
	fs := []app.Failure{{Check: "certificate-expired", Detail: "the cluster CA ended on 2010-01-01"}}
	for i, name := range certNodes {
		fs = append(fs, app.Failure{
			Check: "certificate-expired", Node: name, ID: "instance-" + strconv.Itoa(i+1),
			Detail: "its node certificate ended about 2001-01-01",
		})
	}
	return fs
}

// TestValidateClusterWarnsAboutCertificatesThatEndSoon tells, as warnings, which node certificates end within 30 days.
// A node certificate ends a year after its machine was made.
func TestValidateClusterWarnsAboutCertificatesThatEndSoon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		time.Sleep(339*24*time.Hour + 12*time.Hour) // the bubble's clock starts at 2000-01-01, a leap year
		var told []string
		svc.OnWarning = func(w string) { told = append(told, w) }

		v := mustValidate(t, svc)

		wantFailures(t, v)
		want := []string{openAPIWarning, oneZoneWarning}
		for _, name := range certNodes {
			want = append(want, "the certificate of node "+name+" ends about 2001-01-01, in 26 days; node certificates "+
				"last one year, and a node gets a new one when it is replaced")
		}
		if diff := cmp.Diff(want, v.Warnings); diff != "" {
			t.Errorf("warnings (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(want, told); diff != "" {
			t.Errorf("OnWarning was told (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterFailsForCertificatesThatEnded fails the checks of the certificates of the nodes and of the CA
// once they have ended.
func TestValidateClusterFailsForCertificatesThatEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := builtCluster(t)
		time.Sleep(11 * 366 * 24 * time.Hour)

		v := mustValidate(t, svc)

		wantFailures(t, v, expiredCertificates()...)
		if len(v.Warnings) != 2 {
			t.Errorf("warnings = %q, want the two of the cluster", v.Warnings)
		}
	})
}

// TestValidateClusterFailsForCertificatesBesideNomadThatCannotBeAsked shows the failures of the certificates beside
// the failure of a Nomad that tent cannot ask: an expired CA may be why tent cannot reach Nomad.
func TestValidateClusterFailsForCertificatesBesideNomadThatCannotBeAsked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *app.Service, *nomadWorld)
		want   app.Failure
	}{
		{"no leader", func(_ *testing.T, _ *app.Service, w *nomadWorld) { w.NoLeader() },
			app.Failure{Check: "nomad-no-leader"}},
		{"no mark", func(t *testing.T, svc *app.Service, _ *nomadWorld) { deleteObject(t, svc, markPath) },
			app.Failure{
				Check: "nomad-not-set-up", Detail: "Nomad is not bootstrapped yet: tent update cluster --yes bootstraps it",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w, _ := builtWorld(t)
				time.Sleep(11 * 366 * 24 * time.Hour)
				tc.change(t, svc, w)

				v := mustValidate(t, svc)

				if len(v.Failures) != 1+len(certNodes)+1 || v.Failures[0].Check != tc.want.Check {
					t.Fatalf("failures = %+v, want %s and the %d of the certificates", v.Failures, tc.want.Check,
						len(certNodes)+1)
				}
				if tc.want.Detail != "" && v.Failures[0].Detail != tc.want.Detail {
					t.Errorf("detail = %q, want %q", v.Failures[0].Detail, tc.want.Detail)
				}
				if diff := cmp.Diff(expiredCertificates(), v.Failures[1:]); diff != "" {
					t.Errorf("certificate failures (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestValidateClusterOrdersFailuresOfAllSources puts the failures of the machines, of Nomad and of the certificates in
// one list, in the order of the checks, then by node. The server that left is an older machine than the one without a
// vote, so its failures are found first and come after.
func TestValidateClusterOrdersFailuresOfAllSources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := builtWorld(t)
		time.Sleep(11 * 366 * 24 * time.Hour)
		if err := f.HaltInstance(t.Context(), "instance-4"); err != nil {
			t.Fatal(err)
		}
		w.ChangePeers(func(p []nomadops.Peer) []nomadops.Peer {
			return slices.DeleteFunc(p, func(x nomadops.Peer) bool { return x.Name == "prod-servers-1.global" })
		})
		w.ChangeServer("prod-servers-0", func(sv *nomadops.ServerHealth) { sv.Serf, sv.Healthy = "left", false })

		v := mustValidate(t, svc)

		var got []string
		for _, x := range v.Failures {
			got = append(got, x.Check+" "+x.Node)
		}
		want := []string{
			"machine-not-running prod-workers-0",
			"server-no-vote prod-servers-1",
			"server-not-alive prod-servers-0",
			"server-unhealthy prod-servers-0",
			"certificate-expired ",
			"certificate-expired prod-servers-0",
			"certificate-expired prod-servers-1",
			"certificate-expired prod-servers-2",
			"certificate-expired prod-workers-0",
			"certificate-expired prod-workers-1",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("failures (-want +got):\n%s", diff)
		}
	})
}

// TestValidateClusterShowsNoSecrets keeps every secret of the cluster out of the text, the JSON, the warnings and the
// errors of a valid cluster, of a cluster that fails checks of Nomad and of one whose Nomad call fails.
func TestValidateClusterShowsNoSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		secrets := secretsOf(t, svc.Store)
		var shown strings.Builder
		show := func(v app.Validation, err error) {
			shown.WriteString(validText(t, v) + encodeJSON(t, v, "  ") + strings.Join(v.Warnings, "\n"))
			if err != nil {
				shown.WriteString(err.Error())
			}
		}
		svc.OnWarning = func(w string) { shown.WriteString(w + "\n") }
		show(svc.ValidateCluster(t.Context(), "prod"))
		w.Unhealthy()
		w.DropNode("prod-workers-0")
		show(svc.ValidateCluster(t.Context(), "prod"))
		w.Fail(t, "Peers", errBoom)
		show(svc.ValidateCluster(t.Context(), "prod"))
		w.NoLeader()
		show(svc.ValidateCluster(t.Context(), "prod"))
		deleteObject(t, svc, gossipPath)
		show(svc.ValidateCluster(t.Context(), "prod"))

		if len(secrets) != len(secretPaths) {
			t.Fatalf("the store held %d secrets, want %d", len(secrets), len(secretPaths))
		}
		secrettest.CheckHidden(t, map[string]string{"the output": shown.String()}, secrets, "")
	})
}

// TestValidateClusterIsInterruptedWhileAskingNomad says "interrupted" when the context ends during a Nomad call, and
// does not turn the end into a failure.
func TestValidateClusterIsInterruptedWhileAskingNomad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
			if c.Name == "Peers" {
				cancel()
				return ctx.Err()
			}
			return next(ctx)
		})

		v, err := svc.ValidateCluster(ctx, "prod")

		if err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
		}
		if diff := cmp.Diff(app.Validation{}, v); diff != "" {
			t.Errorf("Validation (-want +got):\n%s", diff)
		}
	})
}
