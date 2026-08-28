# Command reference

codavox is a single binary. Every subcommand is invoked as a short-lived
process.

- [`codavox code-id`](#codavox-code-id)
- [`codavox code-content`](#codavox-code-content)
- [`codavox compilers`](#codavox-compilers)
- [`codavox provenance`](#codavox-provenance)
- [`codavox version`](#codavox-version)
- [Exit codes](#exit-codes)
- [Wiring into puppetserver](#wiring-into-puppetserver)
- [On-disk layout](#on-disk-layout)
- [Environment variables](#environment-variables)

The compiler-side commands (`code-id`, `code-content`) and the operator commands
`compilers` and `provenance` are documented here. Every other subcommand has its own page:

| command | page |
|---|---|
| `codavox publish` | [publishing.md](publishing.md) |
| `codavox agent` | [agent.md](agent.md) |
| `codavox deploy` | [deploying.md](deploying.md) |
| `codavox deploy-server` (and its `webhook` alias) | [deploy-server.md](deploy-server.md) |
| `codavox seal` | [sealing.md](sealing.md) |

The operator commands share a config file, described in
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
a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed
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
$ codavox code-content production a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed manifests/site.pp
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
$ codavox code-content production a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed ../../../../etc/passwd
codavox: opening "../../../../etc/passwd" in a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed: openat ../../../../etc/passwd: path escapes from parent
```

---

## `codavox compilers`

```text
codavox compilers [--publisher <url>] [--certname <name>] [--ssldir <dir>]
                  [--state <dir>] [--json]
```

Prints what every compiler is serving. **Run it on the publisher**, whose own
certificate it uses to read the fleet view.

```console
$ codavox compilers
COMPILER                ENVIRONMENT  CODE_ID       COMMIT        LAST POLL
compiler01.example.com  production   3224ddbe7e3d  a3f1c9e4b2d8  12s ago
compiler01.example.com  testing      9a1f0c4e2b8d  8c02be71f4d9  12s ago
compiler02.example.com  production   7b05ff282795  61d70aa9c3e5  9m0s ago
```

This is the question a fleet cannot otherwise answer: are my compilers on the
current code? Without it you would run `codavox code-id` on every node in turn,
which is fine at four compilers and useless at forty.

### Reading the two id columns

`CODE_ID` is a content hash of the resolved tree — **not a git commit**. It
answers "are these two compilers serving identical code?" exactly, which a
commit cannot: r10k resolves a `Puppetfile` differently at different times, so
the same commit deployed twice can produce different content, and a commit that
changes only a README produces identical content. The `code_id` is what OpenVox
Server pins each static catalog to, so it is the id that has to be exact.

`COMMIT` is the control-repo commit that produced it, which is the id *you*
recognize. codavox reads it from r10k's own `.r10k-deploy.json` at seal time and
records it in the publisher's provenance log; this command joins the two locally,
so you get both without running
[`codavox provenance`](#codavox-provenance) per row.

Reading a row across, then: compiler02 is serving content that came from commit
`61d70aa9c3e5`, and compiler01 is not — so a deploy has landed on one and not the
other.

A `-` in `COMMIT` means no provenance was recorded for that `code_id`. That is
not an error: provenance is best-effort, and a missing record is reported
honestly rather than filled in from a different version. Both ids are shortened
for the table; `--json` carries them in full.

### It is each compiler's own answer

The `code_id` in each row is what that compiler reported about *itself*, read
from the same environment symlink its `code-id` reads. So `codavox compilers`
here and `codavox code-id` there answer the same question and must agree.

That is a stronger claim than it sounds. The publisher also knows which
artifacts it handed out, but a compiler that downloaded one can still have
failed to verify or unpack it, and would then go on serving the previous
version. Inferring convergence from downloads would report that node as current.
Asking it what it is serving does not.

What the two cannot share is freshness. A report is as old as that compiler's
last poll, which is why `LAST POLL` is on every row: a compiler that stopped
polling an hour ago is reporting what it was serving an hour ago.

Two rows are worth reading carefully:

- **`(not reported)`** — the compiler is polling but said nothing. Either it has
  nothing deployed yet, or it is running an agent older than this feature. It is
  listed rather than hidden, because an incomplete view should be visible.
- **A compiler that is missing entirely** has not polled since the publisher
  started. The view is in memory and best effort, so a publisher restart empties
  it; a healthy fleet refills it within one poll interval.

### `--json`

For monitoring. It carries both ids **at full length** — a shortened id cannot
be compared exactly — plus the fetch history and counters the table leaves out:

```json
[
  {
    "certname": "compiler01.example.com",
    "last_seen": "2026-07-25T18:33:48Z",
    "last_poll": "2026-07-25T18:33:48Z",
    "serving": {
      "production": "7b05ff28279c54d252387a522beee5a434c234713c8c8c545ee34bc531930d3a"
    },
    "serving_at": "2026-07-25T18:33:48Z",
    "fetched": {
      "production": {
        "code_id": "7b05ff28279c54d252387a522beee5a434c234713c8c8c545ee34bc531930d3a",
        "at": "2026-07-25T18:33:48Z"
      }
    },
    "polls": 240,
    "fetches": 3,
    "commits": {
      "production": "addb2d6b638117234da99bc97c18bc8a0f069dfe"
    }
  }
]
```

This is [`GET /v1/compilers`](publishing.md#get-v1compilers)'s own shape plus
`commits`, which this command resolves locally. So a check written against the
endpoint reads this output unchanged, and one written against this output does
not break when pointed at the endpoint — it simply sees no commits. An
environment with no recorded provenance is **absent from `commits`** rather than
present and empty.

A useful check is "every compiler reports the same `code_id` for the
environment, and polled recently". Note the difference between `serving` and
`fetched`: the first is what the compiler said about itself, the second is what
the publisher watched it download.

### Other options

`--publisher` overrides the URL (default
`https://<certname>:<port from publish.listen>`); `--state` overrides where the
provenance log is read from (default `<root>/state`). An empty fleet is not an
error: the command says so on stderr and exits `0`.

The same data is available directly at
[`GET /v1/compilers`](publishing.md#get-v1compilers).

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
5f2e9c19eb03db14be61df036f5b2af3e377f290    deployed 2026-07-24 12:00:00 -0400    sealed 2026-07-24T16:00:00Z
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

**Let the agent expire the environment cache.** With `environment_timeout` set
— and a production compiler sets it to `unlimited` — the server keeps compiling
the tree it already parsed after the agent swaps the symlink, and stamps those
catalogs with the new `code_id`. So after every swap the agent sends
`DELETE /puppet-admin-api/v1/environment-cache?environment=<env>` to the server
on this node, and the shipped `auth.conf` refuses that. Add this rule to
`/etc/puppetlabs/puppetserver/conf.d/auth.conf` before wiring the commands:

```hocon
{
    match-request: {
        path: "/puppet-admin-api/v1/environment-cache"
        type: path
        method: delete
    }
    # pp_role, by OID — see below for why not by name.
    allow: { extensions: { "1.3.6.1.4.1.34380.1.1.13": "openvox_compiler" } }
    sort-order: 200
    name: "codavox environment cache flush"
}
```

The OID is `pp_role`; the short name does not resolve on a compiler, whose CA
service is disabled. A node whose certificate has no `pp_role` is admitted by
certname instead. See [Expiring the environment
cache](agent.md#expiring-the-environment-cache) for the reasons, what happens
when the flush fails, and when to turn it off.

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
