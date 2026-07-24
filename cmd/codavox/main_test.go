package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The argv[0] names are part of the deployed interface: puppetserver's
// versioned-code.conf points directly at these paths, so renaming one silently
// breaks every compiler that has already been configured.
func TestArgv0Commands(t *testing.T) {
	want := map[string]string{
		"codavox-code-id":      "code-id",
		"codavox-code-content": "code-content",
	}

	for name, cmd := range want {
		got, ok := argv0Commands[name]
		if !ok {
			t.Errorf("argv0Commands missing %q", name)
			continue
		}
		if got != cmd {
			t.Errorf("argv0Commands[%q] = %q, want %q", name, got, cmd)
		}
	}

	if len(argv0Commands) != len(want) {
		t.Errorf("argv0Commands has %d entries, want %d", len(argv0Commands), len(want))
	}
}

func TestRunRejectsUnknownSubcommand(t *testing.T) {
	if err := run("bogus", nil); err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
}

func TestRunRejectsWrongArgCount(t *testing.T) {
	if err := run("code-id", nil); err == nil {
		t.Error("code-id with no args should error")
	}
	if err := run("code-id", []string{"a", "b"}); err == nil {
		t.Error("code-id with two args should error")
	}
	if err := run("code-content", []string{"env", "id"}); err == nil {
		t.Error("code-content with two args should error")
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

// The provenance subcommand reads the publisher's local log; it does no network
// I/O, so a written log file is all the test needs.
func TestProvenanceSubcommandReadsLocalLog(t *testing.T) {
	state := t.TempDir()
	rec := `{"code_id":"a3f1c9e4b2d8","environment":"production","commit":"cafef00d",` +
		`"deployed_at":"2026-07-24 12:00:00 -0400","sealed_at":"2026-07-24T16:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(state, provenanceFile), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return run("provenance", []string{"production", "a3f1c9e4b2d8", "--state", state})
	})
	if err != nil {
		t.Fatalf("provenance query: %v", err)
	}
	if !strings.Contains(out, "cafef00d") {
		t.Errorf("output %q does not name the commit", out)
	}

	out, err = captureStdout(t, func() error {
		return run("provenance", []string{"production", "a3f1c9e4b2d8", "--state", state, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if len(recs) != 1 || recs[0]["commit"] != "cafef00d" {
		t.Errorf("--json records = %+v, want one for cafef00d", recs)
	}

	// An id with no record is an honest empty answer, not an error, and nothing
	// is written to stdout that a script could mistake for a result.
	out, err = captureStdout(t, func() error {
		return run("provenance", []string{"production", "beefbeefbeef", "--state", state})
	})
	if err != nil {
		t.Errorf("querying an unrecorded id should not error, got %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("unrecorded id printed to stdout: %q", out)
	}
}

// TestConfigDrivesCommandState checks the config-to-command wiring end to end
// through provenance, which reads its state directory and does no network I/O:
// a config file supplies the state directory, and a --state flag overrides it.
func TestConfigDrivesCommandState(t *testing.T) {
	configured := t.TempDir()
	rec := `{"code_id":"a3f1c9e4b2d8","environment":"production","commit":"cafef00d","sealed_at":"2026-07-24T16:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(configured, provenanceFile), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("state: "+configured+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The config's state directory is used when no --state flag is given.
	out, err := captureStdout(t, func() error {
		return run("provenance", []string{"--config", cfgPath, "production", "a3f1c9e4b2d8"})
	})
	if err != nil {
		t.Fatalf("provenance with config: %v", err)
	}
	if !strings.Contains(out, "cafef00d") {
		t.Errorf("config-supplied state was not used; output = %q", out)
	}

	// A --state flag overrides the config: an empty dir yields no record.
	out, err = captureStdout(t, func() error {
		return run("provenance", []string{"--config", cfgPath, "--state", t.TempDir(), "production", "a3f1c9e4b2d8"})
	})
	if err != nil {
		t.Fatalf("provenance with overriding --state: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--state did not override config; output = %q", out)
	}
}

func TestProvenanceSubcommandRejectsBadArgs(t *testing.T) {
	cases := map[string][]string{
		"one argument":        {"production"},
		"unknown flag":        {"production", "id", "--bogus"},
		"invalid environment": {"Bad Env!", "abc"},
		"invalid code_id":     {"production", "bad/id"},
	}
	for name, args := range cases {
		if err := run("provenance", args); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
