package nomadops_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// snapshotRequest is the request of SaveSnapshot.
var snapshotRequest = gotRequest{Method: http.MethodGet, Path: "/v1/operator/snapshot", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad"}

// restoreRequest is the request of RestoreSnapshot: its body is the snapshot, which checkRequests names.
var restoreRequest = gotRequest{Method: http.MethodPut, Path: "/v1/operator/snapshot", Query: "region=" + region,
	Token: clientToken, Peer: "cli." + region + ".nomad", Body: "<" + snapshotName + ">"}

// snapshotName names the snapshot for checkRequests and the checks of what a call shows.
const snapshotName = "the snapshot"

// testSnapshot returns a snapshot of 300 bytes that starts as a gzip file does and holds every byte value, so that a
// call that changes the bytes in any way shows.
func testSnapshot() secret.Secret {
	snap := []byte{0x1f, 0x8b, 0x08, 0x00}
	for i := range 296 {
		snap = append(snap, byte(i*7))
	}
	return snap
}

// snapshotSecrets names the client's token and the snapshot for checkRequests and the checks of what a call shows.
func snapshotSecrets(token, snap secret.Secret) map[string]secret.Secret {
	return map[string]secret.Secret{clientToken: token, snapshotName: snap}
}

// digestOf returns the Digest header that Nomad sends for body.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=" + base64.StdEncoding.EncodeToString(sum[:])
}

// snapshotAnswer returns a handler that answers as Nomad does for a snapshot: a gzip file with the digest in a header,
// and no Content-Encoding.
func snapshotAnswer(snap []byte) http.HandlerFunc {
	return snapshotAnswerWithDigest(snap, digestOf(snap))
}

// snapshotAnswerWithDigest is snapshotAnswer with the Digest header digest; an empty digest sends no header.
func snapshotAnswerWithDigest(snap []byte, digest string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-gzip")
		if digest != "" {
			w.Header().Set("Digest", digest)
		}
		_, _ = w.Write(snap)
	}
}

// TestSaveSnapshotReadsALargeSnapshotWithinTheCall checks that a snapshot larger than what the transport reads ahead
// comes back whole: its body is read before the call's context ends.
func TestSaveSnapshotReadsALargeSnapshotWithinTheCall(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	snap := secret.Secret(bytes.Repeat(testSnapshot(), 4000))
	srv := newNomadServer(t, p.server, p.ca.Bundle(), snapshotAnswer(snap))

	got, err := newClient(t, srv, p, token).SaveSnapshot(t.Context())

	if err != nil || !bytes.Equal(got, snap) {
		t.Errorf("SaveSnapshot() = %d bytes, %s; want the %d that Nomad sent", len(got),
			show(t, err, snapshotSecrets(token, snap)), len(snap))
	}
}

func TestSaveSnapshotOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	snap := testSnapshot()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), snapshotAnswer(snap))

	got, err := newClient(t, srv, p, token).SaveSnapshot(t.Context())

	if err != nil {
		t.Fatalf("SaveSnapshot: %s", show(t, err, snapshotSecrets(token, snap)))
	}
	if !bytes.Equal(got, snap) {
		t.Errorf("SaveSnapshot() = %d bytes that differ from the %d that Nomad sent", len(got), len(snap))
	}
	checkRequests(t, srv, snapshotSecrets(token, snap), snapshotRequest)
	secrets := map[string][]byte{snapshotName: snap}
	secrettest.CheckHidden(t, secrettest.Printed(t, got), secrets, got.String())
}

