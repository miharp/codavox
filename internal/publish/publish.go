// Package publish serves environment versions and artifacts to compilers.
package publish

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/seal"
)

// sealedEnv is a published environment: its code_id and the materialized
// artifact that reproduces it.
type sealedEnv struct {
	codeID   string
	artifact string // path to the deterministic .tar.gz
}

// Store holds sealed environments and the artifacts that reproduce them.
type Store struct {
	// BaseDir contains one directory per environment, as r10k deploys it.
	BaseDir string
	// ArtifactDir holds the materialized .tar.gz for each current version.
	// Serving reads from here, never from BaseDir, so a compiler can never
	// observe a half-written tree from an r10k deploy that is still in progress.
	ArtifactDir string

	mu     sync.RWMutex
	sealed map[string]sealedEnv // environment -> {code_id, artifact}

	// prov, when set, records where each sealed tree came from. It is optional
	// because it is diagnostic: a Store with no log still seals and serves.
	prov *Log
}

// NewStore returns a Store reading environments from baseDir and writing
// materialized artifacts under artifactDir.
func NewStore(baseDir, artifactDir string) *Store {
	return &Store{BaseDir: baseDir, ArtifactDir: artifactDir, sealed: map[string]sealedEnv{}}
}

// EnableProvenance makes Reseal capture control-repo provenance into log.
func (s *Store) EnableProvenance(log *Log) { s.prov = log }

// Reseal rescans the basedir directory, updates the published code_ids, and
// materializes an immutable artifact for each current version.
//
// Sealing is not done per request. It walks and hashes an entire environment,
// far too expensive to repeat for every polling compiler, and two compilers
// polling either side of an r10k run could otherwise observe different ids for
// what is meant to be one deploy.
//
// Materializing the artifact here — rather than tarring the basedir directory
// when a compiler asks for it — is what makes serving safe while r10k is
// deploying. The bytes a compiler downloads are a snapshot taken when Reseal
// ran, not whatever the basedir directory happens to hold at request time, so a
// deploy in progress can never be streamed as a corrupt, half-written artifact.
func (s *Store) Reseal() error {
	if s.ArtifactDir == "" {
		return fmt.Errorf("store has no artifact directory")
	}

	entries, err := os.ReadDir(s.BaseDir)
	if err != nil {
		return fmt.Errorf("reading basedir directory: %w", err)
	}

	s.mu.RLock()
	prev := s.sealed
	s.mu.RUnlock()

	next := map[string]sealedEnv{}
	var failures []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		env := e.Name()
		// Skip rather than fail: one badly named directory in the basedir area
		// should not stop every other environment from being published.
		if layout.ValidateEnvironment(env) != nil {
			continue
		}
		envDir := filepath.Join(s.BaseDir, env)
		id, err := seal.CodeID(envDir)
		if err != nil {
			// One environment failing must not stop the others being published,
			// the same rule the agent applies when converging. An unreadable file
			// or a stray socket in one module would otherwise block every
			// environment's deploy — and, at startup, stop the publisher coming up
			// at all, so no compiler in the estate could update.
			//
			// This does not soften "no fallbacks": the failed environment is
			// reported and keeps whatever version it was already serving. Nothing
			// is served under a code_id that does not describe it.
			s.carryForward(next, prev, env, &failures, fmt.Errorf("sealing %s: %w", env, err))
			continue
		}

		artifact := filepath.Join(s.ArtifactDir, layout.VersionDirName(env, id)+".tar.gz")
		// Reuse the artifact when content is unchanged; only re-materialize on a
		// new code_id, or if the file went missing since the last reseal.
		if prev[env].codeID != id || !fileExists(artifact) {
			if err := materializeArtifact(artifact, envDir); err != nil {
				s.carryForward(next, prev, env, &failures,
					fmt.Errorf("materializing artifact for %s: %w", env, err))
				continue
			}
		}
		next[env] = sealedEnv{codeID: id, artifact: artifact}

		// Provenance is best-effort and must never fail a reseal: a deploy does
		// not depend on knowing which commit produced it. A missing or malformed
		// .r10k-deploy.json simply yields no record.
		if s.prov != nil {
			if d, ok := readDeployRecord(envDir); ok {
				_ = s.prov.Record(Provenance{
					CodeID:     id,
					Env:        env,
					Commit:     d.Signature,
					DeployedAt: d.FinishedAt,
					SealedAt:   time.Now().UTC(),
				})
			}
		}
	}

	s.mu.Lock()
	old := s.sealed
	s.sealed = next
	s.mu.Unlock()

	reapArtifacts(old, next)

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%w: %s", ErrPartialReseal, strings.Join(failures, "; "))
	}
	return nil
}

// ErrPartialReseal means some environments sealed and others did not. Callers
// distinguish it from a hard failure — an unreadable basedir directory, say —
// because the publisher can still serve everything that did seal, and refusing
// to start over one broken environment would strand the whole estate.
var ErrPartialReseal = errors.New("reseal incomplete")

