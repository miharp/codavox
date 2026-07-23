// Package agent keeps a compiler's deployed code in step with the publisher.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
)

// Config controls a running agent.
type Config struct {
	// BaseURL is the publisher, e.g. https://puppet.example.com:8150.
	BaseURL string
	// Layout locates version directories and the environment path.
	Layout layout.Layout
	// Client must carry the node's Puppet certificate.
	Client *http.Client
	// Interval between polls.
	Interval time.Duration
	// Keep is how many superseded versions to retain per environment.
	Keep int
	// MinAge is how long a superseded version is retained regardless of Keep.
	MinAge time.Duration
	Logger *slog.Logger
}

// Defaults applied by New for unset fields.
const (
	DefaultInterval = 30 * time.Second
	DefaultKeep     = 3
	DefaultMinAge   = 2 * time.Hour
)

// Agent polls a publisher and converges local state onto it.
type Agent struct {
	cfg Config
}

// New returns an Agent, filling in defaults.
func New(cfg Config) (*Agent, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("agent needs a publisher URL")
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("agent needs an HTTP client carrying this node's certificate")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultKeep
	}
	if cfg.MinAge <= 0 {
		cfg.MinAge = DefaultMinAge
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	cfg.BaseURL = publish.TrimBase(cfg.BaseURL)
	return &Agent{cfg: cfg}, nil
}

// Run polls until ctx is cancelled.
//
// A poll failure is logged and retried on the next tick rather than being
// fatal. The compiler keeps serving the version it already has, so a publisher
// outage degrades to "no new deploys" rather than "no catalogs" — which is the
// property that makes this better than a shared filesystem.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Once(ctx); err != nil {
		a.cfg.Logger.Error("initial sync failed", "error", err)
	}

	for {
		// Jitter spreads a fleet of compilers out; without it, restarting them
		// together makes them poll in lockstep forever.
		wait := a.cfg.Interval + rand.N(a.cfg.Interval/4) //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			if err := a.Once(ctx); err != nil {
				a.cfg.Logger.Error("sync failed", "error", err)
			}
		}
	}
}

// Once performs a single reconciliation against the publisher.
func (a *Agent) Once(ctx context.Context) error {
	want, err := a.fetchEnvironments(ctx)
	if err != nil {
		return err
	}

	var failures []string
	for env, codeID := range want {
		changed, err := a.sync(ctx, env, codeID)
		if err != nil {
			// One environment failing must not stop the others converging.
			a.cfg.Logger.Error("environment sync failed", "environment", env, "code_id", codeID, "error", err)
			failures = append(failures, env)
			continue
		}
		if changed {
			a.cfg.Logger.Info("environment updated", "environment", env, "code_id", codeID)
		}
		if err := a.reap(env, codeID); err != nil {
			a.cfg.Logger.Warn("reaping old versions failed", "environment", env, "error", err)
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("failed to sync: %s", strings.Join(failures, ", "))
	}
	return nil
}

func (a *Agent) fetchEnvironments(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+publish.EnvironmentsPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("polling publisher: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("publisher returned %s", resp.Status)
	}

	var envs map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
		return nil, fmt.Errorf("decoding environment list: %w", err)
	}
	return envs, nil
}

// sync converges one environment, reporting whether anything changed.
func (a *Agent) sync(ctx context.Context, env, codeID string) (bool, error) {
	if err := layout.ValidateEnvironment(env); err != nil {
		return false, err
	}
	if err := layout.ValidateCodeID(codeID); err != nil {
		return false, err
	}

	current, err := a.cfg.Layout.CurrentCodeID(env)
	if err == nil && current == codeID {
		return false, nil
	}

	dir := a.cfg.Layout.VersionDir(env, codeID)
	if _, statErr := os.Stat(dir); statErr != nil {
		if err := a.download(ctx, env, codeID); err != nil {
			return false, err
		}
	}

	if err := a.swap(env, codeID); err != nil {
		return false, err
	}
	return true, nil
}

