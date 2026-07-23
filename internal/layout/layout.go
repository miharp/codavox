// Package layout resolves and validates codavox's on-disk paths.
//
// The validation here deliberately mirrors OpenVox Server's own schemas
// (puppetlabs/puppetserver/common.clj). Rejecting bad input before it reaches
// the filesystem gives a clearer error than letting OpenVox Server reject our
// output after the fact.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultRoot is where deployed environment versions live.
const DefaultRoot = "/opt/puppetlabs/codavox"

// DefaultEnvironmentPath is a codavox-owned directory that OpenVox Server is
// pointed at, deliberately *not* the stock /etc/puppetlabs/code/environments.
//
// A freshly installed OpenVox Server ships a populated skeleton at
// code/environments/production — data, environment.conf, hiera.yaml, manifests
// and modules. rename(2) cannot replace a real directory with a symlink, so
// managing that path would mean either refusing to start or moving an
// operator's directory aside on first run. Worse, that directory may hold code
// somebody deployed by other means, and taking it over would make it vanish
// from where they left it.
//
// Owning a separate codedir avoids the collision entirely: codavox creates it,
// only ever puts symlinks in it, and the stock path is left untouched for
// anyone still using it. Point OpenVox Server at it with:
//
//	puppet config set --section main environmentpath /opt/puppetlabs/codavox/environments
const DefaultEnvironmentPath = "/opt/puppetlabs/codavox/environments"

// RootEnvVar and EnvironmentPathEnvVar override the defaults. Intended for
// tests and for running the binary as an unprivileged user.
const (
	RootEnvVar            = "CODAVOX_ROOT"
	EnvironmentPathEnvVar = "CODAVOX_ENVIRONMENTPATH"
)

// environmentPattern matches OpenVox Server's Environment schema: "Alphanumeric
// and _ only", i.e. re-matches #"\w+". Go's \w is ASCII-only, as is Java's by
// default, so the two agree.
var environmentPattern = regexp.MustCompile(`^\w+$`)

// codeIDPattern matches OpenVox Server's CodeId schema, which rejects any
// character outside alphanumerics and - _ ; : — note this excludes '/', '.',
// '+' and '=', so base64 digests are not usable as code IDs.
var codeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-:;]+$`)

// Layout resolves paths beneath a codavox root directory.
type Layout struct {
	// Root holds unpacked environment versions.
	Root string
	// EnvironmentPath is OpenVox Server's environmentpath, where each
	// environment is a symlink into Root.
	EnvironmentPath string
}

// New returns a Layout from the environment, falling back to the defaults.
func New() Layout {
	l := Layout{Root: DefaultRoot, EnvironmentPath: DefaultEnvironmentPath}
	if v := os.Getenv(RootEnvVar); v != "" {
		l.Root = v
	}
	if v := os.Getenv(EnvironmentPathEnvVar); v != "" {
		l.EnvironmentPath = v
	}
	return l
}

// ValidateEnvironment reports whether name is acceptable to OpenVox Server.
func ValidateEnvironment(name string) error {
	if !environmentPattern.MatchString(name) {
		return fmt.Errorf("invalid environment %q: must be alphanumeric and _ only", name)
	}
	return nil
}

// ValidateCodeID reports whether id is acceptable to OpenVox Server.
func ValidateCodeID(id string) error {
	if !codeIDPattern.MatchString(id) {
		return fmt.Errorf("invalid code_id %q: must contain only alphanumerics and '-', '_', ';', ':'", id)
	}
	return nil
}

// VersionDir is the unpacked tree for a specific (env, code_id) pair.
func (l Layout) VersionDir(env, codeID string) string {
	return filepath.Join(l.Root, "versions", VersionDirName(env, codeID))
}

// VersionDirName is the directory name encoding an environment and code_id.
func VersionDirName(env, codeID string) string { return env + "_" + codeID }

// EnvironmentLink is the symlink OpenVox Server resolves for an environment.
func (l Layout) EnvironmentLink(env string) string {
	return filepath.Join(l.EnvironmentPath, env)
}

// CurrentCodeID returns the code_id an environment is currently serving.
//
// The answer is derived from the environment symlink rather than from a
// separate state file, and that is a correctness requirement rather than a
// shortcut.
//
// OpenVox Server does two independent things when compiling a static catalog:
// it reads the environment directory, and it runs code-id-command. If those
// consulted two different sources, every deploy would have a window where they
// disagreed — a catalog compiled from one version but stamped with another,
// whose file content then resolves against the wrong tree. Swapping a symlink
// and writing a file cannot be made simultaneous, and no ordering avoids it:
// swap first and the id lags the tree, write first and the tree lags the id.
//
// Reading the link makes the two answers the same fact. A single rename(2)
// changes what OpenVox Server serves and what this reports at the same instant.
//
// It stays a single syscall, which matters because OpenVox Server spawns
// code-id-command fresh on every catalog compile with no caching.
func (l Layout) CurrentCodeID(env string) (string, error) {
	if err := ValidateEnvironment(env); err != nil {
		return "", err
	}

	link := l.EnvironmentLink(env)
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("reading environment link %s: %w", link, err)
	}

	// The link points at versions/<env>_<code_id>.
	name := filepath.Base(target)
	prefix := env + "_"
	id, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", fmt.Errorf("environment link %s points at %q, which is not a %s version directory", link, name, env)
	}
	if id == "" {
		return "", fmt.Errorf("environment link %s carries an empty code_id", link)
	}
	if err := ValidateCodeID(id); err != nil {
		return "", fmt.Errorf("environment link %s: %w", link, err)
	}

	return id, nil
}
