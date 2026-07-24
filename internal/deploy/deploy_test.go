package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/miharp/codavox/internal/publish"
	"github.com/miharp/codavox/internal/seal"
)

// writeFakeR10k writes a stand-in r10k that stages the given environments and
// exits with exitCode. It ignores its arguments and materializes a fixed tree,
// which is all Run needs: Run's job is to invoke r10k and seal whatever it
// produced, not to drive r10k's resolution.
func writeFakeR10k(t *testing.T, staging string, envs []string, exitCode int) string {
	t.Helper()
	script := "#!/bin/sh\n"
	for _, env := range envs {
		dir := filepath.Join(staging, env)
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
	staging := t.TempDir()
	r10k := writeFakeR10k(t, staging, []string{"production"}, 0)

	results, err := Run(Config{
		R10kPath:   r10k,
		StagingDir: staging,
		StateDir:   t.TempDir(),
		Modules:    true,
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
	want, err := seal.CodeID(filepath.Join(staging, "production"))
	if err != nil {
		t.Fatal(err)
	}
	if r.CodeID != want {
		t.Errorf("code_id = %s, want %s", r.CodeID, want)
	}
}

func TestRunAllListsStagedEnvironments(t *testing.T) {
	staging := t.TempDir()
	r10k := writeFakeR10k(t, staging, []string{"production", "testing"}, 0)

	results, err := Run(Config{
		R10kPath:   r10k,
		StagingDir: staging,
		StateDir:   t.TempDir(),
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
	staging := t.TempDir()
	r10k := writeFakeR10k(t, staging, []string{"production"}, 3)

	if _, err := Run(Config{
		R10kPath:   r10k,
		StagingDir: staging,
		StateDir:   t.TempDir(),
	}, []string{"production"}, false, false); err == nil {
		t.Fatal("expected an error when r10k exits non-zero")
	}
}

func TestRunRejectsBadArgs(t *testing.T) {
	staging := t.TempDir()
	r10k := writeFakeR10k(t, staging, []string{"production"}, 0)
	base := Config{R10kPath: r10k, StagingDir: staging, StateDir: t.TempDir()}

	cases := map[string]struct {
		cfg  Config
		envs []string
		all  bool
	}{
		"no environment and not --all": {base, nil, false},
		"both environments and --all":  {base, []string{"production"}, true},
		"no staging directory":         {Config{R10kPath: r10k}, []string{"production"}, false},
		"invalid environment name":     {base, []string{"Bad Env!"}, false},
	}
	for name, c := range cases {
		if _, err := Run(c.cfg, c.envs, c.all, false); err == nil {
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
