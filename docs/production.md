# Running codavox in production

[installation.md](installation.md) covers getting the package onto a node. This
covers the decisions you have to make before a fleet depends on it: what has to
be true first, how much disk and network it costs, what survives what, and how
you know it is working.

- [Before you start](#before-you-start)
- [Topology and ports](#topology-and-ports)
- [Authorizing compilers](#authorizing-compilers)
- [Sizing](#sizing)
- [What survives what](#what-survives-what)
- [Rollout](#rollout)
- [Monitoring](#monitoring)
- [Upgrades and version skew](#upgrades-and-version-skew)
- [Backup and rebuild](#backup-and-rebuild)
- [Removing an environment](#removing-an-environment)
- [Decommissioning a compiler](#decommissioning-a-compiler)

## Before you start

Three things must already be true. None of them are codavox's to provide:

1. **r10k already deploys your control repo** on the primary, into a directory
   holding one subdirectory per environment. codavox reads that directory; it
   does not replace r10k, and [deliberately never resolves a Puppetfile
   itself](design.md).
2. **Every compiler is enrolled with the Puppet CA**, with a signed certificate,
   a private key, the CA certificate, and a CRL under `$ssldir`. codavox issues
   no certificates and runs no CA — it reuses what is already there.
3. **You know which node runs the publisher.** It is normally the primary,
   because that is where r10k runs and the publisher must read the
   basedir locally.

### The three code directories

codavox touches code directories differently on each side, and the primary is the
one that surprises people: **codavox adds nothing there.**

On the **primary**, it reads the tree r10k already writes and writes only its own
artifacts:

```text
/etc/puppetlabs/code/environments/       r10k writes it; codavox only READS it
  production/  testing/                  <- this directory is `basedir`
/opt/puppetlabs/codavox/state/
  artifacts/                             sealed .tar.gz, one per current version
  provenance.jsonl
```

On each **compiler**, it owns two directories and leaves the stock codedir alone:

```text
/opt/puppetlabs/codavox/versions/
  production_3224ddbe…/                  unpacked tree, one per code_id
  production_7b05ff28…/                  the previous one, kept for in-flight runs
/opt/puppetlabs/codavox/environments/    OpenVox Server's environmentpath points here
  production -> ../versions/production_3224ddbe…/    the atomic swap
/etc/puppetlabs/code/environments/       untouched; the stock skeleton stays
```

A compiler's codedir is separate because a fresh OpenVox Server ships a populated
`production/` in the stock path, and `rename(2)` cannot replace a real directory
with a symlink. PE hits the same wall and solves it the same way, moving `codedir`
to `/etc/puppetlabs/puppetserver/code` when versioned deploys are on.

### Setting `basedir`

`basedir` is r10k's `basedir` — the directory holding environments, not its
parent. Take the value from `r10k.yaml`:

```yaml
# r10k.yaml
sources:
  puppet:
    basedir: /etc/puppetlabs/code/environments   # <- this is codavox's basedir
```

| your setup | `basedir` |
|---|---|
| Stock OpenVox with r10k | `/etc/puppetlabs/code/environments` |
| PE-shaped layout | `/etc/puppetlabs/code-staging/environments` |

**There is no staging step**, which is why the setting is not called `staging`:
codavox writes nothing to this directory and keeps no copy of it. A path one
level too high produces a publisher that starts cleanly and advertises nothing.

## Topology and ports

Every connection originates at the compiler. Nothing connects *into* a compiler,
which is what lets one behind a firewall or in another network converge without
an inbound rule.

```text
  PRIMARY                                  COMPILER
  ┌───────────────────────────┐            ┌──────────────────────────────┐
  │ git push / CI             │            │                              │
  │        │                  │            │  codavox agent               │
  │        v   :8170          │            │    │  poll + fetch           │
  │  codavox deploy-server    │            │    │                         │
  │        │                  │            │    v                         │
  │        v                  │            │  versions/<env>_<code_id>/   │
  │  r10k ──> basedir         │            │    ^                         │
  │             │             │            │    │ atomic symlink swap     │
  │             v      :8150  │            │  environments/<env>          │
  │       codavox publish  <──┼────────────┼────┘   ^                     │
  │                           │ mutual TLS │        │                     │
  └───────────────────────────┘            │  openvox-server              │
                                           │  (code-id, code-content)     │
                                           └──────────────────────────────┘

  The only connection between the two nodes is the compiler dialing :8150.
```

| port | service | who connects | authentication |
|---|---|---|---|
| `8150` | `codavox publish` | each compiler's agent | mutual TLS, `pp_role` or certname, CRL checked per request |
| `8170` | `codavox deploy-server` | your CI or git forge | bearer token (API) or shared secret (webhook), over TLS |

Neither collides with OpenVox Server's `8140`, OpenVoxDB's `8081`, or PE's
`8143`. Only `8150` needs to be reachable from compilers; `8170` is optional and
only needed if you deploy by API or webhook rather than by running
[`codavox deploy`](deploying.md) on the primary.

**Firewall rule to add:** compilers → primary on `8150/tcp`. That is the whole
list.

### A primary that compiles its own catalogs

Everything above assumes the publisher and the compilers are different machines.
They need not be, and usually should not be: any node that compiles catalogs
benefits from serving versioned ones, and that includes the primary. A primary
that manages itself is compiling at least one catalog — its own.

So the same node runs both halves and is a client of its own publisher. This is
**not a separate topology**. It is how a primary is set up whether or not the
estate has compilers, and adding one later is purely additive: point the new
compiler at the same publisher and nothing here changes.

For an estate with a single OpenVox Server this is also what makes static
catalogs work at all. `static_catalogs` already defaults to true, but stays
inert until a `code-id-command` and a `code-content-command` are wired up, and
open-source OpenVox Server ships neither — only the hook they plug into.

You can supply that pair yourself; the contract is small, and a shell script
meets the latency budget (see
[versioned-code-contract.md](versioned-code-contract.md)). The two parts a
hand-rolled pair usually gets wrong are the ones codavox exists for. First,
**Puppetfile content**: most `puppet:///modules/...` sources point into modules
r10k installed, which are in no commit of your control repo, so a
`code-content-command` backed by `git show <sha>:<path>` cannot serve them.
Answering requires the *resolved* tree. Second, **retention**: superseded
versions have to stay readable until in-flight agent runs finish with them, so a
script that reads the live environment directory serves whatever is current —
the precise silent mismatch static catalogs exist to prevent.

```text
  PRIMARY (also the compiler)
  ┌────────────────────────────────────────────────────┐
  │  r10k ──> basedir                                  │
  │             │                                      │
  │             v      :8150                           │
  │       codavox publish <──┐                         │
  │                          │ mutual TLS, on loopback │
  │  codavox agent ──────────┘                         │
  │      │  poll + fetch                               │
  │      v                                             │
  │  versions/<env>_<code_id>/                         │
  │      ^                                             │
  │      │ atomic symlink swap                         │
  │  environments/<env>                                │
  │      ^                                             │
  │      │                                             │
  │  openvox-server (code-id, code-content)            │
  └────────────────────────────────────────────────────┘

  No firewall rule at all: the connection never leaves the host.
```

Nothing about the mechanism changes. The node polls itself, verifies the artifact
by resealing it, and swaps its own environment symlink, so a `code_id` in a
catalog means what it means on any compiler.

The one behavioral difference to expect: the primary's own catalogs now converge
on the agent's poll interval rather than the instant r10k finishes, exactly like
every other node. That is the point — the primary stops being the one machine
serving code no `code_id` describes.

The publisher must still be addressed by **certname**, not `localhost`: it
presents this node's Puppet certificate, and `localhost` does not verify against
it.

Two things cost more here than on a compiler.

**The cutover is riskier, not safer.** Pointing `environmentpath` at a directory
the agent has not filled yet stops catalog compilation. On a compiler that is
recoverable — the primary still compiles, and the next agent run repairs it. On a
node compiling its own catalog the agent that would apply the fix needs a catalog
from the server it just broke, so the repair is an SSH session. That holds
whether or not the estate has compilers: a self-managing node cannot Puppet its
way out of it. Either use
[`codavox::primary`](https://github.com/miharp/puppet-codavox), which waits for
the `codavox_environments` fact to report the environment converged before wiring
anything, or do it in two passes by hand: install and start the agent first,
confirm `codavox code-id production` answers, and only then set
`environmentpath`.

**Disk roughly doubles.** The [Sizing](#sizing) table splits publisher and
compiler storage across rows because they are often different machines. Here they
land on the same disk, on top of r10k's basedir, which was already there.
For one 37 MB environment at the default `keep: 3`, that is 37 MB of basedir plus
a 7.1 MB artifact plus four unpacked versions — about 190 MB, against 150 MB for
a compiler that stores no artifacts.

The node does **not** have to authorize itself. The publisher always admits its
own certname, because that node already holds the basedir in plaintext on local
disk — admitting it grants nothing, and requiring it to be written down would
only be a step to forget. The allowlist below names the compilers you add later.

Its own `puppetserver` is a different matter: the agent expires the environment
cache there after every swap, and a primary's certificate usually carries no
`pp_role`, so the `auth.conf` rule from [Wiring into
puppetserver](commands.md#wiring-into-puppetserver) admits it by certname
instead — `allow: ["puppet.example.com"]`. If the primary also serves compilers
that flush *their own* servers, nothing here changes: each agent talks only to
the server on its own node.

It still has to name *something*: a publisher with an empty allowlist is refused
at startup rather than coming up serving nobody. On a node that will never have
compilers, `allow_roles: ['openvox_compiler']` satisfies that and authorizes no
one, which is the accurate description of the estate.

## Authorizing compilers

Being enrolled with the Puppet CA is not enough to fetch code. Every agent in
the estate clears that bar, and Puppet manifests routinely reference internal
hostnames, credential paths, and topology.

Which mechanism you use depends on when the compiler's certificate was issued:

| situation | use |
|---|---|
| Compilers you are about to enrol | `pp_role` — write `pp_role: openvox_compiler` into `csr_attributes.yaml` before the CSR is submitted |
| Compilers already enrolled | `publish.allow_certnames` — list them by certname |

`pp_role` is an X.509 extension fixed at issue time, so an existing compiler
cannot be given one without revoking, cleaning, re-enrolling, and restarting.
Demanding a PKI operation before you can try codavox at all is the wrong order,
so both work and either admits:

```yaml
# /etc/codavox/config.yaml on the primary
publish:
  allow_certnames:
    - compiler01.example.com
    - compiler02.example.com
```

Move a node at a time: drop each from the list as its certificate is re-issued
with a role. Matching is exact — no globs — because an allowlist that matched
loosely would admit more than it says.

See [publishing.md](publishing.md#authorization-ca-membership-is-not-enough) for
the full reasoning.

### Revocation

The publisher reads `$ssldir/crl.pem` — the same file every Puppet service
reads — and applies it to **every request**, not only at handshake. So
`puppetserver ca revoke --certname compiler02.example.com` takes effect on that
compiler's next poll, with nothing to restart.

A missing or unverifiable CRL is a **startup error**. An estate that distributes
no CRL must say so explicitly with `publish.certificate_revocation: false`
rather than getting a silent downgrade to "nothing is revoked".

## Sizing

### Disk

Measured on `internal/treegen`'s mid-sized fixture — 50 modules, 2,503 files,
37 MB — which seals to a **7.1 MB** artifact. Real Puppet code is text and
compresses comparably; scale from your own control repo's size.

| node | what it stores | how much |
|---|---|---|
| Publisher | one artifact per environment, current version only | `~20% of tree size × environments` |
| Publisher | provenance log, one line per seal | a few hundred bytes per deploy, forever |
| Compiler | the current version plus `keep` superseded ones, per environment | `tree size × (keep + 1) × environments` |

The rows are per *role*, not per machine. On [a primary that compiles its own
catalogs](#a-primary-that-compiles-its-own-catalogs) both roles are the same
node, so add them together — and note that r10k's basedir is on that disk too.

The compiler side is what to plan for, and it is **unpacked**, not compressed.
`keep` counts *superseded* versions; the current one is always retained on top
of them. With the defaults (`keep: 3`) and three environments of 37 MB, that is
four copies each — about 440 MB. Versions are full copies, with no deduplication
of identical files across them, so the arithmetic is exactly
`size × (keep + 1) × environments`.

Lowering `agent.keep` is the lever, and it is safe to take low. What protects an
in-flight catalog compile is not `keep` but **`agent.min_age`** (default `2h`):
a superseded version is retained until it is older than that regardless of
`keep`, because an agent run holding a catalog stamped with the previous
`code_id` will still request file content for it. Set `min_age` comfortably
longer than your agent run interval plus its runtime.

A crash mid-download (a `SIGKILL`, an OOM kill) can also leave one abandoned
extraction directory behind, at most the size of one unpacked version. It is
not a growing leak: the agent sweeps it once it has sat untouched past the
same `min_age`. See [agent.md](agent.md#extractions-abandoned-by-a-crash).

### CPU and time

Per deploy, from [performance.md](performance.md), on the 35 MB fixture:

| phase | who pays it | cost |
|---|---|---|
| seal | publisher, once | ~80 ms |
| materialize artifact | publisher, once | ~2.7 s (gzip dominates) |
| unpack | every compiler | ~600 ms |
| verify (re-seal) | every compiler | ~96 ms |
| symlink swap | every compiler | ~0.2 ms |
| `code-id` | every catalog compile | microseconds |

The publisher's work is per deploy, not per compiler, because the artifact is
materialized once at seal time and served from disk. Adding compilers costs
bandwidth, not CPU.

### Network

Each compiler transfers one artifact per environment per deploy — 7 MB on the
fixture — and otherwise polls with a request and response of a few hundred
bytes every `agent.interval` (default 30 s). A converged fleet is nearly silent.

Every poll is jittered by up to 25% of the interval — including the agent's
first poll after it starts, not only the ones between polls. That first poll
matters on its own: without jittering it, a fleet restarted together (a
package upgrade, a reboot) makes every agent's very first request land in the
same instant, which the steady-state jitter cannot reach, since it only takes
effect after that first call returns.

**The one real burst is a coordinated restart landing on a pending deploy.**
The poll itself is cheap no matter how many compilers make it at once — one
JSON-encoded map, no disk I/O — but if every agent's first poll also finds a
new `code_id` (a fleet rolling out for the first time, or restarting into a
deploy that landed while it was down), every compiler fetches the artifact
inside that same jittered window. That is bandwidth, not CPU: fleet size
times artifact size, spread over roughly `agent.interval / 4` (7.5 s by
default), landing on one publisher. `serveArtifact` sets no cap of its own —
it is a plain file open and copy — so this is genuinely fleet size times
artifact size divided by the jitter window, not smoothed further:

| fleet size | data (at 7 MB/artifact) | sustained over 7.5 s |
|---|---|---|
| 50 compilers | 350 MB | ~47 MB/s (~370 Mbps) |
| 1,000 compilers | 7 GB | ~930 MB/s (~7.5 Gbps) |

The first is unremarkable on any real link. The second is not — plan for it
if you are rolling out to a large fleet at once, or restarting one right after
a deploy: widen `agent.interval` for the rollout, stagger the restart in
batches, or simply expect the burst and size the primary's network for it.

## What survives what

This is the table to read before deciding codavox is safe to depend on.

| failure | effect |
|---|---|
| **Publisher down** | No new deploys. Every compiler keeps serving the version it has, and catalogs continue to compile. This is the property that makes polling preferable to a shared filesystem, where losing the server loses catalog compilation entirely. |
| **Compiler down** | It converges on its own when it comes back, with nothing replayed to it. A missed deploy is not a missed *event* — the agent compares what it has against what is advertised. |
| **Network partition** | Same as publisher down, from that compiler's side. |
| **Agent stopped on a compiler** | That compiler keeps serving its current version indefinitely. Catalogs compile; they are just stale, and [`codavox compilers`](commands.md#codavox-compilers) shows it as stale with an old `LAST POLL`. |
| **One environment fails to seal** | The other environments still publish. The failed one keeps its last good version and is reported. Refusing to publish anything over one bad module would turn a local problem into a fleet-wide outage. |
| **Corrupt or truncated artifact** | Rejected. The agent re-derives the `code_id` from the extracted tree; a mismatch discards it and leaves the environment untouched. |
| **Disk full on a compiler** | The sync fails and is retried next poll. The environment is untouched, because the swap only happens after a version is fully unpacked and verified. |
| **Publisher restarts** | It reseals at startup and serves again. The fleet view empties and refills within one poll interval. |
| **`code-id` cannot answer** | Catalog compilation **fails**, loudly. There is no fallback by design: serving plausible-but-wrong content while exiting `0` is the exact failure static catalogs exist to prevent. |

The last row is the one to internalize. codavox trades a silent-wrong-answer
failure mode for a loud-no-answer one. That is the whole point, but it means a
compiler wired to codavox before its agent has converged fails every catalog
compile — see [Rollout](#rollout).

### Publisher redundancy

There is one publisher. Running two is possible — they seal the same basedir
directory to the same `code_id`, because sealing is reproducible — but codavox
does not coordinate them, and pointing agents at one address means you need a
load balancer or DNS to fail over.

Given that a publisher outage degrades to "no new deploys" rather than "no
catalogs", the usual answer is to leave it single and restore it at leisure.
Decide deliberately rather than discovering it during an incident.

## Rollout

Installing the package changes nothing; **wiring a compiler is the careful
step**. Bring each one up in this order:

1. Install the package. It is inert — no unit is enabled, nothing is written to
   `puppetserver`'s config.
2. Allow the agent to expire this server's environment cache: add the
   `auth.conf` rule from [Wiring into
   puppetserver](commands.md#wiring-into-puppetserver) and restart
   `puppetserver`. On its own the rule changes nothing — no one is calling the
   endpoint yet — so this is safe to do ahead of time.
3. Configure and start `codavox-agent`. Let it converge, so the environment
   symlinks exist.
4. Confirm with `codavox code-id production` on that node, and that it appears
   in `codavox compilers` on the publisher.
5. *Then* set `environmentpath` and `versioned-code.conf`, and restart
   `puppetserver`.
6. Compile a catalog against that compiler and confirm it works, **before**
   touching the next one.

Reverse it by reverting those two settings; the compiler is back to stock.

Canary one compiler for at least a deploy cycle before rolling the fleet. The
failure you are looking for is environment-specific — a module that seals but
does not compile — and it will show up on the first real catalog, not on the
agent's convergence.

## Monitoring

### Is the fleet converged?

```console
codavox compilers
```

```text
COMPILER                ENVIRONMENT  CODE_ID       COMMIT        LAST POLL
compiler01.example.com  production   3224ddbe7e3d  a3f1c9e4b2d8  12s ago
compiler02.example.com  production   7b05ff282795  61d70aa9c3e5  9m0s ago
```

This is each compiler's own answer, read from the same symlink `code-id` reads.
Two things are worth alerting on, and `--json` gives both:

- **`serving` differs across compilers for the same environment** for longer
  than a deploy takes to propagate. One node is stuck.
- **`last_poll` is older than a few intervals**, or a compiler you expect is
  missing entirely. Its agent is down, or it cannot reach the publisher.

Do not alert on `fetched` or `fetches`: a converged compiler polls constantly
and fetches nothing, which is the healthy steady state.

The view is in memory, so a publisher restart empties it. Treat "missing" as
meaningful only after the fleet has had an interval to poll.

### Is the publisher up?

```console
curl -s https://primary.example.com:8150/v1/health
```

```json
{"status": "ok"}
```

### Is a specific compiler right?

```console
codavox code-id production      # on the compiler
```

This must match what `codavox compilers` reports for that node. They read the
same symlink, so a disagreement means one of them is not looking at the node you
think it is.

### Logs

All three daemons log to the journal:

```console
journalctl -u codavox-publish -f
journalctl -u codavox-agent -f
```

The agent logs `environment updated` with the new `code_id` on every convergence,
which is the line to grep when reconstructing when a node moved, followed by
`environment cache flushed` once the server has been told. An `environment cache
flush failed` between them means catalogs are being compiled from the old tree
under the new `code_id` until it succeeds; a `403` there is the `auth.conf` rule
missing.

## Upgrades and version skew

Upgrade the **publisher first**, then compilers. A rolling upgrade leaves the two
at different versions for a while, which is expected:

- A **newer publisher with older agents** works. Agents that predate fleet
  reporting simply send no report and show as `(not reported)`; they still poll,
  fetch, verify, and converge.
- An **older publisher with newer agents** works. The publisher ignores the
  report header it does not know about.

The artifact format and the `code_id` derivation are the compatibility surface
that matters, because a change to either would make every compiler re-download
every environment on the first deploy after the upgrade. Release notes call that
out when it happens; treat a `code_id` that changes without a code change as the
signal.

Upgrading a compiler's package does not disturb what it is serving: the binary
is replaced, the version directories and symlinks are not. Restart
`codavox-agent` to pick up the new binary.

### Upgrading to 0.7

From 0.7 the agent expires each environment it swaps from OpenVox Server's
cache, and treats a refused flush as a failed sync — so a compiler upgraded
before its `auth.conf` allows the flush reports every deploy as `sync failed`
with a `403`, while still deploying the code. Add the rule from [Wiring into
puppetserver](commands.md#wiring-into-puppetserver) **before** upgrading the
agent; with [`puppet-codavox`](https://github.com/miharp/puppet-codavox) that
means module 0.4.0 first, which writes it. The rule is harmless to an older
agent. Nothing about the artifact format or the `code_id` derivation changed, so
no environment is re-downloaded.

## Backup and rebuild

Almost nothing here is precious, by design:

| state | on loss |
|---|---|
| Compiler version directories | Re-downloaded on the next poll. |
| Environment symlinks | Recreated by the agent. Catalogs fail until it converges. |
| Publisher artifacts | Regenerated from the basedir at the next seal. |
| Fleet view | In memory; refills within one poll interval. |
| **Provenance log** | **Not recoverable.** |

The provenance log (`<state>/provenance.jsonl`) is the one thing worth backing
up. It maps each `code_id` to the control-repo commit that produced it, and it
is built from r10k's `.r10k-deploy.json` at seal time — so history for past
deploys cannot be reconstructed after the fact. Losing it costs you the
`COMMIT` column for old versions, nothing more; nothing stops working.

A rebuilt primary re-seals the same basedir to the **same `code_id`**,
because sealing is reproducible. Compilers see no change and download nothing.

## Removing an environment

Deleting a branch and redeploying removes the environment from the primary, and
the publisher stops advertising it. The
[webhook](deploy-server.md#branch-deletions) runs that deploy for you when it
sees the deletion; by hand it is any deploy at all, because r10k purges
unmanaged environments at the end of every run:

```console
$ r10k deploy environment -v -p
INFO -> Removing unmanaged path /etc/puppetlabs/code/environments/testing

$ curl .../v1/environments
{"production":"835b1663c47534367242ba7616c5c60369bc25ced2dcb5b92808d31b58ef217a"}
```

**That does not remove it from any compiler.** Every compiler keeps the
environment it already has, and catalogs for it keep compiling:

```console
$ ls /opt/puppetlabs/codavox/environments/     # on each compiler
production  testing

$ puppet agent -t --environment testing
Notice: Catalog compiled by compiler01.example.com
Notice: Applied catalog in 0.06 seconds
```

This is deliberate, and it is the same rule as everywhere else here: the agent
adds and updates, and never deletes on its own. A publisher that comes up
pointed at an empty directory advertises nothing, and an agent that treated
"nothing advertised" as "delete everything" would take the entire fleet's code
away over a configuration mistake. Deleting is opt-in because it is the one
action that cannot be undone by fixing the publisher.

The consequence is worth stating plainly: **if you deleted an environment
because it contained something that should not be running, deleting it from the
control repo does not stop it running.** Until you prune, every compiler still
serves it to anything that asks for it by name.

### Seeing what is left

`codavox compilers` lists it, per node, so the remainder is visible rather than
silent:

```text
COMPILER                ENVIRONMENT  CODE_ID       COMMIT        LAST POLL
compiler01.example.com  production   835b1663c475  8cb7b0396b19  9s ago
compiler02.example.com  production   835b1663c475  8cb7b0396b19  7s ago
compiler02.example.com  testing      283116cf7877  52dd1cc66493  7s ago
```

compiler02 is still serving `testing`; compiler01 has pruned it. An environment
appearing here that the publisher no longer advertises is exactly this case.

### Completing the removal

```yaml
agent:
  prune_environments: true
```

The agent then removes the environment on its next poll:

```text
level=INFO msg="pruned environment" environment=testing
```

Two details matter when you turn it on:

- **The symlink goes immediately**, so new compiles for that environment fail
  loudly. That is correct — the environment no longer exists — but it is abrupt
  for anything still pointed at it, so check `codavox compilers` for who is
  serving it before enabling.
- **The version directories stay until `min_age`** (default `2h`). An agent run
  that received a catalog stamped with that `code_id` still resolves file content
  by it, and cutting that off would turn a successful run into a failed one. So
  disk is not reclaimed at the moment of pruning.

Pruning never acts on an empty advertisement, so a publisher misconfiguration
still cannot cascade into a fleet-wide deletion:

```text
level=WARN msg="publisher advertised no environments; skipping prune"
```

## Decommissioning a compiler

```console
puppetserver ca revoke --certname compiler02.example.com
```

That is the whole operation on the codavox side. The publisher applies the CRL
per request, so the node loses access to code on its next poll without a restart
of anything. It stays in the fleet view until the publisher restarts, with a
`LAST POLL` that stops advancing — which is the correct report of what happened.

Revoking a compiler's Puppet certificate revoking its access to code is a
deliberate consequence of reusing the Puppet CA rather than building a second
PKI, and it is what an operator would expect.
