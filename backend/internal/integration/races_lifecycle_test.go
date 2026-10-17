package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// reassignRace books a Northside brake job (Blair, the first choice) on each round's day and races
// moving it to Casey against an admin change to Casey that the move would violate, each order pinned
// once (raceOrders): the change holding Casey FOR UPDATE while the move queues on its FOR SHARE, and
// the move holding that share lock (paused on the dealership-day lock) while the change queues. The
// service clock reads midnight of the round's day, so earlier rounds' jobs are past for the stranded
// checks.
func reassignRace(t *testing.T, change func(svc *booking.Service, day booking.Date) error, undo func(svc *booking.Service) error) {
	t.Helper()
	pool, st, _, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		svc := serviceOn(t, st, day, 0, 0)
		job, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleA, seed.ServiceBrake, londonOn(t, day, 14, 0))
		if err != nil {
			t.Fatal(err)
		}
		if job.TechnicianID != seed.TechBlair {
			t.Fatalf("%s: precondition: brake job on %s, want Blair", day, job.TechnicianName)
		}
		var changeErr, moveErr error
		runOrdered(t, pool, order, raceSide{
			run:  func() { changeErr = change(svc, day) },
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run: func() {
				_, moveErr = svc.Reassign(ctx, staffDispatcher, job.ID, booking.ReassignRequest{TechnicianID: seed.TechCasey})
			},
			gate: dayGate(seed.DealerNorth, job.StartAt), queued: "FOR SHARE",
		})
		w := oneWinner(t, day.String(), loses(changeErr, domain.CodeConflict), loses(moveErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the change to Casey", "the reassign"})
		moved := w == 1
		wantTech := seed.TechBlair
		if moved {
			wantTech = seed.TechCasey
		}
		if got := scalar[uuid.UUID](t, pool, `SELECT technician_id FROM appointments WHERE id = $1`, job.ID); got != wantTech {
			t.Fatalf("%s: job on %s, want %s (move err %v, change err %v)", day, got, wantTech, moveErr, changeErr)
		}
		wantEvents(t, day.String(), pool, job.ID, domain.EventAppointmentReassigned, boolInt(moved))
		if !moved {
			if err := undo(svc); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// Deactivating the target technician races a reassign onto them: never both.
func TestReassignRacingTechnicianDeactivationNeverBothWin(t *testing.T) {
	t.Parallel()
	reassignRace(t, func(svc *booking.Service, _ booking.Date) error {
		_, err := svc.UpdateTechnician(t.Context(), staffAdmin, seed.TechCasey, store.TechnicianPatch{Active: new(false)})
		return err
	}, func(svc *booking.Service) error {
		_, err := svc.UpdateTechnician(t.Context(), staffAdmin, seed.TechCasey, store.TechnicianPatch{Active: new(true)})
		return err
	})
}

// A roster edit taking the afternoon off the target technician races a reassign onto them.
func TestReassignRacingRosterEditNeverBothWin(t *testing.T) {
	t.Parallel()
	reassignRace(t, func(svc *booking.Service, day booking.Date) error {
		_, err := svc.ReplaceShifts(t.Context(), staffAdmin, seed.TechCasey, allWeekShifts(day.ISOWeekday()), 0)
		return err
	}, func(svc *booking.Service) error {
		_, err := svc.ReplaceShifts(t.Context(), staffAdmin, seed.TechCasey, nil, 0)
		return err
	})
}

// A dated closure of the target day races a reschedule onto it (Riverside oil), each order pinned
// once: the closure holding the dealership FOR UPDATE, or the reschedule holding its FOR SHARE.
func TestRescheduleRacingClosureNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		from, to := londonDay(t, i+1), londonDay(t, i+11)
		job, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, from, 8, 0))
		if err != nil {
			t.Fatal(err)
		}
		var closeErr, moveErr error
		target := londonOn(t, to, 14, 0)
		runOrdered(t, pool, order, raceSide{
			run: func() {
				_, closeErr = svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: to.String(), Reason: "Stock take"}, 0)
			},
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run: func() {
				_, moveErr = svc.Reschedule(ctx, staffDispatcher, job.ID, booking.RescheduleRequest{StartAt: target})
			},
			gate: dayGate(seed.DealerRiver, target), queued: "FOR SHARE",
		})
		w := oneWinner(t, to.String(), loses(closeErr, domain.CodeConflict), loses(moveErr))
		wantOrderWinner(t, to.String(), order, w, [2]string{"the closure", "the reschedule"})
		moved := w == 1
		wantEvents(t, to.String(), pool, job.ID, domain.EventAppointmentRescheduled, boolInt(moved))
		if !moved {
			if _, err := svc.DeleteClosure(ctx, staffAdmin, seed.DealerRiver, to, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	onClosed := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		JOIN dealership_closures c ON c.dealership_id = a.dealership_id
			AND c.day = (a.start_at AT TIME ZONE 'Europe/London')::date
		WHERE a.status IN (`+holdingCodes+`)`)
	if onClosed != 0 {
		t.Fatalf("%d holds on a closed day", onClosed)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// A weekday override closing Riverside at noon races moving the day's 08:00 job to 14:00, each
// order pinned once (as above).
func TestRescheduleRacingHoursChangeNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i, order := range raceOrders { // distinct weekdays
		day := londonDay(t, i+1)
		job, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, day, 8, 0))
		if err != nil {
			t.Fatal(err)
		}
		var hoursErr, moveErr error
		target := londonOn(t, day, 14, 0)
		runOrdered(t, pool, order, raceSide{
			run: func() {
				_, hoursErr = svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
					{Weekday: day.ISOWeekday(), OpenTime: "08:00", CloseTime: "12:00"},
				}, 0)
			},
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run: func() {
				_, moveErr = svc.Reschedule(ctx, staffDispatcher, job.ID, booking.RescheduleRequest{StartAt: target})
			},
			gate: dayGate(seed.DealerRiver, target), queued: "FOR SHARE",
		})
		w := oneWinner(t, day.String(), loses(hoursErr, domain.CodeConflict), loses(moveErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the hours change", "the reschedule"})
		moved := w == 1
		wantEvents(t, day.String(), pool, job.ID, domain.EventAppointmentRescheduled, boolInt(moved))
		if !moved {
			if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, nil, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// noShowAt books Riverside oil at 14:00 on day and marks it a no-show at 14:05; it returns the
// appointment and a service whose clock reads 14:05 that day.
func noShowAt(t *testing.T, st *store.Store, svc *booking.Service, day booking.Date, vehicle uuid.UUID) (domain.Appointment, *booking.Service) {
	t.Helper()
	job, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceOil, londonOn(t, day, 14, 0))
	if err != nil {
		t.Fatal(err)
	}
	at1405 := serviceOn(t, st, day, 14, 5)
	if _, err := at1405.NoShow(t.Context(), staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	return job, at1405
}

// An absence of the booked technician over the rest of the window races reinstating a late
// customer: the technician row lock orders them, exactly one wins. Each order is pinned once: the
// absence holding Eve FOR UPDATE, or the reinstatement holding her FOR SHARE.
func TestReinstateRacingAbsenceExactlyOneWins(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i, order := range raceOrders {
		day := londonDay(t, i+1)
		job, at1405 := noShowAt(t, st, svc, day, seed.VehicleA)
		var absenceErr, reinstateErr error
		runOrdered(t, pool, order, raceSide{
			run: func() {
				_, absenceErr = at1405.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
					StartsAt: londonOn(t, day, 14, 10), EndsAt: londonOn(t, day, 15, 0), Reason: "Dentist",
				}, 0)
			},
			gate: outboxGate, queued: "FOR UPDATE",
		}, raceSide{
			run:  func() { _, reinstateErr = at1405.Reinstate(ctx, staffDispatcher, job.ID, 0) },
			gate: dayGate(seed.DealerRiver, job.StartAt), queued: "FOR SHARE",
		})
		w := oneWinner(t, day.String(), loses(absenceErr, domain.CodeConflict), loses(reinstateErr))
		wantOrderWinner(t, day.String(), order, w, [2]string{"the absence", "the reinstatement"})
		wantEvents(t, day.String(), pool, job.ID, domain.EventAppointmentReinstated, boolInt(reinstateErr == nil))
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// Reinstate re-reads the calendar after the dealership FOR SHARE. A calendar change holding the
// dealership row (hours cut to noon, or a closure, uncommitted) makes it wait; once that commits,
// reinstate must refuse the kept window (config changes do not count a no-show as stranded), and
// once it rolls back, reinstate goes through.
func TestReinstateWaitsForCalendarChange(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	const cutHours = `INSERT INTO dealership_hours (dealership_id, weekday, closed, open_time, close_time)
		VALUES ($1, $2, false, '08:00', '12:00')`
	const closeDay = `INSERT INTO dealership_closures (dealership_id, day, reason) VALUES ($1, $2::date, 'Flood')`
	cases := []struct {
		name   string
		sql    string
		arg    func(booking.Date) any
		commit bool
	}{
		{"hours cut to noon committed", cutHours, func(d booking.Date) any { return d.ISOWeekday() }, true},
		{"hours cut to noon rolled back", cutHours, func(d booking.Date) any { return d.ISOWeekday() }, false},
		{"closure committed", closeDay, func(d booking.Date) any { return d.String() }, true},
		{"closure rolled back", closeDay, func(d booking.Date) any { return d.String() }, false},
	}
	for i, tc := range cases {
		day := londonDay(t, 1+i)
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			job, at1405 := noShowAt(t, st, svc, day, seed.VehicleA)
			admin, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admin.Rollback(ctx) }()
			if _, err := admin.Exec(ctx, `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`, seed.DealerRiver); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Exec(ctx, tc.sql, seed.DealerRiver, tc.arg(day)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := at1405.Reinstate(ctx, staffDispatcher, job.ID, 0)
				done <- err
			}()
			awaitLockWaiter(t, pool, "FROM dealerships WHERE id = $1 FOR SHARE")
			if tc.commit {
				err = admin.Commit(ctx)
			} else {
				err = admin.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = <-done
			if tc.commit {
				expectCode(t, err, domain.CodeUnavailable)
			} else if err != nil {
				t.Fatalf("reinstate after the calendar change rolled back: %v", err)
			}
			wantEvents(t, tc.name, pool, job.ID, domain.EventAppointmentReinstated, boolInt(err == nil))
			// Undo a committed change so the next case starts from the default calendar.
			if _, err := pool.Exec(ctx, `DELETE FROM dealership_hours WHERE dealership_id = $1`, seed.DealerRiver); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM dealership_closures WHERE dealership_id = $1`, seed.DealerRiver); err != nil {
				t.Fatal(err)
			}
		})
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Multi-line jobs at Riverside (oil + brake: lubrication and brakes at level 2 in a general bay)
// need Eve and Bay B. Three of them race, listing the lines in both orders (service-type share
// locks are taken in id order, so the orders cannot deadlock), the third overlapping the first
// two by an hour: exactly one wins, and its two lines are booked.
func TestMultiLineBookingsRacingExactlyOneWins(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	oilBrake := []uuid.UUID{seed.ServiceOil, seed.ServiceBrake}
	brakeOil := []uuid.UUID{seed.ServiceBrake, seed.ServiceOil}
	for offset := 1; offset <= 5; offset++ {
		day := londonDay(t, offset)
		reqs := []booking.BookRequest{
			{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeIDs: oilBrake, StartAt: londonOn(t, day, 8, 0)},
			{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeIDs: brakeOil, StartAt: londonOn(t, day, 8, 0)},
			{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleC, ServiceTypeIDs: brakeOil, StartAt: londonOn(t, day, 9, 0)},
		}
		items := make([]domain.Appointment, len(reqs))
		errs := make([]error, len(reqs))
		raceN(len(reqs), func(i int) {
			items[i], _, errs[i] = svc.Book(ctx, staffAdmin, reqs[i])
		})
		w := items[oneWinner(t, day.String(), allLose(errs)...)]
		if w.TechnicianID != seed.TechEve || w.ServiceBayID != seed.BayR2 || len(w.Lines) != 2 || w.DurationMinutes != 120 {
			t.Fatalf("%s: winner %+v", day, w)
		}
		if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_lines WHERE appointment_id = $1`, w.ID); n != 2 {
			t.Fatalf("%s: %d lines", day, n)
		}
		assertEventsFor(t, pool, w.ID)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// With an express-lane limit of 90 minutes a waiter's brake job at Northside may use the only
// express bay (Bay 1), which a non-waiter oil change needs too. The waiter takes a general bay
// while one is free, so whatever the interleaving both are booked: the quick lane is never spent
// on a job that had another bay (outcome, not just choice: no capacity lost to the upgrade).
func TestWaiterSoftUpgradeRacingOilChange(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	if _, err := svc.UpdateDealership(ctx, staffAdmin, seed.DealerNorth, store.DealershipPatch{ExpressMaxMinutes: new(int16(90))}); err != nil {
		t.Fatal(err)
	}
	for offset := 1; offset <= 6; offset++ {
		day := londonDay(t, offset)
		start := londonOn(t, day, 14, 0)
		var waiter, oil domain.Appointment
		var waiterErr, oilErr error
		race(func() {
			waiter, _, waiterErr = svc.Book(ctx, staffAdmin, booking.BookRequest{
				DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake,
				StartAt: start, IsWaiter: true, IdempotencyKey: uuid.NewString(),
			})
		}, func() {
			oil, oilErr = bookAt(t, svc, seed.DealerNorth, seed.VehicleB, seed.ServiceOil, start)
		})
		if waiterErr != nil || oilErr != nil {
			t.Fatalf("%s: both fit (general bays free for the waiter), got waiter %v, oil %v", day, waiterErr, oilErr)
		}
		if waiter.ServiceBayType != domain.BayGeneral {
			t.Fatalf("%s: waiter on %s with general bays free", day, waiter.ServiceBayName)
		}
		if oil.ServiceBayID != seed.BayN1 {
			t.Fatalf("%s: oil change on %s", day, oil.ServiceBayName)
		}
		assertEventsFor(t, pool, waiter.ID)
		assertEventsFor(t, pool, oil.ID)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertStagingWithinCapacity(t, pool)
}
