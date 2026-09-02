#!/bin/bash
# Exercise every codavox feature end to end against the running two-node
# topology. Host-side: pokes both containers with docker exec. Sourced values
# and the topology come from run.sh, but this can also be run on its own against
# an already-provisioned stack (e.g. after KEEP=1 ./run.sh).
#
# No `set -e`: every feature runs so one failure does not hide the rest.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=test/integration/lib.sh
source "$here/lib.sh"

FAILED=0
pass() { ok "$1"; }
fail() { printf '\033[1;31m  FAIL\033[0m %s\n' "$1" >&2; FAILED=1; }

# Change the served tree so the next reseal yields a new code_id, then reseal.
bump_and_reseal() {
  docker exec "$PRIMARY" bash -lc \
    "echo '# bump $(date +%s%N)' >> /etc/puppetlabs/code/environments/production/manifests/site.pp"
  docker exec "$PRIMARY" systemctl reload codavox-publish
}

# Feature 1: the real contract — a genuine puppetserver, wired to codavox, emits
# a static catalog stamped with the code_id the compiler reports serving.
log "Feature 1 — puppetserver emits a static catalog carrying the served code_id"
docker exec "$COMPILER" bash -lc '/opt/puppetlabs/bin/puppet agent -t --server compiler >/tmp/agent.log 2>&1'
rc=$?
if [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]; then
  served=$(code_id "$COMPILER" production)
  incat=$(docker exec "$COMPILER" bash -lc \
    "grep -o '\"code_id\":\"[^\"]*\"' /opt/puppetlabs/puppet/cache/client_data/catalog/compiler.json | head -1")
  if [ -n "$served" ] && printf '%s' "$incat" | grep -q "$served"; then
    pass "catalog carries code_id $served"
  else
    fail "catalog code_id ($incat) does not match served ($served)"
  fi
else
  docker exec "$COMPILER" tail -20 /tmp/agent.log 2>/dev/null || true
  fail "puppet agent run failed (rc=$rc)"
fi

# Feature 2: a reseal on the primary yields a new code_id, and the compiler's
# agent converges on it by polling — no push.
log "Feature 2 — a reseal produces a new code_id the compiler converges on"
old=$(code_id "$COMPILER" production)
bump_and_reseal
if wait_for 45 "$COMPILER" "[ \"\$(codavox-code-id production)\" != '$old' ]"; then
  pass "code_id advanced $old -> $(code_id "$COMPILER" production)"
else
  fail "compiler did not converge on a new code_id (still $old)"
fi

# Feature 3: a compiler offline across a deploy misses nothing — it catches up on
# its next poll, with no event replayed to it.
log "Feature 3 — a compiler offline across a deploy catches up on the next poll"
before=$(code_id "$COMPILER" production)
docker exec "$COMPILER" systemctl stop codavox-agent
bump_and_reseal
sleep 8
during=$(code_id "$COMPILER" production)
if [ "$during" != "$before" ]; then
  fail "code_id changed while the agent was stopped ($before -> $during)"
else
  docker exec "$COMPILER" systemctl start codavox-agent
  if wait_for 45 "$COMPILER" "[ \"\$(codavox-code-id production)\" != '$before' ]"; then
    pass "caught up to $(code_id "$COMPILER" production) after restart, no replay"
  else
    fail "compiler did not catch up after the agent restarted"
  fi
fi

# Feature 4: a deploy expires OpenVox Server's environment cache.
#
# This compiler runs with environment_timeout = unlimited (provision-compiler.sh),
# as a production compiler does for compile speed, and a single JRuby instance
# so every compile hits the same cache. With the cache held, the symlink swap
# alone changes nothing the server compiles: it keeps the tree it parsed at its
# first catalog while code-id, reading the symlink just moved, reports the new
# code_id. The catalog that results is stamped with a code_id that does not
# describe it — the plausible-but-wrong answer static catalogs exist to rule
# out — so the assertion first reproduces that with the flush switched off, and
# only then shows the flush fixing it. A flush that was never needed proves
# nothing.
log "Feature 4 — a deploy expires the compiler's environment cache"

SITE_PP=/etc/puppetlabs/code/environments/production/manifests/site.pp
CATALOG=/opt/puppetlabs/puppet/cache/client_data/catalog/compiler.json

