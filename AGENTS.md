# Unified Service Scheduler — agent guide

Keyloop Scenario A: resource-constrained service booking. Go API (2 replicas) + Postgres + Redis + Kafka + WebSocket, React web UI. Design: `docs/PLAN.md`, `docs/system-design.md`. Directory-specific rules: `backend/AGENTS.md`, `web/AGENTS.md`.

## Layout

| Path | What |
|---|---|
| `backend/cmd/server` | API binary; `backend/cmd/stress` load/integrity client |
| `backend/internal/` | `booking` (allocation engine), `store` (pgx SQL), `httpapi` (gin), `migrate` (embedded SQL), `worker` (outbox), `events` (Kafka), `realtime` (WS hub), `notify` (customer messages), `cache` (Redis), `config`, `auth`, `domain`, `seed`, `integration` (DB tests), `testdb` |
| `web/src/` | React 19 + Vite + TS UI |
| `scripts/` | `up/down`, `fmt`, `lint`, `verify`, `check-migrations`, `cluster-checks`, `smoke*`, `stress`, `bench`, `soak`, `check-all`, `demo-prep`, `ensure-env`, `lib/` (Python/bash helpers) |
| `deploy/` | gateway (Nginx), Prometheus, Grafana |

## Commands

```bash
./scripts/fmt.sh          # auto-format Go
./scripts/lint.sh         # = CI: OpenAPI copies (cmp), migration immutability, shellcheck, python helpers parse, gofmt, vet, golangci (go tool), go mod tidy drift, govulncheck, tsc, file-size
./scripts/check-migrations.sh [base]  # fails if an existing migration was modified/deleted/renamed (CI: PR base or push "before")
cd backend && go test ./...           # integration tests SKIP without TEST_DATABASE_URL (most of the suite!); use verify.sh test
cd web && npm test && npm run build
./scripts/up.sh [--keep] / down.sh    # full compose stack; up.sh starts from a fresh database (seed only) unless --keep
./scripts/verify.sh [fast|test|full]  # verification tiers: lint / + web unit tests + Go integration tests (-race) / check-all (no argument = full)
./scripts/check-all.sh                # everything incl. 2-backend cluster, WS, stress, chaos
./scripts/cluster-checks.sh [stack|smoke|cluster|ws|dms|stress|chaos ...]  # cluster e2e checks on a running stack (shared by check-all.sh and the Cluster workflow)
./scripts/stress.sh                   # requirements matrix: river/north/storm hammers, truth (SQL oracle), fill, quality (vs first fit + repair), customers → PASS/FAIL per requirement
./scripts/bench.sh                    # capacity of the running stack on this machine (read/book/hot/journey ramps, Little's law, pool, PG commits)
./scripts/demo-prep.sh                # on a fresh up.sh stack: adds the demo's walk-in car (Liam Carter's Ford Focus) and checks the slot grids the walkthrough shows (PASS/FAIL)
```

golangci-lint runs as `go tool golangci-lint` (pinned in `backend/go.mod`), config `backend/.golangci.yml`.

## Repo-wide rules

- **Max 1000 lines** per `.go`/`.ts`/`.tsx` file, tests included. Split by responsibility before crossing it (`lint.sh` fails otherwise).
- **OpenAPI has two identical copies**: `docs/openapi.yaml` and `backend/internal/httpapi/docs/openapi.yaml` (embedded, served at `/api/v1/docs`). Change both, hand-written, no codegen (`lint.sh` fails when they differ).
- Every API change updates OpenAPI + `web/src/types.ts`/`api.ts` in the same change.
- Config comes from `.env` (created from `.env.example` by `scripts/ensure-env.sh`, which replaces the `dev-only-change-me` JWT_SECRET placeholder with a random secret; the server refuses that placeholder); never commit `.env` or real secrets.
- A change is done when the matching verification tier passes (see `.claude/skills/verify`). Never claim green when integration tests were skipped.

## Skills

Agent skills live in `.claude/skills/` (read by both Claude Code and Cursor). Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs. Only skills present in `.claude/skills/` exist here; ignore references to other `samber/...` skills.
