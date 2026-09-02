package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
)

// writeFakeR10k writes a stand-in r10k that stages the given environments and
// exits with exitCode. It ignores its arguments and materializes a fixed tree,
// which is all Run needs: Run's job is to invoke r10k and seal whatever it
// produced, not to drive r10k's resolution.
func writeFakeR10k(t *testing.T, basedir string, envs []string, exitCode int) string {
	t.Helper()
	script := "#!/bin/sh\n"
	for _, env := range envs {
		dir := filepath.Join(basedir, env)
		script += fmt.Sprintf("mkdir -p %q\n", filepath.Join(dir, "manifests"))
		script += fmt.Sprintf("printf 'node default { }\\n' > %q\n", filepath.Join(dir, "manifests", "site.pp"))
		script += fmt.Sprintf("printf '{\"name\":%q,\"signature\":\"commit-%s\"}' > %q\n",
			env, env, filepath.Join(dir, ".r10k-deploy.json"))
	}
	script += fmt.Sprintf("exit %d\n", exitCode)

	path := filepath.Join(t.TempDir(), "r10k")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	return path
}

func TestRunDeploysAndSeals(t *testing.T) {
	basedir := t.TempDir()
	state := t.TempDir()
	r10k := writeFakeR10k(t, basedir, []string{"production"}, 0)
	fakePublisher(t, state)

	results, err := Run(context.Background(), Config{
		R10kPath: r10k,
		BaseDir:  basedir,
		StateDir: state,
		Modules:  true,
	}, []string{"production"}, false, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Env != "production" {
		t.Errorf("env = %q, want production", r.Env)
	}
	if r.Commit != "commit-production" {
		t.Errorf("commit = %q, want commit-production", r.Commit)
	}

	// The reported code_id must equal an independent seal of the staged tree,
	// or the deploy is reporting an id nothing can be served for.
	want, err := seal.CodeID(filepath.Join(basedir, "production"))
	if err != nil {
		t.Fatal(err)
	}
	if r.CodeID != want {
		t.Errorf("code_id = %s, want %s", r.CodeID, want)
	}
}

func TestRunAllListsStagedEnvironments(t *testing.T) {
	basedir := t.TempDir()
	state := t.TempDir()
	r10k := writeFakeR10k(t, basedir, []string{"production", "testing"}, 0)
	fakePublisher(t, state)

	results, err := Run(context.Background(), Config{
		R10kPath: r10k,
		BaseDir:  basedir,
		StateDir: state,
	}, nil, true, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.Env] = true
	}
	if len(results) != 2 || !got["production"] || !got["testing"] {
		t.Errorf("results = %+v, want production and testing", results)
	}
}

