package app_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// accessTTL is how long the access of the tests lasts.
const accessTTL = 2 * time.Hour

// hintOfAccess is the end of the error of an operator access that no server answered.
const hintOfAccess = "; tent reaches the servers on port 4646: check spec.access.api"

// tokenName returns the name of the token that an access for purpose asks for, as the cluster lock names its holder.
func tokenName(purpose string) string {
	owner, host := statestore.LocalHolder()
	return "tent " + purpose + " " + owner + "@" + host
}

// mustAccess asks for the operator access of the test cluster for an export, and stops the test on an error.
func mustAccess(t *testing.T, svc *app.Service) app.Access {
	t.Helper()
	acc, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)
	if err != nil {
		t.Fatalf("OperatorAccess: %v", err)
	}
	return acc
}

// apiAddresses returns the public address of each machine called names, on the API port, in the order of names.
func apiAddresses(t *testing.T, f *vultrfake.Fake, names ...string) []string {
	t.Helper()
	var out []string
	for _, name := range names {
		found := false
		for _, in := range f.Instances() {
			if in.Hostname == name {
				out, found = append(out, in.MainIP+":4646"), true
			}
		}
		if !found {
			t.Fatalf("the fake has no machine %s", name)
		}
	}
	return out
}

// certOf returns the certificate that the access holds, and stops the test when it holds none.
func certOf(t *testing.T, acc app.Access) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(acc.Cert)
	if block == nil {
		t.Fatal("the access holds no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// storedSecretBytes returns the secrets of the test cluster in the store, by name, as bytes.
func storedSecretBytes(t *testing.T, svc *app.Service) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"the CA key": get(t, svc.Store, caKeyPath), "the gossip key": get(t, svc.Store, gossipPath),
		"the bootstrap secret": get(t, svc.Store, aclPath),
	}
}

// wantAccessError fails the test unless err says want and shows none of the secrets.
func wantAccessError(t *testing.T, err error, want string, secrets map[string][]byte) {
	t.Helper()
	wantError(t, err, want)
	if err != nil {
		secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, secrets, "")
	}
}

// TestOperatorAccessMakesATokenAndACertificate asks for the access of a built cluster: one management token with a
// name and the TTL, which is not the bootstrap secret, and an operator certificate of the cluster's CA that ends with
// the token. The answering server comes first among the servers' addresses, and the store, the lock and the
// cloud's objects stay as they were.
func TestOperatorAccessMakesATokenAndACertificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := builtWorld(t)
		rec := &writeLog{Store: svc.Store}
		svc.Store = rec
		secrets := storedSecretBytes(t, svc)
		before, cloudCalls, nomadBefore := snapshot(t, svc.Store), len(f.Calls()), len(w.Log())
		lists, now := countCalls(f, "ListInstances"), time.Now()

		acc := mustAccess(t, svc)

		calls := w.Log()[nomadBefore:]
		if len(calls) != 1 || calls[0].Name != "CreateToken" {
			t.Fatalf("calls of Nomad = %v, want one CreateToken", calls)
		}
		wantArg := tokenName("export nomad") + " " + accessTTL.String()
		if calls[0].Arg != wantArg || w.nodeName(calls[0].Server) != "prod-servers-0" {
			t.Errorf("CreateToken %q to %s, want %q to prod-servers-0", calls[0].Arg, w.nodeName(calls[0].Server), wantArg)
		}
		wantIssued := []nomadfake.IssuedToken{{Name: tokenName("export nomad"), TTL: accessTTL, Accessor: acc.Accessor}}
		if diff := cmp.Diff(wantIssued, w.Issued()); diff != "" {
			t.Errorf("tokens that Nomad issued (-want +got):\n%s", diff)
		}
		if len(acc.Token) == 0 || string(acc.Token) == string(secrets["the bootstrap secret"]) {
			t.Errorf("the token is the bootstrap secret or empty")
		}
		if end := now.Add(accessTTL).Truncate(time.Second); acc.Cluster != "prod" || acc.Region != "global" ||
			!acc.Until.Equal(end) || acc.Until.Location() != time.UTC {
			t.Errorf("cluster %q, region %q, until %v; want prod, global, %v in UTC", acc.Cluster, acc.Region, acc.Until,
				end)
		}
		wantServers := apiAddresses(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2")
		if diff := cmp.Diff(wantServers, acc.Servers); diff != "" {
			t.Errorf("servers (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(string(get(t, svc.Store, caBundlePath)), string(acc.CA)); diff != "" {
			t.Errorf("CA bundle (-want +got):\n%s", diff)
		}
		wantOperatorCertificate(t, acc, now, now.Add(accessTTL))

		wantSnapshot(t, svc.Store, before)
		if len(rec.writes) != 0 {
			t.Errorf("writes to the store: %v", rec.writes)
		}
		wantLockFree(t, svc.Store)
		wantNoWrites(t, f.Calls()[cloudCalls:])
		if n := countCalls(f, "ListInstances") - lists; n != 1 {
			t.Errorf("%d lists of the machines, want 1", n)
		}
	})
}

