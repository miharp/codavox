# shellcheck shell=bash
# Shared host-side helpers for the codavox integration harness. Sourced by
# run.sh and features.sh; not executed directly.

# Exported because the sourcing scripts, not this file, are what use them.
export PRIMARY=codavox-primary
export COMPILER=codavox-compiler

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
ok()  { printf '\033[1;32m  ok \033[0m%s\n' "$*"; }
die() { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

# dexec <container> <command-string>
dexec() { docker exec "$1" bash -lc "$2"; }

# code_id <container> <environment> — the code_id the node reports serving.
code_id() { docker exec "$1" codavox-code-id "$2" 2>/dev/null; }

# wait_for <seconds> <container> <command-string> — poll until it succeeds.
wait_for() {
  local timeout="$1" c="$2" cmd="$3" waited=0
  until docker exec "$c" bash -lc "$cmd" >/dev/null 2>&1; do
    [ "$waited" -ge "$timeout" ] && return 1
    sleep 3
    waited=$((waited + 3))
  done
}