# compile_catalog — request a fresh catalog from the compiler; 0 and 2 are both
# a successful run.
compile_catalog() {
  docker exec "$COMPILER" bash -lc \
    '/opt/puppetlabs/bin/puppet agent -t --server compiler >/tmp/agent.log 2>&1; rc=$?; [ $rc -eq 0 ] || [ $rc -eq 2 ]'
}
# catalog_has <text> — whether the last catalog the compiler received carries it.
catalog_has() { docker exec "$COMPILER" grep -qF -- "$1" "$CATALOG"; }
# catalog_code_id — the code_id stamped on that catalog.
catalog_code_id() {
  docker exec "$COMPILER" bash -lc "grep -o '\"code_id\":\"[^\"]*\"' $CATALOG | head -1 | cut -d'\"' -f4"
}
# deploy_marker <text> — add a top-level notify to site.pp on the primary,
# reseal, and wait for the compiler to converge on the new code_id.
deploy_marker() {
  local prev
  prev=$(code_id "$COMPILER" production)
  docker exec "$PRIMARY" bash -lc "printf 'notify { \"%s\": }\n' '$1' >> $SITE_PP"
  docker exec "$PRIMARY" systemctl reload codavox-publish
  wait_for 45 "$COMPILER" "[ \"\$(codavox-code-id production)\" != '$prev' ]"
}
# set_flush <true|false> — flip the agent's flush and restart it.
set_flush() {
  docker exec "$COMPILER" sed -i "s/^  flush_environment_cache: .*/  flush_environment_cache: $1/" /etc/codavox/config.yaml
  docker exec "$COMPILER" systemctl restart codavox-agent
}

# First, the hazard. Prime the cache with a compile, then deploy with the flush
# off: the new code_id must arrive while the catalog stays on the old tree.
set_flush false
stale="codavox stale-cache marker $(date +%s%N)"
if compile_catalog && deploy_marker "$stale" && compile_catalog; then
  served=$(code_id "$COMPILER" production)
  stamped=$(catalog_code_id)
  if catalog_has "$stale"; then
    fail "without a flush the catalog still picked up the deploy — this server has no cache to expire, so the feature is untested"
  elif [ "$stamped" = "$served" ]; then
    pass "without a flush the catalog is stamped $served but compiled from the old tree"
  else
    fail "without a flush the catalog is stamped $stamped, the compiler serves $served"
  fi
else
  docker exec "$COMPILER" tail -20 /tmp/agent.log 2>/dev/null || true
  fail "could not reproduce a stale environment cache (deploy or compile failed)"
fi

# Then the fix. With the flush back on, the next deploy is what the server
# compiles — and it also carries the marker the stale compile missed.
set_flush true
fresh="codavox flushed-cache marker $(date +%s%N)"
if deploy_marker "$fresh" && compile_catalog; then
  if catalog_has "$fresh" && catalog_has "$stale"; then
    pass "after the flush the catalog carries the deploy, stamped $(catalog_code_id)"
  else
    fail "after the flush the catalog still lacks the deployed change"
    docker exec "$COMPILER" grep -o '"title":"codavox[^"]*"' "$CATALOG" 2>/dev/null | sed 's/^/      /'
  fi
else
  docker exec "$COMPILER" tail -20 /tmp/agent.log 2>/dev/null || true
  fail "deploy or compile failed with the flush on"
fi

# The agent says what it did, in the journal — see README.md on why not
# `docker logs`. Not `grep -q`: under pipefail it exits on the first match,
# journalctl dies of SIGPIPE, and the pipeline reports the line absent. That
# raced for a while and lost once the journal grew.
if docker exec "$COMPILER" journalctl -u codavox-agent --no-pager 2>/dev/null \
  | grep 'environment cache flushed' >/dev/null; then
  pass "the agent logged the flush"
else
  fail "the agent never logged 'environment cache flushed'"
  docker exec "$COMPILER" journalctl -u codavox-agent --no-pager -n 15 2>&1 | tail -15
fi

# Feature 5: the deploy server's health endpoint is open, and the deploy API is
# gated by the bearer token.
log "Feature 5 — deploy server: health open, deploy API token-gated"
health=$(docker exec "$COMPILER" bash -lc "curl -sk --max-time 5 https://primary:8170/v1/health")
if printf '%s' "$health" | grep -q '"status":"ok"'; then
  pass "GET /v1/health -> $health"
else
  fail "health check failed ($health)"
fi

unauth=$(docker exec "$COMPILER" bash -lc \
  "curl -sk -o /dev/null -w '%{http_code}' --max-time 5 https://primary:8170/v1/deploys")
if [ "$unauth" = "401" ]; then
  pass "GET /v1/deploys without a token -> 401"
