// Package seal derives a reproducible code_id from a staged environment tree.
//
// The code_id must be a pure function of tree content: the same tree sealed on
// two machines, at different times, by different users, must produce the same
// id. Anything else breaks the guarantee that compilers serving the same id are
// serving the same code, which is the property the whole system rests on.
//
// Sealing therefore builds a canonical manifest and hashes that, rather than
// hashing files directly. The manifest is reproducible by construction, and it
// can be printed — when two trees disagree, diffing manifests shows exactly
// which entry differs, which a bare digest cannot.
package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestVersion prefixes every manifest. Hashing it means a future change to
// the canonical form yields different ids rather than silently colliding with
// ids produced by the old algorithm.
const manifestVersion = "codavox-tree-v1"

// excluded names are skipped wherever they appear in the tree.
//
// .git holds packfiles and reflogs that differ between clones of identical
// content. .r10k-deploy.json embeds deploy timestamps, so including it would
// make every deploy of unchanged code produce a new code_id.
var excluded = map[string]bool{
	".git":              true,
	".r10k-deploy.json": true,
}

// Entry is one line of a manifest.
type Entry struct {
	// Kind is "file" or "link".
	Kind string
	// Mode is normalized to 0644 or 0755 for files, empty for links. Real
	// modes vary with the umask of whoever ran r10k, so only the executable
	// bit is meaningful.
	Mode string
	// Digest is the SHA-256 of file content, or of a symlink's target.
	Digest string
	// Size is the content length, or the target length for a symlink.
	Size int64
	// Path is slash-separated and relative to the tree root.
	Path string
}

// String renders the canonical manifest line.
func (e Entry) String() string {
	if e.Kind == "link" {
		return fmt.Sprintf("link %s %d %s", e.Digest, e.Size, e.Path)
	}
	return fmt.Sprintf("file %s %s %d %s", e.Mode, e.Digest, e.Size, e.Path)
}

// Manifest walks root and returns its canonical manifest.
//
// Deliberately excluded from the manifest, because none is a property of the
// code itself: modification times, ownership, and permission bits beyond the
// executable bit. Including any of them would make identical code seal to
// different ids depending on who deployed it and when.
func Manifest(root string) ([]byte, error) {
	entries, err := walk(root)
	if err != nil {
		return nil, err
	}

	// Sort by path so filesystem iteration order cannot affect the result.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var buf bytes.Buffer
	buf.WriteString(manifestVersion)
	buf.WriteByte('\n')
	for _, e := range entries {
		buf.WriteString(e.String())
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// CodeID returns the hex SHA-256 of root's manifest.
//
// Hex, not base64: OpenVox Server's CodeId schema rejects '+' and '=', so a
// base64 digest would be refused at runtime.
func CodeID(root string) (string, error) {
	m, err := Manifest(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(m)
	return hex.EncodeToString(sum[:]), nil
}

func walk(root string) ([]Entry, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("reading tree root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tree root %s is not a directory", root)
	}

	var entries []Entry
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}

		if excluded[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		switch {
		case d.IsDir():
			// Directories carry no content. They are implied by the paths of
			// the entries beneath them, so an empty directory is invisible to
			// the manifest — matching git, and avoiding a dependence on
			// whether a transport preserves empty directories.
			return nil

		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("reading link %s: %w", rel, err)
			}
			// Hash the target rather than following the link: following would
			// double-count files inside the tree and could escape it entirely.
			sum := sha256.Sum256([]byte(target))
			entries = append(entries, Entry{
				Kind:   "link",
				Digest: hex.EncodeToString(sum[:]),
				Size:   int64(len(target)),
				Path:   rel,
			})
			return nil

		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			digest, err := fileDigest(path)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{
				Kind:   "file",
				Mode:   normalizeMode(fi.Mode()),
				Digest: digest,
				Size:   fi.Size(),
				Path:   rel,
			})
			return nil

		default:
			// Sockets, devices and FIFOs have no reproducible content and have
			// no business in a Puppet environment. Failing is better than
			// silently sealing a tree that cannot be faithfully reproduced.
			return fmt.Errorf("unsupported file type at %s: %s", rel, d.Type())
		}
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// normalizeMode reduces a file mode to its only reproducible component.
func normalizeMode(m fs.FileMode) string {
	if m.Perm()&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from walking the tree
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IsExcluded reports whether a path component is omitted from sealing.
func IsExcluded(name string) bool { return excluded[name] }

// ExcludedNames lists the omitted path components, for documentation and tests.
func ExcludedNames() []string {
	names := make([]string, 0, len(excluded))
	for n := range excluded {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ManifestString renders a manifest for human inspection.
func ManifestString(root string) (string, error) {
	m, err := Manifest(root)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(m), "\n"), nil
}
