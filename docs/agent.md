# Agent

The agent runs on each compiler, polling the publisher and converging local
code onto whatever it advertises.

```console
codavox agent --publisher https://puppet.example.com:8150
```

Run a single reconciliation and exit — useful from a systemd timer or for
testing:

```console
codavox agent --publisher https://puppet.example.com:8150 --once
```

## Options

| flag | default | purpose |
|---|---|---|
| `--publisher` | *required* | Publisher base URL |
| `--once` | off | Reconcile once and exit |
| `--interval` | `30s` | Poll interval |
| `--certname` | system hostname | This node's Puppet certname |
| `--ssldir` | `/etc/puppetlabs/puppet/ssl` | Puppet SSL directory |
| `--environmentpath` | `/opt/puppetlabs/codavox/environments` | Where environment links live |
| `--keep` | `3` | Superseded versions retained per environment |
| `--min-age` | `2h` | Minimum retention regardless of `--keep` |
| `--max-unpacked` | `2G` | Most one artifact may expand to on disk; past it, the artifact is refused |
| `--prune-environments` | off | Remove environments the publisher no longer serves |
| `--puppetserver` | `https://<certname>:8140` | The OpenVox Server on this node whose environment cache to expire |
| `--flush-environment-cache` | `true` | Expire the environment in that server's cache after every swap |

## What a reconciliation does

1. Poll `/v1/environments` for the current `code_id` of each environment.
2. Skip any environment already at that version.
3. Fetch `GET /v1/artifact/{environment}/{code_id}` over the same mutual TLS
   as the poll, and extract it to a temporary directory.
4. **Verify by resealing the extracted tree** and comparing to the requested
   `code_id`.
5. Rename the temporary directory into place.
6. Atomically swap the environment symlink.
7. Reap superseded versions.
8. Once every environment is in place, tell this node's OpenVox Server to
   expire each one that moved from its environment cache.

## codavox owns its codedir

codavox manages `/opt/puppetlabs/codavox/environments`, **not** the stock
`/etc/puppetlabs/code/environments`. Point OpenVox Server at it:

```console
puppet config set --section main environmentpath /opt/puppetlabs/codavox/environments
```

This is not a preference. A freshly installed OpenVox Server ships a populated
skeleton at `code/environments/production` — `data`, `environment.conf`,
`hiera.yaml`, `manifests`, `modules` — and `rename(2)` cannot replace a real
directory with a symlink. Managing that path would mean either refusing to
start or moving an operator's directory aside on first run, and that directory
may hold code deployed by other means.

Owning a separate codedir avoids the collision entirely, and leaves the stock
path untouched for anyone still using it.

**PE does the same thing, one level up.** With versioned deploys enabled it
moves `codedir` itself from `/etc/puppetlabs/code` to
`/etc/puppetlabs/puppetserver/code`, a symlink its file-sync client points at
the current versioned directory
(`puppet_enterprise::profile::master`, lines 158–159 and 429 of
`profile/master.pp`, verified in `pe-modules 2025.11.0.51`). Turning versioned
deploys back off replaces that symlink with one back to `/etc/puppetlabs/code`.
So the stock codedir is not where versioned code is served from in PE either —
serving a directory you swap atomically and letting the deploy tool own the
default path are the same requirement in both designs.

The difference is granularity: PE swaps one symlink for the whole codedir,
codavox swaps one per environment. That is what lets a single broken environment
keep its last good version while the others converge.

## Pull, not push

Polling is the correctness mechanism, not an optimization.

A webhook is fire-and-forget: a compiler that is unreachable when a deploy
happens misses the event permanently and stays stale until somebody notices. A
polling agent has no such state to lose — it compares what it has against what
the publisher advertises and closes the gap, whether it missed one deploy or
twenty.

A publisher outage degrades to *no new deploys*. The compiler keeps serving the
version it already has, so catalogs continue to compile. That is the property
that makes this preferable to a shared filesystem, where losing the server
means losing catalog compilation entirely.

Polls are jittered by up to 25% of the interval. Without it, a fleet restarted
together polls in lockstep forever.

## The fetch is streamed, not staged

