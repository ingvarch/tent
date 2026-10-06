package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/statestore"
)

// oneZoneWarning is the warning about the test cluster, whose machines all run in one region.
const oneZoneWarning = "cluster prod runs in one failure domain, ams: an outage there takes the whole cluster down"

// builtCluster returns a service over the test cluster, built by an update, the Vultr fake it ran on and the store's
// root directory.
func builtCluster(t *testing.T) (*app.Service, *vultrfake.Fake, string) {
	t.Helper()
	svc, root := newService(t)
	mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
	f, _ := withCloud(svc)
	mustUpdate(t, svc)
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

// TestValidateClusterPassesTheMachineChecks validates the cluster that an update built: it is valid as far as the
// machines go, and tells the counts, the pinned Nomad version and the warnings about the specs and the zone.
func TestValidateClusterPassesTheMachineChecks(t *testing.T) {
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
		svc, _ := newUpdate(t)
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

// TestValidateClusterFindsMachinesThatChanged fails the checks of the machines that a halt and a delete leave.
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
		svc, f, _ := builtCluster(t)
		before, calls := snapshot(t, svc.Store), len(f.Calls())

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
			{
				Check: "nomad-no-leader",
				Detail: "Nomad has no leader, or tent cannot reach it: no route; tent reaches the servers on port " +
					"4646: check spec.access.api",
			},
		},
		Warnings: []string{openAPIWarning, oneZoneWarning},
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
