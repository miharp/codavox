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
)

// removeEnv deletes an environment from staging and reseals, the way deleting a
// control-repo branch and redeploying would drop it from the publisher.
func (f *fixture) removeEnv(t *testing.T, env string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(f.staging, env)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Reseal(); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRemovesDeletedEnvironment(t *testing.T) {
	f := newFixture(t)
	f.agent.cfg.Prune = true
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	testID := f.publishEnv(t, "testing", map[string]string{"manifests/site.pp": "test\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.layout.CurrentCodeID("testing"); err != nil {
		t.Fatalf("testing was not deployed: %v", err)
	}

	// Delete testing from the control repo, redeploy: it leaves the publisher.
	f.removeEnv(t, "testing")
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The symlink is gone, and its version too (MinAge is a nanosecond here).
	if _, err := os.Lstat(f.layout.EnvironmentLink("testing")); !os.IsNotExist(err) {
		t.Errorf("testing environment link still present: %v", err)
	}
	if _, err := os.Stat(f.layout.VersionDir("testing", testID)); !os.IsNotExist(err) {
		t.Errorf("testing version directory still present: %v", err)
	}
	// production is untouched.
	if _, err := f.layout.CurrentCodeID("production"); err != nil {
		t.Errorf("production was affected by pruning testing: %v", err)
	}
}

func TestPruneIsOptInByDefault(t *testing.T) {
	f := newFixture(t) // Prune defaults to false
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	f.publishEnv(t, "testing", map[string]string{"manifests/site.pp": "test\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	f.removeEnv(t, "testing")
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// Without opt-in, a removed environment lingers rather than being deleted.
	if _, err := f.layout.CurrentCodeID("testing"); err != nil {
		t.Errorf("testing was pruned without --prune-environments: %v", err)
	}
}

func TestPruneSkipsEmptyAdvertisement(t *testing.T) {
	f := newFixture(t)
	f.agent.cfg.Prune = true
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// Staging emptied: the publisher now advertises nothing.
	f.removeEnv(t, "production")
	if len(f.store.Environments()) != 0 {
		t.Fatal("setup: publisher still advertises environments")
	}

	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// An empty advertisement is treated as suspicious, not "delete everything."
	if _, err := f.layout.CurrentCodeID("production"); err != nil {
		t.Errorf("production was pruned on an empty advertisement: %v", err)
	}
}

func TestPruneSkippedWhenPublisherUnreachable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	f.publishEnv(t, "testing", map[string]string{"manifests/site.pp": "test\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// A second agent, same node, pointed at a publisher that is down.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	down, err := New(Config{
		BaseURL: deadURL,
		Layout:  f.layout,
		Client:  &http.Client{Timeout: 2 * time.Second},
		Prune:   true,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := down.Once(ctx); err == nil {
		t.Fatal("expected an error when the publisher is unreachable")
	}

	// A failed poll must never be read as "every environment was deleted."
	for _, env := range []string{"production", "testing"} {
		if _, err := f.layout.CurrentCodeID(env); err != nil {
			t.Errorf("%s was pruned on a failed poll: %v", env, err)
		}
	}
}

func TestPruneKeepsRecentVersionForInFlightRuns(t *testing.T) {
	f := newFixture(t)
	f.agent.cfg.Prune = true
	f.agent.cfg.MinAge = time.Hour // a just-deployed version is protected
	ctx := context.Background()

	f.publishEnv(t, "production", map[string]string{"manifests/site.pp": "prod\n"})
	testID := f.publishEnv(t, "testing", map[string]string{"manifests/site.pp": "test\n"})
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	f.removeEnv(t, "testing")
	if err := f.agent.Once(ctx); err != nil {
		t.Fatal(err)
	}

	// The symlink goes immediately — no new compiles for a deleted environment.
	if _, err := os.Lstat(f.layout.EnvironmentLink("testing")); !os.IsNotExist(err) {
		t.Errorf("testing environment link was not removed: %v", err)
	}
	// But its version survives MinAge, so an in-flight run's code-content still
	// resolves it.
	if _, err := os.Stat(f.layout.VersionDir("testing", testID)); err != nil {
		t.Errorf("a recent version of a pruned environment was reaped: %v", err)
	}
}