The response body is piped straight into the extractor — nothing is written to
disk as a plain file first. There is no intermediate copy of the artifact
sitting around to be inspected or retried from; the only things that ever
touch disk are the temporary extraction directory and, on success, the final
version directory. What happens to that temporary directory if the process
dies mid-extraction is covered under [Reaping](#reaping).

The body is a gzipped tar (`Content-Type: application/gzip`). That gzip is the
archive format itself, produced once at seal time — not an HTTP
`Content-Encoding` the transport adds on top.

## Extraction is bounded

Every file in an artifact is bounded by its own declared size, but the sum is
what fills a disk. A gzip stream of zeros compresses about 1000:1, so a
megabyte of artifact could expand to gigabytes, and every compiler pulls the
same artifact at once — so a publisher serving such a thing, compromised or
merely wrong, would fill the whole fleet's disks in one poll.
[Verification](#verification-is-by-resealing-not-by-checksum) would refuse
the tree, but only after it was on disk, which is too late for a tree whose
point is its size.

So extraction refuses, before writing the byte that would cross it, any
artifact that expands past `--max-unpacked` (default `2G`) or past a million
entries:

```text
level=ERROR msg="sync failed" environment=production error="extracting artifact: refusing archive that expands past 2147483648 bytes at modules/big/files/blob"
```

The refusal is a failed sync like any other: the temporary directory is
discarded, the environment keeps serving its previous version, and the agent
tries again next poll. There is no way to turn the bound off, only to raise
it, and the default is far above any Puppet code tree — a mid-sized control
repo is tens of megabytes — while staying far below a disk. Raise it only for
an environment that really carries that much.

## Verification is by resealing, not by checksum

The agent re-derives the `code_id` from the extracted tree and compares it to
the one it asked for.

A transfer checksum would only prove the bytes arrived intact. Resealing proves
the *tree on disk* is the one the `code_id` names — which is the claim every
catalog compiled against it depends on. It catches corruption in transit, a
truncated response, a bug in extraction, and a publisher serving content that
does not match its advertised id.

A version that fails verification is discarded and the environment is left
untouched.

## Atomic swap

The new version is extracted to a temporary directory and renamed into place,
so a failed or partial transfer never leaves something that looks like a valid
version.

The environment link is then created under a temporary name and `rename(2)`d
over the old one. `rename(2)` is atomic: OpenVox Server resolves either the old
version or the new one, never an absent or half-written link.

**Do not use `ln -sf`.** It unlinks before creating, leaving a window where the
environment does not exist at all — during which catalog compilation fails.

## Expiring the environment cache

The swap alone is not a deploy. OpenVox Server caches each environment it has
compiled — the parsed manifests, the module list, the loaded types — for
`environment_timeout`, and a production compiler sets that to `unlimited` so it
does not re-parse the whole environment on every catalog. A server holding that
cache never looks at the symlink again. So after a swap it keeps compiling the
tree it parsed earlier, while `code-id` — spawned fresh on every compile and
reading the symlink that just moved — reports the new `code_id`. Every catalog
compiled in that state is stamped with a `code_id` that does not describe it,
and `code-content` then serves the new tree's bytes for resources compiled from
the old one. That is the plausible-but-wrong answer static catalogs exist to
rule out, and it exits 0.

So after every reconciliation that moved anything, the agent asks the server on
this node to expire each environment that moved:

```text
DELETE https://<certname>:8140/puppet-admin-api/v1/environment-cache?environment=<env>
```

It expires that environment only, not the whole cache: nothing else changed,
and dropping every environment would make each deploy re-parse the whole
estate. A pruned environment is expired too, or the server would keep compiling
an environment that no longer exists. The request goes over the same client as
the poll, so it carries this node's own Puppet certificate — which is what the
server must be told to admit.

### The server has to allow it

The `auth.conf` OpenVox Server ships has no rule for that path, so it falls
through to `puppetlabs deny all` and the request is refused with `403`. Add a
rule to `/etc/puppetlabs/puppetserver/conf.d/auth.conf` and restart
`puppetserver`:

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

That admits the same `pp_role` the publisher already requires of a compiler, so
one rule covers the fleet. It names the extension by OID rather than as
`pp_role` because a compiler runs with its CA service disabled, and the admin
API is then authorized with no OID-to-short-name map at all: a rule written
`pp_role: "openvox_compiler"` matches nothing there, and the server logs only
`denied by rule 'codavox environment cache flush'`. (On a CA-bearing primary
the short name would work; the OID works on both.) A node whose certificate
carries no `pp_role` — a primary that compiles its own catalogs, or a compiler
enrolled before codavox — is admitted by certname instead:
`allow: ["compiler01.example.com"]`. With the
[`puppet_authorization`](https://forge.puppet.com/modules/puppetlabs/puppet_authorization)
module, `puppet_authorization::rule` writes the same rule;
[`provision-compiler.sh`](../test/integration/provision-compiler.sh) shows it
done by hand with the `hocon` gem the agent package ships.

### A failed flush is a failed reconciliation

Until the flush lands the server is compiling the old tree under the new
`code_id`, so a refused or unreachable flush fails the reconciliation: `--once`
exits 1, and the daemon logs it at ERROR — collapsed on repeats, like any other
sync failure — with a `403` naming the missing rule rather than reading like a
certificate problem. The swap is **not** rolled back. It cannot be undone
safely, and the server has to be told either way; what the failure means is
that this node is not yet converged.

The flush stays owed and is retried on every poll until it succeeds, whether or
not anything new was deployed. The usual reason it fails is that `puppetserver`
is down or restarting, and a restarted server has an empty cache — so the
retry is mostly for the case where the rule above is missing, where it keeps
saying so until you add it. A restarted *agent* forgets what it owed; if you
restart it after a failed flush, expire the cache yourself with the `DELETE`
above or restart `puppetserver`.

Nothing about this needs the agent to run beside its server in any particular
order. Bring a compiler up agent-first, as [Rollout](production.md#rollout)
says: the first flush fails because nothing is listening on 8140 yet, and lands
on the first poll after the server comes up.

### Turning it off

Set `flush_environment_cache: false` (or `--flush-environment-cache false`) on
a compiler that runs with `environment_timeout = 0`, where every compile
re-reads the environment and there is nothing to expire. Point `puppetserver:`
at the server if it does not listen on `https://<certname>:8140`. The default
addresses the server by this node's certname rather than `localhost` for the
same reason `codavox compilers` does: the server presents its Puppet
certificate, which is issued for the certname and does not verify against
`localhost`.

PE's file sync makes the same call after every sync it commits, for the same
reason.

## Reaping

A version is retained while it is current, while it is among the most recent
`--keep`, or while it is younger than `--min-age`.

The age rule is the one that matters. An agent run that received a catalog
stamped with an older `code_id` will still request file content for it, and
deleting that tree turns a successful run into a failed one. `--min-age` should
comfortably exceed your longest agent run.

The current version is never reaped, regardless of age or count.

### Extractions abandoned by a crash

A crash mid-download — a `SIGKILL`, an OOM kill, a power loss — never runs
`download`'s deferred cleanup, so its dot-prefixed extraction directory is left
on disk exactly as it was.

Reaping a version and reaping one of these are different problems. A live
extraction in another process looks identical to an abandoned one, so it can
never be touched on sight — pulling a directory out from under a live
extraction would be worse than leaving it. Age is what tells them apart: a
live extraction keeps creating entries under its directory, which keeps
bumping that directory's own `mtime`, so it never goes stale on its own; an
abandoned one stops the instant the process dies. Once one has sat untouched
past `--min-age` — the same bound that already governs how long reap waits
before it can be sure nothing still needs something — it is swept.

## Pruning deleted environments

By default the agent adds and updates environments but never deletes one — a
removed environment simply stops being updated, and its symlink and versions
linger. With `--prune-environments`, the agent also removes an environment the
publisher no longer serves: it deletes the environment symlink immediately, so
new compiles fail loudly, and reaps that environment's versions by `--min-age`
so an in-flight run's file content still resolves until the age passes.

Deletion is destructive, so it is guarded:

- **Only after a successful poll.** A publisher that is unreachable is never read
  as "every environment was deleted"; a failed poll prunes nothing.
- **Never on an empty advertisement.** If the publisher serves *zero*
  environments — far more likely a misconfiguration or an empty basedir
  directory than a real mass deletion — the agent prunes nothing and logs a
  warning. Deleting the last environment stays a manual action.

For a deletion to reach here at all, r10k must be configured to purge removed
environments from the basedir (`purge_levels` in `r10k.yaml`); otherwise the deleted
branch never leaves the publisher's basedir. See
[deploying.md](deploying.md#deleting-environments).

## Reporting what it serves

On every poll the agent tells the publisher what this compiler is currently
serving, as an `X-Codavox-Serving: <env>=<code_id>,...` header on the request it
was already making. The values come from its own environment symlinks — the same
ones `code-id` reads — so the publisher's fleet view and this node's `code-id`
give the same answer.

Read it with [`codavox compilers`](commands.md#codavox-compilers) on the
publisher.

**A reconciliation that changes anything reports again before it returns**, so
the fleet view is current the moment a compiler converges rather than at its
next poll. Waiting would put a whole interval between "this compiler is on the
new code" and "the fleet view says so" — long enough for an operator watching a
deploy land to read a converged compiler as a stale one. It costs one request,
and only on a run that changed something: the steady state of a converged
compiler finding nothing to do adds nothing, and the request goes down the
keep-alive connection the poll just used.

Two more consequences follow from reporting rather than being asked:

- **No compiler listens on anything.** Every connection still originates here,
  which is what lets a compiler behind a firewall converge at all. PE solves the
  same problem by having each compiler expose its state on a status endpoint and
  something central collect it — which needs a listener per compiler and
  PuppetDB to find them.
- **The report describes disk, not intent.** A version that failed verification
  was discarded, so the agent reports the version it is still serving. Inferring
  convergence from downloads would have reported the new one.

It is best effort throughout: an unreadable symlink is left out of the report
rather than failing the poll, and a failed report is not retried, because the
next poll carries the same one. Trading a working deploy for a diagnostic would
be the wrong way round. An agent older than this feature sends no header and
shows as `(not reported)`.

## Failure handling

One environment failing to sync does not stop the others. Failures are logged
and retried on the next poll; the compiler keeps serving what it has.

### Repeated failures are collapsed

A publisher outage is survivable by design — this compiler keeps serving and
catalogs keep compiling — so logging it at ERROR on every poll would describe a
non-event as an emergency. At the default 30s interval that is 120 lines an hour
per compiler, and a fleet through a maintenance window produces thousands.
Anything alerting on ERROR then fires continuously for a state the design calls
fine, which is how real alerts get muted.

Going quiet would be the wrong correction: a log that stops mentioning a problem
reads like the problem stopped. So repeats back off instead.

| event | level |
|---|---|
| first failure of a run | `ERROR` |
| a failure whose **cause changed** — unreachable, then a revoked certificate | `ERROR` |
| repeats, at the 2nd, 4th, 8th, 16th … | `WARN`, carrying `consecutive` |
| recovery | `INFO`, carrying `after_failed_attempts` |

A two-hour outage at the default interval is 240 polls and **eight log lines**,
which still says when it started, that it is ongoing, and when it ended:

```text
level=ERROR msg="sync failed" error="polling publisher: ... connection refused"
level=WARN  msg="sync failed" error="..." consecutive=2
level=WARN  msg="sync failed" error="..." consecutive=4
...
level=INFO  msg="sync recovered" after_failed_attempts=240
```

A cause that changes mid-outage is never swallowed as more of the same, because
that is a different problem wearing the same shape.

## Verifying convergence

The convergence test builds the real binaries, runs a publisher and two
compilers with separate SSL material, and checks that both reach the same
`code_id` — including a compiler that was offline across a deploy:

```console
go test ./internal/agent/ -run TestTwoCompilersConverge -v
```

```text
both compilers at 7b05ff28279c54d252387a522beee5a434c234713c8c8c545ee34bc531930d3a
both compilers converged to 73433dde9ecef3ab1709d4c957bf2ba914f16685bae2d2f4abb7b5c4e66eb8e3 after catch-up
```
