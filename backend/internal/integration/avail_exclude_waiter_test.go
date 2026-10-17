package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The reschedule view follows the move's waiter flag: left out, it is the booked one (Reschedule
// keeps it when its body leaves isWaiter out), so a start reachable only by the waiter's soft
// upgrade to the quick lane is not offered to a booked non-waiter; asked for, it is the flag the
// move will carry.
func TestRescheduleAvailabilityKeepsBookedWaiterFlag(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	d := provisionShop(t, svc, "Waiter flag")
	limit := int16(90)
	if _, err := svc.UpdateDealership(ctx, staffAdmin, d.ID, store.DealershipPatch{ExpressMaxMinutes: &limit}); err != nil {
		t.Fatal(err)
	}
	addBay(t, svc, d.ID, "General", domain.BayGeneral)
	addBay(t, svc, d.ID, "Lane", domain.BayExpress)
	addTechnician(t, svc, d.ID, "Brakes A", 2, domain.SkillBrakes)
	addTechnician(t, svc, d.ID, "Brakes B", 2, domain.SkillBrakes)
	appt := mustBook(t, svc, booking.BookRequest{
		DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake,
		StartAt: londonOn(t, day, 9, 0), IdempotencyKey: uuid.NewString(),
	})
	// The general bay is taken at noon; only the quick lane is free then.
	mustBook(t, svc, booking.BookRequest{
		DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake,
		StartAt: londonOn(t, day, 12, 0), IdempotencyKey: uuid.NewString(),
	})
	noon := londonOn(t, day, 12, 0)
	offeredAtNoon := func(q booking.AvailabilityQuery) bool {
		t.Helper()
		grid, err := svc.Availability(ctx, staffAdmin, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, slot := range grid.Slots {
			if slot.Start.Equal(noon) {
				return slot.Available
			}
		}
		t.Fatalf("no 12:00 slot in %+v", grid.Slots)
		return false
	}
	q := booking.AvailabilityQuery{
		DealershipID: d.ID, VehicleID: appt.VehicleID, ServiceTypeID: seed.ServiceBrake, Day: day, ExcludeAppointmentID: appt.ID,
	}

	// No flag given: the booked non-waiter, whom the quick lane does not take.
	if offeredAtNoon(q) {
		t.Fatal("12:00 offered to a booked non-waiter with only the quick lane free")
	}
	_, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: noon, IfMatch: appt.Version})
	expectCode(t, err, domain.CodeUnavailable)

	// Asked for as a waiter: offered, and the move that carries the flag takes it.
	q.IsWaiter, q.WaiterGiven = true, true
	if !offeredAtNoon(q) {
		t.Fatal("12:00 not offered to the job moved as a waiter")
	}
	waiter := true
	moved, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: noon, IsWaiter: &waiter, IfMatch: appt.Version})
	if err != nil {
		t.Fatalf("reschedule as a waiter to the offered 12:00: %v", err)
	}
	if !moved.IsWaiter || moved.ServiceBayType != domain.BayExpress {
		t.Fatalf("moved %+v, want a waiter in the quick lane", moved)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
