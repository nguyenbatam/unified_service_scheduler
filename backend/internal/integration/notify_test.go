package integration_test

import (
	"errors"
	"log/slog"
	"strconv"
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

// publishedEvents runs the outbox worker once and returns what it put on the bus.
func publishedEvents(t *testing.T, st *store.Store) []realtime.Event {
	t.Helper()
	bus := newTestBus(busRules{})
	if _, err := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig()).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	return bus.events()
}

func TestNotificationsFromEventsAndReminders(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, start))
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	cfg := notify.DefaultConfig()
	cfg.MaxAttempts = 2
	n := notify.New(st, sender, slog.Default(), cfg)

	// Confirmation: one email per event, deduplicated on redelivery (at-least-once Kafka).
	evs := publishedEvents(t, st)
	if len(evs) != 1 || evs[0].ID.String() == "" {
		t.Fatalf("events = %+v", evs)
	}
	for range 2 {
		if err := n.HandleEvent(ctx, evs[0]); err != nil {
			t.Fatal(err)
		}
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE appointment_id = $1`, item.ID); got != 1 {
		t.Fatalf("notifications = %d, want 1 (deduped)", got)
	}
	if claimed, err := n.Flush(ctx); err != nil || claimed != 1 {
		t.Fatalf("flush = %d %v", claimed, err)
	}
	if len(sender.sent) != 1 || sender.sent[0].Kind != domain.KindConfirmation || sender.sent[0].Channel != domain.ChannelEmail ||
		sender.sent[0].Recipient != "amelia.clarke@example.com" {
		t.Fatalf("sent = %+v", sender.sent)
	}
	if got := scalar[string](t, pool, `SELECT `+notificationStatusText("status")+` FROM notifications WHERE appointment_id = $1`, item.ID); got != "sent" {
		t.Fatalf("status = %s", got)
	}

	// Reminders: only for slots booked well ahead, once per slot, SMS first.
	if _, err := pool.Exec(ctx, `UPDATE appointments SET scheduled_at = $2 WHERE id = $1`, item.ID, start.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n.WithClock(func() time.Time { return start.Add(-23 * time.Hour) })
	for round, want := range []int{1, 0} {
		got, err := n.EnqueueReminders(ctx)
		if err != nil || got != want {
			t.Fatalf("round %d: enqueued %d (err %v), want %d", round, got, err, want)
		}
	}
	reminder := scalar[string](t, pool, `SELECT `+notificationChannelText("channel")+` FROM notifications WHERE appointment_id = $1 AND kind = `+code(domain.KindReminder24h), item.ID)
	if reminder != "sms" {
		t.Fatalf("reminder channel = %s", reminder)
	}
	// Cancelled before the reminder went out: it is skipped, not sent.
	if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+notificationStatusText("status")+` FROM notifications WHERE appointment_id = $1 AND kind = `+code(domain.KindReminder24h), item.ID); got != "skipped" {
		t.Fatalf("reminder status = %s, want skipped", got)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("a cancelled appointment's reminder was sent: %+v", sender.sent)
	}

	// A booking made today for tomorrow gets no "24 h" reminder (it would arrive right after booking).
	fresh, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, start))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET scheduled_at = $2 WHERE id = $1`, fresh.ID, start.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 0 {
		t.Fatalf("enqueued %d (err %v), want 0", got, err)
	}

	// Delivery failures back off, then the row is failed (alerted via metrics).
	evs = publishedEvents(t, st)
	for _, ev := range evs {
		if err := n.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	sender.fail = errors.New("gateway down")
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() WHERE status = `+code(domain.NotificationPending)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	failed := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE status = `+code(domain.NotificationFailed)+` AND last_error = 'gateway down'`)
	if failed == 0 {
		t.Fatal("rows that kept failing must end as failed")
	}
}

