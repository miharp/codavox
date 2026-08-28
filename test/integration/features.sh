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
# `docker logs`.
if docker exec "$COMPILER" journalctl -u codavox-agent --no-pager 2>/dev/null \
  | grep -q 'environment cache flushed'; then
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

# Feature 9: revoking a compiler's Puppet certificate revokes its access to code.
# The certificate stays cryptographically valid and keeps its pp_role, so only
# the CRL check stops it — and it must take effect without restarting anything.
#
# This runs last: the compiler cannot fetch code afterwards.
log "Feature 9 — revoking a compiler's certificate cuts off its access to code"
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
  | grep -qi "revoked"; then
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
