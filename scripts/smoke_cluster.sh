#!/usr/bin/env bash
# Proves two backend instances share Postgres (+ Redis) state: book on A, replay and read on B,
# cancel on B and read on A. Every response names the instance that served it (X-Instance-ID
# header), so each step asserts which backend actually answered.
# Env: BACKEND_A_URL, BACKEND_B_URL, GATEWAY_URL, ADMIN_EMAIL / ADMIN_PASSWORD.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
A="${BACKEND_A_URL:-http://127.0.0.1:18081}"
B="${BACKEND_B_URL:-http://127.0.0.1:18082}"
G="${GATEWAY_URL:-http://127.0.0.1:8080}"

python3 -I - "$ROOT/scripts/lib" "$A" "$B" "$G" <<'PY'
import sys, uuid

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, Holds, credentials

a, b, g = [x.rstrip("/") for x in sys.argv[1:4]]


def call(base, method, path, body=None, headers=None):
    """(status, body, headers); any HTTP status >= 400 raises with the response body."""
    return Client(base).call(method, path, body, headers)


def served_by(headers, what):
    instance = headers.get("X-Instance-ID")
    assert instance, f"{what}: response has no X-Instance-ID header"
    return instance

for base in (a, b, g):
    status, health, _ = call(base, "GET", "/api/v1/health")
    assert status == 200 and health["status"] == "ok", (base, health)

_, _, ha = call(a, "GET", "/api/v1/health")
_, _, hb = call(b, "GET", "/api/v1/health")
id_a, id_b = served_by(ha, "health on A"), served_by(hb, "health on B")
assert id_a != id_b, f"A and B are the same instance ({id_a}): nothing multi-instance is being tested"

auth, _ = Client(a).login(*credentials())
holds = Holds(Client(a), auth)  # cancelled at exit unless this smoke cancels them itself

# Lists are paged (X-Next-Cursor): get_all reads every page; any one vehicle will do.
dealers = Client(a).get_all("/api/v1/dealerships", auth)
services = Client(a).get_all("/api/v1/service-types", auth)
_, vehicles, _ = call(a, "GET", "/api/v1/vehicles", headers=auth)
river = next(d for d in dealers if d["name"].startswith("Riverside"))
oil = next(s for s in services if s["code"] == "OIL")
vehicle = vehicles[0]

# First date with a free start (today runs out of slots in the afternoon).
_, nxt, _ = call(
    a,
    "GET",
    f"/api/v1/availability/next?dealershipId={river['id']}&vehicleId={vehicle['id']}&serviceTypeId={oil['id']}&days=14&limit=1",
    headers=auth,
)
assert nxt["slots"], nxt
free = nxt["slots"][0]
day = free["date"]
key = str(uuid.uuid4())
payload = {
    "dealershipId": river["id"],
    "vehicleId": vehicle["id"],
    "serviceTypeId": oil["id"],
    "startAt": free["start"],
}

# Book on A.
status, created, h = call(a, "POST", "/api/v1/appointments", payload, {**auth, "Idempotency-Key": key})
holds.add(created)
assert status == 201, created
assert served_by(h, "book") == id_a, f"booking served by {h.get('X-Instance-ID')}, want {id_a}"

# The same key on B replays A's booking (shared Postgres/Redis), it does not book again.
status, replay, h = call(b, "POST", "/api/v1/appointments", payload, {**auth, "Idempotency-Key": key})
assert status == 200 and replay["id"] == created["id"], replay
assert served_by(h, "replay") == id_b, f"replay served by {h.get('X-Instance-ID')}, want {id_b}"

# B's day board shows A's booking.
status, schedule, h = call(b, "GET", f"/api/v1/schedule?dealershipId={river['id']}&date={day}", headers=auth)
assert served_by(h, "schedule") == id_b
assert any(x["id"] == created["id"] for x in schedule["appointments"]), schedule

# Cancel on B; A reads the cancellation straight away (Postgres is the only state, no cache to wait for).
status, cancelled, h = call(b, "POST", f"/api/v1/appointments/{created['id']}/cancel", headers=auth)
assert status == 200 and cancelled["status"] == 5, cancelled
holds.released(created["id"])
assert served_by(h, "cancel") == id_b
status, again, h = call(a, "GET", f"/api/v1/appointments/{created['id']}", headers=auth)
assert served_by(h, "read back") == id_a
assert again["status"] == 5 and again["version"] == cancelled["version"], again

print(f"smoke_cluster ok: booked on {id_a}, replayed + read + cancelled on {id_b}, read back on {id_a}")
PY
