package deployserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/deploy"
)

const (
	apiToken = "api-token-xyz"
	secret   = "webhook-secret-abc"
)

// fakeDeployer records what it was asked to deploy and returns canned results.
type fakeDeployer struct {
	mu    sync.Mutex
	calls []deploy.Request
	err   error
}

func (f *fakeDeployer) Deploy(req deploy.Request) ([]deploy.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	// Simulate --all resolving to a fixed set the caller did not name.
	envs := req.Environments
	if req.All {
		envs = []string{"production", "testing"}
	}
	res := make([]deploy.Result, 0, len(envs))
	for _, e := range envs {
		res = append(res, deploy.Result{Env: e, CodeID: "id-" + e})
	}
	return res, f.err
}

func newServer(t *testing.T, d Deployer) *Server {
	t.Helper()
	s := New(Config{
		Deployer: d,
		APIToken: []byte(apiToken),
		Secret:   []byte(secret),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Start(ctx)
	return s
}

func send(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func bearer() map[string]string { return map[string]string{"Authorization": "Bearer " + apiToken} }

func waitTerminal(t *testing.T, s *Server, id string) Record {
	t.Helper()
	for range 200 {
		if rec, ok := s.get(id); ok && (rec.Status == StatusComplete || rec.Status == StatusFailed) {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deploy %s never reached a terminal state", id)
	return Record{}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decoding response: %v\n%s", err, rec.Body.String())
	}
	return r
}

func TestCreateDeployAsync(t *testing.T) {
	fake := &fakeDeployer{}
	s := newServer(t, fake)

	resp := send(t, s, "POST", "/v1/deploys", `{"environments":["production"]}`, bearer())
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}
	rec := decode(t, resp)
	if rec.ID == "" {
		t.Fatal("response has no deploy id")
	}
	if rec.Source != "api" {
		t.Errorf("source = %q, want api", rec.Source)
	}

	final := waitTerminal(t, s, rec.ID)
	if final.Status != StatusComplete {
		t.Errorf("status = %q, want complete", final.Status)
	}
	if len(final.Results) != 1 || final.Results[0].CodeID != "id-production" {
		t.Errorf("results = %+v, want one for production", final.Results)
	}
}

func TestCreateDeployModules(t *testing.T) {
	fake := &fakeDeployer{}
	s := newServer(t, fake)

	resp := send(t, s, "POST", "/v1/deploys",
		`{"environments":["production"],"modules":["apache","nginx"],"wait":true}`, bearer())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	rec := decode(t, resp)
	if len(rec.Modules) != 2 || rec.Modules[0] != "apache" {
		t.Errorf("record modules = %v, want [apache nginx]", rec.Modules)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 1 || len(fake.calls[0].Modules) != 2 {
		t.Fatalf("deployer called with %+v, want the two modules", fake.calls)
	}

	// A name r10k would silently match against nothing is refused up front.
	for _, bad := range []string{`"puppetlabs/apache"`, `"puppetlabs-apache"`, `"Apache"`} {
		resp := send(t, s, "POST", "/v1/deploys",
			`{"environments":["production"],"modules":[`+bad+`]}`, bearer())
		if resp.Code != http.StatusBadRequest {
			t.Errorf("modules [%s]: status = %d, want 400", bad, resp.Code)
		}
	}
}

func TestCreateDeployWaitBlocksUntilComplete(t *testing.T) {
	s := newServer(t, &fakeDeployer{})

	resp := send(t, s, "POST", "/v1/deploys", `{"environments":["production"],"wait":true}`, bearer())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	rec := decode(t, resp)
	if rec.Status != StatusComplete {
		t.Errorf("wait returned status %q, want complete", rec.Status)
	}
	if rec.FinishedAt == nil {
		t.Error("completed deploy has no finished_at")
	}
}

func TestCreateDeployAllFillsEnvironments(t *testing.T) {
	fake := &fakeDeployer{}
	s := newServer(t, fake)

	resp := send(t, s, "POST", "/v1/deploys", `{"all":true,"wait":true}`, bearer())
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	rec := decode(t, resp)
	got := map[string]bool{}
	for _, e := range rec.Environments {
		got[e] = true
	}
	if !got["production"] || !got["testing"] {
		t.Errorf("environments = %v, want production and testing filled from results", rec.Environments)
	}
	if len(fake.calls) != 1 {
		t.Errorf("deployer called %d times, want 1", len(fake.calls))
	}
}

func TestCreateDeployAuthAndValidation(t *testing.T) {
	s := newServer(t, &fakeDeployer{})

	t.Run("no token", func(t *testing.T) {
		if resp := send(t, s, "POST", "/v1/deploys", `{"environments":["production"]}`, nil); resp.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.Code)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		h := map[string]string{"Authorization": "Bearer nope"}
		if resp := send(t, s, "POST", "/v1/deploys", `{"environments":["production"]}`, h); resp.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.Code)
		}
	})
	t.Run("neither environments nor all", func(t *testing.T) {
		if resp := send(t, s, "POST", "/v1/deploys", `{}`, bearer()); resp.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.Code)
		}
	})
	t.Run("both environments and all", func(t *testing.T) {
		if resp := send(t, s, "POST", "/v1/deploys", `{"environments":["production"],"all":true}`, bearer()); resp.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.Code)
		}
	})
}

