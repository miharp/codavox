// Package deployserver is the deploy control plane: an HTTP daemon on the
// primary with two front doors — a token-authenticated deploy API and a
// secret-authenticated webhook — feeding one queue, one worker, and one deploy
// history. Both funnel through internal/deploy, so however a deploy is
// triggered it takes the same path and appears in the same history.
package deployserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miharp/codavox/internal/deploy"
	"github.com/miharp/codavox/internal/webhook"
)

// Status is where a deploy is in its lifecycle.
type Status string

// Deploy lifecycle states, from submission to a terminal result.
const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusComplete Status = "complete"
	StatusFailed   Status = "failed"
)

// Record is one deploy, however it was triggered.
type Record struct {
	ID           string          `json:"id"`
	Status       Status          `json:"status"`
	Source       string          `json:"source"` // "api" or "webhook"
	Environments []string        `json:"environments,omitempty"`
	All          bool            `json:"all,omitempty"`
	SubmittedAt  time.Time       `json:"submitted_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Results      []deploy.Result `json:"results,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// Deployer runs a deploy. The real implementation is Runner; tests substitute a
// recorder so the server's HTTP and queue behavior is exercised without r10k.
type Deployer interface {
	Deploy(envs []string, all bool) ([]deploy.Result, error)
}

// Runner is the production Deployer: it wraps internal/deploy.Run.
type Runner struct {
	Config deploy.Config
}

// Deploy runs r10k and reseals. It does not wait for the publisher to serve —
// the record is complete once the deploy has landed on the primary, and
// compilers converge on their own by polling.
func (r Runner) Deploy(envs []string, all bool) ([]deploy.Result, error) {
	return deploy.Run(r.Config, envs, all, false)
}

// Config configures a Server.
type Config struct {
	Deployer   Deployer
	APIToken   []byte // enables the deploy API when non-empty
	Secret     []byte // enables the webhook when non-empty
	MaxHistory int
	QueueDepth int
	Logger     *slog.Logger
}

// Server holds the deploy queue, worker, and history, and routes both APIs.
type Server struct {
	deployer Deployer
	apiToken []byte
	secret   []byte
	logger   *slog.Logger
	maxHist  int
	mux      http.Handler

	mu      sync.Mutex
	records map[string]*Record
	order   []string // ids, oldest first
	queue   chan string
}

var errQueueFull = errors.New("deploy queue full")

const waitTimeout = 5 * time.Minute

// New builds a Server. Routes are enabled by which credentials are set: the API
// when APIToken is non-empty, the webhook when Secret is. Call Start to run the
// worker.
func New(cfg Config) *Server {
	if cfg.MaxHistory <= 0 {
		cfg.MaxHistory = 100
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = 64
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s := &Server{
		deployer: cfg.Deployer,
		apiToken: cfg.APIToken,
		secret:   cfg.Secret,
		logger:   cfg.Logger,
		maxHist:  cfg.MaxHistory,
		records:  map[string]*Record{},
		queue:    make(chan string, cfg.QueueDepth),
	}

	mux := http.NewServeMux()
	if len(s.apiToken) > 0 {
		mux.HandleFunc("POST /v1/deploys", s.handleCreateDeploy)
		mux.HandleFunc("GET /v1/deploys", s.handleListDeploys)
		mux.HandleFunc("GET /v1/deploys/{id}", s.handleGetDeploy)
	}
	if len(s.secret) > 0 {
		mux.HandleFunc("POST /v1/webhook", s.handleWebhook)
	}
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Start runs the deploy worker until ctx is cancelled. Deploys run one at a
// time; internal/deploy's basedir lock also serializes against any other
// process, so this worker being single is belt-and-suspenders for that host.
func (s *Server) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.queue:
			s.process(id)
		}
	}
}

