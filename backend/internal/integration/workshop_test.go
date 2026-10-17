package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// newVehicle registers a fresh vehicle for Amelia: seed vehicles are already booked today.
func newVehicle(t testing.TB, svc *booking.Service) uuid.UUID {
	t.Helper()
	vin := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:17]
	v, err := svc.CreateVehicle(t.Context(), staffAdmin, booking.VehicleInput{
		CustomerID: seed.CustomerA, VIN: vin, Make: "Test", Model: "Car", Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// Riverside MAJOR (180 min) needs Eve (the only general level-3 tech) and Bay B (the only general
// bay), so two majors there always compete for the same technician and bay.
func riverMajor(t *testing.T, svc *booking.Service, vehicle uuid.UUID, start time.Time) domain.Appointment {
	t.Helper()
	item, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceMajor, start)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestReinstateLateArrival(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	appt := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0)) // 08:00–11:00
	at840 := serviceOn(t, st, today, 8, 40)
	if _, err := at840.NoShow(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err := at840.Reinstate(ctx, customerAmelia, appt.ID, 0)
	expectCode(t, err, domain.CodeForbidden)

	back, err := serviceOn(t, st, today, 8, 45).Reinstate(ctx, staffDispatcher, appt.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if back.Status != domain.StatusCheckedIn || back.CheckedInAt == nil || back.Version != appt.Version+2 {
		t.Fatalf("reinstated = status %d checkedIn %v version %d", back.Status, back.CheckedInAt, back.Version)
	}
	wantEvents(t, "reinstated", pool, appt.ID, domain.EventAppointmentReinstated, 1)
	// It holds the bay/technician again: the slot is no longer bookable.
	_, err = bookAt(t, at840, seed.DealerRiver, newVehicle(t, svc), seed.ServiceMajor, londonOn(t, today, 9, 0))
	expectCode(t, err, domain.CodeUnavailable)
}

func TestReinstateRefusedWhenRebookedRetiredOrOver(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	appt := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0))
	at840 := serviceOn(t, st, today, 8, 40)
	if _, err := at840.NoShow(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	// The released slot was sold to someone else (walk-in at 09:00 on Eve / Bay B).
	walkIn := riverMajor(t, at840, newVehicle(t, svc), londonOn(t, today, 9, 0))
	_, err := serviceOn(t, st, today, 8, 45).Reinstate(ctx, staffDispatcher, appt.ID, 0)
	expectCode(t, err, domain.CodeConflict)

	if _, err := at840.Cancel(ctx, staffDispatcher, walkIn.ID, 0); err != nil {
		t.Fatal(err)
	}
	// Eve was deactivated meanwhile (allowed: a no-show does not hold her).
	if _, err := st.UpdateTechnician(ctx, londonOn(t, today, 8, 40), seed.TechEve, store.TechnicianPatch{Active: new(false)}); err != nil {
		t.Fatal(err)
	}
	_, err = serviceOn(t, st, today, 8, 45).Reinstate(ctx, staffDispatcher, appt.ID, 0)
	expectCode(t, err, domain.CodeConflict)
	if _, err := st.UpdateTechnician(ctx, londonOn(t, today, 8, 40), seed.TechEve, store.TechnicianPatch{Active: new(true)}); err != nil {
		t.Fatal(err)
	}
	// The booked window is over.
	_, err = serviceOn(t, st, today, 11, 0).Reinstate(ctx, staffDispatcher, appt.ID, 0)
	expectCode(t, err, domain.CodeUnavailable)
}

func TestStartWorkRefusedWhileEarlierJobInProgress(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	first := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0))   // 08:00–11:00
	second := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 11, 0)) // 11:00–14:00
	// Both cars are in and the first job starts inside its window (late, at 10:50).
	inWindow := serviceOn(t, st, today, 10, 50)
	for _, id := range []uuid.UUID{first.ID, second.ID} {
		if _, err := inWindow.CheckIn(ctx, staffDispatcher, id, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inWindow.StartWork(ctx, staffDispatcher, first.ID, 0); err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 11, 5)
	// The first job overran (nobody extended it): Eve and Bay B are still busy with it.
	_, err := clock.StartWork(ctx, staffDispatcher, second.ID, 0)
	expectCode(t, err, domain.CodeConflict)
	if _, err := clock.Complete(ctx, staffDispatcher, first.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := clock.StartWork(ctx, staffDispatcher, second.ID, 0); err != nil {
		t.Fatalf("start after the earlier job completed: %v", err)
	}
}

// Two dispatchers start two jobs of the same technician at once: exactly one wins.
func TestConcurrentStartWorkOneJobPerTechnician(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i := range 5 {
		day := londonDay(t, i)
		first := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, day, 8, 0))
		second := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, day, 11, 0))
		clock := serviceOn(t, st, day, 10, 55)
		for _, id := range []uuid.UUID{first.ID, second.ID} {
			if _, err := clock.CheckIn(ctx, staffDispatcher, id, 0); err != nil {
				t.Fatal(err)
			}
		}
		errs := make([]error, 2)
		jobs := []uuid.UUID{first.ID, second.ID}
		raceN(len(jobs), func(j int) { _, errs[j] = clock.StartWork(ctx, staffDispatcher, jobs[j], 0) })
		// Recount in Postgres, not only the return codes: one job in progress for Eve, one STARTED event.
		inProgress := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments WHERE technician_id = $1 AND status = `+code(domain.StatusInProgress), seed.TechEve)
		started := scalar[int](t, pool, `
			SELECT count(*)::int FROM outbox_events WHERE event_type = `+code(domain.EventAppointmentStarted)+` AND aggregate_id = ANY($1)`, jobs)
		if inProgress != 1 || started != 1 {
			t.Fatalf("day %s: %d jobs in progress and %d STARTED events for one technician, want 1 and 1", day, inProgress, started)
		}
		winner := jobs[oneWinner(t, day.String(), allLose(errs, domain.CodeConflict)...)]
		// Leave the technician free for the next day's round.
		if _, err := clock.Complete(ctx, staffDispatcher, winner, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverrunSweeperExtendsThenFlags(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0)) // 08:00–11:00
	at := func(h, m int) *booking.Service { return serviceOn(t, st, today, h, m) }
	if _, err := at(8, 0).CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := at(8, 0).StartWork(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	sweep := func(svc *booking.Service, wantExtended, wantFlagged int) domain.Appointment {
		t.Helper()
		extended, flagged, err := svc.HandleOverruns(ctx, 15*time.Minute)
		if err != nil || extended != wantExtended || flagged != wantFlagged {
			t.Fatalf("sweep = extended %d flagged %d err %v, want %d/%d", extended, flagged, err, wantExtended, wantFlagged)
		}
		item, err := st.GetAppointment(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	if got := sweep(at(10, 59), 0, 0); !got.EndAt.Equal(londonOn(t, today, 11, 0)) {
		t.Fatalf("not overrunning yet, end=%s", got.EndAt)
	}
	if got := sweep(at(11, 5), 1, 0); !got.EndAt.Equal(londonOn(t, today, 11, 15)) {
		t.Fatalf("auto-extended end=%s want 11:15", got.EndAt)
	}
	next := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 11, 30)) // Eve / Bay B again
	if got := sweep(at(11, 20), 1, 0); !got.EndAt.Equal(londonOn(t, today, 11, 30)) {
		t.Fatalf("extended up to the next job, end=%s", got.EndAt) // half-open: 11:30 touches, no overlap
	}
	flaggedJob := sweep(at(11, 31), 0, 1)
	if flaggedJob.OverrunAt == nil || !flaggedJob.EndAt.Equal(londonOn(t, today, 11, 30)) {
		t.Fatalf("blocked overrun must be flagged, got overrunAt=%v end=%s", flaggedJob.OverrunAt, flaggedJob.EndAt)
	}
	wantEvents(t, "blocked overrun", pool, job.ID, domain.EventAppointmentOverrunning, 1)
	wantEvents(t, "auto-extensions", pool, job.ID, domain.EventAppointmentExtended, 2)
	sweep(at(11, 40), 0, 0) // still blocked: alerted once, not on every tick

	// The dispatcher moves the next job: the next sweep resumes extending. The job stays flagged
	// (still overrunning, alerted once), so it keeps holding Eve and Bay B for the day.
	if _, err := svc.Cancel(ctx, staffDispatcher, next.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := sweep(at(11, 40), 1, 0); got.OverrunAt == nil || !got.EndAt.Equal(londonOn(t, today, 11, 45)) {
		t.Fatalf("after the next job moved: overrunAt=%v end=%s want still flagged/11:45", got.OverrunAt, got.EndAt)
	}
	if _, err := bookAt(t, at(11, 40), seed.DealerRiver, newVehicle(t, svc), seed.ServiceMajor, londonOn(t, today, 12, 0)); err == nil {
		t.Fatal("a job was booked right after a flagged overrun whose extension resumed")
	}
	wantEvents(t, "resumed overrun", pool, job.ID, domain.EventAppointmentOverrunning, 1)
	// Auto-extension stops at service duration + MaxExtend (08:00 + 3h + 4h = 15:00): still
	// flagged, and left out of the sweep from then on.
	if got := sweep(at(15, 10), 1, 0); got.OverrunAt == nil || !got.EndAt.Equal(londonOn(t, today, 15, 0)) {
		t.Fatalf("at the auto-extension cap: overrunAt=%v end=%s want flagged/15:00", got.OverrunAt, got.EndAt)
	}
	sweep(at(15, 20), 0, 0)
	// A manual extend clears the flag too.
	extended, err := at(15, 10).Extend(ctx, staffDispatcher, job.ID, 15, 0)
	if err != nil || extended.OverrunAt != nil {
		t.Fatalf("manual extend = overrunAt %v err %v", extended.OverrunAt, err)
	}
}

// The extension and a booking of the next slot race for the same bay/technician: one wins.
func TestOverrunExtensionAndNextBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i := 1; i <= 5; i++ {
		day := londonDay(t, i)
		job := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, day, 8, 0)) // 08:00–11:00
		early := serviceOn(t, st, day, 8, 0)
		if _, err := early.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := early.StartWork(ctx, staffDispatcher, job.ID, 0); err != nil {
			t.Fatal(err)
		}
		vehicle := newVehicle(t, svc)
		var extended int
		var sweepErr, bookErr error
		race(func() {
			extended, _, sweepErr = serviceOn(t, st, day, 11, 5).HandleOverruns(ctx, 15*time.Minute)
		}, func() {
			_, bookErr = bookAt(t, early, seed.DealerRiver, vehicle, seed.ServiceMajor, londonOn(t, day, 11, 0))
		})
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		// The sweep reports a lost race by not extending, not as an error.
		var sweepLost error
		if extended != 1 {
			sweepLost = domain.NewError(domain.CodeConflict, "not extended")
		}
		oneWinner(t, day.String(), loses(sweepLost, domain.CodeConflict), loses(bookErr, refusals...))
		assertNoOverlaps(t, pool)
		assertAllocationsValid(t, pool)
		if _, err := early.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	assertNoOrphanEvents(t, pool)
}

// Reinstating a no-show and selling its freed slot race: one wins, the other gets 409/422.
func TestReinstateAndRebookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i := 1; i <= 5; i++ {
		day := londonDay(t, i)
		appt := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, day, 8, 0))
		at840 := serviceOn(t, st, day, 8, 40)
		if _, err := at840.NoShow(ctx, staffDispatcher, appt.ID, 0); err != nil {
			t.Fatal(err)
		}
		vehicle := newVehicle(t, svc)
		var reinstateErr, bookErr error
		race(func() {
			_, reinstateErr = serviceOn(t, st, day, 8, 45).Reinstate(ctx, staffDispatcher, appt.ID, 0)
		}, func() {
			_, bookErr = bookAt(t, at840, seed.DealerRiver, vehicle, seed.ServiceMajor, londonOn(t, day, 9, 0))
		})
		oneWinner(t, day.String(), loses(reinstateErr, domain.CodeConflict), loses(bookErr, refusals...))
		wantEvents(t, day.String(), pool, appt.ID, domain.EventAppointmentReinstated, boolInt(reinstateErr == nil))
		assertNoOverlaps(t, pool)
		assertAllocationsValid(t, pool)
	}
	assertNoOrphanEvents(t, pool)
}

// Northside takes 2 arrivals per hour. A no-show frees a staging place that may be resold.
func TestReinstateRespectsStagingCapacity(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	brake := func(svc *booking.Service, h, m int) domain.Appointment {
		t.Helper()
		item, err := bookAt(t, svc, seed.DealerNorth, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, today, h, m))
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	late := brake(svc, 14, 0)
	brake(svc, 14, 0) // the 14:00 hour is now full
	at1401 := serviceOn(t, st, today, 14, 1)
	if _, err := at1401.NoShow(ctx, staffDispatcher, late.ID, 0); err != nil {
		t.Fatal(err)
	}
	brake(at1401, 14, 30) // the freed place is sold
	_, err := serviceOn(t, st, today, 14, 10).Reinstate(ctx, staffDispatcher, late.ID, 0)
	expectCode(t, err, domain.CodeUnavailable)
}

func TestAppointmentSpanCappedAt24Hours(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0)) // 08:00–11:00
	clock := serviceOn(t, st, today, 8, 0)
	if _, err := clock.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	// In progress: an overrun is extended past closing (checked-in work must still fit the day).
	if _, err := clock.StartWork(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	// 11:00 + 5×4h = 07:00 next day (23h span); one more 4h step would make it 27h.
	for range 5 {
		if _, err := clock.Extend(ctx, staffDispatcher, job.ID, 240, 0); err != nil {
			t.Fatal(err)
		}
	}
	_, err := clock.Extend(ctx, staffDispatcher, job.ID, 240, 0)
	expectCode(t, err, domain.CodeUnavailable)

	_, err = pool.Exec(ctx, `UPDATE appointments SET end_at = start_at + interval '25 hours' WHERE id = $1`, job.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" /* check_violation */ || pgErr.ConstraintName != "appointments_max_span" {
		t.Fatalf("a 25h appointment: got %v, want the appointments_max_span check violation", err)
	}
}

// Reinstate re-checks only the part of the booked window still ahead of now, like the config
// guards: a no-show is always past its start, and a roster edit since then that only touches the
// window's past (allowed, the row was not holding) does not refuse the late arrival.
func TestReinstateIgnoresWindowPast(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	appt := riverMajor(t, svc, newVehicle(t, svc), londonOn(t, today, 8, 0)) // 08:00–11:00
	at840 := serviceOn(t, st, today, 8, 40)
	if _, err := at840.NoShow(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	// The technician's shift now starts 08:30: behind 08:40, so the stranded guard allows it.
	if _, err := at840.ReplaceShifts(ctx, staffAdmin, appt.TechnicianID, []domain.TechnicianShift{{
		Weekday: today.ISOWeekday(), StartTime: "08:30", EndTime: "18:00",
	}}, 0); err != nil {
		t.Fatalf("shift change over the window's past: %v", err)
	}
	if _, err := serviceOn(t, st, today, 8, 45).Reinstate(ctx, staffDispatcher, appt.ID, 0); err != nil {
		t.Fatalf("reinstate after a roster edit over the window's past: %v", err)
	}
}

// Commit runs detached from the request's cancellation: a deadline or disconnect landing after the
// last statement must not report a failure for a write the server commits anyway.
func TestCommitSurvivesCanceledContext(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	appt := riverMajor(t, svc, newVehicle(t, svc), londonAt(t, 9, 0))
	reqCtx, cancel := context.WithCancel(ctx)
	tx, err := st.Begin(reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.LockAppointment(reqCtx, appt.ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Commit(reqCtx); err != nil {
		t.Fatalf("commit after the request context ended: %v", err)
	}
}
