# Decision record (historical): the plan for the Unified Service Scheduler

> Kept as a decision record of how the work was directed. The plan was approved before implementation continued, then updated as decisions landed (the "Done (v1.x)" and review notes). The sign-off checklist at the end is the gate the coding agent had to wait on. Some early details are superseded; for example, the exclusion constraints now cover the vehicle too and every holding status, and idempotency keys are unique per user. Where this file and [system-design.md](system-design.md) differ, system-design.md is authoritative.

At the time, this was the version locked before coding continued: the agent was not allowed to implement more until it was approved.

The Keyloop brief asks for one layer. The assessed layer is the **backend**; the **web UI** is a client harness to demo the booking flow, not a second implemented layer.

---

## 0. Review of the “Lead / DMS industry standard” draft

Three sources were compared: (A) the Scenario A brief, (B) the draft with 5 architecture commitments + Clean Architecture + Redis/WS/Outbox, (C) the automotive retail spec (FRM, bay type, skill level, waiter, staging, workshop state machine).

### 0.1 What the Keyloop brief actually grades

Three acceptance criteria:

1. Book by vehicle + service type + dealership + desired time.
2. Before confirming: check that a **ServiceBay** and a **qualified Technician** are free for the whole duration.
3. Persist the Appointment linked to customer, vehicle, technician, service bay.

Engineering Lead criteria: clear design, correct execution, tests as proof, AI collaboration with ownership, 5–10 minute video.

### 0.2 The “5 commitments” draft — keep / change / cut

| Commitment | Lead review | Decision (shipped / submitted) |
|---|---|---|
| **1. Prevent double-booking / races** | Required | **Done.** Postgres `EXCLUDE USING gist` on `tstzrange` (bay + technician + vehicle, holding statuses `confirmed`/`checked_in`/`in_progress`). Losing a race → **409**. Concurrent integration test |
| **2. Idempotency** | Required | **Done.** Postgres unique `(created_by, idempotency_key)` (scoped per user) in the same TX + **Redis cache** for fast multi-instance replay. Same key/payload → 200; different payload → 409 |
| **3. Outbox + zero-loss events** | Right when there is fan-out | **Done.** `outbox_events` in the same TX → worker → **Kafka** `appointments.events` |
| **4. WebSocket real-time** | Dispatcher dashboard | **Done.** Each backend consumes Kafka (a fresh consumer group per boot: `scheduler-ws-<instance_id>-<random>`) → broadcasts to its local hub `GET /api/v1/ws/dispatch`. FE day board is live |
| Redis Cluster (multi-region PTS) | Enterprise multi-region | **No.** One local Redis (idempotency replay, rate limits, single-use WS tickets) is enough for a take-home |
| HTTP router | Production groups | **Gin v1** + `gorilla/websocket`. No Fiber (fasthttp) |
| Waiter priority | DMS standard | **Done (V1.1).** `is_waiter` + express soft-upgrade — §4.1 |

**Why GiST exclusion, not Redlock / FOR UPDATE alone?**

- `SELECT … FOR UPDATE` locks **bay/technician rows**; it does not by itself protect the **time range**.
- `EXCLUDE … gist (id WITH =, tstzrange WITH &&)` is a hard Postgres guarantee even when the app has bugs.
- Redis does **not** replace the DB for double-booking; it only speeds up idempotency replay. The Postgres unique constraint in the same TX is still the source of truth.

### 0.3 The “automotive industry standard” draft — keep the thinking, cut the code scope

True to a real DMS: FRM / labor hours, skill level, bay type (express / general / diagnostic / alignment), waiter priority, staging capacity, state machine `BOOKED → CHECKED_IN → IN_PROGRESS → WORK_COMPLETED → CLOSED`.

