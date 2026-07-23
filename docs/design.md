# stagehand — design notes

> A stagehand moves the set pieces into position so every performance is
> identical. That is the job: get exactly the same code onto every compiler,
> and let each one say precisely which version it is serving.

Status: **design exploration.** Nothing implemented. Written 2026-07-23.

See [versioned-code-contract.md](versioned-code-contract.md) for the verified
puppetserver interface this builds on.

## Problem

Distributing Puppet code safely to compilers. The available options are all
unsatisfying:

- **webhooks** — push, fire-and-forget. A compiler that is down misses the
  event permanently. No convergence, no catch-up, no way to detect divergence.
- **NFS** — single point of failure for *catalog compilation itself*, not just
  for deploys. No atomicity: readers can observe partial state.
- **rsync** — not atomic, not versioned, no notion of which version is live.

### The reframe

All three distribute *code*. None distributes *identity*. Nothing in those
systems can answer "which exact version is this compiler serving," so there is
no way to detect divergence, let alone reason about it.

PE's answer is not a better transport — it is `code_id`. Once every compiler
can answer *what version am I serving* and *give me file X at version Y*,
divergence stops being a correctness bug and becomes a latency property.
Compilers converge on their own, and an agent mid-run gets consistent file
content even if code changes underneath it. That last property is what static
catalogs buy, and it is what none of webhook/NFS/rsync can offer at any price.

### The trap that kills the obvious design

"Run r10k on every compiler and trigger it well" **cannot converge**, and not
because of triggering.

**r10k is not deterministic across time.** A Puppetfile with `:latest`, or any
branch ref, resolves differently on Tuesday than on Monday. Two compilers
running r10k an hour apart get *different code from the same control-repo
commit*.

This is the real architectural reason PE stages centrally and ships artifacts.

> **Distribute resolved trees, never instructions for producing them.**

Any design that re-runs resolution per-compiler has split brain built in.

## Architecture

1. **Stage** — r10k deploys to a staging dir on one node. Fully resolved.
2. **Seal** — content-hash the tree (hex; see contract). That hash *is* the code_id.
3. **Publish** — authenticated endpoint exposing current code_id per environment.
4. **Distribute** — compilers **poll**, fetch the artifact for the new code_id,
   unpack to `…/code/<env>_<code_id>/`, then atomically swap a symlink.
5. **Identify** — `code-id` reads the symlink target (via a cached file);
   `code-content` serves `(code_id, path)` from the versioned dirs.
6. **Reap** — drop old versions after a TTL exceeding the longest agent run.

### Two properties to preserve

**Pull, not push.** Polling is self-healing; a compiler that was down catches
up on its next tick. Webhooks are acceptable *only* as a latency optimisation
layered on top, never as the correctness mechanism.

**Versioned dirs from day one.** PE's `versioned_deploys` exists to avoid
pausing puppetserver during the swap, and PE is actively deprecating the
non-versioned path. Symlink swap gives atomicity for free.

### Why this beats NFS on availability

Compilers hold their own copy. A primary outage means *no new deploys*; with
NFS it means *no catalogs at all*. The deploy plane is decoupled from the data
plane. mTLS between compilers and primary comes free off the existing Puppet CA.

## Transport options

| option | pros | cons |
|---|---|---|
| **bare git repo** | delta transfer, resumable, versioning + dedup free; closest to file sync's actual implementation | needs a git endpoint; checkout of huge trees is slowish |
| **OCI artifact** | layer dedup, registries everywhere with auth/mirroring/CDN solved; fits Vox Pupuli's container investment | new infra dependency |
| **tarball over HTTPS + hash** | dumbest thing that works | no dedup; poor at scale |

Git or OCI. OCI is the most modern fit given where Vox Pupuli already invests.

## Repo and packaging

**Own repo, not inside openvox-server.** The hook is already shipped and
enabled, so the server needs zero modification — which removes the only real
argument for living there. Release cadence, language freedom, and the fact that
publisher/agent/scripts have three different deployment targets all point the
same way.

Single Go binary, subcommands:

```
stagehand publish       # primary: run r10k, seal, serve version + artifacts
stagehand agent         # compiler: poll, fetch, unpack, symlink-swap, reap
stagehand code-id       # per-compile — must be ~1ms
stagehand code-content  # per static_file_content request
```

Plus a **separate Forge module** (`voxpupuli/stagehand`) to configure it, per
Vox Pupuli convention.

**Repo layout and packaging layout are independent decisions.** Own repo does
not preclude shipping the binary in the openvox-server package or container
image to guarantee presence on compilers. Get "always there" from packaging;
do not pay for it in source coupling.

**Escape hatch, not a starting point:** if a compiled binary's per-compile
spawn cost still proves too high, *that* is when to implement a real in-JVM
versioned-code-service in openvox-server — with measurements in hand, the way
PE evidently did.

## Known hard parts

- **Reaping vs in-flight agents.** Cannot delete a code_id something may still
  request via code-content. TTL plus refcounting.
- **puppetserver's environment cache** interacting with symlink swaps.
- **Environment deletion** propagating correctly to compilers.
- **Poller robustness** — the boring work that determines whether this is
  trustworthy.

## Open questions

- Survey whether anyone in the community has already built a file-sync
  alternative. Not investigated.
- Does openbolt bundle a compiled rugged? If so Vox Pupuli already has a
  cross-platform libgit2 build recipe in-house, which de-risks packaging
  rugged for r10k considerably. **Highest-value unknown.**
- Whether the agent should reuse r10k's Puppetfile resolution or treat the
  staged tree as fully opaque. (Leaning opaque.)
