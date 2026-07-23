package seal

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tree builds a directory from a path->content map. A content value prefixed
// with "link:" creates a symlink to the remainder.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(body, "link:"); ok {
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustCodeID(t *testing.T, root string) string {
	t.Helper()
	id, err := CodeID(root)
	if err != nil {
		t.Fatalf("CodeID(%s): %v", root, err)
	}
	return id
}

// The phase gate: identical content must seal to an identical id regardless of
// where it lives or when it was written.
func TestCodeIDIsReproducible(t *testing.T) {
	files := map[string]string{
		"manifests/site.pp":           "node default { }\n",
		"modules/apache/init.pp":      "class apache { }\n",
		"modules/apache/files/a.conf": "Listen 80\n",
		"Puppetfile":                  "mod 'apache'\n",
	}

	a := mustCodeID(t, tree(t, files))
	b := mustCodeID(t, tree(t, files))

	if a != b {
		t.Errorf("identical trees sealed differently:\n  %s\n  %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("code_id is %d chars, want 64 (hex sha256)", len(a))
	}
}

// The failure this guards against is subtle and expensive: r10k redeploying
// unchanged code would produce a new code_id on every run, so every compiler
// would churn through a full fetch and swap for no reason.
func TestModificationTimeIsIgnored(t *testing.T) {
	root := tree(t, map[string]string{"manifests/site.pp": "node default { }\n"})
	before := mustCodeID(t, root)

	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "manifests/site.pp"), old, old); err != nil {
		t.Fatal(err)
	}

	if after := mustCodeID(t, root); after != before {
		t.Errorf("mtime changed the code_id:\n  before %s\n  after  %s", before, after)
	}
}

// Non-executable permission bits vary with the umask of whoever ran r10k.
func TestNonExecutablePermissionBitsAreIgnored(t *testing.T) {
	root := tree(t, map[string]string{"data.txt": "x\n"})
	before := mustCodeID(t, root)

	if err := os.Chmod(filepath.Join(root, "data.txt"), 0o600); err != nil {
		t.Fatal(err)
	}

	if after := mustCodeID(t, root); after != before {
		t.Errorf("umask-dependent mode changed the code_id:\n  before %s\n  after  %s", before, after)
	}
}

// The executable bit is real content: a script that loses +x behaves
// differently, so it must change the id.
func TestExecutableBitIsSignificant(t *testing.T) {
	root := tree(t, map[string]string{"script.sh": "#!/bin/sh\n"})
	before := mustCodeID(t, root)

	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	if after := mustCodeID(t, root); after == before {
		t.Error("executable bit did not change the code_id")
	}
}

func TestContentChangesTheCodeID(t *testing.T) {
	a := mustCodeID(t, tree(t, map[string]string{"a.pp": "one\n"}))
	b := mustCodeID(t, tree(t, map[string]string{"a.pp": "two\n"}))
	if a == b {
		t.Error("different content sealed to the same code_id")
	}
}

// A file moved between directories is a different tree even with identical
// bytes, so path must be part of the hash.
func TestPathIsSignificant(t *testing.T) {
	a := mustCodeID(t, tree(t, map[string]string{"one/a.pp": "x\n"}))
	b := mustCodeID(t, tree(t, map[string]string{"two/a.pp": "x\n"}))
	if a == b {
		t.Error("relocating a file did not change the code_id")
	}
}

