package integration_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A client that times out and resends while its original request is still in flight must get
// the original booking back (200 replay), not a 409 from colliding with itself.
func TestConcurrentRetrySameKeyReplays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		dealer uuid.UUID // Northside has staging capacity (advisory lock path); Riverside does not
	}{
		{"with staging capacity", seed.DealerNorth},
		{"without staging capacity", seed.DealerRiver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // each subtest has a database of its own
			pool, _, svc, _ := openTestDB(t)
			req := booking.BookRequest{
				DealershipID: tc.dealer, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
				StartAt: londonAt(t, 14, 0), IdempotencyKey: "retry-" + uuid.NewString(),
			}
			id := raceSameKey(t, svc, req, 6)
			assertEventsFor(t, pool, id)
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}

// Redis is only a cache: when it is unreachable, keyed bookings still work off Postgres.
func TestBookingSurvivesRedisOutage(t *testing.T) {
	t.Parallel()
	st, _, ctx := openTestStore(t)
	dead, err := cache.New("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	day, loc := suiteToday()
	svc := booking.New(st, cache.NewIdempotency(dead), booking.WithClock(func() time.Time { return day.Start(loc) }))
	req := oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0))
	first, created, err := svc.Book(ctx, staffAdmin, req)
	if err != nil || !created {
		t.Fatalf("book with Redis down: %v", err)
	}
	again, created, err := svc.Book(ctx, staffAdmin, req)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay with Redis down: created=%v err=%v", created, err)
	}
}

// An admin deactivating the only qualified technician while a booking is mid-flight must not
// leave an appointment assigned to an inactive technician.
func TestBookingRacingTechnicianDeactivation(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	admin, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Rollback(ctx) }()
	// Riverside oil needs lubrication: only Eve qualifies. Lock and deactivate her, uncommitted.
	if _, err := admin.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, seed.TechEve); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE technicians SET active = false WHERE id = $1`, seed.TechEve); err != nil {
		t.Fatal(err)
	}

	type result struct {
		item    domain.Appointment
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		item, created, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
		done <- result{item, created, err}
	}()
	// The booking sees Eve as free and active in its snapshot, then blocks on her row (FOR SHARE).
	// Commit only once it is really waiting there, so the test exercises the post-wait re-check.
	awaitLockWaiter(t, pool, "FROM technicians")
	if err := admin.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.created {
		active := scalar[bool](t, pool, `SELECT active FROM technicians WHERE id = $1`, r.item.TechnicianID)
		t.Fatalf("booked on technician %s (active=%v) after deactivation", r.item.TechnicianName, active)
	}
	expectCode(t, r.err, domain.CodeUnavailable)
	assertNoOrphanEvents(t, pool)
}

// Two dispatchers moving the same appointment at once: exactly one move wins.
func TestConcurrentReschedulesOfSameAppointment(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	orig, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	targets := []time.Time{londonAt(t, 15, 0), londonAt(t, 16, 0)}
	errs := make([]error, len(targets))
	raceN(len(targets), func(i int) {
		_, errs[i] = svc.Reschedule(ctx, staffDispatcher, orig.ID, booking.RescheduleRequest{StartAt: targets[i], IfMatch: orig.Version})
	})
	oneWinner(t, "concurrent moves", allLose(errs, domain.CodeConflict, domain.CodePrecondition)...)
	holding, _, err := svc.ListAppointmentsPage(ctx, staffAdmin, booking.AppointmentQuery{VehicleID: seed.VehicleA})
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, a := range holding {
		if a.Status.HoldsResources() && a.DealershipID == seed.DealerRiver {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("holding Riverside appointments for the car = %d, want 1", active)
	}
	// The loser left no trace: one move recorded (outbox + audit), version bumped once.
	if outbox, history := eventCount(t, pool, orig.ID, domain.EventAppointmentRescheduled); outbox != 1 || history != 1 {
		t.Fatalf("APPOINTMENT_RESCHEDULED outbox=%d history=%d, want 1 and 1", outbox, history)
	}
	if v := scalar[int](t, pool, `SELECT version FROM appointments WHERE id = $1`, orig.ID); v != orig.Version+1 {
		t.Fatalf("version = %d, want %d", v, orig.Version+1)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Even the service layer refuses admin changes from non-admins (not just route middleware).
func TestAdminChangesRequireAdminInService(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	_, _, err := svc.ListTechniciansAdmin(ctx, staffDispatcher, uuid.Nil, 0, nil)
	expectCode(t, err, domain.CodeForbidden)
	_, err = svc.UpdateDealership(ctx, customerAmelia, seed.DealerNorth, storePatchStaging(4))
	expectCode(t, err, domain.CodeForbidden)
	updated, err := svc.UpdateDealership(ctx, staffAdmin, seed.DealerNorth, storePatchStaging(4))
	if err != nil || updated.StagingCapacity != 4 || updated.Today == "" {
		t.Fatalf("admin update: %+v %v", updated, err)
	}
}

// raceSameKey sends n concurrent copies of req (one key, one body) as Amelia: exactly one creates
// the appointment and every copy returns it (the others replay). It returns the appointment id.
func raceSameKey(t *testing.T, svc *booking.Service, req booking.BookRequest, n int) uuid.UUID {
	t.Helper()
	var created atomic.Int32
	ids := make([]uuid.UUID, n)
	raceN(n, func(i int) {
		item, ok, err := svc.Book(t.Context(), customerAmelia, req)
		if err != nil {
			t.Errorf("attempt %d: %v", i, err)
			return
		}
		if ok {
			created.Add(1)
		}
		ids[i] = item.ID
	})
	if created.Load() != 1 {
		t.Fatalf("created=%d want 1", created.Load())
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("attempt %d got %s, want the same appointment %s", i, id, ids[0])
		}
	}
	return ids[0]
}
