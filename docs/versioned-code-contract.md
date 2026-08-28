# The `versioned-code-service` contract

Everything here was verified by reading source, not documentation.

Source: `~/projects/openvox-server` @ `a2e0bb8a`

This is the interface `codavox` must satisfy. It is stable, open source, and
already enabled in shipped openvox-server packages — **no server changes are
required.**

## The service exists and is on by default

`src/clj/puppetlabs/services/versioned_code_service/` is present in
openvox-server, and the service is registered in
`ezbake/system-config/services.d/bootstrap.cfg` — the *packaging* bootstrap, not
merely the dev one:

```text
puppetlabs.services.versioned-code-service.versioned-code-service/versioned-code-service
```

The service is a pluggable hook: puppetserver delegates "what is the current
code version" and "give me file X at version Y" to two external commands. Any
implementation that satisfies the contract below works, which is why codavox
can exist entirely outside the server.

## Configuration

`/etc/puppetlabs/puppetserver/conf.d/versioned-code.conf`:

```hocon
versioned-code: {
  code-id-command: "/opt/puppetlabs/bin/codavox-code-id"
  code-content-command: "/opt/puppetlabs/bin/codavox-code-content"
}
```

`validate-config!` enforces **both-or-neither**. Setting exactly one throws
`IllegalStateException` at startup:

> Only one of "versioned-code.code-id-command" and
> "versioned-code.code-content-command" was set. Both or neither must be set.

## The two commands

```text
code-id-command      <environment>                        -> stdout = code_id
code-content-command <environment> <code-id> <file-path>  -> stdout = file bytes
```

Behavior, from `versioned_code_core.clj`:

- **Exit 0 is mandatory.** Non-zero throws `IllegalStateException` carrying
  exit code, stdout and stderr into the server log.
- **stderr on a zero exit is tolerated but logged at ERROR level.** Keep both
  commands silent on success or the log fills at one line per catalog compile.
- code-id stdout is `trim-newline`'d. Nothing else is normalized — there is an
  explicit `TODO` in the source about control characters and encodings, so do
  not emit anything exotic.
- code-content uses `execute-command-streamed`, so file bytes stream rather
  than buffering in the JVM heap. Large files are safe.

## With nothing configured, catalogs quietly stop being static

`current-code-id` returns `nil` when `code-id-command` is unset
(`versioned_code_service.clj:24-28`). The service announces it once at INFO
during init (`:17-18`), and never again:

> No code-id-command set for versioned-code-service. Code-id will be nil.

That `nil` reaches the catalog compiler, which gates static compilation on four
conditions at once (`lib/puppet/indirector/catalog/compiler.rb:310`, in the
`ruby/puppet` submodule @ `77cc7a49da`, openvox 8.21.0):

```ruby
if node.environment && node.environment.static_catalogs? && options[:static_catalog] && options[:code_id]
  checksum_type = common_checksum_type(options[:checksum_type])
```

`checksum_type` stays `nil` if any one of the four fails, and the compile log
line is selected from it a few lines down: `Compiled static catalog for ...`
when it is set, `Compiled catalog for ...` when it is not.

So `static_catalogs` defaulting to `true` is not enough on its own, and the
downgrade is silent: no error, no per-compile warning, just an ordinary catalog.
Puppet's own setting description says as much (`lib/puppet/defaults.rb:258`),
that static catalog compilation "occurs only on Puppet Server when the
`code-id-command` and `code-content-command` settings are configured".

**The compile log is the check.** `Compiled catalog` means no `code_id` reached
the compiler, whatever `environment.conf` says. `config_version` is a separate
setting that labels the catalog and has no bearing on this gate.

## Validation landmines

Both from `src/clj/puppetlabs/puppetserver/common.clj`:

```clojure
(def CodeId      ;; only alphanumerics and - _ ; :
  (schema/pred (comp not (partial re-find #"[^_\-:;a-zA-Z0-9]")) "code-id"))

(def Environment ;; alphanumeric and _ only
  (schema/pred (comp not nil? (partial re-matches #"\w+")) "environment"))
```

- **code_id rejects `/`, `.`, `+`, `=`.** A hex git SHA is fine. `<env>_<sha>`
  is fine. A base64 or otherwise padded content hash will be **rejected at
  runtime** by `get-current-code-id!`. Use hex.
- **Environment names are `\w+` only.** This happens to agree with r10k, which
  sanitizes `\W` -> `_` (`lib/r10k/action/deploy/environment.rb:41`), but the
  two agree by coincidence rather than contract. Test it explicitly.

## The environment cache is the server's, and a deploy must expire it

Verified at openvox-server `d4000e57` (main, 2026-08-28) and openvox
`96aaa7cf`; this section is newer than the commit at the top.

