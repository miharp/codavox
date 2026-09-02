// Package agent keeps a compiler's deployed code in step with the publisher.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
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
	// MaxUnpacked bounds how far one artifact may expand on disk, in bytes;
	// zero is seal.DefaultMaxBytes. An artifact past it is refused before the
	// byte that would cross it is written, so a publisher — compromised, or
	// just wrong — cannot fill every compiler's disk with one small file.
	MaxUnpacked int64
	// Prune removes environments the publisher no longer advertises. It is off
	// by default: deleting an environment is destructive, so an operator opts in.
	Prune bool
	// PuppetServer is the base URL of the OpenVox Server this node runs, e.g.
	// https://compiler01.example.com:8140, whose environment cache the agent
	// flushes after every swap. Empty disables the flush, for a compiler whose
	// environment_timeout is 0 and so has no cache to flush.
	PuppetServer string
	Logger       *slog.Logger
}

// EnvironmentCachePath is OpenVox Server's admin endpoint for expiring a cached
// environment: DELETE with ?environment=<name>. Verified against
// puppet_admin_core.clj; see docs/versioned-code-contract.md.
const EnvironmentCachePath = "/puppet-admin-api/v1/environment-cache"

// flushTimeout bounds one cache flush. The shared client's own timeout is sized
// for artifact downloads, which is far too generous for a request that returns
// 204 with no body.
const flushTimeout = 30 * time.Second

// Defaults applied by New for unset fields.
const (
	DefaultInterval = 30 * time.Second
	DefaultKeep     = 3
	DefaultMinAge   = 2 * time.Hour
)

// Agent polls a publisher and converges local state onto it.
type Agent struct {
	cfg Config

	// syncFailures collapses a run of identical poll failures. Only Run touches
	// it, so a single agent loop owns it and Once stays free of logging state.
	syncFailures failureLog

	// pendingFlush names the environments whose OpenVox Server cache is owed a
	// flush: swapped or pruned, but not yet expired server-side. An entry
	// outlives a failed flush so the next poll retries it — a swap cannot be
	// undone once the symlink has moved, and until the flush lands the server
	// keeps compiling the old tree while code-id reports the new one. Only Once
	// touches it, from the single loop that owns the agent.
	pendingFlush map[string]bool
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
	cfg.PuppetServer = publish.TrimBase(cfg.PuppetServer)
	return &Agent{cfg: cfg, pendingFlush: map[string]bool{}}, nil
}

// Run polls until ctx is cancelled.
//
// A poll failure is logged and retried on the next tick rather than being
// fatal. The compiler keeps serving the version it already has, so a publisher
// outage degrades to "no new deploys" rather than "no catalogs" — which is the
// property that makes this better than a shared filesystem.
func (a *Agent) Run(ctx context.Context) error {
	// The first poll is jittered too, by the same amount used between polls.
	// Without this, a fleet restarted together — a package upgrade, a reboot —
	// makes every agent's very first request land in the same instant: the
	// one poll the steady-state jitter below cannot reach, since it only takes
	// effect after this call returns.
	initial := rand.N(a.cfg.Interval / 4) //nolint:gosec // jitter, not a secret
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(initial):
	}
	a.reportSync(a.Once(ctx))

	for {
		// Jitter spreads a fleet of compilers out; without it, restarting them
		// together makes them poll in lockstep forever.
		wait := a.cfg.Interval + rand.N(a.cfg.Interval/4) //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			a.reportSync(a.Once(ctx))
		}
	}
}

// reportSync logs the outcome of one reconciliation.
//
// Repeats are collapsed rather than logged per poll: an unreachable publisher is
// a survivable state, and describing it at ERROR every interval buries the moment
// it started and trains people to ignore the level. See failureLog.
func (a *Agent) reportSync(err error) {
	if err != nil {
		a.syncFailures.failed(a.cfg.Logger, "sync failed", err)
		return
	}
	a.syncFailures.succeeded(a.cfg.Logger, "sync recovered")
}

