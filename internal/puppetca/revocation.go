package puppetca

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

// RevocationMode selects how far up a peer's chain revocation is checked.
//
// The values and the default are Puppet's, from the certificate_revocation
// setting: PE configures ssl-crl-path on every service listener and leaves
// puppetserver on the puppet.conf default of "chain". Matching the names means
// an operator who knows what certificate_revocation does already knows what
// this does.
type RevocationMode string

// Revocation modes, mirroring Puppet's certificate_revocation setting.
const (
	// RevocationChain checks every certificate the peer presented. Puppet's
	// default, and the default here.
	RevocationChain RevocationMode = "chain"
	// RevocationLeaf checks only the peer's own certificate.
	RevocationLeaf RevocationMode = "leaf"
	// RevocationDisabled skips the CRL entirely. For estates that do not
	// distribute one; a revoked compiler then keeps its access until its
	// certificate expires.
	RevocationDisabled RevocationMode = "false"
)

// ParseRevocationMode maps a configured string to a mode. An empty value means
// the default, so an operator who sets nothing gets Puppet's behavior.
func ParseRevocationMode(s string) (RevocationMode, error) {
	switch s {
	case "":
		return RevocationChain, nil
	case string(RevocationChain), string(RevocationLeaf):
		return RevocationMode(s), nil
	// Accept the spellings YAML and operators actually produce for "off".
	case "false", "off", "none", "no":
		return RevocationDisabled, nil
	default:
		return "", fmt.Errorf("invalid certificate_revocation %q: want chain, leaf, or false", s)
	}
}

// ErrCertificateRevoked means the peer authenticated against the CA but its
// certificate appears on the CRL.
var ErrCertificateRevoked = errors.New("peer certificate is revoked")

// crlChecker answers "is this certificate revoked?" from the Puppet CA's CRL.
//
// The file is re-read when it changes on disk rather than only at startup:
// revoking a compiler is an incident response, and an operator who runs
// `puppetserver ca revoke` expects the compiler to lose access without also
// having to restart the publisher on every node.
type crlChecker struct {
	path    string
	issuers []*x509.Certificate

	mu      sync.Mutex
	stamp   fileStamp
	revoked map[string]bool // issuer-scoped serial -> revoked
	loaded  bool
}

// fileStamp is the cheap change signal: a stat per handshake, and a re-parse
// only when the file is actually different.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func statCRL(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modTime: fi.ModTime(), size: fi.Size()}, nil
}

// newCRLChecker loads the CRL once so a missing or malformed file fails at
// startup rather than on the first compiler that connects. Refusing to start is
// deliberate: silently serving code to revoked nodes because a file was
// unreadable is the kind of quiet downgrade this project exists to avoid. An
// estate with no CRL sets certificate_revocation to false and says so.
func newCRLChecker(path string, issuers []*x509.Certificate) (*crlChecker, error) {
	c := &crlChecker{path: path, issuers: issuers}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// reload parses the CRL file into a revoked-serial set.
func (c *crlChecker) reload() error {
	stamp, err := statCRL(c.path)
	if err != nil {
		return fmt.Errorf("reading CRL %s: %w", c.path, err)
	}

	pemBytes, err := os.ReadFile(c.path) // #nosec G304 -- path derived from the node's ssldir
	if err != nil {
		return fmt.Errorf("reading CRL %s: %w", c.path, err)
	}

	revoked := map[string]bool{}
	var found int
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "X509 CRL" {
			continue
		}
		list, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return fmt.Errorf("parsing CRL %s: %w", c.path, err)
		}

		// Verify the CRL against the CA that signed it. An unverified CRL is
		// worth nothing: anyone who can write the file could otherwise revoke
		// every compiler, or un-revoke themselves.
		issuer, err := c.issuerFor(list)
		if err != nil {
			return err
		}
		if err := list.CheckSignatureFrom(issuer); err != nil {
			return fmt.Errorf("CRL %s is not signed by %s: %w", c.path, issuer.Subject, err)
		}

		found++
		for _, entry := range list.RevokedCertificateEntries {
			revoked[revocationKey(list.Issuer.String(), entry.SerialNumber)] = true
		}
	}

	if found == 0 {
		return fmt.Errorf("no X509 CRL found in %s", c.path)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = revoked
	c.stamp = stamp
	c.loaded = true
	return nil
}

// issuerFor finds the CA certificate that issued a CRL.
func (c *crlChecker) issuerFor(list *x509.RevocationList) (*x509.Certificate, error) {
	for _, ca := range c.issuers {
		if ca.Subject.String() == list.Issuer.String() {
			return ca, nil
		}
	}
	return nil, fmt.Errorf("CRL %s is issued by %q, which is not in the CA bundle", c.path, list.Issuer)
}

// revocationKey scopes a serial to its issuer. Serial numbers are unique only
// per CA, so a bare serial would let one CA's revocation shadow another's
// perfectly valid certificate.
func revocationKey(issuer string, serial *big.Int) string {
	return issuer + "\x00" + serial.Text(16)
}

// check reports an error if any of certs is revoked, re-reading the CRL when it
// has changed on disk.
func (c *crlChecker) check(certs []*x509.Certificate) error {
	if err := c.refreshIfChanged(); err != nil {
		// A CRL that has become unreadable or invalid since startup must not
		// quietly downgrade to "nothing is revoked". Keep serving on the last
		// good copy and surface the problem on the connection.
		return err
	}

	c.mu.Lock()
	revoked := c.revoked
	c.mu.Unlock()

	for _, cert := range certs {
		if revoked[revocationKey(cert.Issuer.String(), cert.SerialNumber)] {
			return fmt.Errorf("%w: %q (serial %s)",
				ErrCertificateRevoked, cert.Subject.CommonName, cert.SerialNumber.Text(16))
		}
	}
	return nil
}

// refreshIfChanged re-parses the CRL when the file's mtime or size has moved.
func (c *crlChecker) refreshIfChanged() error {
	stamp, err := statCRL(c.path)
	if err != nil {
		return fmt.Errorf("reading CRL %s: %w", c.path, err)
	}

	c.mu.Lock()
	unchanged := c.loaded && stamp == c.stamp
	c.mu.Unlock()
	if unchanged {
		return nil
	}
	return c.reload()
}
