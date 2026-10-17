package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

func TestOutboxPublishesThinRoutedEvents(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	bus := newTestBus(busRules{})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	n, err := ob.Flush(ctx)
	if err != nil || n != 1 {
		t.Fatalf("flush n=%d err=%v", n, err)
	}
	sent := bus.envelopes()
	ev := sent[0].Event
	if sent[0].Key != item.ID.String() || ev.Type != domain.EventAppointmentCreated || ev.Kind != realtime.KindAppointment ||
		ev.CustomerID != seed.CustomerA || ev.Appointment.Version != 1 {
		t.Fatalf("event = %+v", ev)
	}
	status := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, item.ID)
	if status != "published" {
		t.Fatalf("status = %s", status)
	}
}

func TestOutboxRetryBackoffAndPoison(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	a := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	b := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 15, 0)))

	// One message fails: only that row is retried, with backoff.
	bus := newTestBus(busRules{byKey: map[string]error{b.ID.String(): errors.New("partition leader unavailable")}})
	cfg := worker.DefaultOutboxConfig()
	cfg.MaxRejections = 3
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", cfg)
	if _, err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, a.ID); got != "published" {
		t.Fatalf("a = %s", got)
	}
	waiting := scalar[bool](t, pool, `
		SELECT status = `+code(domain.OutboxPending)+` AND next_attempt_at > now() AND attempts = 1 AND last_error IS NOT NULL
		FROM outbox_events WHERE aggregate_id = $1`, b.ID)
	if !waiting {
		t.Fatal("failed row must be pending with backoff and error")
	}
	if n, err := ob.Flush(ctx); err != nil || n != 0 {
		t.Fatalf("row inside backoff window must not be claimed, got %d (err %v)", n, err)
	}

	// A broker outage (whole-batch failure) only backs off: it never poisons rows, however long.
	brokerDown := errors.New("broker down")
	bus.update(func(r *busRules) { r.batch = brokerDown })
	for range 4 {
		mustExec(t, pool, `UPDATE outbox_events SET next_attempt_at = now() WHERE aggregate_id = $1`, b.ID)
		if _, err := ob.Flush(ctx); !errors.Is(err, brokerDown) {
			t.Fatalf("whole-batch failure must surface the broker error, got %v", err)
		}
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, b.ID); got != "pending" {
		t.Fatalf("b after broker outage = %s, want pending", got)
	}

	// After the outage `attempts` is high, but only real rejections count toward poisoning:
	// one more transient rejection (2 of 3) keeps the row alive…
	bus.update(func(r *busRules) { r.batch = nil })
	mustExec(t, pool, `UPDATE outbox_events SET next_attempt_at = now() WHERE aggregate_id = $1`, b.ID)
	if _, err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, b.ID); got != "pending" {
		t.Fatalf("b after 2nd rejection = %s, want pending (outage must not count)", got)
	}
	// …and the third rejection poisons it.
	mustExec(t, pool, `UPDATE outbox_events SET next_attempt_at = now() WHERE aggregate_id = $1`, b.ID)
	if _, err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, b.ID); got != "failed" {
		t.Fatalf("b after 3rd rejection = %s, want failed", got)
	}
}

