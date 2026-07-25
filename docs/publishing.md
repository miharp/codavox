# Publishing

The publisher seals staged environments and serves them to compilers over
mutual TLS.

```console
codavox publish --staging /etc/puppetlabs/code-staging
```

```text
sealed production 3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb
listening on :8150 as puppet.example.com (roles: openvox_compiler)
```

## Options

| flag | default | purpose |
|---|---|---|
| `--staging` | *required* | Directory holding one subdirectory per environment |
| `--listen` | `:8150` | Address to serve on |
| `--certname` | system hostname | Node's Puppet certname |
| `--ssldir` | `/etc/puppetlabs/puppet/ssl` | Puppet SSL directory |
| `--allow-role` | `openvox_compiler` | `pp_role` permitted to fetch code; repeatable |
| `--state` | `<root>/state` | Directory for the provenance log and materialized artifacts |

**codavox stages nothing.** It reads a directory r10k already populated.
Not owning the deploy keeps the trust boundary small and lets existing r10k
workflows continue untouched.

## Identity: no second PKI

codavox issues no certificates and runs no CA. Every node in an OpenVox
deployment is already enrolled with the primary's CA — the agent run that joins
a compiler to the pool leaves a signed certificate, a private key, the CA
certificate, and a CRL at well-known paths:

```text
/etc/puppetlabs/puppet/ssl/certs/<certname>.pem
/etc/puppetlabs/puppet/ssl/private_keys/<certname>.pem
/etc/puppetlabs/puppet/ssl/certs/ca.pem
/etc/puppetlabs/puppet/ssl/crl.pem
```

codavox reuses them. There is nothing to provision, distribute, or rotate, and
revoking a compiler's Puppet certificate revokes its access to code as a side
effect — which is what an operator would expect to happen.

## Revocation: enforced, not assumed

That side effect is a real check, not a hope. A revoked certificate stays
cryptographically valid and keeps its `pp_role`, so mutual TLS on its own would
go on admitting a compiler forever after you revoked it.

The publisher therefore reads `<ssldir>/crl.pem` — the same file every other
Puppet service checks — and refuses any peer whose certificate appears on it.
This follows PE, whose Puppet module sets `ssl-crl-path` to `$ssldir/crl.pem` on
every service listener and leaves puppetserver on puppet.conf's
`certificate_revocation` default.

`publish.certificate_revocation` takes Puppet's own values:

| value | effect |
|---|---|
| `chain` | check every certificate the peer presented. **Default**, as in Puppet. |
| `leaf` | check only the peer's own certificate. |
| `false` | skip the CRL entirely. |

Three properties follow from how it is implemented, and all are deliberate:

- **The CRL is re-read when it changes**, and checked **on every request**, not
  only when a connection is established. That distinction is the whole point: an
  agent polls over one keep-alive connection and never handshakes again, so a
  handshake-only check would keep serving a compiler revoked minutes ago until
  its connection happened to drop — on precisely the node you are trying to cut
  off. Revoking with
  `puppetserver ca revoke --certname compiler02.example.com` takes effect on
  that compiler's next poll, with no restart of anything.
- **A missing or unverifiable CRL is a startup error**, not a silent downgrade
  to "nothing is revoked". The CRL's signature is checked against the CA
  bundle, so a CRL written by anyone other than your CA is refused rather than
  believed. An estate that distributes no CRL sets `certificate_revocation` to
  `false` and says so out loud.

## Authorization: CA membership is not enough

Requiring a certificate signed by the Puppet CA proves only that the peer is
*some* enrolled node. **Every agent in the estate clears that bar.** Puppet
manifests routinely reference internal hostnames, credential paths, and
topology, so a compromised leaf node should not be able to read the whole
estate's code.

The publisher therefore also requires a `pp_role` certificate extension
(OID `1.3.6.1.4.1.34380.1.1.13`) matching `--allow-role`. `ovadm::add_compiler`
writes `pp_role: openvox_compiler` into `csr_attributes.yaml` before the CSR is
submitted, so a compiler's signed certificate carries its role and no extra
configuration is needed.

The check runs in TLS `VerifyConnection` rather than `VerifyPeerCertificate`.
That distinction matters: **`VerifyPeerCertificate` is skipped entirely on
resumed sessions**, so a peer that completed one handshake could keep
reconnecting without the role ever being rechecked.

`ServerTLS` refuses to build a configuration with no allowed roles, so the
constraint cannot be omitted by accident.

