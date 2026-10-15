package publish

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provenance records where a sealed tree came from.
//
// It is publisher-only diagnostic state. It is never written into an artifact
// and never shipped to a compiler, so it cannot influence a code_id or the
// content a compiler serves — the whole point of keeping it here rather than in
// the tree. It answers the one question content addressing cannot: "which
// control-repo commit produced the version this compiler is serving?"
//
// One code_id can have several records. A commit that does not change resolved
// content seals to the same code_id, so the same id legitimately traces back to
// more than one commit; that is useful history, not a conflict. Records are
// per environment, because two environments with identical trees share a
// code_id but are deployed independently.
type Provenance struct {
	CodeID string `json:"code_id"`
	Env    string `json:"environment"`
	// Commit is the control-repo commit r10k resolved, from the "signature"
	// field of .r10k-deploy.json. Empty when r10k left no record.
	Commit string `json:"commit,omitempty"`
	// DeployedAt is r10k's own "finished_at" string, passed through verbatim
	// rather than parsed, so a change in r10k's timestamp format cannot break
	// capture. Empty when r10k left no record.
	DeployedAt string `json:"deployed_at,omitempty"`
	// SealedAt is when the publisher observed and sealed the tree. This one is
	// codavox's, so it is a real RFC 3339 timestamp.
	SealedAt time.Time `json:"sealed_at"`
	// Modules lists the git checkouts inside the tree and the commit each was
	// checked out at. A control-repo commit pins a module only when its
	// Puppetfile entry is immutable; a branch resolves to whatever the remote
	// held at deploy time, so this is the only record of what was sealed.
	// Empty for a tree with no git modules, and in records written before
	// codavox captured them.
	Modules []Module `json:"modules,omitempty"`
}

// Module is one git checkout inside a sealed environment, usually a Puppetfile
// module r10k cloned.
//
// It is read from the checkout's own .git rather than from the module_deploys
// list in .r10k-deploy.json, because that list goes stale: `r10k deploy
// module`, which `codavox deploy --modules` runs, re-resolves a module without
// rewriting the file. HEAD describes the tree being sealed whichever deploy
// path produced it.
type Module struct {
	// Path is the checkout's directory relative to the environment, with
	// forward slashes.
	Path string `json:"path"`
	// Commit is the object id HEAD names. Empty when HEAD is anything but a
	// bare id, such as a branch reference, which r10k never leaves: resolving
	// one would be guessing at what was checked out.
	Commit string `json:"commit,omitempty"`
	// Remote is remote.origin.url with any credentials removed, because
	// `codavox provenance` prints it. Empty when there is no origin.
	Remote string `json:"remote,omitempty"`
}

// Log is an append-only record of provenance, persisted as JSON lines.
//
// It is loaded once at startup and appended to as the publisher seals. The file
// is one JSON object per line so a partial write damages at most the last
// record, and so an operator can read or grep it directly.
type Log struct {
	path string

	mu      sync.Mutex
	records []Provenance
	newest  map[string]int // env + "\x00" + codeID -> index of its newest record
}

// OpenLog loads the provenance log at path, creating nothing.
//
// A missing file is not an error: it means nothing has been sealed yet. Only a
// file that exists but cannot be read is a failure, because that hides history
// that is genuinely there.
func OpenLog(path string) (*Log, error) {
	l := &Log{path: path, newest: map[string]int{}}

	f, err := os.Open(path) // #nosec G304 -- operator-supplied state path
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("opening provenance log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Records are short, but a pathological line should not wedge startup.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var p Provenance
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("parsing provenance log %s: %w", path, err)
		}
		l.records = append(l.records, p)
		// File order is append order, so the last record read for a key is its
		// newest.
		l.newest[key(p.Env, p.CodeID)] = len(l.records) - 1
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading provenance log %s: %w", path, err)
	}
	return l, nil
}

func key(env, codeID string) string { return env + "\x00" + codeID }

// sameSource reports whether two records for one environment and code_id name
// the same commit and the same modules, ignoring when each was sealed.
func sameSource(a, b Provenance) bool {
	return a.Commit == b.Commit && slices.Equal(a.Modules, b.Modules)
}

// Record appends p unless the newest record for its environment and code_id
// already names the same commit and the same modules.
//
// Every reseal records every environment, so this is what keeps identical
// reseals from growing the log. It compares against the newest record only,
// not against every commit ever seen, because the newest record is what the
// fleet view and `codavox provenance` report as the commit an environment is
// deployed from. Skipping any known pair would hide a branch reset to a commit
// it had before, and leave the commit in between reported as current. The
// environment is part of the key for the same reason: a branch cut from
// production has production's code_id and commit, and must still get a record
// of its own.
//
// Modules are compared too, so that a log written before they were captured
// gains them at the next reseal instead of answering with none forever.
//
// Recording is best-effort by contract: the caller must not fail a deploy
// because provenance could not be written. A write failure still leaves the
// record in memory, so the running process can report it, but the error is
// returned so the caller can surface it if it wants to.
func (l *Log) Record(p Provenance) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := key(p.Env, p.CodeID)
	if i, ok := l.newest[k]; ok && sameSource(l.records[i], p) {
		return nil
	}
	l.records = append(l.records, p)
	l.newest[k] = len(l.records) - 1

	// The log is publisher-only diagnostic state read by the operator on this
	// node; nothing else needs it, so keep it tight (0700 dir, 0600 file).
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- operator-supplied state path
	if err != nil {
		return fmt.Errorf("opening provenance log for append: %w", err)
	}
	defer func() { _ = f.Close() }()

	line, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding provenance record: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("appending provenance record: %w", err)
	}
	return nil
}

