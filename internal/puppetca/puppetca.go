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
//
// Reusing the material also means following it. The files belong to Puppet,
// which replaces them without telling anyone: a certificate is re-issued, the
// CA certificate is given a later end date, a server is restored from a
// backup. The daemons therefore re-read the certificate, the key, and the CA
// bundle when they change on disk, as they do the CRL, so that none of those
// needs a restart of codavox to take effect.
package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
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

// Load reads the CA certificate pool and the node's keypair, once.
func (p Paths) Load() (tls.Certificate, *x509.CertPool, error) {
	m, err := p.load()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return m.cert, m.pool, nil
}

// load reads the node's keypair and the CA bundle as they are on disk.
func (p Paths) load() (*material, error) {
	cert, err := tls.LoadX509KeyPair(p.Cert(), p.Key())
	if err != nil {
		return nil, fmt.Errorf("loading keypair for %s: %w", p.CertName, err)
	}

	pemBytes, err := os.ReadFile(p.CACert())
	if err != nil {
		return nil, fmt.Errorf("reading CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in %s", p.CACert())
	}

	cas, err := parseCertificates(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p.CACert(), err)
	}

	return &material{cert: cert, pool: pool, cas: cas}, nil
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
	// AllowedRoles are the pp_role values permitted to fetch code.
	AllowedRoles []string
	// AllowedCertnames are individual certificates permitted to fetch code,
	// matched exactly against the certificate's common name.
	//
	// It exists for estates that already have compilers. pp_role is an X.509
	// extension fixed when a certificate is issued, so a node enrolled before
	// anyone had heard of codavox cannot be given one without re-issuing its
	// certificate — revoke, clean, re-enrol, restart, for every compiler. That
	// is a PKI operation to demand before someone can try this at all.
	//
	// Naming certnames is not the weaker option it looks like: an explicit list
	// of hosts is narrower than a class of them. It is simply per-node, so it
	// does not scale to an estate that grows, which is why pp_role remains the
	// better answer for anything newly enrolled.
	AllowedCertnames []string
	// Revocation selects CRL checking. The zero value is RevocationChain,
	// matching Puppet's certificate_revocation default.
	Revocation RevocationMode
}

// ServerTLS builds a TLS configuration for the publisher, admitting only peers
// that are authorized and not revoked.
//
// A peer is authorized by carrying an allowed pp_role, or by being named in
// AllowedCertnames. Either suffices, so an estate can move to roles a node at a
// time rather than all at once. At least one of the two must be configured:
// verifying against the CA alone admits every node in the estate, since each
// one holds an agent certificate from the same authority.
//
// These constraints live in this constructor rather than being bolted on by the
// caller, because a revoked or unauthorized peer must be refused before it can
// ask for anything.
//
// The returned Revocation is the same check, callable per request. The caller
// must apply it: a handshake happens once per connection, and an HTTP client
// with keep-alive will reuse that connection long after a certificate has been
// revoked. It is nil only when revocation is disabled, and Check tolerates a
// nil receiver so it can be wired in unconditionally.
//
// The configuration follows the ssldir: each handshake uses the certificate,
// key, and CA bundle on disk at that moment, so a re-issued certificate or an
// extended CA certificate takes effect without a restart. If they cannot be
// read, the handshake fails with the reason.
func (p Paths) ServerTLS(policy ServerPolicy) (*tls.Config, *Revocation, error) {
	if len(policy.AllowedRoles) == 0 && len(policy.AllowedCertnames) == 0 {
		return nil, nil, errors.New("at least one allowed pp_role or certname is required")
	}
	mode := policy.Revocation
	if mode == "" {
		mode = RevocationChain
	}

	src, first, err := p.open()
	if err != nil {
		return nil, nil, err
	}

	// The publisher always admits its own certname. An operator reading the
	// fleet view from the node the publisher runs on has that node's key in
	// hand already, so requiring it to be listed would only be a step to forget
	// — and a node presenting its own certificate to its own listener is not a
	// peer being trusted, it is the same trust boundary on both ends.
	certnames := policy.AllowedCertnames
	if p.CertName != "" {
		certnames = append(append([]string(nil), certnames...), p.CertName)
	}

	var rev *Revocation
	verify := VerifyConnectionIdentity(policy.AllowedRoles, certnames)
	if mode != RevocationDisabled {
		// PE points ssl-crl-path at $ssldir/crl.pem on every service listener;
		// this is the same file.
		crl, err := newCRLChecker(p.CRL(), src.issuers)
		if err != nil {
			return nil, nil, err
		}
		rev = &Revocation{crl: crl, mode: mode}
		verify = chainVerifiers(verify, func(cs tls.ConnectionState) error {
			return rev.Check(&cs)
		})
	}

	return src.serverConfig(first, func(m *material) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{m.cert},
			ClientCAs:    m.pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
			NextProtos:   serverProtos,
			// VerifyConnection, not VerifyPeerCertificate: the latter is skipped
			// entirely on resumed sessions, so a peer that handshook once could
			// keep reconnecting without the role ever being rechecked.
			VerifyConnection: verify,
		}
	}), rev, nil
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
//
// Like ServerTLS, it follows the ssldir: each handshake presents the
// certificate on disk at that moment.
func (p Paths) ServerCertTLS() (*tls.Config, error) {
	src, first, err := p.open()
	if err != nil {
		return nil, err
	}
	return src.serverConfig(first, func(m *material) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{m.cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   serverProtos,
		}
	}), nil
}

