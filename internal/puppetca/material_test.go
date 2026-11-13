package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

const (
	primary  = "puppet.example.com"
	compiler = "compiler01.example.com"
)

// serve runs an HTTP server on a TLS configuration the way the publisher and
// the deploy server do, and returns its URL. The handler reports the serial of
// the certificate the client presented, if any.
func serve(t *testing.T, cfg *tls.Config, connections *atomic.Int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
		// A refused handshake is the point of several tests; keep it out of
		// the test output.
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) > 0 {
				_, _ = fmt.Fprint(w, r.TLS.PeerCertificates[0].SerialNumber.String())
			}
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew && connections != nil {
				connections.Add(1)
			}
		},
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

// dial makes one request over a new connection and returns the server's
// certificate. A handshake the server refuses comes back as the error.
func dial(t *testing.T, url string, cfg *tls.Config) (*x509.Certificate, error) {
	t.Helper()
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true},
	}
	resp, err := client.Get(url) //nolint:noctx // a test request to a local listener
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.TLS.PeerCertificates[0], nil
}

// get makes one request through a client and returns the body.
func get(t *testing.T, client *http.Client, url string) (string, error) {
	t.Helper()
	resp, err := client.Get(url) //nolint:noctx // a test request to a local listener
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// compilerConfig is a compiler's side of a handshake, read once from its
// ssldir and trusting the given CA.
func compilerConfig(t *testing.T, ca, trust *testca.CA) *tls.Config {
	t.Helper()
	return &tls.Config{
		Certificates: []tls.Certificate{ca.TLSCert(t, compiler, "openvox_compiler")},
		RootCAs:      trust.Pool(t),
		MinVersion:   tls.VersionTLS12,
	}
}

func serverTLS(t *testing.T, ssldir string) *tls.Config {
	t.Helper()
	cfg, _, err := Paths{SSLDir: ssldir, CertName: primary}.ServerTLS(
		ServerPolicy{AllowedRoles: []string{"openvox_compiler"}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A server's certificate can be replaced under a running publisher: ovadm does
// it when the old one has expired, and restoring a backup does it too. The
// publisher was seen on port 8150 still presenting the old certificate while
// OpenVox Server on 8140 presented the new one, until it was restarted.
func TestServerPresentsAReissuedCertificate(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, primary, "openvox_server")
	url := serve(t, serverTLS(t, ssldir), nil)
	client := compilerConfig(t, ca, ca)

	before, err := dial(t, url, client)
	if err != nil {
		t.Fatal(err)
	}

	reissued := ca.Reissue(t, ssldir, primary, "openvox_server")

	after, err := dial(t, url, client)
	if err != nil {
		t.Fatal(err)
	}
	if after.SerialNumber.Cmp(before.SerialNumber) == 0 {
		t.Fatal("the publisher still presents the certificate it started with")
	}
	if after.SerialNumber.Cmp(reissued.SerialNumber) != 0 {
		t.Errorf("the publisher presents serial %s, want the re-issued %s", after.SerialNumber, reissued.SerialNumber)
	}
}

// Extending a CA certificate gives it a later end date and changes nothing
// else. A publisher holding the old copy works until the old end date and then
// refuses every compiler, however long ago the CA was extended on disk.
func TestServerTrustsAnExtendedCA(t *testing.T) {
	expired := testca.NewUntil(t, time.Now().Add(-time.Hour))
	extended := expired.Extend(t, time.Now().Add(24*time.Hour))

	// The publisher started before the extension, so it holds the old copy.
	// The compiler already has the new one.
	ssldir := expired.SSLDir(t, primary, "openvox_server")
	url := serve(t, serverTLS(t, ssldir), nil)
	client := compilerConfig(t, expired, extended)

	if _, err := dial(t, url, client); err == nil {
		t.Fatal("a compiler was admitted against a CA certificate that has expired")
	}

	extended.WriteCA(t, ssldir)

	if _, err := dial(t, url, client); err != nil {
		t.Fatalf("the publisher still refuses compilers after the CA was extended on disk: %v", err)
	}
}

// The files are read again, not trusted to be whole: a key that does not match
// its certificate must fail the connection, not fall back to the previous
// pair, and the next connection after it is put right must succeed.
func TestServerRefusesWhileTheMaterialIsBroken(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, primary, "openvox_server")
	paths := Paths{SSLDir: ssldir, CertName: primary}
	url := serve(t, serverTLS(t, ssldir), nil)
	client := compilerConfig(t, ca, ca)

	if _, err := dial(t, url, client); err != nil {
		t.Fatal(err)
	}

	// A new certificate has landed and its key has not: what a reader sees if
	// it looks between the two writes.
	other, _ := ca.Issue(t, primary, "openvox_server", false)
	replace(t, paths.Cert(), other)

	if _, err := dial(t, url, client); err == nil {
		t.Fatal("the publisher served a connection with a certificate and key that do not match")
	}

	ca.Reissue(t, ssldir, primary, "openvox_server")

	if _, err := dial(t, url, client); err != nil {
		t.Fatalf("the publisher did not recover once the material was whole again: %v", err)
	}
}

// A CA bundle replaced together with its CRL must be the bundle the new CRL is
// verified against. Checked against the bundle read at startup, the new CRL
// would be rejected as coming from an unknown CA and every compiler refused.
func TestCRLIsVerifiedAgainstTheCAOnDisk(t *testing.T) {
	old := testca.New(t)
	ssldir := old.SSLDir(t, primary, "openvox_server")
	cfg := serverTLS(t, ssldir)

	replacement := testca.NewUntil(t, time.Now().Add(48*time.Hour))
	good := testca.ParseCert(t, first(replacement.Issue(t, compiler, "openvox_compiler", false)))
	revoked := testca.ParseCert(t, first(replacement.Issue(t, "compiler02.example.com", "openvox_compiler", false)))

	replacement.WriteCA(t, ssldir)
	replacement.WriteCRL(t, ssldir, revoked)
	touchCRL(t, ssldir)

	state := func(cert *x509.Certificate) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert, replacement.Cert}}
	}
	if err := cfg.VerifyConnection(state(good)); err != nil {
		t.Errorf("a compiler of the new CA was refused: %v", err)
	}
	if err := cfg.VerifyConnection(state(revoked)); !errors.Is(err, ErrCertificateRevoked) {
		t.Errorf("error = %v, want ErrCertificateRevoked", err)
	}
}

