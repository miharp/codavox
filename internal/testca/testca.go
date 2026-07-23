// Package testca issues certificates the way a Puppet CA does, including the
// pp_role extension, so authorization can be exercised against realistic input
// rather than hand-built structs.
//
// It exists only to support tests. Nothing in the shipped binary imports it.
package testca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PPRoleOID is Puppet's registered pp_role certificate extension, verified
// against openvox lib/puppet/ssl/oids.rb.
var PPRoleOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 1, 13}

// CA is a throwaway certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	DER  []byte
}

// New returns a new CA.
func New(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Puppet CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Cert: cert, Key: key, DER: der}
}

// Issue signs a leaf certificate for cn.
//
// An empty role omits the pp_role extension, modelling an ordinary agent.
// rawRole writes the value unwrapped instead of as an ASN.1 UTF8String, so the
// tolerant decoding path can be exercised.
func (ca *CA) Issue(t *testing.T, cn, role string, rawRole bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn, "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	if role != "" {
		value := []byte(role)
		if !rawRole {
			value, err = asn1.Marshal(role)
			if err != nil {
				t.Fatal(err)
			}
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: PPRoleOID, Value: value}}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// TLSCert issues a leaf and returns it as a tls.Certificate.
func (ca *CA) TLSCert(t *testing.T, cn, role string) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := ca.Issue(t, cn, role, false)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Pool returns a CertPool trusting this CA.
func (ca *CA) Pool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.PEM()) {
		t.Fatal("failed to add CA certificate to pool")
	}
	return pool
}

// PEM returns the CA certificate in PEM form.
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.DER})
}

// SSLDir lays out a node's SSL material at Puppet's standard paths and returns
// the directory and certname.
func (ca *CA) SSLDir(t *testing.T, certname, role string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"certs", "private_keys"} {
		// 0755 mirrors Puppet's real ssldir layout.
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	certPEM, keyPEM := ca.Issue(t, certname, role, false)
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "certs", certname+".pem"), certPEM)
	write(filepath.Join(dir, "private_keys", certname+".pem"), keyPEM)
	write(filepath.Join(dir, "certs", "ca.pem"), ca.PEM())

	return dir
}
