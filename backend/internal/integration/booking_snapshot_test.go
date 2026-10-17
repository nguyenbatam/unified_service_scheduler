package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/testdb"
)

// testRedis connects to TEST_REDIS_URL (skipped without it, failed under REQUIRE_INTEGRATION=1;
// never a default Redis, which may be the dev stack's). Tests share that Redis database, so every
// key they write carries a fresh uuid.
func testRedis(t *testing.T) *cache.Idempotency {
	t.Helper()
	client, err := cache.New(testdb.RedisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(t.Context()); err != nil {
		t.Fatalf("redis: %v", err)
	}
	return cache.NewIdempotency(client)
}

// A replay is an appointment read: once the creator's scope no longer covers the appointment
// (a dispatcher moved to another dealership), retrying the key is a 404 on both the Redis fast
// path and the Postgres path, never the other dealership's booking.
func TestIdempotentReplayRespectsScope(t *testing.T) {
	t.Parallel()
	st, _, ctx := openTestStore(t)
	day, loc := suiteToday()
	clock := booking.WithClock(func() time.Time { return day.Start(loc) })
	withRedis := booking.New(st, testRedis(t), clock)
	postgresOnly := booking.New(st, nil, clock)

	req := oilAt(seed.DealerNorth, seed.VehicleB, londonOn(t, londonDay(t, 1), 11, 0))
	req.IdempotencyKey = "scope-" + uuid.NewString()
	first, created, err := withRedis.Book(ctx, northDispatcher, req)
	if err != nil || !created {
		t.Fatalf("book: created=%v err=%v", created, err)
	}
	if again, created, err := withRedis.Book(ctx, northDispatcher, req); err != nil || created || again.ID != first.ID {
		t.Fatalf("same scope must replay: created=%v err=%v", created, err)
	}

	moved := northDispatcher
	moved.DealershipID = seed.DealerRiver
	for name, svc := range map[string]*booking.Service{"redis fast path": withRedis, "postgres": postgresOnly} {
		t.Run(name, func(t *testing.T) {
			_, _, err := svc.Book(ctx, moved, req)
			expectCode(t, err, domain.CodeNotFound)
		})
	}
	// A different payload under the same key is still 404, not a 409 that confirms the key exists.
	other := req
	other.StartAt = londonOn(t, londonDay(t, 1), 11, 30)
	_, _, err = postgresOnly.Book(ctx, moved, other)
	expectCode(t, err, domain.CodeNotFound)
}

// Job lines come back in the caller's order (line 1 = primary) whatever the id order, one read
// for all lines; an unknown line is 404. Reschedule moves the window and never rewrites the
// booked lines or snapshot.
func TestMultiLineOrderAndRescheduleKeepsSnapshot(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	// Oil sorts before Major by id; the caller lists Major first.
	ids := []uuid.UUID{seed.ServiceMajor, seed.ServiceOil}
	appt, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeIDs: ids, StartAt: londonOn(t, day, 8, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(appt.Lines) != 2 || appt.Lines[0].ServiceTypeID != seed.ServiceMajor || appt.Lines[1].ServiceTypeID != seed.ServiceOil ||
		appt.ServiceTypeID != seed.ServiceMajor || appt.DurationMinutes != 210 {
		t.Fatalf("lines = %+v primary=%s duration=%d", appt.Lines, appt.ServiceTypeID, appt.DurationMinutes)
	}
	_, _, err = svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, StartAt: londonOn(t, day, 14, 0),
		ServiceTypeIDs: []uuid.UUID{seed.ServiceOil, uuid.New()},
	})
	expectCode(t, err, domain.CodeNotFound)

	moved, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: londonOn(t, day, 9, 0), IfMatch: appt.Version})
	if err != nil {
		t.Fatal(err)
	}
	if !moved.StartAt.Equal(londonOn(t, day, 9, 0)) || moved.Version != appt.Version+1 ||
		moved.DurationMinutes != appt.DurationMinutes || moved.ServiceTypeID != appt.ServiceTypeID ||
		moved.RequiredBayType != appt.RequiredBayType || moved.MinSkillLevel != appt.MinSkillLevel ||
		fmt.Sprint(moved.Lines) != fmt.Sprint(appt.Lines) || fmt.Sprint(moved.RequiredSkills) != fmt.Sprint(appt.RequiredSkills) {
		t.Fatalf("reschedule changed the snapshot:\n before %+v\n after  %+v", appt, moved)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_lines WHERE appointment_id = $1`, appt.ID); n != 2 {
		t.Fatalf("lines after reschedule = %d", n)
	}
	// Each change recorded its event and audit row in the same TX.
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE appointment_id = $1`, appt.ID); n != 2 {
		t.Fatalf("history rows = %d, want 2", n)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1`, appt.ID); n != 2 {
		t.Fatalf("outbox rows = %d, want 2", n)
	}
}

// Concurrent bookings racing for the same slot fill each bay and technician at most once. With
// two general bays and two brake technicians at Northside, three cars → exactly two winners on
// distinct bays and technicians, whatever order the bay collisions happen in.
func TestConcurrentBookingsFillEachBayOnce(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	vehicles := []uuid.UUID{seed.VehicleA, seed.VehicleB, seed.VehicleC}
	for offset := 2; offset <= 6; offset++ {
		start := londonOn(t, londonDay(t, offset), 14, 0)
		results := make([]domain.Appointment, len(vehicles))
		errs := make([]error, len(vehicles))
		raceN(len(vehicles), func(i int) {
			results[i], _, errs[i] = svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, vehicles[i], start))
		})
		bays, techs := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
		for _, i := range expectWinners(t, fmt.Sprintf("day +%d", offset), 2, allLose(errs, refusals...)...) {
			if bays[results[i].ServiceBayID] || techs[results[i].TechnicianID] {
				t.Fatalf("day +%d: bay or technician double-booked: %+v", offset, results)
			}
			bays[results[i].ServiceBayID], techs[results[i].TechnicianID] = true, true
			assertEventsFor(t, pool, results[i].ID)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertStagingWithinCapacity(t, pool)
}
