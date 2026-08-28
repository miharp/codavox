package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
	"github.com/miharp/codavox/internal/testca"
)

// fakePeerState builds the ConnectionState mutual TLS would have produced, so a
// plaintext test server can still exercise the certname path.
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

// fixture wires a publisher and a compiler-side agent against temp directories.
type fixture struct {
	basedir string
	store   *publish.Store
	server  *httptest.Server
	agent   *Agent
	layout  layout.Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	basedir := t.TempDir()
	store := publish.NewStore(basedir, t.TempDir())
	server := httptest.NewServer(publish.Handler(store, nil, nil))
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

	return &fixture{basedir: basedir, store: store, server: server, agent: a, layout: l}
}

// publishEnv writes an environment into basedir and reseals.
func (f *fixture) publishEnv(t *testing.T, env string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(f.basedir, env)
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

// A dot-prefixed directory is what download leaves behind when a crash --
// SIGKILL, an OOM kill, a power loss -- skips its deferred cleanup mid-extract.
// It must be swept once clearly abandoned, but never while it could still be a
// live extraction in another process, which looks identical until it is old
// enough to rule that out.
func TestReapRemovesAbandonedExtractionsPastMinAge(t *testing.T) {
	f := newFixture(t)
	f.agent.cfg.MinAge = time.Hour
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "x\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	versionsDir := filepath.Join(f.layout.Root, "versions")

	abandoned := filepath.Join(versionsDir, ".production_deadbeef.123456")
	if err := os.Mkdir(abandoned, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}

	// mtime defaults to now, well inside MinAge -- simulating an extraction
	// still actively creating entries under it.
	live := filepath.Join(versionsDir, ".production_cafef00d.654321")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("abandoned extraction past MinAge was not reaped: err=%v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("extraction younger than MinAge was reaped: %v", err)
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

// A single reconciliation must leave the publisher knowing what this node now
// serves. Reporting only on the poll would trail every deploy by a full
// interval, which is long enough for an operator watching a deploy land to read
// a converged compiler as a stale one.
func TestOnceReportsTheVersionItJustDeployed(t *testing.T) {
	f := newFixture(t)

	// Stand in for what mutual TLS would put on the request, so the publisher
	// files the report under a certname.
	peers := publish.NewPeers()
	reporting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.TLS = fakePeerState(t, "compiler01.example.com")
		publish.Handler(f.store, nil, peers).ServeHTTP(w, r)
	}))
	defer reporting.Close()
	f.agent.cfg.BaseURL = reporting.URL
	f.agent.cfg.Client = reporting.Client()

	want := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "v1\n"})

	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	list := peers.List()
	if len(list) != 1 {
		t.Fatalf("got %d peers, want 1", len(list))
	}
	if got := list[0].Serving["production"]; got != want {
		t.Errorf("publisher has %q after one sync, want the version just deployed %q", got, want)
	}

	// A converged run adds no request. The follow-up report exists for the run
	// that changed something; paying for it every interval would be a poll's
	// worth of traffic per compiler for no new information.
	before := list[0].Polls
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := peers.List()[0].Polls - before; got != 1 {
		t.Errorf("a converged run made %d requests, want 1", got)
	}
}

// cacheFlushRecorder stands in for OpenVox Server's admin API, recording each
// environment it was asked to expire and answering with whatever status the
// test has queued.
type cacheFlushRecorder struct {
	*httptest.Server
	status   int
	flushed  []string
	requests int
}

func newCacheFlushRecorder(t *testing.T) *cacheFlushRecorder {
	t.Helper()
	r := &cacheFlushRecorder{status: http.StatusNoContent}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests++
		if req.Method != http.MethodDelete || req.URL.Path != EnvironmentCachePath {
			t.Errorf("unexpected request %s %s; want DELETE %s", req.Method, req.URL.Path, EnvironmentCachePath)
		}
		r.flushed = append(r.flushed, req.URL.Query().Get("environment"))
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.Close)
	return r
}

