// Package acceptance times codavox's per-deploy phases on a realistic tree.
//
// It measures only codavox's own work — seal, materialize, unpack, verify,
// swap, and the per-compile commands — not r10k, which codavox shells out to
// and does not own. r10k's git and network time dominates a real deploy and is
// too variable to be a regression signal; isolating codavox's cost is what
// makes a regression visible.
//
// Run it on demand:
//
//	CODAVOX_ACCEPTANCE=1 go test -v ./test/acceptance/
//
// Point it at a real resolved tree (an r10k basedir's environment) to get
// real-shape numbers instead of the synthetic fixture:
//
//	CODAVOX_ACCEPTANCE=1 CODAVOX_ACCEPTANCE_TREE=/etc/puppetlabs/code-staging/production \
//	  go test -v ./test/acceptance/
package acceptance

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/content"
	"github.com/miharp/codavox/internal/layout"
	"github.com/miharp/codavox/internal/seal"
	"github.com/miharp/codavox/internal/treegen"
)

func TestDeployPhaseTimings(t *testing.T) {
	if os.Getenv("CODAVOX_ACCEPTANCE") == "" {
		t.Skip("set CODAVOX_ACCEPTANCE=1 to run the timing harness")
	}

	src, size := sourceTree(t)
	mb := float64(size) / (1 << 20)
	t.Logf("tree: %.1f MB", mb)

	timed := func(name string, f func() error) {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d := time.Since(start)
		rate := ""
		if d > 0 {
			rate = fmt.Sprintf("%7.1f MB/s", mb/d.Seconds())
		}
		t.Logf("  %-22s %8.1f ms   %s", name, float64(d.Microseconds())/1000, rate)
	}

	// Publisher: seal, then materialize the artifact.
	var id string
	timed("seal (code_id)", func() (err error) { id, err = seal.CodeID(src); return })

	artifact := filepath.Join(t.TempDir(), "artifact.tar.gz")
	timed("materialize artifact", func() error {
		f, err := os.Create(artifact)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return seal.WriteArchive(f, src)
	})

	// Compiler: unpack, verify by re-sealing, then swap the symlink.
	base := t.TempDir()
	l := layout.Layout{
		Root:            filepath.Join(base, "codavox"),
		EnvironmentPath: filepath.Join(base, "environments"),
	}
	verDir := l.VersionDir("production", id)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	timed("agent unpack", func() error {
		af, err := os.Open(artifact)
		if err != nil {
			return err
		}
		defer func() { _ = af.Close() }()
		return seal.ExtractArchive(af, verDir)
	})
	timed("verify (re-seal)", func() error {
		got, err := seal.CodeID(verDir)
		if err != nil {
			return err
		}
		if got != id {
			return fmt.Errorf("verify mismatch: %s != %s", got, id)
		}
		return nil
	})
	if err := os.MkdirAll(l.EnvironmentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	timed("symlink swap", func() error {
		tmp := l.EnvironmentLink("production") + ".tmp"
		if err := os.Symlink(verDir, tmp); err != nil {
			return err
		}
		return os.Rename(tmp, l.EnvironmentLink("production"))
	})

	// Per-compile commands.
	timed("code-id", func() error { _, err := l.CurrentCodeID("production"); return err })
	timed("code-content (site.pp)", func() error {
		return content.Copy(io.Discard, l, "production", id, "manifests/site.pp")
	})
}

// sourceTree returns the tree to measure and its size: a caller-supplied real
// tree, or the synthetic fixture.
func sourceTree(t *testing.T) (string, int64) {
	t.Helper()
	if real := os.Getenv("CODAVOX_ACCEPTANCE_TREE"); real != "" {
		t.Logf("measuring real tree at %s", real)
		return real, dirSize(t, real)
	}
	dir := t.TempDir()
	st, err := treegen.Generate(dir, treegen.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic tree: %d files", st.Files)
	return dir, st.Bytes
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}
