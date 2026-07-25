# Contributing to codavox

## Development

Requires Go 1.26.

```console
go build ./cmd/codavox
go test -race ./...            # -race matters: the agent swaps while code-id reads
golangci-lint run ./...        # CI pins v2.12.2
gofmt -l .                     # must print nothing
```

CI runs those, and four more gates that are easy to forget locally because they
are not `go` commands:

```console
go test -run '^$' -fuzz FuzzExtractArchive -fuzztime 60s ./internal/seal/
find . -name '*.sh' -not -path './.git/*' -print0 | xargs -0 shellcheck --external-sources
npx cspell '**/*.md'           # docs only; add terms to cspell.json, do not reword
govulncheck ./...
```

It also runs markdownlint and the benchmarks, and cross-compiles
`linux/amd64` and `linux/arm64`. Run all of it locally before opening a pull
request.

Beyond that, **anything touching TLS, the agent's HTTP client, packaging, or the
systemd units needs the integration harness too** — see
[Integration testing](#integration-testing) below for why the Go tests cannot
cover those.

### Testing the commands by hand

`CODAVOX_ROOT` and `CODAVOX_ENVIRONMENTPATH` override the deployment paths, so
you can exercise both commands without root or a real OpenVox Server. `code-id`
reads the environment symlink — there is no state file — so create one pointing
at a version directory:

```console
export CODAVOX_ROOT=/tmp/codavox-root
export CODAVOX_ENVIRONMENTPATH="$CODAVOX_ROOT/environments"
mkdir -p "$CODAVOX_ROOT/versions/production_abc123/manifests" "$CODAVOX_ENVIRONMENTPATH"
echo 'node default { }' > "$CODAVOX_ROOT/versions/production_abc123/manifests/site.pp"
ln -s "$CODAVOX_ROOT/versions/production_abc123" "$CODAVOX_ENVIRONMENTPATH/production"

go run ./cmd/codavox code-id production                               # -> abc123
go run ./cmd/codavox code-content production abc123 manifests/site.pp
```

### Integration testing

The Go tests cover a great deal, but they cannot cover the
`versioned-code-service` contract itself: that contract is with a live
puppetserver, running as a different user, on a real filesystem, holding real
Puppet certificates. [`test/integration/`](test/integration/) stands up a
two-node OpenVox topology in Docker and exercises the whole chain against it.

```console
./test/integration/run.sh          # build HEAD, provision, test, tear down
KEEP=1 ./test/integration/run.sh   # leave the topology up for inspection
```

Requires Docker and [GoReleaser](https://goreleaser.com); takes a few minutes.
See [test/integration/README.md](test/integration/README.md) for the topology,
what each feature asserts, and how to debug a failed run.

CI runs it for you on a pull request that changes a workflow, the packaging, or
the harness itself — the paths where nothing else would exercise the change. It
always runs on push to `main`.

**Run it yourself before a pull request that touches TLS, the agent's HTTP
client, packaging, or the systemd units.** Those are the areas where a change can pass
every Go test and still be broken in production, because the tests do not
reproduce the shape of the deployed system:

| the tests do this | production does this |
|---|---|
| `agent --once` — a fresh process, so a fresh connection, per sync | a long-lived daemon reusing one keep-alive connection for hours |
| run as one user, in a temp directory | agent as root, OpenVox Server as `puppet`, in `/opt/puppetlabs` |
| link a Go library into the test binary | install an rpm, enable a unit, log to the journal |

Each column has produced a real bug. Certificate revocation checked only during
the TLS handshake passed every Go test and still served a revoked compiler
indefinitely, because the running agent never handshakes again. Version
directories left at mode `0700` failed every catalog compile with `EACCES`.
Neither is visible without a real server.

## Design rules

Two rules are load-bearing. Changes that weaken either need a strong argument.

**No fallbacks.** A missing environment link, an undeployed `code_id`, or an
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

Commits are **DCO signed-off** and **GPG-signed**, the way OpenVox projects
expect. A `prepare-commit-msg` hook appends the `Signed-off-by` trailer for you;
enable it once per clone, and turn on commit signing:

```console
git config core.hooksPath .githooks
git config commit.gpgsign true
```

The hook adds the trailer only when one is not already present, so `git commit
-s` still works. Signing needs a GPG key registered with your forge account.

## Documentation

The [README](README.md) links every document; keep them current as the code
changes. Three carry specific obligations:

| document | obligation |
|---|---|
| [docs/commands.md](docs/commands.md) | Update with every user-visible change to a command |
| [docs/configuration.md](docs/configuration.md) | Update when a flag or config setting changes |
| [docs/versioned-code-contract.md](docs/versioned-code-contract.md) | Cite the openvox-server source file and line, and note the commit the claim was verified against — not the documentation |