// withAccessSecrets adds the token's secret and the certificate's key of acc to secrets.
func withAccessSecrets(secrets map[string][]byte, acc app.Access) map[string][]byte {
	all := map[string][]byte{"the token": acc.Token.Bytes(), "the certificate's key": acc.Key.Bytes()}
	for name, s := range secrets {
		all[name] = s
	}
	return all
}

// wantOperatorCertificate fails the test unless the access holds the certificate of an operator of the region global,
// made by the cluster's CA for client authentication only, valid from five minutes before from to end, with its key.
func wantOperatorCertificate(t *testing.T, acc app.Access, from, end time.Time) {
	t.Helper()
	cert := certOf(t, acc)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(acc.CA) {
		t.Fatal("the CA bundle holds no certificate")
	}
	_, err := cert.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: from,
	})
	if err != nil {
		t.Errorf("the certificate does not verify against the CA for client authentication: %v", err)
	}
	if diff := cmp.Diff([]string{"cli.global.nomad"}, cert.DNSNames); diff != "" {
		t.Errorf("DNS names (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, cert.ExtKeyUsage); diff != "" {
		t.Errorf("extended key usage (-want +got):\n%s", diff)
	}
	start := from.Add(-5 * time.Minute)
	if !cert.NotBefore.Equal(start.Truncate(time.Second)) || !cert.NotAfter.Equal(end.Truncate(time.Second)) {
		t.Errorf("the certificate is valid from %v to %v, want %v to %v", cert.NotBefore, cert.NotAfter, start, end)
	}
	if _, err := tls.X509KeyPair(acc.Cert, acc.Key.Bytes()); err != nil {
		t.Errorf("the key does not belong to the certificate: %v", err)
	}
}

// TestOperatorAccessMovesToTheNextServer loses the answer of the first server: the second makes a token, which the
// access holds, and its address comes first, the others after it by name. Nomad keeps the lost token.
func TestOperatorAccessMovesToTheNextServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := builtWorld(t)
		w.LoseResponse(t, "CreateToken")
		nomadBefore := len(w.Log())

		acc := mustAccess(t, svc)

		wantServers := apiAddresses(t, f, "prod-servers-1", "prod-servers-0", "prod-servers-2")
		if diff := cmp.Diff(wantServers, acc.Servers); diff != "" {
			t.Errorf("servers (-want +got):\n%s", diff)
		}
		var to []string
		for _, c := range w.Log()[nomadBefore:] {
			to = append(to, c.Name+" "+w.nodeName(c.Server))
		}
		if diff := cmp.Diff([]string{"CreateToken prod-servers-0", "CreateToken prod-servers-1"}, to); diff != "" {
			t.Errorf("calls of Nomad (-want +got):\n%s", diff)
		}
		issued := w.Issued()
		if len(issued) != 2 || issued[1].Accessor != acc.Accessor || issued[0].Accessor == issued[1].Accessor {
			t.Errorf("issued tokens %+v, want two, the second held by the access (%s)", issued, acc.Accessor)
		}
	})
}

