#!/usr/bin/env bash
# End-to-end check of the DMS operations features against a running stack (gateway):
# staff scope, first-available search, multi-line booking, reassign, audit history, in-place
# reschedule, technician roster, dealership policy, and the customer booking horizon.
# Usage: BASE_URL=http://127.0.0.1:8080 ./scripts/smoke_dms.sh
# Env: {ADMIN,DISPATCHER,CUSTOMER}_EMAIL / _PASSWORD (default: the seeded demo users).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${BASE_URL:-http://127.0.0.1:8080}"

python3 -I - "$ROOT/scripts/lib" "$BASE" <<'PY'
import datetime, os, random, sys, uuid

sys.path.insert(0, sys.argv.pop(1))
from codes import EVENT
from smoke_http import Client, Holds, credentials

api = Client(sys.argv[1])


def call(method, path, body=None, headers=None):
    """(status, body, headers) for any HTTP status: this smoke asserts refusals (403, 422) too."""
    return api.request(method, path, body, headers)


def login(email, password):
    return api.login(email, password)


def expect(status, want, body, what):
    assert status == want, f"{what}: status {status}, want {want}: {body}"


admin, _ = login(*credentials())
dispatcher, dispatcher_user = login(*credentials("DISPATCHER", "dispatcher@scheduler.local", "dispatch123"))
customer, _ = login(*credentials("CUSTOMER", "customer@scheduler.local", "customer123"))
holds = Holds(api, dispatcher)  # cancelled at exit unless this smoke cancels them itself

# Lists are paged (X-Next-Cursor): get_all reads every page; one page of vehicles is enough.
dealers = api.get_all("/api/v1/dealerships", admin)
services = api.get_all("/api/v1/service-types", admin)
_, vehicles, _ = call("GET", "/api/v1/vehicles", headers=admin)
north = next(d for d in dealers if d["name"].startswith("Northside"))
river = next(d for d in dealers if d["name"].startswith("Riverside"))
oil = next(s for s in services if s["code"] == "OIL")
brake = next(s for s in services if s["code"] == "BRAKE")
assert "policy" in north and "stagingWindowMinutes" in north, north

# Staff scope: the demo dispatcher works at Northside only.
assert dispatcher_user.get("dealershipId") == north["id"], dispatcher_user
status, body, _ = call("GET", f"/api/v1/schedule?dealershipId={river['id']}&date={river['today']}", headers=dispatcher)
expect(status, 403, body, "dispatcher on another dealership's board")

# First available for two job lines, on a random far-off day so reruns do not collide. The seed is
# printed (SMOKE_SEED reruns one), so a failure can be replayed on the same day and vehicle.
seed = int(os.environ.get("SMOKE_SEED") or random.SystemRandom().randrange(1 << 31))
print(f"smoke_dms: SMOKE_SEED={seed}")
rng = random.Random(seed)
today = datetime.date.fromisoformat(north["today"])
day = today + datetime.timedelta(days=rng.randint(120, 360))
vehicle = rng.choice(vehicles)
lines = f"{oil['id']},{brake['id']}"
status, nxt, _ = call(
    "GET",
    f"/api/v1/availability/next?dealershipId={north['id']}&vehicleId={vehicle['id']}"
    f"&serviceTypeIds={lines}&from={day.isoformat()}&limit=2",
    headers=dispatcher,
)
expect(status, 200, nxt, "availability/next")
assert nxt["durationMinutes"] == oil["durationMinutes"] + brake["durationMinutes"] and nxt["slots"], nxt
slot = nxt["slots"][0]

status, appt, hdrs = call("POST", "/api/v1/appointments", {
    "dealershipId": north["id"], "vehicleId": vehicle["id"],
    "serviceTypeIds": [oil["id"], brake["id"]], "startAt": slot["start"],
}, {**dispatcher, "Idempotency-Key": str(uuid.uuid4())})
holds.add(appt)
expect(status, 201, appt, "multi-line booking")
assert len(appt["lines"]) == 2 and appt["durationMinutes"] == nxt["durationMinutes"], appt
assert hdrs.get("ETag") == f'"{appt["version"]}"', hdrs

# Reassign to another general bay that is free (the board lists bays and technicians).
status, board, _ = call("GET", f"/api/v1/schedule?dealershipId={north['id']}&date={slot['date']}", headers=dispatcher)
expect(status, 200, board, "board")
assert board["technicians"], board
busy = {a["serviceBayId"] for a in board["appointments"]
        if a["status"] in (1, 2, 3) and a["startAt"] < appt["endAt"] and a["endAt"] > appt["startAt"]}
