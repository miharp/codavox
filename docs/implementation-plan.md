# codavox — implementation plan

Written 2026-07-23. Companion to [design.md](design.md) and
[versioned-code-contract.md](versioned-code-contract.md).

Integration testing runs on `~/projects/ovadm`'s Docker Compose environment,
with `~/projects/control-repo` on Vagrant as a higher-fidelity tier-2 check.

---

## 0. What already exists (and why it motivates this)

`control-repo` already ships `profile::static_catalogs`, which deploys working
`code_id.sh` and `code_content.sh` scripts. It is a genuine working baseline —
and it demonstrates all three problems codavox exists to solve:

1. **`code_id.sh` invokes `/opt/puppetlabs/puppet/bin/ruby -rjson` to parse
   `.r10k-deploy.json`.** That is a full Ruby interpreter start on *every
   catalog compile* — the ~100 ms path the contract doc warns about, live.
2. **It falls back to `date +%s`.** A timestamp code_id changes on every call,
   so content addressing degrades to nothing and historical lookups are
   impossible.
3. **`code_content.sh` falls back to reading the current filesystem** when the
   git lookup misses — silently serving *wrong-version* content while
   appearing to succeed. This defeats the entire point of static catalogs and
   fails silently, which is the worst failure mode available.

These scripts are the **control group**. Every phase below should be measured
and diffed against them. Phase 2 alone should produce a dramatic, defensible
latency number using nothing but the existing repo.

---

## 1. Topology — ovadm Docker (primary harness)

Use `~/projects/ovadm`'s Docker Compose environment, not a hand-built Vagrant
topology. It already provides exactly what codavox needs:

- `ovadm-server` (Rocky 9 + systemd, CA), `ovadm-compiler01`, `ovadm-agent`
- The agent is pre-pointed at **compiler01** for catalogs via
  `docker/agent-puppet.conf` — the compiler is already in the catalog path
- `ovadm::add_compiler` handles install, `puppet.conf`, CSR submission,
  signing, SSL, and service start in one plan

It also stamps `pp_role: openvox_compiler` into the compiler certificate, so
`$trusted['extensions']['pp_role']` classifies which nodes get the codavox
agent — no ENC, no node list to maintain. That is a better fit than anything
the Vagrant setup offers.

**Adding `compiler02`** — required, since convergence *between* compilers is
the property under test and a single compiler makes it unobservable:

- `docker-compose.yml`: copy the `compiler01` service block, change
  `container_name` and `hostname`
- `docker/inventory.yaml`: one more `docker://ovadm-compiler02` target
- `bolt plan run ovadm::add_compiler server_host=puppet compiler_hosts=compiler01,compiler02`

Node roles for codavox:

| container | role |
|---|---|
| `ovadm-server` | CA + **publisher**; stages code and runs `codavox publish` |
| `ovadm-compiler01` | openvox-server + `codavox agent` |
| `ovadm-compiler02` | openvox-server + `codavox agent` |
| `ovadm-agent` | catalog verification; already targets a compiler |

### Why this beats Vagrant here

- **Iteration speed.** codavox is poll-driven; the test loop runs constantly.
  `docker compose up -d` against a 5-VM, 9 GB Parallels topology is not close.
- **The catch-up test becomes trivial.** Test 2 is `docker stop ovadm-compiler02`
  → deploy → `docker start`. In Vagrant that is a slow `halt`/`up` cycle.
- **Partition testing becomes possible.** `docker network disconnect` lets us
  simulate a compiler that is up but unreachable — distinct from one that is
  down, and a case polling must handle. That is awkward to stage in Vagrant.
- **Compilers already exist.** No new provisioning to write or maintain.

### One setup detail that matters

Mount `/opt/puppetlabs/codavox` as a **named volume, not overlayfs**. Atomic
symlink swap is a core correctness claim, and testing rename semantics on a
container's overlay filesystem risks either false confidence or spurious
failures. Put the versions tree on a real filesystem.

### Vagrant as tier 2

Keep `control-repo` on Vagrant as a **higher-fidelity check before any
release**, not as the working loop. Containers share a kernel and do not
exercise SELinux file contexts, firewalld, or true systemd timer behavior —
all of which a real deployment hits. Run the suite there at phase 5, not on
every change.

`control-repo` remains valuable regardless as the **A/B baseline**: it has the
existing `profile::static_catalogs` to measure against.

**Open question to settle early:** compilers already get certs from the
server's CA via `add_compiler`, so artifact-fetch mTLS can likely reuse them
directly. Confirm before phase 3 — it may remove the whole question.

---

## 2. Repo scaffolding

```text
codavox/
  cmd/codavox/main.go
  internal/
    codeid/        # hashing, validation, state file
    seal/          # staging tree -> artifact
    transport/     # interface + implementations
    agent/         # poll, fetch, swap, reap
    publish/       # serve versions + artifacts
  docs/
  test/integration/
```

- Go module: `github.com/<org>/codavox` — org still undecided (`OpenVoxProject`
  vs `voxpupuli`); both module paths verified free. **Decide before first push**;
  changing it after anyone imports is painful.
