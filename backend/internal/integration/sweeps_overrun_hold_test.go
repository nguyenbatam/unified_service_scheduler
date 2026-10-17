package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A sweep that runs late cannot add a whole step (the car's next booking, at another dealership,
// starts inside it), so it extends up to that booking instead of leaving the job's end where it was: the bay
// and technician stay held while the car is on the lift. The job is flagged once; the next sweep
// finds nothing more to do.
func TestOverrunSweepExtendsUpToTheNextBooking(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	vehicle := newVehicle(t, svc)
	job, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	// The car's next booking is at another dealership: only the vehicle blocks the extension.
	other, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	if _, err := bookAt(t, svc, other, vehicle, seed.ServiceOil, londonOn(t, today, 10, 0)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+` WHERE id = $1`, job.ID)

	// 10:10: the next whole step ends 10:15, past the 10:00 booking.
	sweeper := serviceOn(t, st, today, 10, 10)
	extended, flagged, err := sweeper.HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 0 || flagged != 1 {
		t.Fatalf("sweep = extended %d flagged %d err %v, want 0/1", extended, flagged, err)
	}
	got := mustGet(t, svc, job.ID)
	if !got.EndAt.Equal(londonOn(t, today, 10, 0)) || got.OverrunAt == nil {
		t.Fatalf("job = end %s overrunAt %v, want 10:00 and flagged", got.EndAt.In(got.StartAt.Location()), got.OverrunAt)
	}
	wantEvents(t, "partial extension", pool, job.ID, domain.EventAppointmentExtended, 1)
	wantEvents(t, "flag", pool, job.ID, domain.EventAppointmentOverrunning, 1)

	extended, flagged, err = serviceOn(t, st, today, 10, 25).HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 0 || flagged != 0 {
		t.Fatalf("next sweep = extended %d flagged %d err %v, want 0/0 (already flagged, no room)", extended, flagged, err)
	}
	// Still blocked right at its end, it does not queue on the day lock every tick: a booking
	// holding that day does not make the sweep wait out its lock_timeout.
	writer, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if err := writer.LockDealershipDay(ctx, seed.DealerRiver, uuid.Nil, job.StartAt, job.StartAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	extended, flagged, err = serviceOn(t, st, today, 10, 40).HandleOverruns(ctx, 15*time.Minute)
	if took := time.Since(began); err != nil || extended != 0 || flagged != 0 || took > 250*time.Millisecond {
		t.Fatalf("blocked sweep while the day is held = extended %d flagged %d err %v in %s, want 0/0 at once (no day lock)", extended, flagged, err, took)
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Flagged overrunning work keeps its bay and technician until it is completed: its end_at no
// longer says when the car leaves the lift, so neither availability nor a booking offers the slot
// after it. Completing it gives the bay back (the next day: TestFlaggedOverrunCarriesOverToTheNextDay).
func TestFlaggedOverrunHoldsItsBayForTheDay(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	// A shop whose only bay is the express lane: an oil change can go nowhere else (no general bay
	// to spill into). A second lubrication technician keeps the technician side free, so only the
	// bay hold can refuse.
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress},
		[]policyTech{{name: "Aaron", level: 1, skills: lube}, {name: "Bea", level: 1, skills: lube}})
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now() WHERE id = $1`, job.ID)

	clock := serviceOn(t, st, today, 9, 40)
	avail, err := clock.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: dealer, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: today,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range avail.Slots {
		if !s.Start.Before(londonOn(t, today, 10, 0)) && s.Available {
			t.Fatalf("slot %s offered while the quick lane still holds a flagged overrun", s.Start)
		}
	}
	_, err = bookAt(t, clock, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 11, 0))
	expectCode(t, err, domain.CodeUnavailable)

	if _, err := clock.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := bookAt(t, clock, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 11, 0)); err != nil {
		t.Fatalf("after completion: %v", err)
	}
	// Another day is not affected.
	if _, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, londonDay(t, 1), 9, 0)); err != nil {
		t.Fatalf("next day: %v", err)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// The technician side of the same hold: with a bay to spare, a flagged overrun on the only
// qualified technician still takes him until it is completed.
func TestFlaggedOverrunHoldsItsTechnicianForTheDay(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral}, []policyTech{{name: "Aaron", level: 2, skills: brakes}})
	job, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, today, 9, 0), false) // 09:00–10:30
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now() WHERE id = $1`, job.ID)

	clock := serviceOn(t, st, today, 10, 40)
	avail, err := clock.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: dealer, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, Day: today,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range avail.Slots {
		if s.Available {
			t.Fatalf("slot %s offered while the only brake technician is on a flagged overrun (free bays %d)", s.Start, s.FreeBays)
		}
	}
	_, err = policyBook(ctx, t, clock, dealer, seed.ServiceBrake, londonOn(t, today, 13, 0), false)
	expectCode(t, err, domain.CodeUnavailable)
	if _, err := clock.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := policyBook(ctx, t, clock, dealer, seed.ServiceBrake, londonOn(t, today, 13, 0), false); err != nil {
		t.Fatalf("after completion: %v", err)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
