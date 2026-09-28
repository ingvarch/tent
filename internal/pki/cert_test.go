package pki_test

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
)

var (
	serverAuth = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	clientAuth = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
)

func TestIssueNode(t *testing.T) {
	ca := newCA(t, now)
	at := now.Add(time.Hour)
	for _, tc := range []struct {
		role  v1alpha1.Role
		names []string // the Nomad names of the node
	}{
		{v1alpha1.RoleServer, []string{"server.europe.nomad"}},
		{v1alpha1.RoleClient, []string{"client.europe.nomad"}},
		{v1alpha1.RoleCombined, []string{"server.europe.nomad", "client.europe.nomad"}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			c, err := ca.IssueNode(tc.role, "europe", at)
			if err != nil {
				t.Fatalf("IssueNode: %v", err)
			}
			cert := checkLeaf(t, ca, c, append(tc.names, "localhost"), []string{"127.0.0.1"})
			checkValidity(t, cert, at.Add(-5*time.Minute), at.AddDate(1, 0, 0))
			if want := slices.Concat(serverAuth, clientAuth); !slices.Equal(cert.ExtKeyUsage, want) {
				t.Errorf("ExtKeyUsage = %v, want %v", cert.ExtKeyUsage, want)
			}
			for _, name := range append(tc.names, "localhost", "127.0.0.1") {
				for _, usage := range [][]x509.ExtKeyUsage{serverAuth, clientAuth} {
					if err := verify(t, ca, cert, name, usage, at); err != nil {
						t.Errorf("Verify(%s, %v): %v", name, usage, err)
					}
				}
			}
		})
	}
}

func TestIssueOperator(t *testing.T) {
	ca := newCA(t, now)
	at := now.Add(time.Hour)
	for _, ttl := range []time.Duration{24 * time.Hour, 90 * time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			c, err := ca.IssueOperator("europe", ttl, at)
			if err != nil {
				t.Fatalf("IssueOperator: %v", err)
			}
			cert := checkLeaf(t, ca, c, []string{"cli.europe.nomad"}, nil)
			checkValidity(t, cert, at.Add(-5*time.Minute), at.Add(ttl))
			if !slices.Equal(cert.ExtKeyUsage, clientAuth) {
				t.Errorf("ExtKeyUsage = %v, want %v", cert.ExtKeyUsage, clientAuth)
			}
			if err := verify(t, ca, cert, "cli.europe.nomad", clientAuth, at); err != nil {
				t.Errorf("Verify for client auth: %v", err)
			}
			if err := verify(t, ca, cert, "cli.europe.nomad", serverAuth, at); err == nil {
				t.Error("Verify for server auth succeeded, want an error")
			}
		})
	}
}

// TestIssueEndsWithTheCA checks that no certificate outlives the CA that issues it.
func TestIssueEndsWithTheCA(t *testing.T) {
	ca := newCA(t, now)
	end := ca.Certificate().NotAfter
	at := end.Add(-time.Hour)
	node, err := ca.IssueNode(v1alpha1.RoleServer, "global", at)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	checkValidity(t, parseCert(t, node.Cert), at.Add(-5*time.Minute), end)
	operator, err := ca.IssueOperator("global", 24*time.Hour, at)
	if err != nil {
		t.Fatalf("IssueOperator: %v", err)
	}
	checkValidity(t, parseCert(t, operator.Cert), at.Add(-5*time.Minute), end)
}

// TestIssueUsesTheActiveSigner checks that a CA loaded from a bundle of two issues with the certificate that matches
// its key, not the first of the bundle. The signer b ends an hour before a, so the end shows which one capped it.
func TestIssueUsesTheActiveSigner(t *testing.T) {
	a, b := newCA(t, now), newCA(t, now.Add(-time.Hour))
	ca, err := pki.LoadCA(slices.Concat(a.Bundle(), b.Bundle()), b.Key())
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	end := b.Certificate().NotAfter
	at := end.Add(-time.Hour)
	c, err := ca.IssueNode(v1alpha1.RoleServer, "global", at)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	cert := parseCert(t, c.Cert)
	if want := b.Certificate().SubjectKeyId; !bytes.Equal(cert.AuthorityKeyId, want) {
		t.Errorf("AuthorityKeyId = %x, want the signer's subject key id %x", cert.AuthorityKeyId, want)
	}
	checkValidity(t, cert, at.Add(-5*time.Minute), end)
	if err := verify(t, ca, cert, "server.global.nomad", serverAuth, at); err != nil {
		t.Errorf("Verify against the bundle of both: %v", err)
	}
	if err := verify(t, a, cert, "server.global.nomad", serverAuth, at); err == nil {
		t.Error("Verify against the first CA alone succeeded, want an error")
	}
}

