#!/usr/bin/env bash
# One-shot full verification for local production-shaped stack.
# Usage: ./scripts/check-all.sh
# It leaves the full stack running (pass or fail) for inspection: ./scripts/down.sh stops it.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"
# shellcheck source=lib/testdb.sh
source "$ROOT/scripts/lib/testdb.sh"

RED=$'\033[31m'
GRN=$'\033[32m'
YLW=$'\033[33m'
NC=$'\033[0m'
step() { echo; echo "${YLW}==>${NC} $*"; }
ok() { echo "${GRN}OK${NC} $*"; }
# Set once the full stack is starting: from then on a failure also dumps its state and logs.
STACK_UP=0
diagnose() {
  [[ "$STACK_UP" == 1 ]] || return 0
  echo "${YLW}--- docker compose ps${NC}"
  docker compose ps -a || true
  echo "${YLW}--- docker compose logs (tail)${NC}"
  docker compose logs --no-color --tail=80 || true
}
fail() { echo "${RED}FAIL${NC} $*"; diagnose; exit 1; }
# Any other failing command (set -e) names its line before the script exits.
on_err() {
  local rc=$? line=$1 cmd=$2
  echo "${RED}FAIL${NC} line ${line}: ${cmd} (exit ${rc})"
  diagnose
}
trap 'on_err "$LINENO" "$BASH_COMMAND"' ERR
WAIT_TIMEOUT="${WAIT_TIMEOUT:-300}"

step "0. toolchain"
command -v go >/dev/null || fail "go missing"
command -v docker >/dev/null || fail "docker missing"
command -v npm >/dev/null || fail "npm missing"
command -v python3 >/dev/null || fail "python3 missing"
command -v curl >/dev/null || fail "curl missing"
[[ "$(go version)" == *go1.27* ]] || echo "${YLW}warn${NC}: expected Go 1.27, got $(go version)"
ok "tools present"

step "1. infra up (postgres redis kafka)"
# --wait blocks on each service's compose healthcheck (pg_isready, redis ping, kafka-topics).
docker compose up -d --wait --wait-timeout "$WAIT_TIMEOUT" db redis kafka \
  || { STACK_UP=1; fail "db/redis/kafka not healthy within ${WAIT_TIMEOUT}s"; }
# Database clock at the start of this run: later checks only look at rows this run created.
RUN_START=$(docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT now()")
[[ -n "$RUN_START" ]] || fail "could not read the database clock"
ok "db + redis + kafka healthy (run start ${RUN_START})"

step "2. ensure scheduler_test database"
ensure_test_db || fail "scheduler_test database (scripts/lib/testdb.sh)"
ok "scheduler_test ready"

step "3. lint (openapi copies, migrations, shellcheck, gofmt, vet, golangci, tidy, govulncheck, tsc)"
bash "$ROOT/scripts/lint.sh"
ok "lint"

step "4. unit/integration tests"
# TEST_DATABASE_URL, TEST_REDIS_URL (its own logical DB, not the stack's), REQUIRE_INTEGRATION.
export_test_env
# The ERR trap does not see inside a subshell (bash 3.2 has no set -E), so every subshell and
# function call below ends in `|| fail`. Under `|| fail` errexit is off inside it: chain with &&.
# One run, race detector on everything: the goroutines live in integration/worker/realtime/
# events/notify, not in internal/booking.
(cd backend && go test -count=1 -race -timeout 15m ./...) || fail "backend tests"
ok "backend tests"
# The database-backed benchmarks run a few iterations, so they keep compiling and working (the
# capacity of the running stack is scripts/bench.sh).
(cd backend && go test -count=1 -run '^$' -bench . -benchtime 20x ./internal/integration/) || fail "benchmarks"
ok "benchmarks (smoke)"

step "5. web unit tests + production build"
# lint.sh (step 3) installs node_modules when they are missing; reinstall only when they are older
# than the lockfile / package.json (npm's own hidden lockfile records the last install), so a fresh
# install is not thrown away and redone a minute later.
web_deps_current() {
  [[ -f web/node_modules/.package-lock.json && -f web/package-lock.json ]] &&
    [[ web/node_modules/.package-lock.json -nt web/package-lock.json ]] &&
    [[ web/node_modules/.package-lock.json -nt web/package.json ]]
}
install_web=1
if web_deps_current; then
  install_web=0
  echo "web/node_modules installed after package-lock.json and package.json: npm ci skipped"
fi
(
  cd web &&
    { if [[ "$install_web" == 0 ]]; then :; elif [[ -f package-lock.json ]]; then npm ci; else npm install; fi; } &&
    npm test &&
    npm run build
) || fail "web tests + build"
ok "web tests + build"

step "6. full compose stack (2 backends + gateway + web + prometheus + grafana)"
STACK_UP=1
# Every service has a healthcheck (backends: /health/ready; gateway, web, prometheus, grafana:
# their own endpoints), so --wait replaces polling and fails with a timeout instead of looping.
docker compose up -d --build --wait --wait-timeout "$WAIT_TIMEOUT" \
  || fail "stack not healthy within ${WAIT_TIMEOUT}s"
ok "stack up"

# Shared with the Cluster workflow: Prometheus scraping both backends, gateway smoke, book A /
# read B, WebSocket fan-out, DMS operations + notifications, stress + SQL integrity.
step "7. cluster checks (scripts/cluster-checks.sh)"
RUN_START="$RUN_START" bash scripts/cluster-checks.sh || fail "cluster checks"
ok "cluster checks"

echo
echo "${GRN}check-all passed${NC} (the stack is still up; ./scripts/down.sh stops it)"
echo "UI      http://localhost:${WEB_PORT}"
echo "API     http://localhost:${GATEWAY_PORT}  (backends :${BACKEND_1_HOST_PORT} / :${BACKEND_2_HOST_PORT})"
echo "Prom    http://localhost:${PROMETHEUS_PORT}"
echo "Grafana http://localhost:${GRAFANA_PORT}  (${GF_SECURITY_ADMIN_USER}/${GF_SECURITY_ADMIN_PASSWORD}, dashboard: Unified Service Scheduler)"