// Lookup returns the records for an (env, code_id), most recently sealed first.
//
// The order is append order reversed rather than a sort on SealedAt. Records
// are appended as they are sealed, so the two agree, but append order is the
// one Record compares against, and a clock stepped backwards cannot reorder it.
//
// An empty result is the honest answer "nothing was recorded for this id," not
// an error: provenance is best-effort, so its absence must never masquerade as
// a different id's history.
func (l *Log) Lookup(env, codeID string) []Provenance {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Provenance
	for i := len(l.records) - 1; i >= 0; i-- {
		if p := l.records[i]; p.Env == env && p.CodeID == codeID {
			out = append(out, p)
		}
	}
	return out
}

// Match returns the code_ids recorded for env that begin with prefix, sorted.
//
// It lets an operator look a version up by what a run log shows, the short id
// in a catalog label, rather than only by the full id. It never chooses between
// several matches: which one a report meant is not something the log can know,
// so an ambiguous prefix is the caller's to refuse.
func (l *Log) Match(env, prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []string
	for k := range l.newest {
		e, id, _ := strings.Cut(k, "\x00")
		if e == env && strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// deployRecord is the subset of r10k's .r10k-deploy.json codavox reads.
type deployRecord struct {
	Name       string `json:"name"`
	Signature  string `json:"signature"`
	FinishedAt string `json:"finished_at"`
}

// readDeployRecord reads r10k's deploy metadata from a staged environment.
//
// It is deliberately forgiving: a missing or malformed file yields ok=false and
// no error, because provenance is diagnostic and its absence must never stop a
// deploy. .r10k-deploy.json is excluded from sealing, so reading it here has no
// effect on the code_id.
func readDeployRecord(envDir string) (deployRecord, bool) {
	b, err := os.ReadFile(filepath.Join(envDir, ".r10k-deploy.json")) // #nosec G304
	if err != nil {
		return deployRecord{}, false
	}
	var d deployRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return deployRecord{}, false
	}
	return d, true
}

// readModules lists the git checkouts inside a staged environment and the
// commit each has checked out, in lexical path order.
//
// r10k clones every git module with its own .git, under both its shellgit and
// rugged providers, and checks the resolved commit out as a detached HEAD. The
// environment's own .git is the control repo, whose commit is already the
// record's Commit, so it is not listed.
//
// Like the rest of provenance it is forgiving: an unreadable directory is
// skipped, and an unreadable HEAD or config leaves that field empty, because
// none of it may fail a reseal. A .git that is a file rather than a directory
// is listed with neither. .git is excluded from sealing, so nothing read here
// can affect the code_id.
func readModules(envDir string) []Module {
	var mods []Module
	_ = filepath.WalkDir(envDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != ".git" {
			return nil
		}
		if dir := filepath.Dir(path); dir != envDir {
			if rel, err := filepath.Rel(envDir, dir); err == nil {
				m := Module{Path: filepath.ToSlash(rel)}
				if d.IsDir() {
					m.Commit = readHead(path)
					m.Remote = readOrigin(path)
				}
				mods = append(mods, m)
			}
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return mods
}

// readHead returns the object id a checkout's HEAD names, or "" when HEAD is a
// reference or cannot be read.
func readHead(gitDir string) string {
	b, err := readSmall(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(string(b)); isObjectID(id) {
		return id
	}
	return ""
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object id.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readOrigin returns remote.origin.url from a checkout's config, with any
// credentials removed, or "" when there is none.
//
// It reads the one key it needs rather than implementing git's config format.
// What r10k leaves, through `git clone` or rugged's config API, is the
// canonical `[remote "origin"]` section with the url on a line of its own.
func readOrigin(gitDir string) string {
	b, err := readSmall(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			name, sub, _ := strings.Cut(strings.Trim(line, "[]"), " ")
			// Section names are case-insensitive; subsection names are not.
			inOrigin = strings.EqualFold(name, "remote") && strings.TrimSpace(sub) == `"origin"`
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !inOrigin || !ok || !strings.EqualFold(strings.TrimSpace(name), "url") {
			continue
		}
		return redactURL(configValue(strings.TrimSpace(value)))
	}
	return ""
}

// configValue strips the quoting or trailing comment from a git config value.
func configValue(v string) string {
	if strings.HasPrefix(v, `"`) {
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[1 : 1+end]
		}
		return ""
	}
	if i := strings.IndexAny(v, "#;"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// redactURL drops the user information from a remote URL, which is where an
// HTTPS remote carries its token.
//
// A remote with no scheme, a path or the scp-style user@host:path, is returned
// as it is: the scp form has no place for a secret. A URL with a scheme that
// does not parse is dropped rather than printed, because what it carries cannot
// be told.
func redactURL(remote string) string {
	if !strings.Contains(remote, "://") {
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// readSmall reads at most 64 KiB of a file. HEAD and config are far smaller;
// the bound keeps a pathological file from costing a reseal anything.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a path inside the operator's basedir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}
