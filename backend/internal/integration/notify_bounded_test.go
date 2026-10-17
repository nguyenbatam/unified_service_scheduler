package integration_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/notify"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The bookkeeping skip before each claim only touches stale rows that are due, and passes over a
// row another sender holds instead of waiting for it; the claim itself never hands out a stale row
// either way (the stale predicate is part of the claim).
func TestNotificationStaleSkipIsBounded(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, start))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
	// Three reminders for slots the appointment is not at (stale), one per slot so the
	// one-live-reminder-per-slot index accepts them.
	stale := func(offset time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{{
			ID: id, AppointmentID: item.ID, Kind: domain.KindReminder24h, Channel: domain.ChannelSMS, Recipient: "0770",
			Subject: "s", Body: "b", DedupeKey: "stale|" + id.String(), StartAt: start.Add(offset),
		}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	due, later, held := stale(time.Hour), stale(2*time.Hour), stale(3*time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() + interval '1 minute' WHERE id = $1`, later); err != nil {
		t.Fatal(err)
	}
	other, err := pool.Begin(ctx) // another sender holds this row
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, `SELECT 1 FROM notifications WHERE id = $1 FOR UPDATE`, held); err != nil {
		t.Fatal(err)
	}

	// Waiting on the held row would last until the pool's lock_timeout; SKIP LOCKED must not wait
	// at all, so anything under half of it (generous under -race and a loaded CI box) is a pass.
	lockTimeout := store.DefaultPoolOptions().LockTimeout
	waitCtx, cancel := context.WithTimeout(ctx, 2*lockTimeout)
	defer cancel()
	began := time.Now()
	claimed, err := st.ClaimNotifications(waitCtx, 10)
	if err != nil {
		t.Fatalf("claim next to a locked stale row: %v", err)
	}
	if took := time.Since(began); took > lockTimeout/2 {
		t.Fatalf("claim waited %s on a row another sender holds", took)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d stale rows", len(claimed))
	}
	status := func(id uuid.UUID) string {
		return scalar[string](t, pool, `SELECT `+notificationStatusText("status")+` FROM notifications WHERE id = $1`, id)
	}
	if status(due) != "skipped" || status(later) != "pending" || status(held) != "pending" {
		t.Fatalf("statuses due=%s later=%s held=%s, want skipped/pending/pending", status(due), status(later), status(held))
	}

	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNotifications(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if status(held) != "skipped" {
		t.Fatalf("held row once released = %s, want skipped", status(held))
	}
}

// Reminders for every due appointment of a rule go in one insert; a second sweep adds nothing.
func TestReminderSweepEnqueuesAllDueInOneRound(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	var ids []uuid.UUID
	for i, vehicle := range []uuid.UUID{seed.VehicleA, seed.VehicleB} {
		item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, vehicle, start.Add(time.Duration(i)*time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET scheduled_at = start_at - interval '72 hours' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	n := notify.New(st, &recordingSender{}, slog.Default(), notify.DefaultConfig()).
		WithClock(func() time.Time { return start.Add(-23 * time.Hour) })
	for round, want := range []int{2, 0} {
		got, err := n.EnqueueReminders(ctx)
		if err != nil || got != want {
			t.Fatalf("round %d: enqueued %d (err %v), want %d", round, got, err, want)
		}
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE kind = `+code(domain.KindReminder24h)+` AND appointment_id = ANY($1)`, ids); got != 2 {
		t.Fatalf("reminders = %d, want one per appointment", got)
	}
}

// The bookkeeping skip looks only at a window of the oldest due rows (4x the claim's limit), so a
// large backlog of rows that cannot go out yet (here: newer notices waiting behind an older one
// that backs off) never makes every claim walk all of it. A stale row behind the window is left
// pending, and never claimed, until the window reaches it.
func TestNotificationStaleSkipWindowIsBounded(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 11, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, start))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
	const limit, backlog = 10, 60 // window = 40 rows
	// v1 backs off (not due); v2..v61 are due but wait behind it: pending, not stale, not claimable.
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications (id, appointment_id, kind, channel, recipient, subject, body, dedupe_key,
		                           start_at, appointment_version, next_attempt_at)
		SELECT gen_random_uuid(), $1, CASE WHEN v = 1 THEN `+code(domain.KindConfirmation)+` ELSE `+code(domain.KindCancelled)+` END, `+code(domain.ChannelEmail)+`, 'a@example.com',
		       's', 'b', 'backlog|' || v, $2, v,
		       CASE WHEN v = 1 THEN now() + interval '1 hour' ELSE now() - interval '1 hour' + v * interval '1 second' END
		FROM generate_series(1, $3::int + 1) AS v`, item.ID, start, backlog); err != nil {
		t.Fatal(err)
	}
	staleID := uuid.New() // a reminder for a slot the appointment is not at, due after the backlog
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{{
		ID: staleID, AppointmentID: item.ID, Kind: domain.KindReminder24h, Channel: domain.ChannelSMS, Recipient: "0770",
		Subject: "s", Body: "b", DedupeKey: "stale|" + staleID.String(), StartAt: start.Add(time.Hour),
	}}); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		return scalar[string](t, pool, `SELECT `+notificationStatusText("status")+` FROM notifications WHERE id = $1`, staleID)
	}
	claimed, err := st.ClaimNotifications(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 || status() != "pending" {
		t.Fatalf("claimed %d, stale row %s: want nothing claimed and the row behind the window left pending", len(claimed), status())
	}
	// Part of the backlog goes away (purged here; marking it sent would make the rest stale); the
	// window now reaches the stale row.
	if _, err := pool.Exec(ctx, `DELETE FROM notifications WHERE dedupe_key LIKE 'backlog|%' AND appointment_version > 30`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNotifications(ctx, limit); err != nil {
		t.Fatal(err)
	}
	if status() != "skipped" {
		t.Fatalf("stale row once inside the window = %s, want skipped", status())
	}
}
