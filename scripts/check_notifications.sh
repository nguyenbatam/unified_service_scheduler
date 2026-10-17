#!/usr/bin/env bash
# Customer notifications end to end (Kafka shared group → queue → sender) for one appointment that
# was booked and then cancelled (scripts/smoke_dms.sh prints it as APPOINTMENT_ID=…): both the
# confirmation and the cancellation must be sent, and no notification created since SINCE failed.
# Usage: ./scripts/check_notifications.sh <appointment-id> [since-timestamptz]
#   SINCE defaults to the last 15 minutes (scope the "none failed" check to this run's rows).
#   WAIT_SECONDS (default 30): how long to wait for both to be sent.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"

APPT="${1:?usage: check_notifications.sh <appointment-id> [since]}"
SINCE="${2:-}"
[[ "$APPT" =~ ^[0-9a-fA-F-]{36}$ ]] || { echo "not an appointment id: $APPT" >&2; exit 2; }

psql_at() {
  docker compose exec -T db psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -Atc "$1"
}
if [[ -z "$SINCE" ]]; then
  SINCE=$(psql_at "SELECT now() - interval '15 minutes'")
fi
# Only digits, separators and a zone may appear: it is interpolated into SQL below.
[[ "$SINCE" =~ ^[0-9:.\ +T-]+$ ]] || { echo "bad since timestamp: $SINCE" >&2; exit 2; }

# Codes come from scripts/lib/codes.py (the mirror of internal/domain).
code() { python3 "$ROOT/scripts/lib/codes.py" "$1"; }
SENT=$(code notification_status.sent)
FAILED_STATUS=$(code notification_status.failed)
CONFIRMATION=$(code notification_kind.confirmation)
CANCELLED=$(code notification_kind.cancelled)
KINDS="SELECT count(DISTINCT kind) FROM notifications WHERE appointment_id = '$APPT' AND status = $SENT AND kind IN ($CONFIRMATION, $CANCELLED)"
GOT=""
for _ in $(seq 1 "${WAIT_SECONDS:-30}"); do
  GOT=$(psql_at "$KINDS")
  [[ "$GOT" == 2 ]] && break
  sleep 1
done
if [[ "$GOT" != 2 ]]; then
  echo "FAIL notifications for $APPT: $GOT of confirmation + cancelled sent, want both" >&2
  psql_at "SELECT kind, channel, status, attempts, coalesce(last_error, '') FROM notifications WHERE appointment_id = '$APPT' ORDER BY created_at" >&2 || true
  exit 1
fi
FAILED=$(psql_at "SELECT count(*) FROM notifications WHERE status = $FAILED_STATUS AND created_at >= '$SINCE'::timestamptz")
if [[ "$FAILED" -ne 0 ]]; then
  echo "FAIL $FAILED notification(s) failed since $SINCE" >&2
  exit 1
fi
echo "notifications ok: $APPT sent ($GOT), none failed since $SINCE"
