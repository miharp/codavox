// Package layout resolves and validates codavox's on-disk paths.
//
// The validation here deliberately mirrors puppetserver's own schemas
// (puppetlabs/puppetserver/common.clj). Rejecting bad input before it reaches
// the filesystem gives a clearer error than letting puppetserver reject our
// output after the fact.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultRoot is where deployed environment versions and state live.
const DefaultRoot = "/opt/puppetlabs/codavox"

// RootEnvVar overrides DefaultRoot. Intended for tests and for running the
// binary as an unprivileged user; not expected in production.
const RootEnvVar = "CODAVOX_ROOT"

// environmentPattern matches puppetserver's Environment schema: "Alphanumeric
// and _ only", i.e. re-matches #"\w+". Go's \w is ASCII-only, as is Java's by
// default, so the two agree.
var environmentPattern = regexp.MustCompile(`^\w+$`)

// codeIDPattern matches puppetserver's CodeId schema, which rejects any
// character outside alphanumerics and - _ ; : — note this excludes '/', '.',
// '+' and '=', so base64 digests are not usable as code IDs.
var codeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-:;]+$`)

// Layout resolves paths beneath a codavox root directory.
type Layout struct {
	Root string
}

// New returns a Layout rooted at CODAVOX_ROOT if set, else DefaultRoot.
func New() Layout {
	if root := os.Getenv(RootEnvVar); root != "" {
		return Layout{Root: root}
	}
	return Layout{Root: DefaultRoot}
}

// ValidateEnvironment reports whether name is acceptable to puppetserver.
func ValidateEnvironment(name string) error {
	if !environmentPattern.MatchString(name) {
		return fmt.Errorf("invalid environment %q: must be alphanumeric and _ only", name)
	}
	return nil
}

// ValidateCodeID reports whether id is acceptable to puppetserver.
func ValidateCodeID(id string) error {
	if !codeIDPattern.MatchString(id) {
		return fmt.Errorf("invalid code_id %q: must contain only alphanumerics and '-', '_', ';', ':'", id)
	}
	return nil
}

// StateFile is the path holding the currently deployed code_id for env.
func (l Layout) StateFile(env string) string {
	return filepath.Join(l.Root, "state", env+".codeid")
}

// VersionDir is the unpacked tree for a specific (env, code_id) pair.
func (l Layout) VersionDir(env, codeID string) string {
	return filepath.Join(l.Root, "versions", env+"_"+codeID)
}

// CurrentCodeID returns the deployed code_id for env.
//
// This is on the critical path of every static catalog compile: puppetserver
// spawns the process fresh each time and does not cache. It must stay a single
// open+read — no git, no directory walk, no lock, no JSON parsing.
//
// It never falls back to a derived or generated value. A missing or malformed
// state file is an error, because emitting a wrong-but-plausible code_id would
// silently corrupt content addressing rather than fail visibly.
func (l Layout) CurrentCodeID(env string) (string, error) {
	if err := ValidateEnvironment(env); err != nil {
		return "", err
	}

	raw, err := os.ReadFile(l.StateFile(env))
	if err != nil {
		return "", fmt.Errorf("reading code_id for environment %q: %w", env, err)
	}

	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("empty code_id in %s", l.StateFile(env))
	}
	if err := ValidateCodeID(id); err != nil {
		return "", fmt.Errorf("in %s: %w", l.StateFile(env), err)
	}

	return id, nil
}
