package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/uuid"
)

// secretsOf returns the test cluster's secrets that the store s holds, by path.
func secretsOf(t *testing.T, s statestore.Store) map[string][]byte {
	t.Helper()
	secrets := map[string][]byte{}
	for _, p := range secretPaths {
		data, _, err := s.Get(t.Context(), p)
		switch {
		case errors.Is(err, statestore.ErrNotFound):
		case err != nil:
			t.Fatalf("get %s: %v", p, err)
		default:
			secrets[p] = data
		}
	}
	return secrets
}

// wantSecrets fails the test unless the store s holds exactly the secrets of want, byte for byte. Its messages show
// the paths, never the secrets.
func wantSecrets(t *testing.T, s statestore.Store, want map[string][]byte) {
	t.Helper()
	got := secretsOf(t, s)
	for _, p := range secretPaths {
		w, wok := want[p]
		g, gok := got[p]
		switch {
		case wok != gok:
			t.Errorf("%s is stored: %t, want %t", p, gok, wok)
		case !bytes.Equal(w, g):
			t.Errorf("%s changed", p)
		}
	}
}

// storedCA returns the test cluster's CA in the store s. It fails the test unless the bundle holds one certificate and
// the key matches it.
func storedCA(t *testing.T, s statestore.Store) *pki.CA {
	t.Helper()
	bundle := get(t, s, caBundlePath)
	if n := bytes.Count(bundle, []byte("-----BEGIN CERTIFICATE-----")); n != 1 {
		t.Errorf("the CA bundle holds %d certificates, want 1", n)
	}
	ca, err := pki.LoadCA(bundle, get(t, s, caKeyPath))
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	return ca
}

// putLog records each write to the store other than the lock's, as "put <path>", or "create <path>" for a put that
// creates only.
type putLog struct {
	statestore.Store
	mu   sync.Mutex
	puts []string
}

func (w *putLog) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	if p != lockPath {
		verb := "put "
		if opts.IfNoneMatch {
			verb = "create "
		}
		w.mu.Lock()
		w.puts = append(w.puts, verb+p)
		w.mu.Unlock()
	}
	return w.Store.Put(ctx, p, data, opts)
}

// TestUpdateWritesTheSecrets writes the CA's key and bundle, the gossip key and the ACL bootstrap secret, each only
// when it does not exist yet, after it raises the tent version and before it changes the infrastructure.
func TestUpdateWritesTheSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
		rec := &putLog{Store: svc.Store}
		svc.Store, svc.Version, svc.Now = rec, "v0.5.0", func() time.Time { return now }
		var first map[string][]byte // the secrets at the first progress event
		svc.OnProgress = func(app.Progress) {
			if first == nil {
				first = secretsOf(t, rec)
			}
		}

		plan := mustUpdate(t, svc)

		if diff := cmp.Diff(secretNames, plan.Secrets); diff != "" {
			t.Errorf("the plan's secrets (-want +got):\n%s", diff)
		}
		want := []string{
			"put " + versionPath, "create " + caKeyPath, "create " + caBundlePath, "create " + gossipPath,
			"create " + aclPath, "put " + completedPath,
		}
		if diff := cmp.Diff(want, rec.puts); diff != "" {
			t.Errorf("writes to the store (-want +got):\n%s", diff)
		}
		if len(first) != len(secretPaths) {
			t.Errorf("%d secrets are stored when the infrastructure starts, want %d", len(first), len(secretPaths))
		}
		cert := storedCA(t, svc.Store).Certificate()
		if cert.Subject.CommonName != "tent prod CA" {
			t.Errorf("the CA's subject is %s, want CN=tent prod CA", cert.Subject)
		}
		if want := now.Add(-5 * time.Minute); !cert.NotBefore.Equal(want) {
			t.Errorf("the CA is valid from %v, want %v", cert.NotBefore, want)
		}
		if want := now.AddDate(10, 0, 0); !cert.NotAfter.Equal(want) {
			t.Errorf("the CA is valid until %v, want %v", cert.NotAfter, want)
		}
		key, err := base64.StdEncoding.DecodeString(string(get(t, svc.Store, gossipPath)))
		if err != nil || len(key) != 32 {
			t.Errorf("the gossip key decodes to %d bytes (%v), want 32", len(key), err)
		}
		if !uuid.Valid(string(get(t, svc.Store, aclPath))) {
			t.Error("the ACL bootstrap secret is not a lower-case UUID v4")
		}
	})
}

// TestUpdateSignsTheCANow makes the CA valid from five minutes before the update when Now is not set.
func TestUpdateSignsTheCANow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		start := time.Now()
		mustUpdate(t, svc)
		end := time.Now()
		from := storedCA(t, svc.Store).Certificate().NotBefore
		if from.Before(start.Add(-5*time.Minute).Truncate(time.Second)) || from.After(end.Add(-5*time.Minute)) {
			t.Errorf("the CA is valid from %v, want five minutes before a time from %v to %v", from, start, end)
		}
	})
}