// carryForward keeps an environment on the version it was already serving when
// this reseal could not produce a new one.
//
// Dropping it instead would unpublish working code because of an unrelated
// failure, and every polling compiler would see the environment vanish. Keeping
// the last good version is the honest answer to "what is this environment?" —
// the failure is reported separately, and the operator's next deploy retries.
func (s *Store) carryForward(next, prev map[string]sealedEnv, env string, failures *[]string, err error) {
	*failures = append(*failures, err.Error())
	if last, ok := prev[env]; ok && fileExists(last.artifact) {
		next[env] = last
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// materializeArtifact writes srcDir as a deterministic gzipped tar at dst.
//
// It writes a temporary file in the same directory and renames it into place,
// so rename(2) publishes the artifact atomically: a reader never sees a
// partially written one.
func materializeArtifact(dst, srcDir string) error {
	// 0700: the artifact directory is publisher-only. The publisher reads these
	// files to stream them; nothing else needs them.
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("creating artifact directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removing tmp is a no-op once it has been renamed away.
	defer func() { _ = os.Remove(tmpName) }()

	if err := seal.WriteArchive(tmp, srcDir); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("installing artifact: %w", err)
	}
	return nil
}

// reapArtifacts removes materialized artifacts that are no longer current.
//
// Only the current version of each environment is servable, so a superseded
// artifact is dead weight. An in-flight download holds an open descriptor, so
// unlinking the file it is streaming is safe on Unix: the bytes stay readable
// until the handle closes.
func reapArtifacts(old, next map[string]sealedEnv) {
	keep := make(map[string]bool, len(next))
	for _, v := range next {
		keep[v.artifact] = true
	}
	for _, v := range old {
		if v.artifact != "" && !keep[v.artifact] {
			_ = os.Remove(v.artifact)
		}
	}
}

// Environments returns the currently published environment to code_id map.
func (s *Store) Environments() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]string, len(s.sealed))
	for k, v := range s.sealed {
		out[k] = v.codeID
	}
	return out
}

// CodeID returns the published code_id for env.
func (s *Store) CodeID(env string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.sealed[env]
	return v.codeID, ok
}

// Artifact returns the materialized artifact path for a current (env, code_id).
//
// A stale or unknown pair returns ok=false. Only the current version is
// servable, because it is the only artifact kept on disk; compilers retain old
// versions themselves, which is what in-flight agent runs actually need.
func (s *Store) Artifact(env, codeID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.sealed[env]
	if !ok || v.codeID != codeID || v.artifact == "" {
		return "", false
	}
	return v.artifact, true
}

// PeerCheck re-validates a request's peer. It is applied per request rather
// than only at handshake, and returns an error to refuse the request.
//
// It is a function rather than a concrete type so publish does not depend on
// how the peer is judged — the publisher's job is to apply the check on every
// request, not to know that it is a CRL lookup.
type PeerCheck func(*tls.ConnectionState) error

// Handler routes the publisher's HTTP API, applying check to every request and
// recording what each compiler did into peers.
//
// A nil check disables the check, which is what a plaintext test server wants;
// a nil peers disables recording. Both are separate arguments because they are
// separate concerns that happen to share the same moment in the request.
func Handler(s *Store, check PeerCheck, peers *Peers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/environments", s.handleEnvironments(peers))
	mux.HandleFunc("GET /v1/artifact/{env}/{codeID}", s.handleArtifact(peers))
	mux.HandleFunc("GET /v1/compilers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Never cached: the whole value is in how fresh last_seen is.
		w.Header().Set("Cache-Control", "no-store")
		list := peers.List()
		if list == nil {
			list = []Peer{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return guardPeer(mux, check)
}

// guardPeer re-checks the peer on every request.
//
// TLS verification runs once per connection. An agent polls over a keep-alive
// connection every 30 seconds and never handshakes again, so a certificate
// revoked after that first handshake would go on being served until the
// connection happened to drop — on the one node an operator most wants cut off.
// Checking here closes that window to a single request.
func guardPeer(next http.Handler, check PeerCheck) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			if err := check(r.TLS); err != nil {
				// 403, not 401: the peer authenticated fine, it is simply no
				// longer allowed. Naming it makes the publisher's log answer
				// "why did that compiler stop updating?" without a packet
				// capture.
				fmt.Fprintf(os.Stderr, "refusing %s: %v\n", r.URL.Path, err)
				http.Error(w, "peer is not permitted", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// handleEnvironments serves the version map, recording the poll.
//
// Recording happens here rather than in the middleware because the middleware
// runs before the mux has routed, so a request's path values are not populated
// yet — an artifact fetch would be recorded with an empty environment.
func (s *Store) handleEnvironments(peers *Peers) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		certname := peerCertname(r.TLS)
		now := time.Now().UTC()
		// Serving first: it is what admits a compiler to the fleet view, and
		// observePoll only touches peers already known. Doing it the other way
		// round would drop the first poll of every compiler from the count.
		peers.observeServing(certname, ParseServing(r.Header.Get(ServingHeader)), now)
		peers.observePoll(certname, now)
		s.serveEnvironments(w)
	}
}

func (s *Store) serveEnvironments(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Polling is the correctness mechanism, so this response must never be
	// served from a cache that could pin a compiler to a stale version.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.Environments()); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
	}
}

// handleArtifact streams a version's artifact, recording the fetch.
//
// The fetch is recorded only once the artifact has been found, so a compiler
// asking for a stale code_id and getting a 404 is never reported as holding it.
func (s *Store) handleArtifact(peers *Peers) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.serveArtifact(w, r, peers)
	}
}

