// Package publish serves environment versions and artifacts to compilers.
package publish

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/seal"
)

// Store holds sealed environments and the artifacts that reproduce them.
type Store struct {
	// StagingDir contains one directory per environment, as r10k deploys it.
	StagingDir string

	mu     sync.RWMutex
	sealed map[string]string // environment -> code_id
}

// NewStore returns a Store reading environments from stagingDir.
func NewStore(stagingDir string) *Store {
	return &Store{StagingDir: stagingDir, sealed: map[string]string{}}
}

// Reseal rescans the staging directory and updates the published code_ids.
//
// Sealing is not done per request. It walks and hashes an entire environment,
// which is far too expensive to repeat for every polling compiler, and it
// would also mean two compilers polling either side of an r10k run could
// observe different ids for what is meant to be one deploy.
func (s *Store) Reseal() error {
	entries, err := os.ReadDir(s.StagingDir)
	if err != nil {
		return fmt.Errorf("reading staging directory: %w", err)
	}

	next := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		env := e.Name()
		// Skip rather than fail: one badly named directory in the staging area
		// should not stop every other environment from being published.
		if layout.ValidateEnvironment(env) != nil {
			continue
		}
		id, err := seal.CodeID(filepath.Join(s.StagingDir, env))
		if err != nil {
			return fmt.Errorf("sealing %s: %w", env, err)
		}
		next[env] = id
	}

	s.mu.Lock()
	s.sealed = next
	s.mu.Unlock()
	return nil
}

// Environments returns the currently published environment to code_id map.
func (s *Store) Environments() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]string, len(s.sealed))
	for k, v := range s.sealed {
		out[k] = v
	}
	return out
}

// CodeID returns the published code_id for env.
func (s *Store) CodeID(env string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.sealed[env]
	return id, ok
}

// Handler routes the publisher's HTTP API.
func Handler(s *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/environments", s.handleEnvironments)
	mux.HandleFunc("GET /v1/artifact/{env}/{codeID}", s.handleArtifact)
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}

func (s *Store) handleEnvironments(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Polling is the correctness mechanism, so this response must never be
	// served from a cache that could pin a compiler to a stale version.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.Environments()); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
	}
}

func (s *Store) handleArtifact(w http.ResponseWriter, r *http.Request) {
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

	current, ok := s.CodeID(env)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown environment %q", env), http.StatusNotFound)
		return
	}

	// Only the current version is servable. Serving an arbitrary historical
	// id would mean re-sealing to find it, and keeping every past tree on the
	// publisher; compilers retain old versions themselves for in-flight runs.
	if codeID != current {
		http.Error(w,
			fmt.Sprintf("code_id %s is not current for %s (current is %s)", codeID, env, current),
			http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	// The body is content-addressed by the code_id in the URL, so it can never
	// change meaning; anything that caches it may keep it indefinitely.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", env+"_"+codeID+".tar.gz"))

	// Streamed rather than buffered: environments run to hundreds of megabytes
	// and several compilers may poll at once.
	if err := seal.WriteArchive(w, filepath.Join(s.StagingDir, env)); err != nil {
		// Headers are already sent, so the status cannot be corrected. The
		// truncated body fails the agent's digest check, which is the backstop
		// that makes a partial transfer safe.
		return
	}
}

// Server wraps an HTTP server configured for mutual TLS.
type Server struct {
	Addr      string
	Store     *Store
	TLSConfig *tls.Config
}

// ListenAndServeTLS serves until the process exits.
//
// Certificates come from the TLS configuration rather than from files, because
// codavox reuses the Puppet CA material already on the node instead of holding
// a PKI of its own.
func (srv *Server) ListenAndServeTLS() error {
	s := &http.Server{
		Addr:      srv.Addr,
		Handler:   Handler(srv.Store),
		TLSConfig: srv.TLSConfig,
		// A compiler that stalls mid-request must not hold a connection open
		// indefinitely; the publisher serves the whole estate.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s.ListenAndServeTLS("", "")
}

// EnvironmentsPath is the polling endpoint compilers use.
const EnvironmentsPath = "/v1/environments"

// ArtifactPath builds the artifact URL for an environment and code_id.
func ArtifactPath(env, codeID string) string {
	return "/v1/artifact/" + env + "/" + codeID
}

// TrimBase normalizes an operator-supplied base URL.
func TrimBase(base string) string { return strings.TrimRight(base, "/") }
