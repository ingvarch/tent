package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// nodeYears is how long a node certificate is valid.
const nodeYears = 1

// Certificate is a certificate that a CA issued and its private key, both PEM. The key never prints, as a Secret.
type Certificate struct {
	Cert []byte // the certificate
	Key  Secret // its private key, PKCS#8
}

// IssueNode issues the certificate of a Nomad node of the role in the Nomad region: server.<region>.nomad for a
// server, client.<region>.nomad for a client and both for a combined node, each also for localhost and 127.0.0.1. It
// is for server and client authentication, and valid from five minutes before now until a year after now, or until
// the CA ends if that is sooner.
func (ca *CA) IssueNode(role v1alpha1.Role, region string, now time.Time) (Certificate, error) {
	names, err := nodeNames(role, region)
	if err != nil {
		return Certificate{}, fmt.Errorf("node certificate: %w", err)
	}
	c, err := ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: names[0]},
		DNSNames:    append(names, "localhost"),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		NotAfter:    now.AddDate(nodeYears, 0, 0),
	}, now)
	if err != nil {
		return Certificate{}, fmt.Errorf("node certificate: %w", err)
	}
	return c, nil
}

// nodeNames returns the Nomad names of a node of the role in the region.
func nodeNames(role v1alpha1.Role, region string) ([]string, error) {
	if region == "" {
		return nil, errors.New("no Nomad region")
	}
	var names []string
	if role.RunsServer() {
		names = append(names, "server."+region+".nomad")
	}
	if role.RunsClient() {
		names = append(names, "client."+region+".nomad")
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("role %q is not server, client or combined", role)
	}
	return names, nil
}

// IssueOperator issues the certificate of an operator of the Nomad region, cli.<region>.nomad, for client
// authentication only. It is valid from five minutes before now until ttl after now, or until the CA ends if that is
// sooner. The ttl must be above zero.
func (ca *CA) IssueOperator(region string, ttl time.Duration, now time.Time) (Certificate, error) {
	if region == "" {
		return Certificate{}, errors.New("operator certificate: no Nomad region")
	}
	if ttl <= 0 {
		return Certificate{}, fmt.Errorf("operator certificate: TTL %s is not above zero", ttl)
	}
	name := "cli." + region + ".nomad"
	c, err := ca.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		DNSNames:    []string{name},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotAfter:    now.Add(ttl),
	}, now)
	if err != nil {
		return Certificate{}, fmt.Errorf("operator certificate: %w", err)
	}
	return c, nil
}

// issue signs a certificate for a new ECDSA P-256 key. The template holds the subject, the names, the extended key
// usage and the end; issue sets the rest: a random serial number, the start backdate before now, the end of the CA
// when the template's is later, and the key usage DigitalSignature.
func (ca *CA) issue(tmpl *x509.Certificate, now time.Time) (Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Certificate{}, fmt.Errorf("generate the key: %w", err)
	}
	if tmpl.SerialNumber, err = newSerial(); err != nil {
		return Certificate{}, err
	}
	tmpl.NotBefore = now.Add(-backdate)
	if tmpl.NotAfter.After(ca.signer.NotAfter) {
		tmpl.NotAfter = ca.signer.NotAfter
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.signer, &key.PublicKey, ca.key)
	if err != nil {
		return Certificate{}, fmt.Errorf("sign the certificate: %w", err)
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return Certificate{}, err
	}
	return Certificate{Cert: pemOf(certificateBlock, der), Key: keyPEM}, nil
}