func (s *Store) serveArtifact(w http.ResponseWriter, r *http.Request, peers *Peers) {
	env := r.PathValue("env")
	codeID := r.PathValue("codeID")

	if err := layout.ValidateEnvironment(env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := layout.ValidateCodeID(codeID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path, ok := s.Artifact(env, codeID)
	if !ok {
		http.Error(w,
			fmt.Sprintf("no current artifact for %s at code_id %s", env, codeID),
			http.StatusNotFound)
		return
	}

	f, err := os.Open(path) // #nosec G304 -- path is a materialized artifact this store wrote
	if err != nil {
		// A reseal may have reaped it between the lookup and the open; the
		// compiler retries on its next poll.
		http.Error(w, "artifact no longer available", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	peers.observeFetch(peerCertname(r.TLS), env, codeID, time.Now().UTC())

	w.Header().Set("Content-Type", "application/gzip")
	// The body is content-addressed by the code_id in the URL, so it can never
	// change meaning; anything that caches it may keep it indefinitely.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", layout.VersionDirName(env, codeID)+".tar.gz"))

	if _, err := io.Copy(w, f); err != nil {
		// Headers are already sent, so the status cannot be corrected. The
		// truncated body fails the agent's verify-by-reseal, which is the
		// backstop that makes a partial transfer safe.
		return
	}
}

// Server wraps an HTTP server configured for mutual TLS.
type Server struct {
	Addr      string
	Store     *Store
	TLSConfig *tls.Config
	// PeerCheck re-validates the peer on every request. TLSConfig only judges a
	// peer when a connection is established, which a keep-alive client does once
	// and then never again.
	PeerCheck PeerCheck
	// Peers records what each compiler was observed doing, for the fleet view.
	Peers *Peers
}

// Serve runs until ctx is cancelled, then shuts down gracefully.
//
// Certificates come from the TLS configuration rather than from files, because
// codavox reuses the Puppet CA material already on the node instead of holding
// a PKI of its own.
func (srv *Server) Serve(ctx context.Context) error {
	s := &http.Server{
		Addr:      srv.Addr,
		Handler:   Handler(srv.Store, srv.PeerCheck, srv.Peers),
		TLSConfig: srv.TLSConfig,
		// A compiler that stalls mid-request must not hold a connection open
		// indefinitely; the publisher serves the whole estate.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// Derive from ctx (so it is request-scoped) but strip its cancellation:
		// ctx is already done, and Shutdown needs a live context to bound the
		// drain on.
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(shutCtx)
	}()

	if err := s.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ArtifactsDir is where the publisher materializes artifacts under a state dir.
func ArtifactsDir(stateDir string) string { return filepath.Join(stateDir, "artifacts") }

// ArtifactPathFor is the on-disk artifact file for a version under a state dir.
// The deploy path uses it to wait for the publisher to have materialized a
// version, which is the signal that a reseal has taken effect.
func ArtifactPathFor(stateDir, env, codeID string) string {
	return filepath.Join(ArtifactsDir(stateDir), layout.VersionDirName(env, codeID)+".tar.gz")
}

// PidFilePath is where the running publisher records its pid, so a deploy can
// signal it to reseal.
func PidFilePath(stateDir string) string { return filepath.Join(stateDir, "publish.pid") }

// EnvironmentsPath is the polling endpoint compilers use.
const EnvironmentsPath = "/v1/environments"

// CompilersPath reports what the publisher knows about each compiler.
const CompilersPath = "/v1/compilers"

// ServingHeader is how an agent reports what it is currently serving, as
// `environment=code_id` pairs separated by commas.
//
// The agent reads these from its own environment symlinks — the same source
// code-id reads — so the publisher reports the compiler's own answer rather
// than inferring one from what it was seen to fetch. PE reaches the same place
// from the other direction: its file-sync client exposes its state on the
// standard status endpoint, and something central fans out to collect it. That
// needs a listener on every compiler and PuppetDB to discover them; folding the
// report into a poll the agent already makes needs neither, and keeps every
// connection outbound from the compiler.
const ServingHeader = "X-Codavox-Serving"

// maxServingHeader bounds what is parsed from a peer, so an estate with an
// unreasonable number of environments cannot turn a poll into unbounded work.
const maxServingHeader = 8 << 10

// ArtifactPath builds the artifact URL for an environment and code_id.
func ArtifactPath(env, codeID string) string {
	return "/v1/artifact/" + env + "/" + codeID
}

// TrimBase normalizes an operator-supplied base URL.
func TrimBase(base string) string { return strings.TrimRight(base, "/") }