// download fetches, verifies and unpacks a version.
//
// Extraction happens into a temporary directory that is renamed into place
// only once the content has been verified, so a failed or partial transfer
// never leaves a directory that looks like a valid version.
func (a *Agent) download(ctx context.Context, env, codeID string) error {
	url := a.cfg.BaseURL + publish.ArtifactPath(env, codeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: publisher returned %s", url, resp.Status)
	}

	final := a.cfg.Layout.VersionDir(env, codeID)
	// 0755: the agent runs as root but OpenVox Server reads these trees as the
	// puppet user. Tightening this without also managing group ownership would
	// leave every compiler unable to read its own code.
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil { // #nosec G301
		return fmt.Errorf("creating versions directory: %w", err)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(final), "."+layout.VersionDirName(env, codeID)+".*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	// Removing tmp is a no-op once it has been renamed away.
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := seal.ExtractArchive(resp.Body, tmp); err != nil {
		return fmt.Errorf("extracting artifact: %w", err)
	}

	// Verify by resealing rather than by digesting the transfer. A matching
	// download digest would only prove the bytes arrived intact; resealing
	// proves the tree on disk is the one the code_id names, which is the claim
	// every catalog compiled against it depends on.
	got, err := seal.CodeID(tmp)
	if err != nil {
		return fmt.Errorf("verifying artifact: %w", err)
	}
	if got != codeID {
		return fmt.Errorf("artifact for %s does not match its code_id: got %s, want %s", env, got, codeID)
	}

	if err := os.Rename(tmp, final); err != nil {
		// Another agent process may have won the race; that is fine, since the
		// content is identical by construction.
		if _, statErr := os.Stat(final); statErr == nil {
			return nil
		}
		return fmt.Errorf("installing version directory: %w", err)
	}
	return nil
}

// swap atomically repoints the environment at a version.
//
// The link is created under a temporary name and renamed over the old one.
// rename(2) is atomic, so OpenVox Server either resolves the old version or
// the new one and never an absent or half-written link. `ln -sf` unlinks
// first, leaving a window where the environment does not exist at all.
func (a *Agent) swap(env, codeID string) error {
	// 0755 for the same reason as the versions directory: OpenVox Server
	// resolves these links as the puppet user.
	if err := os.MkdirAll(a.cfg.Layout.EnvironmentPath, 0o755); err != nil { // #nosec G301
		return fmt.Errorf("creating environment path: %w", err)
	}

	target := a.cfg.Layout.VersionDir(env, codeID)
	link := a.cfg.Layout.EnvironmentLink(env)

	tmp := filepath.Join(a.cfg.Layout.EnvironmentPath,
		fmt.Sprintf(".%s.%d.tmp", env, time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("creating temporary link: %w", err)
	}

	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swapping environment link: %w", err)
	}
	return nil
}

// reap removes superseded versions of an environment.
//
// A version is kept while it is current, while it is among the most recent
// Keep, or while it is younger than MinAge. The age rule is the one that
// matters: an agent run that received a catalog stamped with an old code_id
// will still request file content for it, and deleting that tree turns a
// successful run into a failed one.
func (a *Agent) reap(env, current string) error {
	versionsDir := filepath.Join(a.cfg.Layout.Root, "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	prefix := env + "_"
	type version struct {
		name    string
		modTime time.Time
	}
	var candidates []version

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if e.Name() == layout.VersionDirName(env, current) {
			continue
		}
		// Skip in-progress extractions, which are dot-prefixed.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, version{name: e.Name(), modTime: info.ModTime()})
	}

	// Newest first, so the retained set is the most recently deployed.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	cutoff := time.Now().Add(-a.cfg.MinAge)
	for i, v := range candidates {
		if i < a.cfg.Keep {
			continue
		}
		if v.modTime.After(cutoff) {
			continue
		}
		path := filepath.Join(versionsDir, v.name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing %s: %w", v.name, err)
		}
		a.cfg.Logger.Info("reaped old version", "environment", env, "version", v.name)
	}
	return nil
}
