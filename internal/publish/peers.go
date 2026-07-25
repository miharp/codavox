package publish

import (
	"crypto/tls"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miharp/codavox/internal/layout"
)

// Peers records what the publisher has observed each compiler do.
//
// It answers the question a fleet cannot otherwise answer: "are my compilers on
// the current code?" Without it, convergence — the property this project exists
// to provide — is only visible by running `codavox code-id` on every node in
// turn, which is fine at four compilers and useless at forty. The publisher is
// the one place that already sees them all, because every compiler authenticates
// to it by certificate on every poll.
//
// # Two kinds of fact
//
// Serving is what the compiler reported about itself, read from the same
// environment symlink code-id reads. That is the compiler's own answer, so
// comparing it against the current versions says who has converged.
//
// Fetched and the counters are what the publisher observed. They are weaker: an
// agent that fetched an artifact can still fail to verify or unpack it, and
// would then keep serving the previous version — which is exactly why the agent
// reports the symlink rather than letting the publisher infer from downloads.
//
// A compiler running an older agent sends no report, so it shows fetches and no
// serving. That is a version skew to notice, not an outage.
//
// It is deliberately in memory and best effort. Persisting it would create a
// second store of state the symlink already owns, with its own staleness and
// failure modes, to answer a question that is diagnostic rather than
// load-bearing. A publisher restart empties it, and it refills as compilers
// poll — within one interval for a healthy fleet.
type Peers struct {
	mu   sync.Mutex
	seen map[string]*peerState
}

type peerState struct {
	lastSeen   time.Time
	lastPoll   time.Time
	fetched    map[string]fetchRecord // environment -> what it pulled
	serving    map[string]string      // environment -> what it says it serves
	servingAt  time.Time
	polls      uint64
	fetchCount uint64
}

type fetchRecord struct {
	codeID string
	at     time.Time
}

// Peer is one compiler, as the publisher has observed it.
type Peer struct {
	Certname string `json:"certname"`
	// LastSeen is any authenticated request, so a compiler that is polling but
	// has nothing to fetch still looks alive.
	LastSeen time.Time `json:"last_seen"`
	// LastPoll is the last time it asked which versions are current.
	LastPoll time.Time `json:"last_poll,omitzero"`
	// Serving is what the compiler reported it is serving, read from its own
	// environment symlinks. This is the compiler's answer, not an inference:
	// compare it against the current versions to see who has converged.
	Serving map[string]string `json:"serving,omitempty"`
	// ServingAt is when that report arrived.
	ServingAt time.Time `json:"serving_at,omitzero"`
	// Fetched is the most recent artifact pulled per environment. A compiler
	// already holding the current version never appears here for it, which is
	// the normal steady state rather than a problem.
	Fetched map[string]PeerFetch `json:"fetched,omitempty"`
	Polls   uint64               `json:"polls"`
	Fetches uint64               `json:"fetches"`
}

// PeerFetch is one artifact download.
type PeerFetch struct {
	CodeID string    `json:"code_id"`
	At     time.Time `json:"at"`
}

// NewPeers returns an empty observation set.
func NewPeers() *Peers { return &Peers{seen: map[string]*peerState{}} }

// state returns the record for a certname, creating it. Callers hold p.mu.
//
// Entries are never evicted. The set is bounded by who is authorized to reach
// the publisher at all — a pp_role or an explicit certname the operator
// configured — so it cannot grow beyond the estate.
func (p *Peers) state(certname string) *peerState {
	st, ok := p.seen[certname]
	if !ok {
		st = &peerState{fetched: map[string]fetchRecord{}}
		p.seen[certname] = st
	}
	return st
}

// observePoll records that a compiler asked which versions are current.
func (p *Peers) observePoll(certname string, at time.Time) {
	if p == nil || certname == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(certname)
	st.lastSeen = at
	st.lastPoll = at
	st.polls++
}

// observeFetch records that a compiler pulled an environment's artifact.
func (p *Peers) observeFetch(certname, env, codeID string, at time.Time) {
	if p == nil || certname == "" || env == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(certname)
	st.lastSeen = at
	st.fetched[env] = fetchRecord{codeID: codeID, at: at}
	st.fetchCount++
}

// List returns every observed compiler, most recently seen first.
func (p *Peers) List() []Peer {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Peer, 0, len(p.seen))
	for certname, st := range p.seen {
		peer := Peer{
			Certname:  certname,
			LastSeen:  st.lastSeen,
			LastPoll:  st.lastPoll,
			ServingAt: st.servingAt,
			Polls:     st.polls,
			Fetches:   st.fetchCount,
		}
		if len(st.serving) > 0 {
			peer.Serving = make(map[string]string, len(st.serving))
			for env, id := range st.serving {
				peer.Serving[env] = id
			}
		}
		if len(st.fetched) > 0 {
			peer.Fetched = make(map[string]PeerFetch, len(st.fetched))
			for env, f := range st.fetched {
				peer.Fetched[env] = PeerFetch{CodeID: f.codeID, At: f.at}
			}
		}
		out = append(out, peer)
	}

	// Most recently seen first, then by name so the order is stable when two
	// compilers were seen in the same instant.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Certname < out[j].Certname
	})
	return out
}

// peerCertname is the common name of the certificate a request presented, or ""
// when there is none — a plaintext test server, or a route reached without
// mutual TLS.
func peerCertname(cs *tls.ConnectionState) string {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return ""
	}
	return cs.PeerCertificates[0].Subject.CommonName
}

// ParseServing reads the environment=code_id pairs an agent reported.
//
// Malformed pairs are skipped rather than failing the request: this is
// diagnostic, and a poll must never break because a compiler sent something
// unexpected. Values are validated, so a peer cannot inject arbitrary text into
// the fleet view.
func ParseServing(header string) map[string]string {
	if header == "" || len(header) > maxServingHeader {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(header, ",") {
		env, codeID, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		if layout.ValidateEnvironment(env) != nil || layout.ValidateCodeID(codeID) != nil {
			continue
		}
		out[env] = codeID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// observeServing records what a compiler said it is serving.
func (p *Peers) observeServing(certname string, serving map[string]string, at time.Time) {
	if p == nil || certname == "" || len(serving) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(certname)
	st.lastSeen = at
	st.serving = serving
	st.servingAt = at
}
