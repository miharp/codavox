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

## What a reconciliation does

1. Poll `/v1/environments` for the current `code_id` of each environment.
2. Skip any environment already at that version.
3. Fetch the artifact, extract it to a temporary directory.
4. **Verify by resealing the extracted tree** and comparing to the requested
   `code_id`.
5. Rename the temporary directory into place.
6. Atomically swap the environment symlink.
7. Reap superseded versions.

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

## Reaping

A version is retained while it is current, while it is among the most recent
`--keep`, or while it is younger than `--min-age`.

The age rule is the one that matters. An agent run that received a catalog
stamped with an older `code_id` will still request file content for it, and
deleting that tree turns a successful run into a failed one. `--min-age` should
comfortably exceed your longest agent run.

The current version is never reaped, regardless of age or count.

## Failure handling

One environment failing to sync does not stop the others. Failures are logged
and retried on the next poll; the compiler keeps serving what it has.

## Verifying convergence

The convergence test builds the real binaries, runs a publisher and two
compilers with separate SSL material, and checks that both reach the same
`code_id` — including a compiler that was offline across a deploy:

```console
go test ./internal/agent/ -run TestTwoCompilersConverge -v
```

```text
both compilers at 7b05ff28279c54d2...
both compilers converged to 73433dde9ecef3ab... after catch-up
```
