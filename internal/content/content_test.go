package content

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/miharp/codavox/internal/layout"
)

// setup builds a version tree for (env, codeID) containing files.
func setup(t *testing.T, env, codeID string, files map[string]string) layout.Layout {
	t.Helper()
	l := layout.Layout{Root: t.TempDir()}
	dir := l.VersionDir(env, codeID)
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestCopy(t *testing.T) {
	l := setup(t, "production", "abc123", map[string]string{
		"manifests/site.pp":  "node default {}\n",
		"modules/f/file.txt": "hello\n",
	})

	t.Run("serves file content", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Copy(&buf, l, "production", "abc123", "manifests/site.pp"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := buf.String(); got != "node default {}\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("tolerates a leading slash", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Copy(&buf, l, "production", "abc123", "/modules/f/file.txt"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := buf.String(); got != "hello\n" {
			t.Errorf("got %q", got)
		}
	})

	// The defining behaviour. The shell baseline fell back to reading the
	// current filesystem when its lookup missed, exiting 0 while serving
	// content from the wrong version — a silent correctness failure that
	// defeats the purpose of static catalogs.
	t.Run("undeployed version fails, never falls back", func(t *testing.T) {
		var buf bytes.Buffer
		err := Copy(&buf, l, "production", "notdeployed", "manifests/site.pp")
		if err == nil {
			t.Fatal("expected error for undeployed version, got nil")
		}
		if !errors.Is(err, ErrVersionNotDeployed) {
			t.Errorf("got %v, want ErrVersionNotDeployed", err)
		}
		if buf.Len() != 0 {
			t.Errorf("wrote %q on failure, want nothing", buf.String())
		}
	})

	t.Run("missing file within a deployed version is an error", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Copy(&buf, l, "production", "abc123", "nope.txt"); err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})

	t.Run("directory is not servable", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Copy(&buf, l, "production", "abc123", "manifests"); err == nil {
			t.Fatal("expected error for directory, got nil")
		}
	})

	t.Run("rejects invalid environment and code_id", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Copy(&buf, l, "bad/env", "abc123", "x"); err == nil {
			t.Error("expected error for invalid environment")
		}
		if err := Copy(&buf, l, "production", "bad/id", "x"); err == nil {
			t.Error("expected error for invalid code_id")
		}
	})
}

// TestCopyTraversal covers the security boundary. puppetserver passes the
// requested path through from the agent, and the version tree is unpacked from
// a downloaded artifact, so neither the path nor the tree's symlinks are
// trustworthy.
func TestCopyTraversal(t *testing.T) {
	l := setup(t, "production", "abc123", map[string]string{
		"ok.txt": "fine\n",
	})

	secret := filepath.Join(l.Root, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("dot-dot escape is blocked", func(t *testing.T) {
		for _, p := range []string{
			"../secret.txt",
			"../../secret.txt",
			"subdir/../../secret.txt",
			"/../secret.txt",
		} {
			var buf bytes.Buffer
			if err := Copy(&buf, l, "production", "abc123", p); err == nil {
				t.Errorf("path %q escaped the version root", p)
			}
			if bytes.Contains(buf.Bytes(), []byte("SECRET")) {
				t.Errorf("path %q leaked secret content", p)
			}
		}
	})

	t.Run("symlink escape is blocked", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink semantics differ on windows")
		}
		link := filepath.Join(l.VersionDir("production", "abc123"), "escape")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := Copy(&buf, l, "production", "abc123", "escape"); err == nil {
			t.Error("symlink escaped the version root")
		}
		if bytes.Contains(buf.Bytes(), []byte("SECRET")) {
			t.Error("symlink leaked secret content")
		}
	})
}
