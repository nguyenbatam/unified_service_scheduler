package integration_test

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/notify"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

// drainOutboxEvents runs the outbox worker until nothing is claimable and returns what it published.
func drainOutboxEvents(t *testing.T, st *store.Store) []realtime.Event {
	t.Helper()
	bus := newTestBus(busRules{})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	for {
		n, err := ob.Flush(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	return bus.events()
}

func changeNotice(item domain.Appointment, version int) store.NewNotification {
	return store.NewNotification{
		ID: uuid.New(), AppointmentID: item.ID, Kind: domain.KindRescheduled, Channel: domain.ChannelEmail,
		Recipient: "a@example.com", Subject: "s", Body: "b", StartAt: item.StartAt,
		DedupeKey: fmt.Sprintf("%s|v%d|email", item.ID, version), AppointmentVersion: version,
	}
}

func claimIDs(t *testing.T, st *store.Store) []store.Notification {
	t.Helper()
	batch, err := st.ClaimNotifications(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

// Events reach the notifier out of order (redelivery, replay): only the newest change notice for
// an appointment goes out; older ones, whenever they arrive, are skipped.
func TestNewerChangeNoticeSupersedesOlder(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	final := londonOn(t, londonDay(t, 1), 12, 0)
	for _, start := range []time.Time{londonOn(t, londonDay(t, 1), 11, 0), final} {
		if _, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: start}); err != nil {
			t.Fatal(err)
		}
	}
	evs := drainOutboxEvents(t, st)
	if len(evs) != 3 || evs[0].Type != domain.EventAppointmentCreated || evs[1].Appointment.Version != 2 || evs[2].Appointment.Version != 3 {
		t.Fatalf("events out of order: %+v", evs)
	}

	sender := &recordingSender{}
	n := notify.New(st, sender, slog.Default(), notify.DefaultConfig())
	for _, ev := range []realtime.Event{evs[2], evs[1]} { // newest first
		if err := n.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || sender.sent[0].Kind != domain.KindRescheduled || !sender.sent[0].StartAt.Equal(final) {
		t.Fatalf("sent = %+v, want only the move to 12:00", sender.sent)
	}
	// The confirmation arrives last (late redelivery): stale, never sent.
	if err := n.HandleEvent(ctx, evs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("a superseded notice went out: %+v", sender.sent)
	}
	got := scalar[string](t, pool, `
		SELECT string_agg(appointment_version || ':' || `+notificationStatusText("status")+`, ',' ORDER BY appointment_version)
		FROM notifications WHERE appointment_id = $1`, item.ID)
	if got != "1:skipped,2:skipped,3:sent" {
		t.Fatalf("notifications = %s", got)
	}
}

// A newer notice waits while an older one is being sent (so they cannot cross on the wire), and an
// older one claimed before the newer arrived is dropped by the pre-send re-check.
func TestChangeNoticeWaitsForOlderInFlight(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(v int) {
		t.Helper()
		if n, err := st.EnqueueNotifications(ctx, []store.NewNotification{changeNotice(item, v)}); err != nil || n != 1 {
			t.Fatalf("enqueue v%d: %d %v", v, n, err)
		}
	}
	if err := st.Exec(ctx, `UPDATE notifications SET status = `+code(domain.NotificationSent)); err != nil { // seed / booking noise
		t.Fatal(err)
	}
	enqueue(1)
	first := claimIDs(t, st)
	if len(first) != 1 || first[0].AppointmentVersion != 1 {
		t.Fatalf("claim v1 = %+v", first)
	}
	enqueue(2)
	if got := claimIDs(t, st); len(got) != 0 {
		t.Fatalf("v2 claimed while v1 is still sending: %+v", got)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{first[0].ID}, first[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	second := claimIDs(t, st)
	if len(second) != 1 || second[0].AppointmentVersion != 2 {
		t.Fatalf("claim after v1 sent = %+v", second)
	}

	enqueue(3) // arrives after v2 was claimed, before it is sent
	skipped, err := st.SkipStaleNotifications(ctx, []uuid.UUID{second[0].ID}, second[0].ClaimedAt)
	if err != nil || len(skipped) != 1 || skipped[0] != second[0].ID {
		t.Fatalf("pre-send re-check = %v %v, want v2 skipped", skipped, err)
	}
	third := claimIDs(t, st)
	if len(third) != 1 || third[0].AppointmentVersion != 3 {
		t.Fatalf("claim v3 = %+v", third)
	}
	// The newest is never stale.
	if skipped, err := st.SkipStaleNotifications(ctx, []uuid.UUID{third[0].ID}, third[0].ClaimedAt); err != nil || len(skipped) != 0 {
		t.Fatalf("newest notice skipped: %v %v", skipped, err)
	}
}

// The slot check is part of the claim: a reminder for a slot the appointment no longer holds is
// never claimed, and one claimed just before a reschedule is dropped right before sending.
func TestReminderClaimAfterReschedule(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	old, moved := londonOn(t, londonDay(t, 1), 11, 0), londonOn(t, londonDay(t, 1), 12, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, old))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE notifications SET status = `+code(domain.NotificationSent))
	reminder := func(start time.Time) store.NewNotification {
		return store.NewNotification{
			ID: uuid.New(), AppointmentID: item.ID, Kind: domain.KindReminder24h, Channel: domain.ChannelSMS, Recipient: "0770",
			Subject: "s", Body: "b", StartAt: start, DedupeKey: "reminder_24h|" + item.ID.String() + "|" + start.String(),
		}
	}
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{reminder(old)}); err != nil {
		t.Fatal(err)
	}
	claimed := claimIDs(t, st)
	if len(claimed) != 1 {
		t.Fatalf("claim = %+v", claimed)
	}
	if _, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: moved}); err != nil {
		t.Fatal(err)
	}
	skipped, err := st.SkipStaleNotifications(ctx, []uuid.UUID{claimed[0].ID}, claimed[0].ClaimedAt)
	if err != nil || len(skipped) != 1 {
		t.Fatalf("claimed reminder for the old slot not dropped before send: %v %v", skipped, err)
	}

	// A sweep that read the old slot enqueues late: never claimable.
	late := reminder(old)
	late.DedupeKey += "|late"
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{late, reminder(moved)}); err != nil {
		t.Fatal(err)
	}
	got := claimIDs(t, st)
	if len(got) != 1 || !got[0].StartAt.Equal(moved) {
		t.Fatalf("claim = %+v, want only the new slot's reminder", got)
	}
	if status := scalar[string](t, pool, `SELECT `+notificationStatusText("status")+` FROM notifications WHERE id = $1`, late.ID); status != "skipped" {
		t.Fatalf("stale reminder status = %s", status)
	}
}

// One INSERT for the whole batch, still idempotent on dedupe_key (inside the batch and across calls).
func TestEnqueueNotificationsBatch(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	a, b := changeNotice(item, 1), changeNotice(item, 2)
	dup := changeNotice(item, 2) // same dedupe key as b
	if n, err := st.EnqueueNotifications(ctx, []store.NewNotification{a, b, dup}); err != nil || n != 2 {
		t.Fatalf("first batch = %d %v, want 2", n, err)
	}
	r := a
	r.ID, r.Kind, r.DedupeKey, r.AppointmentVersion = uuid.New(), domain.KindReminder2h, "r|"+item.ID.String(), 0
	if n, err := st.EnqueueNotifications(ctx, []store.NewNotification{a, r}); err != nil || n != 1 {
		t.Fatalf("second batch = %d %v, want 1", n, err)
	}
	if !scalar[bool](t, pool, `SELECT appointment_version IS NULL FROM notifications WHERE id = $1`, r.ID) {
		t.Fatal("a reminder must carry no appointment version")
	}
	if n, err := st.EnqueueNotifications(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty batch = %d %v", n, err)
	}
}

// Batched marks keep the lease fence: a sender whose lease expired cannot overwrite the outcome.
func TestNotificationBatchMarksAreFenced(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE notifications SET status = `+code(domain.NotificationSent))
	other, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, londonDay(t, 1), 11, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{changeNotice(item, 1), changeNotice(other, 1)}); err != nil {
		t.Fatal(err)
	}
	batch := claimIDs(t, st)
	if len(batch) != 2 {
		t.Fatalf("claim = %+v", batch)
	}
	stale := batch[0].ClaimedAt.Add(-time.Second)
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{batch[0].ID, batch[1].ID}, stale); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotificationsRetry(ctx, []store.SendFailure{{ID: batch[0].ID, Reason: "late"}}, stale, 8); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE status = `+code(domain.NotificationSending)); n != 2 {
		t.Fatalf("stale lease changed rows: sending = %d", n)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{batch[0].ID}, batch[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotificationsRetry(ctx, []store.SendFailure{{ID: batch[1].ID, Reason: "gateway down"}}, batch[1].ClaimedAt, 8); err != nil {
		t.Fatal(err)
	}
	got := scalar[string](t, pool, `
		SELECT string_agg(`+notificationStatusText("status")+` || ':' || COALESCE(last_error, ''), ',' ORDER BY status)
		FROM notifications WHERE id = ANY($1)`, []uuid.UUID{batch[0].ID, batch[1].ID})
	if got != "pending:gateway down,sent:" {
		t.Fatalf("outcomes = %s", got)
	}
}

// Booked then cancelled before the confirmation went out: the customer gets both, in order — a
// cancellation for a booking they were never told about would be confusing. A confirmation that
// only arrives after the cancellation went out (late redelivery) is dropped.
func TestConfirmationThenCancellationBothSentInOrder(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `UPDATE notifications SET status = `+code(domain.NotificationSent)); err != nil { // seed / booking noise
		t.Fatal(err)
	}
	notice := func(kind domain.NotificationKind, v int) store.NewNotification {
		n := changeNotice(item, v)
		n.Kind = kind
		return n
	}
	confirmation, cancellation := notice(domain.KindConfirmation, 1), notice(domain.KindCancelled, 2)
	if n, err := st.EnqueueNotifications(ctx, []store.NewNotification{cancellation, confirmation}); err != nil || n != 2 {
		t.Fatalf("enqueue: %d %v", n, err)
	}
	first := claimIDs(t, st)
	if len(first) != 1 || first[0].Kind != domain.KindConfirmation {
		t.Fatalf("first claim = %+v, want only the confirmation (the cancellation waits)", first)
	}
	if skipped, err := st.SkipStaleNotifications(ctx, []uuid.UUID{first[0].ID}, first[0].ClaimedAt); err != nil || len(skipped) != 0 {
		t.Fatalf("confirmation dropped although nothing newer went out: %v %v", skipped, err)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{first[0].ID}, first[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	second := claimIDs(t, st)
	if len(second) != 1 || second[0].Kind != domain.KindCancelled {
		t.Fatalf("second claim = %+v, want the cancellation", second)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{second[0].ID}, second[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}

	// Another channel's confirmation that only shows up now, after that channel's cancellation went
	// out: out of order, so skipped.
	lateCancel, lateConfirm := notice(domain.KindCancelled, 2), notice(domain.KindConfirmation, 1)
	lateCancel.Channel, lateConfirm.Channel = domain.ChannelSMS, domain.ChannelSMS
	lateCancel.DedupeKey, lateConfirm.DedupeKey = lateCancel.DedupeKey+"|sms", lateConfirm.DedupeKey+"|sms"
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{lateCancel}); err != nil {
		t.Fatal(err)
	}
	sms := claimIDs(t, st)
	if len(sms) != 1 {
		t.Fatalf("sms claim = %+v", sms)
	}
	if err := st.MarkNotificationsSent(ctx, []uuid.UUID{sms[0].ID}, sms[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{lateConfirm}); err != nil {
		t.Fatal(err)
	}
	if got := claimIDs(t, st); len(got) != 0 {
		t.Fatalf("late confirmation claimed after the cancellation went out: %+v", got)
	}
}
