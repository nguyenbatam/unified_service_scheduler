---
name: add-api-endpoint
description: >-
  Checklist for adding or changing an HTTP endpoint end to end in this repo —
  gin route + handler in backend/internal/httpapi, domain errors, auth/role
  middleware, metrics, hand-written OpenAPI (two copies), web api.ts/types.ts,
  and tests. Use when adding a route, changing a request/response shape, or
  exposing a new field to the UI.
---

# Add / change an API endpoint

## Backend

1. **Route** in `httpapi/server.go` `engine()` (`Handler()` only wraps it in `withCompression`):
   - Public: under `v1` (only health, docs, auth, ws today).
   - Authenticated: under `authed` (`requireAuth`). Self-service routes take a throttle: `plainReads` for reads, `reads`/`nextReads` for availability, `writes` for mutations.
   - Staff: under `staff` (`requireRole(domain.RoleAdmin, domain.RoleDispatcher)`), e.g. workshop transitions.
   - Admin: under `admin` (`requireRole(domain.RoleAdmin)`).
   - Mutations require JWT; booking-like POSTs accept `Idempotency-Key`; versioned mutations read `s.ifMatch(c)` and pass it to the service (`domain.CheckIfMatch` → 412).
   - Responses of 1 KiB or more are compressed (`httpapi/compress.go`). A body that carries a token or secret must sit under `/api/v1/auth/` or be added to `uncompressed` there, with a case in `compress_test.go` (BREACH). Never enable gzip for `/api/` in nginx: it weakens the strong ETag that `If-Match` needs.
2. **Handler** in `handlers.go` / `admin.go` / `auth.go` (keep each file < 1000 lines; add a new file by resource if needed):
   - Decode body with `s.bindJSON(c, &req, "invalid … body")` (one JSON value; over 1 MiB → 413 `too_large`); IDs with `s.pathUUID` / `s.queryUUID`.
   - Call `booking.Service` (or `store` for plain reads); never put SQL in handlers.
   - Respond via `s.ok` / `s.result` / `s.reply(c, http.StatusCreated|http.StatusOK, body, version, err)` (a version > 0 sets the strong ETag); errors via `s.fail`.
   - A list is paged: `s.pageLimit` + `cursor(...)` with a `newPageScope(listing, filters…)`, a store keyset query (`store/page.go`, add its SQL to `store/explain_test.go`), and `s.page(c, items, nextToken(...), err)`; the web reads it with `fetchPage` ("Show more", `usePagedList`) or `fetchAll` (small dropdowns only).
3. **Errors**: return `domain.NewError(domain.CodeX, "human message")` from service code. Codes map to `invalid` 400, `not_found` 404, `conflict` 409, `unavailable` 422, `unauthorized` 401, `forbidden` 403, `precondition_failed` 412, `precondition_required` 428, `rate_limited` 429, `busy` 503 (lock/statement timeout or request deadline). Raw errors become 500 and are logged with `request_id`.
4. **State changes on appointments** go through a `store.Tx` that writes the outbox row in the same TX (`InsertAppointmentWithEvent`, `MoveAppointmentWithEvent`, or `SetStatus`/`ExtendEnd` + `RecordAppointmentEvent`), emit a `domain.EventAppointment*` event (a new one: see `scheduler-orchestration`), and need the event added to `LIVE_APPT_EVENTS` in `web/src/live.ts` (plus routing in `realtime/event.go` `capacityEvents` if it changes availability) and to the WS description in OpenAPI.
5. Metrics: the `observe()` middleware already records requests/latency per route; add a domain counter only for business outcomes (like `scheduler_bookings_total`).

## Contract

6. Update `docs/openapi.yaml` (path, params, request/response schemas, error responses, `bearerAuth`). The PostToolUse hook copies it to `backend/internal/httpapi/docs/openapi.yaml`; check both match with `cmp`.
7. Web: add the type to `web/src/types.ts` (no `any`, enums as the numeric `const` objects) and a method on `api` in `web/src/api.ts` using relative `/api/v1/...`.

## Tests

8. Handler-level test in `internal/integration/http_test.go` (status codes incl. 401/403/409) and service/unit tests where logic lives.
9. Run `verify` tier **fast + test**; for booking/lifecycle endpoints also `scripts/smoke_cluster.sh` (book on one backend, read on the other).