// TestSaveSnapshotRefusesAnAnswerItCannotTrust checks that a snapshot with another digest than the header's, with no
// digest or with one of a kind that the client does not know is a permanent error that returns no bytes, and that an
// answer that breaks off is an error that may succeed later, not a shorter snapshot.
func TestSaveSnapshotRefusesAnAnswerItCannotTrust(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	snap := testSnapshot()
	const prefix = "nomad: GET /v1/operator/snapshot: "
	cut := func(w http.ResponseWriter, _ *http.Request) { // the connection ends before the whole body came
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Header().Set("Digest", digestOf(snap))
		w.Header().Set("Content-Length", strconv.Itoa(len(snap)))
		_, _ = w.Write(snap[:100])
	}
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		want     string
		notReady bool
	}{
		{"a digest of other bytes", snapshotAnswerWithDigest(snap, digestOf(snap[1:])), prefix + "mismatch checksum",
			false},
		{"no digest", snapshotAnswerWithDigest(snap, ""), prefix + "invalid digest format", false},
		{"a digest of an unknown kind", snapshotAnswerWithDigest(snap, "md5=AAAA"),
			prefix + "unsupported checksum format", false},
		{"an answer that breaks off", cut, prefix + "unexpected EOF", true},
		{"an empty answer with its digest", snapshotAnswerWithDigest([]byte{}, digestOf([]byte{})),
			prefix + "no snapshot in the answer", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newNomadServer(t, p.server, p.ca.Bundle(), tc.handler)

			got, err := newClient(t, srv, p, token).SaveSnapshot(t.Context())

			checkCallErr(t, err, tc.want, tc.notReady, snapshotSecrets(token, snap))
			if got != nil {
				t.Errorf("SaveSnapshot() = %d bytes with the error, want none", len(got))
			}
		})
	}
}

// snapshotCall is SaveSnapshot or RestoreSnapshot, with the request that it makes.
type snapshotCall struct {
	name, method string
	do           func(context.Context, nomadops.API) error
}

// snapshotCalls are SaveSnapshot and RestoreSnapshot of a snapshot.
var snapshotCalls = []snapshotCall{
	{"SaveSnapshot", http.MethodGet, func(ctx context.Context, a nomadops.API) error {
		got, err := a.SaveSnapshot(ctx)
		if err != nil && got != nil {
			return fmt.Errorf("SaveSnapshot() = %d bytes with the error %w, want none", len(got), err)
		}
		return err
	}},
	{"RestoreSnapshot", http.MethodPut, func(ctx context.Context, a nomadops.API) error {
		return a.RestoreSnapshot(ctx, testSnapshot())
	}},
}

// snapshotPath is the path of both snapshot calls.
const snapshotPath = "/v1/operator/snapshot"

// TestSnapshotCallErrors checks the class of each answer that a snapshot call can get. A snapshot that Nomad cannot
// take or restore now is an error that may succeed later, like every 500.
func TestSnapshotCallErrors(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	const taking = "Raft error when taking snapshot: cannot take snapshot now, wait until the configuration entry at " +
		"78 has been applied (have applied 77)"
	const restoring = "failed to restore from snapshot: failed to decompress snapshot: gzip: invalid header"
	cases := []struct {
		name     string
		status   int
		body     string
		notReady bool
	}{
		{"the configuration entry is not applied", http.StatusInternalServerError, taking, true},
		{"a snapshot that Nomad refuses", http.StatusInternalServerError, restoring, true},
		{"no leader", http.StatusInternalServerError, "No cluster leader", true},
		{"a server that is not ready", http.StatusServiceUnavailable, "starting", true},
		{"rate limited", http.StatusTooManyRequests, "slow down", true},
		{"a gone node's text", http.StatusInternalServerError, "node not found", true},
		{"no permission", http.StatusForbidden, "Permission denied", false},
		{"a bad request", http.StatusBadRequest, "bad", false},
		{"not found", http.StatusNotFound, "not found", false},
	}
	snap := testSnapshot()
	for _, c := range snapshotCalls {
		for _, tc := range cases {
			t.Run(c.name+" "+tc.name, func(t *testing.T) {
				srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(tc.status, tc.body))

				err := c.do(t.Context(), newClient(t, srv, p, token))

				want := "nomad: " + c.method + " " + snapshotPath + ": " + strconv.Itoa(tc.status) + ": " + tc.body
				checkCallErr(t, err, want, tc.notReady, snapshotSecrets(token, snap))
				if errors.Is(err, nomadops.ErrGone) {
					t.Errorf("error = %v matches ErrGone, want no gone class for a snapshot", err)
				}
				if n := len(srv.requests()); n != 1 {
					t.Errorf("the server got %d requests, want 1", n)
				}
			})
		}
	}
}

func TestRestoreSnapshotOverMTLS(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	snap := testSnapshot()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))

	err := newClient(t, srv, p, token).RestoreSnapshot(t.Context(), snap)

	if err != nil {
		t.Fatalf("RestoreSnapshot: %s", show(t, err, snapshotSecrets(token, snap)))
	}
	checkRequests(t, srv, snapshotSecrets(token, snap), restoreRequest)
}

