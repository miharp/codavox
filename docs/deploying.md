# Deploying

`codavox deploy` runs r10k to stage code, then triggers the publisher to reseal
— one command that takes a change from the control repo to serving on the
primary. It is the familiar `puppet-code deploy` verb, for OpenVox Server.

```console
$ codavox deploy production --basedir /etc/puppetlabs/code/environments
production    deployed    a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed    (commit 5f2e9c1)
```

Run it on the primary, next to the publisher.

## What it does

1. Runs r10k: `r10k deploy environment <environment>… --puppetfile`, which
   resolves the control-repo branches and their Puppetfile modules into the
   basedir. r10k is synchronous, so `deploy` always waits for it.
2. Seals each staged tree to report its `code_id` and reads the control-repo
   commit r10k resolved.
3. Signals the running publisher with `SIGHUP` so it reseals and begins serving
   the new versions.

codavox does not re-resolve code per compiler — that is its load-bearing
invariant. Running r10k here does not weaken it: r10k runs once, centrally,
producing one resolved tree that every compiler converges onto, exactly as PE's
Code Manager runs r10k once and distributes the result.

## Options

| flag | default | purpose |
|---|---|---|
| *(positional)* | | Environments to deploy; omit with `--all` |
| `--all` | | Deploy every environment r10k manages |
| `--wait` | | Block until the publisher serves each new `code_id` |
| `--no-modules` | | Skip Puppetfile module resolution (`r10k` without `--puppetfile`) |
| `--r10k` | `r10k` on `PATH`, then `/opt/puppetlabs/puppet/bin/r10k` | r10k binary |
| `--r10k-config` | r10k's own lookup | r10k.yaml passed with `--config` |
| `--basedir` | *required* | r10k's basedir, the same directory the publisher serves |
| `--state` | `<root>/state` | Publisher state directory (pidfile and artifacts) |
| `--json` | | Emit results as a JSON array |

`--basedir` and `--state` must match the publisher's own flags. r10k's basedir,
`publish --basedir`, and `deploy --basedir` are the same directory.

## Where your control repo is configured

In r10k, not in codavox. codavox has no control-repo setting: `deploy` shells
out to r10k, and r10k reads the control repo URL from the `sources:` section of
its own `r10k.yaml`:

```yaml
# /etc/puppetlabs/r10k/r10k.yaml
sources:
  puppet:
    remote: https://github.com/example/control-repo.git
    basedir: /etc/puppetlabs/code/environments
```

Which `r10k.yaml` is used follows r10k's own rules. With `--r10k-config` unset,
codavox passes no `--config` at all, so r10k performs its normal lookup —
`/etc/puppetlabs/r10k/r10k.yaml` on a stock install. Set `--r10k-config` (or
`r10k_config` in the [config file](configuration.md)) only to pin a different
file, and `--r10k` only to pin a different binary.

To change which repo or branches you deploy, edit `r10k.yaml` exactly as you
would without codavox. codavox picks the change up on the next deploy, because
it distributes whatever r10k resolves into the basedir.

## `--wait`

Without `--wait`, `deploy` runs r10k, sends the signal, and returns — the reseal
happens asynchronously in the publisher.

With `--wait`, it blocks until the publisher has materialized the artifact for
each new `code_id`, then reports `serving`:

```console
$ codavox deploy production --wait --basedir /etc/puppetlabs/code/environments
production    deployed    a3f1c9e4b2d8bb803c020b3aee66cd8887123234ea0c6e7143c0add73ff431ed    (commit 5f2e9c1)    serving
```

This is primary-side completion: the new version is sealed and servable. It does
not wait for every compiler to converge — compilers poll and catch up on their
own, and a stronger fleet-wide wait is a later feature.

## Triggering the reseal

`deploy` signals the publisher through the pidfile the publisher writes to
`<state>/publish.pid`. It verifies the process is alive before signaling, so a
stale pidfile from a crashed publisher is reported rather than acted on.

Only a live publisher's claim on that file counts. A second `codavox publish`
sharing the state directory is refused — `a publisher is already running as pid N`
— rather than displacing the incumbent, and a pidfile left behind by a crash or a
`SIGKILL` is taken over, since nothing gets to clean up after those.

**If no publisher can be signalled, `deploy` exits 1.** It still stages the code
and still reports each `code_id`, because the caller needs to know the basedir
moved — but the deploy has not achieved what it was asked to do, and no compiler
will ever see it:

```console
$ codavox deploy production
production   deployed   de85ed717e0a2a01c1a8ba1a1f036d7ce386ed953696fa57331c2ac48a80b255   (commit 4786bf9)
codavox: deployed to the basedir but nothing is serving it: no running publisher (no pidfile)
$ echo $?
1
```

Exiting 0 there would print a fresh `code_id` beside the word `deployed` while
nothing served it, which CI records as a green deploy — the plausible-but-wrong
success this project exists to prevent. `--wait` has always been an error in that
case, because there is nothing to wait for; now the plain form agrees with it.

The same signal an operator or r10k `postrun` hook sends (see
[publishing.md](publishing.md#resealing)) is what `deploy` sends for you, so a
codavox deploy and a plain r10k-plus-`postrun` deploy converge to the same
state.

## Concurrent deploys

Every deploy — from the command, the webhook, or a script — takes an exclusive
lock (`<state>/deploy.lock`) around r10k, because r10k rewrites the
directory in place and two overlapping runs would corrupt each other's trees. A
second deploy waits for the first to finish rather than racing it. The lock is
advisory and released if the holder exits, so a crashed deploy does not wedge
it.

## Deleting environments

When a branch is removed from the control repo, its environment is deleted by
letting r10k purge it from the basedir and letting each compiler's agent prune it.

Configure r10k to purge removed environments — codavox does not do this, because
`deploy` only observes r10k's output:

```yaml
# r10k.yaml
deploy:
  purge_levels: [environment]
```

With that set, deleting a branch and redeploying removes the environment
directory from the basedir, so `publish` stops advertising it. Each compiler running
the agent with [`--prune-environments`](agent.md#pruning-deleted-environments)
then removes it. Both halves are opt-in — codavox never deletes an operator's
code by default.

## Other ways to deploy

`deploy` is the command-line front door onto the deploy path. Two others reuse
the same path rather than reimplementing it: a control-repo **push webhook** and
a token-authenticated **deploy API**, both served by
[`deploy-server`](deploy-server.md), so a push or a CI job deploys the way
`puppet-code` does.

## Exit codes

| code | meaning |
|---|---|
| `0` | every requested environment deployed |
| `1` | r10k failed, an environment could not be sealed, or a `--wait` timed out |
| `2` | usage error |

Per-environment failures do not abort the others; the exit code reflects any
failure, and `--json` reports each environment's status individually.
