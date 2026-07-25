package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
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

	srv, _, err := p.ServerTLS(ServerPolicy{AllowedRoles: []string{"openvox_compiler"}})
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
func TestVerifyConnectionIdentity(t *testing.T) {
	ca := testca.New(t)
	verify := VerifyConnectionIdentity([]string{"openvox_compiler", "openvox_server"}, nil)

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
		if !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("got %v, want ErrNotAuthorized", err)
		}
	})

	t.Run("certificate without pp_role is refused", func(t *testing.T) {
		certPEM, _ := ca.Issue(t, "plain.example.com", "", false)
		if err := verify(stateFor(t, certPEM)); !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("got %v, want ErrNotAuthorized", err)
		}
	})

	t.Run("no certificate is refused", func(t *testing.T) {
		if err := verify(tls.ConnectionState{}); err == nil {
			t.Error("expected an error when no certificate is presented")
		}
	})
}

// The brownfield path: an estate whose compilers were enrolled long before
// anyone had heard of codavox has no pp_role on any certificate, and adding one
// means re-issuing every certificate. Naming them admits them today.
func TestVerifyConnectionIdentityByCertname(t *testing.T) {
	ca := testca.New(t)

	stateFor := func(t *testing.T, certPEM []byte) tls.ConnectionState {
		t.Helper()
		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}

	t.Run("named certname is admitted without any pp_role", func(t *testing.T) {
		verify := VerifyConnectionIdentity(nil, []string{"old-compiler.example.com"})
		certPEM, _ := ca.Issue(t, "old-compiler.example.com", "", false)
		if err := verify(stateFor(t, certPEM)); err != nil {
			t.Errorf("named certname rejected: %v", err)
		}
	})

	t.Run("an unnamed node is still refused", func(t *testing.T) {
		verify := VerifyConnectionIdentity(nil, []string{"old-compiler.example.com"})
		certPEM, _ := ca.Issue(t, "web01.example.com", "", false)
		if err := verify(stateFor(t, certPEM)); !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("got %v, want ErrNotAuthorized", err)
		}
	})

	// The migration state: some certificates re-issued with a role, the rest
	// still named. Both have to work at once or the estate cannot move
	// incrementally, which is the whole point.
	t.Run("roles and certnames both admit", func(t *testing.T) {
		verify := VerifyConnectionIdentity([]string{"openvox_compiler"}, []string{"old-compiler.example.com"})

		reissued, _ := ca.Issue(t, "new-compiler.example.com", "openvox_compiler", false)
		if err := verify(stateFor(t, reissued)); err != nil {
			t.Errorf("re-issued compiler rejected: %v", err)
		}

		legacy, _ := ca.Issue(t, "old-compiler.example.com", "", false)
		if err := verify(stateFor(t, legacy)); err != nil {
			t.Errorf("legacy compiler rejected: %v", err)
		}

		other, _ := ca.Issue(t, "web01.example.com", "webserver", false)
		if err := verify(stateFor(t, other)); !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("unrelated node admitted: %v", err)
		}
	})

	// Exact match only. A certname allowlist that matched loosely would be a
	// quiet way to admit more than was written down.
	t.Run("matching is exact", func(t *testing.T) {
		verify := VerifyConnectionIdentity(nil, []string{"compiler.example.com"})
		for _, cn := range []string{
			"compiler.example.com.attacker.net",
			"evil-compiler.example.com",
			"compiler.example.co",
		} {
			certPEM, _ := ca.Issue(t, cn, "", false)
			if err := verify(stateFor(t, certPEM)); !errors.Is(err, ErrNotAuthorized) {
				t.Errorf("%q was admitted by a %q allowlist", cn, "compiler.example.com")
			}
		}
	})

	// The error has to say which check failed, or an operator cannot tell a
	// missing pp_role from a wrong one without inspecting the certificate.
	t.Run("the refusal says why", func(t *testing.T) {
		verify := VerifyConnectionIdentity([]string{"openvox_compiler"}, nil)

		none, _ := ca.Issue(t, "plain.example.com", "", false)
		if err := verify(stateFor(t, none)); err == nil || !strings.Contains(err.Error(), "carries no pp_role") {
			t.Errorf("missing-role error unhelpful: %v", err)
		}

		wrong, _ := ca.Issue(t, "web01.example.com", "webserver", false)
		if err := verify(stateFor(t, wrong)); err == nil || !strings.Contains(err.Error(), `pp_role "webserver"`) {
			t.Errorf("wrong-role error does not name the role: %v", err)
		}
	})
}

// Authorizing nobody would admit every node the CA ever signed, so it has to be
// refused at construction rather than discovered in production.
func TestServerTLSRequiresAnAllowlist(t *testing.T) {
	ca := testca.New(t)
	p := Paths{SSLDir: ca.SSLDir(t, "puppet.example.com", "openvox_server"), CertName: "puppet.example.com"}

	if _, _, err := p.ServerTLS(ServerPolicy{}); err == nil {
		t.Error("ServerTLS with neither roles nor certnames should fail rather than admit everything")
	}
	if _, _, err := p.ServerTLS(ServerPolicy{AllowedCertnames: []string{"compiler.example.com"}}); err != nil {
		t.Errorf("certnames alone should be a valid allowlist: %v", err)
	}
}

func TestServerTLSRequiresARole(t *testing.T) {
	ca := testca.New(t)
	p := Paths{SSLDir: ca.SSLDir(t, "puppet.example.com", "openvox_server"), CertName: "puppet.example.com"}
	if _, _, err := p.ServerTLS(ServerPolicy{}); err == nil {
		t.Error("ServerTLS with no allowed roles should fail rather than admit everything")
	}
}

// The fleet view is read from the node the publisher runs on, using that node's
// own certificate — which does not carry a compiler role and which nobody
// thinks to add to an allowlist. Admitting it is what makes the operator
// command work with no extra configuration.
func TestServerTLSAdmitsItsOwnCertname(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	paths := Paths{SSLDir: ssldir, CertName: "puppet.example.com"}
	cfg, _, err := paths.ServerTLS(ServerPolicy{
		AllowedRoles: []string{"openvox_compiler"},
		Revocation:   RevocationDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	self := connectionState(t, ca, "puppet.example.com", "openvox_server")
	if err := cfg.VerifyConnection(self); err != nil {
		t.Errorf("the publisher refused its own certificate: %v", err)
	}

	// It admits itself, not every node without a role. Another node's
	// certificate is still refused.
	other := connectionState(t, ca, "web01.example.com", "")
	if err := cfg.VerifyConnection(other); err == nil {
		t.Error("a node with no role and no listing was admitted")
	}
}

// connectionState builds what a completed handshake with this peer would leave.
func connectionState(t *testing.T, ca *testca.CA, certname, role string) tls.ConnectionState {
	t.Helper()
	certPEM, _ := ca.Issue(t, certname, role, false)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
}