else
  fail "unauthenticated deploys returned $unauth, want 401"
fi

auth=$(docker exec "$COMPILER" bash -lc \
  "curl -sk -o /dev/null -w '%{http_code}' --max-time 5 -H 'Authorization: Bearer integration-token' https://primary:8170/v1/deploys")
if [ "$auth" = "200" ]; then
  pass "GET /v1/deploys with the token -> 200"
else
  fail "authenticated deploys returned $auth, want 200"
fi

# Feature 6: no fallback — asking for content at a version that is not deployed,
# or an unknown environment, is a hard error, never a plausible-but-wrong answer.
log "Feature 6 — no fallback: undeployed content and unknown env are hard errors"
if docker exec "$COMPILER" codavox-code-content production notdeployed manifests/site.pp >/dev/null 2>&1; then
  fail "code-content for an undeployed code_id exited 0"
else
  pass "code-content for an undeployed code_id exits non-zero"
fi
if docker exec "$COMPILER" codavox-code-id nosuchenv >/dev/null 2>&1; then
  fail "code-id for an unknown environment exited 0"
else
  pass "code-id for an unknown environment exits non-zero"
fi

# Feature 7: the agent reaps old version directories rather than letting them
# accumulate (keep=2).
log "Feature 7 — the agent prunes old version directories (keep=2)"
for _ in 1 2 3; do bump_and_reseal; sleep 8; done
sleep 8
count=$(docker exec "$COMPILER" bash -lc \
  "ls -d /opt/puppetlabs/codavox/versions/production_* 2>/dev/null | wc -l" | tr -d ' ')
if [ "${count:-0}" -le 3 ]; then
  pass "version directories held at $count (<= keep + current)"
else
  fail "version directories grew to $count (prune not reaping)"
fi

# Feature 8: the publisher's fleet view reports what the compiler itself says it
# is serving, and agrees with that node's own code-id.
#
# The agent here is a long-running daemon polling over one keep-alive connection,
# which is exactly what no Go test can observe: every Go test drives `agent
# --once`, a fresh process and a fresh connection per sync. If the report only
# rode on the first request of a connection, or the publisher only read the
# header at handshake, it would pass every unit test and fail here.
log "Feature 8 — the fleet view matches what the compiler reports serving"

# Read it the way an operator does: on the publisher, using that node's own
# certificate. No --publisher, so the default URL is exercised too.
fleet_json() {
  docker exec "$PRIMARY" codavox compilers --json 2>/dev/null
}

# reported_code_id — read the compiler's reported production code_id from a
# fleet view on stdin. Parsed on the host: the harness inspects everything else
# host-side too, and the container image is not ours to add tools to.
reported_code_id() {
  python3 -c 'import json,sys
d = json.load(sys.stdin)
print(next((p["serving"].get("production", "") for p in d
            if p["certname"] == "compiler" and p.get("serving")), ""))' 2>/dev/null
}

if fleet=$(fleet_json) && [ -n "$fleet" ]; then
  pass "codavox compilers returned a fleet view"
else
  fail "codavox compilers returned nothing"
  fleet='[]'
fi

own=$(code_id "$COMPILER" production)
serving=$(printf '%s' "$fleet" | reported_code_id)

if [ -n "$serving" ] && [ "$serving" = "$own" ]; then
  pass "publisher and compiler agree on $own"
else
  fail "publisher says '$serving', the compiler's own code-id says '$own'"
fi

# And it must track a deploy, not just report a stale answer that happened to
# match. The agent reports again as soon as it converges, so this is bounded by
# the poll interval, not by two of them.
bump_and_reseal
if wait_for 60 "$COMPILER" "[ \"\$(codavox-code-id production)\" != '$own' ]"; then
  moved=$(code_id "$COMPILER" production)
  # Give the report that follows convergence a moment to land.
  sleep 5
  after=$(fleet_json | reported_code_id)
  if [ "$after" = "$moved" ]; then
    pass "the fleet view followed the deploy to $moved"
  else
    fail "the fleet view says '$after' after the compiler moved to '$moved'"
  fi
else
  fail "the compiler did not converge on a new code_id"
fi