// A reminder enqueued for the old slot is skipped once the appointment moves, and the new slot
// gets its own reminder (dedupe is on the slot, not on timestamps).
func TestReminderFollowsReschedule(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	old, moved := londonOn(t, londonDay(t, 1), 11, 0), londonOn(t, londonDay(t, 1), 12, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, old))
	if err != nil {
		t.Fatal(err)
	}
	backdate := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE appointments SET scheduled_at = start_at - interval '72 hours' WHERE id = $1`, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	backdate()
	sender := &recordingSender{}
	n := notify.New(st, sender, slog.Default(), notify.DefaultConfig()).WithClock(func() time.Time { return old.Add(-23 * time.Hour) })
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 1 {
		t.Fatalf("enqueued %d (err %v)", got, err)
	}
	if _, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: moved}); err != nil {
		t.Fatal(err)
	}
	backdate()
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("a reminder for the old slot went out: %+v", sender.sent)
	}
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 1 {
		t.Fatalf("new slot: enqueued %d (err %v), want 1", got, err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || !sender.sent[0].StartAt.Equal(moved) {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

// Moved away and back (A -> B -> A): the reminder skipped when it left A must not block a fresh
// reminder for A once it returns.
func TestReminderAfterReturningToEarlierSlot(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	slotA, slotB := londonOn(t, londonDay(t, 1), 11, 0), londonOn(t, londonDay(t, 1), 12, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, slotA))
	if err != nil {
		t.Fatal(err)
	}
	// As if each move happened days ahead: a reminder needs the slot booked well before its start.
	// Each move gets its own, later scheduled_at, as Reschedule (now()) gives it in real life.
	hoursAhead := 72
	backdate := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE appointments SET scheduled_at = start_at - make_interval(hours => $2) WHERE id = $1`,
			item.ID, hoursAhead); err != nil {
			t.Fatal(err)
		}
		hoursAhead--
	}
	moveTo := func(start time.Time) {
		t.Helper()
		if _, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: start}); err != nil {
			t.Fatal(err)
		}
		backdate()
	}
	backdate()
	sender := &recordingSender{}
	n := notify.New(st, sender, slog.Default(), notify.DefaultConfig()).WithClock(func() time.Time { return slotA.Add(-23 * time.Hour) })
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 1 {
		t.Fatalf("slot A: enqueued %d (err %v), want 1", got, err)
	}
	moveTo(slotB)
	if _, err := n.Flush(ctx); err != nil { // skips the reminder for A
		t.Fatal(err)
	}
	moveTo(slotA)
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 1 {
		t.Fatalf("back at A: enqueued %d (err %v), want 1", got, err)
	}
	if got, err := n.EnqueueReminders(ctx); err != nil || got != 0 {
		t.Fatalf("back at A, second sweep: enqueued %d (err %v), want 0 (deduped)", got, err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || !sender.sent[0].StartAt.Equal(slotA) {
		t.Fatalf("sent = %+v, want one reminder for slot A", sender.sent)
	}
}

// Two producers that race (e.g. instances sweeping around an edit that bumps the version but
// keeps the slot) cannot leave two live reminders for one slot, whatever their dedupe keys; once
// the live one is skipped (the job left the slot) a new one is accepted.
func TestOneLiveReminderPerSlot(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 11, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, start))
	if err != nil {
		t.Fatal(err)
	}
	reminder := func(key string) store.NewNotification {
		return store.NewNotification{
			ID: uuid.New(), AppointmentID: item.ID, Kind: domain.KindReminder24h, Channel: domain.ChannelSMS, Recipient: "0770",
			Subject: "s", Body: "b", StartAt: start, DedupeKey: key,
		}
	}
	for i, want := range []int{1, 0} {
		n, err := st.EnqueueNotifications(ctx, []store.NewNotification{reminder("producer-" + strconv.Itoa(i))})
		if err != nil || n != want {
			t.Fatalf("producer %d: inserted %d (err %v), want %d", i, n, err, want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status = `+code(domain.NotificationSkipped)+` WHERE appointment_id = $1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.EnqueueNotifications(ctx, []store.NewNotification{reminder("after-skip")}); err != nil || n != 1 {
		t.Fatalf("after skip: inserted %d (err %v), want 1", n, err)
	}
}
