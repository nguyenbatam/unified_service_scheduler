#!/usr/bin/env bash
# Prepares and checks the state the recorded walkthrough demo relies on, on a stack
# freshly started with ./scripts/up.sh. Adds the walk-in car the demo books for (Liam Carter, 2020
# Ford Focus; skipped when it exists) — the seeded cars all have a demo booking, so their grids show
# "car already booked" where the demo needs the workshop's own reasons — then reads the slot grids
# the script points at and prints PASS/FAIL for each. Writes nothing else.
# Env: BASE_URL (gateway, default http://127.0.0.1:8080).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${BASE_URL:-http://127.0.0.1:8080}"

python3 -I - "$ROOT/scripts/lib" "$BASE" <<'PY'
import datetime, sys
from zoneinfo import ZoneInfo

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client

api = Client(sys.argv[1])
NORTH = "11111111-1111-4111-8111-111111111111"
OIL = "51000000-0000-4000-8000-000000000001"
BRAKE = "51000000-0000-4000-8000-000000000002"
BMW = "e1000000-0000-4000-8000-000000000001"
VIN = "WF0FXXGCDF0000004"
LONDON = ZoneInfo("Europe/London")
failed = []


def check(ok, what):
    print(f"  {'PASS' if ok else 'FAIL'}  {what}")
    if not ok:
        failed.append(what)


staff, _ = api.login("dispatcher@scheduler.local", "dispatch123")

found = api.get_all(f"/api/v1/vehicles?q={VIN}", staff)
if found:
    ford = found[0]
    print(f"walk-in car exists: {ford['year']} {ford['make']} {ford['model']} ({ford['customerName']})")
else:
    _, liam, _ = api.call("POST", "/api/v1/customers", {"name": "Liam Carter", "phone": "07700 900444"}, staff)
    _, ford, _ = api.call(
        "POST", "/api/v1/vehicles",
        {"customerId": liam["id"], "vin": VIN, "make": "Ford", "model": "Focus", "year": 2020}, staff,
    )
    print("added walk-in car: 2020 Ford Focus (Liam Carter)")

now = datetime.datetime.now(LONDON)
today = now.date()
board = api.call("GET", f"/api/v1/schedule?dealershipId={NORTH}&date={today}", None, staff)[1]
seed_today = any(a["vehicleId"] == BMW for a in board["appointments"])
seed_day = today if seed_today else today + datetime.timedelta(days=1)
print(f"London now {now:%a %d %b %H:%M}; demo bookings of the seed are on {'TODAY' if seed_today else 'TOMORROW'} ({seed_day})")


def grid(service, day):
    q = f"dealershipId={NORTH}&vehicleId={ford['id']}&serviceTypeId={service}&date={day}"
    slots = api.call("GET", f"/api/v1/availability?{q}", None, staff)[1]["slots"]
    return {s["start"][11:16]: ("OK" if s["available"] else s.get("reason", "?")) for s in slots}


def show(g):
    return " | ".join(f"{k} {v}" for k, v in g.items())


print(f"\nWindow B — Ford + Brake Service, {seed_day} (whole duration, lunch):")
brake = grid(BRAKE, seed_day)
print("  " + show(brake))
check(brake.get("08:30") == "OK", "08:30 offered (08:30-10:00 ends as the 10:00 brake jobs start)")
check(brake.get("09:00") in ("no_resources", "no_technician", "no_bay"),
      f"09:00 refused although both brake technicians are free at 09:00 (got {brake.get('09:00')})")
check(brake.get("11:00") == "lunch_break", f"11:00 refused for lunch, 11:00-12:30 crosses 12:00 (got {brake.get('11:00')})")
check(brake.get("13:00") == "OK", "13:00 offered (turns into 'This car is already booked then' once window A books it)")

print(f"\nWindow A — Ford + Oil Change, {today} (the booking):")
oil = grid(OIL, today)
print("  " + show(oil))
check(oil.get("12:00") == "lunch_break", "12:00 refused for lunch")
if seed_today:
    check(oil.get("10:00") == "staging_full", f"10:00 refused: check-in area full, 2 arrivals already (got {oil.get('10:00')})")
check(oil.get("13:00") == "OK", "13:00 offered: the slot to book")
slot = datetime.datetime.combine(today, datetime.time(13, 0), LONDON)
check(now < slot - datetime.timedelta(minutes=10), "13:00 London is still ahead (book it before then)")

customer = Client(sys.argv[1])
cust, user = customer.login("customer@scheduler.local", "customer123")
cars = customer.get_all("/api/v1/vehicles", cust)
print("\nWindow C — customer:")
check([c["id"] for c in cars] == [BMW], f"{user['name']} sees only the BMW ({len(cars)} car(s))")
dealers = customer.get_all("/api/v1/dealerships", cust)
north = next(d for d in dealers if d["id"] == NORTH)
check(north["policy"]["bookingHorizonDays"] == 90, "Northside books online up to 90 days ahead")

print()
if failed:
    print(f"{len(failed)} check(s) failed: rerun ./scripts/up.sh, then this script, before recording.")
    sys.exit(1)
print("Ready to record.")
PY