## API

### `GET /v1/environments`

```json
{
  "production": "3224ddbe7e3d05fe236823b4596fac8eeebc9ceb38c47d551de912b496884beb",
  "testing": "9a1f0c4e2b8d7f36a5c19e04b7d2836af41c9e5d0b8a37f26c1d4e90a5b8c3f7"
}
```

Served `no-store`. Polling is the correctness mechanism, so a cached response
would pin a compiler to a stale version and defeat convergence.

### `GET /v1/artifact/{environment}/{code_id}`

Returns the deterministic gzipped tar for that version.

The artifact is **materialized at seal time**, written to `<state>/artifacts`
and served from there — never tarred from the staging directory on demand. This
is what makes serving safe while r10k is mid-deploy: the bytes a compiler
downloads are the snapshot taken when the tree was quiescent, so an overwrite in
progress can never be streamed as a half-written archive whose bytes no longer
match the advertised `code_id`.

Only the **current** `code_id` is servable; a stale one returns `404`, because
it is the only artifact kept on disk. A superseded artifact is reaped on the
next reseal — safely, since an in-flight download holds an open descriptor and
finishes even after the file is unlinked. Compilers retain old *versions*
themselves, which is what in-flight agent runs actually need.

Served `immutable` with a one-year max-age. The body is content-addressed by
the `code_id` in the URL, so it can never change meaning.

### `GET /v1/health`

```json
{"status": "ok"}
```

## Resealing

Sealing walks and hashes an entire environment, so it happens on `Reseal`, not
per request. Two compilers polling either side of an r10k run would otherwise
observe different ids for what is meant to be one deploy.

The publisher seals once at startup and again on every **`SIGHUP`**. Wire that
to the deploy:

```yaml
# r10k.yaml
postrun: ['/bin/sh', '-c', 'systemctl reload codavox-publish']
```

or send the signal directly (`systemctl reload codavox-publish`, or
`kill -HUP <pid>`). Because the signal fires only after r10k has returned, the
staging tree is quiescent when the reseal runs, so no reseal ever observes a
half-written deploy. This is deliberately an *explicit post-deploy trigger*
rather than a filesystem watch: codavox does not own the deploy, and a watch
would have to reconstruct the "deploy finished" signal that `postrun` already
provides — the same shape as PE's Code Manager committing after r10k, only
across a process boundary because codavox observes rather than runs r10k.

`SIGTERM` and interrupt shut the server down gracefully, draining in-flight
downloads.

A directory whose name OpenVox Server would reject is skipped rather than
treated as fatal — one badly named directory in the staging area should not
stop every other environment from being published.

## Provenance

A `code_id` is a content hash, so it answers "is this compiler serving the same
code as that one?" but not "which control-repo commit produced it?" — the
artifact deliberately excludes `.git` and `.r10k-deploy.json`, so a compiler
carries no way back to a commit.

The publisher closes that gap. When it seals an environment it reads r10k's
`.r10k-deploy.json` from the staging tree — which is still on disk, only
*excluded from sealing* — and appends a record to a local log:

```text
<state>/provenance.jsonl
```

Each line maps a `code_id` to the control-repo commit (`signature`), r10k's
deploy timestamp, and when the publisher sealed it. Query it with
[`codavox provenance`](commands.md#codavox-provenance).

Three properties are deliberate:

- **Publisher-only.** The log never enters an artifact and never reaches a
  compiler, so it cannot influence a `code_id` or the bytes a compiler serves.
  Reading `.r10k-deploy.json` here has no effect on sealing, which still excludes
  it.
- **One `code_id`, many commits.** A commit that does not change resolved content
  seals to the same `code_id`, so an id can trace back to several commits. That
  is recorded history, not a conflict.
- **Best-effort, never load-bearing.** A missing or malformed `.r10k-deploy.json`
  records nothing and never fails a deploy. This is not a violation of the
  no-fallbacks rule: that rule forbids serving *wrong content* while reporting
  success. Provenance is diagnostic metadata, and its honest absence — reported
  as "no provenance recorded" — is the correct answer, never a stand-in from a
  different version.

## Testing it

The end-to-end test builds the real binary, lays out SSL material the way ovadm
leaves it on a node, and checks over an actual TLS connection that a compiler
is admitted and an ordinary agent is refused:

```console
go test ./internal/publish/ -run TestPublishBinaryEndToEnd -v
```
