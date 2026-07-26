package publish

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

func TestPeersRecordsPollsAndFetches(t *testing.T) {
	p := NewPeers()
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	p.observePoll("compiler01.example.com", base)
	p.observeFetch("compiler01.example.com", "production", "abc123", base.Add(time.Second))
	p.observePoll("compiler01.example.com", base.Add(30*time.Second))

	list := p.List()
	if len(list) != 1 {
		t.Fatalf("got %d peers, want 1", len(list))
	}
	got := list[0]

	if got.Certname != "compiler01.example.com" {
		t.Errorf("certname = %q", got.Certname)
	}
	if got.Polls != 2 || got.Fetches != 1 {
		t.Errorf("polls=%d fetches=%d, want 2 and 1", got.Polls, got.Fetches)
	}
	// last_seen tracks any request, so a compiler with nothing to fetch still
	// looks alive.
	if !got.LastSeen.Equal(base.Add(30 * time.Second)) {
		t.Errorf("last_seen = %v, want the most recent poll", got.LastSeen)
	}
	if f := got.Fetched["production"]; f.CodeID != "abc123" {
		t.Errorf("fetched production = %+v, want abc123", f)
	}
}

// The steady state, and the thing easiest to get wrong: a converged compiler
// polls constantly and fetches nothing. Reading "no recent fetch" as "not
// converged" would report a healthy fleet as broken.
func TestPeersConvergedCompilerPollsWithoutFetching(t *testing.T) {
	p := NewPeers()
	now := time.Now().UTC()

	for i := range 10 {
		p.observePoll("compiler01.example.com", now.Add(time.Duration(i)*30*time.Second))
	}

	got := p.List()[0]
	if got.Polls != 10 {
		t.Errorf("polls = %d, want 10", got.Polls)
	}
	if got.Fetches != 0 {
		t.Errorf("fetches = %d, want 0", got.Fetches)
	}
	if len(got.Fetched) != 0 {
		t.Errorf("fetched = %+v, want empty", got.Fetched)
	}
	// It is still visibly alive, which is the whole point.
	if got.LastSeen.IsZero() || got.LastPoll.IsZero() {
		t.Error("a polling compiler has no last_seen or last_poll")
	}
}

func TestPeersOrdersByMostRecentlySeen(t *testing.T) {
	p := NewPeers()
	base := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	p.observePoll("old.example.com", base)
	p.observePoll("recent.example.com", base.Add(time.Minute))
	// Same instant as recent: the tiebreak is the name, so the order is stable.
	p.observePoll("also-recent.example.com", base.Add(time.Minute))

	var names []string
	for _, peer := range p.List() {
		names = append(names, peer.Certname)
	}
	want := []string{"also-recent.example.com", "recent.example.com", "old.example.com"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

// A nil Peers is how a plaintext test server and any caller that does not want
// the fleet view are configured, so every method has to tolerate it.
func TestPeersNilIsInert(t *testing.T) {
	var p *Peers
	p.observePoll("compiler01.example.com", time.Now())
	p.observeFetch("compiler01.example.com", "production", "abc", time.Now())
	if got := p.List(); got != nil {
		t.Errorf("List on a nil Peers = %v, want nil", got)
	}
}

// The publisher serves many compilers at once, so recording must be safe under
// concurrent requests. This is the reason CI runs -race.
func TestPeersConcurrentObservations(t *testing.T) {
	p := NewPeers()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "compiler.example.com"
			if i%2 == 0 {
				name = "other.example.com"
			}
			p.observePoll(name, time.Now().UTC())
			p.observeFetch(name, "production", "abc123", time.Now().UTC())
		}(i)
	}
	wg.Wait()

	var total uint64
	for _, peer := range p.List() {
		total += peer.Polls
	}
	if total != 50 {
		t.Errorf("recorded %d polls, want 50", total)
	}
}

