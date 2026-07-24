#!/bin/bash
# Self-contained end-to-end harness for codavox — no ovadm.
#
# Builds a codavox package from the current checkout, stands up a two-node
# OpenVox topology (primary CA + publisher, compiler wired to codavox),
# provisions both with plain shell, then exercises every feature end to end.
#
#   ./test/integration/run.sh          # build, provision, test, tear down
#   KEEP=1 ./test/integration/run.sh   # leave the topology up for inspection
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
# shellcheck source=test/integration/lib.sh
source "$here/lib.sh"

KEEP="${KEEP:-0}"
COMPOSE="docker compose -f $here/compose.yml"

teardown() { [ "$KEEP" = "1" ] || $COMPOSE down -v >/dev/null 2>&1 || true; }
trap teardown EXIT

log "Building a codavox package from the current checkout"
# goreleaser is often installed under GOPATH/bin, which isn't always on PATH.
command -v go >/dev/null && PATH="$PATH:$(go env GOPATH)/bin"
command -v goreleaser >/dev/null || die "goreleaser not found (see docs/installation.md)"
( cd "$repo" && goreleaser release --snapshot --clean --skip=publish >/tmp/codavox-goreleaser.log 2>&1 ) \
  || { tail -20 /tmp/codavox-goreleaser.log; die "goreleaser build failed"; }

log "Starting the two-node topology"
$COMPOSE up -d --build

install_codavox() {
  local c="$1" arch pkgarch rpm
  arch=$(docker exec "$c" uname -m)
  case "$arch" in
    x86_64)  pkgarch=amd64 ;;
    aarch64) pkgarch=arm64 ;;
    *)       die "unsupported arch $arch" ;;
  esac
  rpm=$(ls "$repo"/dist/codavox_*_linux_"${pkgarch}".rpm 2>/dev/null | head -1)
  [ -n "$rpm" ] || die "no linux/$pkgarch package in dist/"
  docker cp "$rpm" "$c:/tmp/codavox.rpm" >/dev/null
  docker exec "$c" dnf install -y -q /tmp/codavox.rpm
}

log "Installing codavox (HEAD snapshot) on both nodes"
install_codavox "$PRIMARY"
install_codavox "$COMPILER"

log "Provisioning the primary (CA + publisher + deploy server)"
docker cp "$here/provision-primary.sh" "$PRIMARY:/tmp/provision-primary.sh" >/dev/null
docker exec "$PRIMARY" bash /tmp/provision-primary.sh

log "Provisioning the compiler (agent + wired OpenVox Server)"
docker cp "$here/provision-compiler.sh" "$COMPILER:/tmp/provision-compiler.sh" >/dev/null
docker exec "$COMPILER" bash /tmp/provision-compiler.sh primary

bash "$here/features.sh"
