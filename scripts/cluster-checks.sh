#!/usr/bin/env bash
# End-to-end checks against the running full stack (2 backends behind the gateway, Prometheus):
# the one implementation shared by ./scripts/check-all.sh (step 7) and the Cluster workflow,
# so the two cannot drift. Start the stack first (docker compose up -d --wait).
# Usage: ./scripts/cluster-checks.sh [step...]    (no step = all of them, in this order)
#   stack    gateway reaches the backends, each backend's identity, /metrics, Prometheus scraping
#            both backends (polls `up` until 1, fails after PROMETHEUS_WAIT seconds, default 60)
#   smoke    gateway smoke (scripts/smoke.sh)
#   cluster  multi-instance sync: book on A, replay/read/cancel on B (scripts/smoke_cluster.sh)
#   ws       WebSocket fan-out: book on A, event on B (scripts/smoke_ws.sh)
#   dms      DMS operations (scripts/smoke_dms.sh), then that appointment's customer notifications
#            (scripts/check_notifications.sh; only rows created since RUN_START count)
#   stress   concurrent booking stress + SQL integrity (scripts/stress.sh; CONCURRENCY default 300,
#            SLOTS default 3, …), every requirement of the matrix and its essential checks required
#            to PASS (STRESS_REQUIRE, default all — Q5/beats first fit, Q5/repair and Q5/fit gain included, from the
#            quality day; empty opts out). The chaos runs pick their phases and name what they
#            require, never all: none of them plays the quality day.
#   chaos    Redis stopped (stress river hammer + customers + the cross-instance idempotency race
#            pass on Postgres alone), Kafka paused (outbox queues, drains after unpause, and the queued events
#            reach WebSocket listeners on both backends), backend-1 killed mid-hammer and restarted
#            (the requests it cut off replayed with their keys — at least one required —, exact
#            capacity, integrity, outbox drained)
# Env: RUN_START (timestamptz; default: the database clock when this script starts).
#      STRESS_SUMMARY_MD: every stress run appends its compact requirements matrix there (Markdown).
#      CHAOS_REPORT_DIR: each chaos stress run writes its requirements matrix JSON there.
#      CHAOS_RATE / CHAOS_CONCURRENCY / CHAOS_SLOTS / CHAOS_KILL_AFTER: the backend-kill hammer
#      (open loop, default 100 bookings/s, 300 requests × 3 slots per phase, kill 3 s into it).
# Every login-using step first clears the per-IP login limit (20 / 5 min) the smokes would trip.
# Diagnostics (compose ps/logs) are left to the caller.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"

RED=$'\033[31m'
GRN=$'\033[32m'
YLW=$'\033[33m'
NC=$'\033[0m'
step() { echo; echo "${YLW}==>${NC} $*"; }
ok() { echo "${GRN}OK${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*" >&2; exit 1; }

GATEWAY="http://127.0.0.1:${GATEWAY_PORT}"
BACKEND_A="http://127.0.0.1:${BACKEND_1_HOST_PORT}"
BACKEND_B="http://127.0.0.1:${BACKEND_2_HOST_PORT}"
PROMETHEUS="http://127.0.0.1:${PROMETHEUS_PORT}"

# redis_del deletes the keys matching each pattern (REDIS_PASSWORD, when the stack sets one, is
# handed to redis-cli as REDISCLI_AUTH).
redis_del() {
  local auth=()
  [[ -z "${REDIS_PASSWORD:-}" ]] || auth=(-e "REDISCLI_AUTH=${REDIS_PASSWORD}")
  local p
  for p in "$@"; do
    # The scan's own failure (auth, connection) must not hide behind xargs' exit status.
    docker compose exec -T ${auth[@]+"${auth[@]}"} redis sh -c "keys=\$(redis-cli --scan --pattern '$p') || exit 1; [ -z \"\$keys\" ] || echo \"\$keys\" | xargs redis-cli del" >/dev/null \
      || return 1
  done
}

reset_login_limits() {
  redis_del 'ratelimit:login:*' || fail "reset login rate limits"
}

# Value of Prometheus' `up` for one scrape target ("" while it has no sample yet).
prom_up() {
  curl -sf --max-time 5 -G "${PROMETHEUS}/api/v1/query" --data-urlencode "query=up{instance=\"$1\"}" \
    | python3 -I -c '
import json, sys
r = json.load(sys.stdin).get("data", {}).get("result", [])
print(r[0]["value"][1] if r else "")' 2>/dev/null || true
}