# The table form is what an operator actually reads.
#
# COMMIT is expected to be "-" here: this harness stages code by appending to
# site.pp rather than by running r10k, so there is no .r10k-deploy.json to read a
# control-repo commit from. That is the no-fallback rule showing up in the fleet
# view — a missing provenance record reads as absent, never as another version's
# commit — so it is asserted rather than tolerated.
if table=$(docker exec "$PRIMARY" codavox compilers 2>/dev/null) \
  && printf '%s' "$table" | grep -q compiler; then
  pass "codavox compilers listed the compiler"
  printf '%s\n' "$table" | sed 's/^/      /'
  if printf '%s' "$table" | grep -qE '^compiler +production +[0-9a-f]{12} +- '; then
    pass "an unrecorded code_id reports no commit rather than inventing one"
  else
    fail "expected COMMIT to be '-' with no r10k deploy record"
  fi
else
  fail "codavox compilers did not list the compiler"
fi

# Feature 9: the deploy path against a real r10k. provision-primary.sh seeded
# a control repo from the served tree plus a Puppetfile naming a local module
# repo; nothing has run r10k yet. `codavox deploy` must run it, seal the
# result, and — with --wait — return once the publisher serves it. The compiler
# then converges on a tree that now carries the module and an r10k deploy
# record, so the fleet view can finally report a commit.
log "Feature 9 — codavox deploy runs r10k, seals, and the compiler converges on it"
before=$(code_id "$COMPILER" production)
if out=$(docker exec "$PRIMARY" codavox deploy production --wait 2>&1); then
  pass "codavox deploy production --wait exited 0"
else
  fail "codavox deploy production --wait failed: $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"
fi
if printf '%s\n' "$out" | grep -qE '^production[[:space:]]+deployed[[:space:]]+[0-9a-f]{64}.*serving'; then
  pass "reported the environment deployed and serving"
else
  fail "unexpected deploy output: $(printf '%s' "$out" | head -2 | tr '\n' ' ')"
fi
if wait_for 60 "$COMPILER" "[ \"\$(codavox-code-id production)\" != '$before' ]"; then
  pass "compiler converged on the r10k-built tree $(code_id "$COMPILER" production)"
else
  fail "compiler did not converge after the r10k deploy"
fi
if docker exec "$COMPILER" test -f /opt/puppetlabs/codavox/environments/production/modules/apache/manifests/init.pp; then
  pass "the compiler serves the Puppetfile module r10k resolved"
else
  fail "modules/apache is missing from the compiler's production tree"
fi
if docker exec "$PRIMARY" codavox compilers 2>/dev/null | grep -qE '^compiler +production +[0-9a-f]{12} +[0-9a-f]{7,} '; then
  pass "the fleet view now reports the control-repo commit r10k recorded"
else
  fail "the fleet view shows no commit after an r10k deploy"
fi

# Feature 10: a module deploy re-resolves only the named module, the way Code
# Manager's modules parameter does, and the two ways r10k stays silent — a
# short name that matches nothing, and a long name it would never match — are
# both refused rather than reported as deployed.
log "Feature 10 — deploy --modules re-resolves one module, and refuses what r10k would ignore"
docker exec "$PRIMARY" bash -lc \
  "cd /srv/modules/apache && echo '# module change $(date +%s%N)' >> manifests/init.pp && git commit -qam 'apache 2'"
before=$(code_id "$COMPILER" production)
if out=$(docker exec "$PRIMARY" codavox deploy production --modules apache --wait 2>&1) \
  && printf '%s\n' "$out" | grep -qE '^production[[:space:]]+deployed'; then
  pass "deploy --modules apache exited 0"
else
  fail "deploy --modules apache failed: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
fi
if wait_for 60 "$COMPILER" "grep -q 'module change' /opt/puppetlabs/codavox/environments/production/modules/apache/manifests/init.pp"; then
  pass "the compiler serves the re-resolved module ($before -> $(code_id "$COMPILER" production))"
else
  fail "the module change never reached the compiler"
fi
if out=$(docker exec "$PRIMARY" codavox deploy production --modules nosuchmodule 2>&1); then
  fail "deploy --modules nosuchmodule exited 0 (r10k deploys nothing and exits 0 for it; codavox must not)"
elif printf '%s' "$out" | grep -q "not in production's Puppetfile: nosuchmodule"; then
  pass "a module not in the Puppetfile fails the deploy: $(printf '%s' "$out" | grep -o "not in production's Puppetfile: nosuchmodule")"
else
  fail "deploy --modules nosuchmodule failed for another reason: $(printf '%s' "$out" | head -1)"
fi
if out=$(docker exec "$PRIMARY" codavox deploy production --modules puppetlabs/apache 2>&1); then
  fail "a long module name was accepted"
