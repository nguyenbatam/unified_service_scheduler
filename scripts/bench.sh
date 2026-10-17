#!/usr/bin/env bash
# Capacity benchmark of the running stack on this machine (Go client, -mode bench): for each workload,
# run in the order book, read, hot, journey (book = uncontended staff bookings, read = availability of
# the days book just filled, hot = bookings racing for one start, every fully sent round counted in
# Postgres to hold exactly its 20 places, journey = a signed-in customer's visit: availability then
# booking through the customer policy and throttle) it offers an increasing open-loop rate and
# reports the highest step that meets the p99 budget, next to the server view (in-flight requests vs
# Little's law, pool use, Postgres commits), the client's own dispatch lag, and the host it ran on.
# Afterwards it runs the SQL integrity checks over everything it booked and waits for the outbox.
# Usage: ./scripts/bench.sh [extra flags for backend/cmd/stress]
# Env (each maps to a flag; see backend/cmd/stress/main.go and bench.go):
#   BENCH_WORKLOADS        read,book,hot,journey (default all)
#   BENCH_READ_RATES       default 100,400,1600,3200,6400 req/s
#   BENCH_BOOK_RATES       default 50,200,400,800,1600 req/s
#   BENCH_HOT_RATES        default 100,400,800,1600 req/s
#   BENCH_JOURNEY_RATES    default 1,2,4,8,16 users/s (each: availability read + booking as a
#                          signed-in customer; one load generator is one IP, and the customer throttle
#                          allows an IP 120 bookings / 600 reads a minute by default, so from one
#                          machine the journey's knee is that budget — the stack's write capacity is
#                          "book"; start the stack with WRITE_LIMIT_PER_IP / AVAILABILITY_LIMIT_PER_IP
#                          raised to bench the customer path itself)
#   BENCH_STEP             length of one step (default 10s)
#   BENCH_READ_SLO_MS / BENCH_BOOK_SLO_MS / BENCH_HOT_SLO_MS / BENCH_JOURNEY_SLO_MS
#                          p99 budgets (default 250 / 500 / 1000 / 750)
#   BENCH_DAYS             days the book and journey workloads each spread over (default 150)
#   REPORT_JSON            write every step as JSON to this path
# The bench books on its own "Stress Bench" dealership and cancels everything it booked at the end.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"

: "${STRESS_DATABASE_URL:=postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=disable}"
export STRESS_DATABASE_URL

cd "$ROOT/backend"
go build -o bin/stress ./cmd/stress
exec bin/stress -mode bench \
  -base-url "${BASE_URL:-http://127.0.0.1:${GATEWAY_PORT:-8080}}" \
  -backend-a "${BACKEND_A_URL:-http://127.0.0.1:${BACKEND_1_HOST_PORT:-18081}}" \
  -backend-b "${BACKEND_B_URL:-http://127.0.0.1:${BACKEND_2_HOST_PORT:-18082}}" \
  -dsn "$STRESS_DATABASE_URL" \
  "$@"
