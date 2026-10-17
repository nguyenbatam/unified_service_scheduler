package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The day board carries that day's lunch (the board draws the band), and none on a closed day.
func TestScheduleCarriesTheDaysLunch(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	// The first open day from 45 days out (the seed closes some weekdays).
	var day booking.Date
	for off := 45; ; off++ {
		day = londonDay(t, off)
		open, err := svc.Schedule(ctx, staffAdmin, seed.DealerNorth, day)
		if err != nil {
			t.Fatal(err)
		}
		if open.Closed {
			if open.LunchStart != "" || open.LunchEnd != "" {
				t.Fatalf("closed day with lunch: %+v", open)
			}
			continue
		}
		if open.LunchStart != "12:00" || open.LunchEnd != "13:00" {
			t.Fatalf("lunch = %q–%q", open.LunchStart, open.LunchEnd)
		}
		break
	}

	cal, err := svc.Calendar(ctx, staffAdmin, seed.DealerNorth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: day.String(), Reason: "Stocktake"}, cal.Version); err != nil {
		t.Fatal(err)
	}
	closed, err := svc.Schedule(ctx, staffAdmin, seed.DealerNorth, day)
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Closed || closed.LunchStart != "" || closed.LunchEnd != "" {
		t.Fatalf("closure: %+v", closed)
	}
}

// The lunch break is half-open like every window: a job ending exactly when lunch starts and one
// starting exactly when it ends are bookable (and offered), one crossing either edge is not. A
// fresh shop (08:00–18:00, lunch 12:00–13:00, no staging cap) with a bay and a technician per job,
// so only the calendar can refuse.
func TestLunchEdgesAreBookable(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	d := provisionShop(t, svc, "Lunch Edges")
	mustExec(t, pool, `UPDATE dealerships SET lunch_start = TIME '12:00', lunch_end = TIME '13:00', staging_capacity = 0 WHERE id = $1`, d.ID)
	addBay(t, svc, d.ID, "Express 1", domain.BayExpress)
	addBay(t, svc, d.ID, "General 1", domain.BayGeneral)
	addTechnician(t, svc, d.ID, "Lena Lube", 1, domain.SkillLubrication)
	addTechnician(t, svc, d.ID, "Bram Brakes", 2, domain.SkillBrakes)
	day := londonDay(t, 2)
	for _, tc := range []struct {
		name         string
		service      uuid.UUID
		hour, minute int
		bookable     bool
	}{
		{"oil ending exactly at lunch start (11:30–12:00)", seed.ServiceOil, 11, 30, true},
		{"oil starting exactly at lunch end (13:00–13:30)", seed.ServiceOil, 13, 0, true},
		{"brake ending exactly at lunch start (10:30–12:00)", seed.ServiceBrake, 10, 30, true},
		{"brake starting exactly at lunch end (13:00–14:30)", seed.ServiceBrake, 13, 0, true},
		{"brake crossing lunch start (11:00–12:30)", seed.ServiceBrake, 11, 0, false},
		{"oil inside lunch (12:30–13:00)", seed.ServiceOil, 12, 30, false},
		{"brake crossing lunch end (12:30–14:00)", seed.ServiceBrake, 12, 30, false},
	} {
		start := londonOn(t, day, tc.hour, tc.minute)
		car := newVehicle(t, svc)
		avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
			DealershipID: d.ID, VehicleID: car, ServiceTypeID: tc.service, Day: day,
		})
		if err != nil {
			t.Fatal(err)
		}
		slot := slotAt(t, avail, start)
		if slot.Available != tc.bookable || (!tc.bookable && slot.Reason != "lunch_break") {
			t.Fatalf("%s: available=%v reason=%q, want available=%v (reason lunch_break when refused)", tc.name, slot.Available, slot.Reason, tc.bookable)
		}
		item, err := bookAt(t, svc, d.ID, car, tc.service, start)
		switch {
		case tc.bookable && err != nil:
			t.Fatalf("%s: %v, want booked", tc.name, err)
		case !tc.bookable:
			expectCode(t, err, domain.CodeUnavailable)
		default:
			// Cancelled again so every case sees the empty shop.
			if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
}
