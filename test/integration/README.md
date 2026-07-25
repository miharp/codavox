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
Java 21). Version directories live on a named volume rather than the container
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
4. **Deploy server.** `GET /v1/health` is open; the deploy API is **gated by the
   bearer token** (401 without, 200 with).
5. **No fallback.** Asking for content at an undeployed `code_id`, or an unknown
   environment, is a **hard error** — never a plausible-but-wrong answer.
6. **Prune.** The agent **reaps old version directories** rather than letting
   them accumulate.
7. **Revocation.** `puppetserver ca revoke` on a compiler's certificate **cuts
   off its access to code** — with no restart of the publisher, and while the
   certificate is still cryptographically valid and still carries its `pp_role`.
   Runs last, because the compiler cannot fetch code afterwards.

## Files

| file | role |
|---|---|
| [`run.sh`](run.sh) | Orchestrator: builds the package, brings up the stack, provisions, runs the features, tears down |
| [`provision-primary.sh`](provision-primary.sh) | Boots the CA, seeds an environment, starts the publisher + deploy server |
| [`provision-compiler.sh`](provision-compiler.sh) | Enrolls a `pp_role=openvox_compiler` cert, converges the agent, wires OpenVox Server |
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

## Two bugs a real server found that unit tests could not

**Deployed version directories were mode 0700.** `os.MkdirTemp` creates 0700 and
the agent renames that into place, so OpenVox Server — running as the `puppet`
user while the agent runs as root — failed every catalog compile with `EACCES`
on `environment.conf`. No same-user test can see this.

**Managing the stock codedir is not viable.** A fresh OpenVox Server ships a
populated skeleton at `code/environments/production`, and `rename(2)` cannot
replace a real directory with a symlink. codavox owns
`/opt/puppetlabs/codavox/environments` instead, which is also what PE's versioned
deploys do with `/etc/puppetlabs/puppetserver/code`.