func TestSwapFlushesThatEnvironmentsCache(t *testing.T) {
	f := newFixture(t)
	ps := newCacheFlushRecorder(t)
	f.agent.cfg.PuppetServer = ps.URL

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})
	f.publishEnv(t, "staging", map[string]string{"manifests/site.pp": "node default { }\n"})
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	got := append([]string(nil), ps.flushed...)
	sort.Strings(got)
	if want := []string{"production", "staging"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flushed %v after the first deploy, want %v", got, want)
	}

	// Nothing changed: nothing to expire. A flush on every poll would make the
	// server re-parse both environments every 30 seconds for no reason.
	ps.flushed = nil
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(ps.flushed) != 0 {
		t.Errorf("a no-op reconciliation flushed %v", ps.flushed)
	}

	// Only the environment that moved is expired, not every cached one.
	f.publishEnv(t, "staging", map[string]string{"manifests/site.pp": "node default { notify { 'x': } }\n"})
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if want := []string{"staging"}; !reflect.DeepEqual(ps.flushed, want) {
		t.Errorf("flushed %v after staging moved, want %v", ps.flushed, want)
	}
}

// A swap cannot be undone, so a flush that fails is a reconciliation that
// failed — the server is compiling the old tree under the new code_id until it
// lands — and it stays owed, retried on the next poll even with nothing new to
// deploy.
func TestFailedFlushFailsTheSyncAndIsRetried(t *testing.T) {
	f := newFixture(t)
	ps := newCacheFlushRecorder(t)
	ps.status = http.StatusForbidden
	f.agent.cfg.PuppetServer = ps.URL

	want := f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})
	err := f.agent.Once(context.Background())
	if err == nil {
		t.Fatal("Once succeeded although the environment cache flush was refused")
	}
	if !strings.Contains(err.Error(), "flush") || !strings.Contains(err.Error(), "production") {
		t.Errorf("error should name the flush and the environment, got: %v", err)
	}
	// The deploy itself still landed: refusing the swap would not help, the
	// server has to be told either way.
	if got, _ := f.layout.CurrentCodeID("production"); got != want {
		t.Errorf("code-id = %s after a failed flush, want %s (the swap must not be rolled back)", got, want)
	}

	ps.status = http.StatusNoContent
	ps.flushed = nil
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once after the server started allowing the flush: %v", err)
	}
	if want := []string{"production"}; !reflect.DeepEqual(ps.flushed, want) {
		t.Errorf("retry flushed %v, want %v", ps.flushed, want)
	}

	// Paid off: a third poll owes nothing.
	ps.flushed = nil
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(ps.flushed) != 0 {
		t.Errorf("an already-flushed environment was flushed again: %v", ps.flushed)
	}
}

func TestPruneFlushesTheRemovedEnvironment(t *testing.T) {
	f := newFixture(t)
	ps := newCacheFlushRecorder(t)
	f.agent.cfg.PuppetServer = ps.URL
	f.agent.cfg.Prune = true

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})
	f.publishEnv(t, "feature", map[string]string{"manifests/site.pp": "node default { }\n"})
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	// The publisher stops advertising feature; the server must stop compiling
	// its cached copy too, or it keeps serving an environment that no longer
	// exists.
	if err := os.RemoveAll(filepath.Join(f.basedir, "feature")); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Reseal(); err != nil {
		t.Fatal(err)
	}
	ps.flushed = nil
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if want := []string{"feature"}; !reflect.DeepEqual(ps.flushed, want) {
		t.Errorf("flushed %v after pruning feature, want %v", ps.flushed, want)
	}
}

func TestNoPuppetServerMeansNoFlush(t *testing.T) {
	f := newFixture(t)
	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "node default { }\n"})
	if err := f.agent.Once(context.Background()); err != nil {
		t.Fatalf("Once with the flush disabled: %v", err)
	}
	if len(f.agent.pendingFlush) != 0 {
		t.Errorf("flush disabled, yet %v is recorded as owed", f.agent.pendingFlush)
	}
}
