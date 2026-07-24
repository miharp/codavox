// Package webhook receives control-repo push notifications and deploys the
// pushed environment.
//
// It is a front door onto the deploy path, not a second implementation of it:
// a push is authenticated, mapped to an environment, and handed to a Deployer,
// which the command wires to the same internal/deploy.Run the CLI uses. Keeping
// r10k behind that seam is what lets the whole request path be tested without
// running a deploy.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/miharp/codavox/internal/layout"
)

// Deployer deploys a single environment. The real implementation wraps
// internal/deploy.Run; tests substitute a recorder.
type Deployer interface {
	Deploy(env string) error
}

const (
	// DefaultQueueDepth bounds how many deploys can be pending before the
	// receiver sheds load with 503 rather than growing unboundedly.
	DefaultQueueDepth = 64
	// maxBodyBytes caps a payload. GitHub documents a 25 MiB maximum, and the
	// whole body must be read to verify the HMAC, so this is that ceiling.
	maxBodyBytes = 25 << 20
)

// Handler is the webhook HTTP handler and its deploy worker.
type Handler struct {
	secret   []byte
	deployer Deployer
	logger   *slog.Logger
	queue    chan string
	mux      http.Handler
}

// New returns a Handler. Call Start to run the deploy worker.
func New(secret []byte, d Deployer, logger *slog.Logger) *Handler {
	h := &Handler{
		secret:   secret,
		deployer: d,
		logger:   logger,
		queue:    make(chan string, DefaultQueueDepth),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/webhook", h.handleWebhook)
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	h.mux = mux
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Start runs the deploy worker until ctx is cancelled.
//
// Deploys run one at a time: r10k mutates the staging directory in place, so
// concurrent deploys would clobber each other. A burst of pushes queues behind
// the single worker rather than racing.
func (h *Handler) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-h.queue:
			if err := h.deployer.Deploy(env); err != nil {
				h.logger.Error("webhook deploy failed", "environment", env, "error", err)
			}
		}
	}
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	provider := detectProvider(r)
	if err := h.authenticate(provider, r, body); err != nil {
		// Do not echo the reason to the caller; log it for the operator.
		h.logger.Warn("webhook authentication failed", "provider", provider, "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ev, err := parseEvent(provider, r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ev.ignore {
		// 200 so the provider records success and does not retry a push we
		// intentionally did nothing with.
		h.logger.Info("webhook ignored", "provider", provider, "reason", ev.reason)
		w.WriteHeader(http.StatusOK)
		return
	}

	env, err := environmentFromRef(ev.ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	select {
	case h.queue <- env:
		h.logger.Info("webhook deploy queued", "provider", provider, "environment", env)
		w.WriteHeader(http.StatusAccepted)
	default:
		// The worker is behind; shed load rather than block the provider.
		http.Error(w, "deploy queue full", http.StatusServiceUnavailable)
	}
}

func detectProvider(r *http.Request) string {
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		return "github"
	case r.Header.Get("X-Gitlab-Event") != "":
		return "gitlab"
	default:
		return "generic"
	}
}

func (h *Handler) authenticate(provider string, r *http.Request, body []byte) error {
	switch provider {
	case "github":
		return verifyGitHubSignature(h.secret, body, r.Header.Get("X-Hub-Signature-256"))
	case "gitlab":
		if !secretEqual(h.secret, []byte(r.Header.Get("X-Gitlab-Token"))) {
			return errors.New("gitlab token mismatch")
		}
		return nil
	default:
		if !secretEqual(h.secret, []byte(genericToken(r))) {
			return errors.New("token mismatch")
		}
		return nil
	}
}

// verifyGitHubSignature checks GitHub's HMAC-SHA256 of the body. The secret is
// never on the wire, so this holds even without TLS.
func verifyGitHubSignature(secret, body []byte, header string) error {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return errors.New("missing sha256 signature")
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return errors.New("malformed signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func genericToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func secretEqual(a, b []byte) bool {
	// ConstantTimeCompare returns 0 on a length mismatch, so an empty presented
	// token never matches a configured secret.
	return subtle.ConstantTimeCompare(a, b) == 1
}

type event struct {
	ref    string
	ignore bool
	reason string
}

func parseEvent(provider string, r *http.Request, body []byte) (event, error) {
	switch provider {
	case "github":
		switch e := r.Header.Get("X-GitHub-Event"); e {
		case "push":
		case "ping":
			return event{ignore: true, reason: "ping"}, nil
		default:
			return event{ignore: true, reason: "non-push event " + e}, nil
		}
		var p struct {
			Ref     string `json:"ref"`
			Deleted bool   `json:"deleted"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing github payload: %w", err)
		}
		if p.Deleted {
			return event{ignore: true, reason: "branch deleted"}, nil
		}
		return branchEvent(p.Ref), nil

	case "gitlab":
		var p struct {
			Ref        string `json:"ref"`
			ObjectKind string `json:"object_kind"`
			After      string `json:"after"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing gitlab payload: %w", err)
		}
		if p.ObjectKind != "" && p.ObjectKind != "push" {
			return event{ignore: true, reason: "non-push event " + p.ObjectKind}, nil
		}
		// A branch deletion sends an all-zero "after" sha.
		if p.After != "" && strings.Trim(p.After, "0") == "" {
			return event{ignore: true, reason: "branch deleted"}, nil
		}
		return branchEvent(p.Ref), nil

	default:
		var p struct {
			Ref         string `json:"ref"`
			Environment string `json:"environment"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return event{}, fmt.Errorf("parsing payload: %w", err)
		}
		if p.Environment != "" {
			return event{ref: "refs/heads/" + p.Environment}, nil
		}
		if p.Ref == "" {
			return event{}, errors.New("payload has neither ref nor environment")
		}
		return branchEvent(p.Ref), nil
	}
}

// branchEvent ignores refs that are not branches: tags and other refs do not
// name environments.
func branchEvent(ref string) event {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return event{ignore: true, reason: "non-branch ref " + ref}
	}
	return event{ref: ref}
}

var nonWord = regexp.MustCompile(`\W`)

// environmentFromRef maps a branch ref to an environment name, sanitizing \W to
// _ the way r10k does when it names environments, so the result matches the
// directory r10k deploys.
func environmentFromRef(ref string) (string, error) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == "" {
		return "", errors.New("empty branch ref")
	}
	env := nonWord.ReplaceAllString(branch, "_")
	if err := layout.ValidateEnvironment(env); err != nil {
		return "", err
	}
	return env, nil
}