check_stack() {
  step "stack: gateway, backend identities, metrics, Prometheus"
  # The gateway re-resolves the backends every 5 s (resolver valid=5s): right after `compose up`
  # recreated them it may still hold the old addresses for a moment, so allow it 30 s.
  local tries=0
  until curl -sf --max-time 5 "${GATEWAY}/api/v1/health" >/dev/null; do
    ((tries < 30)) || fail "gateway does not reach the backends (30 s)"
    tries=$((tries + 1))
    sleep 1
  done
  # Each backend names itself in the X-Instance-ID response header.
  # Captured first, then matched with a here-string: under pipefail, `curl … | grep -q` fails when
  # grep exits at its first match and curl dies writing the rest (SIGPIPE).
  local ha hb
  ha=$(curl -sfi --max-time 10 "${BACKEND_A}/api/v1/health") || fail "backend-1 health"
  hb=$(curl -sfi --max-time 10 "${BACKEND_B}/api/v1/health") || fail "backend-2 health"
  grep -qix "x-instance-id: ${BACKEND_1_INSTANCE_ID}" <<<"${ha//$'\r'/}" || fail "backend-1 identity: $ha"
  grep -qix "x-instance-id: ${BACKEND_2_INSTANCE_ID}" <<<"${hb//$'\r'/}" || fail "backend-2 identity: $hb"
  local port metrics
  for port in "$BACKEND_1_HOST_PORT" "$BACKEND_2_HOST_PORT"; do
    metrics=$(curl -sf --max-time 10 "http://127.0.0.1:${port}/metrics") || fail "backend on :${port} /metrics"
    grep -q scheduler_http_requests_total <<<"$metrics" \
      || fail "backend on :${port} /metrics has no scheduler_http_requests_total"
  done
  # Prometheus answers /-/ready before its first scrape of freshly started backends: wait until
  # both targets report up == 1 (a down or never-scraped target fails at the deadline).
  local wait="${PROMETHEUS_WAIT:-60}" target value deadline
  deadline=$((SECONDS + wait))
  for target in backend-1:8080 backend-2:8080; do
    while :; do
      value=$(prom_up "$target")
      [[ "$value" == 1 ]] && break
      ((SECONDS < deadline)) || fail "prometheus: up{instance=\"${target}\"} is '${value}' after ${wait}s, want 1"
      sleep 1
    done
  done
  ok "cluster + metrics stack healthy (Prometheus scrapes both backends)"
}

check_smoke() {
  step "smoke: gateway"
  reset_login_limits
  BASE_URL="$GATEWAY" bash scripts/smoke.sh || fail "gateway smoke"
  ok "gateway smoke"
}

check_cluster() {
  step "cluster: multi-instance sync (book A / read B)"
  reset_login_limits
  BACKEND_A_URL="$BACKEND_A" BACKEND_B_URL="$BACKEND_B" GATEWAY_URL="$GATEWAY" \
    bash scripts/smoke_cluster.sh || fail "multi-instance sync"
  ok "multi-instance sync"
}

check_ws() {
  step "ws: WebSocket fan-out (book A -> event on B)"
  reset_login_limits
  BACKEND_A_URL="$BACKEND_A" BACKEND_B_HOST=127.0.0.1 BACKEND_B_PORT="$BACKEND_2_HOST_PORT" \
    bash scripts/smoke_ws.sh || fail "websocket fan-out"
  ok "websocket e2e"
}

check_dms() {
  step "dms: DMS operations (scope, job lines, reassign, history, roster, policy) + notifications"
  reset_login_limits
  local out appt
  out=$(BASE_URL="$GATEWAY" bash scripts/smoke_dms.sh) || { echo "$out"; fail "dms operations"; }
  echo "$out" | grep -v '^APPOINTMENT_ID=' || true
  appt=$(echo "$out" | sed -n 's/^APPOINTMENT_ID=//p')
  [[ -n "$appt" ]] || fail "smoke_dms did not report its appointment"
  ok "dms operations"
  # smoke_dms booked it (confirmation) and cancelled it (cancellation) a moment ago.
  bash scripts/check_notifications.sh "$appt" "$RUN_START" || fail "notifications"
  ok "notifications (Kafka shared group -> queue -> sender)"
}

