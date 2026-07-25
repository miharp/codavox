package seal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzExtractArchive drives the one place codavox parses untrusted bytes.
//
// An artifact arrives over the network before anything has verified it — the
// code_id check happens on the extracted tree, which means extraction runs
// first, on whatever the publisher (or anything impersonating it) sent. So this
// function must never panic and must never write outside its directory,
// whatever it is fed.
//
// It found a real defect: the traversal guard compared a three-byte prefix by
// slicing, so any entry named with fewer than three characters crashed the
// agent. Ordinary control repos contain such names.
func FuzzExtractArchive(f *testing.F) {
	// Seed with shapes a generic fuzzer would take a long time to construct:
	// valid gzip, valid tar, and the boundary cases around the traversal guard.
	for _, seed := range []struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}{
		{"manifests/site.pp", tar.TypeReg, "", "node default { }\n"},
		{"a", tar.TypeReg, "", ""},
		{"ca", tar.TypeReg, "", "x"},
		{"..", tar.TypeReg, "", "x"},
		{"../escape", tar.TypeReg, "", "x"},
		{"/absolute", tar.TypeReg, "", "x"},
		{"dir", tar.TypeDir, "", ""},
		{"link", tar.TypeSymlink, "target", ""},
		{"link", tar.TypeSymlink, "../../etc/passwd", ""},
		{"link", tar.TypeSymlink, "/etc/passwd", ""},
	} {
		f.Add(seedArchive(seed.name, seed.typeflag, seed.linkname, seed.body))
	}
	// Non-archives, so the gzip and tar error paths are reached too.
	f.Add([]byte{})
	f.Add([]byte("not a gzip stream"))

	f.Fuzz(func(t *testing.T, data []byte) {
		base := t.TempDir()
		dst := filepath.Join(base, "extract")

		// A malformed archive must fail, not panic. The error itself is not
		// interesting; surviving it is.
		_ = ExtractArchive(bytes.NewReader(data), dst)

		// Nothing may appear outside the extraction directory, however the entry
		// paths and symlink targets were constructed.
		entries, err := os.ReadDir(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "extract" {
				t.Fatalf("extraction escaped its directory: wrote %q", e.Name())
			}
		}

		// No file inside may be reachable through a link that leaves the tree,
		// because OpenVox Server serves this tree directly and would follow one.
		walkErr := filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // a partially extracted tree is fine; escaping is not
			}
			if d.Type()&os.ModeSymlink == 0 {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			if filepath.IsAbs(target) {
				t.Errorf("extracted an absolute symlink %s -> %s", path, target)
				return nil
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
			if !strings.HasPrefix(resolved, filepath.Clean(dst)) {
				t.Errorf("extracted an escaping symlink %s -> %s", path, target)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
	})
}

// seedArchive builds a one-entry gzipped tar for the corpus.
func seedArchive(name string, typeflag byte, linkname, body string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	hdr := &tar.Header{Name: name, Typeflag: typeflag, Mode: 0o644, Format: tar.FormatPAX}
	switch typeflag {
	case tar.TypeSymlink:
		hdr.Linkname = linkname
	case tar.TypeReg:
		hdr.Size = int64(len(body))
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil
	}
	if typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil
		}
	}
	if err := tw.Close(); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}
