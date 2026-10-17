#!/usr/bin/env bash
# Heavy concurrent booking stress + side-API / WS fan-out / SQL integrity checks (Go client), ending with
# a requirements matrix: every requirement of the brief with the evidence the run gathered.
# Usage: ./scripts/stress.sh [extra flags for backend/cmd/stress, e.g. -phases river]
# Env (each maps to a flag; see backend/cmd/stress/main.go):
#   BASE_URL              gateway (default http://127.0.0.1:$GATEWAY_PORT)
#   BACKEND_A_URL         backend-1 (default http://127.0.0.1:$BACKEND_1_HOST_PORT)
#   BACKEND_B_URL         backend-2 (default http://127.0.0.1:$BACKEND_2_HOST_PORT)
#   CONCURRENCY           requests per hammered slot (default 1000)
#   SLOTS                 slots per phase (default 5)
#   WORKERS               booking pool (default min(CONCURRENCY, 512))
#   SIDE_WORKERS          read/admin probes during the hammer (default 16)
#   WS_CLIENTS            WS listeners per backend (default 2, max 10 = hub per-user socket cap)
#   PHASES                river (exact capacity, no staging), north (staging + multi-line), storm (reschedules
#                         racing new bookings for one start), truth (one day filled booking by booking against
#                         a SQL oracle), fill (a day of concurrent customers dispatched by staff), quality (a
#                         scripted day where greedy first fit loses: the server vs every first-fit order, and
#                         the one-move repair), customers (signed-in customer accounts booking their own cars)
#                         (default river,north,storm,truth,fill,quality,customers)
#   CUSTOMERS             customer accounts of the customers phase (default 40)
#   FILL_REQUESTS / FILL_RATE / FILL_WORKERS / FILL_SEED   the fill day's customers (150), arrivals per
#                         second (500, open loop; 0 = closed loop), most in flight (0 = all: truly open
#                         loop), demand seed (1)
#   FILL_MARGIN_PCT       most % of labour minutes the concurrent fill day may book below its
#                         sequential replay (default 10)
#   FIRSTFIT_MARGIN_PCT   most % of labour minutes the server's sequential replay may book below a naive
#                         first-fit replay of the same demand on the oracle's model (default 0: never fewer)
#   FIT_SEED / FIT_MIN_GAIN_PCT  the fit days (part of fill: demand 1.0 / 1.15 / 1.3 × the capacity bound,
#                         one customer at a time): demand seed (1), least % gain over first fit in the
#                         median random resource order (0; measured +3.6 … +5.6 % pooled over 20 seeds)
#   QUALITY_MARGIN_PCT    least % more labour minutes the quality day's server must book than the best
#                         first-fit replay of the script in any fixed resource order (default 15; measured +23.1)
#   SLO_BOOK_MS / SLO_READ_MS                  fill-day p99 budgets for booking / availability (1000 / 500;
#                                              the first read from the intended arrival gets SLO_READ_MS too)
#   SLO_HAMMER_MS / SLO_SIDE_MS                hammer p99 budgets: a booking end to end / a side read (3000 / 1000)
#   SLO_OTHER_DAY_MS      p99 budget of bookings on another day of the hammered dealership (1000)
#   POOL_WAIT_BUDGET_MS   most Σ empty-acquire wait the DB pools may add per hammer phase (default 2000)
#   POOL_CAP_FAILS        false: a backend at its DB pool cap is reported, not failed (default true)
#   STRESS_REQUIRE        what must PASS, a SKIP fails: requirement ids (R1 … Q7), id/check for one named
#                         check (e.g. "Q4/ws order"), or all (every row plus the essential checks: Q4/ws,
#                         Q4/ws order, Q5/beats first fit, Q5/repair, Q5/fit gain)
#                         (default: all when the phases are left at their default, none otherwise;
#                         STRESS_REQUIRE= opts out)
#   STRESS_SUMMARY_MD     append the compact requirements matrix as Markdown to this file (CI job summary)
#   STRESS_CHAOS_RESTART  1 when a backend is restarted mid-hammer on purpose (cluster-checks chaos)
#   REPORT_JSON           write the requirements matrix + headline numbers as JSON to this path
#   STRESS_TIMEOUT        whole-run deadline (default 30m)
#   MAX_ATTEMPTS          sends per booking, retried with the same Idempotency-Key (default 6)
#   RATE                  open loop: bookings/s per phase, latency from the intended send time
#                         (default 0 = closed loop: send whenever a worker is free)
#   MAX_UNRESOLVED_PCT    fail a phase when more of its requests stay unresolved (default 10);
#                         a phase that creates no booking always fails
#   STRESS_DATABASE_URL   Postgres for the SQL integrity pass and for cancelling bookings that
#                         committed while their request stayed unresolved (default: compose db)
#   STRESS_ADMIN_EMAIL / STRESS_ADMIN_PASSWORD   (default: the seeded demo admin)
#   START_MARGIN          earliest slot start after a phase is prepared (default 15m; keep above a phase's duration)
#   IDEM_RACERS / IDEM_KEYS   concurrent sends per Idempotency-Key, half to each backend, over IDEM_KEYS
#                         keys after the phases (default 16 × 4; IDEM_RACERS=0 skips)
#   CUSTOMER_REQUESTS     concurrent bookings as the seeded customer; 409/422/429 expected (default 40; 0 skips)
#   STRESS_CUSTOMER_EMAIL / STRESS_CUSTOMER_PASSWORD   (default: the seeded demo customer)
#   MIXED_OPS             bookings per phase reassigned/rescheduled/cancelled with If-Match during the hammer
#                         (default 30 on a full run, so the If-Match mutations and the WebSocket order check
#                         run; 0 otherwise)
#   MIN_INSTANCES         through the gateway, fail unless this many X-Instance-IDs answered bookings (default 2; 0 disables)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"

