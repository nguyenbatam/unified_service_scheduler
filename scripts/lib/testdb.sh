# shellcheck shell=bash
# Test-database setup shared by scripts/verify.sh (tier test) and scripts/check-all.sh. Source it
# after scripts/ensure-env.sh (POSTGRES_* / REDIS_PORT loaded) with the compose `db` service up:
#   # shellcheck source=lib/testdb.sh
#   source "$ROOT/scripts/lib/testdb.sh"
#   ensure_test_db || fail "…"
#   export_test_env

# ensure_test_db creates the scheduler_test database in the compose db unless it exists. Every psql
# runs with ON_ERROR_STOP, so a failed query is a non-zero exit, never a silently empty answer.
ensure_test_db() {
  local exists
  # Captured and compared as a string: `psql … | grep -q 1` under pipefail fails whenever grep
  # exits before psql finished writing (SIGPIPE), and the CREATE then fails on the existing database.
  exists=$(docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atc \
    "SELECT 1 FROM pg_database WHERE datname='scheduler_test'") || {
    echo "testdb: could not list databases" >&2
    return 1
  }
  [[ "$exists" == 1 ]] && return 0
  docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
    -c "CREATE DATABASE scheduler_test" >/dev/null || {
    echo "testdb: could not create scheduler_test" >&2
    return 1
  }
}

# export_test_env points the Go integration tests at the compose db and Redis (logical DB 15, not
# the stack's) and makes a missing database or Redis a failure instead of a skip.
export_test_env() {
  export TEST_DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/scheduler_test?sslmode=disable"
  export TEST_REDIS_URL="redis://127.0.0.1:${REDIS_PORT:-6379}/15"
  export REQUIRE_INTEGRATION=1
}
