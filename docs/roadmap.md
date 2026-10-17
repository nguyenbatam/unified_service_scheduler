# Roadmap, contingency plan and AI integration

What comes after the assessment build, in the order it would be done, and what the system falls back to when a part of it fails. Each item says why it matters and how it fits the current design. The guarantee never moves: **PostgreSQL decides whether a bay, a technician and a vehicle are free** (GiST exclusion), and nothing below (cache, AI, optimiser) is allowed to write a booking around it.

## 1. Product and domain (Phase 2)

| Item | Why | Fit with the current design |
|---|---|---|
| Job length from VIN, mileage and technician speed (FRM / labour-time guides) | A fixed duration per service type over- or under-books the day | `booked_duration_minutes` is already a snapshot on the row; only the value fed into `booking.Combine` changes |
| Multi-technician jobs | Some jobs need two people for part of the time | A second hold table (`appointment_technicians`) with its own GiST constraint; the bay hold stays as is |
| Drop-off capacity model | A car left for the day does not need a fixed window; it uses labour hours per skill | Separate capacity ledger per dealership-day and skill; kept out of V1 because the brief asks for both resources free for the whole duration |
| Parts and loaner cars as resources | A job without parts, or a customer without a courtesy car, is a bad booking | Same pattern as bays: a resource row plus a GiST constraint over the window |
| Repair order lifecycle (`CLOSED` after invoicing), DMS integration | Close the loop with Keyloop's DMS | Outbox events already carry every change; a DMS consumer subscribes to the same topic |
| Overrun recovery | A flagged overrun now holds its bay and technician until completed and alerts staff; the bookings already in its way keep their place until a dispatcher reassigns them | Run the one-move repair on them automatically (same window, another fitting bay or technician) and notify the customer only when no move exists |
| Bay capabilities | The waiter soft-upgrade assumes every quick-lane bay has a lift | Capability flags per bay (lift, alignment rig, diagnostics) matched against each service type's needs, replacing the fixed bay-type order |
| Waitlist instead of a hard 409 | Fill cancellations automatically | Consumer of `APPOINTMENT_CANCELLED`, offers the freed slot through the notifier |
| Real SMS / email providers, localisation, calendar invites (iCal) | Customers need the message in their language and calendar | The notifier already has a webhook sender; add provider adapters behind it |

## 2. Platform and scale

| Item | Why | How |
|---|---|---|
| Distributed tracing (OpenTelemetry) | The async path (outbox → Kafka → WS / notifier) has request IDs only on the HTTP side | Spans on HTTP → booking TX → each SQL round trip; store `traceparent` on `outbox_events`, carry it as a Kafka header, start a linked span in the consumer and the notifier; tail sampling keeps every error and 409; export over OTLP to Tempo or Jaeger |
| Availability cache | Availability is the hot read | Cache per dealership-day in Redis, invalidated by the outbox events of that day; Postgres stays the truth and booking never reads the cache |
| Read replicas | Lists and the day board do not need the primary | Route read-only handlers to a replica; availability that precedes a booking keeps reading the primary |
| Separate WebSocket gateway tier | Keeps long-lived connections apart from API CPU and deploys | Move the `realtime` hub and its per-boot Kafka fan-out consumer into their own process behind the gateway; tickets and `SESSION_REVOKED` handling stay as they are |
| Change data capture (Debezium) | Removes the polling outbox worker at larger scale | Same `outbox_events` table, published by CDC; consumers unchanged |
| Event schema registry (Avro / Protobuf) | Other teams will consume the events | Version the payload; today it is JSON kept in step by a test |
| Partitioning `appointments` by month | Keeps indexes and GiST small after years of history | Native range partitioning on `start_at`. Exclusion constraints then hold per partition only (an `EXCLUDE` must include the partition key with `=`), so two holds straddling a month edge are not checked against each other; the 24 h span bound confines that to the edge day, which needs an explicit guard — or partition by dealership instead, which keeps every constraint whole |
| Multi-tenant isolation | Many dealer groups on one platform | Tenant id on every row and Postgres row-level security as a second line behind `domain.Actor` |
| Kubernetes, HPA, multi-broker Kafka, Redis Cluster, managed Postgres HA | Production topology | The replicas are already stateless; only configuration changes |
| SLOs and error budgets | Decide when to stop shipping features | Booking p99 < 1 s and availability p99 < 500 ms (the bench already measures both), 99.9 % non-5xx |
| Staff SSO (OIDC), MFA, GDPR erasure and retention | Enterprise requirements | Replace the password login for staff; keep JWT access tokens and the refresh rotation |

## 3. Contingency plan (what happens when something fails)

