package seal

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WriteArchive writes root as a deterministic gzipped tar.
//
// Determinism matters as much here as it does for the code_id. An archive that
// varies between runs cannot be content-addressed, cannot be cached or
// deduplicated by a transport, and makes "did this artifact change?"
// unanswerable without unpacking it.
//
// Reproducibility comes from normalizing everything the filesystem supplies
// that is not tree content:
//
//   - entries are emitted in sorted path order, not readdir order
//   - modification times are zeroed
//   - uid/gid are zeroed and owner names dropped
//   - modes are normalized to 0644/0755, matching the manifest
//   - the gzip header carries no timestamp or original filename
//
// The same exclusions apply as for the manifest, so the archive contains
// exactly the content the code_id covers.
func WriteArchive(w io.Writer, root string) error {
	entries, err := walk(root)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	// gzip.Writer would otherwise stamp the current time into the header.
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("creating gzip writer: %w", err)
	}
	zw.ModTime = time.Time{}
	zw.Name = ""
	zw.OS = 255 // unknown, rather than the building platform

	tw := tar.NewWriter(zw)

	for _, e := range entries {
		if err := writeEntry(tw, root, e); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	return nil
}

func writeEntry(tw *tar.Writer, root string, e Entry) error {
	full := filepath.Join(root, filepath.FromSlash(e.Path))

	hdr := &tar.Header{
		Name: e.Path,
		Mode: 0o644,
		Uid:  0,
		Gid:  0,
		// Set the epoch explicitly. A zero time.Time round-trips through tar
		// as Unix 0 anyway, but relying on that is a trap for anyone reading
		// the code later.
		ModTime: time.Unix(0, 0).UTC(),
		Uname:   "",
		Gname:   "",
		Format:  tar.FormatPAX,
	}

	switch e.Kind {
	case "link":
		target, err := os.Readlink(full)
		if err != nil {
			return fmt.Errorf("reading link %s: %w", e.Path, err)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = target
		hdr.Mode = 0o777
		return tw.WriteHeader(hdr)

	case "file":
		if e.Mode == "0755" {
			hdr.Mode = 0o755
		}
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.Size
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing header for %s: %w", e.Path, err)
		}

		f, err := os.Open(full) // #nosec G304 -- path comes from walking the tree
		if err != nil {
			return fmt.Errorf("opening %s: %w", e.Path, err)
		}
		defer func() { _ = f.Close() }()

		n, err := io.Copy(tw, f)
		if err != nil {
			return fmt.Errorf("archiving %s: %w", e.Path, err)
		}
		// A file changing size mid-seal corrupts the archive silently:
		// tar records the header size, so a short read leaves the stream
		// misaligned rather than erroring.
		if n != e.Size {
			return fmt.Errorf("%s changed size during archiving: expected %d bytes, wrote %d", e.Path, e.Size, n)
		}
		return nil

	default:
		return fmt.Errorf("unsupported entry kind %q at %s", e.Kind, e.Path)
	}
}

// ExtractArchive unpacks a gzipped tar produced by WriteArchive into dir.
//
// Entry paths are confined to dir: an artifact is downloaded from the network,
// so its contents are untrusted. A tar entry named "../../etc/passwd" or an
// absolute path must not escape, and neither must a symlink whose target
// points outside the tree.
func ExtractArchive(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = zr.Close() }()

	// 0755: OpenVox Server reads the extracted tree as a different user.
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening extraction root: %w", err)
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		if err := extractEntry(root, dir, tr, hdr); err != nil {
			return err
		}
	}
	return nil
}

func extractEntry(root *os.Root, dir string, tr *tar.Reader, hdr *tar.Header) error {
	name := filepath.Clean(hdr.Name)
	if filepath.IsAbs(name) || name == ".." || len(name) > 1 && name[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("refusing archive entry with escaping path %q", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return mkdirAllIn(root, name)

	case tar.TypeSymlink:
		// os.Root blocks *following* an escaping symlink, but it permits
		// creating one — the link itself is inside the root. That is not
		// enough here. codavox reads the tree through os.Root, but OpenVox
		// Server also serves files from the environment directory directly
		// and would follow such a link. Reject escaping targets at extraction
		// so the planted link never reaches disk.
		if err := checkLinkTarget(name, hdr.Linkname); err != nil {
			return err
		}
		if err := mkdirAllIn(root, filepath.Dir(name)); err != nil {
			return err
		}
		if err := root.Symlink(hdr.Linkname, name); err != nil {
			return fmt.Errorf("creating link %s: %w", name, err)
		}
		return nil

	case tar.TypeReg:
		if err := mkdirAllIn(root, filepath.Dir(name)); err != nil {
			return err
		}
		// Never trust a mode from the archive. Sealing only ever emits 0644
		// or 0755, so anything else is either corruption or an attempt to
		// plant a setuid, setgid, or sticky file on every compiler.
		f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, extractMode(hdr.Mode))
		if err != nil {
			return fmt.Errorf("creating %s: %w", name, err)
		}
		defer func() { _ = f.Close() }()

		// Bound the copy by the declared size so a malformed archive cannot
		// write unbounded data.
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("extracting %s: %w", name, err)
		}
		return nil

	default:
		return fmt.Errorf("unsupported archive entry type %q at %s", hdr.Typeflag, name)
	}
}

// mkdirAllIn creates dir and its parents beneath root, staying confined.
func mkdirAllIn(root *os.Root, dir string) error {
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return nil
	}
	parts := splitPath(dir)
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = filepath.Join(cur, p)
		}
		if err := root.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating directory %s: %w", cur, err)
		}
	}
	return nil
}

func splitPath(p string) []string {
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" {
			break
		}
		p = filepath.Clean(dir)
		if p == "." || p == string(filepath.Separator) {
			break
		}
	}
	return parts
}

// checkLinkTarget rejects a symlink whose target leaves the tree.
//
// Absolute targets are refused outright: a code artifact has no business
// pointing at host paths, and such a link means something different on every
// machine that extracts it. Relative targets are resolved against the link's
// own directory and must stay inside the tree.
func checkLinkTarget(name, target string) error {
	if target == "" {
		return fmt.Errorf("refusing empty symlink target at %s", name)
	}
	if filepath.IsAbs(target) {
		return fmt.Errorf("refusing absolute symlink target %q at %s", target, name)
	}

	// Resolve against the link's own directory, then check the result did not
	// climb out of the tree.
	resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
	first, _, _ := strings.Cut(filepath.ToSlash(resolved), "/")
	if first == ".." {
		return fmt.Errorf("refusing symlink %s -> %q: escapes the tree", name, target)
	}
	return nil
}

// extractMode reduces an archive entry's mode to the only two values sealing
// produces, discarding setuid, setgid, sticky and any other bits.
func extractMode(mode int64) fs.FileMode {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
