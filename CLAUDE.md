# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

codavox distributes versioned Puppet code to OpenVox compilers. It implements
OpenVox Server's (puppetserver's) already-shipped `versioned-code-service`
contract, so the server needs no modification. The core idea: distribute
*resolved code trees* addressed by a content-hash `code_id`, and let each
compiler answer "which exact version am I serving?" — turning divergence from a
correctness bug into a latency property. Compilers **poll** (never push), so a
compiler that was down catches up on its own.

Read [docs/design.md](docs/design.md) and
[docs/versioned-code-contract.md](docs/versioned-code-contract.md) before
proposing architectural changes — the hard constraints are already settled
there.

## Commands

```console
go build ./cmd/codavox          # build
go test -race ./...             # test (CI uses -race; the agent/code-id concurrency makes it load-bearing)
go test ./internal/seal/        # test one package
go test -run TestName ./internal/seal/
go test -bench BenchmarkCurrentCodeID ./internal/layout/   # guards the code-id latency requirement
go test -run '^$' -fuzz FuzzExtractArchive -fuzztime 60s ./internal/seal/   # the untrusted-input boundary
golangci-lint run ./...         # lint (CI pins golangci-lint v2.12.2)
gofmt -l .                      # must print nothing
shellcheck --external-sources $(find . -name '*.sh') .githooks/*   # harness, postinstall, git hooks
npx cspell '**/*.md'            # docs only; add terms to cspell.json rather than rewording
```

Requires Go 1.26 — CI takes the version from `go.mod`, so bumping the `go`
directive moves every workflow with it. Targets `linux/amd64` and `linux/arm64`
(both first-class; arm64 is the primary local dev target on Apple silicon). CI
runs every command above plus `go vet`, markdownlint, govulncheck, and the
cross-compile matrix.

Exercise the compiler-side commands without root using `CODAVOX_ROOT` (see
[CONTRIBUTING.md](CONTRIBUTING.md) for a copy-paste setup).

The integration harness in [test/integration/](test/integration/) is
self-contained: it stands up its own two-node OpenVox topology in Docker, needing
no ovadm. It runs on push to `main`, and on a pull request touching workflows,
packaging, or the harness itself — the paths nothing else verifies. Run it
locally before any change to TLS or the agent's HTTP client: `agent --once` is a
fresh process per sync, so no Go test can see keep-alive behavior.

## Architecture

Single Go binary, `github.com/miharp/codavox`, dispatched by subcommand in
[cmd/codavox/main.go](cmd/codavox/main.go). Two subcommands run per-compiler and
must stay fast; the others run on the primary or as a daemon.

The binary also dispatches on `argv[0]`: symlinks `codavox-code-id` and
`codavox-code-content` map to their subcommands, because OpenVox Server passes
only positional arguments and cannot invoke `codavox code-id <env>` with a
subcommand word.

Package map (`internal/`):

| package | role |
|---|---|
| `layout` | Resolves and validates on-disk paths; `CurrentCodeID` reads the environment symlink. Input validation mirrors OpenVox Server's own schemas. |
| `content` | Serves file bytes for a specific `(env, code_id, path)`, confined to the version dir via `os.Root`. |
| `seal` | Turns a staged tree into a deterministic `code_id` (hex hash) and a byte-identical gzipped tar artifact. Also extracts them — the one place untrusted bytes are parsed, so it is fuzzed. |
| `publish` | `Store` seals staged environments; `Server` serves versions + artifacts over mutual TLS. Also the provenance log. |
| `agent` | Compiler-side poll → fetch → verify → unpack → atomic symlink swap → reap. |
| `puppetca` | Builds TLS config from the Puppet CA material already on each node, including CRL enforcement. codavox issues no certificates. |
| `deploy` | Runs r10k to stage code, seals it, and signals the publisher to reseal. The single deploy path; every entry point funnels through it. |
| `deployserver` | The deploy control plane: token-authenticated deploy API and secret-authenticated webhook over one queue, one worker, one history. |
| `webhook` | Parses and authenticates GitHub, GitLab, and generic push payloads. Pure request mapping; holds no state. |
| `config` | Loads `/etc/codavox/config.yaml`. Deliberately not read by `code-id` or `code-content`. |
| `treegen` | Test-only; builds deterministic control-repo-shaped trees for benchmarks and the acceptance harness. |
| `testca` | Test-only; issues certs with the `pp_role` extension and signs CRLs. Nothing shipped imports it. |

