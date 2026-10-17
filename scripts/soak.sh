#!/usr/bin/env bash
# Soak of the running stack (Go client, -mode soak): a low, steady mixed load on the "Stress River"
# dealership — book, read availability, reschedule and cancel its own bookings with If-Match, at most
# 40 alive — for SOAK_DURATION, sampling both backends every SOAK_SAMPLE. It fails when the outbox or
# notification backlog does not stay flat, a row is poisoned, a backend's resident memory or
# goroutines keep growing, an answer is a 5xx, or p99 latency leaves its budget; then it runs the SQL
# integrity checks over its bookings and waits for the outbox. Not part of check-all (it takes as
# long as you let it). The client refreshes its access token (ACCESS_TOKEN_TTL, 15 min by default)
# through POST /api/v1/auth/refresh with its refresh cookie, so it runs for hours on one login;
# Q4/client session reports the refreshes, and a session it cannot renew fails the soak.
# Usage: ./scripts/soak.sh [extra flags for backend/cmd/stress]
# Env (each maps to a flag; see backend/cmd/stress/main.go and soak.go):
#   SOAK_DURATION          how long the load runs (default 30m; memory and queues are judged with at
#                          least 8 samples)
#   SOAK_RATE              operations per second (default 4)
#   SOAK_SAMPLE            scrape interval (default 30s)
#   SOAK_RSS_GROWTH_PCT    most resident-memory growth per backend, last vs first quarter (default 25)
#   SOAK_QUEUE_MAX         most outbox + notification rows pending in the last third (default 100)
#   SLO_BOOK_MS / SLO_READ_MS  p99 budgets of writes / reads (default 1000 / 500)
#   REPORT_JSON            write the verdicts as JSON to this path
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"

: "${STRESS_DATABASE_URL:=postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=disable}"
export STRESS_DATABASE_URL

cd "$ROOT/backend"
go build -o bin/stress ./cmd/stress
# The whole-run deadline must outlast the soak plus provisioning and the final checks.
exec bin/stress -mode soak \
  -base-url "${BASE_URL:-http://127.0.0.1:${GATEWAY_PORT:-8080}}" \
  -backend-a "${BACKEND_A_URL:-http://127.0.0.1:${BACKEND_1_HOST_PORT:-18081}}" \
  -backend-b "${BACKEND_B_URL:-http://127.0.0.1:${BACKEND_2_HOST_PORT:-18082}}" \
  -dsn "$STRESS_DATABASE_URL" \
  -timeout "${STRESS_TIMEOUT:-24h}" \
  "$@"
