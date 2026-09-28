package pki_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secrettest"
)

// now is when the tests make their CAs and certificates: whole seconds, as certificates keep them.
var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newCA(t *testing.T, at time.Time) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA("prod", at)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return ca
}

func TestNewCA(t *testing.T) {
	checkCA(t, newCA(t, now), "prod", now)
}

// TestNewCAFromKey checks that a CA made from a stored key signs a new certificate with that key.
func TestNewCAFromKey(t *testing.T) {
	old := newCA(t, now)
	later := now.Add(time.Hour)
	ca, err := pki.NewCAFromKey("prod", old.Key(), later)
	if err != nil {
		t.Fatalf("NewCAFromKey: %v", err)
	}
	checkCA(t, ca, "prod", later)
	if !bytes.Equal(ca.Key(), old.Key()) {
		t.Error("Key() is not the key given")
	}
	if ca.Certificate().Equal(old.Certificate()) {
		t.Error("Certificate() is the old certificate, want a new one")
	}
}

func TestNewCARejectsAnEmptyCluster(t *testing.T) {
	const want = "CA: no cluster name"
	if _, err := pki.NewCA("", now); errText(err) != want {
		t.Errorf("NewCA(\"\") error = %v, want %q", err, want)
	}
	if _, err := pki.NewCAFromKey("", newCA(t, now).Key(), now); errText(err) != want {
		t.Errorf("NewCAFromKey(\"\") error = %v, want %q", err, want)
	}
}

func TestNewCAFromKeyRejectsABadKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  pki.Secret
		want string
	}{
		{"not PEM", pki.Secret("junk"), "CA key: not a PEM block"},
		{"a P-384 key", pkcs8(t, ecdsaKey(t, elliptic.P384())), "CA key: not an ECDSA P-256 key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pki.NewCAFromKey("prod", tc.key, now); errText(err) != tc.want {
				t.Errorf("NewCAFromKey() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadCA(t *testing.T) {
	ca := newCA(t, now)
	got, err := pki.LoadCA(ca.Bundle(), ca.Key())
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	checkSameCA(t, got, ca, ca.Bundle())
}

// TestLoadCAAllowsSpaceAfterThePEM checks that white space after the last PEM block of the bundle and of the key is
// not an error.
func TestLoadCAAllowsSpaceAfterThePEM(t *testing.T) {
	ca := newCA(t, now)
	got, err := pki.LoadCA(slices.Concat(ca.Bundle(), []byte("\n \t\r\n")), slices.Concat(ca.Key(), []byte("\n\n")))
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	checkSameCA(t, got, ca, ca.Bundle())
}

// TestLoadCAPicksTheSignerThatMatchesTheKey checks that in a bundle of two CAs, the active signer is the one whose
// public key matches the key.
func TestLoadCAPicksTheSignerThatMatchesTheKey(t *testing.T) {
	a, b := newCA(t, now), newCA(t, now.Add(time.Hour))
	if a.SignerID() == b.SignerID() {
		t.Fatalf("two CAs have the same signer id %s", a.SignerID())
	}
	bundle := slices.Concat(a.Bundle(), b.Bundle())
	for name, want := range map[string]*pki.CA{"first": a, "second": b} {
		t.Run(name, func(t *testing.T) {
			got, err := pki.LoadCA(bundle, want.Key())
			if err != nil {
				t.Fatalf("LoadCA: %v", err)
			}
			checkSameCA(t, got, want, bundle)
		})
	}
}

func TestLoadCAErrors(t *testing.T) {
	ca, other := newCA(t, now), newCA(t, now).Key()
	node, err := ca.IssueNode(v1alpha1.RoleServer, "global", now)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	for _, tc := range []struct {
		name   string
		bundle []byte
		key    pki.Secret
		want   string // the start of the error
	}{
		{"empty bundle", nil, ca.Key(), "CA bundle: no certificates"},
		{"a bundle without PEM", []byte("not a certificate\n"), ca.Key(), "CA bundle: no certificates"},
		{"a key in the bundle", slices.Concat(ca.Bundle(), ca.Key()), ca.Key(),
			`CA bundle: block 2 is of type "PRIVATE KEY", not CERTIFICATE`},
		{"a certificate without basic constraints", slices.Concat(ca.Bundle(), node.Cert), ca.Key(),
			"CA bundle: certificate 2 is not a CA certificate"},
		{"a certificate whose basic constraints say it is not a CA", slices.Concat(ca.Bundle(), notCA(t)), ca.Key(),
			"CA bundle: certificate 2 is not a CA certificate"},
		{"a broken certificate", pemOf("CERTIFICATE", []byte("junk")), ca.Key(), "CA bundle: certificate 1: x509: "},
		// The first 100 bytes of a certificate end inside its second line of base64: with their line ends, the header
		// takes 28 bytes and each line of base64 65.
		{"a cut-off certificate", slices.Concat(ca.Bundle(), ca.Bundle()[:100]), ca.Key(),
			"CA bundle: 100 bytes after certificate 1 are not PEM"},
		{"text after the certificates", slices.Concat(ca.Bundle(), []byte("junk\n")), ca.Key(),
			"CA bundle: 4 bytes after certificate 1 are not PEM"},
		{"no key", ca.Bundle(), nil, "CA key: not a PEM block"},
		{"a key that is not PEM", ca.Bundle(), pki.Secret("junk"), "CA key: not a PEM block"},
		{"two keys", ca.Bundle(), slices.Concat(ca.Key(), other),
			fmt.Sprintf("CA key: %d bytes follow the PEM block", len(bytes.TrimSpace(other)))},
		{"text after the key", ca.Bundle(), slices.Concat(ca.Key(), []byte("junk\n")),
			"CA key: 4 bytes follow the PEM block"},
		{"a SEC1 key", ca.Bundle(), sec1(t), `CA key: a PEM block of type "EC PRIVATE KEY", not PRIVATE KEY`},
		{"a broken key", ca.Bundle(), pemOf("PRIVATE KEY", []byte("junk")), "CA key: asn1: "},
		{"an Ed25519 key", ca.Bundle(), pkcs8(t, ed25519Key(t)), "CA key: not an ECDSA P-256 key"},
		{"a P-384 key", ca.Bundle(), pkcs8(t, ecdsaKey(t, elliptic.P384())), "CA key: not an ECDSA P-256 key"},
		{"the key of another CA", ca.Bundle(), other,
			"CA key: matches no certificate of the CA bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pki.LoadCA(tc.bundle, tc.key)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || got != nil {
				t.Errorf("LoadCA() error = %v, want one starting with %q", err, tc.want)
			}
		})
	}
}