// The deploy server presents the node's certificate too, to callers that hold
// no Puppet certificate of their own.
func TestServerCertTLSPresentsAReissuedCertificate(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, primary, "openvox_server")
	cfg, err := Paths{SSLDir: ssldir, CertName: primary}.ServerCertTLS()
	if err != nil {
		t.Fatal(err)
	}
	url := serve(t, cfg, nil)
	client := &tls.Config{RootCAs: ca.Pool(t), MinVersion: tls.VersionTLS12}

	if _, err := dial(t, url, client); err != nil {
		t.Fatal(err)
	}
	reissued := ca.Reissue(t, ssldir, primary, "openvox_server")
	after, err := dial(t, url, client)
	if err != nil {
		t.Fatal(err)
	}
	if after.SerialNumber.Cmp(reissued.SerialNumber) != 0 {
		t.Error("the deploy server still presents the certificate it started with")
	}
}

// Following the ssldir is done by handing crypto/tls a configuration per
// handshake, which replaces the listener's own, protocol list included. HTTP/2
// has to survive that, or a caller that used it would be quietly downgraded.
func TestServerStillOffersHTTP2(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, primary, "openvox_server")
	cfg, err := Paths{SSLDir: ssldir, CertName: primary}.ServerCertTLS()
	if err != nil {
		t.Fatal(err)
	}
	url := serve(t, cfg, nil)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: ca.Pool(t), MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: true,
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(url) //nolint:noctx // a test request to a local listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Errorf("negotiated %s, want HTTP/2", resp.Proto)
	}
}

// Unchanged files must cost a stat and nothing more: the check runs on every
// handshake and every request.
func TestMaterialIsReadOnlyWhenItChanges(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, compiler, "openvox_compiler")
	src, first, err := Paths{SSLDir: ssldir, CertName: compiler}.open()
	if err != nil {
		t.Fatal(err)
	}

	again, err := src.current()
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("unchanged files were read again")
	}

	ca.Reissue(t, ssldir, compiler, "openvox_compiler")
	changed, err := src.current()
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Error("a replaced certificate was not read")
	}
}