// TestOperatorAccessListsTheServersThatHaveAPublicAddress leaves a server without a public address out of the
// addresses, and a client out of them too.
func TestOperatorAccessListsTheServersThatHaveAPublicAddress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, _ := builtWorld(t)
		if err := f.DeleteInstance(t.Context(), "instance-3"); err != nil {
			t.Fatal(err)
		}
		seedServer(t, f, "prod-servers-2", "")

		acc := mustAccess(t, svc)

		if diff := cmp.Diff(apiAddresses(t, f, "prod-servers-0", "prod-servers-1"), acc.Servers); diff != "" {
			t.Errorf("servers (-want +got):\n%s", diff)
		}
	})
}

// reversedProvider is a provider whose Nodes list the machines in the reverse of its order.
type reversedProvider struct{ cloud.Provider }

func (p reversedProvider) Nodes() cloud.Nodes { return reversedNodes{p.Provider.Nodes()} }

type reversedNodes struct{ cloud.Nodes }

func (n reversedNodes) List(ctx context.Context, cluster string) ([]cloud.Instance, error) {
	machines, err := n.Nodes.List(ctx, cluster)
	slices.Reverse(machines)
	return machines, err
}

// TestOperatorAccessOrdersTheServersByName lists the servers by name, whatever the order in which the cloud lists them.
func TestOperatorAccessOrdersTheServersByName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _, _ := builtWorld(t)
		providers := svc.Providers
		svc.Providers = func(name v1alpha1.Provider) (cloud.Provider, error) {
			p, err := providers(name)
			return reversedProvider{p}, err
		}

		acc := mustAccess(t, svc)

		want := apiAddresses(t, f, "prod-servers-0", "prod-servers-1", "prod-servers-2")
		if diff := cmp.Diff(want, acc.Servers); diff != "" {
			t.Errorf("servers (-want +got):\n%s", diff)
		}
	})
}

// TestOperatorAccessReachesCombinedServers lists the machines of a combined group as the servers.
func TestOperatorAccessReachesCombinedServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, _ := newRelease(t, clusterYAML, combinedYAML)
		mustUpdate(t, svc)

		acc := mustAccess(t, svc)

		if diff := cmp.Diff(apiAddresses(t, f, "prod-all-0", "prod-all-1", "prod-all-2"), acc.Servers); diff != "" {
			t.Errorf("servers (-want +got):\n%s", diff)
		}
	})
}

// TestOperatorAccessDoesNotValidateTheSpecs gives the access of a cluster whose specs an update would refuse now.
func TestOperatorAccessDoesNotValidateTheSpecs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := newRelease(t, keyedClusterYAML, edit(t, serversYAML, "size: 3", "size: 1"), workersYAML)
		mustUpdate(t, svc)
		svc.Validate.AllowSingleServer = false

		if _, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL); err != nil {
			t.Errorf("OperatorAccess: %v", err)
		}
	})
}

// TestOperatorAccessTakesNoLock gives the access while another tent holds the cluster's lock, and leaves its lease as
// it was.
func TestOperatorAccessTakesNoLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		held := holdLock(t, svc.Store)
		lease := held.Lease()

		mustAccess(t, svc)

		if got := holder(t, svc.Store); got == nil || got.ID != lease.ID || !got.ExpiresAt.Equal(lease.ExpiresAt) {
			t.Errorf("Holder = %+v, want the lease that was held, %+v", got, lease)
		}
	})
}

// TestOperatorAccessEndsWithTheEarlierEnd takes the time from the service's clock: the certificate starts and ends by
// it, and the access ends with the earlier of the certificate and the token, whose end Nomad's clock sets.
func TestOperatorAccessEndsWithTheEarlierEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration // of the service's clock from Nomad's
		token  bool          // the token ends first
	}{
		{"the certificate ends first", -time.Hour + 500*time.Millisecond, false},
		{"the token ends first", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, _, _ := builtWorld(t)
				time.Sleep(3 * time.Hour) // the service's clock must not be before the CA's start
				svc.Now = func() time.Time { return time.Now().Add(tc.offset) }
				nomadEnd, certEnd := time.Now().Add(accessTTL), time.Now().Add(tc.offset+accessTTL)

				acc := mustAccess(t, svc)

				want := certEnd.Truncate(time.Second) // a certificate holds its end in whole seconds
				if tc.token {
					want = nomadEnd
				}
				if !acc.Until.Equal(want) {
					t.Errorf("Until = %v, want %v", acc.Until, want)
				}
				wantOperatorCertificate(t, acc, time.Now().Add(tc.offset), certEnd)
			})
		})
	}
}

