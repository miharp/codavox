// Package content serves file bytes for a specific deployed code version.
package content

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/miharp/codavox/internal/layout"
)

// ErrVersionNotDeployed means the requested code_id is not present on this
// node. Callers must surface this as a failure and must never substitute
// content from a different version.
var ErrVersionNotDeployed = errors.New("code version not deployed")

// Copy writes the contents of path, as of the given (env, codeID), to w.
//
// Resolution is confined to the version directory using os.Root, which
// prevents escape via "..", absolute paths, or symlinks pointing outside the
// tree. That matters because the deployed tree arrives as an artifact: a
// malicious or careless symlink inside it must not become an arbitrary file
// read, and puppetserver passes the requested path through from the agent.
//
// It deliberately has no fallback. If the version is absent, this fails rather
// than serving whatever happens to be current — serving mismatched content
// while reporting success defeats the entire point of a static catalog.
func Copy(w io.Writer, l layout.Layout, env, codeID, path string) error {
	if err := layout.ValidateEnvironment(env); err != nil {
		return err
	}
	if err := layout.ValidateCodeID(codeID); err != nil {
		return err
	}

	dir := l.VersionDir(env, codeID)
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s at %s", ErrVersionNotDeployed, codeID, dir)
		}
		return fmt.Errorf("opening version directory: %w", err)
	}
	// Close errors on a read-only handle carry no information.
	defer func() { _ = root.Close() }()

	f, err := root.Open(strings.TrimPrefix(path, "/"))
	if err != nil {
		return fmt.Errorf("opening %q in %s: %w", path, codeID, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory, not a file", path)
	}

	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("streaming %q: %w", path, err)
	}
	return nil
}
