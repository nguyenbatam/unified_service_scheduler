#!/usr/bin/env bash
# Book on backend-1; assert APPOINTMENT_CREATED arrives on backend-2 WebSocket.
# Env: BACKEND_A_URL, BACKEND_B_HOST, BACKEND_B_PORT, ADMIN_EMAIL / ADMIN_PASSWORD.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
A="${BACKEND_A_URL:-http://127.0.0.1:18081}"
B_HOST="${BACKEND_B_HOST:-127.0.0.1}"
B_PORT="${BACKEND_B_PORT:-18082}"

python3 -I - "$ROOT/scripts/lib" "$A" "$B_HOST" "$B_PORT" <<'PY'
import json, sys, time, uuid

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, Holds, credentials
from ws_client import WSClient
from codes import EVENT

a = sys.argv[1].rstrip("/")
b_host, b_port = sys.argv[2], int(sys.argv[3])
api = Client(a)


def http(method, url, body=None, headers=None):
    """Any HTTP status >= 400 raises with the response body."""
    status, payload, _ = api.call(method, url, body, headers)
    return status, payload


auth, _ = api.login(*credentials())
# Every booking this smoke makes is cancelled, pass or fail, so reruns stay repeatable.
holds = Holds(api, auth)
# Browsers open the socket with a short-lived ticket; the access token never goes in a URL.
_, ticket = http("POST", f"{a}/api/v1/auth/ws-ticket", headers=auth)
assert ticket.get("ticket"), ticket

ws = WSClient(b_host, b_port, path=f"/api/v1/ws/dispatch?ticket={ticket['ticket']}")
ev = json.loads(ws.recv_text(time.time() + 5))
assert ev.get("type") == EVENT["CONNECTED"], ev

# Lists are paged (X-Next-Cursor): get_all reads every page; any one vehicle will do.
dealers = api.get_all("/api/v1/dealerships", auth)
services = api.get_all("/api/v1/service-types", auth)
_, vehicles = http("GET", f"{a}/api/v1/vehicles", headers=auth)
river = next(d for d in dealers if d["name"].startswith("Riverside"))
oil = next(s for s in services if s["code"] == "OIL")
vehicle = vehicles[0]
# First date with free starts (today runs out of slots in the afternoon).
_, nxt = http(
    "GET",
    f"{a}/api/v1/availability/next?dealershipId={river['id']}&vehicleId={vehicle['id']}"
    f"&serviceTypeId={oil['id']}&days=14&limit=1",
    headers=auth,
)
assert nxt["slots"], nxt
_, avail = http(
    "GET",
    f"{a}/api/v1/availability?dealershipId={river['id']}&vehicleId={vehicle['id']}"
    f"&serviceTypeId={oil['id']}&date={nxt['slots'][0]['date']}",
    headers=auth,
)
free_slots = [s for s in avail["slots"] if s["available"]]
assert free_slots, "no free Riverside oil slots for smoke_ws"

# backend-2's Kafka consumer may still be joining after a (re)start and starts from the latest
# offset, so an event published before it joined never reaches it. Instead of sleeping a fixed
# time, book and wait for the event; if it does not come, book the next free slot, until a deadline.
seen = False
created = None
deadline = time.time() + 60
for slot in free_slots:
    if time.time() >= deadline:
        break
    key = str(uuid.uuid4())
    status, created = http(
        "POST",
        f"{a}/api/v1/appointments",
        {
            "dealershipId": river["id"],
            "vehicleId": vehicle["id"],
            "serviceTypeId": oil["id"],
            "startAt": slot["start"],
        },
        {**auth, "Idempotency-Key": key},
    )
    holds.add(created)
    assert status == 201, created
    wait_until = min(deadline, time.time() + 8)
    while time.time() < wait_until:
        try:
            msg = ws.recv_text(min(wait_until, time.time() + 2))
        except TimeoutError:
            continue
        ev = json.loads(msg)
        if ev.get("type") == EVENT["APPOINTMENT_CREATED"] and (ev.get("payload") or {}).get("id") == created["id"]:
            seen = True
            break
    if seen:
        break

ws.close()
for appt_id in list(holds.ids):
    status, cancelled = http("POST", f"{a}/api/v1/appointments/{appt_id}/cancel", headers=auth)
    assert status == 200 and cancelled["status"] == 5, cancelled
    holds.released(appt_id)
assert seen, "APPOINTMENT_CREATED not received on backend-2 after book on backend-1"
print("smoke_ws ok: cross-instance APPOINTMENT_CREATED via Kafka fan-out")
PY