- Single static binary, no cgo, so it drops onto any compiler with no runtime.
- Cross-compile matrix: `linux/amd64`, `linux/arm64` at minimum.
- CI: build, `go vet`, `golangci-lint`, unit tests. Integration tests are
  Docker-gated and run locally, not in CI initially.

---

## 3. Packaging (required for POC)

The POC must install via `dnf`/`apt`. Go makes this far easier than it would
have been in Ruby — a static binary with no runtime dependencies means the
package is essentially binary + systemd unit + config + directories.

### Tooling: goreleaser + nfpm

One `.goreleaser.yaml` produces cross-compiled binaries, `.rpm`, `.deb`,
checksums, and a GitHub Release from a single CI job. nfpm builds both package
formats **without needing `rpmbuild` or `dpkg-dev`**, so it runs anywhere
including macOS — no per-distro build hosts.

### arm64 is a primary target, not an afterthought

Local testing is on an M2 Pro: Parallels VMs are aarch64 and Docker on
M-series defaults to arm64. **If `linux/arm64` is not a first-class build
target, nothing installs on the dev machine.** Build `linux/amd64` and
`linux/arm64` from day one.

### The matrix collapses

Because the binary is static with no dependencies, there are no per-distro
builds — one RPM and one DEB per architecture covers everything in the test
estate:

| artifact | covers |
|---|---|
| `codavox-<v>.x86_64.rpm` / `.aarch64.rpm` | Rocky 9 (ovadm), CentOS Stream 9 + 10 (control-repo) |
| `codavox_<v>_amd64.deb` / `_arm64.deb` | Ubuntu 24.04 (control-repo agent02) |

If a distro-specific dependency ever appears, that is a signal something has
gone wrong with the "static binary" premise.

### Package contents

```text
/usr/bin/codavox
/usr/lib/systemd/system/codavox-agent.service
/usr/lib/systemd/system/codavox-publish.service     # primary only; not enabled by default
/etc/codavox/config.yaml                            # %config(noreplace) / deb conffile
/opt/puppetlabs/codavox/versions/                   # owned, 0755 root
/opt/puppetlabs/codavox/state/                      # owned, 0755 root
```

**Use `/usr/bin/codavox`, not `/opt/puppetlabs/bin/`.** That directory is owned
by the openvox-agent package; shipping into it invites file conflicts on
upgrade. `versioned-code.conf` takes an absolute path, so there is nothing to
gain from co-locating.

**Ship one package, not two.** It is a single binary; both units ship and the
operator (or the Forge module) enables what applies. Split into
`codavox-agent`/`codavox-server` only if the publisher grows dependencies the
compilers should not carry.

**postinst does `systemctl daemon-reload` and nothing else** — do not auto-enable
or auto-start. Let the Forge module own that decision, so package installation
is never itself a config change.

### Distribution: GitHub Releases first

For the POC, publish artifacts to GitHub Releases and install by direct URL:

```console
dnf install https://github.com/<org>/codavox/releases/download/v0.1.0/codavox-0.1.0.aarch64.rpm
```

This gets dependency resolution, clean upgrade, and clean uninstall — most of
the value of "installs with dnf/apt" — for almost no work. It also matches a
pattern the ecosystem already uses: ovadm's `install`/`upgrade` plans accept a
`package_url` for exactly this kind of pre-release artifact install.

**Real repository hosting is a separate project**, deliberately out of POC
scope: `createrepo` plus an apt repo, GPG key generation and distribution, key
rotation policy, and eventually a conversation about landing in
`yum.voxpupuli.org` / `apt.voxpupuli.org`. Signing keys in particular are a
commitment, not a build step. Do not start here.

---

## 4. Phase 1 — `code-id` and `code-content` (start here)

The leaf of the dependency tree, independently testable, and it produces the
headline number immediately.

**On-disk layout** (compiler side):

```text
/opt/puppetlabs/codavox/
  versions/<env>_<code_id>/      # unpacked trees
  state/<env>.codeid             # single line, hex code_id
/etc/puppetlabs/code/environments/<env> -> /opt/puppetlabs/codavox/versions/<env>_<code_id>
```

- `codavox code-id <env>` — reads `state/<env>.codeid`, writes it to stdout.
  One `open`+`read`. **No git, no directory walk, no lock, no JSON.**
- `codavox code-content <env> <code_id> <path>` — resolves
  `versions/<env>_<code_id>/<path>` and streams it.

**Correctness rules, learned from the existing scripts:**

- **Never fall back to current filesystem state on a code_id miss.** Exit
  non-zero. A hard failure the operator sees beats silently serving the wrong
  version. This is the single most important behavioral difference from the
  baseline.
- **Never emit a timestamp** or any non-deterministic id.
- Validate the code_id against `[a-zA-Z0-9_\-:;]` before use; reject path
  traversal in `<path>` explicitly.
- Silent on success — **anything on stderr is logged at ERROR by puppetserver
  on every compile.**
- Exit 0 only on genuine success.

