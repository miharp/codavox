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

	// A code_id is a content hash, so the commit is what an operator recognizes.
	commits := map[string]string{
		"production\x00abc123": "a3f1c9e4b2d8f70e",
	}

	var buf bytes.Buffer
	if err := printFleet(&buf, peers, commits, now); err != nil {
		t.Fatal(err)
	}
	// Compare fields, not column widths: the padding is tabwriter's business
	// and would make this test fail on a cosmetic change.
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		rows = append(rows, strings.Fields(line))
	}

	want := [][]string{
		{"COMPILER", "ENVIRONMENT", "CODE_ID", "COMMIT", "LAST", "POLL"},
		// The commit is shortened for the table; --json carries it whole.
		{"compiler01.example.com", "production", "abc123", "a3f1c9e4b2d8", "12s", "ago"},
		// No provenance recorded reads as "-", never as another version's.
		{"compiler01.example.com", "testing", "def456", "-", "12s", "ago"},
		{"compiler02.example.com", "production", "stale9", "-", "9m0s", "ago"},
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
	}, nil, now); err != nil {
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

// A full code_id is 64 hex characters, which would push every other column off a
// terminal. The table shortens it; --json carries the whole thing.
func TestPrintFleetShortensIDs(t *testing.T) {
	const full = "3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb"
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := printFleet(&buf, []publish.Peer{{
		Certname: "compiler01.example.com",
		LastPoll: now,
		Serving:  map[string]string{"production": full},
	}}, nil, now); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, full[:shortID]) {
		t.Errorf("output does not show the shortened id:\n%s", got)
	}
	if strings.Contains(got, full) {
		t.Errorf("output shows the full 64-character id:\n%s", got)
	}
}

// --json must carry everything the table shows, at full length. It is what a
// monitoring check reads, and a truncated id cannot be compared exactly.
func TestFleetRecordsJSON(t *testing.T) {
	const (
		full   = "3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb"
		commit = "a3f1c9e4b2d8f70e5c91847d3b6209fe1a4c8d02"
	)
	peers := []publish.Peer{{
		Certname: "compiler01.example.com",
		LastPoll: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Serving:  map[string]string{"production": full, "testing": "def456"},
		Polls:    240,
	}}

	out, err := json.Marshal(fleetRecords(peers, map[string]string{
		"production\x00" + full: commit,
	}))
	if err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Certname string            `json:"certname"`
		Serving  map[string]string `json:"serving"`
		Commits  map[string]string `json:"commits"`
		Polls    uint64            `json:"polls"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}

	// The embedded Peer's fields stay at the top level, so anything written
	// against /v1/compilers reads this output unchanged.
	if got[0].Certname != "compiler01.example.com" || got[0].Polls != 240 {
		t.Errorf("the peer's own fields did not survive embedding: %+v", got[0])
	}
	if got[0].Serving["production"] != full {
		t.Errorf("code_id = %q, want the full id", got[0].Serving["production"])
	}
	if got[0].Commits["production"] != commit {
		t.Errorf("commit = %q, want %q", got[0].Commits["production"], commit)
	}
	// An environment with no recorded provenance is absent, not empty: a
	// missing record must never read as another version's commit.
	if _, present := got[0].Commits["testing"]; present {
		t.Errorf("testing has a commit it never recorded: %+v", got[0].Commits)
	}
}

// #55: an empty entry passes ServerTLS's "authorizes nobody" guard, because the
// list is non-empty — and then matches nothing, so the publisher starts and
// refuses the whole estate. The only clue was a trailing space in one startup
// line, which is the silent misconfiguration this tool otherwise refuses to allow.
func TestPublishRejectsEmptyAllowlistEntries(t *testing.T) {
	dir := t.TempDir()
	basedir := filepath.Join(dir, "environments")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"empty --allow-role", []string{"--allow-role", ""}, "allow-role"},
		{"empty --allow-certname", []string{"--allow-certname", ""}, "allow-certname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--basedir", basedir}, tc.args...)
			err := run("publish", args)
			if err == nil {
				t.Fatal("started with an empty allowlist entry")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// A non-empty value is still accepted, so the check has not broken the setting.
func TestPublishAcceptsNonEmptyAllowlistEntries(t *testing.T) {
	dir := t.TempDir()
	basedir := filepath.Join(dir, "environments")
	if err := os.MkdirAll(basedir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This fails later, on SSL material that does not exist here — the point is
	// that it gets past allowlist validation rather than being rejected for it.
	err := run("publish", []string{"--basedir", basedir, "--allow-role", "openvox_compiler",
		"--ssldir", filepath.Join(dir, "nossl")})
	if err != nil && strings.Contains(err.Error(), "allow-role") {
		t.Errorf("a valid role was rejected: %v", err)
	}
}