# Clears this machine's per-IP (and per-user) write and availability buckets: every customer of a
# stress run comes from this one IP, which a production limit sized for one real client would
# throttle; a run right after another must not start with that budget already spent. The limits
# themselves are unchanged (the stress customer burst still proves the per-user write throttle).
reset_customer_limits() {
  redis_del 'ratelimit:write:*' 'ratelimit:availability:*' || fail "reset customer rate limits"
}

check_stress() {
  step "stress: concurrent booking + data integrity"
  reset_login_limits
  reset_customer_limits
  # 300 requests × 3 slots per hammer phase (the Cluster workflow's defaults too): enough contention
  # to prove exact capacity while keeping check-all within minutes; stress.sh alone defaults to 1000 × 5.
  # Every row of the matrix and its essential checks must PASS (the binary's "all"; a SKIP fails);
  # an explicitly empty STRESS_REQUIRE opts out.
  BASE_URL="$GATEWAY" BACKEND_A_URL="$BACKEND_A" BACKEND_B_URL="$BACKEND_B" MIXED_OPS="${MIXED_OPS:-30}" \
    CONCURRENCY="${CONCURRENCY:-300}" SLOTS="${SLOTS:-3}" \
    STRESS_REQUIRE="${STRESS_REQUIRE-all}" bash scripts/stress.sh || fail "stress"
  ok "stress"
}

# Degraded dependencies, then recovery:
#   - Redis stopped: bookings still get definite answers (idempotency falls back to Postgres, the
#     shared rate limiter fails open) — the stress river hammer + customers phases and the
#     cross-instance idempotency race (one key raced on both backends at once) pass on Postgres alone;
#   - Kafka paused: bookings still commit, their outbox events queue (not published); once Kafka is
#     back the backlog drains to zero without an operator, and the events reach the WebSocket
#     listeners of both backends that were connected all along;
#   - backend-1 killed (SIGKILL) mid-hammer (the hammer is sent straight to it), then started
#     again: clients resend with the same Idempotency-Key; once both backends are back the stress
#     client replays every request the kill cut off (at least one is required: unresolved ones must
#     now be definite, ones answered on a retry must replay consistently) and a sample of the
#     created ones (200, same appointment), then proves exact river capacity, the refusal oracle,
#     SQL integrity, and the outbox drained (the dead instance's claimed rows are reclaimed).
chaos_ids=""
chaos_tmp=""
chaos_ws_pid=""
chaos_stress_pid=""
# chaos_restore puts the stack back however check_chaos ends (failure, Ctrl-C, CI cancel): a
# background stress run stopped (SIGTERM: it cancels its own bookings), the WS listener stopped,
# Redis and backend-1 started, Kafka unpaused, the chaos bookings cancelled, the temp files removed.
chaos_restore() {
  if [[ -n "$chaos_stress_pid" ]]; then
    kill "$chaos_stress_pid" 2>/dev/null || true
    wait "$chaos_stress_pid" 2>/dev/null || true
    chaos_stress_pid=""
  fi
  if [[ -n "$chaos_ws_pid" ]]; then
    kill "$chaos_ws_pid" 2>/dev/null || true
    wait "$chaos_ws_pid" 2>/dev/null || true
    chaos_ws_pid=""
  fi
  docker compose start redis >/dev/null 2>&1 || true
  docker compose unpause kafka >/dev/null 2>&1 || true
  docker compose start backend-1 >/dev/null 2>&1 || true
  if [[ -n "$chaos_ids" ]]; then
    IDS="$chaos_ids" BASE="$GATEWAY" python3 -I - "$ROOT/scripts/lib" <<'PY' || echo "warn: could not cancel the chaos bookings $chaos_ids" >&2
import os, sys

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, credentials

api = Client(os.environ["BASE"])
auth, _ = api.login(*credentials())
for appt in os.environ["IDS"].split(","):
    api.call("POST", f"/api/v1/appointments/{appt}/cancel", headers=auth)
PY
    chaos_ids=""
  fi
  if [[ -n "$chaos_tmp" ]]; then
    rm -rf "$chaos_tmp"
    chaos_tmp=""
  fi
}