Nothing in the versioned-code service tells the server that code changed.
`code-id-command` is spawned fresh on every compile, but what it compiles comes
from the environment cache, which lives for `environment_timeout`
(`lib/puppet/defaults.rb:668-678`): the default `0` disables caching,
`unlimited` — what the setting's own description tells you to move to in
production — holds an environment "until the server is restarted or told to
refresh the cache." A server holding the cache never re-reads the environment
symlink, so a swap alone leaves it compiling the old tree and stamping the
result with the new `code_id`.

"Told to refresh" is the Puppet Admin API, mounted at `/puppet-admin-api`
(`ezbake/config/conf.d/web-routes.conf:9`), from
`src/clj/puppetlabs/services/puppet_admin/puppet_admin_core.clj`:

```text
DELETE /puppet-admin-api/v1/environment-cache?environment=<name>   -> 204
```

- The route reads the `environment` query parameter (`:82-84`). With it, the
  resource expires that one environment; without it, every environment
  (`:49-53`). Expire one: nothing else changed.
- It answers `204 No Content` with no body, and deliberately defines no media
  types so an `Accept: */*` client is not refused (`:27-47`).
- Expiry lands in the Clojure-side cache **and** in every JRuby instance's
  environment registry
  (`src/clj/puppetlabs/services/jruby/jruby_puppet_core.clj:417-441`), so one
  call covers the whole pool.

**The shipped `auth.conf` denies it.** `ezbake/config/conf.d/auth.conf` has no
rule for `/puppet-admin-api`, so the request falls to `puppetlabs deny all`
(`:317-327`, `sort-order: 999`). The admin handler is wrapped in the ordinary
trapperkeeper authorization (`puppet_admin_core.clj:117-137`), so a rule for the
path is all it takes; the older `puppet-admin.client-whitelist` setting still
works but logs a deprecation warning at startup and is slated for removal
(`puppet_admin_service.clj:22-33`). codavox's rule is in
[agent.md](agent.md#the-server-has-to-allow-it).

**On a compiler, extension short names do not resolve.** The admin service
takes its authorization handler from the CA service (`puppet_admin_service.clj:
12-13, 38`). The real CA service builds that handler with Puppet's
OID-to-short-name map, `puppet-short-names` merged with any
`trusted-oid-mapping-file`
(`src/clj/puppetlabs/services/ca/certificate_authority_service.clj:44-46`,
`src/clj/puppetlabs/puppetserver/certificate_authority.clj:310-341,
2280-2283`). The disabled CA service — what every compiler runs — returns the
bare `wrap-with-authorization-check`
(`src/clj/puppetlabs/services/ca/certificate_authority_disabled_service.clj:
25-27`), which trapperkeeper-authorization calls with `{:oid-map {}}`, so the
only extension it can name is `subject-alt-name`. An `allow` written
`extensions: { pp_role: ... }` therefore matches nothing on a compiler and the
request is "denied by rule". Keying the extension by its OID
(`"1.3.6.1.4.1.34380.1.1.13"`) matches on both, because an unmapped OID is
carried through as itself and a mapped one is translated on the rule side too.
Verified live: the short-name rule answered 403, the OID rule 204, on the same
server.

**Design consequence:** the agent expires the environment after every swap,
and treats a refused flush as a failed reconciliation. A `code-id-command` that
answers correctly while the server compiles something else is the exact
mismatch this whole contract exists to prevent.

## The performance constraint (this drives the language choice)

`current-code-id` is invoked from `with-code-id` in
`src/clj/puppetlabs/services/request_handler/request_handler_core.clj:232`,
inside the request handler, whenever `:include-code-id?` is set.

**There is no caching anywhere in that path.** Every catalog request spawns
`code-id-command` as a fresh process.

At 1000 nodes on a 30-minute interval that is ~33 spawns/sec across the fleet,
on the critical path of every compile:

| implementation | approx startup | CPU per wall-clock second |
| --- | --- | --- |
| Go static binary | 1-2 ms | negligible |
| shell script (readlink) | 1-2 ms | negligible |
| Ruby script | ~100 ms | ~3 s — unusable |

This is the single strongest argument that the compiler-side components must be
compiled binaries, and it rules out the otherwise-natural instinct to write
them in Ruby.

**Design consequence:** the answer only changes at deploy time, so the agent
should write the current code_id to a small file and `code-id-command` becomes
a single `read` syscall — no git invocation, no directory walk, no lock.

If per-compile spawn cost ever proves too high even for a compiled binary, the
fallback is to implement an in-JVM service satisfying the same protocol inside
openvox-server. That is an escape hatch to reach for with measurements in hand,
not a starting point.