func TestIssueSerialsDiffer(t *testing.T) {
	ca := newCA(t, now)
	seen := map[string]bool{ca.Certificate().SerialNumber.String(): true}
	for range 5 {
		node, err := ca.IssueNode(v1alpha1.RoleClient, "global", now)
		if err != nil {
			t.Fatalf("IssueNode: %v", err)
		}
		operator, err := ca.IssueOperator("global", time.Hour, now)
		if err != nil {
			t.Fatalf("IssueOperator: %v", err)
		}
		for _, c := range []pki.Certificate{node, operator} {
			serial := parseCert(t, c.Cert).SerialNumber.String()
			if seen[serial] {
				t.Errorf("serial number %s issued twice", serial)
			}
			seen[serial] = true
		}
	}
}

func TestIssueErrors(t *testing.T) {
	ca := newCA(t, now)
	node := func(role v1alpha1.Role, region string) func() (pki.Certificate, error) {
		return func() (pki.Certificate, error) { return ca.IssueNode(role, region, now) }
	}
	operator := func(region string, ttl time.Duration) func() (pki.Certificate, error) {
		return func() (pki.Certificate, error) { return ca.IssueOperator(region, ttl, now) }
	}
	for _, tc := range []struct {
		name  string
		issue func() (pki.Certificate, error)
		want  string
	}{
		{"unknown role", node("worker", "global"), `node certificate: role "worker" is not server, client or combined`},
		{"no role", node("", "global"), `node certificate: role "" is not server, client or combined`},
		{"node without a region", node(v1alpha1.RoleServer, ""), "node certificate: no Nomad region"},
		{"operator without a region", operator("", time.Hour), "operator certificate: no Nomad region"},
		{"zero TTL", operator("global", 0), "operator certificate: TTL 0s is not above zero"},
		{"negative TTL", operator("global", -time.Hour), "operator certificate: TTL -1h0m0s is not above zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.issue()
			if errText(err) != tc.want || c.Cert != nil || c.Key != nil {
				t.Errorf("error = %v, want %q and no certificate", err, tc.want)
			}
		})
	}
}

// TestCertificateNeverPrintsItsKey checks that no way of printing or logging a certificate shows its private key.
func TestCertificateNeverPrintsItsKey(t *testing.T) {
	c, err := newCA(t, now).IssueNode(v1alpha1.RoleServer, "global", now)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	secrettest.CheckHidden(t, secrettest.Printed(t, c), map[string][]byte{
		"the key": c.Key, "a PEM private key": []byte("PRIVATE KEY"),
	}, fmt.Sprintf("[secret, %d bytes]", len(c.Key)))
}

// checkLeaf checks what every certificate that the CA issues has in common, and its names, and returns the
// certificate.
func checkLeaf(t *testing.T, ca *pki.CA, c pki.Certificate, dns, ips []string) *x509.Certificate {
	t.Helper()
	cert := parseCert(t, c.Cert)
	if !slices.Equal(cert.DNSNames, dns) {
		t.Errorf("DNSNames = %q, want %q", cert.DNSNames, dns)
	}
	var gotIPs []string
	for _, ip := range cert.IPAddresses {
		gotIPs = append(gotIPs, ip.String())
	}
	if !slices.Equal(gotIPs, ips) {
		t.Errorf("IPAddresses = %q, want %q", gotIPs, ips)
	}
	if cert.Subject.String() != "CN="+dns[0] {
		t.Errorf("Subject = %q, want CN=%s", cert.Subject, dns[0])
	}
	if cert.IsCA || cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("CA %t, KeyUsage %b; want a leaf for DigitalSignature only", cert.IsCA, cert.KeyUsage)
	}
	if err := cert.CheckSignatureFrom(ca.Certificate()); err != nil {
		t.Errorf("the CA did not sign the certificate: %v", err)
	}
	checkSerial(t, cert)
	if !parseKey(t, c.Key).PublicKey.Equal(cert.PublicKey) {
		t.Error("the key does not match the certificate")
	}
	return cert
}

// verify checks a certificate against a pool of the CA's bundle, for a name and the usage, at the time at.
func verify(t *testing.T, ca *pki.CA, cert *x509.Certificate, name string, usage []x509.ExtKeyUsage,
	at time.Time,
) error {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.Bundle()) {
		t.Fatal("the CA bundle has no certificates for a pool")
	}
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: at, KeyUsages: usage})
	return err
}

// parseCert parses a certificate of one PEM block.
func parseCert(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	blocks := pemBlocks(t, b)
	if len(blocks) != 1 || blocks[0].Type != "CERTIFICATE" {
		t.Fatalf("%d bytes are not one PEM block of type CERTIFICATE", len(b))
	}
	cert, err := x509.ParseCertificate(blocks[0].Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}