| Failure | Behaviour today | Plan |
|---|---|---|
| Redis down | Booking continues; idempotency falls back to the durable unique index; rate limits fail open except login and register, which fall back to a bounded in-memory limiter (`TestBookingSurvivesRedisOutage`) | Alert on `scheduler_dependency_up{dep="redis"}`; no manual action |
| Kafka down | Bookings commit; events wait in the outbox with backoff, nothing is lost; the live board lags until Kafka recovers, then the backlog drains in order (a reload always shows the current state) | Alert on outbox backlog; it drains on recovery (chaos run proves it) |
| One API replica dies | The gateway sends traffic to the other; a retry with the same `Idempotency-Key` replays | Run at least 2 replicas per zone |
| Postgres primary lost | Bookings and reads fail with an error and readiness goes red; no wrong answer is ever given | Managed HA with synchronous standby (RPO ≈ 0, RTO < 1 min), PITR backups tested monthly |
| A hot dealership-day | In-memory admission gate queues it; other days are unaffected | Raise `dayGateWidth` or shard dealerships across replicas |
| An overrun nobody completes | It holds its bay and technician until completed, so it would block them on later days; the `StaleOverrun` alert fires after 12 h (`scheduler_queue_rows{queue="workshop",status="stale_overrun"}`) | Complete or extend it from the day board; the overrun recovery item above automates the follow-up |
| A domain extra misbehaves (one-move repair, waiter upgrade, overrun sweep) | — | Put each behind a per-dealership flag so it can be switched off without a deploy; the GiST guarantee does not depend on any of them |
| Bad deploy or migration | Migrations are forward-only and immutable | Expand / contract migrations, canary one replica, roll back the image, never the schema |
| Total outage | — | Read-only mode (day board from a replica) and a printed day sheet export so the workshop can keep working by hand |

## 4. AI integration

AI is added **around** the booking engine, never inside its guarantee. An AI feature reads through the public API and books through `POST /appointments` like any client, with a scoped token, an `Idempotency-Key`, and an actor recorded in `appointment_history`. If the AI provider is down, the system works exactly as it does today.

| Idea | Value | How it fits | First step |
|---|---|---|---|
| **Conversational booking assistant** (web chat, WhatsApp, voice) | Customers book in one sentence: "my brakes squeak, Saturday morning, I'll wait" | An LLM agent with tools mapped to `GET /availability/next` and `POST /appointments`, acting with the customer's own token (so ownership checks still apply); it proposes, the customer confirms, the engine decides | Tool definitions generated from `openapi.yaml`; an eval set of 200 scripted conversations |
| **Symptom → service triage** | Customers do not know which service they need | Classify free text into service types, required skill and bay type; staff confirm before work starts | Train on past repair orders; show the suggestion with a confidence and a "not sure" path |
| **Job length prediction** | Fixed durations cause overruns (the overrun sweep already measures them) | A model on actual `started_at`/`completed_at` by service, make, model, year, mileage and technician predicts labour time; bookings use a high percentile, so it feeds the Phase 2 FRM item | Offline evaluation against the overrun history before it touches a booking |
| **No-show prediction** | Empty bays cost money | Score each booking; high-risk ones get an extra reminder or a deposit request; never overbooking, since GiST forbids it, but a waitlist fill | Logistic baseline on no-show history, then compare |
| **Demand forecast and roster suggestions** | Rosters are set by hand | Forecast demand per skill per day; suggest shifts; the admin approves through the roster API, whose stranded-booking guard still applies | Weekly forecast report before any automation |
| **Schedule optimiser** | Greedy best fit plus one move is near, not at, the optimum | A nightly CP-SAT (OR-Tools) re-plan of not-yet-started bookings in the same windows (customers see no change), applied as reassigns that GiST guards; an LLM writes the dispatcher a one-line reason per change | Run in shadow mode and report the gain against the current engine (the stress run already measures fit) |
| **Technician copilot** | Faster write-up and clearer customer messages | Summarise the vehicle's history, open recalls by VIN, draft the customer explanation and an upsell suggestion | Read-only; no write access |
| **AIOps** | Faster incident response | Anomaly detection on the existing Prometheus metrics; an LLM summarises alerts, recent logs and outbox state into an incident note | Start from the existing alert rules |
| **AI in the engineering loop** | Keep quality as the code grows | The `invariant-reviewer` agent runs on every PR in CI; agents generate property tests and new oracle cases from the documented rules | Already used locally (see the README's AI narrative) |

Guardrails for every AI feature:

- **Authority:** AI suggests; the engine and Postgres decide. No AI path writes SQL or bypasses `booking.Service`.
- **Least privilege:** the assistant acts with the customer's token or a dedicated service role; prompt injection cannot reach another customer's data because authorization lives in `booking.Service`.
- **PII:** redact names, phones and emails before a model call; keep data in region; log prompts without PII.
- **Quality:** an eval set per feature, run in CI; shadow mode before any automatic action; a human handoff for low confidence.
- **Cost and latency:** a per-request token budget and timeout; the non-AI path is always available.
- **Audit:** bookings made by the assistant carry its actor id, so they show in history and metrics apart.
