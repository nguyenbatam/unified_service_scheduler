# Unified Service Scheduler (Keyloop Scenario A)

Resource-constrained booking with **two API instances**, shared **Postgres**, **Redis** idempotency, **Kafka** outbox events, and **WebSocket** live sync.

## For reviewers

| | |
|---|---|
| Video walkthrough | [Loom](https://www.loom.com/share/33cac6a7e3b9422ca3e93c833f205cbe) · slides shown in it: [docs/slides.pdf](docs/slides.pdf) |
| System design | [docs/system-design.md](docs/system-design.md) — one-page summary first, diagrams (Mermaid) and details below |
| AI Collaboration Narrative | [in this README](#ai-collaboration-narrative); the plan the work followed, kept as a decision record: [docs/PLAN.md](docs/PLAN.md) |
| API contract | [docs/openapi.yaml](docs/openapi.yaml); Swagger UI at http://localhost:8080/api/v1/docs once the stack runs; [cURL walk-through](#try-the-core-flow-with-curl) |
| Core business tests | [Test](#test) |
| Roadmap, contingency plan, AI integration | [docs/roadmap.md](docs/roadmap.md) |

**Layer implemented:** the backend (Go REST API on PostgreSQL). The React app in `web/` is only the client harness that shows the flow; it is not the assessed layer.

**The guarantee:** a bay, a technician and a vehicle can never be double-booked. Postgres enforces it with GiST exclusion constraints on `tstzrange` over every holding status, so it holds even when two API servers race ([system design → Guarantees](docs/system-design.md#guarantees)).

### Core vs extras

The brief's three requirements live in a small part of the code; everything else is hardening or workshop realism, and all of it sits behind the same database guarantee.

| Core: R1–R3 (read first) | Hardening ("build for the future") | Domain extras (optional) |
|---|---|---|
| Schema and the 3 GiST constraints: `backend/internal/migrate/migrations/001_schema.sql` (`appointments`), with the integrity FKs and checks | Transactional outbox → Kafka → WebSocket (`worker`, `events`, `realtime`) | One-move repair (`booking/repair.go`) |
| Book: `backend/internal/booking/book.go` (`Service.Book`), `allocate.go`, `requirement.go` (job lines, bay fit) | Idempotency keys (Postgres unique index, Redis fast path) | Waiter soft-upgrade, express spill (`booking/requirement.go`, SQL twin `store.bayFitsSQL`); recommended starts (`fit.go`) |
| Availability: `booking/availability.go`, `slots.go`, `duty.go` | Dealership-day lock and admission gate (`gate.go`) | Rosters, staging, lunch, closures, per-dealership policy |
| Candidate SQL: `backend/internal/store/allocation.go` | Metrics, alerts, Grafana; JWT + refresh rotation; rate limits | Lifecycle, overruns, reassign, notifications |
| Tests: `integration/races_winner_test.go`, `booking_capacity_test.go`, `burst_test.go`, `dst_parity_test.go` | Stress oracle, bench, chaos (`backend/cmd/stress`, `scripts/`) | |

**A 30-minute review path:** this README → [system design summary and requirements map](docs/system-design.md#requirements--mechanism--proof) → the core column above → [the cURL walk-through](#try-the-core-flow-with-curl) → `./scripts/verify.sh test`.

Quick start (Docker Engine 25+ with Compose v2):

```bash
./scripts/up.sh            # build and start the whole stack on a fresh database (seed only); creates .env on first run
                           # web UI: http://localhost:8081 (demo logins below)
./scripts/verify.sh test   # lint + web tests + all Go tests against real Postgres/Redis, under -race
./scripts/down.sh
```

## Try the core flow with cURL

With the stack up (`./scripts/up.sh`) and `jq` installed. The seed uses fixed ids: Riverside Service is `22222222-…`, the customer's BMW is `e1000000-…-001`, Oil Change is `51000000-…-001`.

```bash
API=http://localhost:8080/api/v1
DEALER=22222222-2222-4222-8222-222222222222
VEHICLE=e1000000-0000-4000-8000-000000000001
OIL=51000000-0000-4000-8000-000000000001

# Sign in (staff; the customer account works the same way for its own car)
TOKEN=$(curl -s $API/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@scheduler.local","password":"admin123"}' | jq -r .token)

# R2: the first start where a fitting bay AND a qualified, on-duty technician are free for the whole job
START=$(curl -s "$API/availability/next?dealershipId=$DEALER&vehicleId=$VEHICLE&serviceTypeId=$OIL&limit=1" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.slots[0].start')

# R1: request the appointment for that vehicle, service type, dealership and time
KEY=$(uuidgen)
BODY="{\"dealershipId\":\"$DEALER\",\"vehicleId\":\"$VEHICLE\",\"serviceTypeId\":\"$OIL\",\"startAt\":\"$START\"}"
ID=$(curl -s $API/appointments -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" -d "$BODY" | jq -r .id)   # 201 Created, Location: /api/v1/appointments/$ID

# R3: the persisted record names the customer, vehicle, technician and bay
curl -s $API/appointments/$ID -H "Authorization: Bearer $TOKEN" \
  | jq '{id, status, customerName, vehicleLabel, technicianName, serviceBayName, startAt, endAt}'

# A retry with the same key replays: 200 and the same id, no second booking
curl -s -o /dev/null -w '%{http_code}\n' $API/appointments -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" -d "$BODY"
# The same start with a new key is refused (409): this vehicle already holds that window
curl -s $API/appointments -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" -d "$BODY" | jq .
```

Every endpoint, with schemas and error codes, is in Swagger UI at http://localhost:8080/api/v1/docs.

## Stack

| Layer | Tech |
|---|---|
| Gateway | Nginx (`least_conn`, WebSocket upgrade) |
| API | Go **1.27.2**, pgx (no ORM), 2 replicas |
| DB | PostgreSQL **18.6** (enums, `timestamptz`, GiST exclusion, outbox) |
| Cache | Redis **8** |
| Events | Kafka **4** (KRaft) |
| Realtime | WebSocket hub per instance |
| UI | React **19** + Vite **8** + TS **7** |
| Metrics | Prometheus **3.15** + Grafana **13.2** |

## Ports

| Service | Port |
|---|---|
| Gateway (LB) | http://localhost:8080 |
| backend-1 | http://localhost:18081 |
| backend-2 | http://localhost:18082 |
| Web UI | http://localhost:8081 |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (`admin`/`admin`) |
| Postgres | 5432 |
| Redis | 6379 |
| Kafka (host) | 9094 |

Only the gateway and the web UI listen on every interface; everything else is bound to 127.0.0.1. Network exposure, metrics, client-IP handling, auth sessions and the WebSocket endpoint: [docs/operations.md](docs/operations.md).

## Run

Needs Docker Engine 25+ with Compose v2 (healthchecks use `start_interval`; `up --wait`), and for the scripts Go (version in `backend/go.mod`), Node (`web/.nvmrc`), Python 3, and curl; `jq` for the cURL walk-through and the agent hooks, `shellcheck` for `lint.sh`. `./scripts/lint.sh` runs `govulncheck`, which needs network access to the vulnerability database.

Config lives in `.env` (from `.env.example`). `./scripts/up.sh` creates `.env` if missing.

```bash
cp .env.example .env   # optional; edit secrets/ports
./scripts/up.sh        # fresh database every time: the seed only (demo dealerships, users, a few bookings from today)
./scripts/up.sh --keep # keep the existing database (bookings, accounts, stress-run data)
```

By default `up.sh` deletes the local Postgres volume first, so the stack always starts from the seed; stress and bench runs leave thousands of rows behind otherwise. Metrics and dashboards are kept.

Stop: `./scripts/down.sh`

### Demo login (seeded when `SEED=true`)

| Email | Password | Role |
|---|---|---|
| `admin@scheduler.local` | `admin123` | admin (1) |
| `dispatcher@scheduler.local` | `dispatch123` | dispatcher (2), Northside only |
| `customer@scheduler.local` | `customer123` | customer (3), owns Amelia Clarke's BMW |

The login page offers these as one-click buttons only in a demo build: `npm run dev`, or a web
image built with `VITE_DEMO=true` (the compose stack passes `VITE_DEMO` from `.env`, default
`true`). A build without it (`docker build web`, `npm run build`) ships neither the buttons nor the
passwords; set `VITE_DEMO=false` for any real deployment.

## Test

```bash
./scripts/verify.sh test   # lint, web unit tests, then every Go test under -race (starts Postgres + Redis and a test database)
./scripts/verify.sh full   # everything: + benchmark smoke, the 2-backend cluster, WebSocket, stress matrix and chaos (check-all.sh)
```

A plain `go test ./...` skips the integration tests unless `TEST_DATABASE_URL` is set; `verify.sh test` sets it up. Without Docker, the allocation rules alone run in seconds: `cd backend && go test ./internal/booking/...`.

Where the core business logic is tested:

| What | Where | Examples |
|---|---|---|
| Allocation rules: fit, bay type, technician choice, calendar, booking policy | `backend/internal/booking/*_test.go` (unit) | `fit_test.go`, `requirement_test.go`, `calendar_test.go`, `policy_test.go` |
| No double booking for the whole duration, races, idempotency, outbox, Redis outage | `backend/internal/integration/` (real Postgres + Redis) | `TestExclusionConstraints`, `TestBookingRefusedWhenResourceBusyOnlyInSecondHalf`, `TestSlotBurst` (many clients, one slot, two instances: exactly one winner), `TestConcurrentRetrySameKeyReplays`, `TestBookingSurvivesRedisOutage`, `TestOutboxRetryBackoffAndPoison` |
| Availability agrees with booking | `backend/internal/integration/` | `TestAvailabilityMatchesBookingForEverySlot` |
| Every requirement of the brief, judged by an independent SQL oracle | `backend/cmd/stress` via `./scripts/stress.sh` | `TestOracleAgreesWithAvailability` (a Go DB test, every PR); the requirements matrix of `./scripts/stress.sh` ([docs/testing.md](docs/testing.md)) |

### Prove multi-instance sync

```bash
./scripts/smoke_cluster.sh
# Book on :18081 → read/cancel on :18082 (shared DB + Redis)
```

### Stress, bench and chaos

- `./scripts/stress.sh` runs every phase against the running cluster and ends with a requirements matrix: each requirement of the brief (R1–R3) and each quality goal (concurrency, scalability, performance, reliability, optimisation, observability, many users) gets `PASS`/`FAIL`/`SKIP`, judged by an SQL integrity pass and an oracle that re-states the rules without the engine's code.
- `./scripts/bench.sh` measures capacity on this machine (an Apple M4 laptop under load sustained 3200 availability reads/s and 200 uncontended bookings/s within the p99 budget; conditions and the other workloads in [docs/testing.md](docs/testing.md)).
- `./scripts/cluster-checks.sh chaos` repeats key checks with Redis stopped, with Kafka paused, and with a backend killed mid-load.

Phases, thresholds, the full evidence table and bench results: [docs/testing.md](docs/testing.md).

### Lint / format (local = CI)

```bash
./scripts/fmt.sh    # gofmt + golangci formatters
./scripts/lint.sh   # OpenAPI copies, migration immutability, shellcheck, gofmt, go vet, golangci (staticcheck/gosec/modernize/…), go mod tidy drift, govulncheck, tsc, file size
```

`./scripts/verify.sh fast` runs only `lint.sh` (CI's static checks); `test` and `full` are described under [Test](#test). `full` leaves the stack running; `./scripts/down.sh` stops it.

GitHub Actions: `.github/workflows/ci.yml` runs the same `lint.sh`, the Go tests in parallel shards (the integration package dealt into three by test name, every other package in a fourth) and the web tests; `.github/workflows/cluster.yml` builds the Docker images, brings up the 2-backend cluster and runs, in two parallel legs on fresh stacks, the cluster checks with the stress matrix and the chaos checks.

## Data path

`POST /appointments` → TX (`appointments` + `outbox_events`, under the dealership-day lock) → after COMMIT: wake the local outbox worker, NOTIFY the other instances → Redis idempotency → outbox worker → Kafka → every instance routes the event to its WebSocket clients → UI refreshes.

## API docs (OpenAPI / Swagger)

With stack up:

- Spec: http://localhost:8080/api/v1/openapi.yaml  
- UI: http://localhost:8080/api/v1/docs  

Authorize with JWT from `POST /api/v1/auth/login` (demo admin above).

## Docs

- [docs/system-design.md](docs/system-design.md) — architecture, assumptions, tech justifications, observability, GenAI design section
- [docs/slides.pdf](docs/slides.pdf) — the video's slides, a presentation view of the system design (the document is authoritative)
- [docs/PLAN.md](docs/PLAN.md) — the plan the work followed, kept as a decision record
- [docs/decisions.md](docs/decisions.md) — measurements behind the allocation choices, session revocation and the auth-cookie design
- [docs/roadmap.md](docs/roadmap.md) — Phase 2, platform roadmap, contingency plan, AI integration ideas and guardrails
- [docs/testing.md](docs/testing.md) — stress phases, requirements matrix, bench, chaos
- [docs/operations.md](docs/operations.md) — network exposure, metrics, client IP, compression and caching, auth sessions, WebSocket
- [docs/openapi.yaml](docs/openapi.yaml) — API contract (also served at `/api/v1/openapi.yaml`)
- [docs/SKILLS-REVIEW.md](docs/SKILLS-REVIEW.md) — how third-party agent skills were vetted
- [AGENTS.md](AGENTS.md) — rules every coding agent follows in this repo

## AI Collaboration Narrative

### Strategy

Tools: Claude Code as the main coding agent, Cursor alongside it; both read the same `AGENTS.md` rules and `.claude/skills/`.

Treat GenAI as a fast implementer under a fixed architecture brief: Scenario A booking with a **DB-enforced** double-book guarantee, multi-instance demos, and observability. Prefer short, reviewable diffs over large unreviewed dumps.

### Guiding the AI

1. Locked hard rules first (GiST on `tstzrange`, outbox-in-TX, Redis ≠ source of truth).
2. Asked for packages/tests/scripts aligned to those rules; rejected app-level-only locking.
3. Drove domain extras (waiter, staging, lunch, lifecycle, JWT) only after core concurrency tests were green.
4. Required OpenAPI as the external contract for Lead/reviewer tooling.
5. Wrote the plan down and approved it before implementation continued; it was updated as decisions landed and is kept as a decision record ([docs/PLAN.md](docs/PLAN.md)).
6. Turned the rules into guardrails instead of repeating them in prompts. Enforced:
   - CI runs `lint.sh` (OpenAPI copies identical, migration immutability, gofmt, vet, golangci, govulncheck, tsc, file size) and the tests on every push to `main` and every pull request, so a change from an agent or a person meets the same bar.
   - Agent hooks (`.claude/settings.json`, need `jq`): `gofmt` and OpenAPI sync after every edit, confirmation before an existing migration is edited.

   Advisory (the agent is instructed to use them; nothing blocks a change without them):
   - `AGENTS.md` (root, `backend/`, `web/`): the rules every agent reads first (Claude Code and Cursor read the same files).
   - An `invariant-reviewer` sub-agent (`.claude/agents/`) that reviews diffs to `booking`, `store`, `migrate` and `worker` against the booking invariants.
   - Project skills such as `/verify` (fast / test / full tiers); third-party agent skills were vetted before use ([docs/SKILLS-REVIEW.md](docs/SKILLS-REVIEW.md)).

### Verifying and refining

- Unit + integration: concurrent N books → exactly one confirmed.
- `scripts/smoke.sh`, `smoke_cluster.sh`, `smoke_ws.sh`, `stress.sh` for gateway / multi-instance / integrity; the stress run ends with a requirements matrix judged against a SQL oracle, and `bench.sh` measures capacity.
- The load tools found real defects the unit tests could not: GiST deadlocks between overlapping bookings on a realistic mixed day (p99 6.1 s → 105 ms with a dealership-day lock), and losers of a hot slot draining the connection pool (p99 7.5 s; fixed by re-reading candidates under the lock and an in-process admission gate). Each now has a regression test; the table is in [system design → GenAI](docs/system-design.md#genai-in-the-design-phase).
- Independent review passes by sub-agents (one per area: engine, API contract, lifecycle, documentation) against the brief; every finding was verified in the code before it was fixed, and each fix shipped with a test (for example `TestFlaggedOverrunCarriesOverToTheNextDay`, `TestCheckInRefusedOnceTheWindowEnded`, `TestSchemaHoldsTheBookingIntegrity`).
- `scripts/lint.sh` (gofmt, vet, golangci, govulncheck) + CI workflow.
- Human rewrote fragile bits: exclusion constraints, idempotency replay across instances, Kafka consumer join timing.

### Ownership

Final design choices (GiST over Redlock-as-truth, outbox over dual-write, SMALLINT enums, JWT on every business API) are human-owned. The AI wrote most of the code; I owned the design, the invariants and the review, and the tests, the stress oracle and CI decided what was kept.
