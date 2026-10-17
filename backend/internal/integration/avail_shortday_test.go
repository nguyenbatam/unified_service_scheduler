package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// An open day too short for the job is a valid question with an empty answer (200 + reason),
// like a lunch-only conflict; only a job no weekday can ever hold is invalid input (400).
func TestAvailabilityShortDayIsEmptyNotInvalid(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	short := londonDay(t, 1)
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: short.ISOWeekday(), OpenTime: "08:00", CloseTime: "09:00"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	q := booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, Day: short}
	avail, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatalf("a 1h day for a 90 min job must not be an error: %v", err)
	}
	if avail.Closed || avail.Reason != booking.ReasonDayTooShort || len(avail.Slots) != 0 {
		t.Fatalf("short day = %+v, want no slots and reason %q", avail, booking.ReasonDayTooShort)
	}
	// The oil change still fits that day.
	q.ServiceTypeID = seed.ServiceOil
	if avail, err := svc.Availability(ctx, staffAdmin, q); err != nil || avail.Reason != "" || len(avail.Slots) != 2 {
		t.Fatalf("30 min job on a 1h day = %+v, %v; want two slots", avail, err)
	}

	// The multi-day search skips the short day.
	q.ServiceTypeID = seed.ServiceBrake
	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: short, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Slots) != 1 || next.Slots[0].Date != londonDay(t, 2).String() || !next.Slots[0].Start.Equal(londonOn(t, londonDay(t, 2), 8, 0)) {
		t.Fatalf("short day must be skipped: %+v", next)
	}

	// Northside's longest window is 4h (lunch splits 08:00–17:00): a 5h job can never be booked.
	never := booking.AvailabilityQuery{
		DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, Day: short,
		ServiceTypeIDs: []uuid.UUID{seed.ServiceMajor, seed.ServiceBrake, seed.ServiceOil},
	}
	_, err = svc.Availability(ctx, staffAdmin, never)
	expectCode(t, err, domain.CodeInvalid)
	_, err = svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: never, From: short})
	expectCode(t, err, domain.CodeInvalid)
	// …while 3h fits around lunch and only the crossing slots are blocked.
	never.ServiceTypeIDs = nil
	never.ServiceTypeID = seed.ServiceMajor
	avail, err = svc.Availability(ctx, staffAdmin, never)
	if err != nil {
		t.Fatal(err)
	}
	if slot := slotAt(t, avail, londonOn(t, short, 10, 0)); slot.Reason != "lunch_break" {
		t.Fatalf("10:00 major = %+v, want lunch_break", slot)
	}
}
