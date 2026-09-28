package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

const (
	// caYears is how long a CA certificate is valid.
	caYears = 10
	// backdate starts every certificate this long before it is made, so that a machine whose clock is behind
	// accepts it.
	backdate = 5 * time.Minute

	certificateBlock = "CERTIFICATE" // the PEM type of a certificate
	privateKeyBlock  = "PRIVATE KEY" // the PEM type of a PKCS#8 private key
)

// CA is a cluster's certificate authority: a bundle of one or more CA certificates and the private key of one of
// them, the active signer, which issues certificates. More than one certificate lets the CA be rotated. The key never
// prints: fmt shows the address of its pointer and encoding/json skips it.
type CA struct {
	certs  []*x509.Certificate // the bundle, in its order
	signer *x509.Certificate   // the certificate of the bundle whose public key matches key
	key    *ecdsa.PrivateKey
}

// NewCA makes a new CA for a cluster: an ECDSA P-256 key and a self-signed CA certificate for it, of the subject
// CN=tent <cluster> CA, O=tent, valid from five minutes before now, for clock skew, until ten years after now.
func NewCA(cluster string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("CA: generate the key: %w", err)
	}
	return newCA(cluster, key, now)
}

// NewCAFromKey makes a CA as NewCA does, but for a stored key, as Key returns it, instead of a new one. It is for a
// key whose bundle was never written.
func NewCAFromKey(cluster string, key Secret, now time.Time) (*CA, error) {
	k, err := parseKey(key)
	if err != nil {
		return nil, err
	}
	return newCA(cluster, k, now)
}

// newCA signs a CA certificate for the key with the key itself.
func newCA(cluster string, key *ecdsa.PrivateKey, now time.Time) (*CA, error) {
	if cluster == "" {
		return nil, errors.New("CA: no cluster name")
	}
	serial, err := newSerial()
	if err != nil {
		return nil, fmt.Errorf("CA: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tent " + cluster + " CA", Organization: []string{"tent"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(caYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("CA: sign the certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("CA: parse the certificate: %w", err)
	}
	return &CA{certs: []*x509.Certificate{cert}, signer: cert, key: key}, nil
}

// LoadCA reads a CA from its bundle and the key of its active signer, as Bundle and Key return them. The active signer
// is the first certificate of the bundle whose public key matches the key.
func LoadCA(bundle []byte, key Secret) (*CA, error) {
	certs, err := parseBundle(bundle)
	if err != nil {
		return nil, err
	}
	k, err := parseKey(key)
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		if k.PublicKey.Equal(c.PublicKey) {
			return &CA{certs: certs, signer: c, key: k}, nil
		}
	}
	return nil, errors.New("CA key: matches no certificate of the CA bundle")
}

// parseBundle parses the PEM blocks of a bundle, which must all be CA certificates, with only white space after the
// last one.
func parseBundle(bundle []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	block, rest := pem.Decode(bundle)
	for ; block != nil; block, rest = pem.Decode(rest) {
		n := len(certs) + 1
		if block.Type != certificateBlock {
			return nil, fmt.Errorf("CA bundle: block %d is of type %q, not CERTIFICATE", n, block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("CA bundle: certificate %d: %w", n, err)
		}
		if !c.BasicConstraintsValid || !c.IsCA {
			return nil, fmt.Errorf("CA bundle: certificate %d is not a CA certificate", n)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("CA bundle: no certificates")
	}
	if tail := bytes.TrimSpace(rest); len(tail) > 0 {
		return nil, fmt.Errorf("CA bundle: %d bytes after certificate %d are not PEM", len(tail), len(certs))
	}
	return certs, nil
}

// parseKey parses an ECDSA P-256 private key in PKCS#8 PEM, one block with only white space after it. Its errors
// never show the key.
func parseKey(key Secret) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(key)
	if block == nil {
		return nil, errors.New("CA key: not a PEM block")
	}
	if tail := bytes.TrimSpace(rest); len(tail) > 0 {
		return nil, fmt.Errorf("CA key: %d bytes follow the PEM block", len(tail))
	}
	if block.Type != privateKeyBlock {
		return nil, fmt.Errorf("CA key: a PEM block of type %q, not PRIVATE KEY", block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	if ec, ok := k.(*ecdsa.PrivateKey); ok && ec.Curve == elliptic.P256() {
		return ec, nil
	}
	return nil, errors.New("CA key: not an ECDSA P-256 key")
}

// Bundle returns every certificate of the CA as PEM, in the order of the bundle.
func (ca *CA) Bundle() []byte {
	var b []byte
	for _, c := range ca.certs {
		b = append(b, pemOf(certificateBlock, c.Raw)...)
	}
	return b
}

// Key returns the private key of the active signer as PKCS#8 PEM.
func (ca *CA) Key() Secret {
	key, _ := marshalKey(ca.key) // never fails: every constructor makes or parses an ECDSA P-256 key
	return key
}

// SignerID returns the id of the active signer: the subject key identifier of its certificate, in hex.
func (ca *CA) SignerID() string { return hex.EncodeToString(ca.signer.SubjectKeyId) }

// Certificate returns the certificate of the active signer.
func (ca *CA) Certificate() *x509.Certificate { return ca.signer }

// marshalKey returns a private key as PKCS#8 PEM.
func marshalKey(key *ecdsa.PrivateKey) (Secret, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal the private key: %w", err)
	}
	return pemOf(privateKeyBlock, der), nil
}

// pemOf returns der as one PEM block of the type.
func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// newSerial returns a random serial number of at most 128 bits, above zero.
func newSerial() (*big.Int, error) {
	one := big.NewInt(1)
	limit := new(big.Int).Lsh(one, 128)
	n, err := rand.Int(rand.Reader, limit.Sub(limit, one)) // 0 to 2^128 - 2
	if err != nil {
		return nil, fmt.Errorf("serial number: %w", err)
	}
	return n.Add(n, one), nil
}
