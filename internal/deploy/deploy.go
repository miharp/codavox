// Package deploy runs r10k to stage code and triggers the publisher to reseal.
//
// It is the single deploy path. The deploy subcommand calls Run today; a
// webhook receiver or deploy API will call the same function later, so the
// logic that turns "deploy this environment" into staged, sealed, served code
// lives in one place rather than being reimplemented per entry point.
//
// codavox does not re-resolve code per compiler — that is the load-bearing
// invariant. Running r10k here does not weaken it: r10k runs once, centrally,
// producing one resolved tree that every compiler then converges onto, exactly
// as PE's Code Manager runs r10k once and distributes the result.
package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
)

// DefaultWaitTimeout bounds how long Run blocks for the publisher to serve a
// new version when Wait is set. It covers only the reseal, not r10k, which Run
// always waits for synchronously.
const DefaultWaitTimeout = 60 * time.Second

// DefaultLockTimeout bounds how long Run waits for a concurrent deploy to
// release the basedir lock. It is generous because r10k, which a competing
// deploy runs while holding the lock, can itself take minutes.
const DefaultLockTimeout = 10 * time.Minute

// fallbackR10k is the OpenVox package path, tried when r10k is not on PATH.
const fallbackR10k = "/opt/puppetlabs/puppet/bin/r10k"

// Config controls a deploy.
type Config struct {
	// R10kPath is the r10k binary; empty means discover it on PATH, then fall
	// back to the OpenVox package path.
	R10kPath string
	// R10kConfig is an optional r10k.yaml passed to r10k with --config.
	R10kConfig string
	// BaseDir is r10k's basedir and what the publisher seals. Required, and
	// it must match the publisher's --basedir.
	BaseDir string
	// StateDir locates the publisher's pidfile and materialized artifacts. It
	// must match the publisher's --state.
	StateDir string
	// Modules resolves Puppetfile modules (r10k --puppetfile).
	Modules bool
	// WaitTimeout bounds the Wait poll; zero uses DefaultWaitTimeout.
	WaitTimeout time.Duration
	// LockTimeout bounds how long Run waits to acquire the basedir lock behind
	// another deploy; zero uses DefaultLockTimeout.
	LockTimeout time.Duration
	// Stderr receives r10k's output; nil means os.Stderr.
	Stderr io.Writer
}