target = next((b for b in board["bays"] if b["bayType"] == appt["requiredBayType"] and b["id"] not in busy), None)
path = f"/api/v1/appointments/{appt['id']}"
# A far-off day has Northside's other general bay free; if it does not, the check cannot run, which
# is a failure, not a silent skip.
assert target is not None, f"no free {appt['requiredBayType']} bay to reassign to on {slot['date']}: {board['appointments']}"
status, moved, _ = call("POST", path + "/reassign", {"serviceBayId": target["id"]},
                        {**dispatcher, "If-Match": f'"{appt["version"]}"'})
expect(status, 200, moved, "reassign")
assert moved["serviceBayId"] == target["id"] and moved["version"] == appt["version"] + 1, moved
appt = moved
expected_events = [EVENT["APPOINTMENT_CREATED"], EVENT["APPOINTMENT_REASSIGNED"]]
status, body, _ = call("POST", path + "/reassign", {"serviceBayId": appt["serviceBayId"]}, customer)
expect(status, 403, body, "customer reassign")

# In-place reschedule one week later: same id, bumped version.
later = slot["start"].replace(slot["date"], (datetime.date.fromisoformat(slot["date"]) + datetime.timedelta(days=7)).isoformat(), 1)
status, moved, _ = call("POST", path + "/reschedule", {"startAt": later}, {**dispatcher, "If-Match": f'"{appt["version"]}"'})
if status == 200:
    assert moved["id"] == appt["id"] and moved["version"] == appt["version"] + 1 and len(moved["lines"]) == 2, moved
    appt = moved
    expected_events.append(EVENT["APPOINTMENT_RESCHEDULED"])
else:
    expect(status, 422, moved, "reschedule (only a full or closed target may refuse)")

# The audit trail has exactly one row per change, in order, versions 1..n, ending at the
# appointment's current version.
history = api.get_all(path + "/history", dispatcher)
assert [h["eventType"] for h in history] == expected_events, (expected_events, history)
assert [h["version"] for h in history] == list(range(1, len(history) + 1)), history
assert history[-1]["version"] == appt["version"], (history, appt)

# Roster: read, then rewrite the same pattern (If-Match round trip).
techs = api.get_all(f"/api/v1/admin/technicians?dealershipId={river['id']}", admin)
eve = next(t for t in techs if t["name"].startswith("Eve"))
status, roster, _ = call("GET", f"/api/v1/admin/technicians/{eve['id']}/roster", headers=admin)
expect(status, 200, roster, "roster")
status, roster2, _ = call("PUT", f"/api/v1/admin/technicians/{eve['id']}/shifts", {"shifts": roster["shifts"]},
                          {**admin, "If-Match": f'"{roster["version"]}"'})
expect(status, 200, roster2, "replace shifts")
assert roster2["version"] == roster["version"] + 1, roster2
status, body, _ = call("GET", f"/api/v1/admin/technicians/{eve['id']}/roster", headers=dispatcher)
expect(status, 403, body, "dispatcher roster")

# Policy round trip, then the customer booking horizon.
status, dealer, _ = call("PATCH", f"/api/v1/admin/dealerships/{river['id']}",
                         {"policy": {"bookingHorizonDays": river["policy"]["bookingHorizonDays"]}},
                         {**admin, "If-Match": f'"{river["version"]}"'})
expect(status, 200, dealer, "policy patch")
horizon = dealer["policy"]["bookingHorizonDays"]
_, mine, _ = call("GET", "/api/v1/vehicles", headers=customer)
# The first open day past the horizon (a closed day lists no slots and would prove nothing): it
# must list slots, every one refused as beyond_horizon (all() of no slots would pass vacuously).
for extra in range(5, 12):
    far = (datetime.date.fromisoformat(river["today"]) + datetime.timedelta(days=horizon + extra)).isoformat()
    status, avail, _ = call(
        "GET",
        f"/api/v1/availability?dealershipId={river['id']}&vehicleId={mine[0]['id']}&serviceTypeId={oil['id']}&date={far}",
        headers=customer,
    )
    expect(status, 200, avail, "customer availability past horizon")
    if not avail.get("closed"):
        break
assert not avail.get("closed"), f"no open day in the week after horizon+5: {avail}"
assert avail["slots"], f"availability past the horizon on {far} lists no slots: {avail}"
assert all(s["reason"] == "beyond_horizon" and not s["available"] for s in avail["slots"]), avail

# Clean up so reruns stay independent.
status, body, _ = call("POST", path + "/cancel", headers=dispatcher)
expect(status, 200, body, "cancel")
holds.released(appt["id"])
print(f"smoke_dms ok: multi-line {appt['id']} lines=2 history={len(history)} roster v{roster2['version']} horizon={horizon}d")
print(f"APPOINTMENT_ID={appt['id']}")
PY
