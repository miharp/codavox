# Command reference

codavox is a single binary. Every subcommand is invoked as a short-lived
process.

- [`codavox code-id`](#codavox-code-id)
- [`codavox code-content`](#codavox-code-content)
- [`codavox provenance`](#codavox-provenance)
- [`codavox version`](#codavox-version)
- [Exit codes](#exit-codes)
- [Wiring into puppetserver](#wiring-into-puppetserver)
- [On-disk layout](#on-disk-layout)
- [Environment variables](#environment-variables)

The compiler-side commands (`code-id`, `code-content`) and the operator command
`provenance` are documented here. The daemons `publish`, `agent`, and
`deploy-server`, and the `deploy` command, have their own pages:
[publishing.md](publishing.md), [agent.md](agent.md),
[deploy-server.md](deploy-server.md), and [deploying.md](deploying.md). The
operator commands share a config file, described in
[configuration.md](configuration.md).

---

## `codavox code-id`

```text
codavox code-id <environment>
```

Prints the `code_id` currently deployed for an environment, by reading the
environment symlink.

OpenVox Server runs this **on every static catalog compile**, as a fresh
process, with no caching. It is therefore a single `readlink` — no git
invocation, no directory walk, no lock, no JSON parsing.

```console
$ codavox code-id production
a3f1c9e4b2d8
```

**It never invents a value.** If the environment link is missing, is a real
directory, or points somewhere outside the version layout, the command fails. Emitting a generated
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
$ codavox code-content production a3f1c9e4b2d8 ../../../../etc/passwd
codavox: opening "../../../../etc/passwd" in a3f1c9e4b2d8: openat ../../../../etc/passwd: path escapes from parent
```

---

## `codavox provenance`

```text
codavox provenance <environment> <code-id> [--state <dir>] [--json]
```

Prints the control-repo commit that produced a `code_id`, read from the
publisher's local provenance log. **Run it on the publisher** — it reads
`<state>/provenance.jsonl` directly and does no network I/O.

```console
$ codavox provenance production 3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb
a3f1c9e4b2d8    deployed 2026-07-24 12:00:00 -0400    sealed 2026-07-24T16:00:00Z
```

The usual troubleshooting path: read a puzzling compiler's version with
`codavox code-id`, then ask the publisher which commit that content came from.

Because a commit that leaves resolved content unchanged seals to the same
`code_id`, one id can list several commits, printed most recently sealed first.

`--json` emits the records as an array for scripting. `--state` overrides the
state directory (default `<root>/state`, honoring `CODAVOX_ROOT`).

A `code_id` with no recorded provenance is not an error: the command prints
`no provenance recorded` to stderr and exits `0`, because provenance is
best-effort and its absence must never be dressed up as a different version's
history. See [publishing.md](publishing.md#provenance) for how records are
captured.

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

Static catalogs must be on for versioned content to take effect. The
`static_catalogs` setting defaults to `true`, so this is usually already the
case; set it explicitly only if it was turned off:

```ini
[server]
static_catalogs = true
```

Note that OpenVox Server asks for a `code_id` on every catalog compile
regardless — but with no `code-id-command` configured, it gets nothing back and
the versioning is inert. Wiring the two commands above is what makes static
catalogs actually do their job.

See [versioned-code-contract.md](versioned-code-contract.md) for the full
verified interface.

---

## On-disk layout

```text
/opt/puppetlabs/codavox/
  versions/<env>_<code_id>/     unpacked environment trees

/opt/puppetlabs/codavox/environments/<env>
    -> /opt/puppetlabs/codavox/versions/<env>_<code_id>
```

**The symlink is the only source of truth.** `code-id` reports what the link
resolves to, so there is no separate state file to fall out of step with it.

That is a correctness requirement, not a simplification. OpenVox Server does
two independent things when compiling a static catalog: it reads the
environment directory, and it runs `code-id-command`. If those consulted
different sources, every deploy would have a window where they disagreed — a
catalog compiled from one version but stamped with another, whose file content
then resolves against the wrong tree. No ordering avoids it: swap the link
first and the id lags the tree; write the file first and the tree lags the id.

Reading the link makes both answers the same fact. A single `rename(2)` changes
what OpenVox Server serves and what `code-id` reports at the same instant.

Old version directories are retained so `code-content` can answer for a
`code_id` an in-flight agent run is still using.

---

## Environment variables

| variable | default | purpose |
|---|---|---|
| `CODAVOX_ROOT` | `/opt/puppetlabs/codavox` | Override the deployment root. Intended for tests and unprivileged runs, not production. |
| `CODAVOX_ENVIRONMENTPATH` | `/opt/puppetlabs/codavox/environments` | Override OpenVox Server's environmentpath. |

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
