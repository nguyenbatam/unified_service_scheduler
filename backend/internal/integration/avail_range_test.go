package integration_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// NextAvailable reads each input once for its whole range and buckets it per day in Go. Its
// answer must be exactly what asking Availability day by day gives, over a calendar with a
// closure, weekday overrides (one too short for the job, one without lunch), technician shifts
// and a break, an absence across midnight, staging, the car busy elsewhere, and existing bookings.
func TestNextAvailableMatchesPerDayAvailability(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day := func(offset int) booking.Date { return londonDay(t, offset) }

	// Calendar (Northside: 08:00–17:00, lunch 12:00–13:00, 2 arrivals per hour).
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, []domain.WeekdayHours{
		{Weekday: day(3).ISOWeekday(), OpenTime: "08:00", CloseTime: "09:00"}, // too short for 90 min
		{Weekday: day(4).ISOWeekday(), OpenTime: "08:00", CloseTime: "12:00"}, // no lunch
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: day(2).String(), Reason: "Holiday"}, 0); err != nil {
		t.Fatal(err)
	}
	// Roster: Blair (brakes, level 3) works full days on the seeded days, afternoons on day 5 and
	// mornings with a break on day 6; other weekdays off. Casey is absent from day 5 14:00 to day 6 10:00.
	if _, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechBlair, []domain.TechnicianShift{
		{Weekday: day(0).ISOWeekday(), StartTime: "08:00", EndTime: "17:00"},
		{Weekday: day(1).ISOWeekday(), StartTime: "08:00", EndTime: "17:00"},
		{Weekday: day(5).ISOWeekday(), StartTime: "13:00", EndTime: "17:00"},
		{Weekday: day(6).ISOWeekday(), StartTime: "08:00", EndTime: "12:00", BreakStart: "10:00", BreakEnd: "10:30"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddAbsence(ctx, staffAdmin, seed.TechCasey, domain.TechnicianAbsence{
		ID: uuid.New(), StartsAt: londonOn(t, day(5), 14, 0), EndsAt: londonOn(t, day(6), 10, 0), Reason: "Training",
	}, 0); err != nil {
		t.Fatal(err)
	}
	// Bookings: a brake on day 1, the car itself at Riverside on day 5, a full arrival hour on day 6.
	for _, req := range []booking.BookRequest{
		brakeAt(seed.DealerNorth, seed.VehicleB, londonOn(t, day(1), 8, 0)),
		oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day(5), 13, 0)),
		oilAt(seed.DealerNorth, seed.VehicleB, londonOn(t, day(6), 14, 0)),
		oilAt(seed.DealerNorth, seed.VehicleC, londonOn(t, day(6), 14, 30)),
	} {
		if _, _, err := svc.Book(ctx, staffAdmin, req); err != nil {
			t.Fatalf("book %s: %v", req.StartAt, err)
		}
	}

	q := booking.AvailabilityQuery{DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake}
	perDay := map[string]domain.Availability{}
	availabilityOn := func(t *testing.T, actor domain.Actor, d booking.Date) domain.Availability {
		t.Helper()
		key := actor.UserID.String() + d.String()
		if a, ok := perDay[key]; ok {
			return a
		}
		dq := q
		dq.Day = d
		a, err := svc.Availability(ctx, actor, dq)
		if err != nil {
			t.Fatalf("availability %s: %v", d, err)
		}
		perDay[key] = a
		return a
	}

	// The scenario really exercises every rule (otherwise equality would prove little).
	reasons := map[string]bool{}
	for i := range 10 {
		a := availabilityOn(t, staffAdmin, day(i))
		reasons[a.Reason] = true
		if a.Closed {
			reasons["closed"] = true
		}
		for _, s := range a.Slots {
			reasons[s.Reason] = true
		}
	}
	for _, want := range []string{"closed", booking.ReasonDayTooShort, "lunch_break", booking.ReasonVehicleBusy, "staging_full", "no_technician", ""} {
		if !reasons[want] {
			t.Errorf("scenario never produced reason %q (got %v)", want, reasons)
		}
	}

	const days, limit = 10, 20
	for _, actor := range []domain.Actor{staffAdmin, customerAmelia} {
		for offset := range 8 {
			from := day(offset)
			got, err := svc.NextAvailable(ctx, actor, booking.NextQuery{AvailabilityQuery: q, From: from, Days: days, Limit: limit})
			if err != nil {
				t.Fatal(err)
			}
			want := []domain.NextSlot{}
			searchedTo := from
			for i := 0; i < days && len(want) < limit; i++ {
				d := day(offset + i)
				searchedTo = d
				for _, s := range availabilityOn(t, actor, d).Slots {
					if s.Available && len(want) < limit {
						want = append(want, domain.NextSlot{
							Date: d.String(), Start: s.Start, End: s.End, FreeBays: s.FreeBays, FreeTechnicians: s.FreeTechnicians, Recommended: s.Recommended,
						})
					}
				}
			}
			if !reflect.DeepEqual(got.Slots, want) {
				t.Fatalf("actor %s from %s:\n next    = %+v\n per-day = %+v", actor.Role.Name(), from, got.Slots, want)
			}
			if actor.IsStaff() && (got.SearchedFrom != from.String() || got.SearchedTo != searchedTo.String()) {
				t.Fatalf("from %s searched %s..%s, want ..%s", from, got.SearchedFrom, got.SearchedTo, searchedTo)
			}
		}
	}
}
