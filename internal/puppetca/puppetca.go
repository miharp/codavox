// Package puppetca builds TLS configuration from the Puppet CA material that
// is already on disk.
//
// codavox issues no certificates and runs no CA. Every node in an OpenVox
// deployment has been enrolled with the primary's CA already: the agent run
// that joins a compiler to the pool leaves it a signed certificate, a private
// key, the CA certificate, and a CRL, all at well-known paths. Reusing them
// means there is no second PKI to provision, distribute, rotate, or revoke —
// and revoking a compiler's Puppet certificate revokes its access to code as a
// side effect, which is the behavior an operator would expect.
package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultSSLDir is Puppet's ssldir on a package-installed node.
const DefaultSSLDir = "/etc/puppetlabs/puppet/ssl"

// ppRoleOID is Puppet's registered pp_role certificate extension.
//
// Verified against openvox lib/puppet/ssl/oids.rb:
//
//	["1.3.6.1.4.1.34380.1.1.13", 'pp_role', 'Puppet Node Role Name']
//
// ovadm writes pp_role into csr_attributes.yaml before the CSR is submitted,
// so the signed certificate carries the node's role. That turns "this cert was
// signed by our CA" into "this node is a compiler", which is authorization
// rather than mere authentication.
var ppRoleOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 1, 13}

// Paths locates the Puppet SSL material for a node.
type Paths struct {
	SSLDir   string
	CertName string
}

// Cert returns the node's signed certificate path.
func (p Paths) Cert() string {
	return filepath.Join(p.SSLDir, "certs", p.CertName+".pem")
}

// Key returns the node's private key path.
func (p Paths) Key() string {
	return filepath.Join(p.SSLDir, "private_keys", p.CertName+".pem")
}

// CACert returns the CA certificate path.
func (p Paths) CACert() string { return filepath.Join(p.SSLDir, "certs", "ca.pem") }

// CRL returns the certificate revocation list path.
func (p Paths) CRL() string { return filepath.Join(p.SSLDir, "crl.pem") }

// Load reads the CA certificate pool and the node's keypair.
func (p Paths) Load() (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(p.Cert(), p.Key())
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading keypair for %s: %w", p.CertName, err)
	}

	pem, err := os.ReadFile(p.CACert())
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("reading CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return tls.Certificate{}, nil, fmt.Errorf("no certificates found in %s", p.CACert())
	}

	return cert, pool, nil
}

// ServerTLS builds a TLS configuration for the publisher, admitting only peers
// whose certificate carries one of allowedRoles.
//
// The role constraint is part of this constructor rather than something the
// caller bolts on, because verifying against the CA alone admits every node in
// the estate — each one holds an agent certificate from the same authority.
func (p Paths) ServerTLS(allowedRoles ...string) (*tls.Config, error) {
	if len(allowedRoles) == 0 {
		return nil, errors.New("at least one allowed pp_role is required")
	}

	cert, pool, err := p.Load()
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
		// VerifyConnection, not VerifyPeerCertificate: the latter is skipped
		// entirely on resumed sessions, so a peer that handshook once could
		// keep reconnecting without the role ever being rechecked.
		VerifyConnection: VerifyConnectionRole(allowedRoles...),
	}, nil
}

// ClientTLS builds a TLS configuration for a compiler fetching artifacts.
func (p Paths) ClientTLS() (*tls.Config, error) {
	cert, pool, err := p.Load()
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ErrRoleMismatch means the peer authenticated but is not permitted.
var ErrRoleMismatch = errors.New("peer certificate does not carry the required pp_role")

// VerifyConnectionRole returns a VerifyConnection function admitting only
// peers whose certificate carries one of the given pp_role values.
//
// Without this, any node with a Puppet agent certificate could fetch every
// environment's code. A compromised leaf node should not be able to read the
// whole estate's Puppet manifests, which routinely reference internal
// hostnames, credential paths, and topology.
//
// This runs on resumed connections as well as full handshakes, which is why it
// is preferred over VerifyPeerCertificate.
func VerifyConnectionRole(allowed ...string) func(tls.ConnectionState) error {
	permitted := make(map[string]bool, len(allowed))
	for _, r := range allowed {
		permitted[r] = true
	}

	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no peer certificate presented")
		}
		// Chain verification has already happened against ClientCAs; this adds
		// only the role constraint.
		cert := cs.PeerCertificates[0]

		role, ok := PPRole(cert)
		if !ok {
			return fmt.Errorf("%w: certificate for %q carries no pp_role", ErrRoleMismatch, cert.Subject.CommonName)
		}
		if !permitted[role] {
			return fmt.Errorf("%w: %q has pp_role %q", ErrRoleMismatch, cert.Subject.CommonName, role)
		}
		return nil
	}
}

// PPRole extracts the pp_role extension value from a certificate.
func PPRole(cert *x509.Certificate) (string, bool) {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(ppRoleOID) {
			continue
		}
		// Puppet encodes extension values as ASN.1 UTF8String. Older tooling
		// has been known to write a bare string, so fall back rather than
		// rejecting a certificate that is otherwise valid.
		var s string
		if _, err := asn1.Unmarshal(ext.Value, &s); err == nil {
			return s, true
		}
		return string(ext.Value), true
	}
	return "", false
}
