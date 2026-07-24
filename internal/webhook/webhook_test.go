package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordDeployer records the environments it is asked to deploy.
type recordDeployer struct {
	calls chan string
}

func (r *recordDeployer) Deploy(env string) error {
	r.calls <- env
	return nil
}

const testSecret = "s3cr3t-webhook-key"

func newTestHandler(t *testing.T) (*Handler, *recordDeployer) {
	t.Helper()
	rec := &recordDeployer{calls: make(chan string, 4)}
	h := New([]byte(testSecret), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Start(ctx)
	return h, rec
}

func do(t *testing.T, h *Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// expectDeploy asserts a deploy of env was queued within a short window.
func expectDeploy(t *testing.T, rec *recordDeployer, env string) {
	t.Helper()
	select {
	case got := <-rec.calls:
		if got != env {
			t.Errorf("deployed %q, want %q", got, env)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no deploy of %q was queued", env)
	}
}

// expectNoDeploy asserts nothing was queued.
func expectNoDeploy(t *testing.T, rec *recordDeployer) {
	t.Helper()
	select {
	case got := <-rec.calls:
		t.Errorf("unexpected deploy of %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func githubSig(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubPushDeploysSignedBranch(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{"ref":"refs/heads/production"}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	expectDeploy(t, rec, "production")
}

func TestGitHubBadSignatureIsRejected(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{"ref":"refs/heads/production"}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("00", 32),
	})
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGitHubPingIsAcknowledgedNotDeployed(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{"zen":"hello"}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGitHubBranchDeleteIsIgnored(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{"ref":"refs/heads/production","deleted":true}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (ignored)", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGitHubTagPushIsIgnored(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{"ref":"refs/tags/v1.0.0"}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (a tag is not an environment)", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGitLabTokenDeploys(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"ref":"refs/heads/production","object_kind":"push","after":"abc123"}`, map[string]string{
		"X-Gitlab-Event": "Push Hook",
		"X-Gitlab-Token": testSecret,
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	expectDeploy(t, rec, "production")
}

func TestGitLabBadTokenIsRejected(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"ref":"refs/heads/production","object_kind":"push"}`, map[string]string{
		"X-Gitlab-Event": "Push Hook",
		"X-Gitlab-Token": "wrong",
	})
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGitLabBranchDeleteIsIgnored(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"ref":"refs/heads/production","object_kind":"push","after":"0000000000000000000000000000000000000000"}`, map[string]string{
		"X-Gitlab-Event": "Push Hook",
		"X-Gitlab-Token": testSecret,
	})
	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (ignored)", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestGenericBearerDeploys(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"ref":"refs/heads/production"}`, map[string]string{
		"Authorization": "Bearer " + testSecret,
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	expectDeploy(t, rec, "production")
}

func TestGenericEnvironmentFieldDeploys(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"environment":"testing"}`, map[string]string{
		"Authorization": "Bearer " + testSecret,
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	expectDeploy(t, rec, "testing")
}

func TestGenericMissingTokenIsRejected(t *testing.T) {
	h, rec := newTestHandler(t)
	resp := do(t, h, `{"ref":"refs/heads/production"}`, nil)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestMalformedBodyIsRejected(t *testing.T) {
	h, rec := newTestHandler(t)
	body := `{ not json`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.Code)
	}
	expectNoDeploy(t, rec)
}

func TestBranchNameSanitizedToEnvironment(t *testing.T) {
	h, rec := newTestHandler(t)
	// r10k sanitizes \W to _, so feature/new-thing becomes feature_new_thing.
	body := `{"ref":"refs/heads/feature/new-thing"}`
	resp := do(t, h, body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": githubSig(body),
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	expectDeploy(t, rec, "feature_new_thing")
}
