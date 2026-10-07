package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testServerName = "server.global.nomad"
	testToken      = "7f1c0de5-token-for-tests"
)

// testCA is a certificate authority made for one test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the CA certificate: %v", err)
	}
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate of the CA for a server or a client, in PEM, with its key.
func (ca testCA) issue(t *testing.T, serial int64, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{testServerName},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal a key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// startNomad starts a TLS server that wants a client certificate of the CA, like Nomad's API, and returns the
// export that reaches it.
func startNomad(t *testing.T, ca testCA, handler http.HandlerFunc) exportInfo {
	t.Helper()
	serverCert, serverKey := ca.issue(t, 2, x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return writeExport(t, ca, srv.URL, testServerName)
}

// writeExport writes the CA, a client key pair and the token under t.TempDir() and returns the export that names
// them. The token file ends with a newline and spaces.
func writeExport(t *testing.T, ca testCA, address, serverName string) exportInfo {
	t.Helper()
	dir := t.TempDir()
	clientCert, clientKey := ca.issue(t, 3, x509.ExtKeyUsageClientAuth)
	x := exportInfo{
		Address:       address,
		CACert:        filepath.Join(dir, "ca.pem"),
		ClientCert:    filepath.Join(dir, "client.pem"),
		ClientKey:     filepath.Join(dir, "client.key"),
		TLSServerName: serverName,
		TokenFile:     filepath.Join(dir, "token"),
	}
	for path, data := range map[string][]byte{
		x.CACert:     ca.pem,
		x.ClientCert: clientCert,
		x.ClientKey:  clientKey,
		x.TokenFile:  []byte(testToken + " \n"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return x
}

func TestNomadGetSendsTheTrimmedTokenAndDecodes(t *testing.T) {
	var gotToken, gotMethod, gotURI string
	x := startNomad(t, newTestCA(t, "ca"), func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotMethod, gotURI = r.Header.Get("X-Nomad-Token"), r.Method, r.URL.RequestURI()
		_, _ = io.WriteString(w, `{"Name":"web"}`)
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	var out struct{ Name string }
	if err := n.get(context.Background(), "/v1/job/web?namespace=default", &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Name != "web" {
		t.Errorf("decoded %+v, want Name web", out)
	}
	if gotToken != testToken {
		t.Errorf("the token header = %q, want the token without the whitespace of its file", gotToken)
	}
	if gotMethod != http.MethodGet || gotURI != "/v1/job/web?namespace=default" {
		t.Errorf("request = %s %s", gotMethod, gotURI)
	}
}

func TestNomadPostSendsTheBodyAndDecodesTheAnswer(t *testing.T) {
	var gotToken, gotMethod, gotType string
	var gotBody map[string]string
	x := startNomad(t, newTestCA(t, "ca"), func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotMethod, gotType = r.Header.Get("X-Nomad-Token"), r.Method, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"EvalID":"e1"}`)
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	var out struct{ EvalID string }
	if err := n.post(context.Background(), "/v1/jobs", map[string]string{"Job": "web"}, &out); err != nil {
		t.Fatalf("post: %v", err)
	}
	if out.EvalID != "e1" || gotBody["Job"] != "web" {
		t.Errorf("answer %+v, body %v", out, gotBody)
	}
	if gotToken != testToken || gotMethod != http.MethodPost || gotType != "application/json" {
		t.Errorf("token %q, method %s, content type %q", gotToken, gotMethod, gotType)
	}
	if err := n.post(context.Background(), "/v1/jobs", map[string]string{"Job": "web"}, nil); err != nil {
		t.Errorf("post without an answer to decode: %v", err)
	}
}

func TestNomadDelSendsTheTokenAndAcceptsAnEmptyAnswer(t *testing.T) {
	var gotToken, gotMethod string
	x := startNomad(t, newTestCA(t, "ca"), func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotMethod = r.Header.Get("X-Nomad-Token"), r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	if err := n.del(context.Background(), "/v1/job/web?purge=true"); err != nil {
		t.Fatalf("del: %v", err)
	}
	if gotToken != testToken || gotMethod != http.MethodDelete {
		t.Errorf("token %q, method %s", gotToken, gotMethod)
	}
}

func TestNomadErrorsNameTheRequestAndTheStatusAndNeverHoldTheToken(t *testing.T) {
	var sawToken []string
	ca := newTestCA(t, "ca")
	x := startNomad(t, ca, func(w http.ResponseWriter, r *http.Request) {
		sawToken = append(sawToken, r.Header.Get("X-Nomad-Token"))
		switch r.URL.Path {
		case "/v1/jobs":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/v1/long":
			http.Error(w, strings.Repeat("x", 600), http.StatusBadGateway)
		case "/v1/unchanged":
			w.WriteHeader(http.StatusNotModified)
		case "/v1/garbage":
			_, _ = io.WriteString(w, "not json")
		default:
			http.Error(w, "gone", http.StatusNotFound)
		}
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	ctx := context.Background()
	var out any
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"status and body", n.get(ctx, "/v1/jobs", &out), "nomad: GET /v1/jobs: 500: boom"},
		{"a long body is cut", n.get(ctx, "/v1/long", &out), "nomad: GET /v1/long: 502: " + strings.Repeat("x", 512)},
		{"a status of 3xx", n.get(ctx, "/v1/unchanged", &out), "nomad: GET /v1/unchanged: 304: "},
		{"post", n.post(ctx, "/v1/other", nil, nil), "nomad: POST /v1/other: 404: gone"},
		{"delete", n.del(ctx, "/v1/other"), "nomad: DELETE /v1/other: 404: gone"},
	}
	for _, tt := range cases {
		if tt.err == nil || tt.err.Error() != tt.want {
			t.Errorf("%s: error = %v, want %q", tt.name, tt.err, tt.want)
		}
	}
	decodeErr := n.get(ctx, "/v1/garbage", &out)
	if decodeErr == nil || !strings.HasPrefix(decodeErr.Error(), "nomad: GET /v1/garbage: decode: ") {
		t.Errorf("a body that is not JSON gave %v", decodeErr)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cancelErr := n.get(cancelled, "/v1/jobs", &out)
	if cancelErr == nil || !strings.HasPrefix(cancelErr.Error(), "nomad: GET /v1/jobs: ") {
		t.Errorf("a cancelled context gave %v", cancelErr)
	}
	errs := []error{decodeErr, cancelErr}
	for _, tt := range cases {
		errs = append(errs, tt.err)
	}
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("error %q holds the token", err)
		}
	}
	for _, tok := range sawToken {
		if tok != testToken {
			t.Errorf("a request sent the token %q, want the one of the file", tok)
		}
	}
	if len(sawToken) != 6 {
		t.Errorf("the server saw %d requests with the token, want 6", len(sawToken))
	}
}

func TestNomadFollowsNoRedirect(t *testing.T) {
	var other atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	t.Cleanup(elsewhere.Close)
	x := startNomad(t, newTestCA(t, "ca"), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	var out any
	err = n.get(context.Background(), "/v1/jobs", &out)
	if err == nil || !strings.HasPrefix(err.Error(), "nomad: GET /v1/jobs: 307: ") {
		t.Errorf("error = %v, want the redirect as an error", err)
	}
	if other.Load() != 0 {
		t.Errorf("the client followed the redirect: the other server saw %d requests", other.Load())
	}
}

func TestNomadChecksTheServerByCAAndName(t *testing.T) {
	ca := newTestCA(t, "ca")
	x := startNomad(t, ca, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	var out any

	wrongName := x
	wrongName.TLSServerName = "server.other.nomad"
	n, err := newNomad(wrongName)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	if err := n.get(context.Background(), "/v1/jobs", &out); err == nil {
		t.Error("a server name that the certificate lacks was accepted")
	}

	other := newTestCA(t, "other")
	wrongCA := writeExport(t, other, x.Address, testServerName)
	n, err = newNomad(wrongCA)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	if err := n.get(context.Background(), "/v1/jobs", &out); err == nil {
		t.Error("a server certificate of another CA was accepted")
	}

	n, err = newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	if err := n.get(context.Background(), "/v1/jobs", &out); err != nil {
		t.Errorf("the right name and CA failed: %v", err)
	}
}

func TestNomadPresentsTheClientCertificate(t *testing.T) {
	ca := newTestCA(t, "ca")
	var peers int
	x := startNomad(t, ca, func(w http.ResponseWriter, r *http.Request) {
		peers = len(r.TLS.PeerCertificates)
		_, _ = io.WriteString(w, "{}")
	})
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	var out any
	if err := n.get(context.Background(), "/v1/jobs", &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if peers != 1 {
		t.Errorf("the server saw %d client certificates, want 1", peers)
	}
}

func TestNomadRequiresTLS12OrNewer(t *testing.T) {
	x := writeExport(t, newTestCA(t, "ca"), "https://127.0.0.1:1", testServerName)
	n, err := newNomad(x)
	if err != nil {
		t.Fatalf("newNomad: %v", err)
	}
	transport, ok := n.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the transport is a %T", n.client.Transport)
	}
	if got := transport.TLSClientConfig.MinVersion; got != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", got, tls.VersionTLS12)
	}
	if got := transport.TLSClientConfig.ServerName; got != testServerName {
		t.Errorf("ServerName = %q, want %q", got, testServerName)
	}
}

func TestNewNomadReportsFilesThatCannotBeUsed(t *testing.T) {
	ca := newTestCA(t, "ca")
	tests := []struct {
		name   string
		damage func(x exportInfo) exportInfo
		want   string
	}{
		{"no CA file", func(x exportInfo) exportInfo { x.CACert += ".missing"; return x }, "CA"},
		{"a CA file without a certificate", func(x exportInfo) exportInfo {
			mustWrite(t, x.CACert, "junk")
			return x
		}, "CA"},
		{"no client certificate", func(x exportInfo) exportInfo { x.ClientCert += ".missing"; return x }, "client key pair"},
		{"a client key that does not fit", func(x exportInfo) exportInfo {
			mustWrite(t, x.ClientKey, "junk")
			return x
		}, "client key pair"},
		{"no token file", func(x exportInfo) exportInfo { x.TokenFile += ".missing"; return x }, "token"},
		{"an empty token file", func(x exportInfo) exportInfo {
			mustWrite(t, x.TokenFile, " \n")
			return x
		}, "token file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := tt.damage(writeExport(t, ca, "https://127.0.0.1:1", testServerName))
			_, err := newNomad(x)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one holding %q", err, tt.want)
			}
		})
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
