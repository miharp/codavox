# stagehand

Versioned code distribution for OpenVox compilers, built on puppetserver's
open-source `versioned-code-service` hook.

> A stagehand moves the set pieces into position so every performance is
> identical.

**Status: design exploration. Nothing is implemented.**

## Why

Distributing Puppet code to compilers currently means webhooks (push,
fire-and-forget, no catch-up), NFS (single point of failure for compilation
itself), or rsync (not atomic, not versioned). All three distribute *code*;
none distributes *identity*, so nothing can answer "which version is this
compiler serving?"

stagehand distributes resolved code artifacts addressed by a `code_id`, so
compilers converge on their own and agents get consistent file content
mid-run via static catalogs.

## Docs

- [design.md](docs/design.md) — architecture, transport options, repo layout,
  known hard parts
- [versioned-code-contract.md](docs/versioned-code-contract.md) — the verified
  puppetserver interface, its validation rules, and the performance constraint
  that drives the language choice

## Key findings so far

- The `versioned-code-service` hook **already ships enabled** in openvox-server
  packages. No server changes are required.
- `code-id-command` is spawned **once per catalog compile, uncached** — so the
  compiler-side components must be compiled binaries, not Ruby.
- `code_id` accepts only `[a-zA-Z0-9_\-:;]`; use hex hashes, not base64.
- r10k is not deterministic across time, so per-compiler resolution can never
  converge. Distribute resolved trees, never Puppetfiles.
