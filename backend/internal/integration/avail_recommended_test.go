package integration_test

import (
	"slices"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Recommended starts leave no new idle gap: on a one-lane shop (08:00–18:00, lunch 12:00–13:00)
// with an oil change booked 10:00–10:30, the 30-minute starts that butt against it or an edge of
// the bookable stretch (opening, lunch, closing) are recommended, the ones in the middle of a free
// stretch are not. NextAvailable carries the same flag. Booking a recommended start keeps the rest
// of the day in whole stretches, and the start right after it is recommended next.
func TestAvailabilityRecommendsGapFillingStarts(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	d := provisionShop(t, svc, "Recommended")
	if _, err := svc.UpdateDealership(ctx, staffAdmin, d.ID, store.DealershipPatch{LunchStart: new("12:00"), LunchEnd: new("13:00")}); err != nil {
		t.Fatal(err)
	}
	addBay(t, svc, d.ID, "Lane", domain.BayExpress)
	addTechnician(t, svc, d.ID, "Lube", 1, domain.SkillLubrication)
	day := londonDay(t, 1)
	at := func(h, m int) time.Time { return londonOn(t, day, h, m) }
	mustBook(t, svc, oilAt(d.ID, newVehicle(t, svc), at(10, 0)))

	q := booking.AvailabilityQuery{DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceOil, Day: day}
	recommended := func() []time.Time {
		t.Helper()
		avail, err := svc.Availability(ctx, staffAdmin, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []time.Time
		for _, slot := range avail.Slots {
			if slot.Recommended && (!slot.Available || slot.FreeBays == 0 || slot.FreeTechnicians == 0) {
				t.Fatalf("%s recommended but not free: %+v", slot.Start, slot)
			}
			if slot.Recommended {
				out = append(out, slot.Start)
			}
		}
		return out
	}
	expect := func(label string, got []time.Time, want ...time.Time) {
		t.Helper()
		if !slices.EqualFunc(got, want, time.Time.Equal) {
			t.Fatalf("%s: recommended %v, want %v", label, got, want)
		}
	}
	// Opening, either side of the 10:00 job, either side of lunch, and closing.
	first := []time.Time{at(8, 0), at(9, 30), at(10, 30), at(11, 30), at(13, 0), at(17, 30)}
	expect("one booking", recommended(), first...)

	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: day, Days: 1, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var flagged []time.Time
	for _, slot := range next.Slots {
		if slot.Recommended {
			flagged = append(flagged, slot.Start)
		}
	}
	expect("next available", flagged, first...)

	mustBook(t, svc, oilAt(d.ID, newVehicle(t, svc), at(10, 30)))
	expect("after booking 10:30", recommended(), at(8, 0), at(9, 30), at(11, 0), at(11, 30), at(13, 0), at(17, 30))
}