elif printf '%s' "$out" | grep -qi 'short name'; then
  pass "a long module name is refused with the hint, before r10k runs"
else
  fail "a long module name failed without the hint: $(printf '%s' "$out" | head -1)"
fi

# Feature 11: a hung r10k is bounded. r10k blocks forever on a fetch to a
# remote that does not answer, and while it does the basedir lock is held and
# every later deploy queues behind it. The bound must end r10k and everything
# it spawned — git, ssh — or the survivors keep writing the basedir under the
# next deploy.
log "Feature 11 — a hung r10k is killed at --r10k-timeout, children included"
docker exec "$PRIMARY" bash -lc "cat > /tmp/hang-r10k <<'S'
#!/bin/sh
sleep 300 &
echo \$! > /tmp/hang-r10k.child
wait
S
chmod 0755 /tmp/hang-r10k; rm -f /tmp/hang-r10k.child"
start=$(date +%s)
if out=$(docker exec "$PRIMARY" codavox deploy production --r10k /tmp/hang-r10k --r10k-timeout 2s 2>&1); then
  fail "a hung r10k was reported as a successful deploy"
elif printf '%s' "$out" | grep -q 'timed out after 2s'; then
  pass "$(printf '%s' "$out" | grep -o 'r10k deploy timed out after 2s') in $(( $(date +%s) - start ))s"
else
  fail "the deploy failed without timing out: $(printf '%s' "$out" | head -1)"
fi
sleep 2
child=$(docker exec "$PRIMARY" cat /tmp/hang-r10k.child 2>/dev/null)
if [ -n "$child" ] && ! docker exec "$PRIMARY" kill -0 "$child" 2>/dev/null; then
  pass "r10k's child $child is gone: the whole process group was signaled"
else
  fail "r10k's child ${child:-?} survived the timeout"
  docker exec "$PRIMARY" kill -9 "$child" 2>/dev/null || true
fi
if out=$(docker exec "$PRIMARY" codavox deploy production 2>&1); then
  pass "the next deploy ran at once: the lock was released"
else
  fail "the deploy after the timeout failed: $(printf '%s' "$out" | tail -1)"
fi

# Feature 12: an artifact past --max-unpacked is refused before it lands. Each
# file was always bounded by its declared size; the sum was not, and a gzip of
# zeros expands ~1000:1 on every compiler at once. Verification by resealing
# would refuse the tree, but only once it was on disk. A scratch root, so the
# real agent on this node is untouched, and a bound of 64 bytes because the
# whole tree here is a few hundred: the lab's 4 KiB let it through.
log "Feature 12 — an artifact past --max-unpacked is refused before it lands"
out=$(docker exec "$COMPILER" bash -lc 'rm -rf /tmp/cap; CODAVOX_ROOT=/tmp/cap codavox agent --once \
  --environmentpath /tmp/cap/environments --flush-environment-cache false --max-unpacked 64 2>&1; echo "rc=$?"')
if printf '%s' "$out" | grep -q 'expands past 64 bytes'; then
  pass "refused: $(printf '%s' "$out" | grep -o 'refusing archive that expands past 64 bytes at [^" ]*' | head -1)"
else
  fail "no refusal in the agent's output: $(printf '%s' "$out" | head -1 | cut -c1-140)"
fi
if [ "$(docker exec "$COMPILER" bash -lc 'ls /tmp/cap/versions 2>/dev/null | grep -vc "^\."')" = "0" ]; then
  pass "nothing was installed under the scratch root"
else
  fail "a version directory landed despite the refusal"
fi
docker exec "$COMPILER" rm -rf /tmp/cap

# Feature 13: a branch deleted at the webhook is purged from the primary and
# pruned on the compiler. A real branch, so r10k stages a real environment
# and the compiler serves it before it goes; then the webhook's deletion
# deploys everything, r10k's deployment-level purge removes the directory,
# the publisher stops advertising it, and the compiler — prune on — drops it.
log "Feature 13 — a branch deleted at the webhook is purged, and the compiler prunes it"
docker exec "$PRIMARY" git -C /srv/control branch testing production
if out=$(docker exec "$PRIMARY" codavox deploy --all --wait 2>&1) \
  && printf '%s\n' "$out" | grep -qE '^testing[[:space:]]+deployed'; then
  pass "deploy --all staged the testing environment"
else
  fail "testing was not deployed: $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
fi
if wait_for 60 "$COMPILER" "test -L /opt/puppetlabs/codavox/environments/testing"; then
  pass "the compiler serves testing"