// ClientTLS builds a TLS configuration for a compiler fetching artifacts, from
// the material on disk now.
//
// It is a single read, which suits a command that makes its requests and
// exits. A process that keeps running uses ClientTransport, so that it does
// not go on with a certificate or a CA bundle that has since been replaced.
func (p Paths) ClientTLS() (*tls.Config, error) {
	m, err := p.load()
	if err != nil {
		return nil, err
	}
	return clientConfig(m), nil
}

// ClientTransport returns an HTTP transport for a long-running compiler-side
// process, configured as ClientTLS configures one and following the ssldir.
//
// Each request is sent with the certificate, key, and CA bundle on disk at
// that moment. When they have changed, the connections opened with the old
// ones are dropped, so the next request handshakes again. If they cannot be
// read, the request fails with the reason.
func (p Paths) ClientTransport() (http.RoundTripper, error) {
	src, first, err := p.open()
	if err != nil {
		return nil, err
	}
	return &reloadingTransport{src: src, from: first, base: newTransport(first)}, nil
}

// ErrNotAuthorized means the peer authenticated against the CA but is not
// permitted to fetch code.
var ErrNotAuthorized = errors.New("peer certificate is not authorized")

// VerifyConnectionIdentity returns a VerifyConnection function admitting peers
// that carry one of the allowed pp_role values, or whose certname is named
// explicitly.
//
// Without this, any node with a Puppet agent certificate could fetch every
// environment's code. A compromised leaf node should not be able to read the
// whole estate's Puppet manifests, which routinely reference internal
// hostnames, credential paths, and topology.
//
// The two are checked in that order and either admits, so an estate with
// existing compilers can list them by name today and move to roles as
// certificates are re-issued, rather than needing a PKI operation before
// anything works at all.
//
// This runs on resumed connections as well as full handshakes, which is why it
// is preferred over VerifyPeerCertificate.
func VerifyConnectionIdentity(allowedRoles, allowedCertnames []string) func(tls.ConnectionState) error {
	roles := make(map[string]bool, len(allowedRoles))
	for _, r := range allowedRoles {
		roles[r] = true
	}
	certnames := make(map[string]bool, len(allowedCertnames))
	for _, c := range allowedCertnames {
		certnames[c] = true
	}

	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no peer certificate presented")
		}
		// Chain verification has already happened against ClientCAs; this adds
		// only the authorization constraint.
		cert := cs.PeerCertificates[0]
		cn := cert.Subject.CommonName

		if certnames[cn] {
			return nil
		}

		role, hasRole := PPRole(cert)
		if hasRole && roles[role] {
			return nil
		}

		// Say which of the two failed, and with what, so an operator can tell a
		// missing pp_role from a wrong one without inspecting the certificate.
		if !hasRole {
			return fmt.Errorf("%w: %q carries no pp_role and is not an allowed certname", ErrNotAuthorized, cn)
		}
		return fmt.Errorf("%w: %q has pp_role %q and is not an allowed certname", ErrNotAuthorized, cn, role)
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