// TestOperatorAccessEndsWithTheCA ends the access with the CA when the CA ends before the TTL: the certificate never
// outlives its CA.
func TestOperatorAccessEndsWithTheCA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		caEnd := storedCA(t, svc.Store).Certificate().NotAfter
		time.Sleep(time.Until(caEnd.Add(-time.Hour)))

		acc, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", 24*time.Hour)
		if err != nil {
			t.Fatalf("OperatorAccess: %v", err)
		}

		if end := certOf(t, acc).NotAfter; !end.Equal(caEnd) || !acc.Until.Equal(caEnd) {
			t.Errorf("the certificate ends %v and the access %v, want both at the CA's end %v", end, acc.Until, caEnd)
		}
	})
}

// TestOperatorAccessNamesWhatIsMissing fails with what the store lacks, before any call of the cloud or Nomad.
func TestOperatorAccessNamesWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove []string
		want   string
	}{
		{
			"the CA", []string{caKeyPath, caBundlePath},
			"cluster prod: the state store lacks pki/private/ca.key and pki/ca-bundle.pem; run tent update cluster --yes first",
		},
		{
			"the gossip key", []string{gossipPath},
			"cluster prod: the state store lacks secrets/gossip.key; run tent update cluster --yes first",
		},
		{
			"the mark of the bootstrap", []string{markPath},
			"cluster prod: Nomad is not bootstrapped yet (prod/nomad/bootstrapped is missing); " +
				"run tent update cluster --yes first",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				secrets := storedSecretBytes(t, svc)
				for _, p := range tc.remove {
					if err := svc.Store.Delete(t.Context(), p); err != nil {
						t.Fatal(err)
					}
				}
				before, cloudCalls, nomadCalls := snapshot(t, svc.Store), len(f.Calls()), len(w.Log())

				_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

				wantAccessError(t, err, tc.want, secrets)
				wantSnapshot(t, svc.Store, before)
				if len(f.Calls()) != cloudCalls || len(w.Log()) != nomadCalls {
					t.Errorf("%d calls of the cloud and %d of Nomad, want none", len(f.Calls())-cloudCalls,
						len(w.Log())-nomadCalls)
				}
			})
		})
	}
}

// TestOperatorAccessFailsForAStoredSecretThatDoesNotLoad fails as an update does, names the path and shows nothing of
// the secret.
func TestOperatorAccessFailsForAStoredSecretThatDoesNotLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		secrets := storedSecretBytes(t, svc)
		put(t, svc.Store, gossipPath, []byte("not a gossip key, a tale of woe\n"))
		_, updateErr := svc.Update(t.Context(), "prod", false)
		if updateErr == nil {
			t.Fatal("an update with a bad gossip key must fail")
		}

		_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

		wantAccessError(t, err, updateErr.Error(), secrets)
		if !strings.Contains(err.Error(), gossipPath) {
			t.Errorf("error %v, want the path %s", err, gossipPath)
		}
	})
}

// TestOperatorAccessNeedsServerMachines fails for a cluster whose servers are gone, and for one whose servers have
// no public address, before any call of Nomad.
func TestOperatorAccessNeedsServerMachines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		public string // the public address of the servers that replace the deleted ones; "" for none, "-" for no server
		want   string
	}{
		{"no machine", "-", "cluster prod has no server machine; run tent update cluster --yes first"},
		{"no public address", "", "cluster prod: no server has a public address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				secrets := storedSecretBytes(t, svc)
				for _, id := range []string{"instance-1", "instance-2", "instance-3"} {
					if err := f.DeleteInstance(t.Context(), id); err != nil {
						t.Fatal(err)
					}
				}
				if tc.public != "-" {
					seedServer(t, f, "prod-servers-0", tc.public)
				}
				nomadBefore := len(w.Log())

				_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

				wantAccessError(t, err, tc.want, secrets)
				if n := len(w.Log()) - nomadBefore; n != 0 {
					t.Errorf("%d calls of Nomad, want none", n)
				}
			})
		})
	}
}

