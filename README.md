# codavox

Versioned code distribution for OpenVox compilers, built on puppetserver's
open-source `versioned-code-service` hook.

> A *coda* is the passage that brings every performance to the same close.

**Status: early development.** The full chain works and has been run against
real OpenVox Server processes: `codavox deploy` runs r10k and seals, two
compilers converge over mutual TLS, a compiler catches up after missing a
deploy, and an agent receives a static catalog stamped with the codavox
`code_id`. See [test/integration](test/integration/) and the
[implementation plan](docs/implementation-plan.md).

If you are coming from Puppet Enterprise, codavox is the missing Code Manager
piece for OpenVox: `codavox deploy production` is the same verb as
`puppet-code deploy production`, on the same staging-to-compilers model.

## Why

Getting Puppet code onto compilers currently means webhooks (push,
fire-and-forget, so a compiler that was down misses the event permanently),
NFS (a single point of failure for catalog compilation itself, with no
atomicity), or rsync (not atomic, not versioned, no notion of which version is
live).

All three distribute *code*. None distributes *identity* — nothing in those
systems can answer "which exact version is this compiler serving?", so
divergence cannot even be detected, let alone corrected.

codavox distributes resolved code artifacts addressed by a `code_id`. Compilers
converge on their own, and agents get file content consistent with the catalog
they were served, even if code changes mid-run.

## Quickstart

On the primary, serve staged code and deploy it — the deploy verb is the one
you already know from `puppet-code`:

```console
# run the publisher (as a service), pointed at r10k's basedir
$ codavox publish --staging /etc/puppetlabs/code-staging

# deploy: runs r10k, seals, and serves — blocking until it is live
$ codavox deploy production --wait --staging /etc/puppetlabs/code-staging
production    deployed    a3f1c9e4b2d8    (commit 5f2e9c1)    serving
```

On each compiler, run the agent and point OpenVox Server at codavox's commands:

```console
# converge this compiler onto whatever the publisher serves (as a service)
$ codavox agent --publisher https://puppet.example.com:8150

# OpenVox Server then answers "which version am I serving?" in one symlink read
$ codavox code-id production
a3f1c9e4b2d8
```

For a control loop, run `codavox deploy-server` on the primary: a
token-authenticated deploy API (`POST /v1/deploys`, with status and history) and
a push webhook, so CI drives deploys and a control-repo push deploys the matching
environment — the way Code Manager's API and webhook do.

Compilers **poll**, so one that was down catches up on its own. See
[deploying.md](docs/deploying.md), [deploy-server.md](docs/deploy-server.md),
[publishing.md](docs/publishing.md), and [agent.md](docs/agent.md).

## Quick look

```console
$ codavox code-id production
a3f1c9e4b2d8

$ codavox code-content production a3f1c9e4b2d8 manifests/site.pp
node default { }

$ codavox code-content production notdeployed manifests/site.pp
codavox: code version not deployed: notdeployed at /opt/puppetlabs/codavox/versions/production_notdeployed
$ echo $?
1
```

That last case is the point. A version that is not deployed is an **error**,
never a silent fall back to whatever is current.

## Documentation

| document | contents |
|---|---|
| [configuration.md](docs/configuration.md) | The shared config file: location, precedence, and every setting |
| [deploying.md](docs/deploying.md) | Running `codavox deploy`, r10k invocation, the reseal trigger, `--wait` |
| [deploy-server.md](docs/deploy-server.md) | The deploy API and push webhook; deploy status and history |
| [agent.md](docs/agent.md) | Running the compiler-side agent, verification, atomic swap, reaping |
| [publishing.md](docs/publishing.md) | Running the publisher, mutual TLS, and the role constraint |
| [sealing.md](docs/sealing.md) | How a code_id is derived, what is excluded, and why |
| [commands.md](docs/commands.md) | Command reference, exit codes, puppetserver wiring, on-disk layout |
| [design.md](docs/design.md) | Architecture, transport options, repo layout, known hard parts |
| [implementation-plan.md](docs/implementation-plan.md) | Phased build order, test topology, integration tests |
| [versioned-code-contract.md](docs/versioned-code-contract.md) | The verified puppetserver interface and its validation rules |

## Design constraints worth knowing

- **`code-id` runs on every static catalog compile, uncached.** OpenVox Server
  spawns it fresh each time. Measured against a Ruby-based equivalent:
  **83 ms → 3.2 ms per invocation**, of which almost all the remainder is
  process spawn (the work itself is ~14 µs). This is why the compiler-side
  components are a compiled binary rather than a script.
- **No fallbacks.** A missing state file or undeployed `code_id` fails loudly.
  Serving plausible-but-wrong content while exiting `0` is the failure mode
  static catalogs exist to prevent.
- **`code_id` accepts only `[a-zA-Z0-9_\-:;]`** — no `/`, `.`, `+` or `=`. Use
  hex digests; base64 will be rejected by OpenVox Server at runtime.
- **r10k is not deterministic across time.** A Puppetfile with `:latest` or a
  branch ref resolves differently later, so per-compiler resolution can never
  converge. codavox distributes resolved trees, never Puppetfiles.

## Building

```console
go build ./cmd/codavox
go test ./...
```

Requires Go 1.26. `linux/amd64` and `linux/arm64` are both first-class targets.

## License

[Apache-2.0](LICENSE)
