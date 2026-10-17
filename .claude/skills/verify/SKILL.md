---
name: verify
description: >-
  Pick and run the right verification tier for this repo after a change —
  fmt, lint (gofmt/vet/golangci/tsc/file-size), Go unit + Postgres integration
  tests, web tests/build, or the full 2-backend cluster check. Use before
  declaring a backend or web change done, when CI fails, or when asked to
  "verify", "check", or "run the tests".
argument-hint: "[fast|test|full]"
---

# Verify a change

Pick the **smallest tier that covers the change**, run it, and report the real output. Escalate only when the tier passes and the change touches what the next tier covers.

| Tier | Command | Needs | Covers |
|---|---|---|---|
| fmt | `./scripts/fmt.sh` | Go | gofmt + golangci formatters (auto-fix) |
| fast | `./scripts/lint.sh` | Go, npm, python3 (shellcheck: `brew install shellcheck`; skipped with a warning locally if missing, required on CI) | OpenAPI copies identical, migrations immutable, shellcheck `scripts/*.sh scripts/lib/*.sh`, python helpers parse, gofmt check, `go vet`, `go mod tidy -diff`, `go tool golangci-lint` (gosec, staticcheck, modernize…), govulncheck, web `tsc`, 1000-line cap — **same as CI** |
| unit | `cd backend && go test ./...` | Go | Pure Go tests; integration tests **skip** without a DB |
| test | see below | Docker (db + redis) | Booking concurrency + integration against real Postgres |
| web | `cd web && npm test && npm run build` | npm | Vitest + production build |
| full | `./scripts/check-all.sh` | Docker, ~minutes | Everything + benchmark smoke + web build + 2 backends, gateway, smoke, WS fan-out, DMS operations + notifications, stress with its requirements matrix (every row and its essential checks must PASS: `STRESS_REQUIRE=all`), chaos (Redis down, Kafka paused with WS delivery, a backend killed mid-hammer) (step 7 = `scripts/cluster-checks.sh`, also run step by step by the Cluster workflow; Prometheus must scrape both backends `up == 1`). Leaves the stack up |
| soak | `./scripts/soak.sh` | running stack, `SOAK_DURATION` (default 30 min) | Steady mixed load (book / read / reschedule / cancel): outbox + notification backlog, resident memory and goroutines must stay flat, no 5xx, p99 within budget, the client's session refreshed past the access TTL (`Q4/client session`); then the SQL integrity pass (not part of check-all) |
| bench | `./scripts/bench.sh` | running stack, ~5 min | Capacity of this machine: book / read / hot / journey ramps to the p99 knee, with server metrics, client dispatch lag and host context, then the SQL integrity pass over its bookings (not a pass/fail gate for changes) |

Shortcuts: `./scripts/verify.sh fast` (= lint.sh), `./scripts/verify.sh test` (lint + web unit tests (`npm test`) + the integration block below, starting db + redis), `./scripts/verify.sh full` (= check-all.sh; also what `verify.sh` with no argument runs).

## Integration tests (tier `test`)

```bash
docker compose up -d --wait db redis
source scripts/ensure-env.sh   # works from bash or zsh
export TEST_DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/scheduler_test?sslmode=disable"
export TEST_REDIS_URL="redis://127.0.0.1:${REDIS_PORT:-6379}/15"
export REQUIRE_INTEGRATION=1   # fail instead of skip when the DB or Redis is missing
# creates scheduler_test unless it exists (psql -v ON_ERROR_STOP=1); verify.sh / check-all.sh use the same helper
bash -c 'source scripts/ensure-env.sh && source scripts/lib/testdb.sh && ensure_test_db'
cd backend && go test -count=1 -race -timeout 15m ./...   # same as CI; integration runs in parallel, one database per test cloned from a template (~20 s under -race; needs CREATEDB; ITEST_MAX_DBS caps open test DBs, default 5 ≈ 5 × (32 + 20) = 260 conns at worst of max_connections 300; CI raises its Postgres service to 300 too and uses 4)
```

The database creation and the three exports live in `scripts/lib/testdb.sh` (`ensure_test_db`, `export_test_env`), sourced by `verify.sh test` and `check-all.sh`: change them there, not in each script.

## Which tier for which change

- Go code in `booking/`, `store/`, migrations → **fast + test** (always under `-race`).
- `httpapi/`, `auth/` → **fast + unit**; add **test** if it touches booking/store calls.
- `realtime/`, `events/`, `worker/`, outbox → **fast + test**, and **full** (or `smoke_ws.sh`) before calling it done — fan-out across instances is only proven on the cluster.
- `web/` only → `cd web && npx tsc --noEmit && npm test`.
- Docker / gateway / compose → **full**.
- `backend/cmd/stress` → **fast + test**: its DB tests run the SQL integrity pass and the independent oracle against the engine's availability (`TestOracleAgreesWithAvailability`) on every PR; the phases themselves only run against a stack (`./scripts/cluster-checks.sh stress`).
- `scripts/`, workflows → **fast** (shellcheck); cluster-check changes → `./scripts/cluster-checks.sh <step>` against a running stack.

## Rules

- Never report "tests pass" when integration tests were skipped — say they skipped and why.
- If `lint.sh` fails on format, run `fmt.sh` and re-run; do not hand-edit whitespace.
- Do not add `//nolint` to silence gosec/bodyclose/rowserrcheck/contextcheck without a written reason (`nolintlint` enforces `//nolint:<linter> // <reason>`). `sqlclosecheck` is deliberately off: it cannot see `pgx.Rows`.