func TestRunSurfacesR10kFailure(t *testing.T) {
	basedir := t.TempDir()
	r10k := writeFakeR10k(t, basedir, []string{"production"}, 3)

	if _, err := Run(context.Background(), Config{
		R10kPath: r10k,
		BaseDir:  basedir,
		StateDir: t.TempDir(),
	}, []string{"production"}, false, false); err == nil {
		t.Fatal("expected an error when r10k exits non-zero")
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	basedir := t.TempDir()
	r10k := writeFakeR10k(t, basedir, []string{"production"}, 0)
	base := Config{R10kPath: r10k, BaseDir: basedir, StateDir: t.TempDir()}

	cases := map[string]struct {
		cfg  Config
		envs []string
		all  bool
	}{
		"no environment and not --all": {base, nil, false},
		"both environments and --all":  {base, []string{"production"}, true},
		"no basedir directory":         {Config{R10kPath: r10k}, []string{"production"}, false},
		"invalid environment name":     {base, []string{"Bad Env!"}, false},
	}
	for name, c := range cases {
		if _, err := Run(context.Background(), c.cfg, c.envs, c.all, false); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestSignalPublisherRejectsMissingAndStalePidfiles(t *testing.T) {
	t.Run("no pidfile", func(t *testing.T) {
		if err := SignalPublisher(t.TempDir()); err == nil {
			t.Error("expected error when no pidfile exists")
		}
	})

	t.Run("malformed pidfile", func(t *testing.T) {
		state := t.TempDir()
		if err := os.WriteFile(publish.PidFilePath(state), []byte("not-a-pid"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SignalPublisher(state); err == nil {
			t.Error("expected error for a malformed pidfile")
		}
	})
}

func TestResolveR10kExplicitMissing(t *testing.T) {
	if _, err := resolveR10k(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for an explicit r10k path that does not exist")
	}
}

// writeSerializingR10k writes a fake r10k that fails if a second copy runs while
// it is still working, by guarding a marker file. Two deploys that overlap on
// basedir both exec this; if the lock does its job, they never overlap.
func writeSerializingR10k(t *testing.T, basedir, markerDir string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
marker=%q/deploying
if [ -e "$marker" ]; then echo OVERLAP >&2; exit 1; fi
: > "$marker"
mkdir -p %q
printf 'node default { }\n' > %q
printf '{"name":"production","signature":"x"}' > %q
sleep 0.4
rm -f "$marker"
`,
		markerDir,
		filepath.Join(basedir, "production", "manifests"),
		filepath.Join(basedir, "production", "manifests", "site.pp"),
		filepath.Join(basedir, "production", ".r10k-deploy.json"))

	path := filepath.Join(t.TempDir(), "r10k")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	return path
}

// TestConcurrentDeploysSerialize is the reason for the basedir lock: two
// deploys sharing a basedir directory must not run r10k at the same time, or
// they corrupt each other's trees. Both share one state directory, hence one
// lock; the fake r10k fails if it ever sees an overlap.
func TestConcurrentDeploysSerialize(t *testing.T) {
	basedir := t.TempDir()
	state := t.TempDir()
	marker := t.TempDir()
	r10k := writeSerializingR10k(t, basedir, marker)
	fakePublisher(t, state)

	cfg := Config{
		R10kPath:    r10k,
		BaseDir:     basedir,
		StateDir:    state,
		LockTimeout: 30 * time.Second,
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Run(context.Background(), cfg, []string{"production"}, false, false)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("deploy %d failed (overlap not prevented?): %v", i, err)
		}
	}
}

// fakePublisher writes a pidfile naming a real, live process so SignalPublisher
// finds something to signal.
//
// A deploy that cannot reach a publisher is a hard error now, so a test
// exercising staging and sealing still has to satisfy that — and satisfying it
// with a real process rather than a stub means the liveness probe and the SIGHUP
// are genuinely exercised instead of skipped.
func fakePublisher(t *testing.T, stateDir string) {
	t.Helper()
	// A sleep that ignores nothing: SIGHUP terminates it, which is fine. The
	// deploy only needs the signal to be delivered.
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a stand-in publisher: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publish.PidFilePath(stateDir),
		[]byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The second half of #56: a deploy that updated the basedir but could not tell
// anything to seal it has not deployed. It used to print the new code_id beside
// the word "deployed" and exit 0, so CI recorded a green deploy that no compiler
// would ever see.
func TestRunFailsWhenThePublisherCannotBeSignalled(t *testing.T) {
	basedir := t.TempDir()
	r10k := writeFakeR10k(t, basedir, []string{"production"}, 0)

	// No fakePublisher: nothing is listening for the SIGHUP.
	results, err := Run(context.Background(), Config{
		R10kPath: r10k,
		BaseDir:  basedir,
		StateDir: t.TempDir(),
	}, []string{"production"}, false, false)

	if err == nil {
		t.Fatal("a deploy nothing will serve reported success")
	}
	if !strings.Contains(err.Error(), "nothing is serving it") {
		t.Errorf("error = %v, want it to say nothing is serving the deploy", err)
	}
	// The staging half did happen, and the results still describe it — the caller
	// needs both facts to know what state the node is in.
	if len(results) != 1 || results[0].CodeID == "" {
		t.Errorf("results = %+v, want the sealed environment reported alongside the error", results)
	}
}

// writeHangingR10k writes a stand-in r10k that never finishes on its own. It
// backgrounds a sleep and records that child's pid, so a test can check that
// terminating the deploy took the whole process group with it and not just the
// shell codavox spawned.
func writeHangingR10k(t *testing.T, childPidFile string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
sleep 60 &
echo $! > %q
wait
`, childPidFile)
	path := filepath.Join(t.TempDir(), "r10k")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	return path
}

// childPid waits for the fake r10k to record its child's pid and returns it.
func childPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidFile) // #nosec G304 -- test temp file
		if err == nil && strings.TrimSpace(string(b)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("parsing child pid: %v", err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake r10k never recorded its child pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone polls until pid no longer exists, or fails the test.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Signal 0 probes liveness; ESRCH means the process is gone. A
		// reparented child is not ours to reap, so this is the observable.
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("r10k's child %d survived the deploy being terminated", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A wedged r10k — a git fetch to an unreachable remote blocks indefinitely —
// used to hold the basedir lock forever and queue every later deploy behind it
// until someone found the process by hand. The timeout ends it, and ending it
// means the whole process group: r10k forks git, git forks ssh, and a survivor
// would keep writing the basedir under the next deploy.
func TestRunTimesOutAndKillsR10kProcessGroup(t *testing.T) {
	basedir := t.TempDir()
	state := t.TempDir()
	childPidFile := filepath.Join(t.TempDir(), "child.pid")
	r10k := writeHangingR10k(t, childPidFile)
	fakePublisher(t, state)

	cfg := Config{
		R10kPath:    r10k,
		BaseDir:     basedir,
		StateDir:    state,
		R10kTimeout: time.Second,
	}

	start := time.Now()
	_, err := Run(context.Background(), cfg, []string{"production"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("Run error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %s to give up on a hung r10k", elapsed)
	}
	waitGone(t, childPid(t, childPidFile))

	// The lock must be free again: the next deploy — the one the operator runs
	// to retry — must not wait behind the corpse of this one.
	release, err := lockDeploy(state, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("basedir lock still held after a timed-out deploy: %v", err)
	}
	release()
}

// Ctrl-C at the terminal reaches codavox, and r10k lives in its own process
// group so it no longer sees the signal itself. The context is how the
// interrupt is forwarded, and it must end r10k the same way the timeout does.
func TestRunCancelTerminatesR10k(t *testing.T) {
	basedir := t.TempDir()
	state := t.TempDir()
	childPidFile := filepath.Join(t.TempDir(), "child.pid")
	r10k := writeHangingR10k(t, childPidFile)
	fakePublisher(t, state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid := make(chan int, 1)
	go func() {
		// Interrupt only once r10k is demonstrably running, or the cancel
		// lands before Start and proves nothing about the process group.
		pid <- childPid(t, childPidFile)
		cancel()
	}()
	_, err := Run(ctx, Config{R10kPath: r10k, BaseDir: basedir, StateDir: state},
		[]string{"production"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Run error = %v, want interrupted", err)
	}
	waitGone(t, <-pid)
}