: "${STRESS_DATABASE_URL:=postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=disable}"
export STRESS_DATABASE_URL

# A full run (every phase) must prove every requirement and the essential named checks (the
# binary's "all"): a row or such check left SKIP fails it. Only when the phases are the default
# ones (neither PHASES nor a -phases flag) and STRESS_REQUIRE is unset (STRESS_REQUIRE= opts out).
full_run=1
[[ -z "${PHASES:-}" ]] || full_run=0
for arg in "$@"; do
  [[ "$arg" != -phases* && "$arg" != --phases* ]] || full_run=0
done
if [[ $full_run == 1 ]]; then
  : "${STRESS_REQUIRE=all}"
  # Mutations mid-hammer: the only appointment changes of a run before its cleanup, so without
  # them the WebSocket order check has nothing to judge (it then SKIPs).
  : "${MIXED_OPS:=30}"
  export STRESS_REQUIRE MIXED_OPS
fi

cd "$ROOT/backend"
# Build, then exec the binary itself (not `go run`): Ctrl-C / SIGTERM reach the stress client
# directly, which stops the hammer and still cancels the run's bookings. bin/ is git/docker-ignored.
go build -o bin/stress ./cmd/stress
exec bin/stress \
  -base-url "${BASE_URL:-http://127.0.0.1:${GATEWAY_PORT:-8080}}" \
  -backend-a "${BACKEND_A_URL:-http://127.0.0.1:${BACKEND_1_HOST_PORT:-18081}}" \
  -backend-b "${BACKEND_B_URL:-http://127.0.0.1:${BACKEND_2_HOST_PORT:-18082}}" \
  -phases "${PHASES:-river,north,storm,truth,fill,quality,customers}" \
  -max-attempts "${MAX_ATTEMPTS:-6}" \
  -max-unresolved-pct "${MAX_UNRESOLVED_PCT:-10}" \
  -dsn "$STRESS_DATABASE_URL" \
  "$@"