// TestCANeverPrintsItsKey checks that no way of printing or logging a CA shows its private key.
func TestCANeverPrintsItsKey(t *testing.T) {
	ca := newCA(t, now)
	scalar, err := parseKey(t, ca.Key()).Bytes()
	if err != nil {
		t.Fatalf("the scalar of the key: %v", err)
	}
	secrettest.CheckHidden(t, secrettest.Printed(t, ca), map[string][]byte{
		"the key (and its DER)":        ca.Key(),
		"the key's scalar":             scalar,
		"the key's scalar as a number": []byte(new(big.Int).SetBytes(scalar).String()),
		"a PEM private key":            []byte("PRIVATE KEY"),
	}, "")
}

// checkCA checks a CA of one certificate, made for cluster at the time at.
func checkCA(t *testing.T, ca *pki.CA, cluster string, at time.Time) {
	t.Helper()
	c := ca.Certificate()
	if pub, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("the public key is a %T, want an ECDSA P-256 key", c.PublicKey)
	}
	if c.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("SignatureAlgorithm = %v, want %v", c.SignatureAlgorithm, x509.ECDSAWithSHA256)
	}
	if !c.BasicConstraintsValid || !c.IsCA || c.MaxPathLen != 0 || !c.MaxPathLenZero {
		t.Errorf("basic constraints: valid %t, CA %t, path length %d, zero %t; want a CA of path length 0",
			c.BasicConstraintsValid, c.IsCA, c.MaxPathLen, c.MaxPathLenZero)
	}
	if want := x509.KeyUsageCertSign | x509.KeyUsageCRLSign; c.KeyUsage != want {
		t.Errorf("KeyUsage = %b, want %b (CertSign and CRLSign)", c.KeyUsage, want)
	}
	checkValidity(t, c, at.Add(-5*time.Minute), at.AddDate(10, 0, 0))
	if got, want := c.Subject.String(), "CN=tent "+cluster+" CA,O=tent"; got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
	if err := c.CheckSignatureFrom(c); err != nil || c.Issuer.String() != c.Subject.String() {
		t.Errorf("the certificate is not self-signed: issuer %q, %v", c.Issuer, err)
	}
	checkSerial(t, c)
	if got, want := ca.SignerID(), hex.EncodeToString(c.SubjectKeyId); len(c.SubjectKeyId) == 0 || got != want {
		t.Errorf("SignerID() = %q, want the subject key id %q", got, want)
	}
	if blocks := pemBlocks(t, ca.Bundle()); len(blocks) != 1 || blocks[0].Type != "CERTIFICATE" ||
		!bytes.Equal(blocks[0].Bytes, c.Raw) {
		t.Errorf("Bundle() is not the certificate alone, as PEM")
	}
	if !parseKey(t, ca.Key()).PublicKey.Equal(c.PublicKey) {
		t.Error("Key() does not match the certificate")
	}
}