func (s *Server) process(id string) {
	s.mu.Lock()
	rec := s.records[id]
	if rec == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	rec.Status = StatusRunning
	rec.StartedAt = &now
	envs := append([]string(nil), rec.Environments...)
	all := rec.All
	s.mu.Unlock()

	results, err := s.deployer.Deploy(envs, all)

	s.mu.Lock()
	fin := time.Now().UTC()
	rec.FinishedAt = &fin
	rec.Results = results
	if len(rec.Environments) == 0 { // an --all deploy did not know the set up front
		for _, r := range results {
			rec.Environments = append(rec.Environments, r.Env)
		}
	}
	if err != nil {
		rec.Status = StatusFailed
		rec.Error = err.Error()
	} else {
		rec.Status = StatusComplete
	}
	status := rec.Status
	s.mu.Unlock()
	s.logger.Info("deploy finished", "id", id, "status", status)
}

// submit records a deploy and enqueues it. Enqueue and store happen under one
// lock, so the worker — which must take the same lock — cannot observe the id
// on the queue before its record exists.
func (s *Server) submit(source string, envs []string, all bool) (Record, error) {
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	rec := &Record{
		ID:           id,
		Status:       StatusQueued,
		Source:       source,
		Environments: envs,
		All:          all,
		SubmittedAt:  time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.queue <- id:
	default:
		return Record{}, errQueueFull
	}
	s.records[id] = rec
	s.order = append(s.order, id)
	for len(s.order) > s.maxHist {
		delete(s.records, s.order[0])
		s.order = s.order[1:]
	}
	// Return a snapshot, not the stored pointer: the worker may start mutating
	// the record the moment this lock is released.
	return *rec, nil
}

// waitFor blocks until a deploy is terminal or the timeout elapses, returning a
// snapshot either way.
func (s *Server) waitFor(id string, timeout time.Duration) (Record, bool) {
	deadline := time.Now().Add(timeout)
	for {
		rec, ok := s.get(id)
		if !ok {
			return Record{}, false
		}
		if rec.Status == StatusComplete || rec.Status == StatusFailed || time.Now().After(deadline) {
			return rec, true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// get returns a snapshot copy so a handler never marshals a record the worker
// is mutating.
func (s *Server) get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

func (s *Server) list() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- { // newest first
		if rec, ok := s.records[s.order[i]]; ok {
			out = append(out, *rec)
		}
	}
	return out
}

func (s *Server) apiAuthorized(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), s.apiToken) == 1
}

func (s *Server) handleCreateDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req struct {
		Environments []string `json:"environments"`
		All          bool     `json:"all"`
		Wait         bool     `json:"wait"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
	}
	if req.All && len(req.Environments) > 0 {
		http.Error(w, "give environments or all, not both", http.StatusBadRequest)
		return
	}
	if !req.All && len(req.Environments) == 0 {
		http.Error(w, "request needs environments or all", http.StatusBadRequest)
		return
	}

	rec, err := s.submit("api", req.Environments, req.All)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.logger.Info("deploy queued", "id", rec.ID, "source", "api")

	if req.Wait {
		final, _ := s.waitFor(rec.ID, waitTimeout)
		s.writeJSON(w, http.StatusOK, final)
		return
	}
	s.writeJSON(w, http.StatusAccepted, rec)
}

func (s *Server) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rec, ok := s.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such deploy", http.StatusNotFound)
		return
	}
	s.writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleListDeploys(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.writeJSON(w, http.StatusOK, s.list())
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, webhook.MaxBodyBytes))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	if err := webhook.Authenticate(s.secret, r, body); err != nil {
		s.logger.Warn("webhook authentication failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	env, ignore, reason, err := webhook.Environment(r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ignore {
		s.logger.Info("webhook ignored", "reason", reason)
		w.WriteHeader(http.StatusOK)
		return
	}
	rec, err := s.submit("webhook", []string{env}, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.logger.Info("deploy queued", "id", rec.ID, "source", "webhook", "environment", env)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
