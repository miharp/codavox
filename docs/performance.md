# Performance

codavox's per-deploy cost is measured on a realistic synthetic tree, so a
regression shows up as a throughput drop that is independent of the machine.

## What is measured, and what is not

A deploy is r10k resolving modules plus codavox sealing, serving, and unpacking
the result:

```text
deploy = r10k(resolve modules)   +   codavox(seal + materialize + unpack + verify + swap)
         └─ git and network,          └─ deterministic, the regression signal
            not codavox, dominant
```

r10k's time dominates a real deploy and swings with the network, and codavox
does not own it — it shells out to the r10k already on the node. So the
benchmarks and the acceptance harness measure **only codavox's work**, on the
tree r10k produces. To see a true end-to-end "50-module deploy" number, point
the acceptance harness at a real resolved tree (below); the r10k time is then
whatever your control repo and network give you, separately from these numbers.

## The fixture

`internal/treegen` builds a deterministic tree resembling a mid-sized control
repo — by default about 50 modules, ~2,500 files, ~35 MB, with a realistic size
mix. The same seed always produces the same tree and the same `code_id`, so
numbers are comparable run to run.

## Benchmarks

```console
go test -run '^$' -bench . -benchmem ./internal/seal/ ./internal/layout/
```

`SetBytes` reports MB/s against the tree size. The operations:

| benchmark | what it is | who pays it |
|---|---|---|
| `BenchmarkCodeID` | seal: walk + hash the tree | publisher per deploy; every compiler, to verify |
| `BenchmarkWriteArchive` | materialize the gzipped artifact | publisher per deploy |
| `BenchmarkExtractArchive` | unpack the artifact | every compiler per deploy |
| `BenchmarkCurrentCodeID` | read the environment symlink | every static catalog compile |

## Acceptance timing harness

An end-to-end phase breakdown of a single deploy, on demand:

```console
CODAVOX_ACCEPTANCE=1 go test -v ./test/acceptance/
```

```text
tree: 35.3 MB
  seal (code_id)             80.7 ms      437 MB/s
  materialize artifact     2698.2 ms       13 MB/s
  agent unpack              596.5 ms       59 MB/s
  verify (re-seal)           95.9 ms      368 MB/s
  symlink swap                0.2 ms
  code-id                     0.0 ms
  code-content (site.pp)      0.1 ms
```

Point it at a real resolved tree for real-shape numbers:

```console
CODAVOX_ACCEPTANCE=1 \
  CODAVOX_ACCEPTANCE_TREE=/etc/puppetlabs/code-staging/production \
  go test -v ./test/acceptance/
```

## How to read it

- **`code-id` stays microseconds.** It runs on every static catalog compile, so
  this is the number that must never regress. It is a single symlink read.
- **Materializing the artifact dominates codavox's cost.** On the default tree,
  gzip compression is roughly three quarters of the total, at ~13 MB/s — far
  slower than sealing (~440 MB/s) or unpacking (~60 MB/s). It is the first place
  to look for a speedup: the gzip level trades compression ratio for speed, and
  it does not affect the `code_id`, which is hashed from the tree, not the
  archive.
- **Sealing and unpacking scale with tree size.** A control repo several times
  larger than the fixture scales these roughly linearly.

## In CI

The benchmarks and the acceptance harness run in CI for visibility, printed to
the job log — not as a pass/fail gate, because shared runners are too noisy for
a hard threshold. Running them also keeps them from silently failing to compile.
Compare across commits with [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).
