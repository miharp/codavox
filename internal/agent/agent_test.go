package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
)

// fixture wires a publisher and a compiler-side agent against temp directories.
type fixture struct {
	staging string
	store   *publish.Store
	server  *httptest.Server
	agent   *Agent
	layout  layout.Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	staging := t.TempDir()
	store := publish.NewStore(staging)
	server := httptest.NewServer(publish.Handler(store))
	t.Cleanup(server.Close)

	base := t.TempDir()
	l := layout.Layout{
		Root:            filepath.Join(base, "codavox"),
		EnvironmentPath: filepath.Join(base, "environments"),
	}

	a, err := New(Config{
		BaseURL:  server.URL,
		Layout:   l,
		Client:   server.Client(),
		Interval: 10 * time.Millisecond,
		Keep:     2,
		MinAge:   time.Nanosecond, // reap immediately in tests
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{staging: staging, store: store, server: server, agent: a, layout: l}
}

// publishEnv writes an environment into staging and reseals.
func (f *fixture) publishEnv(t *testing.T, env string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(f.staging, env)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Reseal(); err != nil {
		t.Fatal(err)
	}
	return f.store.Environments()[env]
}

func TestSyncDeploysAnEnvironment(t *testing.T) {
	f := newFixture(t)
	want := f.publishEnv(t, "production", map[string]string{
		"manifests/site.pp":      "node default { }\n",
		"modules/apache/init.pp": "class apache { }\n",
	})

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	got, err := f.layout.CurrentCodeID("production")
	if err != nil {
		t.Fatalf("CurrentCodeID: %v", err)
	}
	if got != want {
		t.Errorf("deployed %s, want %s", got, want)
	}

	// The environment link must resolve to real content, not just exist.
	body, err := os.ReadFile(filepath.Join(f.layout.EnvironmentLink("production"), "manifests/site.pp"))
	if err != nil {
		t.Fatalf("reading through the environment link: %v", err)
	}
	if string(body) != "node default { }\n" {
		t.Errorf("content through link = %q", body)
	}
}

// Convergence: the agent moves to whatever the publisher currently advertises.
func TestSyncConvergesOnChange(t *testing.T) {
	f := newFixture(t)
	first := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.layout.CurrentCodeID("production"); got != first {
		t.Fatalf("first deploy: got %s, want %s", got, first)
	}

	second := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { notify { 'x': } }\n"})
	if first == second {
		t.Fatal("test setup: content change did not change the code_id")
	}

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.layout.CurrentCodeID("production"); got != second {
		t.Errorf("after change: got %s, want %s", got, second)
	}
}

// Repeated polls against unchanged content must not churn: re-extracting and
// re-swapping on every tick would make every compiler rewrite its environment
// constantly for no reason.
func TestSyncIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})

	ctx := context.Background()
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	link := f.layout.EnvironmentLink("production")
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := f.agent.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the environment link was rewritten despite unchanged content")
	}
	if newTarget, _ := os.Readlink(link); newTarget != target {
		t.Errorf("link target changed: %s -> %s", target, newTarget)
	}
}