# chaos_report NAME prints where a chaos stress run writes its matrix JSON ("" without
# CHAOS_REPORT_DIR: no report).
chaos_report() {
  [[ -n "${CHAOS_REPORT_DIR:-}" ]] || return 0
  mkdir -p "$CHAOS_REPORT_DIR"
  echo "${CHAOS_REPORT_DIR}/chaos-$1.json"
}

# wait_healthy URL SECONDS polls URL/api/v1/health until it answers 200 (each probe bounded to 5 s,
# so a backend that accepts but never answers cannot stall the loop past its deadline).
wait_healthy() {
  local waited=0
  until curl -sf --max-time 5 "$1/api/v1/health" >/dev/null; do
    ((waited < $2)) || return 1
    sleep 1
    waited=$((waited + 1))
  done
}

chaos_redis() {
  step "chaos: Redis down — bookings keep working on Postgres alone"
  reset_login_limits
  docker compose stop redis >/dev/null || fail "stop redis"
  local rc=0
  # A small river hammer with If-Match mutations too, so the WebSocket order check has changed
  # appointments to judge without Redis. No truth phase: the full stress run already plays it with
  # Redis up, and its Redis-down evidence is covered here — R2a by the SQL overlap checks over the
  # run, R2b by the river hammer's and the customers' refusal oracle (and the customers turned
  # away), cross-instance reads by the hammer's board/read-back on both backends.
  BASE_URL="$GATEWAY" BACKEND_A_URL="$BACKEND_A" BACKEND_B_URL="$BACKEND_B" PHASES=river,customers \
    CONCURRENCY=100 SLOTS=3 MIXED_OPS=12 WS_CLIENTS=2 IDEM_RACERS=16 CUSTOMER_REQUESTS=0 \
    STRESS_REQUIRE="R1,R2a,R2b,R3,Q1,Q4,Q4/ws,Q4/ws order,Q7" REPORT_JSON="$(chaos_report redis-down)" bash scripts/stress.sh || rc=$?
  docker compose start redis >/dev/null || fail "start redis"
  docker compose up -d --wait --wait-timeout 120 redis >/dev/null || fail "redis not healthy again within 120 s"
  [[ $rc -eq 0 ]] || fail "stress with Redis down"
  ok "Redis down: river hammer with If-Match mutations, customers phase and the cross-instance idempotency race passed"
}

