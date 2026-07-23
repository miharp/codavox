# Command reference

codavox is a single binary. Every subcommand is invoked as a short-lived
process.

- [`codavox code-id`](#codavox-code-id)
- [`codavox code-content`](#codavox-code-content)
- [`codavox version`](#codavox-version)
- [Exit codes](#exit-codes)
- [Wiring into puppetserver](#wiring-into-puppetserver)
- [On-disk layout](#on-disk-layout)
- [Environment variables](#environment-variables)

Commands not yet implemented (`publish`, `agent`) are tracked in the
[implementation plan](implementation-plan.md).

---

## `codavox code-id`

```text
codavox code-id <environment>
```

Prints the `code_id` currently deployed for an environment.

OpenVox Server runs this **on every static catalog compile**, as a fresh process,
with no caching. It is therefore a single `open`+`read` of a state file — no
git invocation, no directory walk, no lock, no JSON parsing.

```console
$ codavox code-id production
a3f1c9e4b2d8
```

**It never invents a value.** If the state file is missing, empty, or contains
something OpenVox Server would reject, the command fails. Emitting a generated
or timestamp-derived `code_id` would silently break content addressing — every
compile would produce a different version, and no historical lookup could ever
succeed — while appearing to work.

The output is validated against OpenVox Server's `CodeId` schema before it is
printed, so a malformed id fails here with a clear message rather than as an
`IllegalStateException` inside the JVM.

---

## `codavox code-content`

```text
codavox code-content <environment> <code-id> <file-path>
```

Streams the contents of a file **as of a specific deployed code version** to
stdout. OpenVox Server runs this for each `static_file_content` request.

```console
$ codavox code-content production a3f1c9e4b2d8 manifests/site.pp
node default { }
```

A leading `/` on `<file-path>` is accepted and ignored; the path is always
resolved relative to the version directory.

### It never falls back

If the requested `code_id` is not deployed on this node, the command **fails**.
It does not serve the current version instead.

This is the single most important behavioral guarantee in codavox. Serving
content from a different version than the catalog was compiled against, while
exiting `0`, produces an agent run that applies a mixture of two code versions
and reports success. That is precisely the failure static catalogs exist to
prevent.

```console
$ codavox code-content production notdeployed manifests/site.pp
codavox: code version not deployed: notdeployed at /opt/puppetlabs/codavox/versions/production_notdeployed
$ echo $?
1
```

### Path confinement

Both inputs are untrusted. `<file-path>` originates from the agent and is
passed through by OpenVox Server, and the version tree is unpacked from a
downloaded artifact.

Resolution is confined to the version directory using Go's `os.Root`, which
blocks `..` traversal, absolute paths, **and symlinks pointing outside the
tree**. That last case matters: without it, a symlink inside a deployed tree
would become an arbitrary file read on every compiler.

```console
$ codavox code-content production a3f1c9e4b2d8 ../../state/production.codeid
codavox: opening "../../state/production.codeid" in a3f1c9e4b2d8: openat ...: path escapes from parent
```

---

## `codavox version`

```text
codavox version
```

Prints the build version. Reports `dev` for builds without version metadata.

---

## Exit codes

| code | meaning |
|---|---|
| `0` | success |
| `1` | runtime failure — invalid input, missing state, undeployed version, unreadable file |
| `2` | usage error — unknown subcommand or wrong argument count |

**Both commands are silent on success.** This is a requirement, not a style
choice: OpenVox Server logs anything a versioned-code command writes to stderr
at `ERROR` level *even when the exit code is zero*. A command that chatters on
success fills the server log at one line per catalog compile.

---

## Wiring into puppetserver

`/etc/puppetlabs/puppetserver/conf.d/versioned-code.conf`:

```hocon
versioned-code: {
  code-id-command: "/usr/bin/codavox-code-id"
  code-content-command: "/usr/bin/codavox-code-content"
}
```

OpenVox Server passes only positional arguments, so neither setting can point
at `/usr/bin/codavox` directly — it would be invoked as `codavox production`,
with no subcommand.

codavox dispatches on `argv[0]`, so the package ships symlinks:

```text
/usr/bin/codavox-code-id      -> codavox
/usr/bin/codavox-code-content -> codavox
```

A shell wrapper would also work, but it would add a shell fork to a path that
runs on every catalog compile. A symlink costs nothing. The binary still
accepts `codavox code-id <env>` directly for interactive use.

**Both settings must be set, or neither.** OpenVox Server's `validate-config!`
throws at startup if exactly one is present:

> Only one of "versioned-code.code-id-command" and
> "versioned-code.code-content-command" was set. Both or neither must be set
> for the versioned-code-service to function correctly.

Static catalogs also need enabling in `puppet.conf`:

```ini
[server]
static_catalogs = true
```

See [versioned-code-contract.md](versioned-code-contract.md) for the full
verified interface.

---

## On-disk layout

```text
/opt/puppetlabs/codavox/
  versions/<env>_<code_id>/     unpacked environment trees
  state/<env>.codeid            single line, the deployed code_id

/etc/puppetlabs/code/environments/<env>
    -> /opt/puppetlabs/codavox/versions/<env>_<code_id>
```

The environment path is a symlink, swapped atomically on deploy. Old version
directories are retained so `code-content` can answer for a `code_id` an
in-flight agent run is still using.

---

## Environment variables

| variable | default | purpose |
|---|---|---|
| `CODAVOX_ROOT` | `/opt/puppetlabs/codavox` | Override the deployment root. Intended for tests and unprivileged runs, not production. |

---

## Input validation

Both commands validate against OpenVox Server's own schemas before touching the
filesystem, so bad input fails early with a clear message.

| input | accepted | notes |
|---|---|---|
| environment | `^\w+$` | alphanumerics and `_`. Agrees with r10k, which sanitizes `\W` to `_`. |
| code_id | `^[a-zA-Z0-9_\-:;]+$` | **Excludes `/`, `.`, `+`, `=`.** Use hex digests — a base64 digest will be rejected. |

The base64 exclusion is the easy mistake: `+` and `=` are both valid base64 and
both rejected, so a digest that looks fine in testing fails at runtime.
