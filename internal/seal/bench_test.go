package seal

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/miharp/codavox/internal/treegen"
)

// These benchmarks measure codavox's per-deploy costs on a realistic resolved
// tree — the work r10k's output puts on the publisher (seal, materialize) and
// on every compiler (unpack, and the verify re-seal). Run them with:
//
//	go test -run '^$' -bench . -benchmem ./internal/seal/
//
// b.SetBytes reports MB/s against the tree size, so a regression shows up as a
// throughput drop independent of the machine.

func benchTree(b *testing.B) (string, int64) {
	b.Helper()
	dir := b.TempDir()
	st, err := treegen.Generate(dir, treegen.Default())
	if err != nil {
		b.Fatal(err)
	}
	return dir, st.Bytes
}

// BenchmarkCodeID measures sealing: the publisher runs it on every deploy, and
// every compiler runs it again to verify a downloaded artifact.
func BenchmarkCodeID(b *testing.B) {
	dir, n := benchTree(b)
	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := CodeID(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteArchive measures materializing the artifact the publisher serves.
func BenchmarkWriteArchive(b *testing.B) {
	dir, n := benchTree(b)
	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := WriteArchive(io.Discard, dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExtractArchive measures unpacking, which runs on every compiler on
// every deploy. The per-iteration temp directory setup is excluded from timing.
func BenchmarkExtractArchive(b *testing.B) {
	dir, n := benchTree(b)
	var buf bytes.Buffer
	if err := WriteArchive(&buf, dir); err != nil {
		b.Fatal(err)
	}
	archive := buf.Bytes()

	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		dst, err := os.MkdirTemp(b.TempDir(), "extract")
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if err := ExtractArchive(bytes.NewReader(archive), dst); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		_ = os.RemoveAll(dst)
		b.StartTimer()
	}
}