**Deliverable test:** benchmark `codavox code-id production` against the
existing `code_id.sh` on the same box, N=1000. Expected roughly 100 ms → ~1 ms.
Publish the number; it justifies the whole language decision.

---

## 5. Phase 2 — seal and publish

`codavox publish`:

1. Run (or observe) r10k deploying into a staging dir.
2. Walk the staged tree deterministically, producing a **hex** content hash —
   sorted paths, file modes, content. That hash is the code_id.
3. Write the artifact.
4. Serve two endpoints over mTLS:
   - `GET /v1/environments` → `{env: code_id}` map
   - `GET /v1/artifact/<env>/<code_id>` → artifact bytes

**Hashing must be reproducible.** Same tree in, same id out, on any machine.
Exclude `.git`, `.r10k-deploy.json`, and anything with a mutable timestamp —
otherwise identical code produces different ids and every deploy churns.

**Transport for v1: plain tarball over HTTPS**, behind a `Transport` interface.
Not because it is the best answer — git or OCI probably is — but because it is
the shortest path to a working end-to-end system that integration tests can
exercise. Swap the implementation once the protocol is proven.

---

## 6. Phase 3 — agent

`codavox agent` on each compiler, as a systemd timer or small daemon:

1. Poll `/v1/environments` on an interval (default 30 s; jitter it).
2. On change: fetch artifact, verify hash **before** unpacking.
3. Unpack to `versions/<env>_<new_id>.tmp/`, then rename into place.
4. Write `state/<env>.codeid`.
5. Atomically swap the environment symlink — create a temp symlink, then
   `rename(2)` over the old one. `ln -sf` is **not** atomic; do not use it.
6. Reap old versions: keep last N (default 3) and anything younger than TTL
   (default 2× the longest agent run).

**Order matters.** Symlink swap must land *before* the state file is updated,
or `code-id` briefly advertises a version whose tree is not yet live.

**Pull only.** No webhook in v1 — adding one later is easy, and shipping
without it forces the polling path to be correct.

---

## 7. Phase 4 — Forge module

`voxpupuli/codavox`, kept in a separate repo per Vox Pupuli convention.
Manages the binary, systemd unit, config, and — critically — writes
`versioned-code.conf` pointing at the codavox commands.

It should be able to **take over from `profile::static_catalogs`**, so the
control-repo can flip between baseline and codavox for A/B testing. Model it
closely on the existing profile; that profile is the reference.

---

## 8. Integration tests

Under `test/integration/`, driven by the ovadm Docker topology. Each maps to a specific claim
in [design.md](design.md):

1. **Convergence** — deploy; both compilers report the same code_id within one
   poll interval.
2. **Catch-up / anti-split-brain** — halt `compiler02`, deploy, boot it. It
   converges without intervention. *This is the webhook failure mode; it is the
   headline test.*
3. **Atomicity** — hammer `code-id` in a loop through a swap. It must only ever
   return a complete, valid id, never a partial or missing tree.
4. **Content fidelity** — request `code-content` for code_id X after deploying
   Y. Must serve X's content, or fail loudly. **The existing `code_content.sh`
   fails this test** — demonstrating that is worth doing explicitly.
5. **Static catalog end-to-end** — `puppet agent -t` on agent01 against a
   compiler; verify the catalog carries a code_id and file content resolves.
6. **Reaping safety** — a code_id still referenced by an in-flight run is not
   deleted.
7. **Latency** — the phase 1 benchmark, as a regression guard.

Tests 2 and 4 are the ones that justify the project. Write them first.

---

## 9. Sequencing

| phase | deliverable | gate |
|---|---|---|
| 1 | `code-id` / `code-content` + benchmark vs baseline | latency number published |
| 1.5 | **goreleaser + nfpm producing rpm/deb for amd64 + arm64** | `dnf install <url>` works on a Rocky 9 container |
| 2 | `publish` + reproducible sealing | same tree → same id, twice, two machines |
| 3 | `agent` + `compiler02` added to ovadm compose | tests 1, 2, 3 pass |
| 4 | Forge module | control-repo can A/B baseline vs codavox |
| 5 | tests 4-7 | full suite green |
| 6 | transport swap (git or OCI) | behind the interface, no protocol change |

**Packaging lands at 1.5, immediately after the first working binary.** Doing it
early means every later phase is tested through the same install path operators
will use, rather than by copying binaries around and discovering packaging
problems at the end. It is also cheap at that point — one binary, one unit file
— and gets more expensive the longer it is deferred.

Phase 1 is worth doing even if the project stops there — it is a drop-in
improvement to the control-repo's existing static catalog setup.

---

## 10. Decisions still open

- **GitHub org** — blocks the Go module path. Decide first.
- **Compiler CA relationship** — blocks phase 3 mTLS.
- **Artifact format** — tarball for v1; git vs OCI for v2.
- Whether `publish` should invoke r10k itself or observe a directory it does
  not manage. *Leaning: observe. Not owning the deploy keeps the trust boundary
  small and lets existing r10k workflows continue untouched.*
- Trademark review of the name.
