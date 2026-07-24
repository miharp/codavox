package publish

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// more than one commit; that is useful history, not a conflict.
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
	seen    map[string]bool // codeID + "\x00" + commit
}

// OpenLog loads the provenance log at path, creating nothing.
//
// A missing file is not an error: it means nothing has been sealed yet. Only a
// file that exists but cannot be read is a failure, because that hides history
// that is genuinely there.
func OpenLog(path string) (*Log, error) {
	l := &Log{path: path, seen: map[string]bool{}}

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
		l.seen[key(p.CodeID, p.Commit)] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading provenance log %s: %w", path, err)
	}
	return l, nil
}

func key(codeID, commit string) string { return codeID + "\x00" + commit }

// Record appends p unless an identical (code_id, commit) pair is already known.
//
// Recording is best-effort by contract: the caller must not fail a deploy
// because provenance could not be written. A write failure still leaves the
// record in memory, so the running process can report it, but the error is
// returned so the caller can surface it if it wants to.
func (l *Log) Record(p Provenance) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := key(p.CodeID, p.Commit)
	if l.seen[k] {
		return nil
	}
	l.seen[k] = true
	l.records = append(l.records, p)

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
// An empty result is the honest answer "nothing was recorded for this id," not
// an error: provenance is best-effort, so its absence must never masquerade as
// a different id's history.
func (l *Log) Lookup(env, codeID string) []Provenance {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []Provenance
	for _, p := range l.records {
		if p.Env == env && p.CodeID == codeID {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SealedAt.After(out[j].SealedAt) })
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
