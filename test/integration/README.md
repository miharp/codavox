# Integration testing

Runs codavox against a real OpenVox Server using
[ovadm](https://github.com/miharp/ovadm)'s Docker topology, plus a second
compiler — convergence *between* compilers is the property under test, and a
single compiler makes it unobservable.

## Topology

| container | role |
|---|---|
| `ovadm-server` | CA and publisher; stages code, runs `codavox publish` |
| `ovadm-compiler01` | OpenVox Server + `codavox agent` |
| `ovadm-compiler02` | OpenVox Server + `codavox agent` |
| `ovadm-agent` | agent, already pointed at `compiler01` for catalogs |

`compose.codavox.yml` is layered over ovadm's compose rather than changing that
repo. Version directories are on named volumes rather than the container
overlay filesystem: atomic symlink swap is a core correctness claim, and
testing `rename(2)` semantics on overlayfs risks false confidence.

## Bring it up

```console
docker compose -f ~/projects/ovadm/docker-compose.yml \
  -f test/integration/compose.codavox.yml up -d --build
```

Then install OpenVox Server and enroll both compilers. ovadm's image bakes in a
supported Java, so there is no separate Java step:

```console
cd ~/projects/ovadm
bolt plan run ovadm::install server_host=puppet \
  --inventoryfile ~/projects/codavox/test/integration/inventory.yaml
bolt plan run ovadm::add_compiler server_host=puppet \
  compiler_hosts=compiler01,compiler02 \
  --inventoryfile ~/projects/codavox/test/integration/inventory.yaml
```

## Install and wire codavox

`ovadm::codavox` does the whole setup — installs the package on the server and
compilers, writes each node's config, serves a seeded environment, converges the
agents, then wires OpenVox Server, in the safe order (converge before wiring).

Build a snapshot of the current checkout and hand it to the plan with
`package_url`, so the run tests your code rather than a release:

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

To install the published package instead of a snapshot, drop `package_url` and
pass `codavox_version=<release>`.

## What this exercised

A full deploy chain against real OpenVox Server processes and real Puppet
certificates:

- both compilers converged on the same `code_id` over mutual TLS
- a compiler kept offline across a deploy caught up on a single poll, with no
  event replayed to it
- an agent received a **static catalog** carrying the codavox `code_id` and
  inlined sha256 file metadata
- changing code produced a new `code_id`, and the agent applied the new content

Verify the catalog is genuinely static:

```console
docker exec ovadm-agent grep -o 'code_id[^,]*' \
  /opt/puppetlabs/puppet/cache/client_data/catalog/agent.json
```

## Two bugs this found that unit tests could not

**Deployed version directories were mode 0700.** `os.MkdirTemp` creates 0700
and the agent renames that into place, so OpenVox Server — running as the
`puppet` user while the agent runs as root — failed every catalog compile with
`EACCES` on `environment.conf`. No same-user test can see this.

**Managing the stock codedir is not viable.** A fresh OpenVox Server ships a
populated skeleton at `code/environments/production`, and `rename(2)` cannot
replace a real directory with a symlink. codavox owns
`/opt/puppetlabs/codavox/environments` instead, which is also what PE's
versioned deploys do with `/etc/puppetlabs/puppetserver/code`.

## Tear down

```console
docker compose -f ~/projects/ovadm/docker-compose.yml \
  -f test/integration/compose.codavox.yml down -v
```