// TestUpdateKeepsTheSecrets plans no secrets once they are stored, and an update that changes the cluster leaves them
// byte for byte.
func TestUpdateKeepsTheSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		mustUpdate(t, svc)
		stored := secretsOf(t, svc.Store)
		mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))

		plan := mustUpdate(t, svc)

		if len(plan.Secrets) != 0 {
			t.Errorf("the plan writes the secrets %v, which are stored", plan.Secrets)
		}
		wantNodeChanges(t, plan, createOf("workers", "client", 2))
		wantSecrets(t, svc.Store, stored)
		wantConverged(t, svc)
	})
}

// TestUpdateCompletesTheCA signs a CA bundle for a stored CA key whose bundle was never written, and leaves the key
// and the other secrets as they are.
func TestUpdateCompletesTheCA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		mustUpdate(t, svc)
		if err := svc.Store.Delete(t.Context(), caBundlePath); err != nil {
			t.Fatal(err)
		}
		stored := secretsOf(t, svc.Store)
		before := len(f.Calls())
		now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
		rec := &putLog{Store: svc.Store}
		svc.Store, svc.Now = rec, func() time.Time { return now }

		plan := mustUpdate(t, svc)

		if got, want := planText(t, plan), "State: pki/ca-bundle.pem will be written.\n"; got != want {
			t.Errorf("the plan is\n%s\nwant\n%s", got, want)
		}
		if diff := cmp.Diff([]string{"create " + caBundlePath}, rec.puts); diff != "" {
			t.Errorf("writes to the store (-want +got):\n%s", diff)
		}
		wantNoWrites(t, callsSince(f, before))
		cert := storedCA(t, svc.Store).Certificate()
		if want := now.Add(-5 * time.Minute); !cert.NotBefore.Equal(want) {
			t.Errorf("the CA is valid from %v, want %v", cert.NotBefore, want)
		}
		stored[caBundlePath] = get(t, svc.Store, caBundlePath)
		wantSecrets(t, svc.Store, stored)
		wantConverged(t, svc)
	})
}

// TestUpdateRefusesBrokenSecrets fails the plan, with and without apply, when a stored secret cannot be used: tent
// never replaces a cluster's secrets. It writes nothing and changes nothing in the cloud.
func TestUpdateRefusesBrokenSecrets(t *testing.T) {
	otherCA, err := pki.NewCA("prod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, s statestore.Store)
		want  string
	}{
		{
			"a bundle without its key",
			func(t *testing.T, s statestore.Store) {
				if err := s.Delete(t.Context(), caKeyPath); err != nil {
					t.Fatal(err)
				}
			},
			"the CA key prod/pki/private/ca.key is missing, but the CA bundle prod/pki/ca-bundle.pem is there; " +
				"restore the key: tent never replaces a cluster's CA",
		},
		{
			"a key that matches no certificate of the bundle",
			func(t *testing.T, s statestore.Store) { put(t, s, caKeyPath, otherCA.Key().Bytes()) },
			"prod/pki/private/ca.key and prod/pki/ca-bundle.pem: CA key: matches no certificate of the CA bundle",
		},
		{
			"a bundle that is not PEM",
			func(t *testing.T, s statestore.Store) { put(t, s, caBundlePath, []byte("not PEM")) },
			"prod/pki/private/ca.key and prod/pki/ca-bundle.pem: CA bundle: no certificates",
		},
		{
			"a key without a bundle that is not a key",
			func(t *testing.T, s statestore.Store) {
				if err := s.Delete(t.Context(), caBundlePath); err != nil {
					t.Fatal(err)
				}
				put(t, s, caKeyPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}))
			},
			`prod/pki/private/ca.key: CA key: a PEM block of type "CERTIFICATE", not PRIVATE KEY`,
		},
		{
			"a gossip key of 16 bytes",
			func(t *testing.T, s statestore.Store) {
				put(t, s, gossipPath, []byte(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))))
			},
			"prod/secrets/gossip.key: gossip key: decodes to 16 bytes, not 32",
		},
		{
			"an ACL bootstrap secret in upper case",
			func(t *testing.T, s statestore.Store) {
				put(t, s, aclPath, bytes.ToUpper(get(t, s, aclPath)))
			},
			"prod/secrets/acl-bootstrap-token: ACL bootstrap secret: not a lower-case UUID of version 4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, f := newUpdate(t)
				mustUpdate(t, svc)
				tc.spoil(t, svc.Store)
				mustReplace(t, svc, edit(t, workersYAML, "size: 2", "size: 3"))
				stored := snapshot(t, svc.Store)
				before := len(f.Calls())
				rec := &writeLog{Store: svc.Store}
				svc.Store = rec
				for _, apply := range []bool{false, true} {
					_, err := svc.Update(t.Context(), "prod", apply)
					wantError(t, err, tc.want)
				}
				if len(rec.writes) != 0 {
					t.Errorf("writes to the store: %v", rec.writes)
				}
				wantSnapshot(t, svc.Store, stored)
				wantNoWrites(t, callsSince(f, before))
			})
		})
	}
}

// racingStore writes other to path just before the first put of path reaches the store, as another writer would. It
// records the secrets it receives.
type racingStore struct {
	statestore.Store
	sent
	path  string
	other []byte
	raced bool
}