// checkSameCA checks that got is want with the given bundle.
func checkSameCA(t *testing.T, got, want *pki.CA, bundle []byte) {
	t.Helper()
	if !bytes.Equal(got.Bundle(), bundle) {
		t.Errorf("Bundle() differs from the bundle given")
	}
	if !bytes.Equal(got.Key(), want.Key()) {
		t.Errorf("Key() differs from the key given")
	}
	if got.SignerID() != want.SignerID() {
		t.Errorf("SignerID() = %s, want %s", got.SignerID(), want.SignerID())
	}
	if !got.Certificate().Equal(want.Certificate()) {
		t.Errorf("Certificate() is %q, serial %s; want %q, serial %s", got.Certificate().Subject,
			got.Certificate().SerialNumber, want.Certificate().Subject, want.Certificate().SerialNumber)
	}
}

// checkValidity checks a certificate's validity period.
func checkValidity(t *testing.T, c *x509.Certificate, notBefore, notAfter time.Time) {
	t.Helper()
	if !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) {
		t.Errorf("valid from %s to %s, want from %s to %s", c.NotBefore, c.NotAfter, notBefore, notAfter)
	}
}

// checkSerial checks that a certificate's serial number is positive and has at most 128 bits.
func checkSerial(t *testing.T, c *x509.Certificate) {
	t.Helper()
	if s := c.SerialNumber; s == nil || s.Sign() <= 0 || s.BitLen() > 128 {
		t.Errorf("SerialNumber = %s, want a positive number of at most 128 bits", s)
	}
}

// pemBlocks decodes every PEM block of b and fails the test on anything else in it.
func pemBlocks(t *testing.T, b []byte) []*pem.Block {
	t.Helper()
	var blocks []*pem.Block
	for {
		block, rest := pem.Decode(b)
		if block == nil {
			if len(bytes.TrimSpace(rest)) > 0 {
				t.Fatalf("%d bytes after the last of %d PEM blocks", len(rest), len(blocks))
			}
			return blocks
		}
		blocks, b = append(blocks, block), rest
	}
}

// parseKey parses one PKCS#8 PEM block of an ECDSA P-256 key. Its errors never print the key.
func parseKey(t *testing.T, s pki.Secret) *ecdsa.PrivateKey {
	t.Helper()
	blocks := pemBlocks(t, s)
	if len(blocks) != 1 || blocks[0].Type != "PRIVATE KEY" {
		t.Fatalf("the key is not one PEM block of type PRIVATE KEY")
	}
	key, err := x509.ParsePKCS8PrivateKey(blocks[0].Bytes)
	if err != nil {
		t.Fatalf("the key is not PKCS#8: %v", err)
	}
	k, ok := key.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		t.Fatalf("the key is a %T, want an ECDSA P-256 key", key)
	}
	return k
}

// pemOf returns der as one PEM block of the type.
func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// pkcs8 returns a private key as PKCS#8 PEM.
func pkcs8(t *testing.T, key any) pki.Secret {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return pemOf("PRIVATE KEY", der)
}

// sec1 returns a new ECDSA P-256 key as SEC1 PEM, the form that tent does not use.
func sec1(t *testing.T) pki.Secret {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(ecdsaKey(t, elliptic.P256()))
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	return pemOf("EC PRIVATE KEY", der)
}

func ecdsaKey(t *testing.T, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func ed25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return key
}

// notCA returns a self-signed certificate whose basic constraints say that it is not a CA, as PEM.
func notCA(t *testing.T) []byte {
	t.Helper()
	key := ecdsaKey(t, elliptic.P256())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now, NotAfter: now.Add(time.Hour), BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pemOf("CERTIFICATE", der)
}

// errText returns the error's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
