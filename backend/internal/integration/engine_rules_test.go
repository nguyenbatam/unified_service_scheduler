package integration_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A unique violation from the insert is returned as it is (here: a test-only index), never turned
// into a replay lookup that answers 404: a same-key race never reaches the idempotency index (the
// per-key advisory lock makes the second request replay first), and nothing maps 23505 to a replay.
func TestUniqueViolationIsNotAReplay(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	if _, err := pool.Exec(ctx, `
		CREATE UNIQUE INDEX engine_test_vehicle_once ON appointments (vehicle_id)
		WHERE idempotency_key LIKE 'engine-idem:%'`); err != nil {
		t.Fatal(err)
	}
	first := oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 14, 0))
	first.IdempotencyKey = "engine-idem:1"
	if _, created, err := svc.Book(ctx, staffAdmin, first); err != nil || !created {
		t.Fatalf("first booking: %v", err)
	}
	second := oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 16, 0))
	second.IdempotencyKey = "engine-idem:2"
	_, _, err := svc.Book(ctx, staffAdmin, second)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "engine_test_vehicle_once" {
		t.Fatalf("want the unique violation of engine_test_vehicle_once unchanged, got %v", err)
	}

	// The real idempotency index still replays: same user, key, and payload.
	again, created, err := svc.Book(ctx, staffAdmin, first)
	if err != nil || created || again.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
}

// Config changes only refuse to strand holding appointments, so a no-show's day can be shortened
// afterwards; reinstating it must then re-check the calendar instead of re-holding a window the
// dealership is no longer open for.
func TestReinstateRechecksTheDaysHours(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	appt, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	late := serviceOn(t, st, londonDay(t, 0), 14, 10)
	if _, err := late.NoShow(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	day, _ := suiteToday()
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: day.ISOWeekday(), OpenTime: "08:00", CloseTime: "13:00"},
	}, 0); err != nil {
		t.Fatalf("shorten today (the no-show holds nothing): %v", err)
	}
	_, err = late.Reinstate(ctx, staffDispatcher, appt.ID, 0)
	expectCode(t, err, domain.CodeUnavailable)

	// A lunch break added over the window refuses it too.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: day.ISOWeekday(), OpenTime: "08:00", CloseTime: "18:00", LunchStart: "14:00", LunchEnd: "15:00"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	_, err = late.Reinstate(ctx, staffDispatcher, appt.ID, 0)
	expectCode(t, err, domain.CodeUnavailable)

	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{}, 0); err != nil {
		t.Fatal(err)
	}
	back, err := late.Reinstate(ctx, staffDispatcher, appt.ID, 0)
	if err != nil || back.Status != domain.StatusCheckedIn {
		t.Fatalf("reinstate inside the restored hours: %v %v", back.Status, err)
	}
}

// The no-show sweep writes its events set-based: exactly one outbox row and one history row per
// flipped appointment, matching its new version. (Seq order against the booking's CREATED event is
// not asserted: those rows were inserted by earlier, already committed transactions, so a later
// seq holds for any implementation; the lock-then-draw ordering only matters for concurrent
// writers of one aggregate, which this sequential test cannot observe.)
func TestNoShowSweepEventsOnePerAppointment(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	// Riverside's single express bay, early slots; the seed adds Northside 09:00 and two 10:00 jobs.
	for _, b := range []struct {
		vehicle uuid.UUID
		hour    int
		minute  int
	}{{seed.VehicleA, 8, 0}, {seed.VehicleB, 8, 30}, {seed.VehicleC, 9, 0}} {
		if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, b.vehicle, londonAt(t, b.hour, b.minute))); err != nil {
			t.Fatal(err)
		}
	}
	confirmed := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE status = `+code(domain.StatusConfirmed))
	n, err := serviceOn(t, st, londonDay(t, 0), 12, 0).MarkNoShows(ctx)
	if err != nil || n != confirmed || n != 6 {
		t.Fatalf("swept %d of %d confirmed (want 6): %v", n, confirmed, err)
	}
	if bad := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		WHERE a.status = `+code(domain.StatusNoShow)+` AND (
		   (SELECT count(*) FROM outbox_events o WHERE o.aggregate_id = a.id AND o.event_type = `+code(domain.EventAppointmentNoShow)+`) <> 1
		OR (SELECT count(*) FROM appointment_history h
		    WHERE h.appointment_id = a.id AND h.event_type = `+code(domain.EventAppointmentNoShow)+`
		      AND h.version = a.version AND h.status = `+code(domain.StatusNoShow)+` AND h.actor_id IS NULL) <> 1
		OR (SELECT (o.payload->>'version')::int FROM outbox_events o
		    WHERE o.aggregate_id = a.id AND o.event_type = `+code(domain.EventAppointmentNoShow)+`) <> a.version)`); bad != 0 {
		t.Fatalf("%d swept appointment(s) without exactly one outbox + history row at the new version", bad)
	}
	if again, err := serviceOn(t, st, londonDay(t, 0), 12, 0).MarkNoShows(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep marked %d: %v", again, err)
	}
}
