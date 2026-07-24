// Package treegen builds a synthetic resolved control-repo tree for benchmarks
// and the acceptance timing harness.
//
// It stands in for r10k's output — the tree codavox actually seals, serves, and
// unpacks — so codavox's own costs can be measured on a realistic shape without
// r10k's git and network time, which codavox does not own and cannot regress.
// Generation is deterministic given a seed, so the same options always produce
// the same tree, the same code_id, and comparable benchmark numbers.
//
// Nothing in the shipped binary imports it.
package treegen

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// Options controls the generated tree.
type Options struct {
	// Modules is how many module directories to create under modules/.
	Modules int
	// FilesPerModule is roughly how many files each module holds, spread across
	// manifests, templates, files, and lib.
	FilesPerModule int
	// Seed makes generation deterministic.
	Seed int64
}

// Default is a tree resembling a mid-sized control repo: about 50 modules and a
// few thousand files, tens of megabytes.
func Default() Options {
	return Options{Modules: 50, FilesPerModule: 48, Seed: 1}
}

// Stats reports what Generate produced.
type Stats struct {
	Files int
	Bytes int64
}

// subdirs and how many of a module's files land in each, as weights.
var subdirs = []struct {
	path   string
	weight int
}{
	{"manifests", 5},
	{"templates", 2},
	{"files", 2},
	{"lib/puppet/provider", 1},
}

// Generate writes a synthetic tree under root and returns its size.
func Generate(root string, o Options) (Stats, error) {
	rng := rand.New(rand.NewSource(o.Seed)) //nolint:gosec // deterministic fixture, not security
	var st Stats

	write := func(rel string, size int) error {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		f, err := os.Create(full) // #nosec G304 -- rel is generated, root is caller-supplied
		if err != nil {
			return err
		}
		if err := writeText(f, size, rng); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		st.Files++
		st.Bytes += int64(size)
		return nil
	}

	// Environment-level files, as r10k leaves them.
	for _, e := range []struct {
		rel  string
		size int
	}{
		{"manifests/site.pp", 2 * 1024},
		{"environment.conf", 512},
		{"hiera.yaml", 1024},
	} {
		if err := write(e.rel, e.size); err != nil {
			return st, err
		}
	}

	for m := 0; m < o.Modules; m++ {
		mod := fmt.Sprintf("modules/module%02d", m)
		if err := write(mod+"/metadata.json", 1024); err != nil {
			return st, err
		}
		if err := write(mod+"/README.md", 2*1024+rng.Intn(4*1024)); err != nil {
			return st, err
		}
		for i := 0; i < o.FilesPerModule; i++ {
			sub := pickSubdir(rng)
			rel := fmt.Sprintf("%s/%s/file%02d%s", mod, sub, i, ext(sub))
			if err := write(rel, fileSize(rng)); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}

func pickSubdir(rng *rand.Rand) string {
	total := 0
	for _, s := range subdirs {
		total += s.weight
	}
	n := rng.Intn(total)
	for _, s := range subdirs {
		if n < s.weight {
			return s.path
		}
		n -= s.weight
	}
	return subdirs[0].path
}

func ext(sub string) string {
	switch sub {
	case "manifests":
		return ".pp"
	case "templates":
		return ".erb"
	case "lib/puppet/provider":
		return ".rb"
	default:
		return ""
	}
}

// fileSize draws from a distribution weighted toward small files, with a few
// large ones, the way a real module tree is shaped.
func fileSize(rng *rand.Rand) int {
	switch n := rng.Intn(100); {
	case n < 80: // small: manifests, small templates
		return 1024 + rng.Intn(5*1024)
	case n < 97: // medium: templates, files, lib
		return 8*1024 + rng.Intn(24*1024)
	default: // large: a vendored blob
		return 128*1024 + rng.Intn(384*1024)
	}
}

// dict is a small vocabulary of Puppet-ish tokens. Text built from it
// compresses at a realistic ratio, unlike random bytes.
var dict = []string{
	"class", "define", "node", "include", "require", "contain", "file", "ensure",
	"present", "absent", "package", "service", "exec", "notify", "subscribe",
	"before", "template", "content", "source", "owner", "group", "mode", "path",
	"if", "unless", "case", "default", "true", "false", "undef", "each", "with",
	"lookup", "hiera", "fact", "trusted", "certname", "environment", "profile",
	"role", "params", "config", "manage", "enable", "running", "installed",
}

// writeText writes size bytes of dictionary text to w.
func writeText(w *os.File, size int, rng *rand.Rand) error {
	buf := make([]byte, 0, 8192)
	written := 0
	for written < size {
		buf = buf[:0]
		for len(buf) < 8192 && written+len(buf) < size {
			buf = append(buf, dict[rng.Intn(len(dict))]...)
			if rng.Intn(10) == 0 {
				buf = append(buf, '\n')
			} else {
				buf = append(buf, ' ')
			}
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
		written += len(buf)
	}
	return nil
}
