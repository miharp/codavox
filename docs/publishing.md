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

Only the **current** `code_id` is servable; a stale one returns `404`. Serving
arbitrary historical versions would mean keeping every past tree on the
publisher. Compilers retain old versions themselves, which is what in-flight
agent runs actually need.

Served `immutable` with a one-year max-age. The body is content-addressed by
the `code_id` in the URL, so it can never change meaning.

The archive is streamed rather than buffered — environments reach hundreds of
megabytes and several compilers may poll at once.

### `GET /v1/health`

```json
{"status": "ok"}
```

## Resealing

Sealing walks and hashes an entire environment, so it happens on `Reseal`, not
per request. Two compilers polling either side of an r10k run would otherwise
observe different ids for what is meant to be one deploy.

The publisher seals once at startup. Triggering a reseal after each r10k run is
the operator's job for now; a watch mode is planned.

A directory whose name OpenVox Server would reject is skipped rather than
treated as fatal — one badly named directory in the staging area should not
stop every other environment from being published.

## Testing it

The end-to-end test builds the real binary, lays out SSL material the way ovadm
leaves it on a node, and checks over an actual TLS connection that a compiler
is admitted and an ordinary agent is refused:

```console
go test ./internal/publish/ -run TestPublishBinaryEndToEnd -v
```
