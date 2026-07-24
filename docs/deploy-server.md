# Deploy server

`codavox deploy-server` is the deploy control plane on the primary. It exposes
two front doors — a token-authenticated **deploy API** and a secret-authenticated
**webhook** — that feed one queue, one worker, and one deploy history. However a
deploy is triggered, it runs through the same path
([deploying.md](deploying.md)) and appears in the same history.

```console
codavox deploy-server \
  --api-token /etc/codavox/api.token \
  --secret /etc/codavox/webhook.secret \
  --staging /etc/puppetlabs/code-staging
```

Enable whichever front doors you need: `--api-token` turns on the API,
`--secret` turns on the webhook, and at least one is required. `codavox webhook`
is an alias that runs this command with only the webhook.

## Options

| flag | default | purpose |
|---|---|---|
| `--api-token` | | File holding the API bearer token; enables the deploy API |
| `--secret` | | File holding the webhook shared secret; enables the webhook |
| `--listen` | `:8170` | Address to serve on |
| `--no-tls` | | Serve plain HTTP, for a setup that terminates TLS at a proxy |
| `--history` | `100` | Deploy records to retain in memory |
| `--staging` | *required* | r10k's basedir, the same the publisher serves |
| `--state` | `<root>/state` | Publisher state directory (to signal the reseal) |
| `--r10k` | `r10k` on `PATH`, then `/opt/puppetlabs/puppet/bin/r10k` | r10k binary |
| `--r10k-config` | r10k's default | r10k.yaml passed with `--config` |
| `--certname` | system hostname | Node's Puppet certname, for the server certificate |
| `--ssldir` | `/etc/puppetlabs/puppet/ssl` | Puppet SSL directory |

## The deploy API

Authenticate with `Authorization: Bearer <token>`, matching `--api-token`.

### `POST /v1/deploys`

```json
{ "environments": ["production"], "wait": false }
```

Give `environments` or `"all": true`, not both. Returns a deploy record. Without
`wait` the response is `202 Accepted` with the record `queued`; with
`"wait": true` it blocks until the deploy is terminal and returns `200` with the
final record.

```console
$ curl -H 'Authorization: Bearer <token>' \
    -d '{"environments":["production"],"wait":true}' \
    https://primary:8170/v1/deploys
{ "id": "e210f6…", "status": "complete", "source": "api",
  "environments": ["production"],
  "results": [ { "environment": "production", "code_id": "778ae2…",
                 "commit": "cafe123" } ] }
```

### `GET /v1/deploys/{id}` and `GET /v1/deploys`

The record for one deploy, or the retained history newest-first. Both require the
bearer token.

### Deploy record

| field | meaning |
|---|---|
| `id` | Deploy identifier |
| `status` | `queued`, `running`, `complete`, or `failed` |
| `source` | `api` or `webhook` |
| `environments` | Environments deployed (filled from results for an `all` deploy) |
| `submitted_at` / `started_at` / `finished_at` | Lifecycle timestamps |
| `results` | Per-environment `code_id`, `commit`, and any `error` |
| `error` | Set when the deploy failed |

`status` reaches `complete` once the deploy has landed on the primary — r10k ran,
the tree is sealed, and the publisher was signaled. Compilers converge on their
own by polling, so the record does not wait for every compiler.

## The webhook

A push maps to an environment and deploys it. Point your control repo at
`https://<primary>:8170/v1/webhook`.

The provider is detected from headers; one shared secret authenticates all three.

| Provider | Detected by | Authentication |
|---|---|---|
| GitHub | `X-GitHub-Event` | `X-Hub-Signature-256`, an HMAC-SHA256 of the body keyed by the secret |
| GitLab | `X-Gitlab-Event` | `X-Gitlab-Token` equals the secret |
| Generic | neither header | `Authorization: Bearer <secret>` or `?token=<secret>` |

The pushed branch becomes an environment, sanitizing non-word characters to `_`
as r10k does, so `feature/new-thing` becomes `feature_new_thing`. A generic
caller can name the environment directly with `{"environment":"production"}`.

A push is acknowledged `202` and deployed in the background: git providers time
out in seconds but r10k does not. These are acknowledged `200` and deploy
nothing: pings, non-push events, branch deletions (removing an environment is not
yet supported), and tags or other non-branch refs.

GitHub's HMAC never puts the secret on the wire; GitLab and the generic form send
it in a header, which is why TLS is on by default.

## Serialized deploys

The server runs one deploy at a time, and `internal/deploy` also takes a staging
lock (see [deploying.md](deploying.md#concurrent-deploys)), so an API deploy, a
webhook deploy, and a `codavox deploy` on the command line never run r10k over
each other.

## Credentials

`--api-token` and `--secret` each point at a **file**, not a value, so a
credential never appears in a process list. Comparisons are constant-time.
Configure the same webhook secret in GitHub, GitLab, or your caller, and
distribute the API token to whatever drives deploys.

## TLS

By default the server serves HTTPS with the node's Puppet certificate and
requires **no** client certificate — GitHub and GitLab cannot present one, and
the API caller authenticates with a bearer token. Use `--no-tls` only behind a
proxy that terminates TLS. Unlike the publisher's artifact API, this endpoint is
not protected by mutual TLS; its shared secret and bearer token are the whole
authentication.

## Endpoints

- `POST /v1/deploys`, `GET /v1/deploys`, `GET /v1/deploys/{id}` — the deploy API.
- `POST /v1/webhook` — the push receiver.
- `GET /v1/health` — returns `{"status":"ok"}`.
