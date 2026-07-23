package seal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func archive(t *testing.T, root string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteArchive(&buf, root); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	return buf.Bytes()
}

// The archive must be byte-identical for identical content, or it cannot be
// content-addressed, cached, or deduplicated by a transport.
func TestArchiveIsByteReproducible(t *testing.T) {
	files := map[string]string{
		"manifests/site.pp":      "node default { }\n",
		"modules/apache/init.pp": "class apache { }\n",
	}

	a := archive(t, tree(t, files))
	time.Sleep(1100 * time.Millisecond) // cross a second boundary
	b := archive(t, tree(t, files))

	if !bytes.Equal(a, b) {
		t.Errorf("archives differ across runs: %d vs %d bytes", len(a), len(b))
	}
}

func TestArchiveIgnoresModificationTime(t *testing.T) {
	root := tree(t, map[string]string{"a.pp": "x\n"})
	before := archive(t, root)

	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.pp"), old, old); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(before, archive(t, root)) {
		t.Error("mtime changed the archive bytes")
	}
}

// A gzip header records a timestamp by default, which would silently defeat
// reproducibility even when the tar payload is identical.
func TestArchiveHeadersAreNormalized(t *testing.T) {
	root := tree(t, map[string]string{
		"plain.txt": "x\n",
		"run.sh":    "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	data := archive(t, root)

	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !zr.ModTime.IsZero() {
		t.Errorf("gzip header carries a timestamp: %v", zr.ModTime)
	}
	if zr.Name != "" {
		t.Errorf("gzip header carries a filename: %q", zr.Name)
	}

	tr := tar.NewReader(zr)
	seen := map[string]int64{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// Epoch, not "no value": tar has no way to express an absent mtime,
		// so a fixed constant is the reproducible choice.
		if hdr.ModTime.Unix() != 0 {
			t.Errorf("%s carries non-epoch mtime %v", hdr.Name, hdr.ModTime)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s carries ownership: uid=%d gid=%d uname=%q gname=%q",
				hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
		seen[hdr.Name] = hdr.Mode
	}

	if seen["plain.txt"] != 0o644 {
		t.Errorf("plain.txt mode = %o, want 644", seen["plain.txt"])
	}
	if seen["run.sh"] != 0o755 {
		t.Errorf("run.sh mode = %o, want 755", seen["run.sh"])
	}
}

func TestArchiveAppliesExclusions(t *testing.T) {
	root := tree(t, map[string]string{
		"a.pp":              "x\n",
		".git/HEAD":         "ref: refs/heads/main\n",
		".r10k-deploy.json": `{"started_at":"now"}`,
	})

	zr, err := gzip.NewReader(bytes.NewReader(archive(t, root)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != "a.pp" {
			t.Errorf("archive contains excluded entry %q", hdr.Name)
		}
	}
}

// Round-tripping is what the agent will do, so extraction must reproduce a
// tree that seals to the same code_id.
func TestRoundTripPreservesCodeID(t *testing.T) {
	files := map[string]string{
		"manifests/site.pp":      "node default { }\n",
		"modules/apache/init.pp": "class apache { }\n",
		"nested/deep/file.txt":   "deep\n",
	}
	if runtime.GOOS != "windows" {
		files["link"] = "link:manifests/site.pp"
	}

	src := tree(t, files)
	want := mustCodeID(t, src)

	dst := filepath.Join(t.TempDir(), "extracted")
	if err := ExtractArchive(bytes.NewReader(archive(t, src)), dst); err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}

	if got := mustCodeID(t, dst); got != want {
		t.Errorf("round trip changed the code_id:\n  want %s\n  got  %s", want, got)
	}
}

// An artifact arrives over the network, so its entries are untrusted.
func TestExtractRejectsEscapingPaths(t *testing.T) {
	build := func(t *testing.T, name string, typeflag byte, linkname string) []byte {
		t.Helper()
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		hdr := &tar.Header{Name: name, Typeflag: typeflag, Mode: 0o644, Format: tar.FormatPAX}
		if typeflag == tar.TypeSymlink {
			hdr.Linkname = linkname
		} else {
			hdr.Size = 4
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("evil")); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	cases := []struct {
		name     string
		entry    string
		typeflag byte
		linkname string
	}{
		{"dot-dot traversal", "../escaped.txt", tar.TypeReg, ""},
		{"deep traversal", "a/../../escaped.txt", tar.TypeReg, ""},
		{"absolute path", "/etc/escaped.txt", tar.TypeReg, ""},
		{"symlink escaping the root", "link", tar.TypeSymlink, "../../etc/passwd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.typeflag == tar.TypeSymlink && runtime.GOOS == "windows" {
				t.Skip("symlink semantics differ on windows")
			}
			base := t.TempDir()
			dst := filepath.Join(base, "extract")

			err := ExtractArchive(bytes.NewReader(build(t, tc.entry, tc.typeflag, tc.linkname)), dst)
			if err == nil {
				t.Errorf("entry %q was extracted; expected rejection", tc.entry)
			}

			if _, statErr := os.Stat(filepath.Join(base, "escaped.txt")); statErr == nil {
				t.Errorf("entry %q wrote outside the extraction root", tc.entry)
			}
		})
	}
}

func TestExtractRejectsUnsupportedEntryTypes(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "dev", Typeflag: tar.TypeChar, Mode: 0o644, Format: tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ExtractArchive(&buf, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Error("expected rejection of a character-device entry")
	}
}

// A mode from the archive is attacker-controlled. Sealing only ever emits
// 0644 or 0755, so extraction must not honour setuid, setgid, or sticky bits
// that a crafted artifact could otherwise plant on every compiler.
func TestExtractNormalizesModes(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range []struct {
		name string
		mode int64
	}{
		{"setuid-root", 0o4755},
		{"setgid", 0o2644},
		{"sticky", 0o1644},
		{"world-writable", 0o666},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Typeflag: tar.TypeReg, Mode: e.mode, Size: 2, Format: tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := ExtractArchive(&buf, dst); err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}

	for _, name := range []string{"setuid-root", "setgid", "sticky", "world-writable"} {
		fi, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		mode := fi.Mode()
		if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s extracted with special bits: %v", name, mode)
		}
		if perm := mode.Perm(); perm != 0o644 && perm != 0o755 {
			t.Errorf("%s extracted with mode %o, want 0644 or 0755", name, perm)
		}
	}
}
