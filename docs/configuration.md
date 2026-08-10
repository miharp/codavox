# Configuration

The operator commands read shared settings from a YAML file, so the paths and
options that otherwise repeat across `publish`, `deploy`, and `deploy-server` —
basedir, state, ssldir, certname, r10k — are set once.

```yaml
# /etc/codavox/config.yaml
basedir:  /etc/puppetlabs/code/environments
state:    /opt/puppetlabs/codavox/state
ssldir:   /etc/puppetlabs/puppet/ssl
certname: puppet.example.com
r10k:     /opt/puppetlabs/puppet/bin/r10k
r10k_config: /etc/puppetlabs/r10k/r10k.yaml

publish:
  listen: ":8150"
  allow_roles: [openvox_compiler]
  # For compilers whose certificates predate codavox and carry no pp_role.
  allow_certnames: []
  certificate_revocation: chain

deploy_server:
  listen: ":8170"
  api_token: /etc/codavox/api.token
  secret:    /etc/codavox/webhook.secret

agent:
  publisher: https://puppet.example.com:8150
  interval:  30s
  keep:      3
  min_age:   2h
```

## Location and precedence

The file is found in this order:

1. `--config <file>`
2. the `CODAVOX_CONFIG` environment variable
3. `/etc/codavox/config.yaml`

A file named by `--config` or `CODAVOX_CONFIG` that cannot be read is an error.
A missing file at the default location is not — it simply means no configuration.

For any single setting:

> **a flag overrides the file, and the file overrides the built-in default.**

So the file sets your site's values and a flag overrides one for a single run.

Shared settings sit at the top level; per-daemon settings sit in named sections,
so the two servers' distinct listen addresses do not collide.

## Which commands read it

`publish`, `agent`, `compilers`, `deploy`, `deploy-server`, and `provenance`.

**`code-id` and `code-content` deliberately do not.** OpenVox Server spawns them
on every static catalog compile, so they must stay a single symlink read and a
single file read, with no configuration file to parse on that path. They take
only `CODAVOX_ROOT` and `CODAVOX_ENVIRONMENTPATH`.

## Settings

| key | commands | flag |
|---|---|---|
| `basedir` | publish, deploy, deploy-server | `--basedir` |
| `state` | publish, compilers, deploy, deploy-server, provenance | `--state` |
| `ssldir` | publish, agent, compilers, deploy-server | `--ssldir` |
| `certname` | publish, agent, compilers, deploy-server | `--certname` |
| `environmentpath` | agent | `--environmentpath` |
| `r10k` | deploy, deploy-server | `--r10k` |
| `r10k_config` | deploy, deploy-server | `--r10k-config` |
| `publish.listen` | publish, compilers | `--listen` |
| `publish.allow_roles` | publish | `--allow-role` |
| `publish.allow_certnames` | publish | `--allow-certname` |
| `publish.certificate_revocation` | publish | `--certificate-revocation` |
| `deploy_server.listen` | deploy-server | `--listen` |
| `deploy_server.api_token` | deploy-server | `--api-token` |
| `deploy_server.secret` | deploy-server | `--secret` |
| `deploy_server.history` | deploy-server | `--history` |
| `agent.publisher` | agent, compilers | `--publisher` |
| `agent.interval` | agent | `--interval` |
| `agent.keep` | agent | `--keep` |
| `agent.min_age` | agent | `--min-age` |
| `agent.prune_environments` | agent | `--prune-environments` |

`r10k` and `r10k_config` point at r10k itself, not at your code: the control
repo is configured inside `r10k.yaml` (its `sources:` section), which codavox
reads only through r10k. Leave both unset to use the r10k already on the host.
See [Where your control repo is
configured](deploying.md#where-your-control-repo-is-configured).

## Typos are errors

An unknown key is rejected rather than ignored, so a misspelled setting fails
loudly instead of leaving you wondering why nothing changed:

```console
$ codavox deploy production --config /etc/codavox/config.yaml
codavox: parsing config /etc/codavox/config.yaml: yaml: unmarshal errors:
  line 1: field stagng not found in type config.Config
```
