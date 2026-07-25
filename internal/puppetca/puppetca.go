// Package puppetca builds TLS configuration from the Puppet CA material that
// is already on disk.
//
// codavox issues no certificates and runs no CA. Every node in an OpenVox
// deployment has been enrolled with the primary's CA already: the agent run
// that joins a compiler to the pool leaves it a signed certificate, a private
// key, the CA certificate, and a CRL, all at well-known paths. Reusing them
// means there is no second PKI to provision, distribute, rotate, or revoke.
//
// Revoking a compiler's Puppet certificate revokes its access to code, because
// the publisher checks the same CRL. That is enforced rather than assumed: a
// certificate signed by the CA stays cryptographically valid after revocation,
// so nothing about mutual TLS alone would stop a revoked node from fetching
// every environment. PE takes the same approach — its Puppet module sets
// ssl-crl-path to $ssldir/crl.pem on every service listener, and leaves
// puppetserver on puppet.conf's certificate_revocation default of "chain".
package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
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
	cert, pool, _, err := p.load()
	return cert, pool, err
}

// load additionally returns the parsed CA certificates, which CRL verification
// needs: a pool can only build chains, and checking a CRL's signature requires
// the issuing certificate itself.
func (p Paths) load() (tls.Certificate, *x509.CertPool, []*x509.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(p.Cert(), p.Key())
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("loading keypair for %s: %w", p.CertName, err)
	}

	pemBytes, err := os.ReadFile(p.CACert())
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("reading CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return tls.Certificate{}, nil, nil, fmt.Errorf("no certificates found in %s", p.CACert())
	}

	cas, err := parseCertificates(pemBytes)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("parsing %s: %w", p.CACert(), err)
	}

	return cert, pool, cas, nil
}

// parseCertificates decodes every CERTIFICATE block in a PEM bundle.
func parseCertificates(pemBytes []byte) ([]*x509.Certificate, error) {
	var (
		certs []*x509.Certificate
		rest  = pemBytes
	)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

// ServerPolicy is who the publisher will admit.
type ServerPolicy struct {
	// AllowedRoles are the pp_role values permitted to fetch code. At least one
	// is required.
	AllowedRoles []string
	// Revocation selects CRL checking. The zero value is RevocationChain,
	// matching Puppet's certificate_revocation default.
	Revocation RevocationMode
}

// ServerTLS builds a TLS configuration for the publisher, admitting only peers
// whose certificate carries one of the allowed roles and is not revoked.
//
// Both constraints live in this constructor rather than being bolted on by the
// caller, because verifying against the CA alone admits every node in the
// estate — each one holds an agent certificate from the same authority, and a
// revoked one keeps holding it.
//
// The returned Revocation is the same check, callable per request. The caller
// must apply it: a handshake happens once per connection, and an HTTP client
// with keep-alive will reuse that connection long after a certificate has been
// revoked. It is nil only when revocation is disabled, and Check tolerates a
// nil receiver so it can be wired in unconditionally.
func (p Paths) ServerTLS(policy ServerPolicy) (*tls.Config, *Revocation, error) {
	if len(policy.AllowedRoles) == 0 {
		return nil, nil, errors.New("at least one allowed pp_role is required")
	}
	mode := policy.Revocation
	if mode == "" {
		mode = RevocationChain
	}

	cert, pool, cas, err := p.load()
	if err != nil {
		return nil, nil, err
	}

	var rev *Revocation
	verify := VerifyConnectionRole(policy.AllowedRoles...)
	if mode != RevocationDisabled {
		// PE points ssl-crl-path at $ssldir/crl.pem on every service listener;
		// this is the same file.
		crl, err := newCRLChecker(p.CRL(), cas)
		if err != nil {
			return nil, nil, err
		}
		rev = &Revocation{crl: crl, mode: mode}
		verify = chainVerifiers(verify, func(cs tls.ConnectionState) error {
			return rev.Check(&cs)
		})
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
		// VerifyConnection, not VerifyPeerCertificate: the latter is skipped
		// entirely on resumed sessions, so a peer that handshook once could
		// keep reconnecting without the role ever being rechecked.
		VerifyConnection: verify,
	}, rev, nil
}

// chainVerifiers runs each check in order, stopping at the first failure.
func chainVerifiers(fns ...func(tls.ConnectionState) error) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		for _, fn := range fns {
			if err := fn(cs); err != nil {
				return err
			}
		}
		return nil
	}
}

// ServerCertTLS builds a server TLS configuration that presents the node's
// certificate but requires none from the client.
//
// It is for endpoints that must accept peers which cannot present a Puppet
// certificate — a webhook from GitHub or GitLab — where authentication is by
// shared secret rather than by mutual TLS. The publisher's artifact API uses
// ServerTLS instead; this is deliberately weaker and belongs only where the
// caller is authenticated another way.
func (p Paths) ServerCertTLS() (*tls.Config, error) {
	cert, _, err := p.Load()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
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
