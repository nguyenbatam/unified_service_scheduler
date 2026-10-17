---
name: run-stack
description: >-
  Start, inspect, and stop this repo's full local stack (2 Go backends behind
  an Nginx gateway, Postgres, Redis, Kafka, web UI, Prometheus, Grafana) with
  Docker Compose, log in as the demo admin, and prove multi-instance sync. Use
  when asked to run, start, demo, or screenshot the app, or to check a change
  in the real running system.
argument-hint: "[up|down|status|logs <service>|smoke]"
---

# Run the stack

```bash
./scripts/up.sh      # creates .env if missing; DELETES the Postgres volume, then docker compose up -d --build (seed only)
./scripts/up.sh --keep   # same, keeping the existing database
./scripts/down.sh
docker compose ps
docker compose logs -f backend-1   # or backend-2, gateway, web, db, kafka
```

Default ports (overridable in `.env`):

| Service | URL |
|---|---|
| Gateway (load-balances both backends) | http://localhost:8080 |
| backend-1 / backend-2 | http://localhost:18081 / :18082 |
| Web UI | http://localhost:8081 |
| Swagger UI | http://localhost:8080/api/v1/docs |
| Prometheus / Grafana | http://localhost:9090 / http://localhost:3000 (admin/admin) |

Demo login (seeded when `SEED=true`): `admin@scheduler.local` / `admin123`. Get a token:

```bash
curl -s localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@scheduler.local","password":"admin123"}'
```

## Readiness

Healthy when `curl -sf localhost:18081/api/v1/health` and `:18082` return their own `instance` id, and the gateway `/api/v1/health` answers. Kafka can take ~30–60 s on first boot; WS events lag until the consumer joins.

## Prove it works

- `./scripts/smoke.sh` — gateway happy path.
- `./scripts/smoke_cluster.sh` — book on backend-1, read/cancel on backend-2.
- `./scripts/smoke_ws.sh` — book on A, receive `APPOINTMENT_CREATED` on B's WebSocket.
- `./scripts/stress.sh` — ends with a requirements matrix (every requirement of the brief PASS/FAIL with evidence; `REPORT_JSON` writes it): contended-slot hammer (exact winners, SQL integrity, idempotent resends, customer limits, both backends answered; `MIXED_OPS` adds If-Match mutations), `storm` (reschedules with If-Match racing new bookings for one start), `truth` (a day filled booking by booking against a SQL oracle, availability read on one backend and booked on the other), `fill` (a day of concurrent demand: refusals only when truly full, allocation quality, utilisation), `quality` (a scripted day where greedy first fit loses: the server vs every first-fit order, plus the one-move repair), `customers` (signed-in customer accounts booking their own cars, isolation probes). `PHASES=` picks phases.
- `./scripts/bench.sh` — capacity of this machine's stack: open-loop ramps for read / book / hot / journey, stopping at the first step past its p99 budget, with server metrics (Little's law, pool, PG commits) and host context.
- `./scripts/cluster-checks.sh chaos` — Redis stopped; Kafka paused (the outbox queues, drains afterwards, and the events reach WebSocket listeners on both backends); backend-1 killed mid-hammer and restarted (cut-off requests replay with their keys, capacity exact, integrity holds).
- `./scripts/demo-prep.sh` — on a fresh `up.sh` stack, adds the walk-in car the recorded demo books (Liam Carter's Ford Focus, skipped if present) and checks the slot grids the demo relies on (PASS/FAIL each).

After a backend code change, rebuild with `source scripts/ensure-env.sh && docker compose up -d --build backend-1 backend-2` (sourcing first replaces a `dev-only-change-me` JWT_SECRET placeholder in `.env`, which the backend refuses to start with).