else
  fail "the compiler never picked testing up"
fi

docker exec "$PRIMARY" git -C /srv/control branch -D testing >/dev/null
code=$(docker exec "$COMPILER" bash -lc "curl -sk -o /dev/null -w '%{http_code}' --max-time 5 -X POST \
  -H 'Authorization: Bearer integration-secret' -H 'Content-Type: application/json' \
  -d '{\"environment\":\"testing\",\"deleted\":true}' https://primary:8170/v1/webhook")
if [ "$code" = "202" ]; then
  pass "the webhook accepted the deletion (202)"
else
  fail "the webhook returned $code for a deletion"
fi

rec=""
for _ in $(seq 1 40); do
  rec=$(docker exec "$COMPILER" bash -lc "curl -sk --max-time 5 -H 'Authorization: Bearer integration-token' https://primary:8170/v1/deploys" \
    | python3 -c '
import json,sys
for r in json.load(sys.stdin):
    if r.get("source")=="webhook" and r.get("all"):
        print(r.get("status"), r.get("reason",""), ",".join(r.get("environments") or []), sep="|")
        break' 2>/dev/null)
  case "$rec" in complete*|failed*) break ;; esac
  sleep 3
done
status=$(printf '%s' "$rec" | cut -d'|' -f1)
reason=$(printf '%s' "$rec" | cut -d'|' -f2)
remaining=$(printf '%s' "$rec" | cut -d'|' -f3)
if [ "$status" = "complete" ] && [ "$reason" = "branch for testing deleted" ]; then
  pass "the history shows a complete all-deploy, reason '$reason'"
else
  fail "no complete webhook all-deploy in the history (got '${rec:-nothing}')"
  docker exec "$PRIMARY" journalctl -u codavox-deploy-server --no-pager -n 10 2>&1 | tail -10
fi
case ",$remaining," in
  *,testing,*) fail "r10k did not purge testing: the deploy still reports it among what remains" ;;
  *) pass "the deploy reports only what remains: $remaining" ;;
esac
if docker exec "$PRIMARY" test -e /etc/puppetlabs/code/environments/testing; then
  fail "testing is still staged on the primary"
else
  pass "testing is gone from the primary's basedir"
fi
if wait_for 60 "$COMPILER" "! test -e /opt/puppetlabs/codavox/environments/testing"; then
  pass "the compiler pruned testing"
else
  fail "the compiler still serves testing"
  docker exec "$COMPILER" journalctl -u codavox-agent --no-pager -n 10 2>&1 | tail -10
fi

# Feature 14: revoking a compiler's Puppet certificate revokes its access to code.
# The certificate stays cryptographically valid and keeps its pp_role, so only
# the CRL check stops it — and it must take effect without restarting anything.
#
# This runs last: the compiler cannot fetch code afterwards.
log "Feature 14 — revoking a compiler's certificate cuts off its access to code"
before=$(code_id "$COMPILER" production)
# Keep the output: when this failed in CI it was suppressed, so the log said
# only "could not revoke" and not that the CA host did not resolve.
if revoke_out=$(docker exec "$PRIMARY" /opt/puppetlabs/bin/puppetserver ca revoke \
  --certname compiler 2>&1); then
  pass "revoked the compiler's certificate"
else
  fail "could not revoke the compiler's certificate: $revoke_out"
fi

# The agent polls over a keep-alive connection, so this also exercises the
# publisher re-checking the CRL per request rather than only at handshake.
bump_and_reseal
sleep 10

after=$(code_id "$COMPILER" production)
if [ "$after" = "$before" ]; then
  pass "revoked compiler stayed at $before and received no new code"
else
  fail "a revoked compiler still fetched code ($before -> $after)"
fi

# The publisher must say why, rather than failing silently. Read the unit's
# journal, not `docker logs`: the publisher runs under systemd inside the
# container, so its stderr goes to the journal while docker logs shows PID 1.
if docker exec "$PRIMARY" journalctl -u codavox-publish --no-pager 2>/dev/null \
  | grep -i "revoked" >/dev/null; then
  pass "the publisher logged the refusal"
else
  fail "the publisher refused the compiler without saying it was revoked"
  docker exec "$PRIMARY" journalctl -u codavox-publish --no-pager -n 15 2>&1 | tail -15
fi

echo
if [ "$FAILED" -eq 0 ]; then
  printf '\033[1;32mALL FEATURES PASSED\033[0m\n'
else
  die "one or more features failed"
fi