// Bookkeeping is fenced by the claim: a worker whose lease expired cannot overwrite the row
// another worker re-claimed.
func TestOutboxMarksAreFencedByClaim(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	item := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	first, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v", err)
	}
	// Lease expires; another worker reclaims and claims again.
	mustExec(t, pool, `UPDATE outbox_events SET claimed_at = now() - interval '1 minute' WHERE aggregate_id = $1`, item.ID)
	if _, err := st.ReclaimStaleOutbox(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(second) != 1 {
		t.Fatalf("reclaim+claim: %v", err)
	}
	// The stale worker reports failure: it must not touch the row now leased to the second.
	if err := st.MarkOutboxRetryAfter(ctx, []uuid.UUID{first[0].ID}, first[0].ClaimedAt, "late", 1, 0); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, item.ID); got != "processing" {
		t.Fatalf("stale mark changed the row to %s", got)
	}
	if err := st.MarkOutboxPublished(ctx, []uuid.UUID{second[0].ID}, second[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE aggregate_id = $1`, item.ID); got != "published" {
		t.Fatalf("current lease holder could not publish: %s", got)
	}
}

// Regression: reclaim used created_at, so old backlog rows being published right now were stolen
// by another instance (duplicate publish). The lease must be measured from claimed_at.
func TestOutboxLeaseUsesClaimTime(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	item := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	mustExec(t, pool, `UPDATE outbox_events SET created_at = now() - interval '1 hour' WHERE aggregate_id = $1`, item.ID)

	claimed, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %d", err, len(claimed))
	}
	if n, err := st.ReclaimStaleOutbox(ctx, 30*time.Second); err != nil || n != 0 {
		t.Fatalf("fresh lease on an old row was reclaimed (%d) (err %v)", n, err)
	}
	mustExec(t, pool, `UPDATE outbox_events SET claimed_at = now() - interval '1 minute' WHERE aggregate_id = $1`, item.ID)
	if n, err := st.ReclaimStaleOutbox(ctx, 30*time.Second); err != nil || n != 1 {
		t.Fatalf("expired lease must be reclaimed, got %d (err %v)", n, err)
	}
}

func TestOutboxPurgeAndNotify(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	mustExec(t, pool, `UPDATE outbox_events SET status = `+code(domain.OutboxPublished)+`, published_at = now() - interval '30 days'`)
	n, err := st.PurgePublishedOutbox(ctx, 7*24*time.Hour, 1000)
	if err != nil || n == 0 {
		t.Fatalf("purge n=%d err=%v", n, err)
	}

	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.RunOutboxNotifier(listenCtx, nil) // NOTIFY is sent after commit, outside the booking TX
	woke := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- st.ListenOutbox(listenCtx, func() {
			select {
			case woke <- struct{}{}:
			default:
			}
		})
	}()
	// LISTEN is issued asynchronously; book until a notification arrives.
	deadline := time.After(5 * time.Second)
	vehicles := []uuid.UUID{seed.VehicleA, seed.VehicleB, seed.VehicleC}
	for i := 0; ; i++ {
		if i < len(vehicles) {
			if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, vehicles[i], londonAt(t, 14+i, 0))); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-woke:
			cancel()
			<-done
			return
		case <-deadline:
			t.Fatal("NOTIFY did not wake the listener")
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// The outbox row is written in the booking's own TX (invariant 7): when that insert fails, the
// booking fails with it and leaves nothing behind — no appointment, no lines, no history, no key —
// and the same request (same Idempotency-Key) books normally once the outbox accepts rows again.
func TestOutboxFailureRollsBackBooking(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	vehicle := newVehicle(t, svc)
	req := oilAt(seed.DealerRiver, vehicle, londonOn(t, londonDay(t, 1), 9, 0))
	const refusal = "outbox insert refused by test"
	mustExec(t, pool, `
		CREATE FUNCTION refuse_outbox_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION '`+refusal+`'; END $$`)
	mustExec(t, pool, `CREATE TRIGGER refuse_outbox_insert BEFORE INSERT ON outbox_events
		FOR EACH ROW EXECUTE FUNCTION refuse_outbox_insert()`)

	_, _, err := svc.Book(ctx, staffAdmin, req)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Message != refusal {
		t.Fatalf("book with the outbox refusing rows: %v, want the outbox insert's error", err)
	}
	left := scalar[int](t, pool, `
		SELECT (SELECT count(*) FROM appointments WHERE vehicle_id = $1)
		     + (SELECT count(*) FROM appointment_lines l JOIN appointments a ON a.id = l.appointment_id WHERE a.vehicle_id = $1)
		     + (SELECT count(*) FROM appointment_history h JOIN appointments a ON a.id = h.appointment_id WHERE a.vehicle_id = $1)
		     + (SELECT count(*) FROM appointments WHERE idempotency_key = $2)
		     + (SELECT count(*) FROM appointment_history h WHERE NOT EXISTS (SELECT 1 FROM appointments a WHERE a.id = h.appointment_id))`,
		vehicle, req.IdempotencyKey)
	if left != 0 {
		t.Fatalf("%d rows left behind by the failed booking (appointment, lines, history, key)", left)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE dealership_id = $1 AND start_at = $2`,
		seed.DealerRiver, req.StartAt); n != 0 {
		t.Fatalf("%d appointment(s) in the slot after the failed booking", n)
	}

	mustExec(t, pool, `DROP TRIGGER refuse_outbox_insert ON outbox_events`)
	item, created, err := svc.Book(ctx, staffAdmin, req)
	if err != nil || !created {
		t.Fatalf("the same booking once the outbox accepts rows: created=%v err=%v", created, err)
	}
	assertEventsFor(t, pool, item.ID)
}