// TestRestoreSnapshotRefusesAnEmptySnapshotBeforeAnyRequest checks that no snapshot at all fails without a request:
// Nomad would answer 500 and the caller could take it for a snapshot that is bad now.
func TestRestoreSnapshotRefusesAnEmptySnapshotBeforeAnyRequest(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))
	c := newClient(t, srv, p, token)
	for name, snap := range map[string]secret.Secret{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			checkCallErr(t, c.RestoreSnapshot(t.Context(), snap), "nomad: no snapshot", false,
				clientTokens(token))
		})
	}
	checkRequests(t, srv, clientTokens(token))
}

// TestSnapshotCallsStopWhenTheContextEnds checks that the end of the caller's context ends each call.
func TestSnapshotCallsStopWhenTheContextEnds(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	for _, c := range snapshotCalls {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan struct{})
			srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, got))
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-got
				cancel()
			}()

			err := c.do(ctx, newClient(t, srv, p, token))

			checkCallErr(t, err, "nomad: "+c.method+" "+snapshotPath+": context canceled", false,
				snapshotSecrets(token, testSnapshot()))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want one that matches context.Canceled", err)
			}
		})
	}
}

// TestSnapshotCallsHaveTheirOwnTimeLimit checks that a server that does not answer ends each snapshot call at the
// snapshot limit and a call of another method at the limit of the other calls.
func TestSnapshotCallsHaveTheirOwnTimeLimit(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	srv := newNomadServer(t, p.server, p.ca.Bundle(), blockingHandler(t, nil))
	cl := newClient(t, srv, p, token)
	cl.SetTimeout(30 * time.Millisecond)
	cl.SetSnapshotTimeout(80 * time.Millisecond)

	for _, c := range snapshotCalls {
		t.Run(c.name, func(t *testing.T) {
			err := c.do(t.Context(), cl)

			checkCallErr(t, err, "nomad: "+c.method+" "+snapshotPath+": no answer within 80ms", true,
				snapshotSecrets(token, testSnapshot()))
		})
	}
	t.Run("Leader", func(t *testing.T) {
		_, err := cl.Leader(t.Context())

		checkCallErr(t, err, "nomad: GET /v1/status/leader: no answer within 30ms", true, clientTokens(token))
	})
}

// TestSnapshotCallsOutlastTheLimitOfOtherCalls checks that a snapshot call that takes longer than the limit of the
// other calls, but not longer than its own, succeeds.
func TestSnapshotCallsOutlastTheLimitOfOtherCalls(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	token := pki.NewBootstrapSecret()
	snap := testSnapshot()
	save := snapshotAnswer(snap)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		if r.Method == http.MethodGet {
			save(w, r)
		}
	})
	cl := newClient(t, srv, p, token)
	cl.SetTimeout(30 * time.Millisecond)
	cl.SetSnapshotTimeout(time.Minute)

	got, err := cl.SaveSnapshot(t.Context())
	if err != nil || !bytes.Equal(got, snap) {
		t.Errorf("SaveSnapshot() = %d bytes, %s; want the snapshot", len(got), show(t, err, snapshotSecrets(token, snap)))
	}
	if err := cl.RestoreSnapshot(t.Context(), snap); err != nil {
		t.Errorf("RestoreSnapshot: %s", show(t, err, snapshotSecrets(token, snap)))
	}
}

// TestTheDefaultTimeLimits checks that a call has 30 seconds and a snapshot call 5 minutes.
func TestTheDefaultTimeLimits(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))

	call, snapshot := newClient(t, srv, p, pki.NewBootstrapSecret()).TimeLimits()

	if call != 30*time.Second || snapshot != 5*time.Minute {
		t.Errorf("time limits = %s and %s, want 30s and 5m0s", call, snapshot)
	}
}

// TestSetTimeoutSetsBothTimeLimits checks the helper of the other tests: a test that sets the time limit of the calls
// bounds a snapshot call too, so that no test waits for 5 minutes.
func TestSetTimeoutSetsBothTimeLimits(t *testing.T) {
	p := newPKI(t, v1alpha1.RoleServer, region)
	srv := newNomadServer(t, p.server, p.ca.Bundle(), answer(http.StatusOK, ""))
	c := newClient(t, srv, p, pki.NewBootstrapSecret())

	c.SetTimeout(time.Second)

	if call, snapshot := c.TimeLimits(); call != time.Second || snapshot != time.Second {
		t.Errorf("time limits = %s and %s, want 1s for both", call, snapshot)
	}
}