// TestOperatorAccessRefusesATTLThatIsNotAboveZero fails before any call of the cloud or Nomad.
func TestOperatorAccessRefusesATTLThatIsNotAboveZero(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f, w, _ := builtWorld(t)
				cloudCalls, nomadBefore := len(f.Calls()), len(w.Log())

				_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", ttl)

				wantError(t, err, "operator access: TTL "+ttl.String()+" is not above zero")
				if len(f.Calls()) != cloudCalls || len(w.Log()) != nomadBefore {
					t.Errorf("%d calls of the cloud and %d of Nomad, want none", len(f.Calls())-cloudCalls, len(w.Log())-nomadBefore)
				}
			})
		})
	}
}

// TestOperatorAccessSaysNomadsWordsForATTLOutOfItsLimits fails with Nomad's own text, after one call.
func TestOperatorAccessSaysNomadsWordsForATTLOutOfItsLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		nomadBefore := len(w.Log())

		_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", 48*time.Hour)

		const nomad = "token 0 invalid: 1 error occurred: * expiration time cannot be more than 24h0m0s in the future " +
			"(was 48h0m0s)"
		if err == nil || !strings.HasPrefix(err.Error(), "create the operator token: ") ||
			!strings.HasSuffix(err.Error(), nomad) {
			t.Errorf("error = %v, want the prefix %q and Nomad's text %q", err, "create the operator token: ", nomad)
		}
		if names := nomadCallNames(w, nomadBefore); !slices.Equal(names, []string{"CreateToken"}) {
			t.Errorf("calls of Nomad = %v, want one CreateToken", names)
		}
	})
}

// TestOperatorAccessNamesAFailedCall fails with the cause of a call that no other server answers differently, after
// one call and with no hint about the API's access.
func TestOperatorAccessNamesAFailedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		secrets := storedSecretBytes(t, svc)
		w.Fail(t, "CreateToken", errPermanent)
		nomadBefore := len(w.Log())

		acc, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

		wantAccessError(t, err, "create the operator token: "+errPermanent.Error(), secrets)
		if diff := cmp.Diff(app.Access{}, acc); diff != "" {
			t.Errorf("Access (-want +got):\n%s", diff)
		}
		if names := nomadCallNames(w, nomadBefore); !slices.Equal(names, []string{"CreateToken"}) || len(w.Issued()) != 0 {
			t.Errorf("calls of Nomad %v, %d tokens issued; want one CreateToken and no token", names, len(w.Issued()))
		}
	})
}

// TestOperatorAccessHintsAtTheAPIAccess tells the operator where to look when no server answers.
func TestOperatorAccessHintsAtTheAPIAccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		secrets := storedSecretBytes(t, svc)
		for range 3 {
			w.Fail(t, "CreateToken", errNotReady)
		}

		_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

		if err == nil || !errors.Is(err, nomadops.ErrNotReady) ||
			!strings.HasPrefix(err.Error(), "create the operator token: ") || !strings.HasSuffix(err.Error(), hintOfAccess) {
			t.Fatalf("error = %v, want one that matches ErrNotReady, starts with %q and ends with %q", err,
				"create the operator token: ", hintOfAccess)
		}
		wantAccessError(t, err, err.Error(), secrets)
	})
}

// TestOperatorAccessNeedsTheCluster fails for a cluster that the store does not hold, and writes nothing.
func TestOperatorAccessNeedsTheCluster(t *testing.T) {
	svc, root := newService(t)
	withCloud(svc)

	_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

	wantError(t, err, notFound(svc, "cluster prod"))
	wantNothingWritten(t, root)
}

