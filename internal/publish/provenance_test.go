package publish

import (
	"os"
	"path/filepath"
	"testing"
)

// stageWithDeploy writes an environment's files plus an optional
// .r10k-deploy.json (when deploy is non-empty) into a fresh staging directory.
func stageWithDeploy(t *testing.T, env string, files map[string]string, deploy string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, env, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if deploy != "" {
		if err := os.WriteFile(filepath.Join(dir, env, ".r10k-deploy.json"), []byte(deploy), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// sealWithProvenance seals staging with a provenance log at logPath.
func sealWithProvenance(t *testing.T, staging, logPath string) (*Store, *Log) {
	t.Helper()
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	s := NewStore(staging, t.TempDir())
	s.EnableProvenance(log)
	if err := s.Reseal(); err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	return s, log
}

func TestProvenanceCapturesCommit(t *testing.T) {
	staging := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "node default { }\n"},
		`{"name":"production","signature":"a3f1c9e4b2d8","finished_at":"2026-07-24 12:00:00 -0400"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, staging, logPath)

	id := s.Environments()["production"]
	recs := log.Lookup("production", id)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Commit != "a3f1c9e4b2d8" {
		t.Errorf("commit = %q, want a3f1c9e4b2d8", recs[0].Commit)
	}
	if recs[0].DeployedAt != "2026-07-24 12:00:00 -0400" {
		t.Errorf("deployed_at = %q, want the verbatim r10k timestamp", recs[0].DeployedAt)
	}
	if recs[0].SealedAt.IsZero() {
		t.Error("sealed_at was not stamped")
	}

	// The excluded deploy file must not have leaked into the code_id: an
	// environment sealed without provenance capture gets the same id.
	bare := NewStore(staging, t.TempDir())
	if err := bare.Reseal(); err != nil {
		t.Fatal(err)
	}
	if bare.Environments()["production"] != id {
		t.Error("capturing provenance changed the code_id")
	}
}

func TestProvenanceMissingDeployFileIsNotAnError(t *testing.T) {
	// No .r10k-deploy.json at all: reseal must succeed and simply record nothing.
	staging := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"}, "")
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, staging, logPath)

	if got := log.Lookup("production", s.Environments()["production"]); len(got) != 0 {
		t.Errorf("recorded %d provenance records with no deploy file, want 0", len(got))
	}
	// A log that never captured anything writes no file.
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("provenance file should not exist when nothing was recorded, stat err = %v", err)
	}
}

func TestProvenanceMalformedDeployFileIsNotAnError(t *testing.T) {
	staging := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"}, "{ this is not json")
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, staging, logPath)

	if got := log.Lookup("production", s.Environments()["production"]); len(got) != 0 {
		t.Errorf("recorded %d records from a malformed deploy file, want 0", len(got))
	}
}

func TestProvenanceDeduplicates(t *testing.T) {
	staging := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"},
		`{"name":"production","signature":"cafe1234"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	_, log := sealWithProvenance(t, staging, logPath)

	// Reseal the identical tree twice more; the (code_id, commit) pair is
	// already known, so no duplicate rows accumulate.
	s := NewStore(staging, t.TempDir())
	s.EnableProvenance(log)
	if err := s.Reseal(); err != nil {
		t.Fatal(err)
	}
	if err := s.Reseal(); err != nil {
		t.Fatal(err)
	}

	if got := log.Lookup("production", s.Environments()["production"]); len(got) != 1 {
		t.Errorf("got %d records after three reseals, want 1 (deduplicated)", len(got))
	}
}

func TestProvenancePersistsAcrossReopen(t *testing.T) {
	staging := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"},
		`{"name":"production","signature":"deadbeef"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, _ := sealWithProvenance(t, staging, logPath)
	id := s.Environments()["production"]

	// A fresh process opening the same file must see the recorded history.
	reopened, err := OpenLog(logPath)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	recs := reopened.Lookup("production", id)
	if len(recs) != 1 || recs[0].Commit != "deadbeef" {
		t.Fatalf("reopened log = %+v, want one record for commit deadbeef", recs)
	}

	// Dedup state survives the reload: re-recording the same pair is a no-op.
	if err := reopened.Record(recs[0]); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Lookup("production", id); len(got) != 1 {
		t.Errorf("dedup did not survive reopen: got %d records, want 1", len(got))
	}
}

func TestProvenanceOneCodeIDManyCommits(t *testing.T) {
	// Same resolved content, two different control-repo commits: a commit that
	// does not change the tree seals to the same code_id, and both commits are
	// legitimately recorded against it.
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}

	var id string
	for _, commit := range []string{"1111aaaa", "2222bbbb"} {
		staging := stageWithDeploy(t, "production",
			map[string]string{"manifests/site.pp": "unchanged\n"},
			`{"name":"production","signature":"`+commit+`"}`)
		s := NewStore(staging, t.TempDir())
		s.EnableProvenance(log)
		if err := s.Reseal(); err != nil {
			t.Fatal(err)
		}
		id = s.Environments()["production"]
	}

	recs := log.Lookup("production", id)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 commits for one code_id", len(recs))
	}
	// Most recently sealed first.
	if recs[0].Commit != "2222bbbb" || recs[1].Commit != "1111aaaa" {
		t.Errorf("records not newest-first: %q then %q", recs[0].Commit, recs[1].Commit)
	}
}

func TestProvenanceLookupIsolatesEnvAndCodeID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Record(Provenance{CodeID: "aaa", Env: "production", Commit: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Record(Provenance{CodeID: "aaa", Env: "testing", Commit: "c2"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Record(Provenance{CodeID: "bbb", Env: "production", Commit: "c3"}); err != nil {
		t.Fatal(err)
	}

	if got := log.Lookup("production", "aaa"); len(got) != 1 || got[0].Commit != "c1" {
		t.Errorf("lookup(production, aaa) = %+v, want just c1", got)
	}
	if got := log.Lookup("staging", "aaa"); len(got) != 0 {
		t.Errorf("lookup for an unknown environment returned %d records, want 0", len(got))
	}
}