// Once performs a single reconciliation against the publisher.
func (a *Agent) Once(ctx context.Context) error {
	want, err := a.fetchEnvironments(ctx)
	if err != nil {
		return err
	}

	var failures []string
	var moved bool
	for env, codeID := range want {
		changed, err := a.sync(ctx, env, codeID)
		if err != nil {
			// One environment failing must not stop the others converging.
			a.cfg.Logger.Error("environment sync failed", "environment", env, "code_id", codeID, "error", err)
			failures = append(failures, env)
			continue
		}
		if changed {
			moved = true
			a.oweFlush(env)
			a.cfg.Logger.Info("environment updated", "environment", env, "code_id", codeID)
		}
		if err := a.reap(env, codeID); err != nil {
			a.cfg.Logger.Warn("reaping old versions failed", "environment", env, "error", err)
		}
	}

	// Fleet-wide, not per-environment: it sweeps everything abandoned in
	// versions/, regardless of which environment it belonged to.
	a.reapStaleExtractions()

	// Prune runs only after a successful fetch (a failed one returned above), so
	// a publisher outage is never mistaken for "every environment was deleted."
	// It is independent of per-environment sync failures: a failed sync leaves
	// the environment in want, so it is never a prune candidate.
	if a.cfg.Prune {
		for _, env := range a.prune(want) {
			moved = true
			a.oweFlush(env)
		}
	}

	// Expire what OpenVox Server has cached for every environment that moved,
	// now that every symlink is in place. The swap alone is not a deploy: with
	// environment_timeout set, the server keeps compiling the tree it already
	// parsed while code-id — reading the symlink just moved — reports the new
	// code_id, so a catalog would carry a code_id that does not describe it.
	// That is the silent mismatch static catalogs exist to prevent, so a
	// failed flush fails the reconciliation, and stays owed until it lands.
	unflushed := a.flushPending(ctx)

	// Tell the publisher what this node now serves, rather than leaving it to
	// be noticed on the next poll. The poll above reported the state *before*
	// this reconciliation, so without this the fleet view would trail every
	// deploy by a full interval — long enough for an operator watching a deploy
	// land to read it as a compiler that had not converged.
	//
	// It costs one request, and only when something actually changed. The
	// steady state — a converged compiler polling and finding nothing to do —
	// adds nothing, and it goes down the keep-alive connection the poll just
	// used.
	if moved {
		a.reportServing(ctx)
	}

	var problems []string
	if len(failures) > 0 {
		sort.Strings(failures)
		problems = append(problems, "failed to sync: "+strings.Join(failures, ", "))
	}
	if len(unflushed) > 0 {
		problems = append(problems, "failed to flush the environment cache for: "+strings.Join(unflushed, ", "))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// oweFlush records that env's server-side cache must be expired before this
// node is fully converged. With no PuppetServer configured nothing is ever owed.
func (a *Agent) oweFlush(env string) {
	if a.cfg.PuppetServer == "" {
		return
	}
	a.pendingFlush[env] = true
}

// flushPending expires every environment owed a flush, returning the ones still
// owed afterwards, sorted.
func (a *Agent) flushPending(ctx context.Context) []string {
	if len(a.pendingFlush) == 0 {
		return nil
	}
	var unflushed []string
	for env := range a.pendingFlush {
		if err := a.flushEnvironmentCache(ctx, env); err != nil {
			a.cfg.Logger.Error("environment cache flush failed", "environment", env, "error", err)
			unflushed = append(unflushed, env)
			continue
		}
		delete(a.pendingFlush, env)
		a.cfg.Logger.Info("environment cache flushed", "environment", env)
	}
	sort.Strings(unflushed)
	return unflushed
}

// flushEnvironmentCache asks this node's OpenVox Server to expire one cached
// environment, so its next catalog compile re-reads the tree the environment
// symlink now points at.
//
// It expires one environment, not all of them: the others were not touched, and
// dropping their caches would make every deploy re-parse the whole estate.
// The request goes over the same client as the poll, carrying this node's own
// Puppet certificate, which is what auth.conf on the server must admit.
func (a *Agent) flushEnvironmentCache(ctx context.Context, env string) error {
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()

	target := a.cfg.PuppetServer + EnvironmentCachePath + "?environment=" + url.QueryEscape(env)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusForbidden:
		// The shipped auth.conf denies this endpoint to everyone. Name the fix,
		// because the status alone reads like a certificate problem.
		return fmt.Errorf("DELETE %s: %s (auth.conf on this OpenVox Server must allow this node to reach %s)",
			target, resp.Status, EnvironmentCachePath)
	default:
		return fmt.Errorf("DELETE %s: %s", target, resp.Status)
	}
}

// reportServing re-states what this node serves, immediately after converging.
//
// It repeats the poll rather than using an endpoint of its own: the publisher
// already reads the report from any poll, so a second API to maintain would buy
// nothing. The response is discarded — this call exists for its header.
//
// Failure is ignored and not even logged at error level. The next poll carries
// the same report, so the only cost of a failed one is the lag this exists to
// remove.
func (a *Agent) reportServing(ctx context.Context) {
	serving := a.serving()
	if serving == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+publish.EnvironmentsPath, nil)
	if err != nil {
		return
	}
	req.Header.Set(publish.ServingHeader, serving)

	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		a.cfg.Logger.Debug("reporting deployed versions failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection returns to the pool for the next poll.
	_, _ = io.Copy(io.Discard, resp.Body)
}

func (a *Agent) fetchEnvironments(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.BaseURL+publish.EnvironmentsPath, nil)
	if err != nil {
		return nil, err
	}
	// Report what this compiler is actually serving, read from the same symlinks
	// code-id reads. The publisher can otherwise only infer convergence from
	// what it saw a node fetch, which is not the same claim: an agent that
	// fetched an artifact may still have failed to verify or unpack it.
	//
	// It rides along on a poll the agent already makes, so no compiler needs a
	// listener and every connection stays outbound. This one describes the
	// state before this reconciliation; reportServing follows up if it changes
	// anything.
	if serving := a.serving(); serving != "" {
		req.Header.Set(publish.ServingHeader, serving)
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

// serving renders what this node currently serves, as the publisher's header
// expects. Best effort throughout: an unreadable link is simply omitted, since
// failing a poll over a diagnostic would trade a working deploy for a report.
func (a *Agent) serving() string {
	envs, err := a.localEnvironments()
	if err != nil || len(envs) == 0 {
		return ""
	}
	sort.Strings(envs)

	var b strings.Builder
	for _, env := range envs {
		id, err := a.cfg.Layout.CurrentCodeID(env)
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(env)
		b.WriteByte('=')
		b.WriteString(id)
	}
	return b.String()
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
		return fmt.Errorf("creating basedir directory: %w", err)
	}
	// Removing tmp is a no-op once it has been renamed away.
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := seal.ExtractArchiveWithin(resp.Body, tmp, seal.Limits{Bytes: a.cfg.MaxUnpacked}); err != nil {
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

	// os.MkdirTemp creates 0700. OpenVox Server reads this tree as the puppet
	// user while the agent runs as root, so leaving it private means every
	// catalog compile fails with EACCES on environment.conf. Files inside are
	// already normalized to 0644/0755 during extraction; only the directory
	// the temp helper created needs widening.
	if err := os.Chmod(tmp, 0o755); err != nil { // #nosec G302
		return fmt.Errorf("setting permissions on %s: %w", tmp, err)
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
	return a.reapVersions(env, current, a.cfg.Keep)
}

// reapVersions removes an environment's superseded versions, keeping the current
// one, the most recent keep, and anything younger than MinAge. A deleted
// environment passes current="" and keep=0, so only the age guard protects its
// versions and they all disappear once past MinAge.
func (a *Agent) reapVersions(env, current string, keep int) error {
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
		if i < keep {
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

// reapStaleExtractions removes dot-prefixed extraction directories abandoned
// mid-download by a crash -- a SIGKILL, an OOM kill, a power loss -- none of
// which run download's deferred cleanup, so the directory is left exactly as
// it was.
//
// reapVersions must never touch these: a directory still being extracted by a
// concurrently running agent looks identical to an abandoned one, and pulling
// it out from under a live extraction would be worse than leaving it. Age is
// what tells them apart. A live extraction keeps creating entries under its
// directory, which keeps bumping that directory's own mtime, so it never goes
// stale; an abandoned one stops the instant the process dies and its mtime
// stops moving with it. MinAge is already the bound the rest of reap uses for
// "how long before we are sure nothing still needs this" -- performance.md's
// fixture unpacks in well under a second, so even the 2-hour default leaves an
// enormous margin before this ever mistakes a live extraction for a dead one.
func (a *Agent) reapStaleExtractions() {
	versionsDir := filepath.Join(a.cfg.Layout.Root, "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		// A per-environment reap in this same cycle already surfaces a real
		// ReadDir failure; a missing versions directory just means nothing has
		// been synced yet.
		return
	}

	cutoff := time.Now().Add(-a.cfg.MinAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(versionsDir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			a.cfg.Logger.Warn("reaping abandoned extraction failed", "path", path, "error", err)
			continue
		}
		a.cfg.Logger.Info("reaped abandoned extraction",
			"path", path, "age", time.Since(info.ModTime()).Round(time.Minute))
	}
}

// prune removes environments the publisher no longer advertises.
//
// It never acts on an empty advertisement: a publisher serving zero
// environments is far more likely misconfigured, or pointed at an empty basedir
// directory, than deliberately deleting every environment at once. Deleting the
// last environment stays a manual action.
// It returns the environments it removed, so the caller can re-state what this
// node serves — a pruned environment leaves the set the agent reports — and
// expire what OpenVox Server had cached for each.
func (a *Agent) prune(want map[string]string) []string {
	if len(want) == 0 {
		a.cfg.Logger.Warn("publisher advertised no environments; skipping prune")
		return nil
	}
	local, err := a.localEnvironments()
	if err != nil {
		a.cfg.Logger.Warn("listing local environments failed; skipping prune", "error", err)
		return nil
	}
	var removed []string
	for _, env := range local {
		if _, kept := want[env]; kept {
			continue
		}
		if err := a.pruneEnvironment(env); err != nil {
			a.cfg.Logger.Warn("pruning environment failed", "environment", env, "error", err)
			continue
		}
		removed = append(removed, env)
	}
	return removed
}

// localEnvironments lists the environments deployed on this node — the symlinks
// under the environment path. Dot-prefixed temporary swap files and anything
// that is not a symlink are skipped.
func (a *Agent) localEnvironments() ([]string, error) {
	entries, err := os.ReadDir(a.cfg.Layout.EnvironmentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var envs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if layout.ValidateEnvironment(e.Name()) != nil {
			continue
		}
		envs = append(envs, e.Name())
	}
	return envs, nil
}

// pruneEnvironment removes a deleted environment.
//
// The symlink goes immediately, so new catalog compiles for the environment
// fail loudly, which is correct — it no longer exists. Its version directories
// are reaped only once older than MinAge, so an in-flight agent run that still
// requests file content by code_id is not cut off; code-content resolves the
// version directory directly, without the symlink.
func (a *Agent) pruneEnvironment(env string) error {
	link := a.cfg.Layout.EnvironmentLink(env)
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing environment link: %w", err)
	}
	a.cfg.Logger.Info("pruned environment", "environment", env)
	// No current version to keep and no keep-count: only the age guard applies.
	return a.reapVersions(env, "", 0)
}
