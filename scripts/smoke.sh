#!/usr/bin/env bash
# Env: BASE_URL (gateway), ADMIN_EMAIL / ADMIN_PASSWORD (default: the seeded demo admin).
# Ends with the session refresh round trip (login cookie -> /auth/refresh rotates it -> logout clears
# it) on its own customer account, smoke-refresh@scheduler.local (registered on first use).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${BASE_URL:-http://127.0.0.1:8080}"

python3 -I - "$ROOT/scripts/lib" "$BASE" <<'PY'
import datetime, sys, uuid

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, Holds, credentials

api = Client(sys.argv[1])


def call(method, path, body=None, headers=None):
    """Any HTTP status >= 400 raises with the response body."""
    status, payload, _ = api.call(method, path, body, headers)
    return status, payload


status, health = call("GET", "/api/v1/health")
assert status == 200 and health["status"] == "ok", health

auth, _ = api.login(*credentials())
holds = Holds(api, auth)  # cancelled at exit unless this smoke cancels them itself

# Lists are paged (X-Next-Cursor): get_all reads every page; any one vehicle will do.
dealers = api.get_all("/api/v1/dealerships", auth)
services = api.get_all("/api/v1/service-types", auth)
_, vehicles = call("GET", "/api/v1/vehicles", headers=auth)
north = next(d for d in dealers if d["name"].startswith("Northside"))
river = next(d for d in dealers if d["name"].startswith("Riverside"))
brake = next(s for s in services if s["code"] == "BRAKE")
oil = next(s for s in services if s["code"] == "OIL")
vehicle = vehicles[0]

# The seed books its demo jobs today, or tomorrow when the stack started less than an hour before
# 09:00 (so they are never instantly no-shows): find that day on the Northside board.
def seeded_brakes(day):
    _, board = call("GET", f"/api/v1/schedule?dealershipId={north['id']}&date={day}", headers=auth)
    return [a for a in board["appointments"] if a["serviceTypeId"] == brake["id"] and a["status"] in (1, 2, 3)]

seed_day = north["today"]
if not seeded_brakes(seed_day):
    seed_day = (datetime.date.fromisoformat(seed_day) + datetime.timedelta(days=1)).isoformat()
assert seeded_brakes(seed_day), "seeded brake jobs not found today or tomorrow"

status, avail = call(
    "GET",
    f"/api/v1/availability?dealershipId={north['id']}&vehicleId={vehicle['id']}&serviceTypeId={brake['id']}&date={seed_day}",
    headers=auth,
)
assert status == 200
# The seed books both Northside brake technicians 10:00–11:30 that day, so every brake start that
# overlaps it (08:30–10:30) is blocked: by the technicians, as "past" once the day has moved on, or
# by this car's own seeded 09:00 oil job (this check must not depend on the time of day it runs).
seeded = [s for s in avail["slots"] if "08:30" <= s["start"][11:16] <= "10:30"]
assert seeded and all(
    (not s["available"]) and s.get("reason") in ("no_technician", "no_resources", "past", "vehicle_busy")
    for s in seeded
), avail

# Book on the first date with a free start, not "today": today runs out of slots in the afternoon.
_, nxt = call(
    "GET",
    f"/api/v1/availability/next?dealershipId={river['id']}&vehicleId={vehicle['id']}&serviceTypeId={oil['id']}&days=14&limit=1",
    headers=auth,
)
assert nxt["slots"], nxt
book_day = nxt["slots"][0]["date"]
status, river_avail = call(
    "GET",
    f"/api/v1/availability?dealershipId={river['id']}&vehicleId={vehicle['id']}&serviceTypeId={oil['id']}&date={book_day}",
    headers=auth,
)
free = next(s for s in river_avail["slots"] if s["available"])
key = str(uuid.uuid4())
payload = {
    "dealershipId": river["id"],
    "vehicleId": vehicle["id"],
    "serviceTypeId": oil["id"],
    "startAt": free["start"],
}
status, created = call("POST", "/api/v1/appointments", payload, {**auth, "Idempotency-Key": key})
holds.add(created)
assert status == 201, created
status, replay = call("POST", "/api/v1/appointments", payload, {**auth, "Idempotency-Key": key})
assert status == 200 and replay["id"] == created["id"], replay

status, schedule = call(
    "GET",
    f"/api/v1/schedule?dealershipId={river['id']}&date={book_day}",
    headers=auth,
)
assert any(a["id"] == created["id"] for a in schedule["appointments"]), schedule

status, cancelled = call("POST", f"/api/v1/appointments/{created['id']}/cancel", headers=auth)
assert status == 200 and cancelled["status"] == 5, cancelled
holds.released(created["id"])

# Session refresh: login sets the HttpOnly refresh cookie (uss_refresh); POST /auth/refresh with it
# and X-Requested-With (no Origin, no Bearer) rotates it and answers a fresh access token; logout
# revokes the session and clears the cookie. A dedicated customer account (registered on first
# use, its own cookie jar): logout signs that user out everywhere, so no other smoke, stress run or
# open dashboard is touched.
sess = Client(sys.argv[1], cookies=True)
email, password = "smoke-refresh@scheduler.local", "smoke-refresh-pass-1"
status, session, _ = sess.request("POST", "/api/v1/auth/login", {"email": email, "password": password})
if status == 401:
    status, session, _ = sess.call("POST", "/api/v1/auth/register", {"email": email, "password": password, "name": "Smoke Refresh"})
assert status in (200, 201) and all(session.get(k) for k in ("token", "expiresAt", "sessionExpiresAt")), (status, session)
first = sess.refresh_cookie()
assert first, "login set no uss_refresh cookie this client sends back (AUTH_COOKIE_SECURE=true needs https)"
status, renewed = sess.refresh()
assert status == 200 and renewed.get("token") and renewed.get("expiresAt"), (status, renewed)
assert sess.refresh_cookie() not in (None, first), "refresh did not rotate the refresh cookie"
bearer = {"Authorization": f"Bearer {renewed['token']}"}
_, me = call("GET", "/api/v1/auth/me", headers=bearer)
assert me.get("email") == email, me
status, _, _ = sess.request("POST", "/api/v1/auth/logout", None, bearer)
assert status == 204, status
assert sess.refresh_cookie() is None, "logout did not clear the refresh cookie"
status, _ = sess.refresh()
assert status == 401, f"refresh after logout -> {status}, want 401"
print("smoke ok")
PY
