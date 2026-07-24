# codavox — design notes

> A *coda* is the passage that brings every performance to the same close.
> That is the job: get exactly the same code onto every compiler, and let each
> one say precisely which version it is serving.

Status: **implemented.** This records the architecture and the reasoning behind
it; the design below is what codavox does. Originally written 2026-07-23.

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

The missing primitive is not a better transport — it is `code_id`. Once every
compiler can answer *what version am I serving* and *give me file X at version
Y*, divergence stops being a correctness bug and becomes a latency property.
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

> **Distribute resolved trees, never instructions for producing them.**

Any design that re-runs resolution per-compiler has split brain built in. This
is the constraint that forces central staging plus artifact distribution.

## Architecture

1. **Stage** — r10k deploys to a staging dir on one node. Fully resolved.
2. **Seal** — content-hash the tree (hex; see contract). That hash *is* the code_id.
3. **Publish** — authenticated endpoint exposing current code_id per environment.
4. **Distribute** — compilers **poll**, fetch the artifact for the new code_id,
   unpack to `versions/<env>_<code_id>/`, then atomically swap a symlink.
5. **Identify** — `code-id` reads the environment symlink directly; the symlink
   is the only source of truth, with no separate state file. `code-content`
   serves `(code_id, path)` from the versioned dirs.
6. **Reap** — drop old versions once they are past a keep count and older than a
   minimum age exceeding the longest agent run.

### Two properties to preserve

**Pull, not push.** Polling is self-healing; a compiler that was down catches
up on its next tick. Webhooks are acceptable *only* as a latency optimization
layered on top, never as the correctness mechanism.

**Versioned directories from day one.** Deploying into a fresh
`<env>_<code_id>/` and swapping a symlink makes the cutover atomic — no reader
ever observes a partial tree, and the server never has to be paused or locked
to make the swap safe. Deploying in place cannot offer that, and retrofitting
versioned layout later means redoing the reaping, the code-id lookup, and the
content addressing all at once.

### Why this beats NFS on availability

Compilers hold their own copy. A primary outage means *no new deploys*; with
NFS it means *no catalogs at all*. The deploy plane is decoupled from the data
plane. mTLS between compilers and primary comes free off the existing Puppet CA.

## Transport options

| option | pros | cons |
| --- | --- | --- |
| **bare git repo** | delta transfer, resumable, versioning + dedup free | needs a git endpoint; checkout of huge trees is slowish |
| **OCI artifact** | layer dedup, registries everywhere with auth/mirroring/CDN solved; fits Vox Pupuli's container investment | new infra dependency |
| **tarball over HTTPS + hash** | simplest option that works | no dedup; poor at scale |

Git or OCI. OCI is the most modern fit given where Vox Pupuli already invests.

**v1 is tarball over HTTPS** — the shortest path to a working end-to-end system
that integration tests can exercise. Git or OCI is the v2 direction, not yet
built.

## Repo and packaging

**Own repo, not inside openvox-server.** The hook is already shipped and
enabled, so the server needs zero modification — which removes the only real
argument for living there. Release cadence, language freedom, and the fact that
publisher/agent/scripts have three different deployment targets all point the
same way.

**Language: Go.** Settled, not merely preferred — the per-compile spawn cost
(see contract) rules out interpreted languages for the compiler-side commands,
and a single static binary with no runtime dependency is exactly what you want
landing on every compiler.

Single Go binary, subcommands:

```text
codavox deploy        # primary: run r10k, seal, trigger a reseal
codavox deploy-server # primary: deploy API and control-repo webhook
codavox publish       # primary: seal a staging dir, serve versions + artifacts
codavox agent         # compiler: poll, fetch, unpack, symlink-swap, reap
codavox code-id       # per-compile — must be ~1ms
codavox code-content  # per static_file_content request
```

r10k stays r10k's job: `deploy` runs it and codavox distributes the result, so
`publish` only ever observes a staging dir it does not manage.

A **separate Forge module** (`voxpupuli/codavox`) to configure it, per Vox
Pupuli convention, is still planned.

**Repo layout and packaging layout are independent decisions.** Own repo does
not preclude shipping the binary in the openvox-server package or container
image to guarantee presence on compilers. Get "always there" from packaging;
do not pay for it in source coupling.

## Known hard parts

- **Reaping vs in-flight agents.** *Solved:* the agent keeps a version while it
  is current, among the most recent *keep*, or younger than *min-age*, so a
  code_id an in-flight run still requests via code-content is never deleted.
- **Environment deletion** propagating to compilers. *Addressed, opt-in:* with
  `--prune-environments`, an agent removes an environment the publisher no longer
  serves, guarded so a failed or empty poll never deletes. It relies on r10k
  purging the removed environment from staging.
- **puppetserver's environment cache** interacting with symlink swaps.
- **Poller robustness** — *addressed:* a poll failure is logged and retried on
  the next tick rather than being fatal, so a publisher outage degrades to "no
  new deploys" rather than "no catalogs."

## Open questions

- Whether the agent should reuse r10k's Puppetfile resolution or treat the
  staged tree as fully opaque. *Resolved: opaque.* The deploy runs r10k centrally
  and codavox distributes the result; the agent never resolves anything.
- Does openbolt bundle a compiled rugged? If so Vox Pupuli already has a
  cross-platform libgit2 build recipe in-house, which de-risks packaging
  rugged for r10k considerably. **Highest-value unknown.**
- Survey whether anyone in the community has already built something similar.
  Not investigated.

## Name

`codavox` was checked and is unclaimed on GitHub (zero repositories), RubyGems,
npm, and the Puppet Forge, and is free on pkg.go.dev and the Go module proxy
for both `github.com/openvoxproject/codavox` and `github.com/voxpupuli/codavox`.
It is also a valid Go package identifier — lowercase, no hyphens.

Rejected: `stagehand` (collides with a 23.6k-star browser-agent SDK holding the
name on RubyGems and npm), `codesync` (1300+ GitHub repos, taken on npm), and
`voxsync` — the last because "sync" implies the file-copying model this design
explicitly rejects, and because it borrows `modulesync`'s shape without its
logic (`modulesync` names what it syncs; this would not).

Trademark review has not been done.
