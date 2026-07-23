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

	// r10k sanitises \W to _ when naming environments, so these should never
	// reach us in practice — but puppetserver rejects them, so we must too.
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
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", // hex digest
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

	// Characters puppetserver's CodeId schema rejects. The base64 cases are
	// the trap: a digest encoded as base64 looks fine until puppetserver
	// throws IllegalStateException at runtime.
	invalid := []string{
		"",
		"has/slash",
		"has.dot",
		"has+plus",   // base64
		"has=equals", // base64 padding
		"has space",
	}
	for _, id := range invalid {
		if err := ValidateCodeID(id); err == nil {
			t.Errorf("ValidateCodeID(%q) = nil, want error", id)
		}
	}
}

func TestCurrentCodeID(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(env, contents string) {
		t.Helper()
		if err := os.WriteFile(l.StateFile(env), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("reads and trims", func(t *testing.T) {
		write("production", "deadbeef123\n")
		got, err := l.CurrentCodeID("production")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "deadbeef123" {
			t.Errorf("got %q, want %q", got, "deadbeef123")
		}
	})

	// The critical property: a missing state file must fail loudly. The shell
	// baseline this replaces fell back to `date +%s`, producing a code_id that
	// changed on every call and silently destroyed content addressing.
	t.Run("missing state file is an error, never a fallback", func(t *testing.T) {
		if _, err := l.CurrentCodeID("nonexistent"); err == nil {
			t.Fatal("expected error for missing state file, got nil")
		}
	})

	t.Run("empty state file is an error", func(t *testing.T) {
		write("empty", "   \n")
		if _, err := l.CurrentCodeID("empty"); err == nil {
			t.Fatal("expected error for empty state file, got nil")
		}
	})

	t.Run("malformed code_id is an error", func(t *testing.T) {
		write("bad", "not/a/valid/id\n")
		if _, err := l.CurrentCodeID("bad"); err == nil {
			t.Fatal("expected error for malformed code_id, got nil")
		}
	})

	t.Run("invalid environment is rejected before touching disk", func(t *testing.T) {
		_, err := l.CurrentCodeID("../etc/passwd")
		if err == nil {
			t.Fatal("expected error for invalid environment, got nil")
		}
		if !strings.Contains(err.Error(), "invalid environment") {
			t.Errorf("got %v, want an invalid-environment error", err)
		}
	})
}

// BenchmarkCurrentCodeID guards the property that makes this viable: it runs
// once per static catalog compile, uncached, so it must stay a single read.
func BenchmarkCurrentCodeID(b *testing.B) {
	root := b.TempDir()
	l := Layout{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(l.StateFile("production"), []byte("deadbeef\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := l.CurrentCodeID("production"); err != nil {
			b.Fatal(err)
		}
	}
}
