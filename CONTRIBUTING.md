# Contributing to codavox

## Development

Requires Go 1.26.

```console
go build ./cmd/codavox
go test -race ./...
golangci-lint run ./...
gofmt -l .
```

CI runs all of the above plus markdownlint and the benchmarks, and
cross-compiles `linux/amd64` and `linux/arm64`. Run them locally before opening
a pull request.

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
