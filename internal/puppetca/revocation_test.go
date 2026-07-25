package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

func TestParseRevocationMode(t *testing.T) {
	valid := map[string]RevocationMode{
		"":      RevocationChain, // unset means Puppet's default
		"chain": RevocationChain,
		"leaf":  RevocationLeaf,
		"false": RevocationDisabled,
		"off":   RevocationDisabled,
		"none":  RevocationDisabled,
		"no":    RevocationDisabled,
	}
	for in, want := range valid {
		got, err := ParseRevocationMode(in)
		if err != nil {
			t.Errorf("ParseRevocationMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRevocationMode(%q) = %q, want %q", in, got, want)
		}
	}

	// A typo must be rejected rather than silently disabling revocation, which
	// is the failure that matters: "chian" quietly admitting revoked compilers
	// is far worse than a startup error.
	for _, bad := range []string{"chian", "true", "yes", "1"} {
		if _, err := ParseRevocationMode(bad); err == nil {
			t.Errorf("ParseRevocationMode(%q) should have failed", bad)
		}
	}
}

// The headline property: a compiler whose certificate has been revoked loses
// access to code. Its certificate is still signed by the CA and still carries
// the right pp_role, so nothing but the CRL check stops it.
func TestRevokedCertificateIsRefused(t *testing.T) {
	ca := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	p := Paths{SSLDir: serverSSL, CertName: "puppet.example.com"}

	compilerSSL := ca.SSLDir(t, "compiler01.example.com", "openvox_compiler")
	compiler := testca.CertFor(t, compilerSSL, "compiler01.example.com")

	cfg, _, err := p.ServerTLS(ServerPolicy{AllowedRoles: []string{"openvox_compiler"}})
	if err != nil {
		t.Fatal(err)
	}

	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{compiler, ca.Cert}}
	if err := cfg.VerifyConnection(state); err != nil {
		t.Fatalf("an unrevoked compiler was refused: %v", err)
	}

	// Revoke it the way an operator does, by rewriting the CA's CRL. The
	// publisher keeps running: revocation is incident response, and requiring a
	// restart on every publisher would make it slower exactly when it matters.
	ca.WriteCRL(t, serverSSL, compiler)
	touchCRL(t, serverSSL)

	err = cfg.VerifyConnection(state)
	if err == nil {
		t.Fatal("a revoked compiler was admitted")
	}
	if !errors.Is(err, ErrCertificateRevoked) {
		t.Errorf("error = %v, want ErrCertificateRevoked", err)
	}
}

// leaf checks only the peer's own certificate; chain checks everything it
// presented, which is Puppet's default and catches a revoked intermediate.
func TestRevocationModeScope(t *testing.T) {
	ca := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	p := Paths{SSLDir: serverSSL, CertName: "puppet.example.com"}

	compilerSSL := ca.SSLDir(t, "compiler01.example.com", "openvox_compiler")
	compiler := testca.CertFor(t, compilerSSL, "compiler01.example.com")

	// Revoke the issuer rather than the leaf.
	ca.WriteCRL(t, serverSSL, ca.Cert)

	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{compiler, ca.Cert}}

	leaf, _, err := p.ServerTLS(ServerPolicy{
		AllowedRoles: []string{"openvox_compiler"},
		Revocation:   RevocationLeaf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyConnection(state); err != nil {
		t.Errorf("leaf mode should not inspect the issuer, got %v", err)
	}

	chain, _, err := p.ServerTLS(ServerPolicy{
		AllowedRoles: []string{"openvox_compiler"},
		Revocation:   RevocationChain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.VerifyConnection(state); !errors.Is(err, ErrCertificateRevoked) {
		t.Errorf("chain mode missed a revoked issuer, got %v", err)
	}
}

// Disabling revocation is an explicit choice, and it must not require a CRL to
// be present at all — that is the point of the escape hatch.
func TestRevocationDisabledSkipsTheCRL(t *testing.T) {
	ca := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	compilerSSL := ca.SSLDir(t, "compiler01.example.com", "openvox_compiler")
	compiler := testca.CertFor(t, compilerSSL, "compiler01.example.com")

	// Revoke the compiler, then remove the CRL entirely.
	ca.WriteCRL(t, serverSSL, compiler)
	if err := os.Remove(filepath.Join(serverSSL, "crl.pem")); err != nil {
		t.Fatal(err)
	}

	p := Paths{SSLDir: serverSSL, CertName: "puppet.example.com"}
	cfg, _, err := p.ServerTLS(ServerPolicy{
		AllowedRoles: []string{"openvox_compiler"},
		Revocation:   RevocationDisabled,
	})
	if err != nil {
		t.Fatalf("revocation disabled should not need a CRL: %v", err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{compiler, ca.Cert}}
	if err := cfg.VerifyConnection(state); err != nil {
		t.Errorf("revocation disabled still refused a peer: %v", err)
	}
}

// A missing CRL with revocation enabled must fail at startup, not on the first
// compiler that connects, and must never degrade to "nothing is revoked".
func TestMissingCRLIsAStartupError(t *testing.T) {
	ca := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")
	if err := os.Remove(filepath.Join(serverSSL, "crl.pem")); err != nil {
		t.Fatal(err)
	}

	p := Paths{SSLDir: serverSSL, CertName: "puppet.example.com"}
	if _, _, err := p.ServerTLS(ServerPolicy{AllowedRoles: []string{"openvox_compiler"}}); err == nil {
		t.Fatal("a missing CRL was accepted; revocation would have silently done nothing")
	}
}

// A CRL signed by some other authority proves nothing. Accepting one would let
// anyone who can write the file revoke the whole estate.
func TestCRLFromAnotherCAIsRejected(t *testing.T) {
	ca := testca.New(t)
	other := testca.New(t)
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	if err := os.WriteFile(filepath.Join(serverSSL, "crl.pem"), other.CRL(t), 0o600); err != nil {
		t.Fatal(err)
	}

	p := Paths{SSLDir: serverSSL, CertName: "puppet.example.com"}
	if _, _, err := p.ServerTLS(ServerPolicy{AllowedRoles: []string{"openvox_compiler"}}); err == nil {
		t.Fatal("a CRL signed by an unrelated CA was accepted")
	}
}

// touchCRL advances the CRL's mtime so the reload is deterministic. Two CRLs of
// the same shape can be byte-identical in length and land in the same
// coarse-grained timestamp, which would look unchanged to the stat check; a real
// revocation minutes or days later never does.
func touchCRL(t *testing.T, ssldir string) {
	t.Helper()
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(ssldir, "crl.pem"), later, later); err != nil {
		t.Fatal(err)
	}
}
