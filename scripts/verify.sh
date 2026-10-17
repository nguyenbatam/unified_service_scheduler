#!/usr/bin/env bash
# Verification entrypoint by tier (see .claude/skills/verify):
#   ./scripts/verify.sh fast   lint.sh (= CI's static checks)
#   ./scripts/verify.sh test   fast + web unit tests + Go tests with Postgres/Redis integration under -race
#                              (starts db + redis)
#   ./scripts/verify.sh full   check-all.sh (everything, incl. the 2-backend cluster and stress) — the default
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tier="${1:-full}"
case "$tier" in
  fast)
    exec bash "$ROOT/scripts/lint.sh"
    ;;
  test)
    bash "$ROOT/scripts/lint.sh"
    # Web unit tests (Vitest): lint.sh only type-checks the web app.
    (cd "$ROOT/web" && npm test)
    cd "$ROOT"
    # shellcheck source=ensure-env.sh
    source "$ROOT/scripts/ensure-env.sh"
    # --wait fails on an unhealthy container; the timeout keeps a stuck start from hanging forever.
    docker compose up -d --wait --wait-timeout "${WAIT_TIMEOUT:-300}" db redis
    # shellcheck source=lib/testdb.sh
    source "$ROOT/scripts/lib/testdb.sh"
    ensure_test_db
    export_test_env
    cd "$ROOT/backend"
    exec go test -count=1 -race -timeout 15m ./...
    ;;
  full)
    exec bash "$ROOT/scripts/check-all.sh"
    ;;
  *)
    echo "usage: $0 [fast|test|full]" >&2
    exit 2
    ;;
esac
