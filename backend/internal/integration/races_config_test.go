package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The races below pit a config writer against a booking: they never both commit. Either the
// booking lands and the config change is refused with 409 (it would strand it), or the config
// change lands and the booking loses cleanly (422, or 409). Each order is pinned once (see
// raceOrders): the config writer first holds its row FOR UPDATE (paused on its outbox insert) while
// the booking queues on its FOR SHARE; the booking first holds its share locks (paused on the
// dealership-day lock) while the writer queues on its FOR UPDATE.

// allWeekShifts is a 08:00–18:00 pattern on every weekday except short, which ends at noon.
func allWeekShifts(short int16) []domain.TechnicianShift {
	shifts := make([]domain.TechnicianShift, 0, 7)
	for wd := int16(1); wd <= 7; wd++ {
		end := "18:00"
		if wd == short {
			end = "12:00"
		}
		shifts = append(shifts, domain.TechnicianShift{Weekday: wd, StartTime: "08:00", EndTime: end})
	}
	return shifts
}

// Replacing Eve's weekly pattern (afternoon off on the day) races an oil booking at 14:00 that only
// she can take: the technician row lock orders them, never both.
func TestReplaceShiftsRacingBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders { // distinct weekdays
		day := londonDay(t, i+1)
		at := londonOn(t, day, 14, 0)
		var shiftErr, bookErr error
		runOrdered(t, pool, order, raceSide{
			run: func() {
				_, shiftErr = svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, allWeekShifts(day.ISOWeekday()), 0)
			},
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run:  func() { _, _, bookErr = svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, at)) },
			gate: dayGate(seed.DealerRiver, at), queued: "FOR SHARE",
		})
		w := oneWinner(t, day.String(), loses(shiftErr, domain.CodeConflict), loses(bookErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the shift change", "the booking"})
	}
	// Recount: no hold of Eve's ends after her shift on its weekday.
	offShift := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		JOIN technician_shifts s ON s.technician_id = a.technician_id
			AND s.weekday = extract(isodow FROM a.start_at AT TIME ZONE 'Europe/London')
		WHERE a.technician_id = $1 AND a.status IN (`+holdingCodes+`)
		  AND ((a.end_at AT TIME ZONE 'Europe/London')::time > s.end_time
		       OR (a.start_at AT TIME ZONE 'Europe/London')::time < s.start_time)`, seed.TechEve)
	if offShift != 0 {
		t.Fatalf("%d holds outside the technician's shift", offShift)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// A weekday override closing Riverside at noon races an oil booking at 14:00 that day.
func TestReplaceWeeklyHoursRacingBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		at := londonOn(t, day, 14, 0)
		var hoursErr, bookErr error
		runOrdered(t, pool, order, raceSide{
			run: func() {
				_, hoursErr = svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
					{Weekday: day.ISOWeekday(), OpenTime: "08:00", CloseTime: "12:00"},
				}, 0)
			},
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run:  func() { _, _, bookErr = svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, at)) },
			gate: dayGate(seed.DealerRiver, at), queued: "FOR SHARE",
		})
		// An on-grid start outside the (new) opening hours is 422 unavailable, like a closure.
		w := oneWinner(t, day.String(), loses(hoursErr, domain.CodeConflict), loses(bookErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the hours change", "the booking"})
	}
	late := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		JOIN dealership_hours h ON h.dealership_id = a.dealership_id
			AND h.weekday = extract(isodow FROM a.start_at AT TIME ZONE 'Europe/London')
		WHERE a.dealership_id = $1 AND a.status IN (`+holdingCodes+`)
		  AND (h.closed OR (a.end_at AT TIME ZONE 'Europe/London')::time > h.close_time)`, seed.DealerRiver)
	if late != 0 {
		t.Fatalf("%d holds outside the weekday's opening hours", late)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// technicianRace runs a technician admin change (through booking.Service, as the API does) against
// a booking only that technician can take, then undoes whichever side won so the next round starts
// clean.
func technicianRace(t *testing.T, service uuid.UUID, change, undo store.TechnicianPatch, stillOK func(domain.TechnicianAdmin) bool) {
	t.Helper()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		at := londonOn(t, day, 8, 0)
		var changeErr, bookErr error
		var item domain.Appointment
		runOrdered(t, pool, order, raceSide{
			run:  func() { _, changeErr = svc.UpdateTechnician(ctx, staffAdmin, seed.TechEve, change) },
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run:  func() { item, bookErr = bookAt(t, svc, seed.DealerRiver, seed.VehicleA, service, at) },
			gate: dayGate(seed.DealerRiver, at), queued: "FOR SHARE",
		})
		w := oneWinner(t, day.String(), loses(changeErr, domain.CodeConflict), loses(bookErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the technician change", "the booking"})
		booked := w == 1
		techs, _, err := svc.ListTechniciansAdmin(ctx, staffAdmin, seed.DealerRiver, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tech := range techs {
			if tech.ID == seed.TechEve && booked && !stillOK(tech) {
				t.Fatalf("%s: booked on Eve, but the change committed too: %+v", day, tech)
			}
		}
		if booked {
			assertEventsFor(t, pool, item.ID)
			if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
				t.Fatal(err)
			}
		} else if _, err := svc.UpdateTechnician(ctx, staffAdmin, seed.TechEve, undo); err != nil {
			t.Fatal(err)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// Deactivating Eve (the only Riverside lubrication technician) races an oil booking.
func TestTechnicianDeactivationRacingBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	technicianRace(t, seed.ServiceOil,
		store.TechnicianPatch{Active: new(false)}, store.TechnicianPatch{Active: new(true)},
		func(tech domain.TechnicianAdmin) bool { return tech.Active })
}

// Downgrading Eve below level 3 races a MAJOR booking that needs level 3 (only she has it).
func TestTechnicianSkillDowngradeRacingBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	technicianRace(t, seed.ServiceMajor,
		store.TechnicianPatch{SkillLevel: new(domain.LevelTechnician)}, store.TechnicianPatch{SkillLevel: new(domain.LevelSenior)},
		func(tech domain.TechnicianAdmin) bool { return tech.SkillLevel >= 3 })
}

// Northside takes 2 arrivals per hour. A no-show freed one place in a full 14:00 window; reinstating
// it and a new 14:30 arrival race for that one place. Bays and technicians suffice for both, so
// only the staging cap decides. Both queue on the dealership-day lock, so a pinned order holds that
// lock while the first and then the second queue on it: they get it in arrival order.
func TestReinstateIntoFullStagingWindowRacingBooking(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		late, err := bookAt(t, svc, seed.DealerNorth, newVehicle(t, svc), seed.ServiceOil, londonOn(t, day, 14, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bookAt(t, svc, seed.DealerNorth, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, day, 14, 0)); err != nil {
			t.Fatal(err)
		}
		at1405 := serviceOn(t, st, day, 14, 5)
		if _, err := at1405.NoShow(ctx, staffDispatcher, late.ID, 0); err != nil {
			t.Fatal(err)
		}
		vehicle := newVehicle(t, svc)
		var reinstateErr, bookErr error
		var walkIn domain.Appointment
		window := dayGate(seed.DealerNorth, londonOn(t, day, 14, 0))
		runOrdered(t, pool, order, raceSide{
			run:  func() { _, reinstateErr = at1405.Reinstate(ctx, staffDispatcher, late.ID, 0) },
			gate: window, queued: window.marker,
		}, raceSide{
			run: func() {
				walkIn, bookErr = bookAt(t, at1405, seed.DealerNorth, vehicle, seed.ServiceBrake, londonOn(t, day, 14, 30))
			},
			gate: window, queued: window.marker,
		})
		w := oneWinner(t, day.String(), loses(reinstateErr), loses(bookErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the reinstatement", "the walk-in"})
		arrivals := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE dealership_id = $1 AND status IN (`+holdingCodes+`) AND start_at >= $2 AND start_at < $3`,
			seed.DealerNorth, londonOn(t, day, 14, 0), londonOn(t, day, 15, 0))
		if arrivals != 2 {
			t.Fatalf("%s: %d arrivals in the 14:00 window, want the capacity (2)", day, arrivals)
		}
		wantEvents(t, day.String(), pool, late.ID, domain.EventAppointmentReinstated, boolInt(reinstateErr == nil))
		if bookErr == nil {
			assertEventsFor(t, pool, walkIn.ID)
		}
		assertStagingWithinCapacity(t, pool)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// The orders the free-running races above may never hit, pinned: an admin change holds its row lock
// (uncommitted), a booking that depends on it arrives and waits, then the change commits. The
// booking must judge the committed state, never the snapshot it started with:
//   - roster: the technician check re-reads shifts in a fresh statement once its FOR SHARE is
//     granted (store.LockBookableTechnician), so Eve's new afternoon off refuses the 14:00 job;
//   - hours: the day's calendar is read in the inputs batch after the dealership FOR SHARE
//     (store.BookingInputs), so the new noon closing refuses it too.
func TestConfigChangeCommittingWhileBookingWaits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		lock   string // the admin side's row lock
		change func(day booking.Date) (string, []any)
		marker string // what the waiting booking runs
	}{
		{
			name: "shift pattern",
			lock: `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`,
			change: func(day booking.Date) (string, []any) {
				return `INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time) VALUES ($1, $2, '08:00', '12:00')`,
					[]any{seed.TechEve, day.ISOWeekday()}
			},
			marker: "FOR SHARE",
		},
		{
			name: "weekday hours",
			lock: `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`,
			change: func(day booking.Date) (string, []any) {
				return `INSERT INTO dealership_hours (dealership_id, weekday, closed, open_time, close_time) VALUES ($1, $2, false, '08:00', '12:00')`,
					[]any{seed.DealerRiver, day.ISOWeekday()}
			},
			marker: "FOR SHARE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, _, svc, ctx := openTestDB(t)
			day := londonDay(t, 1)
			admin, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admin.Rollback(ctx) }()
			target := seed.TechEve
			if tc.name == "weekday hours" {
				target = seed.DealerRiver
			}
			if _, err := admin.Exec(ctx, tc.lock, target); err != nil {
				t.Fatal(err)
			}
			sql, args := tc.change(day)
			if _, err := admin.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 14, 0)))
				done <- err
			}()
			awaitLockWaiter(t, pool, tc.marker)
			if err := admin.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			expectCode(t, err, domain.CodeUnavailable)
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}
