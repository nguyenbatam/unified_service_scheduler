---
name: scheduler-orchestration
description: >-
  Keyloop Scenario A Unified Service Scheduler domain and booking orchestration
  for this repo. Use when implementing or changing booking, availability,
  allocation (bay/technician/waiter/staging/lunch), appointment lifecycle,
  multi-instance sync, Redis idempotency, outbox/Kafka, or WebSocket fan-out.
paths:
  - "backend/internal/booking/**"
  - "backend/internal/store/**"
  - "backend/internal/worker/**"
  - "backend/internal/events/**"
  - "backend/internal/realtime/**"
---

# Scheduler orchestration (this repo)

The hard invariants are in `backend/AGENTS.md` (always loaded for `backend/`). This skill is the **how**. Deeper rationale: `docs/PLAN.md` §0.2 and §4.1, `docs/system-design.md`.

## Booking path (`booking.Service.Book`)

1. Validate input; load dealership, vehicle, service type.
2. Idempotency: Redis lookup → per-key advisory lock + Postgres lookup (first in the `Tx.BookingInputs` batch) → replay if same payload, 409 if different.
3. Check the requested slot (30-min grid, shop hours); lunch overlap or full staging → `unavailable` (422).
4. Pick resource candidates: bay of `required_bay_type` (waiter: soft-upgrade to express) + free tech with `skill_level >= min_skill_level` — waiter or not: lowest qualifying level, fewest spare skills, smallest idle gap on the technician's own day, then least loaded (`store.freeTechniciansSQL`). Nothing free: the one-move repair may make room (`booking/repair.go`).
5. In one TX (`booking.allocate`, state in `placement`, `booking/allocate.go`), per candidate under a savepoint: `Tx.InsertAppointmentWithEvent(…, domain.EventAppointmentCreated)` + idempotency key, behind the vehicle + dealership-day locks (a reschedule to another day also locks the day it leaves, `store.ArrivalLock.Leaving`; days in ascending order). The first pass takes those locks with the candidate read, in a savepoint (`alloc_day`): it waits for the day lock when the day is contended (`allocation.lockFirst`: the instance's gate saw others of the day, or a booking of that dealership collided on this instance within the last second, `booking.hotDealers`), else only tries it (free → exact read; busy → stale read, day lock waited for right before the insert). Holding the day lock there it checks technicians `NoWait` (`FOR SHARE SKIP LOCKED` + the fresh-snapshot roster statement): a technician (candidate or repair target) mid roster edit → roll the savepoint back (the day lock goes with it), wait for that technician's share lock outside the day's queue (`Tx.ShareLockTechnician`), and place again (at most 3 give-backs, then a plain lock-last pass). A GiST exclusion violation means a concurrent winner took that pair → re-read the candidates once under the day lock (exact) and try them; nothing free then → the one-move repair once → else `conflict` (409).
6. After commit: cache idempotency result in Redis (best effort, never the truth).

## Adding a lifecycle transition or event

1. Domain: status constant (holding vs releasing — update GiST predicate only via a new migration, see `add-migration`). A new event needs a `domain.EventType` code + name in `domain/codes.go` (codes 12–15 are taken; adjust the `IsAppointment`/`IsStored` ranges), a new migration widening `appointment_history_event_type_check` and `outbox_events_event_type_check`, and the mirror in `web/src/types.ts` `EventType` and `web/src/labels.ts`.
2. Service: a method calling `s.transition(ctx, actor, id, ifMatch, transition{verb, from, to, event: domain.EventAppointmentX, staffOnly, check})` (`booking/lifecycle.go`): it locks the row, checks access, If-Match and `from` (invalid `from` → `conflict`), and writes `SetStatus` + `RecordAppointmentEvent` in one TX.
3. HTTP: route in `httpapi/server.go` `engine()`: under `staff` for workshop transitions, or `appts` with the `writes` throttle if customers may do it (see `add-api-endpoint`).
4. Fan-out is automatic (outbox worker → Kafka `appointments.events` → each instance's hub). Add the event name to OpenAPI's WS description and `LIVE_APPT_EVENTS` in `web/src/live.ts` (plus routing in `realtime/event.go` `capacityEvents` if it changes availability).
5. Prove it: integration test for the transition, `scripts/smoke_ws.sh` for cross-instance delivery.

## Proving concurrency changes

- `go test -count=1 -race ./internal/booking/` and the integration "N concurrent books → exactly one confirmed" test.
- `scripts/stress.sh` asserts one winner per slot and no bay/tech overlap across both backends, races reschedules against new bookings (storm), judges availability against a SQL oracle (truth phase), plays a concurrent day (fill), compares the engine with every first-fit order plus the one-move repair (quality) and many customers (customers), and prints a requirements matrix; `scripts/bench.sh` measures capacity.
- Ask the `invariant-reviewer` subagent to review the diff.
