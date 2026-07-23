# The `versioned-code-service` contract

Everything here was verified by reading source, not documentation. Sources:

- `~/projects/openvox-server` @ `a2e0bb8a`
- PE 2025.11.0 packages (`pe-puppetserver`, `pe-modules`, `pe-r10k`)

This is the interface `stagehand` must satisfy. It is stable, open source, and
already enabled in shipped openvox-server packages — **no server changes are
required.**

## The service exists and is on by default

`src/clj/puppetlabs/services/versioned_code_service/` is present in
openvox-server, and the service is registered in
`ezbake/system-config/services.d/bootstrap.cfg` — the *packaging* bootstrap, not
merely the dev one:

```
puppetlabs.services.versioned-code-service.versioned-code-service/versioned-code-service
```

PE swaps in its own implementation of the same interface
(`puppetlabs.enterprise.services.file-sync.file-sync-versioned-code-service`),
compiled into `puppet-server-release.jar` alongside the OSS one. File sync is
*an* implementation of this hook, not the mechanism itself. That is precisely
why stagehand can exist without touching the server.

## Configuration

`/etc/puppetlabs/puppetserver/conf.d/versioned-code.conf`:

```hocon
versioned-code: {
  code-id-command: "/opt/puppetlabs/bin/stagehand-code-id"
  code-content-command: "/opt/puppetlabs/bin/stagehand-code-content"
}
```

`validate-config!` enforces **both-or-neither**. Setting exactly one throws
`IllegalStateException` at startup:

> Only one of "versioned-code.code-id-command" and
> "versioned-code.code-content-command" was set. Both or neither must be set.

## The two commands

```
code-id-command      <environment>                        -> stdout = code_id
code-content-command <environment> <code-id> <file-path>  -> stdout = file bytes
```

Behaviour, from `versioned_code_core.clj`:

- **Exit 0 is mandatory.** Non-zero throws `IllegalStateException` carrying
  exit code, stdout and stderr into the server log.
- **stderr on a zero exit is tolerated but logged at ERROR level.** Keep both
  commands silent on success or the log fills at one line per catalog compile.
- code-id stdout is `trim-newline`'d. Nothing else is normalised — there is an
  explicit `TODO` in the source about control characters and encodings, so do
  not emit anything exotic.
- code-content uses `execute-command-streamed`, so file bytes stream rather
  than buffering in the JVM heap. Large files are safe.

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
  sanitises `\W` -> `_` (`lib/r10k/action/deploy/environment.rb:41`), but the
  two agree by coincidence rather than contract. Test it explicitly.

## The performance constraint (this drives the language choice)

`current-code-id` is invoked from `with-code-id` in
`src/clj/puppetlabs/services/request_handler/request_handler_core.clj:232`,
inside the request handler, whenever `:include-code-id?` is set.

**There is no caching anywhere in that path.** Every catalog request spawns
`code-id-command` as a fresh process.

At 1000 nodes on a 30-minute interval that is ~33 spawns/sec across the fleet,
on the critical path of every compile:

| implementation | approx startup | CPU per wall-clock second |
|---|---|---|
| Go/Rust static binary | 1-2 ms | negligible |
| shell script (readlink) | 1-2 ms | negligible |
| Ruby script | ~100 ms | ~3 s — unusable |

This is almost certainly why PE bypassed the script hook entirely with an
in-process service. It is the single strongest argument that the compiler-side
components must be compiled binaries, and it rules out the otherwise-natural
instinct to write them in Ruby.

**Design consequence:** the answer only changes at deploy time, so the agent
should write the current code_id to a small file and `code-id-command` becomes
a single `read` syscall — no git invocation, no directory walk, no lock.

## Corroborating evidence from PE

- `puppet_enterprise::master::file_sync_disabled` exists specifically to turn
  file sync off and let an operator supply their own two commands. It registers
  the OSS `versioned-code-service` and writes exactly the config above. PE
  ships the seam deliberately.
- `parse_code_mgmt_git_config.rb` hardcodes `'provider' => git_provider || 'rugged'`.
- Code Manager deploy-pool workers **shell out** to r10k, one environment per
  worker (`deploy_pool_size = 2`, `download_pool_size = 4`); each worker holds
  its own cache copy, which is how PE sidesteps the shared-cachedir race that
  r10k commit `e27a5d14` (#1058) fixed by serialising.
- PE is deprecating non-versioned deploys: `code_management.pp` warns the
  `versioned_deploys` default will flip `false` -> `true`. Build versioned-dir
  + symlink-swap from day one; do not build the thing they are retiring.
