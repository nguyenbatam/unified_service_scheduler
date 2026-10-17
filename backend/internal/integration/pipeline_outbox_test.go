package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
	"github.com/segmentio/kafka-go"
)

// Transient per-message errors (what kafka-go reports for every message of a partition batch hit
// by a leader election or under-replication) only back off: however many rounds, they never count
// a rejection and never poison the row. A content rejection does.
func TestOutboxTransientPerMessageErrorsNeverPoison(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	insertConfigEvent(t, st, agg, "T1")
	insertConfigEvent(t, st, uuid.New(), "OTHER")

	bus := newTestBus(busRules{every: &events.RetriableError{Err: kafka.NotEnoughReplicas}})
	cfg := worker.DefaultOutboxConfig()
	cfg.MaxRejections = 2
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", cfg)
	for range 6 {
		if _, err := ob.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now() WHERE status = `+code(domain.OutboxPending)); err != nil {
			t.Fatal(err)
		}
	}
	if !scalar[bool](t, pool, `
		SELECT bool_and(status = `+code(domain.OutboxPending)+` AND rejections = 0 AND attempts = 6) FROM outbox_events WHERE status <> `+code(domain.OutboxPublished)) {
		t.Fatal("transient errors must leave rows pending with no rejection counted")
	}

	// Outcome unknown (a write abandoned at its deadline): held back past the writer lifetime so
	// it cannot be published again while the original may still land.
	bus.update(func(r *busRules) {
		r.every = &events.RetriableError{Err: fmt.Errorf("%w: %w", events.ErrOutcomeUnknown, context.DeadlineExceeded)}
	})
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET attempts = 0 WHERE status = `+code(domain.OutboxPending)); err != nil {
		t.Fatal(err) // so the ordinary backoff (2 s) would be shorter than the writer lifetime
	}
	if _, err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, pool, `
		SELECT bool_and(status = `+code(domain.OutboxPending)+` AND rejections = 0 AND next_attempt_at >= now() + make_interval(secs => $1::float8))
		FROM outbox_events WHERE status <> `+code(domain.OutboxPublished), (events.MaxWriteLifetime - time.Second).Seconds()) {
		t.Fatal("rows with an unknown publish outcome must wait out the writer lifetime")
	}

	// A content rejection counts: MaxRejections of them poison the row.
	bus.update(func(r *busRules) { r.every = kafka.MessageSizeTooLarge })
	for range 2 {
		if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now() WHERE status = `+code(domain.OutboxPending)); err != nil {
			t.Fatal(err)
		}
		if _, err := ob.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE payload->>'label' = 'T1'`); got != "failed" {
		t.Fatalf("T1 after 2 rejections = %s, want failed", got)
	}
}

// RequeueFailedOutboxIdle puts poisoned rows back in the queue (rejections reset) once the operator
// fixed the cause; rows in any other status are untouched.
func TestRequeueFailedOutbox(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	insertConfigEvent(t, st, uuid.New(), "POISON")
	insertConfigEvent(t, st, uuid.New(), "PENDING")
	poison := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'POISON'`)
	pending := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'PENDING'`)
	published := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE status = `+code(domain.OutboxPublished)+` LIMIT 1`)
	if _, err := pool.Exec(ctx, `
		UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+`, rejections = 10, last_error = 'Message Size Too Large',
		       next_attempt_at = now() + interval '1 hour'
		WHERE id = $1`, poison); err != nil {
		t.Fatal(err)
	}

	n, skipped, err := st.RequeueFailedOutboxIdle(ctx, []uuid.UUID{poison, pending, published})
	if err != nil || n != 1 || len(skipped) != 0 {
		t.Fatalf("requeued %d, skipped %v (err %v), want only the failed row", n, skipped, err)
	}
	if !scalar[bool](t, pool, `
		SELECT status = `+code(domain.OutboxPending)+` AND rejections = 0 AND next_attempt_at <= now() AND claimed_at IS NULL
		FROM outbox_events WHERE id = $1`, poison) {
		t.Fatal("requeued row must be pending, claimable now, with its rejections reset")
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE id = $1`, published); got != "published" {
		t.Fatalf("published row changed to %s", got)
	}
	if n, _, err := st.RequeueFailedOutboxIdle(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty requeue = %d %v", n, err)
	}

	bus := newTestBus(busRules{})
	if _, err := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig()).Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE id = $1`, poison); got != "published" {
		t.Fatalf("requeued row after flush = %s, want published", got)
	}
}

// A requeued failed row is older than the events published after it. While a newer event of the
// same aggregate is being published, the claim must not take the old one too (two in flight). The
// operator endpoint refuses that requeue outright; a requeue whose snapshot predates the claim of
// the newer event can still land, and the claim is the guard then.
func TestClaimOutboxWaitsForInFlightNewerEvent(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	insertConfigEvent(t, st, agg, "OLD")
	insertConfigEvent(t, st, agg, "NEW")
	oldID := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'OLD'`)
	newID := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'NEW'`)
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+` WHERE id = $1`, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxProcessing)+`, claimed_at = now() WHERE id = $1`, newID); err != nil {
		t.Fatal(err)
	}
	if n, skipped, err := st.RequeueFailedOutboxIdle(ctx, []uuid.UUID{oldID}); err != nil || n != 0 || len(skipped) != 1 || skipped[0] != oldID {
		t.Fatalf("requeue while the aggregate is in flight = %d, skipped %v, %v; want it skipped", n, skipped, err)
	}
	// The racing requeue: it committed after the newer event was claimed.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxPending)+`, rejections = 0, next_attempt_at = now() WHERE id = $1`, oldID); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d rows while a newer event of the aggregate is processing", len(claimed))
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxPublished)+`, claimed_at = NULL WHERE id = $1`, newID); err != nil {
		t.Fatal(err)
	}
	claimed, err = st.ClaimOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != oldID {
		t.Fatalf("after the newer event finished, claimed %v, want the requeued row", claimed)
	}
}

