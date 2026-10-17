package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Riverside has one express bay and one lubrication technician, so an oil slot holds one car.
// An in-place reschedule (its own hold excluded) and a new booking race for the same slot. Both
// queue on the dealership-day lock: each order is pinned once by holding it while they arrive.
func TestRescheduleRacingBookingOneWins(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		a, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 8, 0)))
		if err != nil {
			t.Fatal(err)
		}
		target := londonOn(t, day, 10, 0)
		var moveErr, bookErr error
		var b domain.Appointment
		window := dayGate(seed.DealerRiver, target)
		runOrdered(t, pool, order, raceSide{
			run:  func() { _, moveErr = svc.Reschedule(ctx, staffAdmin, a.ID, booking.RescheduleRequest{StartAt: target}) },
			gate: window, queued: window.marker,
		}, raceSide{
			run:  func() { b, _, bookErr = svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, target)) },
			gate: window, queued: window.marker,
		})
		w := oneWinner(t, day.String(), loses(moveErr, refusals...), loses(bookErr, refusals...))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the reschedule", "the booking"})
		wantEvents(t, day.String(), pool, a.ID, domain.EventAppointmentRescheduled, boolInt(moveErr == nil))
		if bookErr == nil {
			assertEventsFor(t, pool, b.ID)
		}
		holds := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE dealership_id = $1 AND status IN (`+holdingCodes+`) AND start_at = $2`, seed.DealerRiver, target)
		if holds != 1 {
			t.Fatalf("%s: %d holds at 10:00", day, holds)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// Starting a job early pulls it into an earlier window; a booking for that window races it. Both
// queue on the dealership-day lock: the first two rounds pin each order, the third runs free. Fresh
// cars each round: the seed holds VehicleA and VehicleB at Northside this morning, and a walk-in
// with a car busy elsewhere would lose on the vehicle rule before ever racing for the slot.
func TestStartEarlyRacingBookingOneWins(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i, hour := range []int{10, 11, 12} {
		order := raceOrders[i]
		job, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, newVehicle(t, svc), londonAt(t, 14+i, 0)))
		if err != nil {
			t.Fatal(err)
		}
		clock := serviceOn(t, st, londonDay(t, 0), hour, 0)
		if _, err := clock.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
			t.Fatal(err)
		}
		var startErr, bookErr error
		var walkIn domain.Appointment
		walkInCar := newVehicle(t, svc)
		window := dayGate(seed.DealerRiver, londonAt(t, hour, 0))
		runOrdered(t, pool, order, raceSide{
			run:  func() { _, startErr = clock.StartWork(ctx, staffDispatcher, job.ID, 0) },
			gate: window, queued: window.marker,
		}, raceSide{
			run: func() {
				walkIn, _, bookErr = clock.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, walkInCar, londonAt(t, hour, 0)))
			},
			gate: window, queued: window.marker,
		})
		label := fmt.Sprintf("early start %02d:00", hour)
		w := oneWinner(t, label, loses(startErr, refusals...), loses(bookErr, refusals...))
		wantOrderWinner(t, label, order, w, [2]string{"the early start", "the walk-in"})
		// Recount in Postgres: exactly one hold covers hh:00 on Riverside's single express bay.
		holds := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE service_bay_id = $1 AND status IN (`+holdingCodes+`)
			  AND tstzrange(start_at, end_at) && tstzrange($2, $3)`,
			seed.BayR1, londonAt(t, hour, 0), londonAt(t, hour, 30))
		if holds != 1 {
			t.Fatalf("%02d:00: %d holds on the express bay, want 1", hour, holds)
		}
		wantEvents(t, fmt.Sprintf("%02d:00", hour), pool, job.ID, domain.EventAppointmentStarted, boolInt(startErr == nil))
		if bookErr == nil {
			assertEventsFor(t, pool, walkIn.ID)
		}
		assertNoOverlaps(t, pool)
		assertAllocationsValid(t, pool)
		if startErr == nil {
			if _, err := clock.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Work already on the lift (even overrunning past the shift end) does not block roster edits.
// Tomorrow, so the job is still in the future for the database's now() used by the check.
func TestRosterEditAllowedWhileJobOverruns(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	tomorrow := londonDay(t, 1)
	roster, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: tomorrow.ISOWeekday(), StartTime: "08:00", EndTime: "10:30",
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, tomorrow, 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	at10 := booking.New(st, nil, booking.WithClock(func() time.Time { return londonOn(t, tomorrow, 10, 0) }))
	if _, err := at10.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := at10.StartWork(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Extend(ctx, staffDispatcher, job.ID, 15, 0); err != nil { // now ends 10:45, past her shift
		t.Fatal(err)
	}
	far := londonDay(t, 20)
	if _, err := svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonOn(t, far, 8, 0), EndsAt: londonOn(t, far, 18, 0), Reason: "Leave",
	}, roster.Version); err != nil {
		t.Fatalf("an overrunning in-progress job must not block unrelated roster edits: %v", err)
	}
}
