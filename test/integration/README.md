# Integration testing

Exercises every codavox feature end to end against **real OpenVox Server
processes and real Puppet certificates** — the one thing unit tests cannot cover,
because the `versioned-code-service` contract is with a live puppetserver.

The default harness is **self-contained**: it stands up its own two-node OpenVox
topology and provisions it with plain shell, so it needs no ovadm or any other
provisioner. That is deliberate — someone adopting codavox without ovadm should
be able to prove the whole chain, and read the scripts as a worked example of
doing it by hand. The same run also drives the CI
[`Integration`](../../.github/workflows/integration.yml) workflow.

## Quick start

```console
./test/integration/run.sh          # build HEAD, provision, test, tear down
KEEP=1 ./test/integration/run.sh   # leave the topology up for inspection
```

Requires Docker (with Compose) and [GoReleaser](https://goreleaser.com). The run
builds a codavox package from the **current checkout**, so it always tests HEAD,
not a release.

## Topology

| container | role |
|---|---|
| `codavox-primary` | Puppet CA; runs `codavox publish` and the deploy server |
| `codavox-compiler` | OpenVox Server wired to codavox + `codavox agent`; doubles as its own puppet agent for the catalog check |

Both are built from [`Dockerfile`](Dockerfile) (Rocky 9 + `openvox-server` +
Java 21 + git and r10k, installed as the OpenVox guide does). Version directories live on a named volume rather than the container
overlay filesystem: atomic symlink swap is a core correctness claim, and testing
`rename(2)` semantics on overlayfs risks either false confidence or flakiness.

Cross-*compiler* convergence is not retested here — it is covered in-process by
`internal/agent`'s Go tests — so one compiler is enough to prove the real-server
contract. The [ovadm alternative](#alternative-provision-with-ovadm) adds a
second compiler if you want that shape too.

## What it exercises

[`features.sh`](features.sh) asserts each of these against the live stack:

1. **Static catalog.** A real puppetserver, wired to codavox, compiles a
   **static catalog stamped with the `code_id`** the compiler reports serving.
2. **Reseal → new `code_id`.** Changing the staged tree and reloading the
   publisher yields a new `code_id`, and the agent **converges on it by polling**
   — no push.
3. **Offline catch-up.** A compiler whose agent is stopped across a deploy
   **catches up on its next poll**, with no event replayed to it.
4. **Environment cache.** The compiler runs with `environment_timeout =
   unlimited`, as a production compiler does. With the agent's flush switched
   off, a deploy **advances the `code_id` while the catalog stays on the old
   tree** — stamped with a `code_id` that does not describe it. With it on, the
   next deploy **is what the server compiles**. The hazard is reproduced first,
   so the fix is not proven against a cache that was never held.
5. **Deploy server.** `GET /v1/health` is open; the deploy API is **gated by the
   bearer token** (401 without, 200 with).
6. **No fallback.** Asking for content at an undeployed `code_id`, or an unknown
   environment, is a **hard error** — never a plausible-but-wrong answer.
7. **Prune.** The agent **reaps old version directories** rather than letting
   them accumulate.
8. **Fleet view.** `codavox compilers` on the publisher reports what the
   compiler **says it is serving**, and that answer **equals the compiler's own
   `codavox-code-id`**. It then follows a deploy through to the new version.
9. **Deploy.** `codavox deploy --wait` runs a **real r10k** against a control
   repo the primary seeded from the served tree, seals the result, returns once
   the publisher serves it, and the compiler converges on a tree that now
   carries a Puppetfile module and an r10k deploy record — so the fleet view
   reports a commit.
10. **Module deploy.** `--modules apache` re-resolves one module and the
    compiler serves the change. A short name that matches nothing in the
    Puppetfile, which r10k deploys nothing for and exits 0, **fails the
    deploy**; a long name (`puppetlabs/apache`) is refused with a hint before
    r10k runs.
11. **Hung r10k.** A stand-in r10k that never returns is **killed at
    `--r10k-timeout`, its child with it**, the deploy fails naming the bound,
    and the next deploy runs at once because the lock was released.
12. **Extraction cap.** An artifact past `--max-unpacked` is **refused before
    it lands**, with nothing installed, in a scratch root so the node's real
    agent is untouched.
13. **Branch deletion.** A branch added to the control repo becomes an
    environment the compiler serves; deleting it and posting the deletion to
    the webhook **deploys everything, r10k purges the directory, the publisher
    stops advertising it, and the compiler prunes it**.
14. **Revocation.** `puppetserver ca revoke` on a compiler's certificate **cuts
    off its access to code** — with no restart of the publisher, and while the
    certificate is still cryptographically valid and still carries its `pp_role`.
    Runs last, because the compiler cannot fetch code afterwards.

Features 4, 8, 13, and 14 are the four that **no Go test can replace**. The
environment cache is puppetserver's, so only a real server can show it going
stale and being expired. Feature 13 needs r10k's own purge and a compiler that
polls for real. For the other two, every Go test drives `agent --once`
— a fresh process and a fresh TLS connection per sync — whereas here the agent
is a long-running daemon polling over one keep-alive connection. A report that
only rode on the first request of a connection, or a CRL consulted only at
handshake, would pass the entire unit suite and fail here.

## Debugging a failed run

`features.sh` deliberately runs without `set -e`, so one failure does not hide
the rest and the summary lists everything that broke.

Keep the topology and poke at it:

```console
KEEP=1 ./test/integration/run.sh
./test/integration/features.sh          # re-run the assertions alone, as often as you like
docker compose -f test/integration/compose.yml down -v   # tear down when finished
```

**Read the journal, not `docker logs`.** The codavox and puppetserver units run
under systemd inside each container, so `docker logs` shows PID 1 and never the
daemon's own output:

```console
docker exec codavox-primary  journalctl -u codavox-publish       --no-pager
docker exec codavox-primary  journalctl -u codavox-deploy-server --no-pager
docker exec codavox-compiler journalctl -u codavox-agent         --no-pager
docker exec codavox-compiler journalctl -u puppetserver          --no-pager
```

This is worth internalizing: an assertion written against `docker logs` reports
a failure that is not real, which is a worse outcome than no assertion at all.

Two more things that mislead:

- **The primary's `server` setting.** `puppetserver ca` talks to the CA over
  HTTPS at whatever `server` names, so if it is wrong, revoking or signing fails
  with a DNS error that says nothing about certificates.
  [`provision-primary.sh`](provision-primary.sh) sets it to `primary`.
- **Never discard a command's output in an assertion.** A test whose job is to
  explain a failure must not hide the one line that explains it.

## Files

| file | role |
|---|---|
| [`run.sh`](run.sh) | Orchestrator: builds the package, brings up the stack, provisions, runs the features, tears down |
| [`provision-primary.sh`](provision-primary.sh) | Boots the CA, seeds an environment, initializes a control repo and a module repo for r10k, starts the publisher + deploy server |
| [`provision-compiler.sh`](provision-compiler.sh) | Enrolls a `pp_role=openvox_compiler` cert, allows the cache flush in `auth.conf`, converges the agent, wires OpenVox Server |
| [`features.sh`](features.sh) | The end-to-end assertions above |
| [`compose.yml`](compose.yml) / [`Dockerfile`](Dockerfile) | The self-contained two-node topology |

The publisher enforces `pp_role=openvox_compiler` on the client certificate, so
`provision-compiler.sh` requests that CSR attribute; codavox then reuses the same
Puppet material for its mutual TLS, issuing no certificates of its own.

## Alternative: provision with ovadm

If you already run [ovadm](https://github.com/miharp/ovadm), its `ovadm::codavox`
plan sets the same thing up — and adds a second compiler, so you also get real
cross-compiler convergence. `compose.codavox.yml` layers a second compiler onto
ovadm's compose, and `inventory.yaml` targets all four nodes.

```console
docker compose -f ~/projects/ovadm/docker-compose.yml \
  -f test/integration/compose.codavox.yml up -d --build

cd ~/projects/ovadm
bolt plan run ovadm::install server_host=puppet \
  --inventoryfile ~/projects/codavox/test/integration/inventory.yaml
bolt plan run ovadm::add_compiler server_host=puppet \
  compiler_hosts=compiler01,compiler02 \
  --inventoryfile ~/projects/codavox/test/integration/inventory.yaml
```

Then build a snapshot and hand it to `ovadm::codavox` so the run tests your code
rather than a release (drop `package_url` and pass `codavox_version=<release>`
to install the published package instead):

```console
goreleaser release --snapshot --clean --skip=publish
RPM=$(ls dist/*linux_arm64.rpm)
for c in ovadm-server ovadm-compiler01 ovadm-compiler02; do
  docker cp "$RPM" "$c:/tmp/codavox.rpm"
done

cd ~/projects/ovadm
bolt plan run ovadm::codavox server_host=puppet \
  compiler_hosts=compiler01,compiler02 \
  package_url=/tmp/codavox.rpm \
  --inventoryfile ~/projects/codavox/test/integration/inventory.yaml
```

## Four bugs a real server found that unit tests could not

**Deployed version directories were mode 0700.** `os.MkdirTemp` creates 0700 and
the agent renames that into place, so OpenVox Server — running as the `puppet`
user while the agent runs as root — failed every catalog compile with `EACCES`
on `environment.conf`. No same-user test can see this.

**Managing the stock codedir is not viable.** A fresh OpenVox Server ships a
populated skeleton at `code/environments/production`, and `rename(2)` cannot
replace a real directory with a symlink. codavox owns
`/opt/puppetlabs/codavox/environments` instead, which is also what PE's versioned
deploys do with `/etc/puppetlabs/puppetserver/code`.

**Revocation checked only at the TLS handshake did nothing.** The publisher
refused revoked certificates in `VerifyConnection`, which runs when a connection
is established — and a running agent polls over one keep-alive connection and
never handshakes again, so a revoked compiler kept fetching code indefinitely.
Every Go test passed, because `agent --once` is a fresh process and therefore a
fresh connection on every sync. Only a real daemon holding a real connection
shows it. The publisher now also checks per request.

**`pp_role` does not resolve in `auth.conf` on a compiler.** The agent's cache
flush was refused with `denied by rule` even though the rule allowed
`extensions: { pp_role: "openvox_compiler" }` and the certificate carried it. A
compiler runs with its CA service disabled, and the disabled service hands the
admin API an authorization handler with **no** OID-to-short-name map, so only
the raw OID (`1.3.6.1.4.1.34380.1.1.13`) or the certname matches there. The
same rule works on the CA-bearing primary, which is where anyone testing by hand
would try it first. See [versioned-code-contract.md](../../docs/versioned-code-contract.md).