// clientTransport is a compiler's long-running client, as the agent builds it.
func clientTransport(t *testing.T, ssldir string) *http.Client {
	t.Helper()
	rt, err := Paths{SSLDir: ssldir, CertName: compiler}.ClientTransport()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: rt}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// peerServer is a publisher-shaped server with material read once, so that
// only the client under test follows the disk.
func peerServer(t *testing.T, ca *testca.CA, connections *atomic.Int32) string {
	t.Helper()
	return serve(t, &tls.Config{
		Certificates: []tls.Certificate{ca.TLSCert(t, primary, "openvox_server")},
		ClientCAs:    ca.Pool(t),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, connections)
}

// The agent polls over one keep-alive connection for as long as it runs. A
// compiler whose certificate was revoked and re-issued has to start presenting
// the new one, which means dropping that connection: the publisher judges
// every request by the certificate the connection was opened with.
func TestClientTransportPresentsAReissuedCertificate(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, compiler, "openvox_compiler")
	issued := testca.CertFor(t, ssldir, compiler)

	var connections atomic.Int32
	url := peerServer(t, ca, &connections)
	client := clientTransport(t, ssldir)

	for range 2 {
		serial, err := get(t, client, url)
		if err != nil {
			t.Fatal(err)
		}
		if serial != issued.SerialNumber.String() {
			t.Fatalf("presented serial %s, want %s", serial, issued.SerialNumber)
		}
	}
	// Without this the test would pass on a client that reconnected for every
	// request, and say nothing about the connection the agent really holds.
	if n := connections.Load(); n != 1 {
		t.Fatalf("two requests used %d connections, want one kept alive", n)
	}

	reissued := ca.Reissue(t, ssldir, compiler, "openvox_compiler")

	serial, err := get(t, client, url)
	if err != nil {
		t.Fatal(err)
	}
	if serial != reissued.SerialNumber.String() {
		t.Errorf("after its certificate was re-issued the agent presented serial %s, want %s", serial, reissued.SerialNumber)
	}
}

// The compiler's side of an extended CA: an agent holding the old copy of the
// CA certificate stops trusting the publisher at the old end date.
func TestClientTransportTrustsAnExtendedCA(t *testing.T) {
	expired := testca.NewUntil(t, time.Now().Add(-time.Hour))
	extended := expired.Extend(t, time.Now().Add(24*time.Hour))

	ssldir := expired.SSLDir(t, compiler, "openvox_compiler")
	url := peerServer(t, extended, nil)
	client := clientTransport(t, ssldir)

	if _, err := get(t, client, url); err == nil {
		t.Fatal("the agent trusted a publisher against a CA certificate that has expired")
	}

	extended.WriteCA(t, ssldir)

	if _, err := get(t, client, url); err != nil {
		t.Fatalf("the agent still refuses the publisher after the CA was extended on disk: %v", err)
	}
}

// As on the server: a request made while the files do not fit together fails
// with the reason, and the one after they are put right succeeds.
func TestClientTransportRefusesWhileTheMaterialIsBroken(t *testing.T) {
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, compiler, "openvox_compiler")
	paths := Paths{SSLDir: ssldir, CertName: compiler}
	url := peerServer(t, ca, nil)
	client := clientTransport(t, ssldir)

	if _, err := get(t, client, url); err != nil {
		t.Fatal(err)
	}

	other, _ := ca.Issue(t, compiler, "openvox_compiler", false)
	replace(t, paths.Cert(), other)

	_, err := get(t, client, url)
	if err == nil {
		t.Fatal("a request went out with a certificate and key that do not match")
	}
	if !strings.Contains(err.Error(), compiler) {
		t.Errorf("the error does not name the keypair: %v", err)
	}

	ca.Reissue(t, ssldir, compiler, "openvox_compiler")

	if _, err := get(t, client, url); err != nil {
		t.Fatalf("the agent did not recover once the material was whole again: %v", err)
	}
}

// replace overwrites one file and moves its mtime on, as testca does.
func replace(t *testing.T, path string, data []byte) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	later := fi.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func first(certPEM, _ []byte) []byte { return certPEM }