chaos_kafka() {
  step "chaos: Kafka paused — the outbox queues, then drains on its own and the events reach WebSocket listeners"
  reset_login_limits
  local ws_log="$chaos_tmp/ws.log" ids_file="$chaos_tmp/ids"
  # One listener per backend, connected (server CONNECTED frame) before the pause and kept open
  # through it; it prints READY, then "A <id>" / "B <id>" for every APPOINTMENT_CREATED it gets.
  BASE_A="$BACKEND_A" BASE_B="$BACKEND_B" python3 -I -u - "$ROOT/scripts/lib" >"$ws_log" 2>&1 <<'PY' &
import json, os, sys, time
from urllib.parse import urlsplit

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, credentials
from ws_client import WSClient
from codes import EVENT

sockets = {}
for name, base in (("A", os.environ["BASE_A"]), ("B", os.environ["BASE_B"])):
    api = Client(base)
    auth, _ = api.login(*credentials())
    _, ticket, _ = api.call("POST", "/api/v1/auth/ws-ticket", headers=auth)
    u = urlsplit(base)
    ws = WSClient(u.hostname, u.port, path=f"/api/v1/ws/dispatch?ticket={ticket['ticket']}")
    ev = json.loads(ws.recv_text(time.time() + 10))
    assert ev.get("type") == EVENT["CONNECTED"], ev
    sockets[name] = ws
print("READY")
deadline = time.time() + float(os.environ.get("WS_LISTEN_SECONDS", "400"))
while time.time() < deadline:
    for name, ws in sockets.items():
        try:
            msg = ws.recv_text(time.time() + 0.2)
        except TimeoutError:
            continue
        ev = json.loads(msg)
        if ev.get("type") == EVENT["APPOINTMENT_CREATED"]:
            print(name, (ev.get("payload") or {}).get("id"))
PY
  chaos_ws_pid=$!
  local waited=0
  until grep -q '^READY$' "$ws_log"; do
    kill -0 "$chaos_ws_pid" 2>/dev/null || { cat "$ws_log"; fail "WS listener exited before connecting"; }
    ((waited < 30)) || { cat "$ws_log"; fail "WS listeners not connected within 30 s"; }
    sleep 1
    waited=$((waited + 1))
  done
  docker compose pause kafka >/dev/null || fail "pause kafka"
  local ids
  if ! ids=$(IDS_FILE="$ids_file" BASE="$GATEWAY" python3 -I - "$ROOT/scripts/lib" <<'PY'
import datetime, os, sys, uuid

sys.path.insert(0, sys.argv.pop(1))
from smoke_http import Client, credentials

api = Client(os.environ["BASE"])
auth, _ = api.login(*credentials())
dealers = api.get_all("/api/v1/dealerships", auth)  # every page (lists are paged)
services = api.get_all("/api/v1/service-types", auth)
_, vehicles, _ = api.call("GET", "/api/v1/vehicles?limit=1", headers=auth)
river = next(d for d in dealers if d["name"].startswith("Riverside"))
oil = next(s for s in services if s["code"] == "OIL")
ids = []
for k in range(3):  # far-off days: nothing else books there
    day = datetime.date.fromisoformat(river["today"]) + datetime.timedelta(days=200 + k)
    _, nxt, _ = api.call("GET", f"/api/v1/availability/next?dealershipId={river['id']}&vehicleId={vehicles[0]['id']}"
                                f"&serviceTypeId={oil['id']}&from={day.isoformat()}&limit=1", headers=auth)
    _, appt, _ = api.call("POST", "/api/v1/appointments", {
        "dealershipId": river["id"], "vehicleId": vehicles[0]["id"], "serviceTypeId": oil["id"],
        "startAt": nxt["slots"][0]["start"]}, {**auth, "Idempotency-Key": str(uuid.uuid4())})
    ids.append(appt["id"])
    with open(os.environ["IDS_FILE"], "w") as f:  # each id as soon as it is booked, for chaos_restore
        f.write(",".join(ids))
print(",".join(ids))
PY
  ); then
    chaos_ids=$(cat "$ids_file" 2>/dev/null || true)
    fail "booking while Kafka is paused"
  fi
  chaos_ids="$ids"
  outbox_left() {
    docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc \
      "SELECT count(*) FROM outbox_events WHERE aggregate_id = ANY(string_to_array('$ids', ',')::uuid[]) AND status <> $(python3 "$ROOT/scripts/lib/codes.py" outbox_status.published)"
  }
  # Checked at once: Kafka was paused before the bookings, so nothing can have been published yet.
  local left
  left=$(outbox_left) || fail "read the outbox"
  docker compose unpause kafka >/dev/null || fail "unpause kafka"
  [[ "$left" -gt 0 ]] || fail "bookings made while Kafka was paused were already published ($left pending): the pause did not take"
  waited=0
  while :; do
    left=$(outbox_left) || fail "read the outbox"
    [[ "$left" -gt 0 ]] || break
    ((waited < 90)) || fail "outbox still holds $left event(s) 90s after Kafka came back"
    sleep 1
    waited=$((waited + 1))
  done
  ok "Kafka paused: 3 bookings committed, their events queued, then all published ${waited}s after unpause"
  # Published is not delivered: each backend's fan-out consumer must resume after the pause and push
  # every queued event to the socket that stayed connected on it.
  local id backend missing ws_waited=0
  while :; do
    missing=""
    for id in ${ids//,/ }; do
      for backend in A B; do
        grep -qx "$backend $id" "$ws_log" || missing+=" $backend:$id"
      done
    done
    [[ -n "$missing" ]] || break
    if ((ws_waited >= 90)) || ! kill -0 "$chaos_ws_pid" 2>/dev/null; then
      cat "$ws_log"
      fail "after Kafka came back, WebSocket listeners never got APPOINTMENT_CREATED for:${missing}"
    fi
    sleep 1
    ws_waited=$((ws_waited + 1))
  done
  kill "$chaos_ws_pid" 2>/dev/null || true
  wait "$chaos_ws_pid" 2>/dev/null || true
  chaos_ws_pid=""
  ok "Kafka paused: every queued event reached the WebSocket listeners on both backends ${ws_waited}s after the drain"
  # Its healthcheck failed while paused; `compose up --wait` gives up on an unhealthy container at
  # once, so poll until a probe (one every 30 s) passes again and leave the stack healthy.
  local kafka health waited_h=0
  kafka=$(docker compose ps -q kafka) || fail "kafka container"
  until health=$(docker inspect -f '{{.State.Health.Status}}' "$kafka") && [[ "$health" == healthy ]]; do
    ((waited_h < 150)) || fail "kafka not healthy again after 150 s (health: ${health:-unknown})"
    sleep 2
    waited_h=$((waited_h + 2))
  done
}

chaos_backend_kill() {
  step "chaos: backend-1 killed mid-hammer — retries replay, nothing is lost or doubled"
  reset_login_limits
  local log="$chaos_tmp/kill.log"
  # Open loop, so the hammer lasts long enough to be hit (300 × 3 requests at 100/s ≈ 9 s per
  # phase), and sent straight to backend-1 (HAMMER_URL): at 100/s and a few ms each a gateway
  # spread leaves ~0 requests in flight on one backend at any instant, so a kill could miss them
  # all; straight to the victim, every request in flight at the kill, and every one sent while it
  # is down, loses its answer, and the stress client then requires at least one such request to
  # be replayed consistently. Everything else (preparation, reads, the replay) goes through the
  # gateway. No mixed ops (-chaos-restart refuses them: the replay could refill room a cancel
  # freed), so the WebSocket order check SKIPs and is not required (Q4/ws, the fan-out, is).
  BASE_URL="$GATEWAY" BACKEND_A_URL="$BACKEND_A" BACKEND_B_URL="$BACKEND_B" HAMMER_URL="$BACKEND_A" PHASES=river,north \
    RATE="${CHAOS_RATE:-100}" CONCURRENCY="${CHAOS_CONCURRENCY:-300}" SLOTS="${CHAOS_SLOTS:-3}" \
    WS_CLIENTS=2 IDEM_RACERS=0 CUSTOMER_REQUESTS=0 MIXED_OPS=0 STRESS_CHAOS_RESTART=1 \
    STRESS_REQUIRE="R1,R2a,R2b,R3,Q1,Q4,Q4/ws" REPORT_JSON="$(chaos_report backend-kill)" \
    bash scripts/stress.sh >"$log" 2>&1 &
  chaos_stress_pid=$!
  local waited=0
  until grep -q '^phase river start' "$log"; do
    kill -0 "$chaos_stress_pid" 2>/dev/null || { cat "$log"; fail "stress exited before its hammer started"; }
    ((waited < 300)) || { cat "$log"; fail "the hammer did not start within 300 s"; }
    sleep 1
    waited=$((waited + 1))
  done
  sleep "${CHAOS_KILL_AFTER:-3}"
  docker compose kill backend-1 >/dev/null || fail "kill backend-1"
  echo "backend-1 killed mid-hammer"
  sleep 3
  docker compose start backend-1 >/dev/null || fail "start backend-1"
  wait_healthy "$BACKEND_A" 120 || fail "backend-1 not healthy within 120 s of its restart"
  echo "backend-1 back"
  local rc=0
  wait "$chaos_stress_pid" || rc=$?
  chaos_stress_pid=""
  cat "$log"
  [[ $rc -eq 0 ]] || fail "stress with backend-1 killed mid-hammer"
  ok "backend-1 killed mid-hammer: lost requests replayed, capacity exact, integrity holds, outbox drained"
}

check_chaos() {
  trap chaos_restore EXIT
  trap 'exit 130' INT TERM
  chaos_tmp=$(mktemp -d)
  chaos_redis
  chaos_kafka
  chaos_backend_kill
  chaos_restore # cancel the chaos bookings, remove the temp files
  trap - EXIT INT TERM
}

steps=("$@")
[[ ${#steps[@]} -gt 0 ]] || steps=(stack smoke cluster ws dms stress chaos)
for s in "${steps[@]}"; do
  case "$s" in
    stack | smoke | cluster | ws | dms | stress | chaos) ;;
    *) fail "unknown step '$s' (stack smoke cluster ws dms stress chaos)" ;;
  esac
done

if [[ -z "${RUN_START:-}" ]]; then
  # Assigned on its own: inside echo "$(…)" a psql failure would go unnoticed and leave it empty.
  RUN_START=$(docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc 'SELECT now()') \
    || fail "could not read the database clock"
  [[ -n "$RUN_START" ]] || fail "could not read the database clock"
fi

for s in "${steps[@]}"; do
  "check_$s"
done
echo
echo "${GRN}cluster checks passed:${NC} ${steps[*]}"
