# Contributing to codavox

## Development

Requires Go 1.26.

```console
go build ./cmd/codavox
go test -race ./...
golangci-lint run ./...
gofmt -l .
```

CI runs all of the above plus markdownlint, and cross-compiles `linux/amd64`
and `linux/arm64`. Run them locally before opening a pull request.

### Testing the commands by hand

`CODAVOX_ROOT` overrides the deployment root, so you can exercise both commands
without root or a real OpenVox Server:

```console
export CODAVOX_ROOT=/tmp/codavox-root
mkdir -p "$CODAVOX_ROOT"/{state,versions/production_abc123/manifests}
echo abc123 > "$CODAVOX_ROOT/state/production.codeid"
echo 'node default { }' > "$CODAVOX_ROOT/versions/production_abc123/manifests/site.pp"

go run ./cmd/codavox code-id production
go run ./cmd/codavox code-content production abc123 manifests/site.pp
```

## Design rules

Two rules are load-bearing. Changes that weaken either need a strong argument.

**No fallbacks.** A missing state file, an undeployed `code_id`, or an
unreadable file is an error. Never substitute a generated value or content from
a different version. Serving plausible-but-wrong content while exiting `0` is
the exact failure static catalogs exist to prevent, and it fails silently.

**`code-id` stays a single read.** OpenVox Server spawns it on every static
catalog compile with no caching. No git invocation, directory walk, lock, or
parsing belongs on that path. `BenchmarkCurrentCodeID` guards this.

## Writing style

Documentation follows the
[OpenVox documentation style guide](https://github.com/OpenVoxProject/openvox-docs/blob/master/CONTRIBUTING.md).
The points that come up most often here:

- **American English**: `behavior`, `normalize`, `sanitize`
- **Serial comma**: "Unix, Linux, and Windows"
- Second person, active voice, concise but not terse
- Avoid patronizing words: *clearly*, *actually*, *obviously*
- Avoid idioms and metaphors that do not translate across languages
- File names, paths, commands, and code in `monospace`
- Fenced code blocks always carry a language identifier; use `console` for
  terminal commands, `text` for layouts and output
- Component naming: **OpenVox Server**, **OpenVoxDB**, **OpenFact**,
  **OpenBolt**. Use `Puppet` only for the DSL or the ecosystem as a whole.
  Literal service names, paths, and package names keep their real spelling
  (`puppetserver`, `/etc/puppetlabs/puppetserver/`, `openvox-server`).

Unfamiliar-but-correct terms go in `cspell.json` rather than being reworded.

## Commits

Commit messages explain **why**, not what — the diff already shows what
changed. Include the reasoning that would otherwise be lost, especially where a
constraint from OpenVox Server's contract drove the design.

> **Open question:** OpenVox projects require GPG-signed (`-S`) and DCO
> signed-off (`-s`) commits. codavox does not enforce this yet. If it is ever
> contributed to OpenVoxProject, retrofitting sign-off across existing history
> means a rebase, so adopting it early is cheaper than adopting it later.

## Documentation

Keep these current as the code changes:

| document | contents |
|---|---|
| [README.md](README.md) | What it is, why, quick look |
| [docs/commands.md](docs/commands.md) | Command reference — update with every user-visible change |
| [docs/design.md](docs/design.md) | Architecture and rationale |
| [docs/implementation-plan.md](docs/implementation-plan.md) | Phased build order and test topology |
| [docs/versioned-code-contract.md](docs/versioned-code-contract.md) | The verified OpenVox Server interface |

The contract document records behavior verified by reading openvox-server
source. When updating it, cite the file and line rather than the
documentation, and note the commit the claim was verified against.