func TestExclusions(t *testing.T) {
	base := map[string]string{"manifests/site.pp": "node default { }\n"}
	want := mustCodeID(t, tree(t, base))

	t.Run("git metadata is excluded", func(t *testing.T) {
		withGit := map[string]string{
			"manifests/site.pp": "node default { }\n",
			".git/HEAD":         "ref: refs/heads/production\n",
			".git/config":       "[core]\n",
		}
		if got := mustCodeID(t, tree(t, withGit)); got != want {
			t.Errorf(".git affected the code_id:\n  want %s\n  got  %s", want, got)
		}
	})

	// This one matters most: .r10k-deploy.json embeds deploy timestamps, so
	// including it would give every redeploy of unchanged code a fresh id.
	t.Run("r10k deploy metadata is excluded", func(t *testing.T) {
		withDeploy := map[string]string{
			"manifests/site.pp": "node default { }\n",
			".r10k-deploy.json": `{"started_at":"2026-07-23T10:00:00Z"}`,
		}
		if got := mustCodeID(t, tree(t, withDeploy)); got != want {
			t.Errorf(".r10k-deploy.json affected the code_id:\n  want %s\n  got  %s", want, got)
		}
	})

	t.Run("differing excluded content still seals identically", func(t *testing.T) {
		a := mustCodeID(t, tree(t, map[string]string{
			"manifests/site.pp": "node default { }\n",
			".r10k-deploy.json": `{"started_at":"2026-01-01T00:00:00Z"}`,
		}))
		b := mustCodeID(t, tree(t, map[string]string{
			"manifests/site.pp": "node default { }\n",
			".r10k-deploy.json": `{"started_at":"2026-12-31T23:59:59Z"}`,
		}))
		if a != b {
			t.Error("differing excluded files produced different code_ids")
		}
	})
}

func TestSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	t.Run("target is part of the hash", func(t *testing.T) {
		a := mustCodeID(t, tree(t, map[string]string{"l": "link:one"}))
		b := mustCodeID(t, tree(t, map[string]string{"l": "link:two"}))
		if a == b {
			t.Error("changing a symlink target did not change the code_id")
		}
	})

	// Following links would double-count in-tree files and could escape the
	// tree entirely, making the id depend on files outside it.
	t.Run("a link is not equivalent to its target's content", func(t *testing.T) {
		linked := mustCodeID(t, tree(t, map[string]string{
			"real.txt": "hello\n",
			"l":        "link:real.txt",
		}))
		regular := mustCodeID(t, tree(t, map[string]string{
			"real.txt": "hello\n",
			"l":        "hello\n",
		}))
		if linked == regular {
			t.Error("symlink and regular file with the same content sealed identically")
		}
	})

	t.Run("a dangling link is sealed, not an error", func(t *testing.T) {
		root := tree(t, map[string]string{"l": "link:/nonexistent/path"})
		if _, err := CodeID(root); err != nil {
			t.Errorf("dangling symlink should seal cleanly: %v", err)
		}
	})
}

// Empty directories are invisible, matching git, so the id does not depend on
// whether a transport preserves them.
func TestEmptyDirectoriesAreIgnored(t *testing.T) {
	root := tree(t, map[string]string{"a.pp": "x\n"})
	before := mustCodeID(t, root)

	if err := os.MkdirAll(filepath.Join(root, "empty/nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	if after := mustCodeID(t, root); after != before {
		t.Errorf("an empty directory changed the code_id:\n  before %s\n  after  %s", before, after)
	}
}

func TestManifestIsInspectable(t *testing.T) {
	root := tree(t, map[string]string{
		"b.pp": "second\n",
		"a.pp": "first\n",
	})

	m, err := ManifestString(root)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(m, "\n")

	if lines[0] != manifestVersion {
		t.Errorf("manifest header = %q, want %q", lines[0], manifestVersion)
	}
	// Sorted output is what makes two manifests diffable.
	if !strings.HasSuffix(lines[1], " a.pp") || !strings.HasSuffix(lines[2], " b.pp") {
		t.Errorf("entries are not sorted by path:\n%s", m)
	}
}

func TestCodeIDErrors(t *testing.T) {
	t.Run("missing root", func(t *testing.T) {
		if _, err := CodeID(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("expected error for missing root")
		}
	})

	t.Run("root is a file", func(t *testing.T) {
		root := t.TempDir()
		f := filepath.Join(root, "f")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := CodeID(f); err == nil {
			t.Error("expected error when root is not a directory")
		}
	})
}

func TestExcludedNames(t *testing.T) {
	got := ExcludedNames()
	want := []string{".git", ".r10k-deploy.json"}
	if len(got) != len(want) {
		t.Fatalf("ExcludedNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ExcludedNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !IsExcluded(".git") || IsExcluded("manifests") {
		t.Error("IsExcluded disagrees with ExcludedNames")
	}
}
