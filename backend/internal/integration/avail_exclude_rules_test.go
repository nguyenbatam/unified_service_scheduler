package integration_test

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The reschedule view applies Reschedule's preconditions, so it never offers starts the move would
// then refuse: a customer past the online change cutoff gets the same 422 (staff are not bound by
// it), and an appointment that is no longer confirmed gets the same 409.
func TestAvailabilityExcludeFollowsRescheduleRules(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	appt, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 11, 0)))
	if err != nil {
		t.Fatal(err)
	}
	q := booking.AvailabilityQuery{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day, ExcludeAppointmentID: appt.ID,
	}
	both := func(label string, svc *booking.Service, actor domain.Actor, code string) {
		t.Helper()
		_, availErr := svc.Availability(ctx, actor, q)
		if code == "" {
			if availErr != nil {
				t.Fatalf("%s: availability %v", label, availErr)
			}
			return
		}
		expectCode(t, availErr, code)
		_, moveErr := svc.Reschedule(ctx, actor, appt.ID, booking.RescheduleRequest{StartAt: londonOn(t, day, 15, 0)})
		expectCode(t, moveErr, code)
	}
	// 10:30: inside Riverside's 60-minute online change cutoff for the 11:00 booking.
	at1030 := serviceOn(t, st, day, 10, 30)
	both("customer past the cutoff", at1030, customerAmelia, domain.CodeUnavailable)
	both("staff past the cutoff", at1030, staffAdmin, "")

	if _, err := at1030.CheckIn(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	both("checked in", at1030, staffAdmin, domain.CodeConflict)
}
