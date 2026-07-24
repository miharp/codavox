package treegen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeterministic is the property the benchmarks rely on: the same options
// produce a byte-identical tree, so numbers are comparable run to run.
func TestDeterministic(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	sa, err := Generate(a, Default())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := Generate(b, Default())
	if err != nil {
		t.Fatal(err)
	}
	if sa != sb {
		t.Errorf("stats differ: %+v vs %+v", sa, sb)
	}

	// Spot-check a file's bytes are identical across generations.
	fa, err := os.ReadFile(filepath.Join(a, "modules/module00/metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.ReadFile(filepath.Join(b, "modules/module00/metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(fa) != string(fb) {
		t.Error("same seed produced different file content")
	}
}

func TestReportsSize(t *testing.T) {
	dir := t.TempDir()
	st, err := Generate(dir, Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default tree: %d files, %.1f MB", st.Files, float64(st.Bytes)/(1<<20))
	if st.Files < 1000 {
		t.Errorf("only %d files; expected a few thousand", st.Files)
	}
}

func TestSmallOptionsForFastTests(t *testing.T) {
	dir := t.TempDir()
	st, err := Generate(dir, Options{Modules: 2, FilesPerModule: 3, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files == 0 {
		t.Error("generated no files")
	}
}
