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
{ find . -name '*.sh' -not -path './.git/*'; find .githooks -type f; } | tr '\n' '\0' | xargs -0 shellcheck --external-sources
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

CI runs it for you on a pull request that changes a workflow, the packaging,
the harness itself, or the deploy path and the agent (`internal/deploy`,
`internal/deployserver`, `internal/webhook`, `internal/agent`) — the paths
where nothing else exercises the change against a real r10k or a real
puppetserver. It always runs on push to `main`.

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

### Cutting a release

The Docker harness runs on every push to `main`, but it cannot stand in for the
[lab](https://github.com/miharp/codavox-lab): real r10k, a real CA with a
revoked compiler, a real puppetserver holding its environment cache. The lab
pins a package, and for a long time that meant a release URL — so its audit
suite could only run after a tag, and the first time it ran it found a release
blocker in something already published.

So a release is the last step, not the first:

1. In the lab, `./scripts/use-snapshot` builds `main` with GoReleaser and
   commits a pin to the snapshot; bring the VMs up fresh and run
   `bash audit/run.sh`. The record it commits carries the snapshot's version
   and the codavox commit it was built from.
2. Bump `VERSION=` in [README.md](README.md) and
   [docs/installation.md](docs/installation.md), and add an upgrade note to
   [docs/production.md](docs/production.md) if the release asks anything of an
   existing estate. The release workflow refuses a tag those files do not
   document.
3. Tag `vX.Y.Z` on `main` and push it. The workflow tests, packages, and
   publishes the release. The package repository at
   <https://packages.harpworks.org> picks it up within the hour, or at once
   with `gh workflow run repository.yml -R miharp/packages`; check the new
   version shows there.
4. In the lab, `./scripts/use-release X.Y.Z` returns the pin to the published
   package.

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
expect. A `prepare-commit-msg` hook appends the `Signed-off-by` trailer for you.
Enable it once per clone, and turn on commit signing:

```console
git config core.hooksPath .githooks
git config commit.gpgsign true
```

Signing needs a GPG key registered with your forge account.

### Crediting a co-author

Work done with an AI assistant carries a `Co-Authored-By` trailer. Configure the
identity once per clone and the hook adds it:

```console
git config codavox.coauthor "Co-Authored-By: <name> <email>"
```

Use whatever identity your assistant documents for itself — there is no shared
registry of these, and vendors differ. The hook accepts any value and validates
none, so it works the same for any tool. `--add` sets more than one, to credit a
person and a tool on the same commit.

Leave it unset and only the sign-off is added: the hook never claims a co-author
you did not have, which is the reason it is opt-in rather than built in.

Both trailers are appended as one contiguous block, because git stops parsing
trailers at a blank line — a gap would turn one block it understands into two it
does not. Each is added only when absent, so `git commit -s` still works and
re-running the hook never duplicates one.

## Documentation

The [README](README.md) links every document; keep them current as the code
changes. Three carry specific obligations:

| document | obligation |
|---|---|
| [docs/commands.md](docs/commands.md) | Update with every user-visible change to a command |
| [docs/configuration.md](docs/configuration.md) | Update when a flag or config setting changes |
| [docs/versioned-code-contract.md](docs/versioned-code-contract.md) | Cite the openvox-server source file and line, and note the commit the claim was verified against — not the documentation |
