"""SMALLINT enum codes for the shell and Python checks (the API, Kafka, WebSocket and database
carry these numbers). The definition is internal/domain in the backend; this is a mirror for code
that cannot import Go, and backend/internal/domain/codes_mirror_test.go fails when they differ.

Python: `from codes import EVENT; EVENT["APPOINTMENT_CREATED"]`.
Shell:  `python3 scripts/lib/codes.py outbox_status.published` prints 3.
"""

import sys

EVENT = {
    "APPOINTMENT_CREATED": 1,
    "APPOINTMENT_RESCHEDULED": 2,
    "APPOINTMENT_CANCELLED": 3,
    "APPOINTMENT_CHECKED_IN": 4,
    "APPOINTMENT_STARTED": 5,
    "APPOINTMENT_COMPLETED": 6,
    "APPOINTMENT_NO_SHOW": 7,
    "APPOINTMENT_REINSTATED": 8,
    "APPOINTMENT_REASSIGNED": 9,
    "APPOINTMENT_EXTENDED": 10,
    "APPOINTMENT_OVERRUNNING": 11,
    "SESSION_REVOKED": 12,
    "CONFIG_UPDATED": 13,
    "AVAILABILITY_CHANGED": 14,
    "CONNECTED": 15,
}

OUTBOX_STATUS = {"pending": 1, "processing": 2, "published": 3, "failed": 4}

NOTIFICATION_STATUS = {"pending": 1, "sending": 2, "sent": 3, "failed": 4, "skipped": 5}

NOTIFICATION_KIND = {
    "confirmation": 1,
    "rescheduled": 2,
    "cancelled": 3,
    "no_show": 4,
    "reminder_24h": 5,
    "reminder_2h": 6,
}

TABLES = {
    "event": EVENT,
    "outbox_status": OUTBOX_STATUS,
    "notification_status": NOTIFICATION_STATUS,
    "notification_kind": NOTIFICATION_KIND,
}

if __name__ == "__main__":
    if len(sys.argv) != 2 or "." not in sys.argv[1]:
        sys.exit("usage: codes.py <table>.<name>   e.g. outbox_status.published")
    table, name = sys.argv[1].split(".", 1)
    try:
        print(TABLES[table][name])
    except KeyError:
        sys.exit(f"codes.py: unknown code {sys.argv[1]}")