// Result is the outcome for one environment.
type Result struct {
	Env     string `json:"environment"`
	CodeID  string `json:"code_id,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Serving bool   `json:"serving"`
	Err     string `json:"error,omitempty"`
}

// Run deploys environments and, when wait is set, blocks until the publisher
// serves each new code_id.
//
// It runs r10k once for all requested environments, seals each resulting tree
// to report its code_id, signals the running publisher to reseal, and — with
// wait — polls until the publisher has materialized the new artifact. The
// returned error is non-nil if the deploy did not fully succeed; the results
// are still returned so a caller can report per-environment status.
func Run(cfg Config, envs []string, all, wait bool) ([]Result, error) {
	if cfg.BaseDir == "" {
		return nil, errors.New("deploy needs a basedir directory")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("deploy needs a state directory")
	}
	if all && len(envs) > 0 {
		return nil, errors.New("give environments or --all, not both")
	}
	if !all && len(envs) == 0 {
		return nil, errors.New("deploy needs an environment name or --all")
	}
	for _, env := range envs {
		if err := layout.ValidateEnvironment(env); err != nil {
			return nil, err
		}
	}

	r10k, err := resolveR10k(cfg.R10kPath)
	if err != nil {
		return nil, err
	}

	// Serialize against every other deploy on this host. r10k mutates the
	// basedir directory in place, so two overlapping deploys — from the CLI, the
	// webhook, or the deploy API — would corrupt each other's trees. The lock is
	// held across r10k and sealing, and through --wait, so a second deploy does
	// not begin r10k until this one's reseal has been observed.
	lockTimeout := cfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	release, err := lockDeploy(cfg.StateDir, lockTimeout)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := runR10k(cfg, r10k, envs); err != nil {
		return nil, err
	}

	// For --all, report whatever is now staged; r10k does not tell us the set.
	deployed := envs
	if all {
		deployed, err = stagedEnvironments(cfg.BaseDir)
		if err != nil {
			return nil, err
		}
	}

	results := make([]Result, 0, len(deployed))
	for _, env := range deployed {
		r := Result{Env: env}
		id, commit, err := sealEnv(cfg.BaseDir, env)
		if err != nil {
			r.Err = err.Error()
			results = append(results, r)
			continue
		}
		r.CodeID = id
		r.Commit = commit
		results = append(results, r)
	}

	// One SIGHUP reseals every staged environment at once.
	signalErr := SignalPublisher(cfg.StateDir)

	if wait {
		if signalErr != nil {
			// Nothing will materialize the new version, so waiting cannot succeed.
			return results, fmt.Errorf("cannot wait for the deploy to be served: %w", signalErr)
		}
		timeout := cfg.WaitTimeout
		if timeout <= 0 {
			timeout = DefaultWaitTimeout
		}
		deadline := time.Now().Add(timeout)
		for i := range results {
			if results[i].Err != "" || results[i].CodeID == "" {
				continue
			}
			if err := waitServing(cfg.StateDir, results[i].Env, results[i].CodeID, deadline); err != nil {
				results[i].Err = err.Error()
			} else {
				results[i].Serving = true
			}
		}
	} else if signalErr != nil {
		// A hard error, not a warning. r10k updated the basedir, but nothing was
		// told to seal it, so no compiler will ever see this deploy — and the
		// per-environment results say "deployed" with a fresh code_id beside it.
		// Exiting 0 there would be exactly the failure this project exists to
		// prevent: a plausible success while the real outcome is wrong, which CI
		// records as green and nobody notices until compilers are found stale.
		_, _ = fmt.Fprintf(stderr(cfg), "codavox: %v\n", signalErr)
		return results, fmt.Errorf("deployed to the basedir but nothing is serving it: %w", signalErr)
	}

	return results, summarize(results)
}

func stderr(cfg Config) io.Writer {
	if cfg.Stderr != nil {
		return cfg.Stderr
	}
	return os.Stderr
}

// resolveR10k finds the r10k binary: an explicit path wins, then PATH, then the
// OpenVox package location.
func resolveR10k(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("r10k not found at %s: %w", explicit, err)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath("r10k"); err == nil {
		return p, nil
	}
	if _, err := os.Stat(fallbackR10k); err == nil {
		return fallbackR10k, nil
	}
	return "", fmt.Errorf("r10k not found on PATH or at %s; set --r10k", fallbackR10k)
}

func runR10k(cfg Config, r10k string, envs []string) error {
	args := []string{"deploy", "environment"}
	args = append(args, envs...) // empty for --all
	if cfg.Modules {
		args = append(args, "--puppetfile")
	}
	if cfg.R10kConfig != "" {
		args = append(args, "--config", cfg.R10kConfig)
	}

	cmd := exec.Command(r10k, args...) // #nosec G204 -- r10k path and env names are validated inputs
	// r10k logs to stderr; send everything there so deploy's own stdout stays
	// reserved for results.
	cmd.Stdout = stderr(cfg)
	cmd.Stderr = stderr(cfg)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("r10k deploy failed: %w", err)
	}
	return nil
}

// stagedEnvironments lists the valid environment directories in baseDir.
func stagedEnvironments(baseDir string) ([]string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("reading basedir directory: %w", err)
	}
	var envs []string
	for _, e := range entries {
		if e.IsDir() && layout.ValidateEnvironment(e.Name()) == nil {
			envs = append(envs, e.Name())
		}
	}
	return envs, nil
}

// sealEnv computes the code_id for a staged environment and reads the commit
// r10k recorded for it.
func sealEnv(baseDir, env string) (codeID, commit string, err error) {
	envDir := filepath.Join(baseDir, env)
	if _, err := os.Stat(envDir); err != nil {
		return "", "", fmt.Errorf("environment %s was not staged: %w", env, err)
	}
	id, err := seal.CodeID(envDir)
	if err != nil {
		return "", "", err
	}
	return id, deployCommit(envDir), nil
}

// deployCommit reads r10k's resolved commit from .r10k-deploy.json, best-effort.
func deployCommit(envDir string) string {
	b, err := os.ReadFile(filepath.Join(envDir, ".r10k-deploy.json")) // #nosec G304
	if err != nil {
		return ""
	}
	var d struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return ""
	}
	return d.Signature
}

// lockDeploy takes an exclusive flock on the deploy lock under stateDir,
// returning a release function. It polls rather than blocking in the syscall so
// it can give up after timeout instead of hanging on a wedged deploy.
//
// flock is advisory and process-associated, released if the holder exits, so a
// crashed deploy does not leave the lock stuck.
func lockDeploy(stateDir string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	path := filepath.Join(stateDir, "deploy.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- state dir is operator-supplied
	if err != nil {
		return nil, fmt.Errorf("opening deploy lock: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking basedir: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errors.New("timed out waiting for a concurrent deploy to finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// SignalPublisher sends SIGHUP to the running publisher so it reseals.
//
// It reads the pidfile the publisher writes under stateDir and verifies the
// process is alive before signaling, so a stale pidfile from a crashed
// publisher is reported rather than signaling an unrelated process.
func SignalPublisher(stateDir string) error {
	pidPath := publish.PidFilePath(stateDir)
	b, err := os.ReadFile(pidPath) // #nosec G304
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no running publisher (no pidfile)")
		}
		return fmt.Errorf("reading publisher pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("malformed publisher pidfile %s: %w", pidPath, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding publisher process %d: %w", pid, err)
	}
	// Signal 0 probes liveness without delivering anything.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("publisher process %d is not running (stale pidfile)", pid)
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("signaling publisher %d: %w", pid, err)
	}
	return nil
}

// waitServing polls until the publisher has materialized the artifact for a
// version, which is the observable signal that its reseal took effect.
func waitServing(stateDir, env, codeID string, deadline time.Time) error {
	artifact := publish.ArtifactPathFor(stateDir, env, codeID)
	for {
		if _, err := os.Stat(artifact); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the publisher to serve this version")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func summarize(results []Result) error {
	var failed []string
	for _, r := range results {
		if r.Err != "" {
			failed = append(failed, r.Env)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("deploy incomplete: %s", strings.Join(failed, ", "))
	}
	return nil
}