func (s *racingStore) Put(ctx context.Context, p string, data []byte, opts statestore.PutOptions) (
	statestore.Version, error,
) {
	s.record(p, data)
	if p == s.path && !s.raced {
		s.raced = true
		if _, err := s.Store.Put(ctx, p, s.other, statestore.PutOptions{}); err != nil {
			return "", err
		}
	}
	return s.Store.Put(ctx, p, data, opts)
}

// TestUpdateKeepsASecretWrittenMeanwhile stops when a secret appears between the plan and its write, with an error
// that shows no secret, and keeps the secret that the other writer wrote. The next update keeps it too.
func TestUpdateKeepsASecretWrittenMeanwhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, f := newUpdate(t)
		other := pki.NewGossipKey().Bytes()
		store := svc.Store
		race := &racingStore{Store: store, path: gossipPath, other: other}
		svc.Store = race

		_, err := svc.Update(t.Context(), "prod", true)

		if err == nil {
			t.Fatal("Update succeeded, want the gossip key written meanwhile")
		}
		secrets := race.received(t, gossipPath)
		secrets["the other writer's "+gossipPath] = other
		if secrettest.CheckHidden(t, map[string]string{"the error": err.Error()}, secrets, ""); t.Failed() {
			t.FailNow() // the error would show a secret
		}
		wantError(t, err, gossipPath+" was written meanwhile; run the command again")
		if !bytes.Equal(get(t, store, gossipPath), other) {
			t.Error("the update replaced the gossip key that the other writer wrote")
		}
		if n := countCalls(f, "CreateVPC"); n != 0 {
			t.Errorf("the update went on to the infrastructure: %d VPC creates", n)
		}
		svc.Store = store
		mustUpdate(t, svc)
		if !bytes.Equal(get(t, store, gossipPath), other) {
			t.Error("the next update replaced the gossip key that the other writer wrote")
		}
		wantConverged(t, svc)
	})
}

// TestUpdateWritesTheSecretsWithoutConditionalPuts writes the secrets with plain puts to a store that cannot create
// only, under its best-effort lock.
func TestUpdateWritesTheSecretsWithoutConditionalPuts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newUpdate(t)
		rec := &putLog{Store: noConditions{svc.Store}}
		svc.Store = rec

		mustUpdate(t, svc)

		var puts []string
		for _, line := range rec.puts {
			if _, p, _ := strings.Cut(line, " "); isSecret(p) {
				puts = append(puts, line)
			}
		}
		want := []string{"put " + caKeyPath, "put " + caBundlePath, "put " + gossipPath, "put " + aclPath}
		if diff := cmp.Diff(want, puts); diff != "" {
			t.Errorf("the writes of the secrets (-want +got):\n%s", diff)
		}
		storedCA(t, svc.Store)
		wantConverged(t, svc)
	})
}

// wantSecretsNotIn fails the test unless the store s holds the test cluster's four secrets and text, named what,
// shows none of them. Its messages name the secret, never its content.
func wantSecretsNotIn(t *testing.T, s statestore.Store, what, text string) {
	t.Helper()
	secrets := secretsOf(t, s)
	if len(secrets) != len(secretPaths) {
		t.Fatalf("the store holds %d secrets, want %d", len(secrets), len(secretPaths))
	}
	secrettest.CheckHidden(t, map[string]string{what: text}, secrets, "")
}

// sent records the secrets that a store receives to write, by path, whether the store writes them or not.
type sent struct {
	mu      sync.Mutex
	secrets map[string][]byte
}

// record keeps data when p is the path of a secret.
func (s *sent) record(p string, data []byte) {
	if !isSecret(p) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string][]byte{}
	}
	s.secrets[p] = bytes.Clone(data)
}

// received returns the secrets that the store received, by path. It stops the test unless they include the one at
// path.
func (s *sent) received(t *testing.T, path string) map[string][]byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[path]; !ok {
		t.Fatalf("the store received no %s", path)
	}
	return maps.Clone(s.secrets)
}

// TestUpdateShowsNoSecrets writes the secrets and shows them in neither the plan, as text and as JSON, nor the
// progress, nor the provider's log.
func TestUpdateShowsNoSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _ := newService(t)
		mustCreate(t, svc, keyedClusterYAML, serversYAML, workersYAML)
		_, log := withCloud(svc)
		var shown strings.Builder
		svc.OnProgress = func(p app.Progress) { shown.WriteString(progressLine(p) + "\n") }
		svc.OnUpdatePlan = func(p app.UpdatePlan) error {
			shown.WriteString(planText(t, p) + encodeJSON(t, p, ""))
			return nil
		}

		plan := mustUpdate(t, svc)

		shown.WriteString(appliedText(t, plan) + encodeJSON(t, plan, "  "))
		if !strings.Contains(shown.String(), "secrets/gossip.key") {
			t.Fatalf("the plan does not name the secrets:\n%s", shown.String())
		}
		wantSecretsNotIn(t, svc.Store, "the output", shown.String())
		wantSecretsNotIn(t, svc.Store, "the provider's log", log.String())
	})
}