// End to end through the handler: the request path is where certname, route and
// recording actually meet, and a mistake there is invisible to the unit tests.
func TestHandlerRecordsPeersFromRequests(t *testing.T) {
	s := basedir(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
	})
	peers := NewPeers()

	// httptest gives no client certificate, so stand in for what mutual TLS
	// would have put on the request.
	withPeer := func(next http.Handler, cn string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.TLS = fakePeerState(t, cn)
			next.ServeHTTP(w, r)
		})
	}

	srv := httptest.NewServer(withPeer(Handler(s, nil, peers), "compiler01.example.com"))
	defer srv.Close()

	current := s.Environments()["production"]

	resp, err := http.Get(srv.URL + EnvironmentsPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(srv.URL + ArtifactPath("production", current))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	list := peers.List()
	if len(list) != 1 {
		t.Fatalf("got %d peers, want 1", len(list))
	}
	if list[0].Polls != 1 || list[0].Fetches != 1 {
		t.Errorf("polls=%d fetches=%d, want 1 and 1", list[0].Polls, list[0].Fetches)
	}
	// The environment and code_id come from the routed path values, so this
	// catches a wrong prefix or a renamed wildcard.
	if f := list[0].Fetched["production"]; f.CodeID != current {
		t.Errorf("fetched = %+v, want production at %s", list[0].Fetched, current)
	}

	// And the fleet view reports it as JSON.
	resp, err = http.Get(srv.URL + CompilersPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var served []Peer
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 || served[0].Certname != "compiler01.example.com" {
		t.Errorf("served fleet view = %+v", served)
	}
}

// A refused peer must not appear in the fleet view: it is not part of the
// estate, and listing it would suggest otherwise.
func TestHandlerDoesNotRecordRefusedPeers(t *testing.T) {
	s := basedir(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
	})
	peers := NewPeers()

	refuseAll := func(*tls.ConnectionState) error { return errRefused }
	withPeer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.TLS = fakePeerState(t, "intruder.example.com")
			next.ServeHTTP(w, r)
		})
	}

	srv := httptest.NewServer(withPeer(Handler(s, refuseAll, peers)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + EnvironmentsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	if got := peers.List(); len(got) != 0 {
		t.Errorf("a refused peer was recorded: %+v", got)
	}
}

// errRefused stands in for any authorization failure.
var errRefused = errors.New("refused for the test")

// fakePeerState builds the ConnectionState mutual TLS would have produced, so
// the handler tests can exercise the certname path without a real handshake.
func fakePeerState(t *testing.T, cn string) *tls.ConnectionState {
	t.Helper()
	ca := testca.New(t)
	certPEM, _ := ca.Issue(t, cn, "openvox_compiler", false)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
}

func TestParseServing(t *testing.T) {
	const valid = "3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb"

	for _, tc := range []struct {
		name   string
		header string
		want   map[string]string
	}{
		{"empty", "", nil},
		{"one", "production=" + valid, map[string]string{"production": valid}},
		{
			"several with spaces",
			"production=" + valid + ", testing=" + valid,
			map[string]string{"production": valid, "testing": valid},
		},
		// A peer controls this header, so anything it could smuggle through must
		// be dropped rather than stored and later rendered as fleet state.
		{"no separator", "production", nil},
		{"invalid environment", "../etc=" + valid, nil},
		{"invalid code_id", "production=a/b", nil},
		{"charset OpenVox Server rejects", "production=abc+def=", nil},
		// One bad pair must not discard the good ones beside it.
		{"partly valid", "bad, production=" + valid, map[string]string{"production": valid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseServing(tc.header)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseServing(%q) = %v, want %v", tc.header, got, tc.want)
			}
			for env, id := range tc.want {
				if got[env] != id {
					t.Errorf("%s = %q, want %q", env, got[env], id)
				}
			}
		})
	}
}

// An oversized header is refused whole. A compiler cannot make the publisher do
// unbounded work on a request it makes every interval.
func TestParseServingBoundsInput(t *testing.T) {
	if got := ParseServing(strings.Repeat("a=b,", maxServingHeader)); got != nil {
		t.Errorf("an oversized header parsed to %v, want nil", got)
	}
}

// The distinction the whole feature rests on: what a compiler says it serves,
// separate from what the publisher watched it download.
func TestHandlerRecordsSelfReportedServing(t *testing.T) {
	s := basedir(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
	})
	peers := NewPeers()
	current := s.Environments()["production"]

	withPeer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.TLS = fakePeerState(t, "compiler01.example.com")
			next.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewServer(withPeer(Handler(s, nil, peers)))
	defer srv.Close()

	poll := func(serving string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+EnvironmentsPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if serving != "" {
			req.Header.Set(ServingHeader, serving)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	// A compiler still on the previous version reports it, without ever having
	// fetched anything through this publisher.
	stale := "9a1f0c4e2b8d7f36a5c19e04b7d2836af41c9e5d0b8a37f26c1d4e90a5b8c3f7"
	poll("production=" + stale)

	got := peers.List()[0]
	if got.Serving["production"] != stale {
		t.Errorf("serving = %v, want the stale id it reported", got.Serving)
	}
	if len(got.Fetched) != 0 {
		t.Errorf("fetched = %v, want empty: it downloaded nothing", got.Fetched)
	}
	if got.ServingAt.IsZero() {
		t.Error("serving_at is zero after a report")
	}

	// Then it converges, and the report replaces the old one rather than
	// accumulating alongside it.
	poll("production=" + current)
	if got := peers.List()[0]; got.Serving["production"] != current {
		t.Errorf("serving = %v, want the current id", got.Serving)
	}

	// An agent too old to report leaves the last known answer standing, rather
	// than erasing it and looking like a regression.
	poll("")
	if got := peers.List()[0]; got.Serving["production"] != current {
		t.Errorf("a report-less poll changed serving to %v", got.Serving)
	}
}

// List hands out copies. A caller that holds a returned Peer must not be able to
// mutate what the next request records.
func TestPeersListCopiesServing(t *testing.T) {
	p := NewPeers()
	p.observeServing("compiler01.example.com", map[string]string{"production": "abc"}, time.Now().UTC())

	p.List()[0].Serving["production"] = "tampered"

	if got := p.List()[0].Serving["production"]; got != "abc" {
		t.Errorf("serving = %q after a caller mutated its copy, want abc", got)
	}
}