// This is the failure mode webhooks cannot handle: a compiler that was
// unreachable while a deploy happened must catch up on its own, with no
// operator intervention and no replayed event.
func TestCatchUpAfterMissingADeploy(t *testing.T) {
	f := newFixture(t)
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v1\n"})

	ctx := context.Background()
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The compiler is "down": the publisher advances twice while it is not
	// polling, so it misses both deploys entirely.
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v2\n"})
	latest := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v3\n"})

	// It comes back and polls once.
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := f.layout.CurrentCodeID("production")
	if err != nil {
		t.Fatal(err)
	}
	if got != latest {
		t.Errorf("did not catch up: got %s, want %s", got, latest)
	}

	body, err := os.ReadFile(filepath.Join(f.layout.EnvironmentLink("production"), "manifests/site.pp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v3\n" {
		t.Errorf("content = %q, want v3", body)
	}
}

func TestMultipleEnvironments(t *testing.T) {
	f := newFixture(t)
	prod := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	test := f.publishEnv(t, "testing", map[string]string{"manifests/site.pp": "test\n"})

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	for env, want := range map[string]string{"production": prod, "testing": test} {
		got, err := f.layout.CurrentCodeID(env)
		if err != nil {
			t.Errorf("%s: %v", env, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", env, got, want)
		}
	}
}

// A corrupted or substituted artifact must not be deployed. Verification is by
// resealing the extracted tree, so tampering anywhere in the pipeline fails.
func TestTamperedArtifactIsRejected(t *testing.T) {
	f := newFixture(t)
	real := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "genuine\n"})

	// A publisher that advertises a genuine code_id but serves different
	// content under it.
	tampered := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tampered, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tampered, "manifests/site.pp"), []byte("MALICIOUS\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == publish.EnvironmentsPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"production":"` + real + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		writeArchiveOf(t, w, tampered)
	}))
	defer evil.Close()

	a, err := New(Config{
		BaseURL: evil.URL,
		Layout:  f.layout,
		Client:  evil.Client(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Once(context.Background()); err == nil {
		t.Fatal("a tampered artifact was accepted")
	}

	// Nothing must have been deployed.
	if _, err := f.layout.CurrentCodeID("production"); err == nil {
		t.Error("an environment was deployed from a tampered artifact")
	}
}

func TestReapRetainsRecentAndCurrent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var ids []string
	for _, body := range []string{"v1\n", "v2\n", "v3\n", "v4\n", "v5\n"} {
		ids = append(ids, f.publishEnv(t, "production", map[string]string{"manifests/site.pp": body}))
		if err := f.agent.Once(ctx); err != nil {
			t.Fatal(err)
		}
		// Distinguish modification times so ordering is deterministic.
		time.Sleep(10 * time.Millisecond)
	}

	current := ids[len(ids)-1]
	if got, _ := f.layout.CurrentCodeID("production"); got != current {
		t.Fatalf("current = %s, want %s", got, current)
	}

	// The current version must survive reaping regardless of age or count;
	// deleting it would break the environment the compiler is serving.
	if _, err := os.Stat(f.layout.VersionDir("production", current)); err != nil {
		t.Errorf("the current version was reaped: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(f.layout.Root, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	// Keep=2 superseded, plus the current one.
	if len(entries) > 3 {
		t.Errorf("kept %d versions, want at most 3", len(entries))
	}
}

// MinAge protects a version an in-flight agent run may still request content
// for, even when Keep alone would drop it.
func TestReapRespectsMinAge(t *testing.T) {
	f := newFixture(t)
	f.agent.cfg.Keep = 0
	f.agent.cfg.MinAge = time.Hour

	ctx := context.Background()
	first := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v1\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v2\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(f.layout.VersionDir("production", first)); err != nil {
		t.Errorf("a version younger than MinAge was reaped: %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{Client: http.DefaultClient}); err == nil {
		t.Error("expected an error when BaseURL is missing")
	}
	if _, err := New(Config{BaseURL: "https://example.com"}); err == nil {
		t.Error("expected an error when Client is missing")
	}

	a, err := New(Config{BaseURL: "https://example.com/", Client: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.Interval != DefaultInterval || a.cfg.Keep != DefaultKeep || a.cfg.MinAge != DefaultMinAge {
		t.Error("defaults were not applied")
	}
	if a.cfg.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", a.cfg.BaseURL)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	f := newFixture(t)
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "x\n"})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- f.agent.Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil; want the context error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
}

// writeArchiveOf writes a deterministic artifact for an arbitrary directory,
// used to simulate a publisher serving content that does not match its id.
func writeArchiveOf(t *testing.T, w io.Writer, dir string) {
	t.Helper()
	if err := seal.WriteArchive(w, dir); err != nil {
		t.Error(err)
	}
}

// OpenVox Server reads deployed trees as the puppet user while the agent runs
// as root. os.MkdirTemp creates 0700, so without an explicit chmod every
// catalog compile fails with EACCES — a failure no same-user test can see.
func TestDeployedVersionIsReadableByOtherUsers(t *testing.T) {
	f := newFixture(t)
	id := f.publishEnv(t, "production", map[string]string{"environment.conf": "modulepath = modules\n"})

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	dir := f.layout.VersionDir("production", id)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm&0o055 != 0o055 {
		t.Errorf("version directory mode is %#o; puppetserver cannot traverse it (want at least r-x for group and other)", perm)
	}

	fi, err := os.Stat(filepath.Join(dir, "environment.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o044 != 0o044 {
		t.Errorf("environment.conf mode is %#o; puppetserver cannot read it", fi.Mode().Perm())
	}
}