func TestGetAndListDeploys(t *testing.T) {
	s := newServer(t, &fakeDeployer{})

	first := decode(t, send(t, s, "POST", "/v1/deploys", `{"environments":["production"],"wait":true}`, bearer()))
	second := decode(t, send(t, s, "POST", "/v1/deploys", `{"environments":["testing"],"wait":true}`, bearer()))

	t.Run("get by id", func(t *testing.T) {
		resp := send(t, s, "GET", "/v1/deploys/"+first.ID, "", bearer())
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.Code)
		}
		if decode(t, resp).ID != first.ID {
			t.Error("got the wrong deploy")
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		if resp := send(t, s, "GET", "/v1/deploys/deadbeef", "", bearer()); resp.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.Code)
		}
	})

	t.Run("list is newest first", func(t *testing.T) {
		resp := send(t, s, "GET", "/v1/deploys", "", bearer())
		var list []Record
		if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 {
			t.Fatalf("got %d records, want 2", len(list))
		}
		if list[0].ID != second.ID || list[1].ID != first.ID {
			t.Error("history is not newest-first")
		}
	})
}

func TestFailedDeployIsRecorded(t *testing.T) {
	s := newServer(t, &fakeDeployer{err: errFake})

	rec := decode(t, send(t, s, "POST", "/v1/deploys", `{"environments":["production"],"wait":true}`, bearer()))
	if rec.Status != StatusFailed {
		t.Errorf("status = %q, want failed", rec.Status)
	}
	if rec.Error == "" {
		t.Error("failed deploy has no error message")
	}
}

func TestWebhookRouteDeploysAndRecords(t *testing.T) {
	fake := &fakeDeployer{}
	s := newServer(t, fake)

	body := `{"ref":"refs/heads/production"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	resp := send(t, s, "POST", "/v1/webhook", body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}

	// The webhook deploy shows up in the same history the API serves.
	var list []Record
	for range 200 {
		r := send(t, s, "GET", "/v1/deploys", "", bearer())
		_ = json.Unmarshal(r.Body.Bytes(), &list)
		if len(list) == 1 && list[0].Status == StatusComplete {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("history has %d records, want 1", len(list))
	}
	if list[0].Source != "webhook" {
		t.Errorf("source = %q, want webhook", list[0].Source)
	}
	if len(list[0].Environments) != 1 || list[0].Environments[0] != "production" {
		t.Errorf("environments = %v, want [production]", list[0].Environments)
	}
}

func TestWebhookBranchDeleteDeploysAll(t *testing.T) {
	fake := &fakeDeployer{}
	s := newServer(t, fake)

	body := `{"ref":"refs/heads/testing","deleted":true}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	resp := send(t, s, "POST", "/v1/webhook", body, map[string]string{
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Code)
	}

	var list []Record
	for range 200 {
		r := send(t, s, "GET", "/v1/deploys", "", bearer())
		_ = json.Unmarshal(r.Body.Bytes(), &list)
		if len(list) == 1 && list[0].Status == StatusComplete {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("history has %d records, want 1", len(list))
	}

	// The deleted branch cannot be deployed by name; r10k purges it only as
	// part of a deploy, so the webhook must have asked for everything.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 1 || !fake.calls[0].All || len(fake.calls[0].Environments) != 0 {
		t.Fatalf("deployer called with %+v, want no envs and All", fake.calls)
	}
	rec := list[0]
	if !rec.All {
		t.Error("record.All = false, want true")
	}
	if rec.Source != "webhook" {
		t.Errorf("source = %q, want webhook", rec.Source)
	}
	if !strings.Contains(rec.Reason, "testing") || !strings.Contains(rec.Reason, "deleted") {
		t.Errorf("reason = %q, want it to name the deleted branch's environment", rec.Reason)
	}
	// Environments reports what remains after the purge, as an --all deploy does.
	if len(rec.Environments) != 2 {
		t.Errorf("environments = %v, want the remaining set", rec.Environments)
	}
}

func TestDisabledRoutesReturn404(t *testing.T) {
	// A server with only a webhook secret must not expose the deploy API.
	s := New(Config{
		Deployer: &fakeDeployer{},
		Secret:   []byte(secret),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if resp := send(t, s, "POST", "/v1/deploys", `{"all":true}`, bearer()); resp.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the API is disabled", resp.Code)
	}
}

var errFake = errFakeType("r10k blew up")

type errFakeType string

func (e errFakeType) Error() string { return string(e) }
