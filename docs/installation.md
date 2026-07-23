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

## Install

Download the package for your platform from the
[releases page](https://github.com/miharp/codavox/releases), then install it by
URL. Both package managers resolve dependencies and support clean removal when
installing from a file.

```console
dnf install https://github.com/miharp/codavox/releases/download/v0.1.0/codavox_0.1.0_linux_aarch64.rpm
```

```console
curl -fsSLO https://github.com/miharp/codavox/releases/download/v0.1.0/codavox_0.1.0_linux_arm64.deb
apt-get install -y ./codavox_0.1.0_linux_arm64.deb
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
/opt/puppetlabs/codavox/state/
```

The two symlinks exist because OpenVox Server passes only positional arguments
to `code-id-command`, so it cannot invoke a subcommand. codavox dispatches on
`argv[0]`. See [commands.md](commands.md).

The binary installs to `/usr/bin`, not `/opt/puppetlabs/bin`. That directory
belongs to the openvox-agent package, and shipping into it risks file conflicts
on upgrade. `versioned-code.conf` takes an absolute path, so nothing is gained
by co-locating.

**Installing the package changes no configuration.** It does not enable a
service or write to `puppetserver`'s config. Wiring codavox into OpenVox Server
is a separate, deliberate step — see
[Wiring into puppetserver](commands.md#wiring-into-puppetserver).

## Verify

```console
codavox version
codavox-code-id production
```

Before any code is deployed, `codavox-code-id` exits non-zero because no state
file exists. That is correct: codavox never invents a `code_id`.

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
