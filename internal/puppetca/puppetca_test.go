package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/miharp/codavox/internal/testca"
)

func TestPathsUsePuppetLayout(t *testing.T) {
	p := Paths{SSLDir: DefaultSSLDir, CertName: "compiler01.example.com"}

	want := map[string]string{
		p.Cert():   "/etc/puppetlabs/puppet/ssl/certs/compiler01.example.com.pem",
		p.Key():    "/etc/puppetlabs/puppet/ssl/private_keys/compiler01.example.com.pem",
		p.CACert(): "/etc/puppetlabs/puppet/ssl/certs/ca.pem",
		p.CRL():    "/etc/puppetlabs/puppet/ssl/crl.pem",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("got %s, want %s", got, expected)
		}
	}
}

func TestLoad(t *testing.T) {
	ca := testca.New(t)
	p := Paths{SSLDir: ca.SSLDir(t, "compiler01.example.com", "openvox_compiler"), CertName: "compiler01.example.com"}

	if _, pool, err := p.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	} else if pool == nil {
		t.Fatal("Load returned a nil CA pool")
	}

	t.Run("missing material is an error", func(t *testing.T) {
		missing := Paths{SSLDir: t.TempDir(), CertName: "absent"}
		if _, _, err := missing.Load(); err == nil {
			t.Error("expected an error for a missing keypair")
		}
	})
}

func TestServerAndClientTLS(t *testing.T) {
	ca := testca.New(t)
	p := Paths{SSLDir: ca.SSLDir(t, "puppet.example.com", "openvox_server"), CertName: "puppet.example.com"}

	srv, err := p.ServerTLS("openvox_compiler")
	if err != nil {
		t.Fatal(err)
	}
	// A publisher that does not require a verified client certificate would
	// serve every environment's code to anything that can reach the port.
	if srv.ClientAuth != 4 { // tls.RequireAndVerifyClientCert
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", srv.ClientAuth)
	}
	if srv.ClientCAs == nil {
		t.Error("server config has no ClientCAs")
	}
	// VerifyPeerCertificate is skipped on resumed sessions, so the role check
	// must live in VerifyConnection or it can be bypassed by reconnecting.
	if srv.VerifyConnection == nil {
		t.Error("server config has no VerifyConnection; the role check would not run on resumed sessions")
	}
	if srv.MinVersion < 0x0303 { // tls.VersionTLS12
		t.Errorf("MinVersion = %x, want at least TLS 1.2", srv.MinVersion)
	}

	cli, err := p.ClientTLS()
	if err != nil {
		t.Fatal(err)
	}
	if cli.RootCAs == nil {
		t.Error("client config has no RootCAs")
	}
	if cli.InsecureSkipVerify {
		t.Error("client config skips verification")
	}
}

func TestPPRole(t *testing.T) {
	ca := testca.New(t)

	parse := func(t *testing.T, certPEM []byte) *x509.Certificate {
		t.Helper()
		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}

	t.Run("utf8string encoded", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "c1.example.com", "openvox_compiler", false)
		role, ok := PPRole(parse(t, certPEM))
		if !ok || role != "openvox_compiler" {
			t.Errorf("PPRole = (%q, %v), want (openvox_compiler, true)", role, ok)
		}
	})

	// Tolerated because rejecting an otherwise valid certificate over an
	// encoding detail would lock a node out of code deploys entirely.
	t.Run("bare string falls back", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "c2.example.com", "openvox_compiler", true)
		role, ok := PPRole(parse(t, certPEM))
		if !ok || role != "openvox_compiler" {
			t.Errorf("PPRole = (%q, %v), want (openvox_compiler, true)", role, ok)
		}
	})

	t.Run("absent extension", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "agent.example.com", "", false)
		if role, ok := PPRole(parse(t, certPEM)); ok {
			t.Errorf("PPRole = (%q, true), want not found", role)
		}
	})
}

// Authentication by CA alone is not enough: every agent in the estate holds a
// certificate from the same CA, and Puppet manifests routinely reference
// internal hostnames, credential paths, and topology.
func TestVerifyConnectionRole(t *testing.T) {
	ca := testca.New(t)
	verify := VerifyConnectionRole("openvox_compiler", "openvox_server")

	stateFor := func(t *testing.T, certPEM []byte) tls.ConnectionState {
		t.Helper()
		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}

	t.Run("permitted role is admitted", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "compiler01.example.com", "openvox_compiler", false)
		if err := verify(stateFor(t, certPEM)); err != nil {
			t.Errorf("compiler rejected: %v", err)
		}
	})

	t.Run("ordinary agent is refused", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "webserver01.example.com", "webserver", false)
		err := verify(stateFor(t, certPEM))
		if err == nil {
			t.Fatal("an agent with an unrelated role was admitted")
		}
		if !errors.Is(err, ErrRoleMismatch) {
			t.Errorf("got %v, want ErrRoleMismatch", err)
		}
	})

	t.Run("certificate without pp_role is refused", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "plain.example.com", "", false)
		if err := verify(stateFor(t, certPEM)); !errors.Is(err, ErrRoleMismatch) {
			t.Errorf("got %v, want ErrRoleMismatch", err)
		}
	})

	t.Run("no certificate is refused", func(t *testing.T) {
		if err := verify(tls.ConnectionState{}); err == nil {
			t.Error("expected an error when no certificate is presented")
		}
	})
}

func TestServerTLSRequiresARole(t *testing.T) {
	ca := testca.New(t)
	p := Paths{SSLDir: ca.SSLDir(t, "puppet.example.com", "openvox_server"), CertName: "puppet.example.com"}
	if _, err := p.ServerTLS(); err == nil {
		t.Error("ServerTLS with no allowed roles should fail rather than admit everything")
	}
}
