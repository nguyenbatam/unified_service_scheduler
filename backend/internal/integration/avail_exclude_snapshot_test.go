package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The reschedule view is computed for the booked snapshot, as Reschedule moves it: after the
// catalog doubles the brake job's labour time and raises its level past the shop's only brake
// technician, a 90-minute brake job booked before the edit still moves anywhere in the day. A grid
// built from the edited catalog would offer nothing (no technician qualifies) and a 180-minute grid
// would hide the late starts the move accepts. The starts the grid offers are ones Reschedule
// takes (the shop's only job is the one moving, so each stays valid after the previous move).
func TestRescheduleAvailabilityUsesBookedSnapshot(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	d := provisionShop(t, svc, "Snapshot")
	addBay(t, svc, d.ID, "General", domain.BayGeneral)
	addTechnician(t, svc, d.ID, "Brakes", 2, domain.SkillBrakes)
	appt := mustBook(t, svc, booking.BookRequest{
		DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake,
		StartAt: londonOn(t, day, 10, 0), IdempotencyKey: uuid.NewString(),
	})
	longer, level := 180, domain.LevelSenior
	if _, err := svc.UpdateServiceType(ctx, staffAdmin, seed.ServiceBrake, store.ServiceTypePatch{DurationMinutes: &longer, MinSkillLevel: &level}); err != nil {
		t.Fatal(err)
	}

	q := booking.AvailabilityQuery{
		DealershipID: d.ID, VehicleID: appt.VehicleID, ServiceTypeID: seed.ServiceBrake, Day: day, ExcludeAppointmentID: appt.ID,
	}
	grid, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if grid.DurationMinutes != 90 {
		t.Fatalf("reschedule grid for %d minutes, want the booked 90", grid.DurationMinutes)
	}
	var offered []time.Time
	for _, slot := range grid.Slots {
		if slot.End.Sub(slot.Start) != 90*time.Minute {
			t.Fatalf("slot %s–%s is not the booked 90 minutes", slot.Start, slot.End)
		}
		if slot.Available {
			offered = append(offered, slot.Start)
		}
	}
	// The shop's only job is the one moving: every start from 08:00 to 16:30 fits it.
	if want := 18; len(offered) != want || !offered[len(offered)-1].Equal(londonOn(t, day, 16, 30)) {
		t.Fatalf("offered %d starts (last %v), want %d through 16:30", len(offered), offered, want)
	}
	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: day, Days: 1, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Slots) != len(offered) || next.DurationMinutes != 90 {
		t.Fatalf("next available: %d slots of %d minutes, want %d of 90", len(next.Slots), next.DurationMinutes, len(offered))
	}

	// What the grid offers, Reschedule takes: the last start (outside a 180-minute grid), the first,
	// and one in between.
	for _, start := range []time.Time{offered[len(offered)-1], offered[0], londonOn(t, day, 12, 0)} {
		cur := mustGet(t, svc, appt.ID)
		moved, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: start, IfMatch: cur.Version})
		if err != nil {
			t.Fatalf("reschedule to offered %s: %v", start.Format("15:04"), err)
		}
		if moved.DurationMinutes != 90 || moved.MinSkillLevel != 2 || moved.EndAt.Sub(moved.StartAt) != 90*time.Minute {
			t.Fatalf("reschedule re-priced the job: %+v", moved)
		}
	}

	// The lines asked for must be the booked ones: another job is a new booking, not a move.
	q.ServiceTypeID, q.ServiceTypeIDs = uuid.Nil, []uuid.UUID{seed.ServiceBrake, seed.ServiceOil}
	_, err = svc.Availability(ctx, staffAdmin, q)
	expectCode(t, err, domain.CodeInvalid)
	// Without the exclusion the same request is a new booking, priced from the catalog as it is now.
	q.ServiceTypeIDs, q.ServiceTypeID, q.ExcludeAppointmentID = nil, seed.ServiceBrake, uuid.Nil
	fresh, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DurationMinutes != 180 {
		t.Fatalf("new booking grid for %d minutes, want the catalog's 180", fresh.DurationMinutes)
	}
	for _, slot := range fresh.Slots {
		if slot.Available {
			t.Fatalf("new booking offered %s, but no technician holds the catalog's level 3", slot.Start)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
