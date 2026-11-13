package puppetca

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// material is one read of the node's certificate, private key, and CA bundle.
type material struct {
	cert tls.Certificate
	pool *x509.CertPool
	// cas are the parsed CA certificates, which CRL verification needs: a pool
	// can only build chains, and checking a CRL's signature requires the
	// issuing certificate itself.
	cas []*x509.Certificate
}

// source hands out a node's SSL material and re-reads it when a file changes.
//
// A daemon that read these files only at startup would keep presenting a
// certificate the CA has since replaced, and keep trusting a CA certificate
// whose end date has since been extended, until someone restarted it. Neither
// shows while the old copies are still valid, so the restart is the step that
// gets forgotten: the daemon carries on for months and then fails at the old
// end date, long after the change that should have prevented it. The CRL has
// always been re-read for the same reason; this does it for the rest of the
// ssldir.
type source struct {
	paths Paths

	mu     sync.Mutex
	stamps [3]fileStamp
	cur    *material
}

// fileStamp is the cheap change signal: a stat per check, and a re-read only
// when the file is actually different.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func statFile(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modTime: fi.ModTime(), size: fi.Size()}, nil
}

// open reads the material once, so that missing or malformed files are a
// startup error rather than a failure on the first connection.
func (p Paths) open() (*source, *material, error) {
	s := &source{paths: p}
	m, err := s.current()
	if err != nil {
		return nil, nil, err
	}
	return s, m, nil
}

// current returns the material as it is on disk now.
//
// A file that has become unreadable, or a certificate and key that no longer
// match, is an error and not a reason to carry on with the previous copy:
// continuing to trust a CA bundle the operator has replaced is the quiet
// downgrade the CRL check refuses for the same reason. The caller fails the
// connection, and the next one reads the files again, so a reader that lands
// between the writes of a new certificate and its key recovers by itself.
func (s *source) current() (*material, error) {
	var stamps [3]fileStamp
	for i, path := range []string{s.paths.Cert(), s.paths.Key(), s.paths.CACert()} {
		stamp, err := statFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		stamps[i] = stamp
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && stamps == s.stamps {
		return s.cur, nil
	}
	m, err := s.paths.load()
	if err != nil {
		return nil, err
	}
	// The stamps were taken before the read. A write that lands between the two
	// leaves them older than the file, so the next call reads it again.
	s.cur, s.stamps = m, stamps
	return m, nil
}

// issuers returns the CA certificates on disk now, for verifying a CRL.
func (s *source) issuers() ([]*x509.Certificate, error) {
	m, err := s.current()
	if err != nil {
		return nil, err
	}
	return m.cas, nil
}

// serverProtos is what net/http offers on a TLS listener by default. A
// configuration returned from GetConfigForClient replaces the listener's own
// for that handshake, protocol list included, so it has to say so itself or
// HTTP/2 clients would be quietly downgraded.
var serverProtos = []string{"h2", "http/1.1"}

// serverConfig returns a server configuration that answers each handshake with
// the material on disk at that moment.
//
// build turns one read of the material into a complete configuration. The
// returned configuration is the one built from first, so its fields describe
// what the listener does; GetConfigForClient then swaps in a configuration
// built from the current material, which is how crypto/tls lets ClientCAs
// change under a running listener. Connections already established keep the
// material they were made with.
func (s *source) serverConfig(first *material, build func(*material) *tls.Config) *tls.Config {
	var (
		mu   sync.Mutex
		from = first
		cfg  = build(first)
	)
	outer := build(first)
	outer.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		m, err := s.current()
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if m != from {
			from, cfg = m, build(m)
		}
		return cfg, nil
	}
	return outer
}

// clientConfig is the TLS configuration for a compiler fetching artifacts.
func clientConfig(m *material) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.cert},
		RootCAs:      m.pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// reloadingTransport is an HTTP transport that follows the material on disk.
//
// Each read of the material gets a transport of its own, and a request goes to
// the one for the material on disk when it is made. Replacing the transport,
// instead of only its configuration, is what stops a keep-alive connection
// from outliving the change: the agent polls over one connection for as long
// as it runs, and that connection would go on presenting the certificate it
// was opened with, including one the CA has revoked and re-issued.
type reloadingTransport struct {
	src *source

	mu   sync.Mutex
	from *material
	base *http.Transport
}

// RoundTrip sends the request with the material on disk now.
func (t *reloadingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m, err := t.src.current()
	if err != nil {
		// A RoundTripper closes the body on every path, errors included.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}

	t.mu.Lock()
	if m != t.from {
		// A request still in flight finishes on its old connection, which is
		// then left for the server's idle timeout to close.
		t.base.CloseIdleConnections()
		t.from, t.base = m, newTransport(m)
	}
	base := t.base
	t.mu.Unlock()

	return base.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach through.
func (t *reloadingTransport) CloseIdleConnections() {
	t.mu.Lock()
	base := t.base
	t.mu.Unlock()
	base.CloseIdleConnections()
}

func newTransport(m *material) *http.Transport {
	return &http.Transport{TLSClientConfig: clientConfig(m)}
}
