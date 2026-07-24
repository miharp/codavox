# Webhooks

`codavox webhook` deploys on a control-repo push. It is a long-running receiver
on the primary that maps the pushed branch to an environment and deploys it —
the same "push and it deploys" reflex as Code Manager's webhook.

```console
codavox webhook --secret /etc/codavox/webhook.secret \
  --staging /etc/puppetlabs/code-staging
```

Point your control repo's webhook at `https://<primary>:8170/v1/webhook`.

## What a push does

1. Authenticate the request against the shared secret.
2. Map the pushed branch to an environment: `refs/heads/production` →
   `production`, sanitizing non-word characters to `_` the way r10k names
   environments, so `feature/new-thing` becomes `feature_new_thing`.
3. Deploy that one environment — `codavox deploy <environment>` under the hood,
   the same r10k-then-reseal path as the command.

A push is acknowledged with `202 Accepted` and the deploy runs in the
background: git providers time out in seconds, but r10k does not, so the
receiver returns immediately and deploys asynchronously. Deploys run **one at a
time** — r10k mutates the staging directory in place, so a burst of pushes
queues behind a single worker rather than racing.

## It is the same deploy path

The receiver is a front door onto `internal/deploy`, not a second
implementation: a webhook deploy and a `codavox deploy` converge to the same
state, and both trigger the publisher the same way. See
[deploying.md](deploying.md).

## Providers

The provider is detected from the request headers; one shared secret
authenticates all three.

| Provider | Detected by | Authentication |
|---|---|---|
| GitHub | `X-GitHub-Event` | `X-Hub-Signature-256`, an HMAC-SHA256 of the body keyed by the secret |
| GitLab | `X-Gitlab-Event` | `X-Gitlab-Token` equals the secret |
| Generic | neither header | `Authorization: Bearer <secret>` or `?token=<secret>` |

GitHub's HMAC never puts the secret on the wire, so it is safe even without TLS.
GitLab and the generic form send the secret in a header, which is why TLS is on
by default.

A generic caller can name the environment directly instead of a ref:

```console
curl -H 'Authorization: Bearer <secret>' \
  -d '{"environment":"production"}' \
  https://primary:8170/v1/webhook
```

## What is ignored

These are acknowledged with `200 OK` — a success the provider records without
retrying — but deploy nothing:

- pings and non-push events
- branch deletions (removing an environment is not yet supported)
- tags and other non-branch refs, which do not name environments

## The secret

`--secret` points at a file, not a value, so the secret never appears in a
process list or shell history. Configure the same string as the webhook secret
in GitHub, GitLab, or your caller. Comparisons are constant-time.

## Options

| flag | default | purpose |
|---|---|---|
| `--secret` | *required* | File holding the shared secret |
| `--listen` | `:8170` | Address to serve on |
| `--no-tls` | | Serve plain HTTP, for a setup that terminates TLS at a proxy |
| `--staging` | *required* | r10k's basedir, the same the publisher serves |
| `--state` | `<root>/state` | Publisher state directory (to signal the reseal) |
| `--r10k` | `r10k` on `PATH`, then `/opt/puppetlabs/puppet/bin/r10k` | r10k binary |
| `--r10k-config` | r10k's default | r10k.yaml passed with `--config` |
| `--certname` | system hostname | Node's Puppet certname, for the server certificate |
| `--ssldir` | `/etc/puppetlabs/puppet/ssl` | Puppet SSL directory |

## TLS

By default the receiver serves HTTPS with the node's Puppet certificate. Unlike
the publisher's artifact API, it does **not** require a client certificate —
GitHub and GitLab cannot present one — so the shared secret is the whole
authentication. Use `--no-tls` only behind a proxy that terminates TLS.

## Endpoints

- `POST /v1/webhook` — the receiver.
- `GET /v1/health` — returns `{"status":"ok"}`.
