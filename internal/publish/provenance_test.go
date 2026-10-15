package publish

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// stageWithDeploy writes an environment's files plus an optional
// .r10k-deploy.json (when deploy is non-empty) into a fresh basedir directory.
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

// sealWithProvenance seals basedir with a provenance log at logPath.
func sealWithProvenance(t *testing.T, basedir, logPath string) (*Store, *Log) {
	t.Helper()
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	s := NewStore(basedir, t.TempDir())
	s.EnableProvenance(log)
	if err := s.Reseal(); err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	return s, log
}

func TestProvenanceCapturesCommit(t *testing.T) {
	basedir := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "node default { }\n"},
		`{"name":"production","signature":"a3f1c9e4b2d8","finished_at":"2026-07-24 12:00:00 -0400"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, basedir, logPath)

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
	bare := NewStore(basedir, t.TempDir())
	if err := bare.Reseal(); err != nil {
		t.Fatal(err)
	}
	if bare.Environments()["production"] != id {
		t.Error("capturing provenance changed the code_id")
	}
}

func TestProvenanceMissingDeployFileIsNotAnError(t *testing.T) {
	// No .r10k-deploy.json at all: reseal must succeed and simply record nothing.
	basedir := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"}, "")
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, basedir, logPath)

	if got := log.Lookup("production", s.Environments()["production"]); len(got) != 0 {
		t.Errorf("recorded %d provenance records with no deploy file, want 0", len(got))
	}
	// A log that never captured anything writes no file.
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("provenance file should not exist when nothing was recorded, stat err = %v", err)
	}
}

func TestProvenanceMalformedDeployFileIsNotAnError(t *testing.T) {
	basedir := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"}, "{ this is not json")
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, log := sealWithProvenance(t, basedir, logPath)

	if got := log.Lookup("production", s.Environments()["production"]); len(got) != 0 {
		t.Errorf("recorded %d records from a malformed deploy file, want 0", len(got))
	}
}

func TestProvenanceDeduplicates(t *testing.T) {
	basedir := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"},
		`{"name":"production","signature":"cafe1234"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	_, log := sealWithProvenance(t, basedir, logPath)

	// Reseal the identical tree twice more; the newest record already names
	// this commit, so no duplicate rows accumulate.
	s := NewStore(basedir, t.TempDir())
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

// A branch just cut from production has production's tree and HEAD, so the
// same code_id and the same commit. It is still its own environment, and the
// fleet view looks its commit up by environment, so it needs its own record.
func TestProvenanceUndivergedBranchGetsItsOwnRecord(t *testing.T) {
	basedir := t.TempDir()
	for _, env := range []string{"production", "feature"} {
		dir := filepath.Join(basedir, env, "manifests")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "site.pp"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		deploy := `{"name":"` + env + `","signature":"cafe1234"}`
		if err := os.WriteFile(filepath.Join(basedir, env, ".r10k-deploy.json"), []byte(deploy), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, log := sealWithProvenance(t, basedir, filepath.Join(t.TempDir(), "provenance.jsonl"))

	envs := s.Environments()
	if envs["production"] != envs["feature"] {
		t.Fatalf("identical trees sealed to different code_ids: %s and %s", envs["production"], envs["feature"])
	}
	for _, env := range []string{"production", "feature"} {
		if got := log.Lookup(env, envs[env]); len(got) != 1 || got[0].Commit != "cafe1234" {
			t.Errorf("lookup(%s) = %+v, want one record for cafe1234", env, got)
		}
	}
}

// Resetting a branch to a commit it had before, after another commit produced
// the same tree, must report the commit it was reset to. That pair is already
// in the log, but it is not the newest record, so it is recorded again.
func TestProvenanceReturnToEarlierCommit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{"aaaa1111", "bbbb2222", "aaaa1111"} {
		if err := log.Record(Provenance{CodeID: "x", Env: "production", Commit: commit}); err != nil {
			t.Fatal(err)
		}
	}
	if got := log.Lookup("production", "x"); len(got) != 3 || got[0].Commit != "aaaa1111" {
		t.Fatalf("lookup = %+v, want three records with aaaa1111 newest", got)
	}

	// A reopened log knows which record is newest, so resealing the same
	// commit again is still a no-op.
	reopened, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Record(Provenance{CodeID: "x", Env: "production", Commit: "aaaa1111"}); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Lookup("production", "x"); len(got) != 3 || got[0].Commit != "aaaa1111" {
		t.Errorf("after reopen, lookup = %+v, want the same three records", got)
	}
}

func TestProvenancePersistsAcrossReopen(t *testing.T) {
	basedir := stageWithDeploy(t, "production",
		map[string]string{"manifests/site.pp": "x\n"},
		`{"name":"production","signature":"deadbeef"}`)
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")

	s, _ := sealWithProvenance(t, basedir, logPath)
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
		basedir := stageWithDeploy(t, "production",
			map[string]string{"manifests/site.pp": "unchanged\n"},
			`{"name":"production","signature":"`+commit+`"}`)
		s := NewStore(basedir, t.TempDir())
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
	if got := log.Lookup("basedir", "aaa"); len(got) != 0 {
		t.Errorf("lookup for an unknown environment returned %d records, want 0", len(got))
	}
}

const (
	apacheSHA  = "a91c3e5f00112233445566778899aabbccddeeff"
	controlSHA = "c0ffee00112233445566778899aabbccddeeff00"
)

// A git module's commit and origin are read from its own .git. The control
// repo's .git at the environment root is the record's Commit, not a module, and
// a checkout whose HEAD is not a bare id is listed with no commit rather than
// one resolved by guessing.
func TestProvenanceCapturesModules(t *testing.T) {
	basedir := stageWithDeploy(t, "production", map[string]string{
		"manifests/site.pp":                "node default { }\n",
		".git/HEAD":                        controlSHA + "\n",
		"modules/apache/manifests/init.pp": "class apache { }\n",
		"modules/apache/.git/HEAD":         apacheSHA + "\n",
		"modules/apache/.git/config": "[core]\n\tbare = false\n" +
			"[remote \"cache\"]\n\turl = /var/cache/r10k/apache.git\n" +
			"[remote \"origin\"]\n\turl = https://x-access-token:s3cret@github.com/voxpupuli/puppet-apache.git\n" +
			"\tfetch = +refs/heads/*:refs/remotes/origin/*\n",
		"modules/concat/manifests/init.pp":    "class concat { }\n",
		"modules/concat/.git/HEAD":            "ref: refs/heads/main\n",
		"modules/stdlib/manifests/init.pp":    "class stdlib { }\n",
		"modules/stdlib/.git":                 "gitdir: /elsewhere\n",
		"site-modules/role/manifests/init.pp": "class role { }\n",
	}, `{"name":"production","signature":"`+controlSHA+`"}`)

	s, log := sealWithProvenance(t, basedir, filepath.Join(t.TempDir(), "provenance.jsonl"))

	recs := log.Lookup("production", s.Environments()["production"])
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	want := []Module{
		{Path: "modules/apache", Commit: apacheSHA, Remote: "https://github.com/voxpupuli/puppet-apache.git"},
		{Path: "modules/concat"},
		{Path: "modules/stdlib"},
	}
	if !slices.Equal(recs[0].Modules, want) {
		t.Errorf("modules = %+v\nwant      %+v", recs[0].Modules, want)
	}
}

// Two seals with the same commit but different module checkouts are different
// sources, even when the trees match: the newest must say which modules it was
// sealed from. Identical ones still add nothing, before and after a reopen.
func TestProvenanceRecordComparesModules(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	first := []Module{{Path: "modules/apache", Commit: apacheSHA}}
	second := []Module{{Path: "modules/apache", Commit: controlSHA}}
	for _, mods := range [][]Module{first, first, second, second} {
		if err := log.Record(Provenance{CodeID: "x", Env: "production", Commit: "cafe1234", Modules: mods}); err != nil {
			t.Fatal(err)
		}
	}
	got := log.Lookup("production", "x")
	if len(got) != 2 || !slices.Equal(got[0].Modules, second) {
		t.Fatalf("lookup = %+v, want two records with the second modules newest", got)
	}

	reopened, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Record(Provenance{CodeID: "x", Env: "production", Commit: "cafe1234", Modules: second}); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Lookup("production", "x"); len(got) != 2 {
		t.Errorf("after reopen, got %d records, want 2", len(got))
	}
}

// A log written before modules were captured has none in its newest record, so
// the next reseal writes one that does rather than answering with none forever.
func TestProvenanceBackfillsModulesIntoAnOlderLog(t *testing.T) {
	basedir := stageWithDeploy(t, "production", map[string]string{
		"manifests/site.pp":        "x\n",
		"modules/apache/.git/HEAD": apacheSHA + "\n",
	}, `{"name":"production","signature":"cafe1234"}`)

	bare := NewStore(basedir, t.TempDir())
	if err := bare.Reseal(); err != nil {
		t.Fatal(err)
	}
	id := bare.Environments()["production"]

	logPath := filepath.Join(t.TempDir(), "provenance.jsonl")
	old := `{"code_id":"` + id + `","environment":"production","commit":"cafe1234","sealed_at":"2026-09-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(logPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	_, log := sealWithProvenance(t, basedir, logPath)

	recs := log.Lookup("production", id)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want the old one plus one with modules", len(recs))
	}
	if want := []Module{{Path: "modules/apache", Commit: apacheSHA}}; !slices.Equal(recs[0].Modules, want) {
		t.Errorf("newest modules = %+v, want %+v", recs[0].Modules, want)
	}
}

func TestLogMatch(t *testing.T) {
	log, err := OpenLog(filepath.Join(t.TempDir(), "provenance.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Provenance{
		{Env: "production", CodeID: "abc222", Commit: "c1"},
		{Env: "production", CodeID: "abc111", Commit: "c2"},
		{Env: "production", CodeID: "abc111", Commit: "c3"},
		{Env: "production", CodeID: "def333", Commit: "c4"},
		{Env: "testing", CodeID: "abc999", Commit: "c5"},
	} {
		if err := log.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		env, prefix string
		want        []string
	}{
		{"production", "abc", []string{"abc111", "abc222"}},
		{"production", "abc111", []string{"abc111"}},
		{"production", "def", []string{"def333"}},
		{"production", "zzz", nil},
		{"testing", "abc", []string{"abc999"}},
		{"staging", "abc", nil},
	} {
		if got := log.Match(tc.env, tc.prefix); !slices.Equal(got, tc.want) {
			t.Errorf("Match(%s, %s) = %v, want %v", tc.env, tc.prefix, got, tc.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/voxpupuli/puppet-apache.git":        "https://github.com/voxpupuli/puppet-apache.git",
		"https://x-access-token:s3cret@github.com/org/repo.git": "https://github.com/org/repo.git",
		"https://glpat-s3cret@gitlab.example.com/org/repo.git":  "https://gitlab.example.com/org/repo.git",
		"ssh://git@github.com/org/repo.git":                     "ssh://github.com/org/repo.git",
		"git@github.com:org/repo.git":                           "git@github.com:org/repo.git",
		"/srv/modules/apache":                                   "/srv/modules/apache",
		"file:///srv/modules/apache":                            "file:///srv/modules/apache",
		"https://user:s3cret@bad host/repo.git":                 "",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadOrigin(t *testing.T) {
	for name, tc := range map[string]struct{ config, want string }{
		"canonical":                    {"[remote \"origin\"]\n\turl = https://example.com/a.git\n", "https://example.com/a.git"},
		"section and key case":         {"[Remote \"origin\"]\n\tURL = https://example.com/a.git\n", "https://example.com/a.git"},
		"quoted":                       {"[remote \"origin\"]\n\turl = \"https://example.com/a.git\"\n", "https://example.com/a.git"},
		"comment":                      {"[remote \"origin\"]\n\turl = /srv/a # local\n", "/srv/a"},
		"other remote only":            {"[remote \"cache\"]\n\turl = /var/cache/a.git\n", ""},
		"subsection is case-sensitive": {"[remote \"Origin\"]\n\turl = /srv/a\n", ""},
		"origin after another":         {"[remote \"cache\"]\n\turl = /var/cache/a.git\n[remote \"origin\"]\n\turl = /srv/a\n", "/srv/a"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(tc.config), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readOrigin(dir); got != tc.want {
			t.Errorf("%s: readOrigin = %q, want %q", name, got, tc.want)
		}
	}
}
