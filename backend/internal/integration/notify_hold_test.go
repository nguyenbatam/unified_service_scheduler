package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// During a sender outage a backed-off change notice blocks the newer ones of its appointment for
// up to 10 minutes. They must leave the claim's scan (next_attempt_at is a cost hint for them),
// come back exactly when the head can go out again, and still go out after it, in order.
func TestBlockedNoticesLeaveTheClaimScan(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, day, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `UPDATE notifications SET status = `+code(domain.NotificationSent)); err != nil { // seed / booking noise
		t.Fatal(err)
	}
	notice := func(a domain.Appointment, kind domain.NotificationKind, v int) {
		t.Helper()
		n := changeNotice(a, v)
		n.Kind = kind
		if got, err := st.EnqueueNotifications(ctx, []store.NewNotification{n}); err != nil || got != 1 {
			t.Fatalf("enqueue %s v%d: %d %v", kind, v, got, err)
		}
	}
	due := func() int {
		return scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE status = `+code(domain.NotificationPending)+` AND next_attempt_at <= now()`)
	}

	notice(item, domain.KindConfirmation, 1)
	head := claimIDs(t, st)
	if len(head) != 1 || head[0].AppointmentVersion != 1 {
		t.Fatalf("claim = %+v, want the confirmation", head)
	}
	const blocked = 40
	for v := 2; v <= blocked+1; v++ { // arrive while the head is sending: due, blocked
		notice(item, domain.KindCancelled, v)
	}
	if err := st.MarkNotificationsRetry(ctx, []store.SendFailure{{ID: head[0].ID, Reason: "gateway down"}}, head[0].ClaimedAt, 8); err != nil {
		t.Fatal(err)
	}
	if n := due(); n != 0 {
		t.Fatalf("%d blocked rows still due behind a backed-off head, want 0 (pushed to its retry)", n)
	}
	if n := scalar[int](t, pool, `
		SELECT count(*)::int FROM notifications l, notifications h
		WHERE h.id = $1 AND l.appointment_id = h.appointment_id AND l.id <> h.id
		  AND l.next_attempt_at <> h.next_attempt_at`, head[0].ID); n != 0 {
		t.Fatalf("%d blocked rows not pushed to exactly the head's retry time", n)
	}
	// Another appointment's notice is claimed straight away.
	notice(other, domain.KindConfirmation, 1)
	if got := claimIDs(t, st); len(got) != 1 || got[0].AppointmentID != other.ID {
		t.Fatalf("claim = %+v, want the other appointment's notice", got)
	}

	// The head's retry time comes: it goes out first; the rest are held while it is sending.
	if err := st.Exec(ctx, `UPDATE notifications SET next_attempt_at = next_attempt_at - interval '1 hour' WHERE appointment_id = $1 AND status = `+code(domain.NotificationPending), item.ID); err != nil {
		t.Fatal(err)
	}
	retried := claimIDs(t, st)
	if len(retried) != 1 || retried[0].ID != head[0].ID {
		t.Fatalf("claim after the backoff = %+v, want the head alone", retried)
	}
	if n := due(); n != 0 {
		t.Fatalf("%d rows due while their head is sending, want 0 (held back)", n)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{retried[0].ID}, retried[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	// Sent: released at once, and the oldest goes next.
	if n := due(); n != blocked {
		t.Fatalf("%d rows due after the head was sent, want %d released", n, blocked)
	}
	next := claimIDs(t, st)
	if len(next) != 1 || next[0].AppointmentVersion != 2 {
		t.Fatalf("claim after the head = %+v, want v2 alone", next)
	}
	// A head that fails for good releases the rest too.
	if err := st.MarkNotificationsRetry(ctx, []store.SendFailure{{ID: next[0].ID, Reason: "bounced"}}, next[0].ClaimedAt, 1); err != nil {
		t.Fatal(err)
	}
	if n := due(); n != blocked-1 {
		t.Fatalf("%d rows due after the head failed, want %d released", n, blocked-1)
	}
}

// Moving a queued notice is only ever about never-claimed rows: a row with a backoff of its own
// keeps it when its predecessor resolves.
func TestReleaseKeepsAClaimedRowsBackoff(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `UPDATE notifications SET status = `+code(domain.NotificationSent)); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 2; v++ {
		n := changeNotice(item, v)
		n.Kind = domain.KindConfirmation
		if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{n}); err != nil {
			t.Fatal(err)
		}
	}
	// v2 failed a send before (attempts 1) and backs off for an hour.
	if err := st.Exec(ctx, `UPDATE notifications SET attempts = 1, next_attempt_at = now() + interval '1 hour' WHERE appointment_id = $1 AND appointment_version = 2`, item.ID); err != nil {
		t.Fatal(err)
	}
	head := claimIDs(t, st)
	if len(head) != 1 || head[0].AppointmentVersion != 1 {
		t.Fatalf("claim = %+v", head)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{head[0].ID}, head[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	wait := scalar[float64](t, pool, `SELECT extract(epoch FROM next_attempt_at - now())::float8 FROM notifications WHERE appointment_id = $1 AND appointment_version = 2`, item.ID)
	if wait < float64(50*time.Minute/time.Second) {
		t.Fatalf("v2's own backoff was shortened to %.0fs", wait)
	}
}