| Industry concept | In design doc | In V1 code |
|---|---|---|
| Standard operation + duration | Yes | `service_types.duration_minutes` + `required_skill` (enough for the brief's AC) |
| Skill level 1–4 | **Done** | `technicians.skill_level` + `service_types.min_skill_level` (Oil 1 / Brake 2 / Major 3) |
| Bay type matching | Yes | `service_bays.bay_type` + `service_types.required_bay_type` |
| VIN → vehicle year → duration coefficient | Phase 2 | Fixed duration per service type |
| Waiter → express + fast tech | **Done** | `is_waiter` + express soft-upgrade (§4.1) |
| Staging / parking capacity | **Done** | `dealerships.staging_capacity` arrivals per `staging_window_minutes` (15/30/60; Northside=2/hour; 0=unlimited) |
| Technician work schedule (shifts, breaks, leave) | **Done (v1.2)** | `technician_shifts` + `technician_absences`; allocate/reassign/availability all apply them |
| Multiple jobs / 1 appointment | **Done (v1.2)** | `appointment_lines` (1–5), requirements snapshotted at booking time |
| Per-dealership policy | **Done (v1.2)** | lead time, cutoff, horizon, no-show grace, express max (`dealerships.*`) |
| Dispatching | **Done (v1.2)** | reassign technician/bay, early start pulls the time window forward, change history, earliest-slot search, staff per dealership |
| Customer notifications | **Done (v1.2)** | confirm/change/cancel + 24h/2h reminders (log or webhook) |
| Lunch break | **Done** | `dealerships.lunch_start/end` (Northside 12:00–13:00) |
| Workshop state machine | **Done (thin)** | `confirmed → checked_in → in_progress → completed` (+ cancel). Complete frees the bay/tech |
| Config cache in Redis | Yes (Phase 2) | Reads Postgres. Small seed volume; optimize later |
| Serializable isolation | Discussed in design | Default `Read Committed` + exclusion constraint. Serializable does not replace GiST exclusion and increases 40001 retries |

**Engineering Lead argument for the video / design:** understand the full DMS matrix, but **cut scope on purpose** — ship a correct constraint engine, with concurrent tests and an FE demo; do not ship an enterprise skeleton that cannot be proven in 10 minutes.

### 0.4 Review conclusion

- Backend = **Resource Orchestration Engine** + thin DMS industry features (waiter, skill level, staging, lunch, workshop lifecycle) + production-shaped stack.
- **Phase 2 / not shipped:** FRM/VIN duration coefficients, multi-tech express, Redlock, separate WS gateway cluster, Redis Streams, `CLOSED` after invoicing.
- The design doc states Phase 2 clearly so reviewers see that the scope cut is deliberate.

---

## 1. Deliverables

| Item | File / location |
|---|---|
| Plan (this file) | `docs/PLAN.md` |
| System design | `docs/system-design.md` (after the plan is locked) |
| Backend | `backend/` |
| Simulated web UI | `web/` |
| Local | `docker-compose.yml`, `scripts/up.sh`, `scripts/down.sh` |
| Full verify | `scripts/verify.sh` → unit/integration → smoke |
| README + AI narrative | `README.md` |

---

## 2. Tech stack (latest stable, 16 Oct 2026)

No RCs. Postgres 19 = RC1. TypeScript 7.1 = dev.

| Layer | Version / choice | Notes |
|---|---|---|
| Go | **1.27** (`go 1.27.2` in go.mod; image `golang:1.27.2-alpine`) | Latest version in use |
| Router | **Gin v1** (`gin-gonic/gin`) | Route groups + middleware; `net/http` compatible |
| DB driver | pgx **v5.11.0** | |
| Metrics | prometheus/client_golang **v1.25.0** | |
| DB | PostgreSQL **18.6** (`postgres:18.6-alpine`) | `btree_gist` + exclusion |
| Web | React **19.3.1**, TS **7.0.2**, Vite **8.3.4**, plugin-react **6.1.2** | |
| Test web | Vitest **5.0.5** | |
| Node local | 25.6.1 | `verify.sh` |
| Node image | `node:26.12.0-alpine` | Static build |
| Nginx | `nginx:1.30.6-alpine` | Stable |

**Included in the local/production-shaped stack:** Redis (idempotency replay, rate limits, single-use WS tickets), Kafka + outbox worker, WebSocket hub, **2 backend instances** + gateway LB.

Module: `github.com/nguyenbatam/unified_service_scheduler/backend`.

---

## 3. Directory structure

Keep a layout with clear layers; do **not** copy the full Clean Architecture boilerplate until it is needed. Rough mapping to usecase / infra / delivery:

```
backend/
  cmd/server/main.go
  internal/
    domain/           # entities, domain errors
    booking/          # orchestration: slots, allocate, book (usecase)
    store/            # postgres (infrastructure)
    migrate/          # SQL embed
    seed/
    httpapi/          # delivery
    integration/      # HTTP + DB tests
  Dockerfile
web/
  src/
  nginx.conf
  Dockerfile
scripts/{up,down,verify,check-all,smoke,smoke_cluster,smoke_ws}.sh
docker-compose.yml    # db, redis, kafka, backend-1/2, gateway, web, prometheus, grafana
docs/PLAN.md
docs/system-design.md
README.md
```

Compose ports: gateway `:8080`, backends `:18081/:18082`, web `:8081`, Prom `:9090`, Grafana `:3000`.

---

## 4. Domain & V1 assumptions (recorded in the system design)

### Orchestration matrix (order of checks when booking)

```
Request: vehicleId, serviceTypeId, dealershipId, startAt, isWaiter?, Idempotency-Key?
   │
   ▼
[1] Idempotency lookup (Postgres UNIQUE + Redis cache)
        │
        ▼
[2] Load vehicle → customer; load service → duration, required_skill, required_bay_type
        │
        ▼
[3] Validate start on the 30-min grid + end ≤ close_time (dealership timezone)
        │
        ▼
[4] Candidate bays: dealership + matching bay_type + no overlap with confirmed
        │   (bay matching required_bay_type first, best-fit gap; isWaiter + general job ≤ express_max_minutes: express is only a fallback — see §4.1)
        ▼
[5] Candidate techs: dealership + matching skill + no overlap with confirmed
        │   (same order for waiter or not: lowest qualifying level → fewest surplus skills → smallest gap → least load — see §4.1)
        ▼
[6] Pick by waiter / non-waiter policy
        │
        ▼
[7] INSERT appointment (confirmed, is_waiter) in a TX; exclusion fails → try another pair / 409
```

### 4.1 Waiter vs Non-waiter (shipped)

In the DMS industry: a **Waiter** customer sits and waits for the car → needs a short duration / fast release. A **Non-waiter** customer drops off the car and leaves → more flexibility, no need to “scrape up” express capacity.

| | Waiter (`isWaiter: true`) | Non-waiter (`false` / default) |
|---|---|---|
| Business meaning | Can be soft-upgraded to an Express Bay when no general bay is left (short job) | Normal allocation by required_bay_type |
| Bay pick | Bay matching `required_bay_type` first, **express is only a fallback** (job ≤ `express_max_minutes`); within each group: best-fit gap, then name | Filter `required_bay_type` (+ express spills over to general for express jobs), best-fit gap, then name |
| Tech pick | **Same as non-waiter** (previously: least load, then most senior — spread work across all technicians, broke up timelines and used up senior technicians that later large jobs needed; 400 stream: best fit 165630 vs 164970 minutes, 31 vs 33 refusals) | Lowest qualifying level → fewest surplus skills → best-fit gap on the technician's own schedule (shifts/breaks are boundaries) → least load (review round 2: least-load spread work across all technicians, breaking up the long gaps that long jobs need) |
| Soft upgrade | If the service requires `general` but an **express** bay is free at the same dealership + same skill → it **may** be assigned express (shorter wait). Do not assign diagnostic/alignment to a waiter unless required | No soft-upgrade; only the exact `required_bay_type` |
| Availability | Slot is still `available` if min(freeBay, freeTech) > 0; optional field `waiterPreferred: true` when an express bay is free (FE hint) | No hint needed |
| Hard guarantee | GiST exclusion unchanged — waiters cannot be double-booked | Same |

**API / schema**

- Column: `appointments.is_waiter BOOLEAN NOT NULL DEFAULT false` (new migration).
- `POST /appointments` body adds `isWaiter` (bool, default `false`).
- Appointment response + schedule board show `isWaiter`.
- Idempotency: `sameBooking` also compares `isWaiter` (same key, different waiter → 409).
- FE: checkbox “I will wait at the dealership” on the wizard; day board badge `Waiter`.

**Required tests**

- Waiter + Oil: assigned express (already in the seed).
- Waiter + Brake (`required_bay_type=general`): if an express bay is free → assign express; if express is full → general, like non-waiter.
- Non-waiter + Brake: never assigned express.
- Concurrent waiter/non-waiter on the same slot still yields exactly 1 confirmed (exclusion).

**Not in waiter V1.1:** shortening `duration` with multi-tech; “fast tech” scoring by historical throughput (waiters use the same best-fit order, no separate proxy); auto-reschedule of non-waiters.

### Operating assumptions

- One appointment = **1 bay + 1 tech** for the whole `[start, end)`.
- Fixed durations: Oil 30 (`lubrication`, bay `express`); Brake 90 (`brakes`, `general`); Major 180 (`general`, `general`).
- 30-minute grid; no overnight; lunch per dealership (`lunch_start`/`lunch_end`, Northside 12:00–13:00).
- Timezone + open/close on the dealership. Seed `Europe/London`.
- Half-open interval: 08:00–08:30 and 08:30–09:00 do **not** conflict.
- Customer is taken from the vehicle; the body does not send customerId.
- Cancel → `cancelled`; exclusion applies only to holding statuses (`confirmed`/`checked_in`/`in_progress`) → the slot reopens.
- **Waiter:** see §4.1 (V1.1).
- **Auth (in place):** JWT login/register/me; seeded demo admin; all business APIs + WS require Bearer (public: health/docs/login/register, and refresh by cookie). Not yet: payment, parts, FRM/VIN duration, multi-tech express, RO `CLOSED` after invoice.

---

## 5. Schema

Tables: `dealerships`, `customers`, `vehicles`, `service_types`, `service_bays`, `technicians`, `technician_skills`, `appointments`, `outbox_events`.

Added compared with the old plan (thin industry-standard domain):

- `service_bays.bay_type` SMALLINT `1=express | 2=general | 3=diagnostic | 4=alignment`
- `service_types.required_bay_type` / `required_skill` use the same SMALLINT codes (skill `1=lubrication | 2=brakes | 3=general`)
- `appointments.is_waiter` boolean (default false)

Hard constraints (resources are held while `confirmed | checked_in | in_progress`):

```sql
-- appointment.status SMALLINT: 1=confirmed 2=checked_in 3=in_progress 4=completed 5=cancelled
EXCLUDE USING gist (service_bay_id WITH =, tstzrange(start_at, end_at, '[)') WITH &&)
  WHERE (status IN (1, 2, 3))
EXCLUDE USING gist (technician_id WITH =, tstzrange(start_at, end_at, '[)') WITH &&)
  WHERE (status IN (1, 2, 3))
```

+ `UNIQUE (created_by, idempotency_key)` (keys are scoped per creating user).

Migrations are embedded and run at startup. Import `time/tzdata` in the Alpine binary.

### Northside seed (the next London day whose 09:00 is at least an hour ahead: today early in the morning, otherwise tomorrow)

- 09:00 Oil — takes 1 express bay + 1 lubrication tech
- 10:00 Brake ×2 — takes all `brakes` techs (10:00 brake slot → `no_technician` even though bays are free)

Riverside is empty for comparison.

---

## 6. API

Prefix `/api/v1`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/health` (= `/health/ready`), `/health/live` | readiness: Postgres down → 503; Redis down or pool saturated → 200 `degraded` |
| GET | `/dealerships` | + `today` per TZ |
| GET | `/service-types` | duration, skill, requiredBayType |
| GET | `/vehicles?q&limit` | + customerName; one page (default 50, max 200), search by VIN/make/model/customer name (pg_trgm) |
| GET | `/vehicles/{id}` | 404 if the vehicle belongs to another customer |
| GET | `/availability?dealershipId&vehicleId&serviceTypeId&date` | slots + freeBays + freeTechnicians + reason |
| GET | `/schedule?dealershipId&date` | bays + confirmed appointments (static Kanban / poll) |
| POST | `/appointments` | allocate; body `isWaiter?`; 201 create / 200 idempotency replay |
| GET | `/appointments/{id}` | |
| POST | `/appointments/{id}/cancel` | |
| GET | `/metrics` | Prometheus |
| GET | `/ws/dispatch` | WebSocket upgrade (single-use `?ticket=` from `POST /auth/ws-ticket`, or Bearer for staff/service clients; dispatch board) |

`reason`: `past` | `lead_time` | `beyond_horizon` | `lunch_break` | `vehicle_busy` | `staging_full` | `no_bay` | `no_technician` | `no_resources` (day level: `reason: day_too_short`; a closed day sets `closed`); full contract in `docs/openapi.yaml`.

Availability query optional `isWaiter=true` (counts express soft-upgrade for general services). Optional `excludeAppointmentId` (reschedule view): ignores that appointment's own hold, like the reschedule itself; must be the caller's visible appointment for the same dealership + vehicle.

Errors: `{"error":{"code","message"}}` — `invalid` 400, `unauthorized` 401, `forbidden` 403, `not_found` 404, `conflict` 409, `precondition_failed` 412, `unavailable` 422, `precondition_required` 428, `rate_limited` 429, `busy` 503.

Headers: `Idempotency-Key`, `X-Request-ID`. JSON logs. Metrics by route pattern.

---

## 7. Web (simulated front end)

One page, relative `/api` (Vite proxy / Nginx proxy).

1. Choose dealership, vehicle, service, date (`today` by default); **Waiter** checkbox (waiting at the dealership).
2. Slot grid; unavailable slots show a reason; Confirm + Idempotency-Key stays stable until the slot changes (changing waiter → new key, or treated as a different payload).
3. Day board by bay; Waiter badge. After book/cancel → refetch + WS event.

WebSocket live refresh. Confirm button disabled while submitting.

---

## 8. Scripts & verify

- `up.sh` / `down.sh` — compose
- `verify.sh [fast|test|full]`: fast = `lint.sh`; test = lint + web `npm test` + db/redis + `go test -count=1 -race ./...` on the test database (`REQUIRE_INTEGRATION=1`); full (default) = `check-all.sh` (2-backend cluster, WS, stress, chaos)
- `smoke.sh` (Python stdlib): health, brake `no_technician`, book, idempotency replay, schedule, cancel

---

## 9. Required tests (Engineering Lead evidence)

**Unit:** slot grid, closing time, half-open overlap, Classify reasons, date parsing without TZ drift, bay_type filter logic.

**Integration (real Postgres):**

- Successful booking; customer = vehicle owner
- Tech missing the skill / bay of the wrong type → not assigned
- Back-to-back on the same bay OK
- Off-grid / outside hours / overrunning closing → 400
- **Concurrent N requests for the same slot / 1 bay + 1 tech → exactly 1 confirmed, the rest 409**
- Idempotency same key / different payload
- Cancel then rebook the same slot OK
- Technician choice: level → surplus skills → best-fit gap → load (`allocation_policy_test.go`), compared with first-fit using an offline oracle (`allocation_regret_test.go`)
- Waiter soft-upgrade to express / Non-waiter does not take express when required=general (§4.1)
- HTTP 201 / 200 replay / error envelope

**Web Vitest:** time formatting per TZ; block layout on the day board.

---

## 10. What the system design had to cover (written before implementation)

Items 1, 4, 5 (staging, waiter priority, skill level) and 6 were built afterwards (see §0.2). Still not built: 2 (separate WebSocket gateway process), 3 (Redis config cache) and the vehicle-age factor in 5.

For the “Problem Solving & System Design” score:

1. Outbox in the same TX as the appointment → worker → Kafka / internal bus → Workshop Kanban.
2. WebSocket gateway as a separate process (connection isolation).
3. Redis cache for `service_types` + skills (read-heavy config).
4. Full RO state machine + WS updates when `IN_PROGRESS` / `WORK_COMPLETED` frees resources earlier than `end_at`.
5. Staging capacity + waiter priority + skill level + vehicle age factor.
6. Multi-instance API: still relies on exclusion; idempotency still in Postgres (or Redis only with a distributed gateway and its own SLA).

---

## 11. Order of work after the plan is locked

1. Align the existing backend with this plan (bay_type, exclusion, idempotency, tests).
2. Wire `web/` to the API.
3. Compose + scripts.
4. `scripts/verify.sh` green.
5. `docs/system-design.md` + `README.md` (AI narrative).

---

## 12. Agent skills (reviewed)

Accept/reject details: `docs/SKILLS-REVIEW.md`.

- **Shared rules:** `AGENTS.md` (root, `backend/`, `web/`) — Cursor reads them directly, Claude Code imports them via `CLAUDE.md`. Booking invariants (GiST exclusion; Redis idempotency; outbox→Kafka→WS; waiter/staging/lunch/lifecycle) live in `backend/AGENTS.md`.
- **BE skills:** curated from [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang) → `.claude/skills/golang-*` (15 skills). The whole repo is not pulled in (gRPC/GraphQL/samber libs/DI frameworks dropped).
- **Project skills:** `.claude/skills/{scheduler-orchestration,verify,add-migration,add-api-endpoint,run-stack}`; hooks + subagent `invariant-reviewer` in `.claude/`.
- **Reject:** [linkedin-skill-assessments-quizzes](https://github.com/Ebazhanov/linkedin-skill-assessments-quizzes) as an agent skill.

## 13. Sign-off checklist (for your review)

- [x] Orchestration + GiST + concurrent test
- [x] Redis idempotency + outbox/Kafka + WS multi-instance
- [x] bay_type, skill code + skill level, waiter, staging, lunch, workshop lifecycle
- [x] OpenAPI 3 + Swagger UI (`/api/v1/docs`)
- [x] System design: assumptions + tech justifications + GenAI design section
- [ ] Remaining Phase 2: FRM/VIN duration, multi-tech express (recorded in the system design)
- [x] 5–10 minute video ([Loom](https://www.loom.com/share/33cac6a7e3b9422ca3e93c833f205cbe))
- [x] Agree on the stack versions in section 2
- [x] Agree on the skill set in section 12 / `docs/SKILLS-REVIEW.md`

*(The checklist above was the approval gate the coding agent waited on.)*
