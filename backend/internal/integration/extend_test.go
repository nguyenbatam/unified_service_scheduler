package integration_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Checked-in work has not started, so Extend re-checks its longer window like a booking: the
// technician's shift (and break/absences) and the day's hours and lunch. Otherwise a checked-in
// hold could run past the shift or across lunch/closing, and the stranded checks would then block
// every roster and calendar edit touching it. In-progress work is an overrun (the car is on the
// lift): it may still run past the shift. A refused extend changes nothing and records no event.
func TestExtendCheckedInRechecksRosterAndCalendar(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	// Eve (Riverside's only oil technician) works 08:00–15:00 today.
	if _, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: today.ISOWeekday(), StartTime: "08:00", EndTime: "15:00",
	}}, 0); err != nil {
		t.Fatal(err)
	}
	checkedIn := func(dealer, vehicle uuid.UUID, hour, minute int) domain.Appointment {
		t.Helper()
		item, _, err := svc.Book(ctx, staffAdmin, oilAt(dealer, vehicle, londonAt(t, hour, minute)))
		if err != nil {
			t.Fatal(err)
		}
		item, err = svc.CheckIn(ctx, staffDispatcher, item.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	refused := func(label string, item domain.Appointment, minutes int, msg string) {
		t.Helper()
		_, err := svc.Extend(ctx, staffDispatcher, item.ID, minutes, 0)
		expectCode(t, err, domain.CodeUnavailable)
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: %v, want a message about %q", label, err, msg)
		}
		got, err := svc.GetAppointment(ctx, staffAdmin, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.EndAt.Equal(item.EndAt) || got.Version != item.Version {
			t.Fatalf("%s: refused extend changed the appointment: end %s v%d, want %s v%d",
				label, got.EndAt, got.Version, item.EndAt, item.Version)
		}
	}

	// Riverside 14:00–14:30, Eve: +30 ends exactly at her shift end; +15 more is past it.
	river := checkedIn(seed.DealerRiver, newVehicle(t, svc), 14, 0)
	extended, err := svc.Extend(ctx, staffDispatcher, river.ID, 30, 0)
	if err != nil {
		t.Fatalf("extend to the shift end: %v", err)
	}
	if !extended.EndAt.Equal(londonAt(t, 15, 0)) || extended.Version != river.Version+1 {
		t.Fatalf("extended to %s v%d, want 15:00 v%d", extended.EndAt, extended.Version, river.Version+1)
	}
	wantEvents(t, "valid extend", pool, river.ID, domain.EventAppointmentExtended, 1)
	refused("past the shift end", extended, 15, "not on duty")
	wantEvents(t, "past the shift end", pool, river.ID, domain.EventAppointmentExtended, 1)

	// Northside (lunch 12:00–13:00, closes 17:00).
	refused("across lunch", checkedIn(seed.DealerNorth, newVehicle(t, svc), 11, 30), 30, "lunch")
	refused("past closing", checkedIn(seed.DealerNorth, newVehicle(t, svc), 16, 30), 30, "closing")

	// The same job in progress is an overrun: it may run past Eve's shift.
	at1405 := serviceOn(t, st, today, 14, 5)
	if _, err := at1405.StartWork(ctx, staffDispatcher, river.ID, 0); err != nil {
		t.Fatal(err)
	}
	overrun, err := at1405.Extend(ctx, staffDispatcher, river.ID, 30, 0)
	if err != nil {
		t.Fatalf("in-progress overrun past the shift end: %v", err)
	}
	if !overrun.EndAt.Equal(londonAt(t, 15, 30)) {
		t.Fatalf("overrun ends %s, want 15:30", overrun.EndAt)
	}
	wantEvents(t, "overrun", pool, river.ID, domain.EventAppointmentExtended, 2)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// A checked-in extend into a technician absence is refused like a shift end.
func TestExtendCheckedInRefusesAbsence(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	if _, err := svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonAt(t, 15, 0), EndsAt: londonAt(t, 16, 0), Reason: "training",
	}, 0); err != nil {
		t.Fatal(err)
	}
	item := mustBook(t, svc, oilAt(seed.DealerRiver, newVehicle(t, svc), londonAt(t, 14, 30)))
	if _, err := svc.CheckIn(ctx, staffDispatcher, item.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Extend(ctx, staffDispatcher, item.ID, 15, 0)
	expectCode(t, err, domain.CodeUnavailable)
}

// The re-check covers only the part of the extended window still ahead of now, like the config
// guards: a roster edit made after check-in that only touches the window's past (the car checked
// in and is still waiting for the lift) does not block extending it.
func TestExtendCheckedInIgnoresWindowPast(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	item := mustBook(t, svc, oilAt(seed.DealerRiver, newVehicle(t, svc), londonAt(t, 14, 0)))
	if _, err := svc.CheckIn(ctx, staffDispatcher, item.ID, 0); err != nil {
		t.Fatal(err)
	}
	at1420 := serviceOn(t, st, today, 14, 20)
	// Eve's shift now starts 14:15: behind 14:20, so the stranded-booking guard lets it through.
	if _, err := at1420.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: today.ISOWeekday(), StartTime: "14:15", EndTime: "15:00",
	}}, 0); err != nil {
		t.Fatalf("shift change over the window's past: %v", err)
	}
	extended, err := at1420.Extend(ctx, staffDispatcher, item.ID, 30, 0)
	if err != nil {
		t.Fatalf("extend after a shift change over the window's past: %v", err)
	}
	if !extended.EndAt.Equal(londonAt(t, 15, 0)) {
		t.Fatalf("extended to %s, want 15:00", extended.EndAt)
	}
	// The part ahead of now is still checked: past the new shift end is refused.
	_, err = at1420.Extend(ctx, staffDispatcher, item.ID, 15, 0)
	expectCode(t, err, domain.CodeUnavailable)
}
