package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A job longer than any opening window of the week is a malformed request (400) for both
// availability and booking — not a 422 "outside opening hours" from one and a 400 from the other —
// while a job that fits some weekday but not the asked day stays a 422.
func TestJobLongerThanAnyWindowIs400ForBookingToo(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	d, err := svc.CreateDealership(ctx, staffAdmin, store.NewDealership{
		Name: "Short Hours", Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "12:00", StagingWindowMinutes: 60,
		Policy: domain.Policy{CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	day := londonDay(t, 1)
	lines := []uuid.UUID{seed.ServiceMajor, seed.ServiceBrake} // 270 min > the 240-minute day
	_, err = svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeIDs: lines, Day: day})
	expectCode(t, err, domain.CodeInvalid)
	_, _, err = svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeIDs: lines, StartAt: londonOn(t, day, 8, 0), IdempotencyKey: uuid.NewString(),
	})
	expectCode(t, err, domain.CodeInvalid)

	// 180 minutes fit the day, but not from 10:00: outside the hours, 422.
	_, _, err = svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceMajor, StartAt: londonOn(t, day, 10, 0), IdempotencyKey: uuid.NewString(),
	})
	expectCode(t, err, domain.CodeUnavailable)
}