Data flow: `publish` (primary) seals an r10k basedir and serves it → `agent`
(compiler) polls, fetches the artifact, unpacks to `versions/<env>_<code_id>/`,
and atomically swaps the `environments/<env>` symlink → `code-id` reads that
symlink, `code-content` serves files from the version dir.

## Load-bearing invariants

These are the reason the project exists. Weakening any of them needs a strong
argument, and each is defended by a test or a doc.

- **No fallbacks, ever.** A missing state file, an undeployed `code_id`, or an
  unreadable file is a hard error (exit 1). Never substitute a generated value,
  a timestamp, or content from a different version. Serving plausible-but-wrong
  content while exiting `0` is the exact failure static catalogs exist to
  prevent, and it fails silently.
- **`code-id` is a single symlink read.** OpenVox Server spawns it fresh on
  every static catalog compile with no caching (measured 83 ms Ruby → 3.2 ms).
  No git, directory walk, lock, or JSON parsing on that path.
  `BenchmarkCurrentCodeID` guards it.
- **The symlink is the only source of truth.** There is no separate state file.
  `code-id` derives from the same symlink OpenVox Server reads, so one
  `rename(2)` changes both what is served and what is reported at the same
  instant. Two sources of truth have no safe swap ordering.
- **Both commands are silent on success.** OpenVox Server logs anything written
  to stderr at ERROR level *even when the exit code is 0*, once per compile.
- **`code_id` charset is `[a-zA-Z0-9_\-:;]`** — no `/`, `.`, `+`, or `=`. Use
  hex digests; base64 is rejected by OpenVox Server at runtime.
- **Sealing must be reproducible.** Same tree in → same `code_id` and
  byte-identical artifact out, on any machine. Non-determinism churns every
  deploy and breaks content addressing.
- **Distribute resolved trees, never Puppetfiles.** r10k is not deterministic
  across time, so per-compiler resolution can never converge. codavox does not
  own the deploy; it observes an r10k basedir.
- **Revocation is enforced per request, not per handshake.** A revoked
  certificate stays cryptographically valid, so mutual TLS alone would keep
  admitting a revoked compiler forever. The publisher checks `$ssldir/crl.pem` —
  the same file every Puppet service reads — re-reads it when it changes, and
  applies it to **every request**. A handshake-only check is not enough: the
  agent polls over one keep-alive connection and never handshakes again, so
  revocation would not land until that connection dropped. A missing or
  unverifiable CRL is a startup error, never a silent downgrade to "nothing is
  revoked". Follows PE, which sets `ssl-crl-path` on every service listener.
- **Fleet visibility is self-reported, never inferred.** `/v1/compilers` and
  `codavox compilers` report what each agent said it is serving, read from its
  own environment symlink — the same one `code-id` reads — so the two must
  agree. The publisher also sees which artifacts it handed out, but a compiler
  that downloaded one can still have failed to verify or unpack it, so inferring
  convergence from downloads would confidently report a stale node as current.
  The view is in-memory and best effort by design: persisting it would create a
  second store of state the symlink already owns, to answer a diagnostic
  question.
- **One broken environment must not stop the others.** `Store.Reseal` and the
  agent's `Once` both isolate per-environment failures: the failed environment
  keeps its last good version and is reported, while the rest converge. Refusing
  to publish anything over one bad module turns a local problem into a
  fleet-wide outage. This is not a fallback — nothing is ever served under a
  `code_id` that does not describe it.

## Conventions

- Documentation follows the OpenVox documentation style guide (American English,
  serial comma, second person, active voice). Component names: **OpenVox
  Server**, **OpenVoxDB**, **OpenFact**, **OpenBolt**; literal service/path
  names keep their real spelling (`puppetserver`, `openvox-server`). Use
  `Puppet` only for the DSL or ecosystem as a whole.
- Unfamiliar-but-correct terms go in [cspell.json](cspell.json), not reworded.
- Commit messages explain **why**, not what — especially where an OpenVox Server
  contract constraint drove the design. A DCO sign-off hook lives in
  [.githooks/](.githooks/).
- Keep [docs/commands.md](docs/commands.md) current with every user-visible
  change; keep [docs/versioned-code-contract.md](docs/versioned-code-contract.md)
  cited to openvox-server source file and line, noting the verified commit.
