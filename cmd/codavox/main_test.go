package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/publish"
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

func TestPrintFleet(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	peers := []publish.Peer{
		{
			Certname: "compiler01.example.com",
			LastPoll: now.Add(-12 * time.Second),
			// Sorted by environment, so two runs of the command are comparable.
			Serving: map[string]string{"testing": "def456", "production": "abc123"},
		},
		{
			Certname: "compiler02.example.com",
			LastPoll: now.Add(-9 * time.Minute),
			Serving:  map[string]string{"production": "stale9"},
		},
	}

	var buf bytes.Buffer
	if err := printFleet(&buf, peers, now); err != nil {
		t.Fatal(err)
	}
	// Compare fields, not column widths: the padding is tabwriter's business
	// and would make this test fail on a cosmetic change.
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		rows = append(rows, strings.Fields(line))
	}

	want := [][]string{
		{"COMPILER", "ENVIRONMENT", "CODE_ID", "LAST", "POLL"},
		{"compiler01.example.com", "production", "abc123", "12s", "ago"},
		{"compiler01.example.com", "testing", "def456", "12s", "ago"},
		{"compiler02.example.com", "production", "stale9", "9m0s", "ago"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%s", len(rows), len(want), buf.String())
	}
	for i := range want {
		if strings.Join(rows[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

// A compiler that polls but reports nothing still gets a row. Dropping it would
// hide a node running an agent too old to report — the one case where the
// operator most needs to know the view is incomplete.
func TestPrintFleetShowsCompilersThatReportNothing(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := printFleet(&buf, []publish.Peer{
		{Certname: "compiler03.example.com", LastPoll: now.Add(-time.Second)},
	}, now); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "compiler03.example.com") ||
		!strings.Contains(got, "(not reported)") {
		t.Errorf("a reporting-less compiler was not shown:\n%s", got)
	}
}

func TestListenPort(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{":8150", "8150"},
		{"0.0.0.0:9000", "9000"},
		{"[::]:9001", "9001"},
		// Unparseable, so the command tries the port everyone uses rather than
		// refusing to run.
		{"", defaultPublishPort},
		{"nonsense", defaultPublishPort},
	} {
		if got := listenPort(tc.listen); got != tc.want {
			t.Errorf("listenPort(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}
