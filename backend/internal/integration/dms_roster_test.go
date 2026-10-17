package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Riverside's only lubrication technician is Eve, so her roster decides every oil slot there.
func TestTechnicianRosterGatesBooking(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	tomorrow := londonDay(t, 1)
	at := func(h, m int) domain.Appointment {
		t.Helper()
		item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, tomorrow, h, m)))
		if err != nil {
			t.Fatalf("book %02d:%02d: %v", h, m, err)
		}
		return item
	}
	refused := func(day booking.Date, h, m int) {
		t.Helper()
		_, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, day, h, m)))
		expectCode(t, err, domain.CodeUnavailable)
	}

	roster, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: tomorrow.ISOWeekday(), StartTime: "12:00", EndTime: "18:00", BreakStart: "14:00", BreakEnd: "14:30",
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	refused(tomorrow, 9, 0)         // before her shift
	refused(tomorrow, 14, 0)        // her break
	refused(londonDay(t, 2), 12, 0) // a weekday she does not work
	booked := at(12, 0)             // inside the shift
	if booked.TechnicianID != seed.TechEve {
		t.Fatalf("tech = %s", booked.TechnicianName)
	}
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceOil, Day: tomorrow,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range avail.Slots {
		_, loc := suiteToday()
		local := slot.Start.In(loc).Format("15:04")
		switch local {
		case "09:00", "14:00", "12:00":
			if slot.Available {
				t.Errorf("%s must be unavailable (%q)", local, slot.Reason)
			}
		case "12:30", "14:30":
			if !slot.Available {
				t.Errorf("%s must be available, got %q", local, slot.Reason)
			}
		}
	}

	// Roster changes may not strand the 12:00 booking.
	_, err = svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: tomorrow.ISOWeekday(), StartTime: "13:00", EndTime: "18:00",
	}}, roster.Version)
	expectCode(t, err, domain.CodeConflict)
	_, err = svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonOn(t, tomorrow, 11, 0), EndsAt: londonOn(t, tomorrow, 13, 0), Reason: "Training",
	}, 0)
	expectCode(t, err, domain.CodeConflict)

	roster, err = svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonOn(t, tomorrow, 16, 0), EndsAt: londonOn(t, tomorrow, 17, 0), Reason: "Dentist",
	}, roster.Version)
	if err != nil || len(roster.Absences) != 1 {
		t.Fatalf("absence: %+v %v", roster, err)
	}
	refused(tomorrow, 16, 0)
	_, err = svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonOn(t, tomorrow, 16, 30), EndsAt: londonOn(t, tomorrow, 17, 30),
	}, 0)
	expectCode(t, err, domain.CodeConflict) // overlaps the first absence
	_, err = svc.DeleteAbsence(ctx, staffAdmin, seed.TechEve, roster.Absences[0].ID, roster.Version-1)
	expectCode(t, err, domain.CodePrecondition)
	if roster, err = svc.DeleteAbsence(ctx, staffAdmin, seed.TechEve, roster.Absences[0].ID, roster.Version); err != nil {
		t.Fatal(err)
	}
	at(16, 0)

	// Back to the dealership's opening hours.
	if _, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{}, roster.Version); err != nil {
		t.Fatal(err)
	}
	at(9, 0)
}

func TestReassignRefusesTechnicianOffDuty(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	tomorrow := londonDay(t, 1)
	job, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, londonOn(t, tomorrow, 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	other := seed.TechCasey
	if job.TechnicianID == seed.TechCasey {
		other = seed.TechBlair
	}
	if _, err := svc.AddAbsence(ctx, staffAdmin, other, domain.TechnicianAbsence{
		StartsAt: londonOn(t, tomorrow, 9, 0), EndsAt: londonOn(t, tomorrow, 12, 0), Reason: "Leave",
	}, 0); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Reassign(ctx, staffDispatcher, job.ID, booking.ReassignRequest{TechnicianID: other})
	expectCode(t, err, domain.CodeUnavailable)
}

// An absence and a booking for the same technician and time never both commit: the absence
// writer locks the technician row FOR UPDATE, the booking re-checks the roster under FOR SHARE.
func TestAbsenceAndBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	tomorrow := londonDay(t, 1)
	for hour := 9; hour <= 13; hour++ {
		start := londonOn(t, tomorrow, hour, 0)
		var bookErr, absenceErr error
		race(func() {
			_, _, bookErr = svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, start))
		}, func() {
			_, absenceErr = svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
				StartsAt: start, EndsAt: start.Add(30 * time.Minute), Reason: "race",
			}, 0)
		})
		oneWinner(t, fmt.Sprintf("%02d:00", hour), loses(bookErr, refusals...), loses(absenceErr, refusals...))
	}
	both := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a JOIN technician_absences ab ON ab.technician_id = a.technician_id
		WHERE a.status IN (`+holdingCodes+`) AND tstzrange(a.start_at, a.end_at) && tstzrange(ab.starts_at, ab.ends_at)`)
	if both != 0 {
		t.Fatalf("%d bookings overlap an absence", both)
	}
}
