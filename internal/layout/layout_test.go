package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEnvironment(t *testing.T) {
	valid := []string{"production", "dev", "my_env", "env123", "UPPER"}
	for _, env := range valid {
		if err := ValidateEnvironment(env); err != nil {
			t.Errorf("ValidateEnvironment(%q) = %v, want nil", env, err)
		}
	}

	// r10k sanitizes \W to _ when naming environments, so these should never
	// reach us in practice — but OpenVox Server rejects them, so we must too.
	invalid := []string{"", "my-env", "my.env", "my/env", "my env", "env!"}
	for _, env := range invalid {
		if err := ValidateEnvironment(env); err == nil {
			t.Errorf("ValidateEnvironment(%q) = nil, want error", env)
		}
	}
}

func TestValidateCodeID(t *testing.T) {
	valid := []string{
		"abc123",
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		"production_deadbeef",
		"with-dash",
		"with:colon",
		"with;semicolon",
	}
	for _, id := range valid {
		if err := ValidateCodeID(id); err != nil {
			t.Errorf("ValidateCodeID(%q) = %v, want nil", id, err)
		}
	}

	// The base64 cases are the trap: a digest encoded as base64 looks fine
	// until OpenVox Server throws IllegalStateException at runtime.
	invalid := []string{"", "has/slash", "has.dot", "has+plus", "has=equals", "has space"}
	for _, id := range invalid {
		if err := ValidateCodeID(id); err == nil {
			t.Errorf("ValidateCodeID(%q) = nil, want error", id)
		}
	}
}

// deploy builds a version directory and points the environment link at it,
// the way the agent does.
func deploy(t *testing.T, l Layout, env, codeID string) {
	t.Helper()
	dir := l.VersionDir(env, codeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	link := l.EnvironmentLink(env)
	_ = os.Remove(link)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
}

func testLayout(t *testing.T) Layout {
	t.Helper()
	base := t.TempDir()
	return Layout{
		Root:            filepath.Join(base, "codavox"),
		EnvironmentPath: filepath.Join(base, "environments"),
	}
}

func TestCurrentCodeID(t *testing.T) {
	l := testLayout(t)
	deploy(t, l, "production", "deadbeef123")

	got, err := l.CurrentCodeID("production")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "deadbeef123" {
		t.Errorf("got %q, want %q", got, "deadbeef123")
	}
}

// The whole point of deriving the id from the link: one rename changes what
// OpenVox Server serves and what code-id reports at the same instant, so there
// is no window where a catalog is compiled from one version and stamped with
// another.
func TestCurrentCodeIDFollowsTheLinkAtomically(t *testing.T) {
	l := testLayout(t)
	deploy(t, l, "production", "aaa111")

	if got, _ := l.CurrentCodeID("production"); got != "aaa111" {
		t.Fatalf("got %q, want aaa111", got)
	}

	// Swap the way the agent does: new link at a temp name, then rename over.
	newDir := l.VersionDir("production", "bbb222")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := l.EnvironmentLink("production") + ".tmp"
	if err := os.Symlink(newDir, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, l.EnvironmentLink("production")); err != nil {
		t.Fatal(err)
	}

	got, err := l.CurrentCodeID("production")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bbb222" {
		t.Errorf("after swap got %q, want bbb222", got)
	}
}

func TestCurrentCodeIDErrors(t *testing.T) {
	l := testLayout(t)

	// A missing environment must fail loudly. The shell baseline this replaces
	// fell back to `date +%s`, producing a code_id that changed on every call
	// and silently destroyed content addressing.
	t.Run("missing environment is an error, never a fallback", func(t *testing.T) {
		if _, err := l.CurrentCodeID("nonexistent"); err == nil {
			t.Fatal("expected an error for a missing environment link")
		}
	})

	t.Run("invalid environment is rejected before touching disk", func(t *testing.T) {
		_, err := l.CurrentCodeID("../etc/passwd")
		if err == nil {
			t.Fatal("expected an error for an invalid environment")
		}
		if !strings.Contains(err.Error(), "invalid environment") {
			t.Errorf("got %v, want an invalid-environment error", err)
		}
	})

	// A real directory instead of a link means something other than codavox is
	// managing this environment. Reporting an id for it would be a lie.
	t.Run("a real directory is not a deployment", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(l.EnvironmentPath, "manual"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CurrentCodeID("manual"); err == nil {
			t.Fatal("expected an error when the environment is a real directory")
		}
	})

	t.Run("link to an unrelated target", func(t *testing.T) {
		if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(l.EnvironmentPath, "stray")
		if err := os.Symlink("/tmp/somewhere-else", link); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CurrentCodeID("stray"); err == nil {
			t.Fatal("expected an error for a link outside the version layout")
		}
	})

	// Guards against a link like production_ with nothing after the prefix.
	t.Run("empty code_id in the link", func(t *testing.T) {
		if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(l.EnvironmentPath, "empty")
		if err := os.Symlink(filepath.Join(l.Root, "versions", "empty_"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CurrentCodeID("empty"); err == nil {
			t.Fatal("expected an error for an empty code_id")
		}
	})

	// An environment must not be able to claim another's version directory.
	t.Run("link to a different environment's version", func(t *testing.T) {
		deploy(t, l, "testing", "ccc333")
		if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(l.EnvironmentPath, "other")
		_ = os.Remove(link)
		if err := os.Symlink(l.VersionDir("testing", "ccc333"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := l.CurrentCodeID("other"); err == nil {
			t.Fatal("an environment link resolved to another environment's version")
		}
	})
}

func TestNewReadsEnvironment(t *testing.T) {
	t.Setenv(RootEnvVar, "/custom/root")
	t.Setenv(EnvironmentPathEnvVar, "/custom/environments")

	l := New()
	if l.Root != "/custom/root" {
		t.Errorf("Root = %q", l.Root)
	}
	if l.EnvironmentPath != "/custom/environments" {
		t.Errorf("EnvironmentPath = %q", l.EnvironmentPath)
	}
}

func TestVersionDirName(t *testing.T) {
	if got := VersionDirName("production", "abc"); got != "production_abc" {
		t.Errorf("VersionDirName = %q, want production_abc", got)
	}
}

// BenchmarkCurrentCodeID guards the property that makes this viable: it runs
// once per static catalog compile, uncached, so it must stay a single syscall.
func BenchmarkCurrentCodeID(b *testing.B) {
	base := b.TempDir()
	l := Layout{
		Root:            filepath.Join(base, "codavox"),
		EnvironmentPath: filepath.Join(base, "environments"),
	}
	dir := l.VersionDir("production", "deadbeef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.Symlink(dir, l.EnvironmentLink("production")); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := l.CurrentCodeID("production"); err != nil {
			b.Fatal(err)
		}
	}
}
