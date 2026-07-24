# Installation

codavox ships as an RPM and a DEB. The binary is static with no runtime
dependencies, so one package per architecture covers every supported distro.

| package | covers |
|---|---|
| `codavox_<version>_linux_amd64.rpm` | Rocky, RHEL, AlmaLinux, CentOS Stream (x86_64) |
| `codavox_<version>_linux_arm64.rpm` | the same, aarch64 |
| `codavox_<version>_linux_amd64.deb` | Debian, Ubuntu (amd64) |
| `codavox_<version>_linux_arm64.deb` | the same, arm64 |

**Pick the right architecture.** OpenVox hosts on Apple silicon — Parallels VMs
and Docker on M-series — are `aarch64`/`arm64`, not `amd64`.

## Where to install it

Install the same package on the **primary** and on **every compiler**. It is one
binary; which parts run depends on the node:

| node | what runs there |
|---|---|
| Primary | `codavox publish`, and `codavox deploy` / `codavox deploy-server` — stages code and serves versioned artifacts to compilers |
| Each compiler | `codavox agent`, which pulls that code, plus `codavox-code-id` and `codavox-code-content`, which OpenVox Server invokes on every catalog compile |

The `codavox-code-id` and `codavox-code-content` symlinks matter on the
**compilers** — that is where OpenVox Server compiles catalogs and calls them.
So on each compiler, after installing, you point OpenVox Server at codavox and
run the agent; see [Wiring into puppetserver](commands.md#wiring-into-puppetserver)
and [agent.md](agent.md).

## Installing is safe; the cutover is the careful part

Installing the package changes nothing about how a compiler compiles catalogs
(see [What the package installs](#what-the-package-installs)) — a compiler that
has the package but is not yet wired behaves exactly as before. The care is in
*wiring* one, because codavox has no fallback: once you point `environmentpath`
at codavox and set the two commands, catalog compilation depends on the agent
having deployed code there. Wire a compiler before its agent has converged and
every catalog compile fails — loudly, which is the point, but it fails.

So bring a compiler online in this order:

1. Install the package (inert).
2. Run `codavox agent` and let it converge — the environment links now exist.
3. *Then* set `versioned-code.conf` and `environmentpath`.
4. Canary one compiler and confirm catalogs compile before rolling the fleet.

It is reversible: [remove the package](#upgrading-and-removing) and revert those
two settings, and the compiler is back to stock.

## Install

Download the package for your platform from the
[releases page](https://github.com/miharp/codavox/releases), then install it by
URL. Both package managers resolve dependencies and support clean removal when
installing from a file.

```console
dnf install https://github.com/miharp/codavox/releases/download/v0.2.0/codavox_0.2.0_linux_arm64.rpm
```

```console
curl -fsSLO https://github.com/miharp/codavox/releases/download/v0.2.0/codavox_0.2.0_linux_arm64.deb
apt-get install -y ./codavox_0.2.0_linux_arm64.deb
```

There is no package repository yet. Hosting one means `createrepo`, an apt
repository, and GPG key generation, distribution, and rotation — a standing
commitment rather than a build step. Installing by URL gives dependency
resolution, upgrade, and clean uninstall in the meantime.

## What the package installs

```text
/usr/bin/codavox
/usr/bin/codavox-code-id       -> codavox
/usr/bin/codavox-code-content  -> codavox
/opt/puppetlabs/codavox/versions/
/usr/lib/systemd/system/codavox-agent.service
/usr/lib/systemd/system/codavox-publish.service
/usr/lib/systemd/system/codavox-deploy-server.service
```

The two symlinks exist because OpenVox Server passes only positional arguments
to `code-id-command`, so it cannot invoke a subcommand. codavox dispatches on
`argv[0]`. See [commands.md](commands.md).

The binary installs to `/usr/bin`, not `/opt/puppetlabs/bin`. That directory
belongs to the openvox-agent package, and shipping into it risks file conflicts
on upgrade. `versioned-code.conf` takes an absolute path, so nothing is gained
by co-locating.

**Installing the package changes no configuration.** The postinstall runs only
`systemctl daemon-reload`; no unit is enabled or started, and nothing is written
to `puppetserver`'s config. Which node runs which daemon is your choice.

## Running the daemons

The three units are driven by [`/etc/codavox/config.yaml`](configuration.md), so
you enable the ones a node needs and the config supplies their settings:

```console
# on the primary
systemctl enable --now codavox-publish        # needs config: staging
systemctl enable --now codavox-deploy-server   # needs config: api_token and/or secret

# on each compiler (after wiring OpenVox Server — see below)
systemctl enable --now codavox-agent           # needs config: agent.publisher
```

`systemctl reload codavox-publish` sends the publisher a `SIGHUP` to reseal after
a deploy, which is what r10k's `postrun` hook can call.

Wiring codavox into OpenVox Server on a compiler is a separate, deliberate step —
see [Wiring into puppetserver](commands.md#wiring-into-puppetserver) and the safe
order above.

## Verify

```console
codavox version
codavox-code-id production
```

Before any code is deployed, `codavox-code-id` exits non-zero because the
environment link does not exist yet. That is correct: codavox never invents a
`code_id`.

## Upgrading and removing

```console
dnf upgrade codavox
dnf remove codavox
```

```console
apt-get install --only-upgrade ./codavox_<version>_linux_arm64.deb
apt-get purge codavox
```

Removal leaves `/opt/puppetlabs/codavox/versions/` in place if it contains
deployed code. Delete it by hand if you want the node fully cleaned.

## Building packages locally

Requires [GoReleaser](https://goreleaser.com).

```console
goreleaser release --snapshot --clean --skip=publish
```

Packages land in `dist/`. To test one end to end:

```console
docker run --rm --platform linux/arm64 -v "$PWD/dist:/dist:ro" rockylinux:9 \
  bash -c 'dnf install -y /dist/codavox_*_linux_arm64.rpm && codavox version'
```