// Every instance purges on the same schedule: rows another purge holds are skipped, not waited
// on, so concurrent purges both make progress and none ends in a lock timeout.
func TestPurgesSkipRowsLockedByAnotherInstance(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE outbox_events SET status = ` + code(domain.OutboxPublished) + `, published_at = now()`) // seed events: recent
	exec(`
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, published_at)
		SELECT gen_random_uuid(), ` + code(domain.AggregateTechnician) + `, gen_random_uuid(), ` + code(domain.EventConfigUpdated) + `, '{}', ` + code(domain.OutboxPublished) + `, now() - interval '8 days'
		FROM generate_series(1, 1500)`)
	appointment := scalar[string](t, pool, `SELECT id::text FROM appointments LIMIT 1`)
	exec(`
		INSERT INTO notifications (id, appointment_id, kind, channel, recipient, subject, body, dedupe_key, status, start_at, created_at)
		SELECT gen_random_uuid(), $1::uuid, `+code(domain.KindConfirmation)+`, `+code(domain.ChannelEmail)+`, 'a@example.com', 's', 'b', 'old:' || g, `+code(domain.NotificationSent)+`,
		       now() - interval '40 days', now() - interval '40 days'
		FROM generate_series(1, 1500) g`, appointment)

	// Another instance's purge is mid-delete on 1000 rows of each table.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, lock := range []string{
		`SELECT id FROM outbox_events WHERE status = ` + code(domain.OutboxPublished) + ` AND published_at < now() - interval '7 days' LIMIT 1000 FOR UPDATE`,
		`SELECT id FROM notifications WHERE status = ` + code(domain.NotificationSent) + ` AND created_at < now() - interval '30 days' LIMIT 1000 FOR UPDATE`,
	} {
		if _, err := tx.Exec(ctx, lock); err != nil {
			t.Fatal(err)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outbox, err := st.PurgePublishedOutbox(waitCtx, 7*24*time.Hour, 1000)
	if err != nil || outbox != 500 {
		t.Fatalf("outbox purge next to a locked batch: deleted %d, err %v; want the 500 unlocked rows at once", outbox, err)
	}
	notified, err := st.PurgeNotifications(waitCtx, 30*24*time.Hour, 1000)
	if err != nil || notified != 500 {
		t.Fatalf("notification purge next to a locked batch: deleted %d, err %v; want the 500 unlocked rows at once", notified, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Now two purgers race over a fresh backlog: no errors, everything deleted exactly once.
	exec(`
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, published_at)
		SELECT gen_random_uuid(), ` + code(domain.AggregateTechnician) + `, gen_random_uuid(), ` + code(domain.EventConfigUpdated) + `, '{}', ` + code(domain.OutboxPublished) + `, now() - interval '8 days'
		FROM generate_series(1, 6000)`)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		total   int64
		errsSum []error
	)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			for {
				n, err := st.PurgePublishedOutbox(waitCtx, 7*24*time.Hour, 500)
				mu.Lock()
				total += n
				if err != nil {
					errsSum = append(errsSum, err)
				}
				mu.Unlock()
				if err != nil || n == 0 {
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errsSum...); err != nil {
		t.Fatalf("concurrent purge: %v", err)
	}
	if total != 7000 { // 6000 new + the 1000 that were locked before
		t.Fatalf("deleted %d, want 7000", total)
	}
	if left := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE published_at < now() - interval '7 days'`); left != 0 {
		t.Fatalf("%d purgeable rows left", left)
	}
}

// A contact of only whitespace (tabs, a no-break space) counts as missing in the reminder sweep,
// as it does for the producer (strings.TrimSpace): otherwise the appointment is selected on every
// sweep, never enqueued, and fills the batch for good.
func TestReminderSweepIgnoresWhitespaceContacts(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 11, 0)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, start))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE customers SET email = E'\t\r\n', phone = ' ' || chr(160) || chr(12288)
		WHERE id = (SELECT customer_id FROM appointments WHERE id = $1)`, item.ID); err != nil {
		t.Fatal(err)
	}
	selected := func() bool {
		t.Helper()
		got, err := st.ReminderCandidates(ctx, domain.KindReminder24h, start.Add(-23*time.Hour), 48*time.Hour, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.AppointmentID == item.ID {
				return true
			}
		}
		return false
	}
	if selected() {
		t.Fatal("a customer with only whitespace contacts must not be a reminder candidate")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE customers SET phone = '+44 7700 900123'
		WHERE id = (SELECT customer_id FROM appointments WHERE id = $1)`, item.ID); err != nil {
		t.Fatal(err)
	}
	if !selected() {
		t.Fatal("a real phone number makes the appointment a candidate again")
	}
}