// TestOperatorAccessChecksTheTentVersion fails for a cluster that a newer tent wrote.
func TestOperatorAccessChecksTheTentVersion(t *testing.T) {
	svc, _ := newService(t)
	mustCreate(t, svc, clusterYAML, serversYAML, workersYAML)
	withCloud(svc)
	put(t, svc.Store, versionPath, []byte("v0.9.0\n"))
	svc.Version = "v0.4.0"

	_, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

	wantError(t, err, "cluster prod needs tent v0.9.0 or newer; this is v0.4.0")
}

// TestOperatorAccessIsInterrupted says "interrupted" when the context ends during the call of Nomad.
func TestOperatorAccessIsInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w.SetHook(func(ctx context.Context, _ nomadfake.Call, _ func(context.Context) error) error {
			cancel()
			return ctx.Err()
		})

		_, err := svc.OperatorAccess(ctx, "prod", "export nomad", accessTTL)

		if err == nil || err.Error() != "interrupted" || !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want interrupted, matching context.Canceled", err)
		}
	})
}

// TestAccessPrintsNoSecret prints an access in every way and finds neither the token, the key nor any other secret of
// the cluster, and the JSON of an access holds the sizes of its two secrets.
func TestAccessPrintsNoSecret(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _, _ := builtWorld(t)
		acc := mustAccess(t, svc)

		b, err := json.Marshal(acc)
		if err != nil {
			t.Fatal(err)
		}

		if n := strings.Count(string(b), "[secret, "); n != 2 {
			t.Errorf("the JSON holds %d secrets' sizes, want 2: %s", n, b)
		}
		secrettest.CheckHidden(t, secrettest.Printed(t, acc), withAccessSecrets(storedSecretBytes(t, svc), acc), "[secret,")
	})
}

// TestOperatorAccessFailsWhenTheCloudDoesNotAnswer returns the error of a list that fails, before any call of Nomad: a
// cluster whose machines cannot be listed is not a cluster without servers.
func TestOperatorAccessFailsWhenTheCloudDoesNotAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f, w, _ := builtWorld(t)
		errDown := errors.New("the cloud is down")
		f.Fail(t, "ListInstances", errDown, 8)
		nomadBefore := len(w.Log())

		acc, err := svc.OperatorAccess(t.Context(), "prod", "export nomad", accessTTL)

		if !errors.Is(err, errDown) {
			t.Errorf("error = %v, want %v", err, errDown)
		}
		if diff := cmp.Diff(app.Access{}, acc); diff != "" {
			t.Errorf("Access (-want +got):\n%s", diff)
		}
		if n := len(w.Log()) - nomadBefore; n != 0 {
			t.Errorf("%d calls of Nomad, want none", n)
		}
	})
}

// TestOperatorAccessNamesTheTokenAfterThePurpose puts the purpose that the caller gives into the token's name.
func TestOperatorAccessNamesTheTokenAfterThePurpose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, w, _ := builtWorld(t)

		if _, err := svc.OperatorAccess(t.Context(), "prod", "ui", accessTTL); err != nil {
			t.Fatalf("OperatorAccess: %v", err)
		}

		if issued := w.Issued(); len(issued) != 1 || issued[0].Name != tokenName("ui") {
			t.Errorf("issued tokens %+v, want one called %q", issued, tokenName("ui"))
		}
	})
}

// TestOperatorAccessUsesTheNomadRegionOfTheSpecs names the certificate and the access after the cluster's Nomad region.
func TestOperatorAccessUsesTheNomadRegionOfTheSpecs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, _ := newRelease(t, edit(t, keyedClusterYAML, "region: ams", "region: ams\n  nomad:\n    region: eu"),
			serversYAML, workersYAML)
		mustUpdate(t, svc)

		acc := mustAccess(t, svc)

		if diff := cmp.Diff([]string{"cli.eu.nomad"}, certOf(t, acc).DNSNames); diff != "" || acc.Region != "eu" {
			t.Errorf("region %q, want eu; DNS names (-want +got):\n%s", acc.Region, diff)
		}
	})
}
